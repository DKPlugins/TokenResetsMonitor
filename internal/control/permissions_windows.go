//go:build windows

package control

import (
	"errors"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func checkPrivateDescriptor(path string, _ os.FileInfo) error {
	unsafePermissions := errors.New("local control descriptor permissions are not private")
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return unsafePermissions
	}
	user, userErr := windows.GetCurrentProcessToken().GetTokenUser()
	if userErr != nil {
		return unsafePermissions
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return unsafePermissions
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount == 0 {
		return unsafePermissions
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(dacl, i, &ace) != nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return unsafePermissions
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.Equals(owner) && !sid.Equals(user.User.Sid) && !sid.IsWellKnown(windows.WinLocalSystemSid) && !sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
			return unsafePermissions
		}
	}
	return nil
}
