//go:build windows

package debugui

import (
	"path/filepath"
	"regexp"
	"testing"
)

func TestFileVersion(t *testing.T) {
	if systemDir == "" {
		t.Fatal("the system directory is unknown")
	}
	got := FileVersion(filepath.Join(systemDir, "kernel32.dll"))
	if !regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`).MatchString(got) {
		t.Fatalf("unexpected kernel32.dll version %q", got)
	}
	if got := FileVersion(filepath.Join(t.TempDir(), "missing.exe")); got != "" {
		t.Fatalf("a missing file has version %q", got)
	}
}
