//go:build windows

package logging

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows ignores Unix mode bits. Apply a protected DACL in CreateFile itself,
// so an archive never inherits a shared directory's readable permissions.
func createPrivateArchive(path string) (*os.File, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, errors.New("cannot determine archive owner")
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")(A;;FA;;;SY)(A;;FA;;;BA)")
	if err != nil {
		return nil, errors.New("cannot construct private archive permissions")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, errors.New("invalid archive path")
	}
	// An alternate data stream shares its containing file's security descriptor.
	if strings.Contains(absolute[len(filepath.VolumeName(absolute)):], ":") {
		return nil, errors.New("archive output must be a regular file, not a data stream")
	}
	name, err := windows.UTF16PtrFromString(absolute)
	if err != nil {
		return nil, errors.New("invalid archive path")
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	handle, err := windows.CreateFile(name, windows.GENERIC_WRITE|windows.READ_CONTROL, 0, &attributes, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	// Fail before writing any diagnostic bytes if this filesystem cannot retain
	// the protected DACL. The exclusive handle also prevents reads while writing.
	actual, securityErr := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	private := false
	if securityErr == nil {
		control, _, controlErr := actual.Control()
		dacl, _, daclErr := actual.DACL()
		private = controlErr == nil && daclErr == nil && dacl != nil && dacl.AceCount > 0 && control&windows.SE_DACL_PROTECTED != 0
	}
	if !private {
		file.Close()
		os.Remove(path)
		return nil, errors.New("archive filesystem does not preserve private permissions")
	}
	return file, nil
}
