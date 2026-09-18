//go:build windows

package debugui

import (
	"fmt"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

var (
	procFindWindowW          = user32.NewProc("FindWindowW")
	procGetWindowTextLengthW = user32.NewProc("GetWindowTextLengthW")
	procGetWindowTextW       = user32.NewProc("GetWindowTextW")
	procIsWindowEnabled      = user32.NewProc("IsWindowEnabled")
)

func TestDialogTemplateIsDWORDAligned(t *testing.T) {
	template := dialogTemplate("title")
	if uintptr(unsafe.Pointer(&template[0]))%4 != 0 {
		t.Fatal("the template is not DWORD-aligned")
	}
	words := unsafe.Slice((*uint16)(unsafe.Pointer(&template[0])), len(template)*2)
	if got := int(words[4]); got != 1+len(buttons) {
		t.Fatalf("the template declares %d controls, want %d", got, 1+len(buttons))
	}
}

// TestDialogSmoke opens the real dialog, so it only runs when asked to:
//
//	$env:WFXS3_UI_TEST = '1'; go test ./internal/debugui/ -run Smoke -v
func TestDialogSmoke(t *testing.T) {
	if os.Getenv("WFXS3_UI_TEST") == "" {
		t.Skip("set WFXS3_UI_TEST=1 to open the debug dialog")
	}
	title := fmt.Sprintf("debugui smoke test %d", os.Getpid())
	var reports, starts atomic.Int32
	var running atomic.Bool
	actions := Actions{
		Report:     func() string { return fmt.Sprintf("report %d\nsecond line", reports.Add(1)) },
		ConfigPath: func() string { return "" },
		StartTest: func() bool {
			starts.Add(1)
			running.Store(true)
			return true
		},
		TestRunning: running.Load,
	}
	result := make(chan error, 1)
	go func() { result <- Show(0, title, actions) }()

	hwnd := waitForWindow(t, title)
	text := call(procGetDlgItem, hwnd, idText)
	if got := windowText(text); got != "report 1\r\nsecond line" {
		t.Fatalf("unexpected initial text %q", got)
	}
	if call(procIsWindowEnabled, call(procGetDlgItem, hwnd, idOpenINI)) != 0 {
		t.Fatal("Open wfxs3.ini is enabled without a settings path")
	}

	send(hwnd, wmCommand, idRefresh, 0)
	if got := windowText(text); got != "report 2\r\nsecond line" {
		t.Fatalf("Refresh did not rebuild the report: %q", got)
	}

	testButton := call(procGetDlgItem, hwnd, idTest)
	send(hwnd, wmCommand, idTest, 0)
	if starts.Load() != 1 || call(procIsWindowEnabled, testButton) != 0 {
		t.Fatal("Test connections did not start a test and wait for it")
	}
	running.Store(false)
	deadline := time.Now().Add(5 * time.Second)
	for call(procIsWindowEnabled, testButton) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the dialog did not notice that the test finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := windowText(text); got != "report 4\r\nsecond line" {
		t.Fatalf("the finished test was not shown: %q", got)
	}

	call(procPostMessageW, hwnd, wmCommand, idOK, 0)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the dialog did not close")
	}
}

func waitForWindow(t *testing.T, title string) uintptr {
	t.Helper()
	name, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if hwnd, _, _ := procFindWindowW.Call(0, uintptr(unsafe.Pointer(name))); hwnd != 0 {
			return hwnd
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the dialog did not appear")
	return 0
}

func windowText(hwnd uintptr) string {
	length := call(procGetWindowTextLengthW, hwnd)
	buffer := make([]uint16, length+1)
	procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)))
	return syscall.UTF16ToString(buffer)
}
