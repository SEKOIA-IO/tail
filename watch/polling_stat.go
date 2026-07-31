// Copyright (c) 2019 FOSS contributors of https://github.com/SEKOIA-IO/tail
//go:build !windows
// +build !windows

package watch

import "os"

// statFile returns metadata for the polled file. On POSIX systems a plain
// os.Stat is always accurate.
func statFile(name string) (os.FileInfo, error) {
	return os.Stat(name)
}
