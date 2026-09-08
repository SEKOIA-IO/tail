// Copyright (c) 2019 FOSS contributors of https://github.com/SEKOIA-IO/tail

package tail

import (
	"io"
	"os"
	"reflect"
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
// not decoded as a U+FEFF character prepended to its first line.
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

// TestUTF8BOMIsNotPartOfTheFirstLine checks that the byte order mark of an UTF-8
// file, which Windows editors and PowerShell write, is discarded as well. Such a
// file is read as-is, without any decoder, so it does not benefit from
// unicode.BOMOverride.
func TestUTF8BOMIsNotPartOfTheFirstLine(t *testing.T) {
	for _, encoding := range []string{"", "UTF-8"} {
		name := "detected"
		if encoding != "" {
			name = "explicit"
		}
		t.Run(name, func(t *testing.T) {
			tailTest, cleanup := NewTailTest("utf8-bom-"+name, t)
			defer cleanup()

			tailTest.CreateFileBytes("test.log", append(utf8BOM, []byte("hello\nwörld\n")...))
			tail := newUnstartedTail(t, tailTest.path+"/test.log", Config{Encoding: encoding})
			tail.openReader()

			line, err := tail.readLine()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if line != "hello" {
				t.Fatalf("expecting <<<hello>>>, but got <<<%s>>> (bytes: %v)", line, []byte(line))
			}

			// The mark counts as read: it is part of the file, only not of its lines
			offset, err := tail.Tell()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if expected := int64(len(utf8BOM) + len("hello\n")); offset != expected {
				t.Fatalf("expecting an offset of %d after the first line, but got %d", expected, offset)
			}

			if line, err = tail.readLine(); err != nil || line != "wörld" {
				t.Fatalf("expecting <<<wörld>>>, but got <<<%s>>> (error: %v)", line, err)
			}
		})
	}
}

// TestUTF8WithoutBOMLosesNothing checks that the byte order mark detection does
// not eat the first bytes of a file that does not start with one, including when
// the file is too short to even hold a mark.
func TestUTF8WithoutBOMLosesNothing(t *testing.T) {
	testCases := []struct {
		name     string
		content  string
		expected string
	}{
		{"ordinary file", "hello\n", "hello"},
		{"file shorter than a mark", "a\n", "a"},
		{"file starting like a mark", "\xef\xbbhello\n", "\xef\xbbhello"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			tailTest, cleanup := NewTailTest("utf8-no-bom", t)
			defer cleanup()

			tailTest.CreateFileBytes("test.log", []byte(testCase.content))
			tail := newUnstartedTail(t, tailTest.path+"/test.log", Config{Encoding: "UTF-8"})
			tail.openReader()

			line, err := tail.readLine()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if line != testCase.expected {
				t.Fatalf("expecting <<<%s>>>, but got <<<%s>>> (bytes: %v)",
					testCase.expected, line, []byte(line))
			}
		})
	}
}

// TestShortFirstLineIsNotWithheld checks that a followed stream whose beginning
// is shorter than a byte order mark is decoded right away.
//
// unicode.BOMOverride withholds the first three bytes of a stream while it
// decides whether they begin a mark. Wrapping a file that carries no mark, where
// the override has nothing to do anyway, made a complete first line of one or
// two bytes sit in the decoder until a third byte was written. On a named pipe,
// whose writer can stay open indefinitely, the line never came out at all.
func TestShortFirstLineIsNotWithheld(t *testing.T) {
	testCases := []struct {
		name     string
		encoding string
		content  []byte
		expected string
	}{
		{"one byte and a newline", "ISO-8859-1", encodeLatin1(t, "a\n"), "a"},
		{"a bare newline", "ISO-8859-1", encodeLatin1(t, "\n"), ""},
		{"a mark-like first byte", "ISO-8859-1", []byte{0xFF, '\n'}, "\u00ff"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			tailTest, cleanup := NewTailTest("short-first-line", t)
			defer cleanup()

			tailTest.CreateFileBytes("test.log", testCase.content)
			// Follow, so that the end of the file is not the end of the data and
			// the decoder is never told that no more bytes will come
			tail := newUnstartedTail(t, tailTest.path+"/test.log",
				Config{Encoding: testCase.encoding, Follow: true})
			tail.openReader()

			line, err := tail.readLine()
			if err != nil {
				t.Fatalf("expecting <<<%s>>> straight away, but got <<<%s>>> (error: %v)",
					testCase.expected, line, err)
			}
			if line != testCase.expected {
				t.Fatalf("expecting <<<%s>>>, but got <<<%s>>> (bytes: %v)",
					testCase.expected, line, []byte(line))
			}
		})
	}
}

