//go:build freebsd && cgo

package main

/*
#cgo freebsd CFLAGS: -I${SRCDIR}/../../DoubleCommander-SDK
#include <string.h>
#include "wfxplugin.h"

static inline int wfx_call_progress(void *fn, int pluginNr, WCHAR *source,
	WCHAR *target, int percent) {
	return ((tProgressProcW)fn)(pluginNr, source, target, percent);
}

static inline void wfx_call_log(void *fn, int pluginNr, int messageType,
	WCHAR *message) {
	((tLogProcW)fn)(pluginNr, messageType, message);
}

static inline int wfx_call_request(void *fn, int pluginNr, int requestType,
	WCHAR *title, WCHAR *text, WCHAR *returned, int maxLen) {
	return ((tRequestProcW)fn)(pluginNr, requestType, title, text, returned,
		maxLen);
}

static inline void wfx_fill_find_data(WIN32_FIND_DATAW *data, int directory,
	DWORD sizeLow, DWORD sizeHigh, DWORD timeLow, DWORD timeHigh,
	const WCHAR *name) {
	memset(data, 0, sizeof(*data));
	data->dwFileAttributes = directory ? FILE_ATTRIBUTE_DIRECTORY : FILE_ATTRIBUTE_NORMAL;
	data->nFileSizeLow = sizeLow;
	data->nFileSizeHigh = sizeHigh;
	data->ftLastWriteTime.dwLowDateTime = timeLow;
	data->ftLastWriteTime.dwHighDateTime = timeHigh;
	if (name != NULL) {
		int i = 0;
		for (; i < MAX_PATH - 1 && name[i] != 0; i++) {
			data->cFileName[i] = name[i];
		}
		data->cFileName[i] = 0;
	}
}
*/
import "C"

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/example/wfxs3/internal/s3store"
	"github.com/example/wfxs3/internal/wfx"
)

const (
	maxWideString = 32768
	invalidHandle = ^uintptr(0)
)

// Return values of FsExecuteFileW, from the WFX SDK.
const (
	execOK       = 0
	execError    = 1
	execYourself = -1
)

var service = wfx.New(s3store.New())

var (
	hostMu sync.Mutex
	host   wfx.HostInfo

	callbacksMu sync.Mutex
	callbacks   callbackPointers
)

var executableInfo = sync.OnceValues(func() (string, string) {
	executable, err := os.Executable()
	if err != nil {
		return "", ""
	}
	return executable, ""
})

type callbackPointers struct {
	pluginNr C.int
	progress unsafe.Pointer
	log      unsafe.Pointer
	request  unsafe.Pointer
}

func main() {}

//export FsInitW
func FsInitW(pluginNr C.int, progress unsafe.Pointer, log unsafe.Pointer, request unsafe.Pointer) C.int {
	service.ResetConfig()
	hostMu.Lock()
	host.PluginNr = int(pluginNr)
	host.Initialized = time.Now()
	hostMu.Unlock()
	pointers := callbackPointers{
		pluginNr: pluginNr,
		progress: progress,
		log:      log,
		request:  request,
	}
	callbacksMu.Lock()
	callbacks = pointers
	callbacksMu.Unlock()
	service.SetCallbacks(wfx.Callbacks{
		Progress: func(source, target string, percent int) bool {
			return invokeProgress(pointers, source, target, percent)
		},
		Log: func(messageType int, message string) {
			invokeLog(pointers, messageType, message)
		},
		Request: func(requestType int, title, text, defaultText string) (string, bool) {
			return invokeRequest(pointers, requestType, title, text, defaultText)
		},
	})
	return 0
}

//export FsFindFirstW
func FsFindFirstW(remotePath *C.WCHAR, findData *C.WIN32_FIND_DATAW) uintptr {
	if findData == nil {
		return invalidHandle
	}
	token, entry, err := service.FindFirst(readWideString(remotePath))
	if err != nil {
		if !errors.Is(err, wfx.ErrEmptyDirectory) {
			service.ReportError(err)
		}
		return invalidHandle
	}
	fillFindData(findData, entry)
	return uintptr(token)
}

//export FsFindNextW
func FsFindNextW(handle uintptr, findData *C.WIN32_FIND_DATAW) C.int {
	if findData == nil {
		return 0
	}
	entry, ok, err := service.FindNext(uint64(handle))
	if err != nil || !ok {
		return 0
	}
	fillFindData(findData, entry)
	return 1
}

//export FsFindClose
func FsFindClose(handle uintptr) C.int {
	if handle != 0 && handle != invalidHandle {
		service.FindClose(uint64(handle))
	}
	return 0
}

