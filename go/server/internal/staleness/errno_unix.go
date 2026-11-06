//go:build unix

package staleness

import (
	"errors"
	"syscall"
)

// libuv's code names and descriptions, which Node's fs errors carry.
var errnoNames = map[syscall.Errno][2]string{
	syscall.ENOENT:       {"ENOENT", "no such file or directory"},
	syscall.EACCES:       {"EACCES", "permission denied"},
	syscall.EPERM:        {"EPERM", "operation not permitted"},
	syscall.ENOTDIR:      {"ENOTDIR", "not a directory"},
	syscall.EISDIR:       {"EISDIR", "illegal operation on a directory"},
	syscall.ELOOP:        {"ELOOP", "too many symbolic links encountered"},
	syscall.ENAMETOOLONG: {"ENAMETOOLONG", "name too long"},
	syscall.EMFILE:       {"EMFILE", "too many open files"},
	syscall.ENFILE:       {"ENFILE", "file table overflow"},
	syscall.EIO:          {"EIO", "i/o error"},
	syscall.ENOMEM:       {"ENOMEM", "not enough memory"},
}

func errnoText(err error) (code, desc string, ok bool) {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return "", "", false
	}
	names, ok := errnoNames[errno]
	return names[0], names[1], ok
}
