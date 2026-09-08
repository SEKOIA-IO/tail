// Copyright (c) 2019 FOSS contributors of https://github.com/SEKOIA-IO/tail

package tail

import (
	"bytes"
	"io"

	"golang.org/x/text/transform"
)

const decodingBufferSize = 8192

// decodingReader reads the lines of a file that is not UTF-8 encoded.
//
// It is a resumable alternative to transform.Reader, which latches the first
// error returned by the underlying reader: once a followed file reaches io.EOF,
// a transform.Reader is "complete" and never reads from the file again, so the
// appended data is never decoded. That silently stops the tailing of every non
// UTF-8 file (the MSSQL ERRORLOG, written in UTF-16LE, for example) after the
// first EOF.
//
// This reader keeps the bytes it could not decode yet (an incomplete character
// at the end of the file, typically) and resumes decoding on the next read, so
// io.EOF is only a transient state.
//
// It also splits the lines itself, instead of leaving that to a bufio.Reader,
// because it is the only way to relate a position in the decoded stream to a
// position in the source stream: a character can be one, two or four bytes
// long, and some bytes, a byte order mark for example, produce no character at
// all, so no arithmetic can convert one into the other.
type decodingReader struct {
	source io.Reader

	// decoder decodes the file as it is read
	decoder transform.Transformer
	// accountant lags behind the decoder and decodes the very same bytes a
	// second time, once the lines they produced have been returned to the
	// caller. Both see the same bytes in the same order, so they go through the
	// same states, and the accountant tells exactly how many source bytes a
	// returned line was made of.
	accountant transform.Transformer

	readBuffer    []byte
	decodeBuffer  []byte
	accountBuffer []byte

	// undecoded holds the bytes read from the file that the decoder has not
	// consumed yet
	undecoded []byte
	// unaccounted holds the bytes the decoder consumed to produce the decoded
	// bytes that have not been returned to the caller yet
	unaccounted []byte
	// decoded holds the decoded bytes that have not been returned yet
	decoded []byte
	// searched is the length of the prefix of decoded that has already been
	// scanned for the delimiter
	searched int
}

func newDecodingReader(source io.Reader, decoder, accountant transform.Transformer) *decodingReader {
	return &decodingReader{
		source:        source,
		decoder:       decoder,
		accountant:    accountant,
		readBuffer:    make([]byte, decodingBufferSize),
		decodeBuffer:  make([]byte, decodingBufferSize),
		accountBuffer: make([]byte, decodingBufferSize),
	}
}

func (reader *decodingReader) PendingSourceBytes() int {
	return len(reader.undecoded) + len(reader.unaccounted)
}

func (reader *decodingReader) ReadString(delim byte) (string, error) {
	for {
		if index := bytes.IndexByte(reader.decoded[reader.searched:], delim); index >= 0 {
			return reader.take(reader.searched + index + 1), nil
		}
		reader.searched = len(reader.decoded)

		n, err := reader.source.Read(reader.readBuffer)
		if n > 0 {
			reader.undecoded = append(reader.undecoded, reader.readBuffer[:n]...)
			if decodeErr := reader.decode(); decodeErr != nil {
				return "", decodeErr
			}
			continue
		}
		if err == nil {
			err = io.EOF
		}
		// Like bufio.Reader, return the data read before the error: the caller
		// decides what to do with a line that has no ending yet. The error is
		// not memorized, so the reader resumes when the file grows again.
		return reader.take(len(reader.decoded)), err
	}
}

// decode transforms as many of the bytes read from the file as possible,
// keeping the trailing bytes that do not form a complete character yet.
func (reader *decodingReader) decode() error {
	for len(reader.undecoded) > 0 {
		// atEOF is always false: the file is still being written, so an
		// incomplete sequence means "the writer has not finished yet", it must
		// not be flushed as an U+FFFD character
		nDst, nSrc, err := reader.decoder.Transform(reader.decodeBuffer, reader.undecoded, false)
		if nDst > 0 {
			reader.decoded = append(reader.decoded, reader.decodeBuffer[:nDst]...)
		}
		if nSrc > 0 {
			// The source bytes are kept for the accountant, which decodes them
			// again once the caller has taken the lines they produced
			reader.unaccounted = append(reader.unaccounted, reader.undecoded[:nSrc]...)
			reader.undecoded = consume(reader.undecoded, nSrc)
		}

		switch err {
		case nil, transform.ErrShortSrc:
			// Either everything was decoded, or the remaining bytes are an
			// incomplete character: keep them for the next read
			return nil
		case transform.ErrShortDst:
			if nDst == 0 && nSrc == 0 {
				// The destination buffer cannot even hold a single character,
				// which cannot happen with decodingBufferSize, but do not loop
				// forever
				return err
			}
		default:
			return err
		}
	}

	return nil
}

// take returns the first count decoded bytes and accounts for the source bytes
// they were made of.
func (reader *decodingReader) take(count int) string {
	if count == 0 {
		return ""
	}

	line := string(reader.decoded[:count])
	reader.account(count)

	reader.decoded = consume(reader.decoded, count)
	if reader.searched -= count; reader.searched < 0 {
		reader.searched = 0
	}

	return line
}

// account replays the decoding of the bytes that produced the decoded bytes
// being returned, to know exactly how many source bytes they were made of, and
// drops them from the pending count.
func (reader *decodingReader) account(decodedCount int) {
	produced, consumed := 0, 0

	for produced < decodedCount {
		// Capping the destination at what is left to account for makes the
		// transformer stop exactly at the end of the line: it never writes a
		// partial character, so it stops on the character boundary the line
		// ends with
		room := decodedCount - produced
		if room > len(reader.accountBuffer) {
			room = len(reader.accountBuffer)
		}

		nDst, nSrc, _ := reader.accountant.Transform(
			reader.accountBuffer[:room], reader.unaccounted[consumed:], false)
		produced += nDst
		consumed += nSrc

		if nDst == 0 && nSrc == 0 {
			// Unreachable: the accountant decodes the very bytes the decoder
			// already decoded. Give up rather than loop forever, and keep the
			// bytes that could not be accounted for: the reported position then
			// lags behind, which makes a later resume read a line twice instead
			// of skipping it.
			break
		}
	}

	reader.unaccounted = consume(reader.unaccounted, consumed)
}

// consume drops the first count bytes of a buffer, keeping its capacity.
func consume(buffer []byte, count int) []byte {
	return buffer[:copy(buffer, buffer[count:])]
}
