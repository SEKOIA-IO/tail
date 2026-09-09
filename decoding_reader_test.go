// Copyright (c) 2019 FOSS contributors of https://github.com/SEKOIA-IO/tail

package tail

import (
	"io"
	"strings"
	"testing"

	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

// growingReader mimics a file that is appended to while it is read: it returns
// io.EOF when it is exhausted, and it can be grown afterwards.
type growingReader struct {
	data      []byte
	position  int
	chunkSize int // when > 0, the maximum number of bytes returned per read
}

func (reader *growingReader) Read(p []byte) (int, error) {
	if reader.position >= len(reader.data) {
		return 0, io.EOF
	}
	available := reader.data[reader.position:]
	if reader.chunkSize > 0 && len(available) > reader.chunkSize {
		available = available[:reader.chunkSize]
	}
	n := copy(p, available)
	reader.position += n
	return n, nil
}

func (reader *growingReader) grow(data []byte) {
	reader.data = append(reader.data, data...)
}

// readLinesUntilEOF reads every line the reader can decode right now.
func readLinesUntilEOF(t *testing.T, reader lineReader) string {
	t.Helper()
	var lines strings.Builder
	for {
		line, err := reader.ReadString('\n')
		lines.WriteString(line)
		switch err {
		case nil:
		case io.EOF:
			return lines.String()
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
}

func utf16LEDecoders() (transform.Transformer, transform.Transformer) {
	return unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder(),
		unicode.UTF16(unicode.LittleEndian, unicode.IgnoreBOM).NewDecoder()
}

func utf16LEDecodersWithBOM() (transform.Transformer, transform.Transformer) {
	decoder, accountant := utf16LEDecoders()
	return unicode.BOMOverride(decoder), unicode.BOMOverride(accountant)
}

func newUTF16LEReader(source io.Reader) *decodingReader {
	decoder, accountant := utf16LEDecoders()
	return newDecodingReader(source, decoder, accountant, false)
}

// TestDecodingReaderResumesAfterEOF is the core of the fix: unlike
// transform.Reader, the decoding reader must stay usable after an EOF.
func TestDecodingReaderResumesAfterEOF(t *testing.T) {
	source := &growingReader{data: encodeUTF16(t, "hello\n", unicode.LittleEndian, false)}
	reader := newUTF16LEReader(source)

	if decoded := readLinesUntilEOF(t, reader); decoded != "hello\n" {
		t.Fatalf("expecting <<<hello\\n>>>, but got <<<%s>>>", decoded)
	}

	// A second EOF, as the tailing loop does while it waits for changes
	if line, err := reader.ReadString('\n'); line != "" || err != io.EOF {
		t.Fatalf("expecting (\"\", EOF), but got (%q, %v)", line, err)
	}

	source.grow(encodeUTF16(t, "world\n", unicode.LittleEndian, false))
	if decoded := readLinesUntilEOF(t, reader); decoded != "world\n" {
		t.Fatalf("expecting <<<world\\n>>> after the append, but got <<<%s>>>", decoded)
	}
}

// TestDecodingReaderKeepsIncompleteCharacter checks that the bytes of a
// character that is only half written are kept until the writer completes it.
func TestDecodingReaderKeepsIncompleteCharacter(t *testing.T) {
	complete := encodeUTF16(t, "ab\n", unicode.LittleEndian, false)
	source := &growingReader{data: complete[:len(complete)-3]}
	reader := newUTF16LEReader(source)

	if decoded := readLinesUntilEOF(t, reader); decoded != "a" {
		t.Fatalf("expecting <<<a>>>, but got <<<%s>>>", decoded)
	}

	source.grow(complete[len(complete)-3:])
	if decoded := readLinesUntilEOF(t, reader); decoded != "b\n" {
		t.Fatalf("expecting <<<b\\n>>> once the character is complete, but got <<<%s>>>", decoded)
	}
}

// TestDecodingReaderWithByteSizedReads checks that a source that returns very
// few bytes at a time, which never form a complete character, is handled.
func TestDecodingReaderWithByteSizedReads(t *testing.T) {
	source := &growingReader{
		data:      encodeUTF16(t, "héllo\nwörld\n", unicode.LittleEndian, false),
		chunkSize: 1,
	}
	reader := newUTF16LEReader(source)

	if decoded := readLinesUntilEOF(t, reader); decoded != "héllo\nwörld\n" {
		t.Fatalf("expecting the two lines, but got <<<%s>>>", decoded)
	}
}

// TestDecodingReaderDropsBOM checks the byte order mark handling of the
// decoders the tail builds.
func TestDecodingReaderDropsBOM(t *testing.T) {
	source := &growingReader{data: encodeUTF16(t, "hello\n", unicode.LittleEndian, true)}
	decoder, accountant := utf16LEDecodersWithBOM()
	reader := newDecodingReader(source, decoder, accountant, false)

	if decoded := readLinesUntilEOF(t, reader); decoded != "hello\n" {
		t.Fatalf("expecting <<<hello\\n>>> without a BOM, but got <<<%s>>> (bytes: %v)",
			decoded, []byte(decoded))
	}
}

// TestDecodingReaderCorrectsEndianness checks that the byte order mark wins over
// a wrongly configured endianness.
func TestDecodingReaderCorrectsEndianness(t *testing.T) {
	source := &growingReader{data: encodeUTF16(t, "hello\n", unicode.BigEndian, true)}
	decoder, accountant := utf16LEDecodersWithBOM()
	reader := newDecodingReader(source, decoder, accountant, false)

	if decoded := readLinesUntilEOF(t, reader); decoded != "hello\n" {
		t.Fatalf("expecting <<<hello\\n>>>, but got <<<%s>>> (bytes: %v)", decoded, []byte(decoded))
	}
}

// TestDecodingReaderWithLargeContent checks the buffering when the content is
// larger than the internal buffers.
func TestDecodingReaderWithLargeContent(t *testing.T) {
	text := strings.Repeat("a line of text\n", 5000)
	source := &growingReader{data: encodeUTF16(t, text, unicode.LittleEndian, false)}
	reader := newUTF16LEReader(source)

	if decoded := readLinesUntilEOF(t, reader); decoded != text {
		t.Fatalf("expecting %d bytes, but got %d", len(text), len(decoded))
	}
}

// TestDecodingReaderWithLineLongerThanTheBuffers checks a line that does not fit
// in the internal buffers, so that it is decoded in several passes.
func TestDecodingReaderWithLineLongerThanTheBuffers(t *testing.T) {
	text := strings.Repeat("x", 3*decodingBufferSize) + "\n"
	source := &growingReader{data: encodeUTF16(t, text, unicode.LittleEndian, false)}
	reader := newUTF16LEReader(source)

	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if line != text {
		t.Fatalf("expecting a line of %d bytes, but got %d", len(text), len(line))
	}
	if pending := reader.PendingSourceBytes(); pending != 0 {
		t.Fatalf("expecting nothing pending after the line, but got %d", pending)
	}
}

// TestDecodingReaderAccountsForSourceBytes checks the accounting that makes the
// reported offsets usable: after each line, the bytes that have not been
// returned to the caller must be counted in source bytes.
func TestDecodingReaderAccountsForSourceBytes(t *testing.T) {
	testCases := []struct {
		name string
		text string
		bom  bool
	}{
		{"ascii", "hello\nworld\n", false},
		{"ascii with a bom", "hello\nworld\n", true},
		{"accented characters", "héllo\nwörld\n", true},
		{"characters outside the basic plane", "a🙂b\nc🙂d\n", true},
		{"empty lines", "\n\n\n", true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			encoded := encodeUTF16(t, testCase.text, unicode.LittleEndian, testCase.bom)
			bomLength := len(encodeUTF16(t, "", unicode.LittleEndian, testCase.bom))
			source := &growingReader{data: encoded}
			decoder, accountant := utf16LEDecodersWithBOM()
			reader := newDecodingReader(source, decoder, accountant, false)

			returned := ""
			for _, expected := range splitLines(testCase.text) {
				line, err := reader.ReadString('\n')
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if line != expected {
					t.Fatalf("expecting <<<%s>>>, but got <<<%s>>>", expected, line)
				}
				returned += expected

				// What has been read from the source, minus what is still
				// pending, is the source the returned lines were made of
				consumed := len(encoded) - reader.PendingSourceBytes()
				expectedConsumed := bomLength +
					len(encodeUTF16(t, returned, unicode.LittleEndian, false))
				if consumed != expectedConsumed {
					t.Fatalf("expecting %d source bytes consumed after %q, but got %d",
						expectedConsumed, expected, consumed)
				}
			}
		})
	}
}

// splitLines splits a text after each newline character, without the empty
// element that trails the last one.
func splitLines(text string) []string {
	lines := strings.SplitAfter(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func BenchmarkDecodingReader(b *testing.B) {
	text := strings.Repeat("2024-01-01 12:00:00.00 spid51  Starting up database 'tempdb'.\n", 20000)
	encoded, err := unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewEncoder().Bytes([]byte(text))
	if err != nil {
		b.Fatal(err)
	}

	b.SetBytes(int64(len(encoded)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		decoder, accountant := utf16LEDecodersWithBOM()
		reader := newDecodingReader(&growingReader{data: encoded}, decoder, accountant, false)
		for {
			_, err := reader.ReadString('\n')
			if err == io.EOF {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
		}
	}
}
