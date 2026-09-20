package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotenv(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	body := "# comment\n\nHARNESS_T_A=plain # trailing\nexport HARNESS_T_B=\"quoted # not a comment\"\nHARNESS_T_C='single'\nHARNESS_T_EMPTY=\nHARNESS_T_EXISTING=from-file\nnot a pair\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HARNESS_T_EXISTING", "from-env")
	for _, k := range []string{"HARNESS_T_A", "HARNESS_T_B", "HARNESS_T_C", "HARNESS_T_EMPTY"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	loaded, err := loadDotenv(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 3 {
		t.Errorf("loaded %v", loaded)
	}
	for k, want := range map[string]string{"HARNESS_T_A": "plain", "HARNESS_T_B": "quoted # not a comment", "HARNESS_T_C": "single", "HARNESS_T_EXISTING": "from-env"} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q want %q", k, got, want)
		}
	}
	if _, ok := os.LookupEnv("HARNESS_T_EMPTY"); ok {
		t.Error("empty value must not be set")
	}
	if l, err := loadDotenv(filepath.Join(t.TempDir(), "missing")); err != nil || l != nil {
		t.Errorf("missing file: %v %v", l, err)
	}
}
