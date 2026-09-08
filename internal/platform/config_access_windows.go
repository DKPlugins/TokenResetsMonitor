//go:build windows

package platform

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Require an explicit read grant instead of reporting installation success for a
// private init file the service cannot read. The installer applies this grant;
// manual installations receive an actionable error without an ACL rewrite.
func checkServiceConfigurationAccess(path string) error {
	denied := errors.New("configuration needs explicit LOCAL SERVICE Read permissions; use the installer or Windows Security settings before service install (keep the file private)")
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return denied
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		return denied
	}
	const readMask = windows.FILE_READ_DATA | windows.FILE_READ_ATTRIBUTES | windows.FILE_READ_EA | windows.READ_CONTROL | windows.SYNCHRONIZE
	var granted windows.ACCESS_MASK
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(dacl, i, &ace) != nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return denied
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsWellKnown(windows.WinLocalServiceSid) {
			continue
		}
		granted |= ace.Mask
	}
	if granted&windows.GENERIC_ALL != 0 || granted&windows.GENERIC_READ != 0 || granted&readMask == readMask {
		return nil
	}
	return denied
}
