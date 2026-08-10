// Copyright (c) 2019 FOSS contributors of https://github.com/SEKOIA-IO/tail

package watch

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"gopkg.in/tomb.v1"
)

// POLL_DURATION is set once for the whole package: the polling goroutines can
// outlive the test that spawned them, so a per-test assignment races with
// their reads.
func TestMain(m *testing.M) {
	POLL_DURATION = 10 * time.Millisecond
	os.Exit(m.Run())
}

func TestPollingWatcherNotifiesOnAppend(t *testing.T) {
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
	defer f.Close()
	if _, err := f.WriteString("world\n"); err != nil {
		t.Fatal(err)
	}

	// The writer stays open through the wait: on Windows the directory entry
	// is not refreshed while a writer holds the file open, which is the
	// regression this test covers.
	select {
	case <-changes.Modified:
	case <-time.After(5 * time.Second):
		t.Fatal("expected a modification notification after an append")
	}
}

func TestPollingWatcherSurvivesUnexpectedStatErrors(t *testing.T) {
	// A stat error that is not "file does not exist" used to call util.Fatal,
	// which exits the whole process from the polling goroutine. The watcher
	// must instead report the file as deleted so the caller can try a reopen,
	// but only after tolerating maxConsecutiveStatErrors failed polls.
	if runtime.GOOS == "windows" {
		t.Skip("relies on symlinks and POSIX ENOTDIR semantics")
	}
	tmp := t.TempDir()
	realDir := filepath.Join(tmp, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "app.log"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(tmp, "logs")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(linkDir, "app.log")

	fw := NewPollingFileWatcher(file)
	var tb tomb.Tomb
	defer tb.Done()
	defer tb.Kill(nil)
	changes, err := fw.ChangeEvents(&tb, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Atomically swap the "logs" symlink so it points to a regular file: stat
	// on the tailed path now fails with ENOTDIR, which is neither IsNotExist
	// nor IsPermission, without any window where it fails with ENOENT (which
	// would legitimately report a deletion on the first poll).
	notADir := filepath.Join(tmp, "notadir")
	if err := os.WriteFile(notADir, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	newLink := filepath.Join(tmp, "logs.new")
	if err := os.Symlink(notADir, newLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(newLink, linkDir); err != nil {
		t.Fatal(err)
	}

	// Each poll sleeps POLL_DURATION, so waiting for half the threshold
	// guarantees fewer than maxConsecutiveStatErrors polls have happened.
	select {
	case <-changes.Deleted:
		t.Fatal("deletion reported before the stat error threshold was reached")
	case <-time.After(POLL_DURATION * maxConsecutiveStatErrors / 2):
	}

	select {
	case <-changes.Deleted:
	case <-time.After(5 * time.Second):
		t.Fatal("expected a deletion notification once the stat error threshold is reached")
	}
}
