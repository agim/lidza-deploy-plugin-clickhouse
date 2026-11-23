package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// realHost is the server the plugin runs on.
type realHost struct{}

func (realHost) Exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (realHost) Run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "DEBIAN_FRONTEND=noninteractive")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (realHost) FreeBytes(path string) (uint64, error) {
	for path != "/" && path != "." {
		if _, err := os.Stat(path); err == nil {
			break
		}
		path = filepath.Dir(path)
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

func (realHost) DirEmpty(path string) bool {
	entries, err := os.ReadDir(path)
	return err != nil || len(entries) == 0
}
