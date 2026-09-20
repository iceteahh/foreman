package audit

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
)

func TestFileSinkRoundTrip(t *testing.T) {
	s, err := NewFileSink(t.TempDir() + "/audit")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	w, err := s.Open(ctx, "run_01J8000000000000000000TEST")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{`{"type":"system"}`, "raw text", `{"type":"result"}` + "\n"} {
		if err := w.Append([]byte(l)); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.HasPrefix(w.URI(), "file://") {
		t.Errorf("uri %q", w.URI())
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := s.Reader(ctx, "run_01J8000000000000000000TEST")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	_ = r.Close()
	if string(b) != "{\"type\":\"system\"}\nraw text\n{\"type\":\"result\"}\n" {
		t.Errorf("content:\n%s", b)
	}
	if _, err := os.Stat(strings.TrimPrefix(w.URI(), "file://")); err != nil {
		t.Error(err)
	}
}

func TestFileSinkRejectsBadID(t *testing.T) {
	s, _ := NewFileSink(t.TempDir())
	if _, err := s.Open(context.Background(), "../escape"); err == nil {
		t.Error("path traversal accepted")
	}
}
