package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/DKPlugins/TokenResetsMonitor/internal/config"
)

func validateInstallation(path string, overrides map[string]string, out, errOut io.Writer) int {
	deferred, err := config.ValidateInstallation(path, overrides)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 2
	}
	if len(deferred) == 0 {
		fmt.Fprintln(out, "Configuration structure and available values are valid.")
	} else {
		fmt.Fprintln(out, "Configuration structure is valid; environment validation deferred for:", strings.Join(deferred, ", "))
		fmt.Fprintln(out, "Provide the referenced variables to the service before starting it. Startup requires full configuration validation.")
	}
	return 0
}
