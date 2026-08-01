package app

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"print-agent/internal/platform"
)

func TestCloseBeforeRunAndPrivateFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agent")
	svc, err := New(Options{DataDir: dir, Driver: platform.UnimplementedDriver{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := svc.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	for _, item := range []struct {
		path string
		mode os.FileMode
	}{
		{dir, 0o700},
		{filepath.Join(dir, "logs"), 0o700},
		{filepath.Join(dir, "logs", "print-agent.log"), 0o600},
		{filepath.Join(dir, "print-agent.db"), 0o600},
	} {
		info, err := os.Stat(item.path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != item.mode {
			t.Errorf("%s mode = %o, want %o", item.path, got, item.mode)
		}
	}
}
