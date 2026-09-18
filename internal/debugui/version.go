//go:build windows

package debugui

import (
	"encoding/binary"
	"fmt"
	"syscall"
	"unsafe"
)

// fixedFileInfoSize is the size of VS_FIXEDFILEINFO.
const fixedFileInfoSize = 52

// FileVersion returns the file version from an executable's version resource,
// such as "11.51.0.0", or "" when it has none.
func FileVersion(filename string) string {
	name, err := syscall.UTF16PtrFromString(filename)
	if err != nil {
		return ""
	}
	var handle uint32
	size, _, _ := procGetFileVersionInfoSizeW.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&handle)))
	if size == 0 {
		return ""
	}
	data := make([]byte, size)
	if ok, _, _ := procGetFileVersionInfoW.Call(uintptr(unsafe.Pointer(name)), 0, size, uintptr(unsafe.Pointer(&data[0]))); ok == 0 {
		return ""
	}
	var info uintptr
	var length uint32
	if ok, _, _ := procVerQueryValueW.Call(uintptr(unsafe.Pointer(&data[0])), uintptr(unsafe.Pointer(wide(`\`))), uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&length))); ok == 0 || length < fixedFileInfoSize {
		return ""
	}
	// VerQueryValueW points into data, so read VS_FIXEDFILEINFO by its offset.
	offset := info - uintptr(unsafe.Pointer(&data[0]))
	if offset > uintptr(len(data)) || uintptr(len(data))-offset < fixedFileInfoSize {
		return ""
	}
	fixed := data[offset:]
	if binary.LittleEndian.Uint32(fixed) != 0xFEEF04BD {
		return ""
	}
	major := binary.LittleEndian.Uint32(fixed[8:])
	minor := binary.LittleEndian.Uint32(fixed[12:])
	return fmt.Sprintf("%d.%d.%d.%d", major>>16, major&0xFFFF, minor>>16, minor&0xFFFF)
}
