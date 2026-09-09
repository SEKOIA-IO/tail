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
	if err == nil {
		defer f.Close()
		return f.Stat()
	}
	if os.IsNotExist(err) {
		return nil, err
	}

	// The file could not be opened, which does not mean it is gone: a writer
	// holding it without sharing it, an antivirus or a backup agent all make the
	// open fail for a moment. os.Stat tells a real deletion from such a transient
	// failure, and is used for nothing else.
	//
	// Its result must never be returned as if it were fresh. It reads the very
	// directory entry this function exists to avoid: while a writer keeps the
	// file open, the size it reports lags behind, and the poller compares sizes,
	// so a lagging one looks like a truncation, reopens the file and ships all of
	// it again. Handing it out would also mix it with the FileInfo obtained
	// through a handle, which os.SameFile compares against, and the two carry
	// their file identifiers differently.
	if _, statErr := os.Stat(name); os.IsNotExist(statErr) {
		return nil, statErr
	}

	// The file is still there, only unreachable for now: report the error of the
	// open, and let the caller's tolerance of consecutive failures decide. A file
	// that stays locked for a moment is skipped for a few polls, without any
	// comparison on unreliable data.
	return nil, err
}
