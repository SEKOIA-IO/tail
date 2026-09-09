// Copyright (c) 2019 FOSS contributors of https://github.com/SEKOIA-IO/tail

package tail

import (
	"io"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/unicode"
)

// The offset reported for a line is the position at which the tailing can be
// resumed. It is what the consumers of this library persist to survive a
// restart, so it must be a position in the file, whatever the encoding of the
// file is.

func encodeLatin1(t *testing.T, text string) []byte {
	t.Helper()
	encoded, err := charmap.ISO8859_1.NewEncoder().Bytes([]byte(text))
	if err != nil {
		t.Fatalf("failed to encode %q: %v", text, err)
	}
	return encoded
}

// collectLines reads the given number of lines and returns them with the offset
// reported for each of them.
func collectLines(t *testing.T, tail *Tail, count int) (lines []string, offsets []int64) {
	t.Helper()
	for i := 0; i < count; i++ {
		select {
		case line, ok := <-tail.Lines:
			if !ok {
				t.Fatalf("tail ended early, after %d lines (error: %v)", i, tail.Err())
			}
			lines = append(lines, line.Text)
			offsets = append(offsets, line.SeekInfo.Offset)
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out after %d lines", i)
		}
	}
	return lines, offsets
}

// TestOffsetsResumeTheTailing tails a file, stops in the middle of it, and
// checks that a new tail started at the reported offset delivers exactly the
// lines that were not delivered yet: no gap, no duplicate.
func TestOffsetsResumeTheTailing(t *testing.T) {
	const lineCount = 12
	const resumeAfter = 5

	var text strings.Builder
	for i := 0; i < lineCount; i++ {
		text.WriteString("line ")
		text.WriteByte(byte('a' + i))
		text.WriteString(" héllo wörld\n")
	}

	testCases := []struct {
		name     string
		encoding string
		encode   func(*testing.T, string) []byte
	}{
		{"utf8", "", func(t *testing.T, text string) []byte { return []byte(text) }},
		{"utf16le", "UTF-16LE", func(t *testing.T, text string) []byte {
			return encodeUTF16(t, text, unicode.LittleEndian, true)
		}},
		{"utf16be", "UTF-16BE", func(t *testing.T, text string) []byte {
			return encodeUTF16(t, text, unicode.BigEndian, true)
		}},
		{"utf16le without a bom", "UTF-16LE", func(t *testing.T, text string) []byte {
			return encodeUTF16(t, text, unicode.LittleEndian, false)
		}},
		{"iso-8859-1", "ISO-8859-1", encodeLatin1},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			tailTest, cleanup := NewTailTest("offsets-"+testCase.name, t)
			defer cleanup()
			tailTest.CreateFileBytes("test.log", testCase.encode(t, text.String()))

			// Read the beginning of the file, and remember where the tailing
			// should be resumed
			tail := tailTest.StartTail("test.log", Config{Follow: true, Poll: true, Encoding: testCase.encoding})
			lines, offsets := collectLines(t, tail, resumeAfter)
			resumeOffset := offsets[len(offsets)-1]
			tail.Stop()
			tail.Cleanup()

			// Resume where the first tail stopped
			resumed := tailTest.StartTail("test.log", Config{
				Follow:   true,
				Poll:     true,
				Encoding: testCase.encoding,
				Location: &SeekInfo{Offset: resumeOffset, Whence: io.SeekStart},
			})
			defer func() {
				resumed.Stop()
				resumed.Cleanup()
			}()

			remaining, _ := collectLines(t, resumed, lineCount-resumeAfter)

			all := append(lines, remaining...)
			expected := splitLines(text.String())
			for i := range expected {
				expected[i] = strings.TrimSuffix(expected[i], "\n")
			}
			if len(all) != len(expected) {
				t.Fatalf("expecting %d lines in total, but got %d: %q", len(expected), len(all), all)
			}
			for i := range expected {
				if all[i] != expected[i] {
					t.Fatalf("line %d: expecting <<<%s>>>, but got <<<%s>>>", i, expected[i], all[i])
				}
			}
		})
	}
}

