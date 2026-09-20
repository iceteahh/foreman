package strutil

import "testing"

func TestTruncate(t *testing.T) {
	for _, tc := range []struct{ in string; n int; want string }{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello world", 5, "hello…"},
	} {
		if got := Truncate(tc.in, tc.n); got != tc.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

func TestTitle(t *testing.T) {
	if got := Title("hello wide world"); got != "Hello Wide World" {
		t.Errorf("Title = %q", got)
	}
}
