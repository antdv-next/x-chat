package host

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

var workspaceMutationMu sync.Mutex

func workspaceMutation(root string, mutate func(*os.Root) error) error {
	workspaceMutationMu.Lock()
	defer workspaceMutationMu.Unlock()
	dir := expandHome(strings.TrimSpace(root))
	if dir == "" {
		return errors.New("未指定工作目录")
	}
	workspace, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer workspace.Close()
	return mutate(workspace)
}

func workspaceTarget(root *os.Root, rel string) (string, error) {
	path := strings.ReplaceAll(rel, "\\", "/")
	if !fs.ValidPath(path) || path == "." || !filepath.IsLocal(filepath.FromSlash(path)) {
		return "", fmt.Errorf("无效的相对路径：%s", rel)
	}
	var prefix string
	for _, segment := range strings.Split(path, "/") {
		if strings.EqualFold(segment, ".git") || (runtime.GOOS == "windows" &&
			(strings.Contains(segment, ":") || strings.TrimRight(segment, " .") != segment)) {
			return "", fmt.Errorf("不允许操作路径：%s", rel)
		}
		prefix = filepath.Join(prefix, segment)
		info, err := root.Lstat(prefix)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("不允许通过符号链接操作路径：%s", rel)
		}
	}
	return filepath.FromSlash(path), nil
}

func createWorkspaceEntry(root, rel string, isDir bool) error {
	return workspaceMutation(root, func(workspace *os.Root) error {
		path, err := workspaceTarget(workspace, rel)
		if err != nil {
			return err
		}
		if err := workspace.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if isDir {
			return workspace.Mkdir(path, 0o755)
		}
		file, err := workspace.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		return file.Close()
	})
}

func renameWorkspaceEntry(root, oldRel, newRel string) error {
	return workspaceMutation(root, func(workspace *os.Root) error {
		oldPath, err := workspaceTarget(workspace, oldRel)
		if err != nil {
			return err
		}
		newPath, err := workspaceTarget(workspace, newRel)
		if err != nil {
			return err
		}
		oldInfo, err := workspace.Lstat(oldPath)
		if err != nil || oldPath == newPath {
			return err
		}
		if newInfo, err := workspace.Lstat(newPath); err == nil {
			if !strings.EqualFold(oldPath, newPath) || !os.SameFile(oldInfo, newInfo) {
				return fmt.Errorf("目标已存在：%s", newRel)
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := workspace.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
			return err
		}
		return workspace.Rename(oldPath, newPath)
	})
}

func deleteWorkspaceEntry(root, rel string) error {
	return workspaceMutation(root, func(workspace *os.Root) error {
		path, err := workspaceTarget(workspace, rel)
		if err != nil {
			return err
		}
		if _, err := workspace.Lstat(path); err != nil {
			return err
		}
		return workspace.RemoveAll(path)
	})
}
