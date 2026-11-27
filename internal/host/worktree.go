package host

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// worktreesRoot returns the base directory where x-chat creates isolated
// git worktrees (~/.x-chat/worktrees).
func shortWorktreeToken() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return time.Now().Format("060102") + "-" + hex.EncodeToString(b)
}

func sanitizeWorktreeSegment(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	if out == "" {
		return "wt-" + shortWorktreeToken()
	}
	if len(out) > 64 {
		out = strings.TrimRight(out[:64], "-.")
	}
	return out
}

// resolveMainGitRepo returns the main repository root for dir (even when dir
// is a subdirectory or an existing linked worktree).
func (h *Host) resolveMainGitRepo(ctx context.Context, dir string) (string, error) {
	top, err := h.runGit(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil || top == "" {
		return "", errors.New("当前目录不是 Git 仓库，无法创建 Worktree")
	}
	commonDir, err := h.runGit(ctx, dir, "rev-parse", "--git-common-dir")
	if err == nil && commonDir != "" {
		if !filepath.IsAbs(commonDir) {
			commonDir = filepath.Join(dir, commonDir)
		}
		commonDir = filepath.Clean(commonDir)
		if filepath.Base(commonDir) == ".git" {
			parent := filepath.Dir(commonDir)
			if isDir(parent) {
				return parent, nil
			}
		}
	}
	return filepath.Clean(top), nil
}

// copyWorktreeIncludes copies local ignored files declared in <mainRepo>/.worktreeinclude
// (following Claude Code / Codex convention) into the newly created worktree.
func copyWorktreeIncludes(mainRepo, wtPath string) []string {
	incFile := filepath.Join(mainRepo, ".worktreeinclude")
	f, err := os.Open(incFile)
	if err != nil {
		return nil
	}
	defer f.Close()

	source, err := os.OpenRoot(mainRepo)
	if err != nil {
		return nil
	}
	defer source.Close()
	target, err := os.OpenRoot(wtPath)
	if err != nil {
		return nil
	}
	defer target.Close()
	seen := map[string]bool{}
	var copied []string

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(filepath.ToSlash(line), "/")
		if _, err := workspaceTarget(source, line); err != nil {
			continue
		}
		matches, err := fs.Glob(source.FS(), line)
		if err != nil || len(matches) == 0 {
			continue
		}
		for _, slashRel := range matches {
			if seen[slashRel] {
				continue
			}
			rel, err := workspaceTarget(source, slashRel)
			if err != nil {
				continue
			}
			if _, err := workspaceTarget(target, slashRel); err != nil {
				continue
			}
			st, err := source.Lstat(rel)
			if err != nil || !st.Mode().IsRegular() || st.Size() > 16<<20 {
				continue
			}
			if err := copyWorktreeFile(source, target, rel, st.Mode().Perm()); err == nil {
				seen[slashRel] = true
				copied = append(copied, slashRel)
			}
		}
	}
	return copied
}

func copyWorktreeFile(source, target *os.Root, path string, mode os.FileMode) error {
	if err := target.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	in, err := source.Open(path)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := target.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = target.Remove(path)
	}
	return err
}

