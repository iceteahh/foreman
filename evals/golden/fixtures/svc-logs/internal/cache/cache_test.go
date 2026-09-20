package cache

import "testing"

func TestSetGet(t *testing.T) {
	c := New()
	c.Set("abc", 300)
	if v, ok := c.Get("abc"); !ok || v != 300 {
		t.Fatalf("Get = %d, %v", v, ok)
	}
	if _, ok := c.Get("missing"); ok {
		t.Fatal("missing key reported as present")
	}
}
