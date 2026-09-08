// Copyright (c) 2019 FOSS contributors of https://github.com/SEKOIA-IO/tail
// Copyright (c) 2015 HPE Software Inc. All rights reserved.
// Copyright (c) 2013 ActiveState Software Inc. All rights reserved.

// SEKOIA-IO/tail provides a Go library that emulates the features of the BSD `tail`
// program. The library comes with full support for truncation/move detection as
// it is designed to work with log rotation tools. The library works on all
// operating systems supported by Go, including POSIX systems like Linux and
// *BSD, and MS Windows. Go 1.9 is the oldest compiler release supported.
package tail

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/SEKOIA-IO/tail/ratelimiter"
	"github.com/SEKOIA-IO/tail/util"
	"github.com/SEKOIA-IO/tail/watch"
	"gopkg.in/tomb.v1"

	"github.com/gogs/chardet"
	"golang.org/x/text/encoding/ianaindex"
	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
)

var (
	// ErrStop is returned when the tail of a file has been marked to be stopped.
	ErrStop = errors.New("tail should now stop")
)

const (
	// defaultEncoding is used when no encoding is configured and none could be detected
	defaultEncoding = "UTF-8"
	// minimumDetectionSample is the number of bytes below which no detection is
	// attempted. Two bytes are enough to identify a file that only contains a
	// byte order mark, as a freshly rotated UTF-16 log file does.
	minimumDetectionSample = 2
	// detectionConfidenceThreshold is the confidence below which the detected
	// encoding is discarded in favor of the default one
	detectionConfidenceThreshold = 80
)

// utf8BOM is the byte order mark of an UTF-8 file. It carries no information,
// UTF-8 having a single byte order, but Windows editors and PowerShell write it.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

type Line struct {
	Text     string    // The contents of the file
	Num      int       // The line number
	SeekInfo SeekInfo  // SeekInfo
	Time     time.Time // Present time
	Err      error     // Error from tail
}

// Deprecated: this function is no longer used internally and it has little of no
// use in the API. As such, it will be removed from the API in a future major
// release.
//
// NewLine returns a * pointer to a Line struct.
func NewLine(text string, lineNum int) *Line {
	return &Line{text, lineNum, SeekInfo{}, time.Now(), nil}
}

// SeekInfo represents arguments to io.Seek. See: https://golang.org/pkg/io/#SectionReader.Seek
type SeekInfo struct {
	Offset int64
	Whence int
}

type logger interface {
	Printf(format string, v ...interface{})
}

// Config is used to specify how a file must be tailed.
type Config struct {
	// File-specifc
	Location  *SeekInfo // Tail from this location. If nil, start at the beginning of the file
	ReOpen    bool      // Reopen recreated files (tail -F)
	MustExist bool      // Fail early if the file does not exist
	Poll      bool      // Poll for file changes instead of using the default inotify
	Pipe      bool      // The file is a named pipe (mkfifo)

	// Generic IO
	Follow        bool // Continue looking for new lines (tail -f)
	MaxLineSize   int  // If non-zero, split longer lines into multiple lines
	CompleteLines bool // Only return complete lines (that end with "\n" or EOF when Follow is false)

	// Optionally, use a ratelimiter (e.g. created by the ratelimiter/NewLeakyBucket function)
	RateLimiter *ratelimiter.LeakyBucket

	// Optionally use a Logger. When nil, the Logger is set to tail.DefaultLogger.
	// To disable logging, set it to tail.DiscardingLogger
	Logger logger

	Encoding string // The encoding of the file. If empty, UTF-8 is assumed
}

type Tail struct {
	Filename string     // The filename
	Lines    chan *Line // A consumable channel of *Line
	Config              // Tail.Configuration

	file    *os.File
	reader  lineReader
	lineNum int

	// detectedEncoding memoizes the encoding detected for the file currently
	// open, so that reopening the reader (on a seek, for example) cannot change
	// the encoding in the middle of the stream. It is reset when the file is
	// reopened, so that a rotated file is examined again. An empty value means
	// that no conclusion could be drawn yet, and that detection must be retried
	// when the file has grown.
	detectedEncoding string

	// awaitingDetection reports that the file is too short for its encoding to be
	// detected at all, and that reading it would consume bytes that may have to
	// be decoded differently once the file has grown.
	awaitingDetection bool

	lineBuf *strings.Builder

	watcher watch.FileWatcher
	changes *watch.FileChanges

	tomb.Tomb // provides: Done, Kill, Dying

	lk sync.Mutex
}