// TestOffsetsAreFilePositions checks the reported offsets against the positions
// of the lines in the file itself, which is what makes them usable as a
// Location, and what the previous computation got wrong for a decoded file: it
// mixed the position of the file with a number of decoded bytes.
func TestOffsetsAreFilePositions(t *testing.T) {
	tailTest, cleanup := NewTailTest("offsets-are-positions", t)
	defer cleanup()

	lines := []string{"hello\n", "wörld\n", "a🙂b\n"}
	content := encodeUTF16(t, strings.Join(lines, ""), unicode.LittleEndian, true)
	tailTest.CreateFileBytes("test.log", content)

	tail := tailTest.StartTail("test.log", Config{Follow: true, Poll: true, Encoding: "UTF-16LE"})
	defer func() {
		tail.Stop()
		tail.Cleanup()
	}()

	_, offsets := collectLines(t, tail, len(lines))

	// The offset of a line is the position at which the next one starts: the
	// byte order mark, plus every line up to and including this one
	position := int64(len(encodeUTF16(t, "", unicode.LittleEndian, true)))
	for i, line := range lines {
		position += int64(len(encodeUTF16(t, line, unicode.LittleEndian, false)))
		if offsets[i] != position {
			t.Fatalf("line %d: expecting the offset %d, but got %d", i, position, offsets[i])
		}
	}
}

// TestOffsetsWithIncompleteLines checks that a line delivered in two pieces,
// because the writer had not finished it when the end of the file was reached,
// reports offsets that resume exactly where each piece ends.
func TestOffsetsWithIncompleteLines(t *testing.T) {
	tailTest, cleanup := NewTailTest("offsets-incomplete", t)
	defer cleanup()

	tailTest.CreateFileBytes("test.log", encodeUTF16(t, "hel", unicode.LittleEndian, true))
	tail := tailTest.StartTail("test.log", Config{Follow: true, Poll: true, Encoding: "UTF-16LE"})
	defer func() {
		tail.Stop()
		tail.Cleanup()
	}()

	// The incomplete line is delivered on its own, and its offset is the
	// position at which the writer will carry on
	_, offsets := collectLines(t, tail, 1)
	bom := int64(len(encodeUTF16(t, "", unicode.LittleEndian, true)))
	if expected := bom + int64(len(encodeUTF16(t, "hel", unicode.LittleEndian, false))); offsets[0] != expected {
		t.Fatalf("expecting the offset %d for the incomplete line, but got %d", expected, offsets[0])
	}

	settle()
	tailTest.AppendFileBytes("test.log", encodeUTF16(t, "lo\n", unicode.LittleEndian, false))

	lines, offsets := collectLines(t, tail, 1)
	if lines[0] != "lo" {
		t.Fatalf("expecting the rest of the line, but got <<<%s>>>", lines[0])
	}
	if expected := bom + int64(len(encodeUTF16(t, "hello\n", unicode.LittleEndian, false))); offsets[0] != expected {
		t.Fatalf("expecting the offset %d at the end of the line, but got %d", expected, offsets[0])
	}
}

// TestOffsetsSurviveARotation checks that the offsets stay consistent with the
// file after it has been recycled, which resets the decoder.
func TestOffsetsSurviveARotation(t *testing.T) {
	tailTest, cleanup := NewTailTest("offsets-rotation", t)
	defer cleanup()

	tailTest.CreateFileBytes("test.log", encodeUTF16(t, "before\n", unicode.LittleEndian, true))
	tail := tailTest.StartTail("test.log", Config{Follow: true, Poll: true, Encoding: "UTF-16LE"})
	defer func() {
		tail.Stop()
		tail.Cleanup()
	}()

	expectLine(t, tail, "before")

	settle()
	tailTest.TruncateFileBytes("test.log", encodeUTF16(t, "after\n", unicode.LittleEndian, true))

	_, offsets := collectLines(t, tail, 1)
	expected := int64(len(encodeUTF16(t, "after\n", unicode.LittleEndian, true)))
	if offsets[0] != expected {
		t.Fatalf("expecting the offset %d in the recycled file, but got %d", expected, offsets[0])
	}
}
