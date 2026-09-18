//go:build windows

package debugui

import (
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// kernel32 is always loaded, and version.dll is not a known DLL, so the other
// libraries are loaded from the system directory by absolute path instead of
// through the DLL search order.
var (
	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procGetSystemDirectoryW = kernel32.NewProc("GetSystemDirectoryW")
	procRtlMoveMemory       = kernel32.NewProc("RtlMoveMemory")

	systemDir = systemDirectory()

	user32  = systemDLL("user32.dll")
	gdi32   = systemDLL("gdi32.dll")
	shell32 = systemDLL("shell32.dll")
	version = systemDLL("version.dll")

	procDialogBoxIndirectParamW = user32.NewProc("DialogBoxIndirectParamW")
	procEnableWindow            = user32.NewProc("EnableWindow")
	procEndDialog               = user32.NewProc("EndDialog")
	procGetClientRect           = user32.NewProc("GetClientRect")
	procGetDlgItem              = user32.NewProc("GetDlgItem")
	procGetFocus                = user32.NewProc("GetFocus")
	procGetSysColor             = user32.NewProc("GetSysColor")
	procGetSysColorBrush        = user32.NewProc("GetSysColorBrush")
	procGetWindowRect           = user32.NewProc("GetWindowRect")
	procKillTimer               = user32.NewProc("KillTimer")
	procMapDialogRect           = user32.NewProc("MapDialogRect")
	procMessageBoxW             = user32.NewProc("MessageBoxW")
	procMoveWindow              = user32.NewProc("MoveWindow")
	procPostMessageW            = user32.NewProc("PostMessageW")
	procSendMessageW            = user32.NewProc("SendMessageW")
	procSetFocus                = user32.NewProc("SetFocus")
	procSetTimer                = user32.NewProc("SetTimer")
	procSetWindowTextW          = user32.NewProc("SetWindowTextW")

	procCreateFontIndirectW = gdi32.NewProc("CreateFontIndirectW")
	procDeleteObject        = gdi32.NewProc("DeleteObject")
	procGetObjectW          = gdi32.NewProc("GetObjectW")
	procSetBkColor          = gdi32.NewProc("SetBkColor")
	procSetTextColor        = gdi32.NewProc("SetTextColor")

	procShellExecuteW = shell32.NewProc("ShellExecuteW")

	procGetFileVersionInfoSizeW = version.NewProc("GetFileVersionInfoSizeW")
	procGetFileVersionInfoW     = version.NewProc("GetFileVersionInfoW")
	procVerQueryValueW          = version.NewProc("VerQueryValueW")
)

const (
	idOK      = 1
	idCancel  = 2
	idText    = 100
	idTest    = 101
	idOpenINI = 102
	idCopy    = 103
	idRefresh = 104

	wmSize                = 0x0005
	wmClose               = 0x0010
	wmGetMinMaxInfo       = 0x0024
	wmNextDlgCtl          = 0x0028
	wmSetFont             = 0x0030
	wmGetFont             = 0x0031
	wmInitDialog          = 0x0110
	wmCommand             = 0x0111
	wmTimer               = 0x0113
	wmCtlColorStatic      = 0x0138
	wmDPIChanged          = 0x02E0
	wmCopy                = 0x0301
	wmApp                 = 0x8000
	emGetSel              = 0x00B0
	emSetSel              = 0x00B1
	emLineScroll          = 0x00B6
	emGetFirstVisibleLine = 0x00CE

	wsPopup        = 0x80000000
	wsChild        = 0x40000000
	wsVisible      = 0x10000000
	wsCaption      = 0x00C00000
	wsVScroll      = 0x00200000
	wsHScroll      = 0x00100000
	wsSysMenu      = 0x00080000
	wsThickFrame   = 0x00040000
	wsMaximizeBox  = 0x00010000
	wsTabStop      = 0x00010000
	wsExClientEdge = 0x00000200

	dsSetFont    = 0x0040
	dsModalFrame = 0x0080
	dsCenter     = 0x0800

	esMultiline   = 0x0004
	esAutoVScroll = 0x0040
	esAutoHScroll = 0x0080
	esReadOnly    = 0x0800

	bsDefPushButton = 0x0001

	// Atoms of the predefined control classes in dialog templates.
	buttonClass = 0x0080
	editClass   = 0x0081

	colorWindow     = 5
	colorWindowText = 8
	swShowNormal    = 1
	mbIconWarning   = 0x30
	fixedPitch      = 1
	ffModern        = 0x30
)

type rect struct {
	Left, Top, Right, Bottom int32
}

type point struct {
	X, Y int32
}

type logFont struct {
	Height, Width, Escapement, Orientation, Weight       int32
	Italic, Underline, StrikeOut, CharSet                byte
	OutPrecision, ClipPrecision, Quality, PitchAndFamily byte
	FaceName                                             [32]uint16
}

func systemDirectory() string {
	buffer := make([]uint16, syscall.MAX_PATH)
	length, _, _ := procGetSystemDirectoryW.Call(uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)))
	if length == 0 || length >= uintptr(len(buffer)) {
		return ""
	}
	return syscall.UTF16ToString(buffer[:length])
}