var (
	// DefaultLogger logs to os.Stderr and it is used when Config.Logger == nil
	DefaultLogger = log.New(os.Stderr, "", log.LstdFlags)
	// DiscardingLogger can be used to disable logging output
	DiscardingLogger = log.New(io.Discard, "", 0)
)

// TailFile begins tailing the file. And returns a pointer to a Tail struct
// and an error. An output stream is made available via the Tail.Lines
// channel (e.g. to be looped and printed). To handle errors during tailing,
// after finishing reading from the Lines channel, invoke the `Wait` or `Err`
// method on the returned *Tail.
func TailFile(filename string, config Config) (*Tail, error) {
	if config.ReOpen && !config.Follow {
		util.Fatal("cannot set ReOpen without Follow.")
	}

	t := &Tail{
		Filename: filename,
		Lines:    make(chan *Line),
		Config:   config,
	}

	if config.CompleteLines {
		t.lineBuf = new(strings.Builder)
	}

	// when Logger was not specified in config, use default logger
	if t.Logger == nil {
		t.Logger = DefaultLogger
	}

	if t.Poll {
		t.watcher = watch.NewPollingFileWatcher(filename)
	} else {
		t.watcher = watch.NewInotifyFileWatcher(filename)
	}

	if t.MustExist {
		var err error
		t.file, err = OpenFile(t.Filename)
		if err != nil {
			return nil, err
		}
	}
	if config.Encoding != "" {
		// Make sure a decoder exists for this encoding
		_, err := ianaindex.IANA.Encoding(config.Encoding)
		if err != nil {
			return nil, err
		}
	}

	go t.tailFileSync()

	return t, nil
}

// Tell returns the position of the consumer in the file, like stdio's ftell(),
// and an error. It is the position at which the tailing can be resumed, and it
// is reported for every line through Line.SeekInfo.
//
// Beware that this value may not be completely accurate because one line from
// the chan(tail.Lines) may have been read already.
func (tail *Tail) Tell() (offset int64, err error) {
	if tail.file == nil {
		return
	}
	offset, err = tail.file.Seek(0, io.SeekCurrent)
	if err != nil {
		return
	}

	tail.lk.Lock()
	defer tail.lk.Unlock()
	if tail.reader == nil {
		return
	}

	// The position of the file is ahead of the consumer by everything that has
	// been read but not returned yet. For a file that is decoded, that includes
	// the bytes held by the decoder, and it is counted in source bytes: the
	// offset stays a position in the file, which is what a Location needs.
	offset -= int64(tail.reader.PendingSourceBytes())
	return
}

// Stop stops the tailing activity.
func (tail *Tail) Stop() error {
	tail.Kill(nil)
	return tail.Wait()
}

// StopAtEOF stops tailing as soon as the end of the file is reached. The function
// returns an error,
func (tail *Tail) StopAtEOF() error {
	tail.Kill(errStopAtEOF)
	return tail.Wait()
}

var errStopAtEOF = errors.New("tail: stop at eof")

func (tail *Tail) close() {
	close(tail.Lines)
	tail.closeFile()
}

func (tail *Tail) closeFile() {
	if tail.file != nil {
		tail.file.Close()
		tail.file = nil
	}
}

func (tail *Tail) reopen() error {
	if tail.lineBuf != nil {
		tail.lineBuf.Reset()
	}
	tail.closeFile()
	tail.lineNum = 0
	tail.detectedEncoding = ""
	tail.awaitingDetection = false
	for {
		var err error
		tail.file, err = OpenFile(tail.Filename)
		if err != nil {
			if os.IsNotExist(err) {
				tail.Logger.Printf("Waiting for %s to appear...", tail.Filename)
				if err := tail.watcher.BlockUntilExists(&tail.Tomb); err != nil {
					if err == tomb.ErrDying {
						return err
					}
					return fmt.Errorf("Failed to detect creation of %s: %s", tail.Filename, err)
				}
				continue
			}
			return fmt.Errorf("Unable to open file %s: %s", tail.Filename, err)
		}
		break
	}
	return nil
}

