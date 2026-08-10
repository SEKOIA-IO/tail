// Copyright (c) 2019 FOSS contributors of https://github.com/SEKOIA-IO/tail
//go:build windows
// +build windows

package watch

import (
	"os"

	"github.com/SEKOIA-IO/tail/winfile"
)

// statFile returns metadata for the polled file.
//
// On Windows, os.Stat reads the directory entry, which NTFS does not refresh
// while another process keeps the file open for writing (SQL Server and its
// ERRORLOG, for example). Polling that stale metadata never detects appends,
// so the tail silently stops following such files. Opening the file and
// querying the handle always returns fresh metadata.
func statFile(name string) (os.FileInfo, error) {
	f, err := winfile.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Stat()
}