//export FsGetFileW
func FsGetFileW(remoteName *C.WCHAR, localName *C.WCHAR, copyFlags C.int, remoteInfo *C.RemoteInfoStruct) C.int {
	status, err := service.GetFile(readWideString(remoteName), readWideString(localName),
		int(copyFlags), remoteInfoTime(remoteInfo))
	if err != nil && !errors.Is(err, wfx.ErrUserAbort) {
		service.ReportError(err)
	}
	return C.int(status)
}

//export FsPutFileW
func FsPutFileW(localName *C.WCHAR, remoteName *C.WCHAR, copyFlags C.int) C.int {
	status, err := service.PutFile(readWideString(localName), readWideString(remoteName), int(copyFlags))
	if err != nil && !errors.Is(err, wfx.ErrUserAbort) {
		service.ReportError(err)
	}
	return C.int(status)
}

//export FsDeleteFileW
func FsDeleteFileW(remoteName *C.WCHAR) C.int {
	ok, err := service.DeleteFile(readWideString(remoteName))
	if err != nil {
		service.ReportError(err)
		return 0
	}
	if ok {
		return 1
	}
	return 0
}

//export FsGetDefRootName
func FsGetDefRootName(rootName *C.char, maxLen C.int) {
	if rootName == nil || maxLen <= 0 {
		return
	}
	buffer := unsafe.Slice((*byte)(unsafe.Pointer(rootName)), int(maxLen))
	for i := range buffer {
		buffer[i] = 0
	}
	root := []byte("S3 API Endpoints")
	if len(root) >= len(buffer) {
		root = root[:len(buffer)-1]
	}
	copy(buffer, root)
}

//export FsSetDefaultParams
func FsSetDefaultParams(defaults *C.FsDefaultParamStruct) {
	if defaults == nil {
		return
	}
	raw := C.GoBytes(unsafe.Pointer(&defaults.DefaultIniName[0]), C.int(len(defaults.DefaultIniName)))
	iniName := decodeUTF8NUL(raw)
	hostMu.Lock()
	host.InterfaceVersion = interfaceVersion(uint32(defaults.PluginInterfaceVersionHi), uint32(defaults.PluginInterfaceVersionLow))
	host.DefaultIniName = iniName
	hostMu.Unlock()
	service.SetDefaultIniName(iniName)
}

//export FsExecuteFileW
func FsExecuteFileW(mainWin uintptr, remoteName *C.WCHAR, verb *C.WCHAR) C.int {
	switch classifyVerb(readWideString(verb)) {
	case verbOpen:
		return execYourself
	case verbProperties:
		showDebugText(readWideString(remoteName))
		return execOK
	}
	return execError
}

type verbKind int

const (
	verbOther verbKind = iota
	verbOpen
	verbProperties
)

func classifyVerb(verb string) verbKind {
	name, _, _ := strings.Cut(strings.TrimSpace(verb), " ")
	switch strings.ToLower(name) {
	case "open":
		return verbOpen
	case "properties":
		return verbProperties
	}
	return verbOther
}

func showDebugText(selected string) {
	report := service.DebugReport(currentHost(), selected)
	callbacksMu.Lock()
	pointers := callbacks
	callbacksMu.Unlock()
	if pointers.request != nil {
		_, _ = invokeRequest(pointers, wfx.RequestMessageOK, "WFX S3 debug info", report, "")
		return
	}
	if pointers.log != nil {
		invokeLog(pointers, wfx.MessageDetails, report)
	}
}

func currentHost() wfx.HostInfo {
	hostMu.Lock()
	info := host
	hostMu.Unlock()
	info.Executable, info.Version = executableInfo()
	return info
}

func interfaceVersion(high, low uint32) string {
	return fmt.Sprintf("%d.%02d", high, low)
}

func decodeUTF8NUL(value []byte) string {
	if end := bytes.IndexByte(value, 0); end >= 0 {
		value = value[:end]
	}
	return string(value)
}

func readWideString(value *C.WCHAR) string {
	if value == nil {
		return ""
	}
	values := unsafe.Slice((*uint16)(unsafe.Pointer(value)), maxWideString)
	length := 0
	for length < len(values) && values[length] != 0 {
		length++
	}
	return string(utf16.Decode(values[:length]))
}

func fillFindData(destination *C.WIN32_FIND_DATAW, entry wfx.FindData) {
	name := utf16.Encode([]rune(entry.Name))
	name = append(name, 0)
	low, high := splitUint64(uint64(maxInt64(entry.Size)))
	timeLow, timeHigh := fileTime(entry.LastModified)
	C.wfx_fill_find_data(
		destination,
		C.int(boolInt(entry.Directory)),
		C.DWORD(low),
		C.DWORD(high),
		C.DWORD(timeLow),
		C.DWORD(timeHigh),
		(*C.WCHAR)(unsafe.Pointer(&name[0])),
	)
	runtime.KeepAlive(name)
}