func (tail *Tail) readLine() (string, error) {
	if tail.awaitingDetection {
		// The file holds too few bytes for its encoding to be detected. Reading
		// them now would mean reading them as UTF-8, and there would be no way
		// back: the decoder that the detection ends up choosing can only align
		// itself on bytes that have not been consumed yet.
		return "", io.EOF
	}

	tail.lk.Lock()
	line, err := tail.reader.ReadString('\n')
	tail.lk.Unlock()

	newlineEnding := strings.HasSuffix(line, "\n")
	line = strings.TrimRight(line, "\r\n")

	// if we don't have to handle incomplete lines, we can return the line as-is
	if !tail.Config.CompleteLines {
		// Note ReadString "returns the data read before the error" in
		// case of an error, including EOF, so we return it as is. The
		// caller is expected to process it if err is EOF.
		return line, err
	}

	if _, err := tail.lineBuf.WriteString(line); err != nil {
		return line, err
	}

	if newlineEnding {
		line = tail.lineBuf.String()
		tail.lineBuf.Reset()
		return line, nil
	} else {
		if tail.Config.Follow {
			line = ""
		}
		return line, io.EOF
	}
}

func (tail *Tail) tailFileSync() {
	defer tail.Done()
	defer tail.close()

	if !tail.MustExist {
		// deferred first open.
		err := tail.reopen()
		if err != nil {
			if err != tomb.ErrDying {
				tail.Kill(err)
			}
			return
		}
	}

	// Seek to requested location on first open of the file.
	if tail.Location != nil {
		_, err := tail.file.Seek(tail.Location.Offset, tail.Location.Whence)
		if err != nil {
			tail.Killf("Seek error on %s: %s", tail.Filename, err)
			return
		}
	}

	tail.openReader()

	// Read line by line.
	for {
		line, err := tail.readLine()

		// Process `line` even if err is EOF.
		if err == nil {
			cooloff := !tail.sendLine(line)
			if cooloff {
				// Wait a second before seeking till the end of
				// file when rate limit is reached.
				msg := ("Too much log activity; waiting a second before resuming tailing")
				offset, _ := tail.Tell()
				tail.Lines <- &Line{msg, tail.lineNum, SeekInfo{Offset: offset}, time.Now(), errors.New(msg)}
				select {
				case <-time.After(time.Second):
				case <-tail.Dying():
					return
				}
				if err := tail.seekEnd(); err != nil {
					tail.Kill(err)
					return
				}
			}
		} else if err == io.EOF {
			if !tail.Follow {
				if line != "" {
					tail.sendLine(line)
				}
				return
			}

			if tail.Follow && line != "" {
				tail.sendLine(line)
			}

			// When EOF is reached, wait for more data to become
			// available. Wait strategy is based on the `tail.watcher`
			// implementation (inotify or polling).
			err := tail.waitForChanges()
			if err != nil {
				if err != ErrStop {
					tail.Kill(err)
				}
				return
			}

			tail.retryEncodingDetection()
		} else {
			// non-EOF error
			tail.Killf("Error reading %s: %s", tail.Filename, err)
			return
		}

		select {
		case <-tail.Dying():
			if tail.Err() == errStopAtEOF {
				continue
			}
			return
		default:
		}
	}
}

// waitForChanges waits until the file has been appended, deleted,
// moved or truncated. When moved or deleted - the file will be
// reopened if ReOpen is true. Truncated files are always reopened.
func (tail *Tail) waitForChanges() error {
	if tail.changes == nil {
		pos, err := tail.file.Seek(0, io.SeekCurrent)
		if err != nil {
			return err
		}
		tail.changes, err = tail.watcher.ChangeEvents(&tail.Tomb, pos)
		if err != nil {
			return err
		}
	}

	select {
	case <-tail.changes.Modified:
		return nil
	case <-tail.changes.Deleted:
		tail.changes = nil
		if tail.ReOpen {
			// XXX: we must not log from a library.
			tail.Logger.Printf("Re-opening moved/deleted file %s ...", tail.Filename)
			if err := tail.reopen(); err != nil {
				return err
			}
			tail.Logger.Printf("Successfully reopened %s", tail.Filename)
			tail.openReader()
			return nil
		}
		tail.Logger.Printf("Stopping tail as file no longer exists: %s", tail.Filename)
		return ErrStop
	case <-tail.changes.Truncated:
		// Always reopen truncated files (Follow is true)
		tail.Logger.Printf("Re-opening truncated file %s ...", tail.Filename)
		if err := tail.reopen(); err != nil {
			return err
		}
		tail.Logger.Printf("Successfully reopened truncated %s", tail.Filename)
		tail.openReader()
		return nil
	case <-tail.Dying():
		return ErrStop
	}
}

