//go:build windows

package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
	"golang.org/x/sys/windows"
)

func sharedArchiveDirectory(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	// Prove that broad inheritable parent permissions do not reach the archive.
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err = windows.SetNamedSecurityInfo(directory, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	return directory
}

func assertPrivateArchiveDescriptor(t *testing.T, sd *windows.SECURITY_DESCRIPTOR) {
	t.Helper()
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("archive inherits directory permissions")
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount == 0 {
		t.Fatal("archive lacks an access-control list")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	seenUser := false
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			t.Fatal("unexpected archive permission entry")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if sid.Equals(user.User.Sid) {
			seenUser = true
			continue
		}
		if !sid.IsWellKnown(windows.WinLocalSystemSid) && !sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) {
			t.Fatalf("archive grants access to another principal: %s", sid.String())
		}
	}
	if !seenUser {
		t.Fatal("archive owner cannot access its file")
	}
}

func TestPrivateArchiveIsProtectedAtCreationInSharedDirectory(t *testing.T) {
	path := filepath.Join(sharedArchiveDirectory(t), "diagnostics.zip")
	file, err := createPrivateArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	// Inspect the live handle before writing data or closing: permissions must
	// already be private, not tightened only after export completes.
	sd, err := windows.GetSecurityInfo(windows.Handle(file.Fd()), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	assertPrivateArchiveDescriptor(t, sd)
	if _, err = file.WriteString("sensitive diagnostics"); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = createPrivateArchive(path); !os.IsExist(err) {
		t.Fatalf("existing archive was not refused: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "sensitive diagnostics" {
		t.Fatalf("existing archive changed: %q %v", data, err)
	}
}

func TestExportKeepsPrivateWindowsACLAfterClosing(t *testing.T) {
	path := filepath.Join(sharedArchiveDirectory(t), "diagnostics.zip")
	input := `{"time":"` + time.Now().UTC().Format(time.RFC3339Nano) + `","msg":"record"}` + "\n"
	if err := Export(config.Defaults(), ExportOptions{Since: time.Hour, Output: path, Input: "-", Stdin: strings.NewReader(input)}); err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	assertPrivateArchiveDescriptor(t, sd)
}

func TestPrivateArchiveRejectsAlternateDataStreams(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.txt")
	if err := os.WriteFile(path, []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := createPrivateArchive(path + ":diagnostics.zip"); err == nil {
		t.Fatal("alternate data stream accepted")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "existing" {
		t.Fatal("containing file changed")
	}
}
