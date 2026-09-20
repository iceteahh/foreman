package events

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
)

// DefaultMaxLine bounds one stdout line (10 MiB). Tool results can be large.
const DefaultMaxLine = 10 << 20

// ErrLineTooLong is returned when a line exceeds the configured maximum.
var ErrLineTooLong = errors.New("events: line exceeds maximum size")

// Decoder reads NDJSON events from a stream.
type Decoder struct {
	r       *bufio.Reader
	maxLine int
	line    int
	err     error
}

// Option configures a Decoder.
type Option func(*Decoder)

// WithMaxLine sets the maximum accepted line length in bytes.
func WithMaxLine(n int) Option { return func(d *Decoder) { d.maxLine = n } }

// NewDecoder wraps r.
func NewDecoder(r io.Reader, opts ...Option) *Decoder {
	d := &Decoder{r: bufio.NewReaderSize(r, 64<<10), maxLine: DefaultMaxLine}
	for _, o := range opts {
		o(d)
	}
	return d
}

// Next returns the next event, io.EOF at end of stream, or ErrLineTooLong.
// Blank lines are skipped; non-JSON lines are returned as TypeRaw events.
func (d *Decoder) Next() (*Event, error) {
	if d.err != nil {
		return nil, d.err
	}
	for {
		line, err := d.readLine()
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				d.err = io.EOF
				d.line++
				return Parse(line, d.line), nil
			}
			d.err = err
			return nil, err
		}
		d.line++
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		return Parse(line, d.line), nil
	}
}

func (d *Decoder) readLine() ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := d.readChunk()
		buf = append(buf, chunk...)
		if len(buf) > d.maxLine {
			return nil, fmt.Errorf("%w (line %d, > %d bytes)", ErrLineTooLong, d.line+1, d.maxLine)
		}
		if err != nil {
			return buf, err
		}
		if !isPrefix {
			return buf, nil
		}
	}
}

// readChunk reads up to a newline; isPrefix reports the line continues.
func (d *Decoder) readChunk() ([]byte, bool, error) {
	chunk, err := d.r.ReadSlice('\n')
	switch {
	case errors.Is(err, bufio.ErrBufferFull):
		out := make([]byte, len(chunk))
		copy(out, chunk)
		return out, true, nil
	case err != nil:
		return chunk, false, err
	}
	return bytes.TrimRight(chunk, "\r\n"), false, nil
}

// Line reports how many lines have been consumed.
func (d *Decoder) Line() int { return d.line }