func splitUint64(value uint64) (uint32, uint32) {
	return uint32(value), uint32(value >> 32)
}

func maxInt64(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

const (
	noFileTimeLow  = 0xFFFFFFFE
	noFileTimeHigh = 0xFFFFFFFF
)

func fileTime(value time.Time) (uint32, uint32) {
	if value.IsZero() {
		return noFileTimeLow, noFileTimeHigh
	}
	const windowsEpochOffset = int64(11644473600)
	intervals := (value.Unix()+windowsEpochOffset)*10000000 + int64(value.Nanosecond()/100)
	if intervals < 0 {
		return noFileTimeLow, noFileTimeHigh
	}
	return splitUint64(uint64(intervals))
}

func timeFromFileTime(low, high uint32) time.Time {
	if low == noFileTimeLow && high == noFileTimeHigh {
		return time.Time{}
	}
	intervals := int64(uint64(high)<<32 | uint64(low))
	if intervals <= 0 {
		return time.Time{}
	}
	const windowsEpochOffset = int64(11644473600)
	const intervalsPerSecond = int64(10000000)
	return time.Unix(
		intervals/intervalsPerSecond-windowsEpochOffset,
		intervals%intervalsPerSecond*100,
	)
}

func remoteInfoTime(info *C.RemoteInfoStruct) time.Time {
	if info == nil {
		return time.Time{}
	}
	return timeFromFileTime(
		uint32(info.LastWriteTime.dwLowDateTime),
		uint32(info.LastWriteTime.dwHighDateTime),
	)
}

func invokeProgress(callbacks callbackPointers, source, target string, percent int) bool {
	if callbacks.progress == nil {
		return false
	}
	sourceUTF16 := utf16.Encode([]rune(source))
	sourceUTF16 = append(sourceUTF16, 0)
	targetUTF16 := utf16.Encode([]rune(target))
	targetUTF16 = append(targetUTF16, 0)
	result := C.wfx_call_progress(
		callbacks.progress,
		callbacks.pluginNr,
		(*C.WCHAR)(unsafe.Pointer(&sourceUTF16[0])),
		(*C.WCHAR)(unsafe.Pointer(&targetUTF16[0])),
		C.int(percent),
	)
	runtime.KeepAlive(sourceUTF16)
	runtime.KeepAlive(targetUTF16)
	return result != 0
}

func invokeLog(callbacks callbackPointers, messageType int, message string) {
	if callbacks.log == nil {
		return
	}
	messageUTF16 := utf16.Encode([]rune(message))
	messageUTF16 = append(messageUTF16, 0)
	C.wfx_call_log(
		callbacks.log,
		callbacks.pluginNr,
		C.int(messageType),
		(*C.WCHAR)(unsafe.Pointer(&messageUTF16[0])),
	)
	runtime.KeepAlive(messageUTF16)
}

func invokeRequest(callbacks callbackPointers, requestType int, title, text, defaultText string) (string, bool) {
	if callbacks.request == nil {
		return "", false
	}
	titleUTF16 := utf16.Encode([]rune(title))
	titleUTF16 = append(titleUTF16, 0)
	textUTF16 := utf16.Encode([]rune(text))
	textUTF16 = append(textUTF16, 0)
	returnedUTF16 := utf16.Encode([]rune(defaultText))
	const maxReturned = 4096
	if len(returnedUTF16) >= maxReturned {
		returnedUTF16 = returnedUTF16[:maxReturned-1]
	}
	returnedUTF16 = append(returnedUTF16, 0)
	if len(returnedUTF16) < maxReturned {
		returnedUTF16 = append(returnedUTF16, make([]uint16, maxReturned-len(returnedUTF16))...)
	}
	result := C.wfx_call_request(
		callbacks.request,
		callbacks.pluginNr,
		C.int(requestType),
		(*C.WCHAR)(unsafe.Pointer(&titleUTF16[0])),
		(*C.WCHAR)(unsafe.Pointer(&textUTF16[0])),
		(*C.WCHAR)(unsafe.Pointer(&returnedUTF16[0])),
		C.int(maxReturned),
	)
	runtime.KeepAlive(titleUTF16)
	runtime.KeepAlive(textUTF16)
	return readUTF16Buffer(returnedUTF16), result != 0
}

func readUTF16Buffer(value []uint16) string {
	length := 0
	for length < len(value) && value[length] != 0 {
		length++
	}
	return string(utf16.Decode(value[:length]))
}
