//go:build windows

package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestConfigCreationBackupAndReplacementWindowsACL(t *testing.T) {
	dir := t.TempDir()
	parent, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	parentACL, _, err := parent.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, parentACL, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := WriteNewBytes(path, []byte("config_version: 1\n")); err != nil {
		t.Fatal(err)
	}
	assertConfigACL(t, path, false)
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	serviceSD, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;LS)")
	if err != nil {
		t.Fatal(err)
	}
	serviceACL, _, err := serviceSD.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, serviceACL, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(path, true); err != nil {
		t.Fatal(err)
	}
	assertConfigACL(t, path, true)
	backups, err := filepath.Glob(path + ".backup-*")
	if err != nil || len(backups) != 1 {
		t.Fatal("migration backup missing")
	}
	assertConfigACL(t, backups[0], false)
}

func assertConfigACL(t *testing.T, path string, wantService bool) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("configuration inherited a shared ACL")
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount == 0 {
		t.Fatal("configuration lacks a protected ACL")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	seenService := false
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if sid.IsWellKnown(windows.WinLocalServiceSid) {
			seenService = true
			continue
		}
		if !sid.Equals(user.User.Sid) && !sid.IsWellKnown(windows.WinLocalSystemSid) && !sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
			t.Fatalf("unexpected principal has config access: %s", sid.String())
		}
	}
	if seenService != wantService {
		t.Fatalf("LocalService read access lost or unexpectedly granted: %t", seenService)
	}
}

func TestMigrationRefusesSharedWindowsACLWithoutChangingConfig(t *testing.T) {
	dir := t.TempDir()
	shared, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := shared.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "legacy.yaml")
	original := []byte("config_version: 1\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(path, true); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("unsafe migration not explained: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatal("unsafe legacy config was changed")
	}
}

func TestMigrationAcceptsSafeInheritedWindowsACL(t *testing.T) {
	dir := t.TempDir()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	safe, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + user.User.Sid.String() + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := safe.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "legacy.yaml")
	if err := os.WriteFile(path, []byte("config_version: 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(path, true); err != nil {
		t.Fatalf("safe inherited permissions rejected: %v", err)
	}
}
