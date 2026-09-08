//go:build windows

package fileio

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// CreatePrivate applies its protected DACL during creation, before any secret
// bytes are written. Chmod alone does not establish private access on Windows.
func CreatePrivate(path string) (*os.File, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, errors.New("cannot determine file owner")
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")(A;;FA;;;SY)(A;;FA;;;BA)")
	if err != nil {
		return nil, errors.New("cannot construct private file permissions")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, errors.New("invalid private file path")
	}
	if strings.Contains(absolute[len(filepath.VolumeName(absolute)):], ":") {
		return nil, errors.New("private output must be a regular file, not a data stream")
	}
	name, err := windows.UTF16PtrFromString(absolute)
	if err != nil {
		return nil, errors.New("invalid private file path")
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	handle, err := windows.CreateFile(name, windows.GENERIC_WRITE|windows.READ_CONTROL, 0, &attributes, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
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
		return nil, errors.New("filesystem does not preserve private permissions")
	}
	return file, nil
}

var replaceFile = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReplaceFileW")

// ReplaceFileW preserves the existing file's DACL, including an explicit
// LocalService read grant. Do not ignore permission-merge failures.
func ReplacePrivate(temp, target string) error {
	if err := validatePrivateTarget(target); err != nil {
		return err
	}
	from, err := windows.UTF16PtrFromString(temp)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	result, _, callErr := replaceFile.Call(uintptr(unsafe.Pointer(to)), uintptr(unsafe.Pointer(from)), 0, 0, 0, 0)
	if result == 0 {
		return callErr
	}
	return nil
}

// Refuse an old shared ACL instead of letting ReplaceFileW silently
// copy it onto a newly protected temporary file. Administrators can deliberately
// establish the intended service ACL before retrying the edit.
func validatePrivateTarget(path string) error {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount == 0 {
		return ErrUnsafePermissions
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return ErrUnsafePermissions
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if sid.Equals(user.User.Sid) || sid.IsWellKnown(windows.WinLocalSystemSid) || sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
			continue
		}
		if sid.IsWellKnown(windows.WinLocalServiceSid) {
			const writeAccess = windows.GENERIC_WRITE | windows.GENERIC_ALL | windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER | windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA | windows.FILE_WRITE_EA | windows.FILE_WRITE_ATTRIBUTES
			if ace.Mask&writeAccess == 0 {
				continue
			}
		}
		return ErrUnsafePermissions
	}
	return nil
}
