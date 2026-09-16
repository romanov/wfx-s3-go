//go:build windows && cgo

package main

/*
#cgo windows CFLAGS: -I${SRCDIR}/../../WFX-SDK-master/src
#include <windows.h>
#include <wchar.h>
#include <string.h>

typedef struct {
    DWORD SizeLow, SizeHigh;
    FILETIME LastWriteTime;
    int Attr;
} RemoteInfoStruct;

typedef struct {
    int size;
    DWORD PluginInterfaceVersionLow;
    DWORD PluginInterfaceVersionHi;
    char DefaultIniName[MAX_PATH];
} FsDefaultParamStruct;

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
        wcsncpy(data->cFileName, name, MAX_PATH - 1);
        data->cFileName[MAX_PATH - 1] = L'\0';
    }
}

static inline void wfx_set_last_error(DWORD code) {
    SetLastError(code);
}

// Total Commander unloads a WFX DLL immediately after calling
// FsGetDefRootName during plugin installation. Go c-shared DLLs cannot be
// safely unloaded while their runtime is still active, so pin this module in
// the process before returning control to Total Commander.
static inline int wfx_pin_module(void) {
    HMODULE module = NULL;
    return GetModuleHandleExW(
        GET_MODULE_HANDLE_EX_FLAG_FROM_ADDRESS |
            GET_MODULE_HANDLE_EX_FLAG_PIN,
        (LPCWSTR)(const void *)&wfx_pin_module,
        &module);
}
*/
import "C"

