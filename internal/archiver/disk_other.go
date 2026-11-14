//go:build !linux

package archiver

import "errors"

// FreeSpace is only implemented on Linux.
func FreeSpace(string) (free, total uint64, err error) {
	return 0, 0, errors.New("free space check not supported on this platform")
}