func (h *Host) createWithWorktree(ctx context.Context, agentID, repoDir, baseBranch, branchName string) (Meta, error) {
	h.worktreeMu.Lock()
	defer h.worktreeMu.Unlock()
	a, ok := h.agent(agentID)
	if !ok {
		return Meta{}, fmt.Errorf("未知的 agent：%s", agentID)
	}
	repoDir = expandHome(strings.TrimSpace(repoDir))
	if repoDir == "" || !isDir(repoDir) {
		return Meta{}, fmt.Errorf("工作目录不存在：%s", repoDir)
	}

	mainRepo, err := h.resolveMainGitRepo(ctx, repoDir)
	if err != nil {
		return Meta{}, err
	}

	baseRef := strings.TrimSpace(baseBranch)
	if baseRef == "" {
		if cur, berr := h.runGit(ctx, repoDir, "symbolic-ref", "--short", "HEAD"); berr == nil && cur != "" {
			baseRef = cur
		} else {
			baseRef = "HEAD"
		}
	}
	if strings.HasPrefix(baseRef, "-") {
		return Meta{}, errors.New("无效的基准分支名")
	}

	branch := strings.TrimSpace(branchName)
	if branch == "" {
		branch = "wt/" + shortWorktreeToken()
	} else {
		if strings.HasPrefix(branch, "-") {
			return Meta{}, errors.New("无效的分支名")
		}
		if _, err := h.runGit(ctx, mainRepo, "check-ref-format", "--branch", branch); err != nil {
			return Meta{}, fmt.Errorf("分支名格式无效：%s", branch)
		}
	}
	if _, err := h.runGit(ctx, mainRepo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		return Meta{}, fmt.Errorf("分支 %s 已存在，请换一个分支名", branch)
	}

	wtBase, err := filepath.Abs(filepath.Join(h.opts.DataDir, "worktrees"))
	if err != nil {
		return Meta{}, err
	}
	repoSlug := sanitizeWorktreeSegment(filepath.Base(mainRepo))
	dirSlug := sanitizeWorktreeSegment(strings.TrimPrefix(branch, "wt/"))
	repoBucket := filepath.Join(wtBase, repoSlug)
	if err := os.MkdirAll(repoBucket, 0o755); err != nil {
		return Meta{}, fmt.Errorf("创建 Worktree 目录失败：%w", err)
	}

	wtPath := filepath.Join(repoBucket, dirSlug)
	if _, err := os.Stat(wtPath); err == nil {
		wtPath = filepath.Join(repoBucket, dirSlug+"-"+shortWorktreeToken())
	}

	if _, err := h.runGit(ctx, mainRepo, "worktree", "add", "-b", branch, wtPath, baseRef); err != nil {
		return Meta{}, fmt.Errorf("创建 Worktree 失败：%w", err)
	}

	copied := copyWorktreeIncludes(mainRepo, wtPath)

	t := now()
	m := Meta{
		ID:             newID(),
		Title:          "新对话",
		AgentID:        a.ID,
		AgentName:      a.Name,
		Cwd:            wtPath,
		Worktree:       true,
		WorktreeRepo:   mainRepo,
		WorktreeBranch: branch,
		WorktreeBase:   baseRef,
		CreatedAt:      t,
		UpdatedAt:      t,
		Status:         "idle",
	}
	h.store.create(m)
	// Remember the source repository (not the ephemeral worktree path) in RecentDirs.
	h.rememberDir(mainRepo, a.ID)
	h.pushMeta(m)
	if len(copied) > 0 {
		preview := strings.Join(copied, "、")
		if len(copied) > 4 {
			preview = strings.Join(copied[:4], "、") + fmt.Sprintf(" 等 %d 个文件", len(copied))
		}
		h.emit(m.ID, Event{"type": "notice", "level": "info", "text": "已按 .worktreeinclude 同步本地文件：" + preview, "t": now()})
	}
	return m, nil
}

// attachWorktree creates a new conversation attached to an existing linked git worktree.
func (h *Host) attachWorktree(ctx context.Context, agentID, wtPath string) (Meta, error) {
	h.worktreeMu.Lock()
	defer h.worktreeMu.Unlock()
	a, ok := h.agent(agentID)
	if !ok {
		return Meta{}, fmt.Errorf("未知的 agent：%s", agentID)
	}
	wtPath = expandHome(strings.TrimSpace(wtPath))
	if wtPath == "" || !isDir(wtPath) {
		return Meta{}, fmt.Errorf("Worktree 目录不存在：%s", wtPath)
	}
	mainRepo, err := h.resolveMainGitRepo(ctx, wtPath)
	if err != nil {
		return Meta{}, err
	}
	wtPath, err = h.linkedWorktreePath(ctx, mainRepo, wtPath)
	if err != nil {
		return Meta{}, err
	}
	branch, _ := h.runGit(ctx, wtPath, "symbolic-ref", "--short", "HEAD")
	baseBranch, _ := h.runGit(ctx, mainRepo, "symbolic-ref", "--short", "HEAD")

	t := now()
	m := Meta{
		ID:             newID(),
		Title:          "新对话",
		AgentID:        a.ID,
		AgentName:      a.Name,
		Cwd:            filepath.Clean(wtPath),
		Worktree:       true,
		WorktreeRepo:   mainRepo,
		WorktreeBranch: branch,
		WorktreeBase:   baseBranch,
		CreatedAt:      t,
		UpdatedAt:      t,
		Status:         "idle",
	}
	h.store.create(m)
	h.rememberDir(mainRepo, a.ID)
	h.pushMeta(m)
	return m, nil
}

