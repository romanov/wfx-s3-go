//go:build windows

// Package debugui shows the plugin's modal debug dialog. It needs only the
// syscall package, and it knows nothing about S3: the plugin supplies the
// report and the actions behind the buttons.
package debugui

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

// Actions connects the dialog to the plugin. The dialog calls them on the
// thread that called Show.
type Actions struct {
	// Report returns the text to show, with "\n" line endings.
	Report func() string
	// ConfigPath returns the settings file to open, or "" when it is unknown.
	ConfigPath func() string
	// StartTest starts a connection test in the background. It returns false
	// when a test is already running.
	StartTest func() bool
	// TestRunning reports whether the connection test is still running.
	TestRunning func() bool
}

// Layout in dialog units, which scale with the dialog font and the DPI.
const (
	dialogWidth  = 440
	dialogHeight = 280
	minWidth     = 320
	minHeight    = 160
	margin       = 7
	gap          = 4
	buttonHeight = 14
)

type button struct {
	id    int
	label string
	width int32 // dialog units
	right bool  // aligned to the right edge of the dialog
	style uint32
}

var buttons = []button{
	{id: idTest, label: "&Test connections", width: 72},
	{id: idOpenINI, label: "&Open wfxs3.ini", width: 64},
	{id: idCopy, label: "Copy &all", width: 50, right: true},
	{id: idRefresh, label: "&Refresh", width: 50, right: true},
	{id: idOK, label: "Close", width: 50, right: true, style: bsDefPushButton},
}

const (
	testTimer        = 1
	testPollInterval = 250 // milliseconds

	// wmApplyFont restores the text font after a DPI change.
	wmApplyFont = wmApp + 1
)

type dialog struct {
	title   string
	actions Actions

	hwnd    uintptr
	text    uintptr
	font    uintptr // the text's monospace font, owned by the dialog
	minSize point
	polling bool
	failure error
}

var (
	// Only one dialog can be open: the dialog procedure finds it here.
	active     atomic.Pointer[dialog]
	dialogProc = syscall.NewCallback(dialogMessage)
)

// Show displays the modal debug dialog, owned by the owner window, and
// returns when the user closes it. owner may be 0 for a dialog without an
// owner.
func Show(owner uintptr, title string, actions Actions) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	d := &dialog{title: title, actions: actions}
	if !active.CompareAndSwap(nil, d) {
		return errors.New("the debug dialog is already open")
	}
	defer active.Store(nil)

	template := dialogTemplate(title)
	result, _, err := procDialogBoxIndirectParamW.Call(0, uintptr(unsafe.Pointer(&template[0])), owner, dialogProc, 0)
	if d.font != 0 {
		call(procDeleteObject, d.font)
	}
	switch {
	case d.failure != nil:
		return d.failure
	case result == ^uintptr(0):
		return fmt.Errorf("show the debug dialog: %w", err)
	}
	return nil
}

// dialogMessage is the dialog procedure. As DLGPROC requires, it returns
// nonzero for the messages that it handles.
func dialogMessage(hwnd, message, wParam, lParam uintptr) (result uintptr) {
	d := active.Load()
	if d == nil {
		return 0
	}
	defer func() {
		// A panic must not unwind through user32 into Total Commander.
		if r := recover(); r != nil {
			d.failure = fmt.Errorf("debug dialog: %v", r)
			call(procEndDialog, hwnd, idCancel)
			result = 0
		}
	}()
	if message == wmInitDialog {
		d.hwnd = hwnd
		d.initialize()
		return 0 // initialize has set the focus
	}
	if d.hwnd == 0 {
		return 0 // the dialog is still being created
	}
	switch message {
	case wmCommand:
		return d.command(int(wParam & 0xFFFF))
	case wmTimer:
		if wParam == testTimer {
			d.pollTest()
		}
		return 1
	case wmSize:
		d.layout()
		return 1
	case wmGetMinMaxInfo:
		setMinTrackSize(lParam, d.minSize)
		return 1
	case wmCtlColorStatic:
		// A read-only edit control would otherwise have a dialog background.
		if lParam != d.text {
			return 0
		}
		call(procSetTextColor, wParam, call(procGetSysColor, colorWindowText))
		call(procSetBkColor, wParam, call(procGetSysColor, colorWindow))
		return call(procGetSysColorBrush, colorWindow)
	case wmDPIChanged:
		// Let the dialog manager rescale the dialog, which resets the fonts of
		// its controls, and restore the text font afterwards.
		call(procPostMessageW, hwnd, wmApplyFont, 0, 0)
		return 0
	case wmApplyFont:
		d.applyFont()
		d.updateMinSize()
		d.layout()
		return 1
	case wmClose:
		call(procEndDialog, hwnd, idCancel)
		return 1
	}
	return 0
}

