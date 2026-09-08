//go:build windows

package logging

import (
	"github.com/DKPlugins/TokenResetsMonitor/internal/fileio"
	"os"
)

func createPrivateArchive(path string) (*os.File, error) { return fileio.CreatePrivate(path) }
