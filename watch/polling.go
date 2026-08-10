// Copyright (c) 2019 FOSS contributors of https://github.com/SEKOIA-IO/tail
// Copyright (c) 2015 HPE Software Inc. All rights reserved.
// Copyright (c) 2013 ActiveState Software Inc. All rights reserved.

package watch

import (
	"os"
	"runtime"
	"time"

	"github.com/SEKOIA-IO/tail/util"
	"gopkg.in/tomb.v1"
)

// PollingFileWatcher polls the file for changes.
type PollingFileWatcher struct {
	Filename string
	Size     int64
}

func NewPollingFileWatcher(filename string) *PollingFileWatcher {
	fw := &PollingFileWatcher{filename, 0}
	return fw
}

var POLL_DURATION time.Duration

func (fw *PollingFileWatcher) BlockUntilExists(t *tomb.Tomb) error {
	for {
		if _, err := os.Stat(fw.Filename); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		select {
		case <-time.After(POLL_DURATION):
			continue
		case <-t.Dying():
			return tomb.ErrDying
		}
	}
	panic("unreachable")
}

// Number of consecutive failed polls tolerated before the file is reported as
// deleted. Stat errors can be transient (antivirus or backup software briefly
// locking the file); reporting a deletion too eagerly makes the tail reopen the
// file and read it again from the start.
const maxConsecutiveStatErrors = 40

func (fw *PollingFileWatcher) ChangeEvents(t *tomb.Tomb, pos int64) (*FileChanges, error) {
	origFi, err := statFile(fw.Filename)
	if err != nil {
		return nil, err
	}

	changes := NewFileChanges()
	var prevModTime time.Time

	// XXX: use tomb.Tomb to cleanly manage these goroutines. replace
	// the fatal (below) with tomb's Kill.

	fw.Size = pos

	go func() {
		prevSize := fw.Size
		consecutiveStatErrors := 0
		for {
			select {
			case <-t.Dying():
				return
			default:
			}

			time.Sleep(POLL_DURATION)
			fi, err := statFile(fw.Filename)
			if err != nil {
				// Windows cannot delete a file if a handle is still open (tail keeps one open)
				// so it gives access denied to anything trying to read it until all handles are released.
				if os.IsNotExist(err) || (runtime.GOOS == "windows" && os.IsPermission(err)) {
					// File does not exist (has been deleted).
					changes.NotifyDeleted()
					return
				}

				// Tolerate transient errors, then report the file as deleted
				// so the tail tries to reopen it instead of dying.
				if consecutiveStatErrors == 0 {
					util.LOGGER.Printf("Unexpected error while polling %s: %s", fw.Filename, err)
				}
				consecutiveStatErrors++
				if consecutiveStatErrors >= maxConsecutiveStatErrors {
					util.LOGGER.Printf("Still unable to poll %s after %d attempts (%s), reporting it as deleted to trigger a reopen", fw.Filename, consecutiveStatErrors, err)
					changes.NotifyDeleted()
					return
				}
				continue
			}
			consecutiveStatErrors = 0

			// File got moved/renamed?
			if !os.SameFile(origFi, fi) {
				changes.NotifyDeleted()
				return
			}

			// File got truncated?
			fw.Size = fi.Size()
			if prevSize > 0 && prevSize > fw.Size {
				changes.NotifyTruncated()
				prevSize = fw.Size
				continue
			}
			// File got bigger?
			if prevSize > 0 && prevSize < fw.Size {
				changes.NotifyModified()
				prevSize = fw.Size
				continue
			}
			prevSize = fw.Size

			// File was appended to (changed)?
			modTime := fi.ModTime()
			if modTime != prevModTime {
				prevModTime = modTime
				changes.NotifyModified()
			}
		}
	}()

	return changes, nil
}

func init() {
	POLL_DURATION = 250 * time.Millisecond
}