import (
	"errors"
	"os"
	"runtime"
	"syscall"
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

var service = wfx.New(s3store.New())

type callbackPointers struct {
	pluginNr uintptr
	progress uintptr
	log      uintptr
	request  uintptr
}

func main() {}

func pinModule() {
	// Pinning is best effort. The plugin can still service the current call if
	// the Windows loader refuses the request, while successful pinning prevents
	// Total Commander from unloading the Go runtime after installation.
	_ = C.wfx_pin_module()
}

//export FsInitW
func FsInitW(pluginNr C.int, progress unsafe.Pointer, log unsafe.Pointer, request unsafe.Pointer) C.int {
	pinModule()
	service.ResetConfig()
	pointers := callbackPointers{
		pluginNr: uintptr(pluginNr),
		progress: uintptr(progress),
		log:      uintptr(log),
		request:  uintptr(request),
	}
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
func FsFindFirstW(remotePath *C.wchar_t, findData *C.WIN32_FIND_DATAW) unsafe.Pointer {
	if findData == nil {
		setLastError(uint32(C.ERROR_INVALID_PARAMETER))
		return unsafe.Pointer(uintptr(invalidHandle))
	}
	token, entry, err := service.FindFirst(readWideString(remotePath))
	if err != nil {
		setFindError(err)
		if !errors.Is(err, wfx.ErrEmptyDirectory) {
			service.ReportError(err)
		}
		return unsafe.Pointer(uintptr(invalidHandle))
	}
	fillFindData(findData, entry)
	return unsafe.Pointer(uintptr(token))
}

//export FsFindNextW
func FsFindNextW(handle unsafe.Pointer, findData *C.WIN32_FIND_DATAW) C.int {
	if findData == nil {
		setLastError(uint32(C.ERROR_INVALID_PARAMETER))
		return 0
	}
	entry, ok, err := service.FindNext(handleToken(handle))
	if err != nil {
		setLastError(uint32(C.ERROR_INVALID_HANDLE))
		return 0
	}
	if !ok {
		return 0
	}
	fillFindData(findData, entry)
	return 1
}

//export FsFindClose
func FsFindClose(handle unsafe.Pointer) C.int {
	token := handleToken(handle)
	if token != 0 && token != uint64(invalidHandle) {
		service.FindClose(token)
	}
	return 0
}

//export FsGetFileW
func FsGetFileW(remoteName *C.wchar_t, localName *C.wchar_t, copyFlags C.int, remoteInfo *C.RemoteInfoStruct) C.int {
	_ = remoteInfo
	status, err := service.GetFile(readWideString(remoteName), readWideString(localName), int(copyFlags))
	if err != nil && !errors.Is(err, wfx.ErrUserAbort) {
		service.ReportError(err)
	}
	return C.int(status)
}

//export FsPutFileW
func FsPutFileW(localName *C.wchar_t, remoteName *C.wchar_t, copyFlags C.int) C.int {
	status, err := service.PutFile(readWideString(localName), readWideString(remoteName), int(copyFlags))
	if err != nil && !errors.Is(err, wfx.ErrUserAbort) {
		service.ReportError(err)
	}
	return C.int(status)
}

//export FsDeleteFileW
func FsDeleteFileW(remoteName *C.wchar_t) C.int {
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
	pinModule()
	if rootName == nil || maxLen <= 0 {
		return
	}
	buffer := unsafe.Slice((*byte)(unsafe.Pointer(rootName)), int(maxLen))
	for i := range buffer {
		buffer[i] = 0
	}
	root := []byte("S3 (Go)")
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
	name := C.GoString((*C.char)(unsafe.Pointer(&defaults.DefaultIniName[0])))
	service.SetDefaultIniName(name)
}

func readWideString(value *C.wchar_t) string {
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

func fileTime(value time.Time) (uint32, uint32) {
	if value.IsZero() {
		return 0, 0
	}
	const windowsEpochOffset = int64(11644473600)
	intervals := (value.Unix()+windowsEpochOffset)*10000000 + int64(value.Nanosecond()/100)
	if intervals < 0 {
		return 0, 0
	}
	return splitUint64(uint64(intervals))
}

func handleToken(handle unsafe.Pointer) uint64 {
	return uint64(uintptr(handle))
}

func setLastError(code uint32) {
	C.wfx_set_last_error(C.DWORD(code))
}

func setFindError(err error) {
	if errors.Is(err, wfx.ErrEmptyDirectory) {
		setLastError(uint32(C.ERROR_NO_MORE_FILES))
		return
	}
	setLastError(uint32(C.ERROR_BAD_NET_RESP))
	if errors.Is(err, os.ErrNotExist) {
		setLastError(uint32(C.ERROR_PATH_NOT_FOUND))
	}
}

func invokeProgress(callbacks callbackPointers, source, target string, percent int) bool {
	if callbacks.progress == 0 {
		return false
	}
	sourceUTF16 := utf16.Encode([]rune(source))
	sourceUTF16 = append(sourceUTF16, 0)
	targetUTF16 := utf16.Encode([]rune(target))
	targetUTF16 = append(targetUTF16, 0)
	r1, _, _ := syscall.SyscallN(callbacks.progress,
		callbacks.pluginNr,
		uintptr(unsafe.Pointer(&sourceUTF16[0])),
		uintptr(unsafe.Pointer(&targetUTF16[0])),
		uintptr(percent),
	)
	runtime.KeepAlive(sourceUTF16)
	runtime.KeepAlive(targetUTF16)
	return r1 != 0
}

func invokeLog(callbacks callbackPointers, messageType int, message string) {
	if callbacks.log == 0 {
		return
	}
	messageUTF16 := utf16.Encode([]rune(message))
	messageUTF16 = append(messageUTF16, 0)
	syscall.SyscallN(callbacks.log,
		callbacks.pluginNr,
		uintptr(messageType),
		uintptr(unsafe.Pointer(&messageUTF16[0])),
	)
	runtime.KeepAlive(messageUTF16)
}

func invokeRequest(callbacks callbackPointers, requestType int, title, text, defaultText string) (string, bool) {
	if callbacks.request == 0 {
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
	r1, _, _ := syscall.SyscallN(callbacks.request,
		callbacks.pluginNr,
		uintptr(requestType),
		uintptr(unsafe.Pointer(&titleUTF16[0])),
		uintptr(unsafe.Pointer(&textUTF16[0])),
		uintptr(unsafe.Pointer(&returnedUTF16[0])),
		uintptr(maxReturned),
	)
	runtime.KeepAlive(titleUTF16)
	runtime.KeepAlive(textUTF16)
	return readUTF16Buffer(returnedUTF16), r1 != 0
}

func readUTF16Buffer(value []uint16) string {
	length := 0
	for length < len(value) && value[length] != 0 {
		length++
	}
	return string(utf16.Decode(value[:length]))
}
