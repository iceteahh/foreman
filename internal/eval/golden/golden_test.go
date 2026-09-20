package golden

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---------- loading ----------

func writeCase(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name+".json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// suiteDir builds a suite directory with one fixture tree shared by the cases.
func suiteDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	fx := filepath.Join(dir, "fixtures", "mod")
	if err := os.MkdirAll(fx, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"go.mod":    "module demo\n\ngo 1.22\n",
		"main.go":   "package demo\n\nfunc Greet() string { return \"hi\" }\n",
		"CLAUDE.md": "# demo\n",
	} {
		if err := os.WriteFile(filepath.Join(fx, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const minimalCase = `{
  "description": "a case",
  "kind": "code_fix",
  "prompt": "do the thing",
  "repo": { "dir": "fixtures/mod" },
  "expect": { "outcome": "pass" }
}`

func TestLoadCaseDefaultsIDToFilename(t *testing.T) {
	dir := suiteDir(t)
	p := writeCase(t, dir, "my-case", minimalCase)
	c, err := LoadCase(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != "my-case" {
		t.Errorf("id = %q, want my-case", c.ID)
	}
	if c.Repo.Branch() != "main" {
		t.Errorf("branch = %q, want main", c.Repo.Branch())
	}
	spec := c.Spec("/tmp/origin.git")
	if spec.Workspace.Repo != "/tmp/origin.git" || spec.Workspace.Ref != "main" || spec.Workspace.Type != "git" {
		t.Errorf("spec workspace %+v", spec.Workspace)
	}
	if spec.RequestedBy != "golden:my-case" {
		t.Errorf("requested_by %q", spec.RequestedBy)
	}
}

func TestLoadRejectsBadCases(t *testing.T) {
	for name, body := range map[string]string{
		"unknown field":   `{"description":"d","kind":"code_fix","prompt":"p","repo":{"dir":"fixtures/mod"},"expect":{"outcome":"pass"},"typo":1}`,
		"bad outcome":     `{"description":"d","kind":"code_fix","prompt":"p","repo":{"dir":"fixtures/mod"},"expect":{"outcome":"delivered"}}`,
		"unknown kind":    `{"description":"d","kind":"nope","prompt":"p","repo":{"dir":"fixtures/mod"},"expect":{"outcome":"pass"}}`,
		"no description":  `{"kind":"code_fix","prompt":"p","repo":{"dir":"fixtures/mod"},"expect":{"outcome":"pass"}}`,
		"no prompt":       `{"description":"d","kind":"code_fix","repo":{"dir":"fixtures/mod"},"expect":{"outcome":"pass"}}`,
		"no repo":         `{"description":"d","kind":"code_fix","prompt":"p","expect":{"outcome":"pass"}}`,
		"both repo forms": `{"description":"d","kind":"code_fix","prompt":"p","repo":{"dir":"fixtures/mod","bundle":"x.bundle"},"expect":{"outcome":"pass"}}`,
		"missing fixture": `{"description":"d","kind":"code_fix","prompt":"p","repo":{"dir":"fixtures/gone"},"expect":{"outcome":"pass"}}`,
		"escaping path":   `{"description":"d","kind":"code_fix","prompt":"p","repo":{"dir":"../../etc"},"expect":{"outcome":"pass"}}`,
		"bad judge":       `{"description":"d","kind":"code_fix","prompt":"p","repo":{"dir":"fixtures/mod"},"expect":{"outcome":"pass","judge_verdict":"great"}}`,
		"files on a fail": `{"description":"d","kind":"code_fix","prompt":"p","repo":{"dir":"fixtures/mod"},"expect":{"outcome":"fail","files_touched":["a.go"]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := suiteDir(t)
			p := writeCase(t, dir, "c", body)
			if _, err := LoadCase(p); err == nil {
				t.Fatalf("%s accepted", name)
			}
		})
	}
}

func TestLoadRejectsDuplicateIDs(t *testing.T) {
	dir := suiteDir(t)
	writeCase(t, dir, "a", `{"id":"same","description":"d","kind":"code_fix","prompt":"p","repo":{"dir":"fixtures/mod"},"expect":{"outcome":"pass"}}`)
	writeCase(t, dir, "b", `{"id":"same","description":"d","kind":"code_fix","prompt":"p","repo":{"dir":"fixtures/mod"},"expect":{"outcome":"pass"}}`)
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "same") {
		t.Fatalf("duplicate ids accepted: %v", err)
	}
}

func TestLoadSortsAndIgnoresSubdirs(t *testing.T) {
	dir := suiteDir(t)
	writeCase(t, dir, "zeta", minimalCase)
	writeCase(t, dir, "alpha", minimalCase)
	// A stray JSON file inside the fixture tree must not become a case.
	if err := os.WriteFile(filepath.Join(dir, "fixtures", "mod", "package.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.IDs(); len(got) != 2 || got[0] != "alpha" || got[1] != "zeta" {
		t.Fatalf("ids = %v", got)
	}
	if _, ok := s.Get("alpha"); !ok {
		t.Error("Get(alpha) missed")
	}
}

func TestLoadEmptyDirFails(t *testing.T) {
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("empty suite accepted")
	}
}

// ---------- fixtures ----------

func TestMaterializeDirBuildsCloneableOrigin(t *testing.T) {
	requireGit(t)
	dir := suiteDir(t)
	p := writeCase(t, dir, "c", minimalCase)
	c, err := LoadCase(p)
	if err != nil {
		t.Fatal(err)
	}
	f := &Fixtures{Root: t.TempDir()}
	origin, err := f.Materialize(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	// What the harness does with it: a shallow single-branch clone.
	work := filepath.Join(t.TempDir(), "clone")
	if out, err := exec.Command("git", "clone", "--quiet", "--depth", "1", "--branch", "main", "--single-branch", origin, work).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v: %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(work, "main.go")); err != nil {
		t.Errorf("fixture file missing from the clone: %v", err)
	}
	// The checked-in fixture must not have been turned into a repository.
	if _, err := os.Stat(filepath.Join(dir, "fixtures", "mod", ".git")); err == nil {
		t.Error("Materialize created a .git inside the suite's fixture tree")
	}
}

func TestMaterializeIsRepeatable(t *testing.T) {
	requireGit(t)
	dir := suiteDir(t)
	c, err := LoadCase(writeCase(t, dir, "c", minimalCase))
	if err != nil {
		t.Fatal(err)
	}
	f := &Fixtures{Root: t.TempDir()}
	first, err := f.Materialize(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	// A leftover origin from a previous case run must be replaced, not merged.
	if err := os.WriteFile(filepath.Join(first, "STALE"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := f.Materialize(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(second, "STALE")); err == nil {
		t.Error("the stale origin was reused")
	}
}

func TestMaterializeBundle(t *testing.T) {
	requireGit(t)
	dir := suiteDir(t)
	bundle := makeBundle(t, filepath.Join(dir, "fixtures", "mod"), filepath.Join(dir, "fixtures", "mod.bundle"))
	c, err := LoadCase(writeCase(t, dir, "c", `{
      "description":"from a bundle","kind":"code_fix","prompt":"p",
      "repo":{"bundle":"fixtures/mod.bundle","ref":"main"},
      "expect":{"outcome":"pass"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bundle); err != nil {
		t.Fatal(err)
	}
	f := &Fixtures{Root: t.TempDir()}
	origin, err := f.Materialize(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "-C", origin, "log", "--oneline", "main").CombinedOutput()
	if err != nil {
		t.Fatalf("log: %v: %s", err, out)
	}
}

func TestMaterializeWrongBranchFails(t *testing.T) {
	requireGit(t)
	dir := suiteDir(t)
	c, err := LoadCase(writeCase(t, dir, "c", `{
      "description":"d","kind":"code_fix","prompt":"p",
      "repo":{"dir":"fixtures/mod","ref":"main"},"expect":{"outcome":"pass"}}`))
	if err != nil {
		t.Fatal(err)
	}
	c.Repo.Ref = "release" // the fixture is committed on main
	f := &Fixtures{Root: t.TempDir()}
	// commitTree creates whatever branch is asked for, so force the mismatch
	// the way a bundle would produce it.
	c.Repo.Dir, c.Repo.Bundle = "", "fixtures/mod.bundle"
	makeBundle(t, filepath.Join(dir, "fixtures", "mod"), filepath.Join(dir, "fixtures", "mod.bundle"))
	if _, err := f.Materialize(context.Background(), c); err == nil {
		t.Fatal("a fixture without the requested branch was accepted")
	}
}

func makeBundle(t *testing.T, src, dst string) string {
	t.Helper()
	work := t.TempDir()
	if err := copyTree(src, work); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "--quiet", "-b", "main"},
		{"config", "user.name", "t"},
		{"config", "user.email", "t@x"},
		{"add", "-A"},
		{"commit", "--quiet", "-m", "fixture"},
		{"bundle", "create", dst, "main"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = work
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dst
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

// A fixture must not carry build or cache droppings into its repository: they
// would land in every case's base commit, and anyone who ran the fixture's own
// tests by hand would change what the suite measures.
func TestMaterializeSkipsCachesAndGit(t *testing.T) {
	requireGit(t)
	dir := suiteDir(t)
	fx := filepath.Join(dir, "fixtures", "mod")
	for _, junk := range []string{"__pycache__/x.pyc", "node_modules/pkg/index.js", ".git/HEAD", ".pytest_cache/v"} {
		p := filepath.Join(fx, junk)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("junk"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c, err := LoadCase(writeCase(t, dir, "c", minimalCase))
	if err != nil {
		t.Fatal(err)
	}
	f := &Fixtures{Root: t.TempDir()}
	origin, err := f.Materialize(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "-C", origin, "ls-tree", "-r", "--name-only", "main").Output()
	if err != nil {
		t.Fatal(err)
	}
	files := strings.Fields(string(out))
	for _, f := range files {
		for _, bad := range []string{"__pycache__", "node_modules", ".pytest_cache", ".git/"} {
			if strings.Contains(f, bad) {
				t.Errorf("fixture repo contains %q", f)
			}
		}
	}
	if len(files) != 3 { // go.mod, main.go, CLAUDE.md
		t.Errorf("fixture repo holds %v, want only the fixture's own files", files)
	}
}
