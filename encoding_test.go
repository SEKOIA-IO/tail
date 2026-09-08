// Copyright (c) 2019 FOSS contributors of https://github.com/SEKOIA-IO/tail

package tail

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/encoding/unicode"
)

// Test helpers

func encodeUTF16(t *testing.T, text string, endianness unicode.Endianness, withBOM bool) []byte {
	t.Helper()
	policy := unicode.IgnoreBOM
	if withBOM {
		policy = unicode.UseBOM
	}
	encoded, err := unicode.UTF16(endianness, policy).NewEncoder().Bytes([]byte(text))
	if err != nil {
		t.Fatalf("failed to encode %q: %v", text, err)
	}
	return encoded
}

func (t TailTest) CreateFileBytes(name string, contents []byte) {
	if err := os.WriteFile(t.path+"/"+name, contents, 0600); err != nil {
		t.Fatal(err)
	}
}

func (t TailTest) AppendFileBytes(name string, contents []byte) {
	f, err := os.OpenFile(t.path+"/"+name, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err = f.Write(contents); err != nil {
		t.Fatal(err)
	}
}

func (t TailTest) TruncateFileBytes(name string, contents []byte) {
	f, err := os.OpenFile(t.path+"/"+name, os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err = f.Write(contents); err != nil {
		t.Fatal(err)
	}
}

// expectLine waits for the next line of the tail, and fails the test when none
// is delivered before the timeout. A file that is no longer followed does not
// report any error, it just goes silent, hence the timeout.
func expectLine(t *testing.T, tail *Tail, expected string) {
	t.Helper()
	select {
	case line, ok := <-tail.Lines:
		if !ok {
			t.Fatalf("tail.Lines was closed while expecting %q (error: %v)", expected, tail.Err())
			return
		}
		if line.Err != nil {
			t.Fatalf("unexpected error while expecting %q: %v", expected, line.Err)
		}
		if line.Text != expected {
			t.Fatalf("expecting <<<%s>>>, but got <<<%s>>> (bytes: %v)", expected, line.Text, []byte(line.Text))
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %q: the file is not followed anymore", expected)
	}
}

// settle leaves the tailer the time to start watching the file again before it
// is written to: the watchers only report the changes that happen after they
// have been set up.
func settle() {
	<-time.After(100 * time.Millisecond)
}

// newUnstartedTail returns a Tail with an open file but no tailing goroutine, so
// that the read path can be exercised deterministically.
func newUnstartedTail(t *testing.T, path string, config Config) *Tail {
	t.Helper()
	if config.Logger == nil {
		config.Logger = DiscardingLogger
	}
	tail := &Tail{Filename: path, Config: config}
	file, err := OpenFile(path)
	if err != nil {
		t.Fatalf("failed to open %s: %v", path, err)
	}
	tail.file = file
	t.Cleanup(func() { file.Close() })
	return tail
}

// Tests

// TestFollowUTF16 checks that a file that is not UTF-8 encoded keeps being
// followed once its end has been reached a first time.
func TestFollowUTF16(t *testing.T) {
	testCases := []struct {
		name       string
		encoding   string
		endianness unicode.Endianness
	}{
		{"utf16le-explicit-encoding", "UTF-16LE", unicode.LittleEndian},
		{"utf16le-detected-encoding", "", unicode.LittleEndian},
		{"utf16be-explicit-encoding", "UTF-16BE", unicode.BigEndian},
		{"utf16be-detected-encoding", "", unicode.BigEndian},
	}

	for _, testCase := range testCases {
		for _, poll := range []bool{false, true} {
			name := testCase.name
			if poll {
				name += "-polling"
			}
			t.Run(name, func(t *testing.T) {
				tailTest, cleanup := NewTailTest(name, t)
				defer cleanup()

				tailTest.CreateFileBytes("test.log", encodeUTF16(t, "hello\n", testCase.endianness, true))
				tail := tailTest.StartTail("test.log", Config{Follow: true, Poll: poll, Encoding: testCase.encoding})

				expectLine(t, tail, "hello")

				// Every append must be delivered: the first EOF used to kill the decoder
				for _, text := range []string{"world\n", "and\n", "again\n"} {
					settle()
					tailTest.AppendFileBytes("test.log", encodeUTF16(t, text, testCase.endianness, false))
					expectLine(t, tail, strings.TrimSuffix(text, "\n"))
				}

				tail.Stop()
				tail.Cleanup()
			})
		}
	}
}

// TestFollowUTF16WrittenByteByByte checks that a line written in several chunks,
// splitting UTF-16 code units across writes, is decoded correctly.
func TestFollowUTF16WrittenByteByByte(t *testing.T) {
	tailTest, cleanup := NewTailTest("utf16-byte-by-byte", t)
	defer cleanup()

	tailTest.CreateFileBytes("test.log", encodeUTF16(t, "hello\n", unicode.LittleEndian, true))
	tail := tailTest.StartTail("test.log", Config{Follow: true, Poll: true, CompleteLines: true})

	expectLine(t, tail, "hello")

	encoded := encodeUTF16(t, "wörld\n", unicode.LittleEndian, false)
	for _, b := range encoded {
		settle()
		tailTest.AppendFileBytes("test.log", []byte{b})
	}

	expectLine(t, tail, "wörld")

	tail.Stop()
	tail.Cleanup()
}

// TestFollowUTF8NoRegression checks that the UTF-8 fast path is left untouched.
func TestFollowUTF8NoRegression(t *testing.T) {
	tailTest, cleanup := NewTailTest("follow-utf8", t)
	defer cleanup()

	tailTest.CreateFile("test.log", "hello\n")
	tail := tailTest.StartTail("test.log", Config{Follow: true})

	expectLine(t, tail, "hello")
	settle()
	tailTest.AppendFile("test.log", "world\n")
	expectLine(t, tail, "world")

	tail.Stop()
	tail.Cleanup()
}

// TestUTF8IsNotDecoded checks that UTF-8 files are read directly from the file,
// without any decoding layer in between.
func TestUTF8IsNotDecoded(t *testing.T) {
	tailTest, cleanup := NewTailTest("utf8-not-decoded", t)
	defer cleanup()

	tailTest.CreateFile("test.log", "hello\n")
	tail := newUnstartedTail(t, tailTest.path+"/test.log", Config{})

	if decoder, _ := tail.newDecoders(); decoder != nil {
		t.Fatalf("expecting no decoder for an UTF-8 file, got %T", decoder)
	}

	tail.openReader()
	if _, ok := tail.reader.(bufferedLineReader); !ok {
		t.Fatalf("expecting the file to be read through a buffered reader, got %T", tail.reader)
	}
}

// TestBOMIsNotPartOfTheFirstLine checks that the byte order mark of a file is
// not decoded as an U+FEFF character prepended to its first line.
func TestBOMIsNotPartOfTheFirstLine(t *testing.T) {
	testCases := []struct {
		name       string
		encoding   string
		endianness unicode.Endianness
	}{
		{"utf16le", "UTF-16LE", unicode.LittleEndian},
		{"utf16be", "UTF-16BE", unicode.BigEndian},
		{"detected", "", unicode.LittleEndian},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			tailTest, cleanup := NewTailTest("bom-"+testCase.name, t)
			defer cleanup()

			tailTest.CreateFileBytes("test.log", encodeUTF16(t, "hello\n", testCase.endianness, true))
			tail := newUnstartedTail(t, tailTest.path+"/test.log", Config{Encoding: testCase.encoding})
			tail.openReader()

			line, err := tail.readLine()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if strings.ContainsRune(line, '\ufeff') {
				t.Fatalf("the byte order mark leaked into the first line: %v", []byte(line))
			}
			if line != "hello" {
				t.Fatalf("expecting <<<hello>>>, but got <<<%s>>> (bytes: %v)", line, []byte(line))
			}
		})
	}
}

// TestSeekToKeepsDecoding checks that seeking does not drop the decoding layer.
func TestSeekToKeepsDecoding(t *testing.T) {
	tailTest, cleanup := NewTailTest("seek-keeps-decoding", t)
	defer cleanup()

	tailTest.CreateFileBytes("test.log", encodeUTF16(t, "hello\nworld\n", unicode.LittleEndian, true))
	tail := newUnstartedTail(t, tailTest.path+"/test.log", Config{Encoding: "UTF-16LE"})
	tail.openReader()

	if line, err := tail.readLine(); err != nil || line != "hello" {
		t.Fatalf("expecting <<<hello>>>, but got <<<%s>>> (error: %v)", line, err)
	}

	if err := tail.seekTo(SeekInfo{Offset: 0, Whence: io.SeekStart}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Without a decoder, the raw UTF-16 bytes give "h\x00e\x00l\x00l\x00o\x00"
	if line, err := tail.readLine(); err != nil || line != "hello" {
		t.Fatalf("expecting <<<hello>>> after the seek, but got <<<%s>>> (bytes: %v, error: %v)",
			line, []byte(line), err)
	}
}

// TestSeekEndKeepsDecoding checks the same thing for the end of the file, which
// is where the tailing loop seeks when it reads a partial line.
func TestSeekEndKeepsDecoding(t *testing.T) {
	tailTest, cleanup := NewTailTest("seek-end-keeps-decoding", t)
	defer cleanup()

	tailTest.CreateFileBytes("test.log", encodeUTF16(t, "hello\n", unicode.LittleEndian, true))
	tail := newUnstartedTail(t, tailTest.path+"/test.log", Config{Encoding: "UTF-16LE"})
	tail.openReader()

	if err := tail.seekEnd(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	tailTest.AppendFileBytes("test.log", encodeUTF16(t, "world\n", unicode.LittleEndian, false))

	if line, err := tail.readLine(); err != nil || line != "world" {
		t.Fatalf("expecting <<<world>>> after the seek, but got <<<%s>>> (bytes: %v, error: %v)",
			line, []byte(line), err)
	}
}

// TestGetEncoding checks the encoding detection, especially on the short files
// produced by a rotation, where the buffer used to be mostly NUL padding.
func TestGetEncoding(t *testing.T) {
	utf16LEBom := []byte{0xFF, 0xFE}
	testCases := []struct {
		name     string
		content  []byte
		expected string
	}{
		{"empty file", nil, "UTF-8"},
		{"single byte", []byte{0xFF}, "UTF-8"},
		{"freshly recycled utf16le file", utf16LEBom, "UTF-16LE"},
		{"utf16le file", encodeUTF16(t, "hello world\n", unicode.LittleEndian, true), "UTF-16LE"},
		{"utf16be file", encodeUTF16(t, "hello world\n", unicode.BigEndian, true), "UTF-16BE"},
		{"ascii file", []byte("hello world\n"), "UTF-8"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			tailTest, cleanup := NewTailTest("get-encoding", t)
			defer cleanup()

			tailTest.CreateFileBytes("test.log", testCase.content)
			tail := newUnstartedTail(t, tailTest.path+"/test.log", Config{})

			if encoding := tail.getEncoding(); encoding != testCase.expected {
				t.Fatalf("expecting %s, but got %s", testCase.expected, encoding)
			}

			// Detection must leave the file position untouched
			if offset, err := tail.file.Seek(0, io.SeekCurrent); err != nil || offset != 0 {
				t.Fatalf("expecting the file position to be left at 0, got %d (error: %v)", offset, err)
			}
		})
	}
}

// TestGetEncodingIsRetriedWhileUndecided checks that a file that is too small to
// be identified leaves the detection undecided, so that it is retried when the
// file has grown, while a full but inconclusive sample settles on the default.
func TestGetEncodingIsRetriedWhileUndecided(t *testing.T) {
	tailTest, cleanup := NewTailTest("undecided-encoding", t)
	defer cleanup()

	tailTest.CreateFileBytes("test.log", nil)
	tail := newUnstartedTail(t, tailTest.path+"/test.log", Config{})

	if encoding := tail.getEncoding(); encoding != "UTF-8" {
		t.Fatalf("expecting UTF-8 on an empty file, but got %s", encoding)
	}
	if tail.detectedEncoding != "" {
		t.Fatalf("expecting the detection to stay undecided, but got %s", tail.detectedEncoding)
	}

	// The detection must pick the encoding up once the file has been written to
	tailTest.AppendFileBytes("test.log", encodeUTF16(t, "hello world\n", unicode.LittleEndian, true))
	if encoding := tail.getEncoding(); encoding != "UTF-16LE" {
		t.Fatalf("expecting UTF-16LE once the file has grown, but got %s", encoding)
	}
}

// TestGetEncodingStopsRetryingOnAFullSample checks that the detection is not run
// again for a file whose beginning is known and inconclusive, which is the case
// of every plain ASCII log file.
func TestGetEncodingStopsRetryingOnAFullSample(t *testing.T) {
	tailTest, cleanup := NewTailTest("decided-encoding", t)
	defer cleanup()

	tailTest.CreateFileBytes("test.log", []byte(strings.Repeat("hello world\n", 200)))
	tail := newUnstartedTail(t, tailTest.path+"/test.log", Config{})

	if encoding := tail.getEncoding(); encoding != "UTF-8" {
		t.Fatalf("expecting UTF-8 on an ASCII file, but got %s", encoding)
	}
	if tail.detectedEncoding != "UTF-8" {
		t.Fatalf("expecting the detection to be settled, but got %q", tail.detectedEncoding)
	}
}

// TestFollowFileCreatedEmptyThenWrittenInUTF16 checks the residual case of the
// detection: a file that is completely empty when it is opened cannot be
// identified, and its encoding must be detected once it has been written to.
func TestFollowFileCreatedEmptyThenWrittenInUTF16(t *testing.T) {
	tailTest, cleanup := NewTailTest("empty-then-utf16", t)
	defer cleanup()

	tailTest.CreateFileBytes("test.log", nil)
	tail := tailTest.StartTail("test.log", Config{Follow: true, Poll: true})

	settle()
	tailTest.AppendFileBytes("test.log", encodeUTF16(t, "hello\n", unicode.LittleEndian, true))
	expectLine(t, tail, "hello")

	settle()
	tailTest.AppendFileBytes("test.log", encodeUTF16(t, "world\n", unicode.LittleEndian, false))
	expectLine(t, tail, "world")

	tail.Stop()
	tail.Cleanup()
}

// TestFollowTruncatedUTF16File checks that a rotated UTF-16 file, which is only
// made of a byte order mark when it is reopened, is still decoded correctly.
func TestFollowTruncatedUTF16File(t *testing.T) {
	tailTest, cleanup := NewTailTest("truncated-utf16", t)
	defer cleanup()

	tailTest.CreateFileBytes("test.log", encodeUTF16(t, "hello\n", unicode.LittleEndian, true))
	tail := tailTest.StartTail("test.log", Config{Follow: true, Poll: true})

	expectLine(t, tail, "hello")

	// Recycle the file the way sp_cycle_errorlog does: only a BOM is left
	settle()
	tailTest.TruncateFileBytes("test.log", encodeUTF16(t, "", unicode.LittleEndian, true))
	settle()
	tailTest.AppendFileBytes("test.log", encodeUTF16(t, "world\n", unicode.LittleEndian, false))

	expectLine(t, tail, "world")

	tail.Stop()
	tail.Cleanup()
}