func (tail *Tail) openReader() {
	tail.lk.Lock()
	defer tail.lk.Unlock()

	if decoder, accountant := tail.newDecoders(); decoder != nil {
		// A decoded file is not read through a bufio.Reader: the decoding reader
		// splits the lines itself, which is what allows it to report a position
		// in the file rather than in the decoded stream
		tail.reader = newDecodingReader(tail.file, decoder, accountant, !tail.Follow)
		return
	}

	var buffered *bufio.Reader
	if tail.MaxLineSize > 0 {
		// add 2 to account for newline characters
		buffered = bufio.NewReaderSize(tail.file, tail.MaxLineSize+2)
	} else {
		buffered = bufio.NewReader(tail.file)
	}
	tail.skipUTF8BOM(buffered)
	tail.reader = newBufferedLineReader(buffered)
}

// skipUTF8BOM discards the byte order mark of an UTF-8 file, if any, so that it
// is not returned as a U+FEFF character prepended to the first line. Decoded
// files get the same treatment from unicode.BOMOverride.
//
// The offsets stay exact: the mark is consumed from the buffer, so it counts as
// read for both the position of the file and the pending bytes of the reader.
func (tail *Tail) skipUTF8BOM(reader *bufio.Reader) {
	if !tail.atStartOfFile() {
		return
	}

	prefix, err := reader.Peek(len(utf8BOM))
	if err == nil && bytes.Equal(prefix, utf8BOM) {
		reader.Discard(len(utf8BOM))
	}
}

// atStartOfFile reports whether the reader that is being built is about to read
// the very first byte of the file.
//
// A byte order mark is only a byte order mark there. The reader is rebuilt on
// every seek and reopen, in particular when the tailing resumes at a stored
// Location, and the bytes found at such a position are ordinary content: FF FE
// is a legitimate U+FEFF character in a UTF-16LE file, and simply "ÿþ" in an
// ISO-8859-1 one.
func (tail *Tail) atStartOfFile() bool {
	position, err := tail.file.Seek(0, io.SeekCurrent)
	if err != nil {
		// The position cannot be told, which is the case of a stream that cannot
		// be seeked, and such a reader necessarily starts at the beginning
		return true
	}
	return position == 0
}

// newDecoders returns the decoders the decoding reader needs, or nil when the
// file is UTF-8 encoded, or cannot be decoded, and must be read as-is.
//
// Two identical decoders are returned: one decodes the file as it is read, the
// other lags behind to count the source bytes of the lines that were returned.
func (tail *Tail) newDecoders() (decoder, accountant transform.Transformer) {
	encoding := tail.getEncoding()
	if strings.ToUpper(encoding) == defaultEncoding {
		// No need for a transformer
		return nil, nil
	}

	encode, err := ianaindex.IANA.Encoding(encoding)
	if err != nil || encode == nil {
		tail.Logger.Printf("No decoder available for the %s encoding of %s, "+
			"reading it as-is", encoding, tail.Filename)
		if tail.Encoding == "" {
			// The detection cannot give a better answer on the next open of the
			// reader: stop detecting, and stop warning, until the file is reopened
			tail.detectedEncoding = defaultEncoding
		}
		return nil, nil
	}

	// BOMOverride drops the byte order mark of the file, if any, instead of
	// decoding it as a U+FEFF character prepended to the first line. It also
	// corrects the endianness when the mark disagrees with the given encoding.
	//
	// It is only applied at the start of the file: a decoder built further in,
	// on a resumed tailing for example, would drop content, and could switch to
	// a completely different encoding for the rest of the file.
	if !tail.atStartOfFile() {
		return encode.NewDecoder(), encode.NewDecoder()
	}
	return unicode.BOMOverride(encode.NewDecoder()), unicode.BOMOverride(encode.NewDecoder())
}

