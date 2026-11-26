package host

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorkspaceCreateRenameDelete(t *testing.T) {
	root := t.TempDir()

	// Create nested file and directory.
	if err := createWorkspaceEntry(root, "src/app.ts", false); err != nil {
		t.Fatalf("create file failed: %v", err)
	}
	if st, err := os.Stat(filepath.Join(root, "src", "app.ts")); err != nil || st.IsDir() {
		t.Fatalf("expected regular file, got err=%v info=%v", err, st)
	}
	if err := createWorkspaceEntry(root, "src/components", true); err != nil {
		t.Fatalf("create dir failed: %v", err)
	}
	if st, err := os.Stat(filepath.Join(root, "src", "components")); err != nil || !st.IsDir() {
		t.Fatalf("expected directory, got err=%v info=%v", err, st)
	}

	// Duplicate creation should fail.
	if err := createWorkspaceEntry(root, "src/app.ts", false); err == nil {
		t.Fatalf("expected duplicate create to fail")
	}

	// Rename file and directory.
	if err := renameWorkspaceEntry(root, "src/app.ts", "src/main.ts"); err != nil {
		t.Fatalf("rename file failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "src", "main.ts")); err != nil {
		t.Fatalf("expected renamed file to exist: %v", err)
	}
	if err := renameWorkspaceEntry(root, "src/components", "src/ui"); err != nil {
		t.Fatalf("rename dir failed: %v", err)
	}
	if st, err := os.Stat(filepath.Join(root, "src", "ui")); err != nil || !st.IsDir() {
		t.Fatalf("expected renamed dir to exist: %v", err)
	}

	// Delete file and non-empty directory.
	if err := deleteWorkspaceEntry(root, "src/main.ts"); err != nil {
		t.Fatalf("delete file failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "src", "main.ts")); !os.IsNotExist(err) {
		t.Fatalf("expected file to be deleted, err=%v", err)
	}
	if err := deleteWorkspaceEntry(root, "src"); err != nil {
		t.Fatalf("delete dir failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "src")); !os.IsNotExist(err) {
		t.Fatalf("expected dir to be deleted, err=%v", err)
	}
}

func TestWorkspaceMutationSafetyGuards(t *testing.T) {
	root := t.TempDir()

	for _, bad := range []string{"", "/", "/absolute", ".", "..", "../escape.txt", "a/../b", ".git", ".git/config", "nested/.GIT/config"} {
		if err := createWorkspaceEntry(root, bad, false); err == nil {
			t.Fatalf("expected create(%q) to be rejected", bad)
		}
		if err := deleteWorkspaceEntry(root, bad); err == nil {
			t.Fatalf("expected delete(%q) to be rejected", bad)
		}
	}
}

func TestWorkspaceMutationRejectsSymlinks(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := createWorkspaceEntry(root, "original.txt", false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := createWorkspaceEntry(root, "link/new.txt", false); err == nil {
		t.Fatal("created a file through a symlink")
	}
	if err := renameWorkspaceEntry(root, "original.txt", "link/keep.txt"); err == nil {
		t.Fatal("renamed over a file through a symlink")
	}
	if err := deleteWorkspaceEntry(root, "link/keep.txt"); err == nil {
		t.Fatal("deleted a file through a symlink")
	}
	if contents, err := os.ReadFile(filepath.Join(outside, "keep.txt")); err != nil || string(contents) != "keep" {
		t.Fatalf("file outside workspace changed: data=%q err=%v", contents, err)
	}
}
