//go:build windows

package state

import (
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows MoveFileEx (used by os.Rename) cannot replace a target that still has
// readers. POSIX rename semantics let shared-delete readers finish reading the
// old snapshot while new readers immediately open its atomic replacement.
func replaceStatusFile(source, target string) error {
	wrap := func(err error) error { return &os.LinkError{Op: "rename", Old: source, New: target, Err: err} }
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return wrap(err)
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return wrap(err)
	}
	to, err := windows.UTF16FromString(absTarget)
	if err != nil {
		return wrap(err)
	}
	handle, err := windows.CreateFile(from, windows.DELETE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return wrap(err)
	}
	defer windows.CloseHandle(handle)
	type renameInfo struct {
		Flags          uint32
		RootDirectory  windows.Handle
		FileNameLength uint32
		FileName       [1]uint16
	}
	var layout renameInfo
	buffer := make([]byte, int(unsafe.Offsetof(layout.FileName))+len(to)*2)
	info := (*renameInfo)(unsafe.Pointer(&buffer[0]))
	info.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS
	info.FileNameLength = uint32((len(to) - 1) * 2)
	copy(unsafe.Slice(&info.FileName[0], len(to)), to)
	if err := windows.SetFileInformationByHandle(handle, windows.FileRenameInfoEx, &buffer[0], uint32(len(buffer))); err != nil {
		return wrap(err)
	}
	return nil
}
