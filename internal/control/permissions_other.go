//go:build !windows

package control

import (
	"errors"
	"os"
)

func checkPrivateDescriptor(_ string, info os.FileInfo) error {
	if info.Mode().Perm()&0077 != 0 {
		return errors.New("local control descriptor permissions are not private")
	}
	return nil
}
