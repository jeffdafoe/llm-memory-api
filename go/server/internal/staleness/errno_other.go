//go:build !unix

package staleness

import (
	"errors"
	"io/fs"
)

// errnoText off Unix knows only the codes io/fs can name; the check runs
// on the Linux server, and this keeps it building elsewhere.
func errnoText(err error) (code, desc string, ok bool) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "ENOENT", "no such file or directory", true
	case errors.Is(err, fs.ErrPermission):
		return "EACCES", "permission denied", true
	}
	return "", "", false
}