func (h *Host) listWorktrees(ctx context.Context, repoDir string) ([]WorktreeInfo, error) {
	repoDir = expandHome(strings.TrimSpace(repoDir))
	if repoDir == "" || !isDir(repoDir) {
		return []WorktreeInfo{}, nil
	}
	mainRepo, err := h.resolveMainGitRepo(ctx, repoDir)
	if err != nil {
		return []WorktreeInfo{}, nil
	}
	out, err := h.runGit(ctx, mainRepo, "worktree", "list", "--porcelain")
	if err != nil || out == "" {
		return []WorktreeInfo{}, nil
	}
	evalMain := filepath.Clean(mainRepo)
	if r, rerr := filepath.EvalSymlinks(evalMain); rerr == nil {
		evalMain = r
	}

	var list []WorktreeInfo
	var cur *WorktreeInfo
	flush := func() {
		if cur == nil || cur.Path == "" || !isDir(cur.Path) {
			cur = nil
			return
		}
		evalPath := filepath.Clean(cur.Path)
		if r, rerr := filepath.EvalSymlinks(evalPath); rerr == nil {
			evalPath = r
		}
		cur.IsMain = evalPath == evalMain
		if !cur.IsMain {
			files, statusErr := h.parsePorcelainFiles(ctx, cur.Path)
			cur.ChangedCount = len(files)
			cur.Dirty = statusErr != nil || len(files) > 0
			list = append(list, *cur)
		}
		cur = nil
	}

	for _, rawLine := range strings.Split(out, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			flush()
			continue
		}
		if after, ok := strings.CutPrefix(line, "worktree "); ok {
			flush()
			cur = &WorktreeInfo{Path: strings.TrimSpace(after)}
		} else if cur != nil {
			if after, ok := strings.CutPrefix(line, "branch "); ok {
				cur.Branch = strings.TrimPrefix(strings.TrimSpace(after), "refs/heads/")
			} else if line == "detached" {
				cur.Branch = "HEAD"
			}
		}
	}
	flush()
	if list == nil {
		list = []WorktreeInfo{}
	}
	return list, nil
}

