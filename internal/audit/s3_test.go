package audit

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestS3SinkValidatesAndSpools(t *testing.T) {
	if _, err := NewS3Sink(S3Config{Bucket: "b"}); err == nil {
		t.Error("missing endpoint accepted")
	}
	if _, err := NewS3Sink(S3Config{Endpoint: "e"}); err == nil {
		t.Error("missing bucket accepted")
	}
	spool := filepath.Join(t.TempDir(), "spool")
	no := false
	s, err := NewS3Sink(S3Config{Endpoint: "127.0.0.1:1", Bucket: "audit", Prefix: "runs", UseSSL: &no, Spool: spool, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if s.Key("run_01") != "runs/run_01.ndjson" || s.URI("run_01") != "s3://audit/runs/run_01.ndjson" {
		t.Errorf("key %q uri %q", s.Key("run_01"), s.URI("run_01"))
	}
	if _, err := s.Open(context.Background(), "../etc/passwd"); err == nil {
		t.Error("invalid run id accepted")
	}

	w, err := s.Open(context.Background(), "run_01")
	if err != nil {
		t.Fatal(err)
	}
	if w.URI() != s.URI("run_01") {
		t.Errorf("writer uri %q", w.URI())
	}
	if err := w.Append([]byte(`{"type":"system"}`)); err != nil {
		t.Fatal(err)
	}
	// While the run is in flight the spool file serves reads.
	rc, err := s.Reader(context.Background(), "run_01")
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()

	// The endpoint is unreachable, so Close reports the upload failure and,
	// crucially, keeps the spool file: the audit trail is never discarded.
	if err := w.Close(); err == nil {
		t.Error("Close hid the upload failure")
	}
	b, err := os.ReadFile(filepath.Join(spool, "run_01.ndjson"))
	if err != nil {
		t.Fatalf("spool file removed despite a failed upload: %v", err)
	}
	if !strings.Contains(string(b), `{"type":"system"}`) {
		t.Errorf("spool contents %q", b)
	}
	rc, err = s.Reader(context.Background(), "run_01")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !strings.Contains(string(got), "system") {
		t.Errorf("reader served %q", got)
	}
}

func TestParseS3URL(t *testing.T) {
	cases := []struct {
		in       string
		endpoint string
		bucket   string
		prefix   string
		ssl      bool
		wantErr  bool
	}{
		{in: "s3://audit-bucket/runs/", bucket: "audit-bucket", prefix: "runs/", ssl: true},
		{in: "http://localhost:9000/audit/runs", endpoint: "localhost:9000", bucket: "audit", prefix: "runs"},
		{in: "https://s3.eu-west-1.amazonaws.com/audit", endpoint: "s3.eu-west-1.amazonaws.com", bucket: "audit", ssl: true},
		{in: "http://localhost:9000/", wantErr: true},
		{in: "ftp://x/y", wantErr: true},
	}
	for _, c := range cases {
		ep, b, p, ssl, err := ParseS3URL(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: expected an error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		if ep != c.endpoint || b != c.bucket || p != c.prefix || ssl != c.ssl {
			t.Errorf("%s: got (%q,%q,%q,%v)", c.in, ep, b, p, ssl)
		}
	}
}
