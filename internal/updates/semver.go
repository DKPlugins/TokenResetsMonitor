// Package updates checks published releases without downloading executable code.
package updates

import (
	"errors"
	"regexp"
	"strings"
)

var versionPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`)

func parseVersion(raw string) ([]string, error) {
	p := versionPattern.FindStringSubmatch(raw)
	if p == nil {
		return nil, errors.New("invalid semantic version")
	}
	for _, id := range strings.Split(p[4], ".") {
		if numeric(id) && len(id) > 1 && id[0] == '0' {
			return nil, errors.New("invalid semantic version")
		}
	}
	return p, nil
}

func numeric(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func compareNumber(a, b string) int {
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	return strings.Compare(a, b)
}

// Compare implements SemVer precedence, including numeric prerelease identifiers.
func Compare(a, b string) (int, error) {
	x, err := parseVersion(a)
	if err != nil {
		return 0, err
	}
	y, err := parseVersion(b)
	if err != nil {
		return 0, err
	}
	for i := 1; i <= 3; i++ {
		if c := compareNumber(x[i], y[i]); c != 0 {
			return c, nil
		}
	}
	if x[4] == y[4] {
		return 0, nil
	}
	if x[4] == "" {
		return 1, nil
	}
	if y[4] == "" {
		return -1, nil
	}
	xp, yp := strings.Split(x[4], "."), strings.Split(y[4], ".")
	for i := 0; i < len(xp) && i < len(yp); i++ {
		a, b := xp[i], yp[i]
		an, bn := numeric(a), numeric(b)
		var c int
		switch {
		case an && bn:
			c = compareNumber(a, b)
		case an:
			c = -1
		case bn:
			c = 1
		default:
			c = strings.Compare(a, b)
		}
		if c != 0 {
			return c, nil
		}
	}
	if len(xp) < len(yp) {
		return -1, nil
	}
	return 1, nil
}