func (d *dialog) initialize() {
	d.text = d.item(idText)
	d.applyFont()
	d.showReport()
	if d.actions.ConfigPath() == "" {
		enable(d.item(idOpenINI), false)
	}
	d.updateMinSize()
	d.layout()
	d.pollTest()
	// Focusing the text would make the dialog manager select all of it.
	call(procSetFocus, d.item(idOK))
}

func (d *dialog) command(id int) uintptr {
	switch id {
	case idOK, idCancel:
		call(procEndDialog, d.hwnd, uintptr(id))
	case idCopy:
		d.copyAll()
	case idRefresh:
		d.refresh()
	case idOpenINI:
		d.openConfig()
	case idTest:
		d.actions.StartTest()
		d.refresh()
		d.pollTest()
	default:
		return 0
	}
	return 1
}

func (d *dialog) item(id int) uintptr {
	return call(procGetDlgItem, d.hwnd, uintptr(id))
}

// scale converts dialog units to pixels.
func (d *dialog) scale(x, y int32) (int32, int32) {
	r := rect{Right: x, Bottom: y}
	procMapDialogRect.Call(d.hwnd, uintptr(unsafe.Pointer(&r)))
	return r.Right, r.Bottom
}

// applyFont gives the text a monospace font the size of the dialog font, which
// the dialog manager has already scaled for the monitor.
func (d *dialog) applyFont() {
	info, ok := fontInfo(send(d.hwnd, wmGetFont, 0, 0))
	if !ok {
		return
	}
	info.FaceName = [32]uint16{}
	copy(info.FaceName[:len(info.FaceName)-1], utf16.Encode([]rune("Consolas")))
	info.PitchAndFamily = fixedPitch | ffModern
	font := createFont(info)
	if font == 0 {
		return
	}
	send(d.text, wmSetFont, font, 1)
	if d.font != 0 {
		call(procDeleteObject, d.font)
	}
	d.font = font
}

func (d *dialog) updateMinSize() {
	window, client := windowRect(d.hwnd), clientRect(d.hwnd)
	width, height := d.scale(minWidth, minHeight)
	d.minSize = point{
		X: width + (window.Right - window.Left) - client.Right,
		Y: height + (window.Bottom - window.Top) - client.Bottom,
	}
}

// layout fills the dialog with the text and puts the buttons in a row below
// it, some aligned left and some right.
func (d *dialog) layout() {
	client := clientRect(d.hwnd)
	marginX, marginY := d.scale(margin, margin)
	gapX, gapY := d.scale(gap, gap)
	_, height := d.scale(0, buttonHeight)
	top := client.Bottom - marginY - height
	moveWindow(d.text, marginX, marginY, client.Right-2*marginX, top-gapY-marginY)

	left, right := marginX, client.Right-marginX
	for _, b := range buttons {
		if !b.right {
			width, _ := d.scale(b.width, 0)
			moveWindow(d.item(b.id), left, top, width, height)
			left += width + gapX
		}
	}
	for i := len(buttons) - 1; i >= 0; i-- {
		if b := buttons[i]; b.right {
			width, _ := d.scale(b.width, 0)
			right -= width
			moveWindow(d.item(b.id), right, top, width, height)
			right -= gapX
		}
	}
}

func (d *dialog) showReport() {
	report := strings.ReplaceAll(d.actions.Report(), "\r\n", "\n")
	setWindowText(d.text, strings.ReplaceAll(report, "\n", "\r\n"))
}

// refresh shows a new report without scrolling back to the top.
func (d *dialog) refresh() {
	first := send(d.text, emGetFirstVisibleLine, 0, 0)
	d.showReport()
	send(d.text, emLineScroll, 0, first)
}

