package fileio

import "errors"

// ErrUnsafePermissions means replacing a legacy file would retain broad access.
var ErrUnsafePermissions = errors.New("existing file permissions are not private")
