// Copyright (c) 2019 FOSS contributors of https://github.com/SEKOIA-IO/tail

package watch

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/tomb.v1"
)

func TestPollingWatcherNotifiesOnAppend(t *testing.T) {
	POLL_DURATION = 10 * time.Millisecond
	file := filepath.Join(t.TempDir(), "app.log")
	if err := os.WriteFile(file, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fw := NewPollingFileWatcher(file)
	var tb tomb.Tomb
	defer tb.Done()
	defer tb.Kill(nil)
	changes, err := fw.ChangeEvents(&tb, 6)
	if err != nil {
		t.Fatal(err)
	}

	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("world\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	select {
	case <-changes.Modified:
	case <-time.After(5 * time.Second):
		t.Fatal("expected a modification notification after an append")
	}
}

func TestPollingWatcherSurvivesUnexpectedStatErrors(t *testing.T) {
	// A stat error that is not "file does not exist" used to call util.Fatal,
	// which exits the whole process from the polling goroutine. The watcher
	// must instead report the file as deleted so the caller can try a reopen.
	POLL_DURATION = 10 * time.Millisecond
	dir := filepath.Join(t.TempDir(), "logs")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "app.log")
	if err := os.WriteFile(file, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fw := NewPollingFileWatcher(file)
	var tb tomb.Tomb
	defer tb.Done()
	defer tb.Kill(nil)
	changes, err := fw.ChangeEvents(&tb, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Replace the parent directory with a regular file: stat on the tailed
	// path now fails with ENOTDIR, which is neither IsNotExist nor
	// IsPermission.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case <-changes.Deleted:
	case <-time.After(5 * time.Second):
		t.Fatal("expected a deletion notification, the polling goroutine likely died")
	}
}