func systemDLL(name string) *syscall.LazyDLL {
	return syscall.NewLazyDLL(filepath.Join(systemDir, name))
}

// call invokes a Win32 function whose arguments are all handles or values. A
// pointer must instead be converted to uintptr in the proc.Call expression
// itself, which keeps the memory it refers to alive and in place.
func call(proc *syscall.LazyProc, args ...uintptr) uintptr {
	result, _, _ := proc.Call(args...)
	return result
}

func send(hwnd, message, wParam, lParam uintptr) uintptr {
	return call(procSendMessageW, hwnd, message, wParam, lParam)
}

func enable(hwnd uintptr, enabled bool) {
	value := uintptr(0)
	if enabled {
		value = 1
	}
	call(procEnableWindow, hwnd, value)
}

func moveWindow(hwnd uintptr, x, y, width, height int32) {
	call(procMoveWindow, hwnd, uintptr(x), uintptr(y), uintptr(max(width, 0)), uintptr(max(height, 0)), 1)
}

func clientRect(hwnd uintptr) rect {
	var r rect
	procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	return r
}

func windowRect(hwnd uintptr) rect {
	var r rect
	procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	return r
}

func setWindowText(hwnd uintptr, text string) {
	procSetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(wide(text))))
}

func messageBox(owner uintptr, text, caption string, flags uintptr) {
	procMessageBoxW.Call(owner, uintptr(unsafe.Pointer(wide(text))), uintptr(unsafe.Pointer(wide(caption))), flags)
}

// shellOpen opens a file with its associated program. Like ShellExecuteW, it
// returns a value greater than 32 on success.
func shellOpen(owner uintptr, filename string) uintptr {
	result, _, _ := procShellExecuteW.Call(owner, uintptr(unsafe.Pointer(wide("open"))), uintptr(unsafe.Pointer(wide(filename))), 0, 0, swShowNormal)
	return result
}

func fontInfo(font uintptr) (logFont, bool) {
	var info logFont
	size, _, _ := procGetObjectW.Call(font, unsafe.Sizeof(info), uintptr(unsafe.Pointer(&info)))
	return info, size != 0
}

func createFont(info logFont) uintptr {
	font, _, _ := procCreateFontIndirectW.Call(uintptr(unsafe.Pointer(&info)))
	return font
}

// setMinTrackSize writes MINMAXINFO.ptMinTrackSize. Windows owns the structure,
// so it is written with RtlMoveMemory rather than through a Go pointer.
func setMinTrackSize(info uintptr, size point) {
	if info == 0 || size == (point{}) {
		return
	}
	const offset = 3 * unsafe.Sizeof(point{}) // after ptReserved, ptMaxSize and ptMaxPosition
	procRtlMoveMemory.Call(info+offset, uintptr(unsafe.Pointer(&size)), unsafe.Sizeof(size))
}

// wide converts s to a NUL-terminated UTF-16 string. Windows cannot show an
// embedded NUL, so it is dropped rather than cutting the text short.
func wide(s string) *uint16 {
	pointer, _ := syscall.UTF16PtrFromString(strings.ReplaceAll(s, "\x00", ""))
	return pointer
}
