package golden

import (
	"fmt"
	"io"
)

// lines is a small write-error-remembering wrapper, the same shape
// audit.errWriter uses: a rendered report is dozens of Fprintf calls, and
// checking each one inline would bury the format strings. Both renderers
// write to a terminal or a CI log, where a write error means the reader is
// already gone, so the first failure is remembered and the rest skipped.
type lines struct {
	w   io.Writer
	err error
}

func (l *lines) printf(format string, args ...any) {
	if l.err != nil {
		return
	}
	if _, err := fmt.Fprintf(l.w, format, args...); err != nil {
		l.err = err
	}
}

func (l *lines) nl() { l.printf("\n") }
