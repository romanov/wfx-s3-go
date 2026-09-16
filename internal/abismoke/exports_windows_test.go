//go:build windows

package abismoke

import (
	"os"
	"syscall"
	"testing"
)

func TestWFXExports(t *testing.T) {
	filename := os.Getenv("WFXS3_DLL")
	if filename == "" {
		t.Skip("set WFXS3_DLL to the built wfxs3.wfx64 path")
	}
	dll, err := syscall.LoadDLL(filename)
	if err != nil {
		t.Fatal(err)
	}
	// The plugin pins itself when its first entry point is called. Keep this
	// handle loaded for the remainder of the test process as an extra guard
	// while inspecting the exported procedures.
	required := []string{
		"FsInitW",
		"FsFindFirstW",
		"FsFindNextW",
		"FsFindClose",
		"FsGetFileW",
		"FsPutFileW",
		"FsDeleteFileW",
		"FsGetDefRootName",
		"FsSetDefaultParams",
	}
	for _, name := range required {
		if _, err := dll.FindProc(name); err != nil {
			t.Errorf("missing export %s: %v", name, err)
		}
	}
}
