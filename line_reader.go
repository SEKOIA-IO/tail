// Copyright (c) 2019 FOSS contributors of https://github.com/SEKOIA-IO/tail

package tail

import "bufio"

// lineReader reads the lines of the tailed file, and keeps track of how much of
// what it has read from the file has not been handed to its caller yet, so that
// the position of the consumer in the file can be computed.
type lineReader interface {
	// ReadString reads until the first occurrence of delim, returning the data
	// read before the error when one occurs, as bufio.Reader does
	ReadString(delim byte) (string, error)

	// PendingSourceBytes returns the number of bytes read from the file that
	// have not been returned to the caller yet, counted in source (undecoded)
	// bytes. Subtracting it from the position of the file gives the position of
	// the consumer, which is what Tail.Tell reports and what a later Location
	// can be set to.
	PendingSourceBytes() int
}

// bufferedLineReader reads a file that needs no decoding: a byte read from the
// file is a byte returned to the caller, so the buffered bytes are the pending
// source bytes.
type bufferedLineReader struct {
	*bufio.Reader
}

func newBufferedLineReader(reader *bufio.Reader) bufferedLineReader {
	return bufferedLineReader{reader}
}

func (reader bufferedLineReader) PendingSourceBytes() int {
	return reader.Buffered()
}