func (h *Host) linkedWorktreePath(ctx context.Context, repo, path string) (string, error) {
	commonDir, err := h.runGit(ctx, repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	gitDir, err := h.runGit(ctx, path, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	top, err := h.runGit(ctx, path, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	if filepath.Clean(filepath.Dir(gitDir)) != filepath.Join(commonDir, "worktrees") || !sameDirectory(top, path) {
		return "", errors.New("目录不是该仓库的已登记 Worktree")
	}
	return filepath.Clean(top), nil
}

func sameDirectory(left, right string) bool {
	leftInfo, err := os.Stat(left)
	if err != nil {
		return false
	}
	rightInfo, err := os.Stat(right)
	return err == nil && os.SameFile(leftInfo, rightInfo)
}

func (h *Host) parsePorcelainFiles(ctx context.Context, dir string) ([]WorktreeFileChange, error) {
	out, err := h.runGitRaw(ctx, dir, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	files := []WorktreeFileChange{}
	records := strings.Split(out, "\x00")
	for index := 0; index < len(records); index++ {
		line := records[index]
		if len(line) < 4 {
			continue
		}
		x, y := line[0], line[1]
		path := line[3:]
		if x == 'R' || y == 'R' || x == 'C' || y == 'C' {
			index++
		}
		st := "M"
		switch {
		case x == '?' && y == '?':
			st = "?"
		case x == 'A' || y == 'A':
			st = "A"
		case x == 'D' || y == 'D':
			st = "D"
		case x == 'R' || y == 'R':
			st = "R"
		}
		files = append(files, WorktreeFileChange{Path: path, Status: st})
	}
	return files, nil
}

func (h *Host) worktreeMergeBase(ctx context.Context, wtDir, baseBranch string) string {
	base := strings.TrimSpace(baseBranch)
	if base == "" {
		return "HEAD"
	}
	if mb, err := h.runGit(ctx, wtDir, "merge-base", base, "HEAD"); err == nil && mb != "" {
		return mb
	}
	return base
}

func (h *Host) parseDiffNameStatus(ctx context.Context, dir, baseRef string) []WorktreeFileChange {
	out, err := h.runGit(ctx, dir, "diff", "--name-status", baseRef)
	if err != nil || out == "" {
		return []WorktreeFileChange{}
	}
	files := []WorktreeFileChange{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			continue
		}
		code := parts[0]
		p := parts[len(parts)-1]
		st := "M"
		switch {
		case strings.HasPrefix(code, "A"):
			st = "A"
		case strings.HasPrefix(code, "D"):
			st = "D"
		case strings.HasPrefix(code, "R"):
			st = "R"
		}
		files = append(files, WorktreeFileChange{Path: p, Status: st})
	}
	return files
}

func (h *Host) worktreeStatus(ctx context.Context, id string) (WorktreeStatus, error) {
	m, ok := h.store.meta(id)
	if !ok {
		return WorktreeStatus{}, errors.New("对话不存在")
	}
	if !m.Worktree {
		return WorktreeStatus{}, nil
	}
	baseBranch := m.WorktreeBase
	if baseBranch == "" && m.WorktreeRepo != "" && isDir(m.WorktreeRepo) {
		baseBranch, _ = h.runGit(ctx, m.WorktreeRepo, "symbolic-ref", "--short", "HEAD")
	}
	if !isDir(m.Cwd) {
		return WorktreeStatus{
			Exists:       false,
			Branch:       m.WorktreeBranch,
			BaseBranch:   baseBranch,
			ChangedFiles: []WorktreeFileChange{},
		}, nil
	}
	branch := m.WorktreeBranch
	if cur, err := h.runGit(ctx, m.Cwd, "symbolic-ref", "--short", "HEAD"); err == nil && cur != "" {
		branch = cur
	}
	if _, err := h.linkedWorktreePath(ctx, m.WorktreeRepo, m.Cwd); err != nil {
		return WorktreeStatus{}, err
	}
	uncommitted, err := h.parsePorcelainFiles(ctx, m.Cwd)
	if err != nil {
		return WorktreeStatus{}, err
	}
	ahead := 0
	if baseBranch != "" {
		if cntStr, err := h.runGit(ctx, m.Cwd, "rev-list", "--count", baseBranch+"..HEAD"); err == nil {
			if n, perr := strconv.Atoi(strings.TrimSpace(cntStr)); perr == nil {
				ahead = n
			}
		}
	}
	// Combine committed changes (relative to merge-base) and uncommitted/untracked files
	// so the user always sees every file that will be applied to the main repo.
	seen := map[string]int{}
	files := []WorktreeFileChange{}
	if baseBranch != "" {
		mb := h.worktreeMergeBase(ctx, m.Cwd, baseBranch)
		for _, f := range h.parseDiffNameStatus(ctx, m.Cwd, mb) {
			seen[f.Path] = len(files)
			files = append(files, f)
		}
	}
	for _, f := range uncommitted {
		if idx, exists := seen[f.Path]; exists {
			files[idx] = f
		} else {
			seen[f.Path] = len(files)
			files = append(files, f)
		}
	}
	return WorktreeStatus{
		Exists:       true,
		Dirty:        len(uncommitted) > 0,
		ChangedCount: len(files),
		AheadCount:   ahead,
		Branch:       branch,
		BaseBranch:   baseBranch,
		ChangedFiles: files,
	}, nil
}

func sameOrSubPath(target, base string) bool {
	cTarget := filepath.Clean(target)
	cBase := filepath.Clean(base)
	if r, err := filepath.EvalSymlinks(cTarget); err == nil {
		cTarget = r
	}
	if r, err := filepath.EvalSymlinks(cBase); err == nil {
		cBase = r
	}
	if cTarget == cBase {
		return true
	}
	return strings.HasPrefix(cTarget, cBase+string(os.PathSeparator))
}

func (h *Host) closeConversationHandles(id, cwd string) {
	idsToClose := map[string]bool{}
	if id != "" {
		idsToClose[id] = true
	}
	if cwd != "" {
		for _, m := range h.store.list() {
			if m.Worktree && sameOrSubPath(m.Cwd, cwd) {
				idsToClose[m.ID] = true
			}
		}
	}
	var liveIDs []string
	h.liveMu.Lock()
	for cid := range idsToClose {
		if _, wasLive := h.lives[cid]; wasLive {
			delete(h.lives, cid)
			liveIDs = append(liveIDs, cid)
		}
	}
	h.liveMu.Unlock()
	if len(liveIDs) > 0 {
		if sc, err := h.sidecar(); err == nil {
			for _, cid := range liveIDs {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = sc.call(ctx, "close", map[string]any{"key": cid}, nil)
				cancel()
			}
		}
	}
	if cwd == "" {
		return
	}
	h.termMu.Lock()
	var toClose []string
	for tid, term := range h.terms {
		if sameOrSubPath(term.cwd, cwd) {
			toClose = append(toClose, tid)
		}
	}
	h.termMu.Unlock()
	for _, tid := range toClose {
		_ = h.termClose(tid)
	}
}

// mergeWorktree merges a worktree conversation's changes back into its base branch
// in the source repository (supporting "merge" and "squash" strategies), and
// optionally removes the worktree and temporary branch afterward.
func (h *Host) mergeWorktree(ctx context.Context, id, strategy, commitMsg string, cleanup bool) error {
	h.worktreeMu.Lock()
	defer h.worktreeMu.Unlock()
	if strategy != "merge" && strategy != "squash" {
		return errors.New("无效的合入方式")
	}
	m, ok := h.store.meta(id)
	if !ok {
		return errors.New("对话不存在")
	}
	if !m.Worktree {
		return errors.New("当前对话不是 Worktree 会话")
	}
	if !isDir(m.Cwd) {
		return errors.New("Worktree 目录已不存在")
	}
	repo := m.WorktreeRepo
	if repo == "" {
		var err error
		repo, err = h.resolveMainGitRepo(ctx, m.Cwd)
		if err != nil {
			return err
		}
	}
	if !isDir(repo) {
		return fmt.Errorf("源仓库目录不存在：%s", repo)
	}

	if _, err := h.linkedWorktreePath(ctx, repo, m.Cwd); err != nil {
		return err
	}
	if err := h.ensureWorktreeIdle(m, cleanup); err != nil {
		return err
	}
	wtBranch, err := h.runGit(ctx, m.Cwd, "symbolic-ref", "--short", "HEAD")
	if err != nil || wtBranch == "" {
		return errors.New("当前 Worktree 处于游离 HEAD，无法自动合入")
	}
	baseBranch := m.WorktreeBase
	if baseBranch == "" {
		baseBranch, _ = h.runGit(ctx, repo, "symbolic-ref", "--short", "HEAD")
	}
	if baseBranch == "" {
		return errors.New("无法确定源仓库基准分支")
	}
	if baseBranch == wtBranch {
		return errors.New("Worktree 分支与基准分支相同，无需合入")
	}
	if _, err := h.runGit(ctx, repo, "check-ref-format", "--branch", wtBranch); err != nil {
		return errors.New("Worktree 分支名无效，拒绝合并")
	}
	if _, err := h.runGit(ctx, repo, "check-ref-format", "--branch", baseBranch); err != nil {
		return errors.New("基准分支名无效，拒绝合并")
	}
	if _, err := h.runGit(ctx, repo, "show-ref", "--verify", "refs/heads/"+baseBranch); err != nil {
		return fmt.Errorf("基准分支不存在：%s", baseBranch)
	}
	repoFiles, err := h.parsePorcelainFiles(ctx, repo)
	if err != nil {
		return err
	}
	if len(repoFiles) > 0 {
		return fmt.Errorf("源仓库有 %d 处未提交更改，请先处理再合入", len(repoFiles))
	}

	// Step 1: If worktree has uncommitted changes, commit them first.
	wtFiles, err := h.parsePorcelainFiles(ctx, m.Cwd)
	if err != nil {
		return err
	}
	msg := strings.TrimSpace(commitMsg)
	if len(wtFiles) > 0 {
		if msg == "" {
			msg = fmt.Sprintf("feat: %s", oneLine(m.Title, 50))
		}
		if _, err := h.runGit(ctx, m.Cwd, "add", "-A"); err != nil {
			return fmt.Errorf("暂存 Worktree 更改失败：%w", err)
		}
		if _, err := h.runGit(ctx, m.Cwd, "commit", "-m", msg); err != nil {
			return fmt.Errorf("提交 Worktree 更改失败：%w", err)
		}
	}

	// Verify there is actually something ahead to merge.
	wtRef := "refs/heads/" + wtBranch
	cntStr, err := h.runGit(ctx, repo, "rev-list", "--count", "refs/heads/"+baseBranch+".."+wtRef, "--")
	if err != nil {
		return err
	}
	if cntStr == "0" {
		return fmt.Errorf("分支 %s 相对 %s 没有新的提交", wtBranch, baseBranch)
	}

	// Step 3: Ensure source repo is on baseBranch.
	curRepoBranch, _ := h.runGit(ctx, repo, "symbolic-ref", "--short", "HEAD")
	if curRepoBranch != baseBranch {
		return fmt.Errorf("源仓库当前在 %s；请先切换到基准分支 %s 再合入", curRepoBranch, baseBranch)
	}

	// Step 4: Merge or Squash into baseBranch, aborting cleanly on conflict.
	if strategy == "squash" {
		if _, err := h.runGit(ctx, repo, "merge", "--squash", wtRef); err != nil {
			_, _ = h.runGit(ctx, repo, "reset", "--merge")
			return fmt.Errorf("压缩合入发生冲突（已回滚源仓库）：%w", err)
		}
		if msg == "" {
			msg = fmt.Sprintf("feat: %s (%s)", oneLine(m.Title, 50), wtBranch)
		}
		if _, err := h.runGit(ctx, repo, "commit", "-m", msg); err != nil {
			_, _ = h.runGit(ctx, repo, "reset", "--merge")
			return fmt.Errorf("提交压缩合入失败：%w", err)
		}
	} else {
		if _, err := h.runGit(ctx, repo, "merge", "--no-ff", "-m", fmt.Sprintf("Merge branch '%s' into %s", wtBranch, baseBranch), wtRef); err != nil {
			_, _ = h.runGit(ctx, repo, "merge", "--abort")
			return fmt.Errorf("合并发生冲突（已中止合并）：%w", err)
		}
	}

	if cleanup {
		if err := h.cleanWorktree(ctx, m, false, true, strategy == "squash"); err != nil {
			return fmt.Errorf("已合入，但清理未完成：%w", err)
		}
	}
	return nil
}

func (h *Host) removeWorktree(ctx context.Context, id string, force, deleteBranch bool) error {
	h.worktreeMu.Lock()
	defer h.worktreeMu.Unlock()
	m, ok := h.store.meta(id)
	if !ok {
		return errors.New("对话不存在")
	}
	if !m.Worktree {
		return nil
	}
	return h.cleanWorktree(ctx, m, force, deleteBranch, force)
}

// removeWorktreeByPath removes any linked git worktree directly by its path (including
// orphan worktrees whose conversations were already deleted) and updates any linked chats.
func (h *Host) removeWorktreeByPath(ctx context.Context, wtPath string, deleteBranch bool) error {
	h.worktreeMu.Lock()
	defer h.worktreeMu.Unlock()
	wtPath = expandHome(strings.TrimSpace(wtPath))
	if wtPath == "" {
		return errors.New("Worktree 路径不能为空")
	}
	wtPath = filepath.Clean(wtPath)
	var repo, branch string
	if isDir(wtPath) {
		mainRepo, err := h.resolveMainGitRepo(ctx, wtPath)
		if err != nil {
			return err
		}
		if sameOrSubPath(mainRepo, wtPath) && sameOrSubPath(wtPath, mainRepo) {
			return errors.New("不能删除主仓库目录")
		}
		linkedPath, err := h.linkedWorktreePath(ctx, mainRepo, wtPath)
		if err != nil {
			return err
		}
		wtPath = linkedPath
		repo = mainRepo
		branch, _ = h.runGit(ctx, wtPath, "symbolic-ref", "--short", "HEAD")
	}
	for _, m := range h.store.list() {
		if m.Worktree && sameOrSubPath(m.Cwd, wtPath) {
			status := h.liveState(m.ID).Status
			if status == "running" || status == "connecting" {
				return errors.New("请先停止当前会话再操作 Worktree")
			}
		}
	}
	h.closeConversationHandles("", wtPath)
	if repo != "" && isDir(repo) {
		if isDir(wtPath) {
			if _, err := h.runGit(ctx, repo, "worktree", "remove", "--force", "--", wtPath); err != nil {
				if rerr := os.RemoveAll(wtPath); rerr != nil {
					return fmt.Errorf("删除 Worktree 目录失败：%w", rerr)
				}
			}
		}
		_, _ = h.runGit(ctx, repo, "worktree", "prune")
		if deleteBranch && branch != "" && branch != "HEAD" {
			_, _ = h.runGit(ctx, repo, "branch", "-D", "--", branch)
		}
	}
	for _, m := range h.store.list() {
		if m.Worktree && sameOrSubPath(m.Cwd, wtPath) {
			if updated, ok := h.store.update(m.ID, func(mm *Meta) {
				if repo != "" && isDir(repo) {
					mm.Cwd = repo
				} else if mm.WorktreeRepo != "" && isDir(mm.WorktreeRepo) {
					mm.Cwd = mm.WorktreeRepo
				}
				mm.AcpSessionID = ""
				mm.Worktree = false
				mm.WorktreeRepo = ""
				mm.WorktreeBranch = ""
				mm.WorktreeBase = ""
			}); ok {
				h.pushMeta(updated)
				h.pushLive(m.ID)
			}
		}
	}
	return nil
}

func (h *Host) ensureWorktreeIdle(meta Meta, exclusive bool) error {
	status := h.liveState(meta.ID).Status
	if status == "running" || status == "connecting" {
		return errors.New("请先停止当前会话再操作 Worktree")
	}
	if exclusive {
		for _, other := range h.store.list() {
			if other.ID != meta.ID && (other.Cwd == meta.Cwd || sameDirectory(other.Cwd, meta.Cwd)) {
				return errors.New("其他会话仍在使用该 Worktree")
			}
		}
	}
	return nil
}

func (h *Host) cleanWorktree(ctx context.Context, m Meta, force, deleteBranch, forceBranch bool) error {
	if err := h.ensureWorktreeIdle(m, true); err != nil {
		return err
	}
	repo := m.WorktreeRepo
	if !isDir(repo) {
		return errors.New("源仓库目录不存在，无法安全清理 Worktree")
	}
	branch := ""
	path, statErr := os.Stat(m.Cwd)
	if statErr == nil {
		if !path.IsDir() {
			return errors.New("Worktree 路径已不是目录")
		}
		if _, err := h.linkedWorktreePath(ctx, repo, m.Cwd); err != nil {
			return err
		}
		status, err := h.worktreeStatus(ctx, m.ID)
		if err != nil {
			return err
		}
		if status.Dirty && !force {
			return fmt.Errorf("Worktree 有 %d 处未提交更改，请先提交或勾选强制清理", status.ChangedCount)
		}
		branch, _ = h.runGit(ctx, m.Cwd, "symbolic-ref", "--short", "HEAD")
		h.closeConversationHandles(m.ID, m.Cwd)
		args := []string{"worktree", "remove"}
		if force {
			args = append(args, "--force")
		}
		if _, err := h.runGit(ctx, repo, append(args, "--", m.Cwd)...); err != nil {
			h.pushLive(m.ID)
			return fmt.Errorf("移除 Worktree 失败：%w", err)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	} else {
		h.closeConversationHandles(m.ID, m.Cwd)
	}

	// Keep the conversation valid if the user cleaned up the worktree without deleting the chat.
	if updated, ok := h.store.update(m.ID, func(mm *Meta) {
		mm.Cwd = repo
		mm.AcpSessionID = ""
		mm.Worktree = false
		mm.WorktreeRepo = ""
		mm.WorktreeBranch = ""
		mm.WorktreeBase = ""
	}); ok {
		h.pushMeta(updated)
		h.pushLive(m.ID)
	}
	if deleteBranch && branch != "" && branch != m.WorktreeBase {
		flag := "-d"
		if forceBranch {
			flag = "-D"
		}
		if _, err := h.runGit(ctx, repo, "branch", flag, "--", branch); err != nil {
			return fmt.Errorf("已移除 Worktree，但保留分支 %s：%w", branch, err)
		}
	}
	return nil
}