func (d *dialog) copyAll() {
	var start, end uint32
	procSendMessageW.Call(d.text, emGetSel, uintptr(unsafe.Pointer(&start)), uintptr(unsafe.Pointer(&end)))
	send(d.text, emSetSel, 0, ^uintptr(0))
	send(d.text, wmCopy, 0, 0)
	send(d.text, emSetSel, uintptr(start), uintptr(end))
}

func (d *dialog) openConfig() {
	filename := d.actions.ConfigPath()
	if filename == "" {
		return
	}
	code := shellOpen(d.hwnd, filename)
	if code > 32 {
		return
	}
	message := fmt.Sprintf("Could not open %s (ShellExecute error %d).", filename, code)
	if _, err := os.Stat(filename); errors.Is(err, os.ErrNotExist) {
		message = filename + " does not exist. Create it from wfxs3.ini.example to add a profile."
	}
	messageBox(d.hwnd, message, d.title, mbIconWarning)
}

// pollTest keeps the Test button and its timer in step with the connection
// test, and shows the results once the test has finished.
func (d *dialog) pollTest() {
	running := d.actions.TestRunning()
	switch {
	case running && !d.polling:
		d.polling = true
		test := d.item(idTest)
		if call(procGetFocus) == test {
			// A disabled control cannot keep the focus.
			send(d.hwnd, wmNextDlgCtl, d.item(idOK), 1)
		}
		enable(test, false)
		call(procSetTimer, d.hwnd, testTimer, testPollInterval, 0)
	case !running && d.polling:
		d.polling = false
		call(procKillTimer, d.hwnd, testTimer)
		enable(d.item(idTest), true)
		d.refresh()
	}
}

// dialogTemplate builds the in-memory DLGTEMPLATE. The controls get their
// positions from layout.
func dialogTemplate(title string) []uint32 {
	var t templateWriter
	t.dword(wsPopup | wsCaption | wsSysMenu | wsThickFrame | wsMaximizeBox | dsModalFrame | dsSetFont | dsCenter)
	t.dword(0) // extended style
	t.word(uint16(1 + len(buttons)))
	t.word(0) // x and y: DS_CENTER positions the dialog
	t.word(0)
	t.word(dialogWidth)
	t.word(dialogHeight)
	t.word(0) // no menu
	t.word(0) // the standard dialog class
	t.text(title)
	t.word(9) // font size in points
	t.text("Segoe UI")
	t.item(wsChild|wsVisible|wsTabStop|wsVScroll|wsHScroll|esMultiline|esReadOnly|esAutoVScroll|esAutoHScroll, wsExClientEdge, idText, editClass, "")
	for _, b := range buttons {
		t.item(wsChild|wsVisible|wsTabStop|b.style, 0, b.id, buttonClass, b.label)
	}
	return t.aligned()
}

type templateWriter struct {
	words []uint16
}

func (t *templateWriter) word(value uint16) {
	t.words = append(t.words, value)
}

func (t *templateWriter) dword(value uint32) {
	t.words = append(t.words, uint16(value), uint16(value>>16))
}

func (t *templateWriter) text(value string) {
	t.words = append(t.words, utf16.Encode([]rune(strings.ReplaceAll(value, "\x00", "")))...)
	t.words = append(t.words, 0)
}

// align pads to a DWORD boundary, where every DLGITEMTEMPLATE must start.
func (t *templateWriter) align() {
	if len(t.words)%2 != 0 {
		t.words = append(t.words, 0)
	}
}

func (t *templateWriter) item(style, extendedStyle uint32, id int, class uint16, text string) {
	t.align()
	t.dword(style)
	t.dword(extendedStyle)
	for range 4 {
		t.word(0) // x, y, width and height
	}
	t.word(uint16(id))
	t.word(0xFFFF) // a predefined class, identified by its atom
	t.word(class)
	t.text(text)
	t.word(0) // no creation data
}

// aligned returns the template in a DWORD-aligned buffer, as
// DialogBoxIndirectParamW requires.
func (t *templateWriter) aligned() []uint32 {
	t.align()
	buffer := make([]uint32, len(t.words)/2)
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(&buffer[0])), len(t.words)), t.words)
	return buffer
}