// TestBOMDecidesTheEncodingWhenResuming checks that the byte order mark of the
// file settles the encoding wherever the reader is built, and not only at the
// beginning of the file.
//
// unicode.BOMOverride is only applied at offset 0, because mark-like bytes found
// anywhere else are ordinary content. A reader built further in, which is what
// every resumed tailing does, therefore used to fall back to the configured
// encoding: a UTF-16BE file tailed as UTF-16LE decoded correctly until the first
// resume, then byte-swapped every character. Worse, a misaligned UTF-16 stream
// holds no line ending at all, so the file went silent instead of looking wrong.
func TestBOMDecidesTheEncodingWhenResuming(t *testing.T) {
	testCases := []struct {
		name       string
		endianness unicode.Endianness
		// encoding is what the configuration says, and it disagrees with, or is
		// less precise than, the mark the file actually carries
		encoding string
	}{
		{"big endian file tailed as little endian", unicode.BigEndian, "UTF-16LE"},
		{"little endian file tailed as big endian", unicode.LittleEndian, "UTF-16BE"},
		{"big endian file tailed as plain utf-16", unicode.BigEndian, "UTF-16"},
		{"little endian file tailed as plain utf-16", unicode.LittleEndian, "UTF-16"},
		{"little endian file with a detected encoding", unicode.LittleEndian, ""},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			tailTest, cleanup := NewTailTest("bom-decides-encoding", t)
			defer cleanup()

			tailTest.CreateFileBytes("test.log",
				encodeUTF16(t, "hello\nworld\n", testCase.endianness, true))

			// Read the first line, and note where the tailing could be resumed
			tail := newUnstartedTail(t, tailTest.path+"/test.log", Config{Encoding: testCase.encoding})
			tail.openReader()
			if line, err := tail.readLine(); err != nil || line != "hello" {
				t.Fatalf("expecting <<<hello>>>, but got <<<%s>>> (error: %v)", line, err)
			}
			offset, err := tail.Tell()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Resume there, the way a restarted consumer does
			resumed := newUnstartedTail(t, tailTest.path+"/test.log", Config{Encoding: testCase.encoding})
			if _, err := resumed.file.Seek(offset, io.SeekStart); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			resumed.openReader()

			line, err := resumed.readLine()
			if err != nil || line != "world" {
				t.Fatalf("expecting <<<world>>> after resuming at %d, but got <<<%s>>> (bytes: %v, error: %v)",
					offset, line, []byte(line), err)
			}
		})
	}
}

// TestMarkLikeBytesAreKeptWhenResuming checks that bytes that look like a byte
// order mark are treated as ordinary content when the reader is built further in
// the file, which is what happens every time the tailing resumes at a stored
// offset.
//
// Dropping them would lose a character, and, worse, the mark of an encoding that
// is not the one of the file switches the decoder for all the rest of the file:
// the ISO-8859-1 case below decodes as UTF-16LE, which turns every line into
// mojibake and, since a misaligned UTF-16 stream contains no line ending, makes
// the file go silent.
func TestMarkLikeBytesAreKeptWhenResuming(t *testing.T) {
	testCases := []struct {
		name     string
		encoding string
		content  []byte
		resume   int64
		expected string
	}{
		{
			name:     "utf16le",
			encoding: "UTF-16LE",
			content:  encodeUTF16(t, "hello\n\ufeffworld\n", unicode.LittleEndian, true),
			// The mark of the file, then the first line, two bytes per character
			resume:   2 + int64(len("hello\n")*2),
			expected: "\ufeffworld",
		},
		{
			name:     "iso-8859-1",
			encoding: "ISO-8859-1",
			content:  encodeLatin1(t, "hello\n\u00ff\u00ferest of the file\n"),
			resume:   int64(len("hello\n")),
			expected: "\u00ff\u00ferest of the file",
		},
		{
			name:     "utf8",
			encoding: "UTF-8",
			content:  append([]byte("hello\n"), append(utf8BOM, []byte("world\n")...)...),
			resume:   int64(len("hello\n")),
			expected: "\ufeffworld",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			tailTest, cleanup := NewTailTest("resume-mark-like", t)
			defer cleanup()

			tailTest.CreateFileBytes("test.log", testCase.content)

			// Through the Location of the configuration, the way a consumer
			// resumes where a previous run of the tail stopped
			tail := tailTest.StartTail("test.log", Config{
				Follow:   true,
				Poll:     true,
				Encoding: testCase.encoding,
				Location: &SeekInfo{Offset: testCase.resume, Whence: io.SeekStart},
			})

			expectLine(t, tail, testCase.expected)

			tailTest.RemoveFile("test.log")
			tail.Stop()
			tail.Cleanup()
		})
	}
}

