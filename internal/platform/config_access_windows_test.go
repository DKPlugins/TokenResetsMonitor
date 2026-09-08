//go:build windows

package platform

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/DKPlugins/TokenResetsMonitor/internal/fileio"
	"golang.org/x/sys/windows"
)

func TestServiceConfigRequiresExplicitReadGrant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	file, err := fileio.CreatePrivate(path)
	if err != nil {
		t.Fatal(err)
	}
	file.Close()
	if err := checkServiceConfigurationAccess(path); err == nil {
		t.Fatal("private configuration was accepted without service read access")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.User.Sid.String() + ")(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;LS)")
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	if err := checkServiceConfigurationAccess(path); err != nil {
		t.Fatalf("explicit service read rejected: %v", err)
	}
	if err := os.WriteFile(path, []byte("private fixture"), 0600); err != nil {
		t.Fatal(err)
	}
}
