package audit

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

var runIDRe = regexp.MustCompile(`^run_[0-9A-HJKMNP-TV-Z]{26}$|^run_[A-Za-z0-9_-]{1,64}$`)

// FileSink writes <root>/<run_id>.ndjson.
type FileSink struct {
	Root string
}

// NewFileSink creates root if needed.
func NewFileSink(root string) (*FileSink, error) {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("audit root: %w", err)
	}
	return &FileSink{Root: root}, nil
}

func (s *FileSink) path(runID string) (string, error) {
	if !runIDRe.MatchString(runID) {
		return "", fmt.Errorf("audit: invalid run id %q", runID)
	}
	return filepath.Join(s.Root, runID+".ndjson"), nil
}

// Open creates (or truncates) the run's log.
func (s *FileSink) Open(_ context.Context, runID string) (Writer, error) {
	p, err := s.path(runID)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("audit open: %w", err)
	}
	return &fileWriter{f: f, w: newBuf(f), uri: "file://" + p}, nil
}

// Reader opens the run's log for reading.
func (s *FileSink) Reader(_ context.Context, runID string) (io.ReadCloser, error) {
	p, err := s.path(runID)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

// bufWriter is the buffered writer both sinks use.
type bufWriter = bufio.Writer

// newBuf wraps f with the sink's standard buffer size.
func newBuf(f *os.File) *bufWriter { return bufio.NewWriterSize(f, 64<<10) }

type fileWriter struct {
	mu  sync.Mutex
	f   *os.File
	w   *bufio.Writer
	uri string
}

func (w *fileWriter) Append(line []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.w.Write(line); err != nil {
		return err
	}
	if len(line) == 0 || line[len(line)-1] != '\n' {
		if err := w.w.WriteByte('\n'); err != nil {
			return err
		}
	}
	return nil
}

func (w *fileWriter) URI() string { return w.uri }

func (w *fileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.w.Flush(); err != nil {
		_ = w.f.Close()
		return err
	}
	if err := w.f.Sync(); err != nil {
		_ = w.f.Close()
		return err
	}
	return w.f.Close()
}

var _ Sink = (*FileSink)(nil)