// TestFileThatIsNotFollowedIsFullyDecoded checks that the bytes a transformer
// keeps back are decoded when the end of the file is the end of the data.
//
// A transformer is allowed to hold bytes for as long as it has not been told
// that no more will come. unicode.BOMOverride holds the first two bytes of a
// file while it decides whether they begin a byte order mark, so a file of one
// or two bytes used to produce nothing at all: the reader returned an empty EOF
// and the tailing ended, silently dropping the whole file.
func TestFileThatIsNotFollowedIsFullyDecoded(t *testing.T) {
	testCases := []struct {
		name     string
		encoding string
		content  []byte
		expected []string
		offsets  []int64
	}{
		{
			name:     "shorter than a byte order mark",
			encoding: "ISO-8859-1",
			content:  encodeLatin1(t, "a\n"),
			expected: []string{"a"},
			offsets:  []int64{2},
		},
		{
			name:     "shorter than a byte order mark, without a line ending",
			encoding: "ISO-8859-1",
			content:  encodeLatin1(t, "ab"),
			expected: []string{"ab"},
			offsets:  []int64{2},
		},
		{
			// The replacement character is what tells the reader of the log that
			// the file ends in the middle of a character
			name:     "truncated in the middle of a character",
			encoding: "UTF-16LE",
			content:  append(encodeUTF16(t, "hello\n", unicode.LittleEndian, true), 0x41),
			expected: []string{"hello", "\ufffd"},
			offsets:  []int64{14, 15},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			tailTest, cleanup := NewTailTest("not-followed", t)
			defer cleanup()

			tailTest.CreateFileBytes("test.log", testCase.content)
			tail := tailTest.StartTail("test.log", Config{
				Follow: false, Encoding: testCase.encoding,
			})
			defer tail.Cleanup()

			lines, offsets := collectLines(t, tail, len(testCase.expected))
			if !reflect.DeepEqual(lines, testCase.expected) {
				t.Fatalf("expecting %q, but got %q", testCase.expected, lines)
			}
			// The flush must not desynchronize the accountant from the decoder
			if !reflect.DeepEqual(offsets, testCase.offsets) {
				t.Fatalf("expecting the offsets %v, but got %v", testCase.offsets, offsets)
			}
			if _, ok := <-tail.Lines; ok {
				t.Fatal("expecting the tailing to be over")
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

// TestFollowFileWithATornBOM checks that a file caught while its byte order mark
// is only half written is not read as UTF-8 in the meantime.
//
// The single byte cannot be decoded, and it cannot be un-read either: consuming
// it would leave the decoder that the detection ends up choosing one byte out of
// phase for the whole file, which produces mojibake with incomplete lines and,
// because a misaligned UTF-16 stream never contains 0x0A 0x00, no line at all
// with complete lines.
func TestFollowFileWithATornBOM(t *testing.T) {
	for _, completeLines := range []bool{false, true} {
		name := "incomplete-lines"
		if completeLines {
			name = "complete-lines"
		}
		t.Run(name, func(t *testing.T) {
			tailTest, cleanup := NewTailTest("torn-bom", t)
			defer cleanup()

			// The writer has only managed to write the first byte of the mark
			tailTest.CreateFileBytes("test.log", []byte{0xFF})
			tail := tailTest.StartTail("test.log", Config{
				Follow: true, Poll: true, CompleteLines: completeLines,
			})

			settle()
			// ...then it finishes the mark and writes two lines
			rest := append([]byte{0xFE},
				encodeUTF16(t, "hello\nworld\n", unicode.LittleEndian, false)...)
			tailTest.AppendFileBytes("test.log", rest)

			expectLine(t, tail, "hello")
			expectLine(t, tail, "world")

			tailTest.RemoveFile("test.log")
			tail.Stop()
			tail.Cleanup()
		})
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