func (tail *Tail) getEncoding() string {
	tail.awaitingDetection = false
	if tail.Encoding != "" {
		return tail.Encoding
	}
	if tail.detectedEncoding != "" {
		return tail.detectedEncoding
	}
	// Detect encoding
	currentOffset, err := tail.file.Seek(0, io.SeekCurrent)
	if err != nil {
		return defaultEncoding
	}
	tail.file.Seek(0, io.SeekStart)
	buf := make([]byte, 1024)
	// A single Read may return less than the whole buffer, even on a large file
	n, err := io.ReadFull(tail.file, buf)
	tail.file.Seek(currentOffset, io.SeekStart)
	if err != nil && err != io.ErrUnexpectedEOF {
		return defaultEncoding
	}
	if n < minimumDetectionSample {
		// Too few bytes to conclude anything: the file has just been created or
		// rotated, and it may well be the first byte of a byte order mark. Leave
		// the detection undecided so that it is retried when the file has grown,
		// and hold the reading back until then, as reading those bytes as UTF-8
		// would make it impossible to decode the rest of the file correctly.
		tail.awaitingDetection = tail.Follow
		return defaultEncoding
	}
	detector := chardet.NewTextDetector()
	// Only the bytes actually read: the tail of the buffer is made of NUL bytes,
	// which the detector confidently reports as UTF-32
	result, err := detector.DetectBest(buf[:n])
	if err != nil || result.Confidence < detectionConfidenceThreshold {
		if n == len(buf) {
			// A full sample was inconclusive: the beginning of the file will not
			// look any different later, stop detecting
			tail.detectedEncoding = defaultEncoding
		}
		return defaultEncoding
	}
	tail.detectedEncoding = result.Charset
	return tail.detectedEncoding
}

// retryEncodingDetection rebuilds the reader when the encoding of the file could
// not be detected yet, typically because the file was empty, or only made of a
// byte order mark, when the reader was opened. It gives the detection a new
// chance before the data that has just been appended is read as UTF-8.
//
// The detection is only allowed to change its mind while nothing has been
// consumed from the file. A decoder can align itself on the bytes it is given,
// not on the bytes that have already been read: installing one at a position
// reached by reading the file as UTF-8 would decode a UTF-16 file one byte out
// of phase, which produces mojibake, and no line ending at all in the common
// case, so the file would go silent. Once the file has been read as UTF-8, it
// keeps being read as UTF-8.
func (tail *Tail) retryEncodingDetection() {
	if tail.Encoding != "" || tail.detectedEncoding != "" {
		return
	}

	position, err := tail.Tell()
	if err != nil {
		return
	}
	if position > 0 {
		// Too late to decode this file differently
		tail.detectedEncoding = defaultEncoding
		tail.awaitingDetection = false
		return
	}

	// Rewind to the position of the consumer before rebuilding: the reader may
	// have read ahead of it, and those bytes have to be decoded, not skipped
	if err := tail.seekTo(SeekInfo{Offset: position, Whence: io.SeekStart}); err != nil {
		tail.Logger.Printf("%s", err)
	}
}

func (tail *Tail) seekEnd() error {
	return tail.seekTo(SeekInfo{Offset: 0, Whence: io.SeekEnd})
}

func (tail *Tail) seekTo(pos SeekInfo) error {
	_, err := tail.file.Seek(pos.Offset, pos.Whence)
	if err != nil {
		return fmt.Errorf("Seek error on %s: %s", tail.Filename, err)
	}
	// Rebuild the whole read chain whenever the file is re-seek'ed: resetting the
	// buffered reader on the file would drop the decoder and return raw bytes
	tail.openReader()
	return nil
}

// sendLine sends the line(s) to Lines channel, splitting longer lines
// if necessary. Return false if rate limit is reached.
func (tail *Tail) sendLine(line string) bool {
	now := time.Now()
	lines := []string{line}

	// Split longer lines
	if tail.MaxLineSize > 0 && len(line) > tail.MaxLineSize {
		lines = util.PartitionString(line, tail.MaxLineSize)
	}

	for _, line := range lines {
		tail.lineNum++
		offset, _ := tail.Tell()
		select {
		case tail.Lines <- &Line{line, tail.lineNum, SeekInfo{Offset: offset}, now, nil}:
		case <-tail.Dying():
			return true
		}
	}

	if tail.Config.RateLimiter != nil {
		ok := tail.Config.RateLimiter.Pour(uint16(len(lines)))
		if !ok {
			tail.Logger.Printf("Leaky bucket full (%v); entering 1s cooloff period.",
				tail.Filename)
			return false
		}
	}

	return true
}

// Cleanup removes inotify watches added by the tail package. This function is
// meant to be invoked from a process's exit handler. Linux kernel may not
// automatically remove inotify watches after the process exits.
// If you plan to re-read a file, don't call Cleanup in between.
func (tail *Tail) Cleanup() {
	watch.Cleanup(tail.Filename)
}
