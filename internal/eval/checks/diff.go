package checks

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/100xteam-ai/foreman/internal/workspace"
)

// DiffScope: every changed file must match one of acceptance.diff_scope.
type DiffScope struct{}

func (DiffScope) Name() string { return "diff_scope" }

func (DiffScope) Run(_ context.Context, in *Input) Result {
	scope := in.Task.Acceptance.DiffScope
	if len(scope) == 0 {
		return na("task defines no diff_scope")
	}
	if in.Capture == nil {
		return na("no workspace capture")
	}
	var out []string
	for _, f := range in.Capture.ChangedFiles {
		if !inScope(f, scope) {
			out = append(out, f)
		}
	}
	ev := fmt.Sprintf("%d file(s) changed, scope=%v", len(in.Capture.ChangedFiles), scope)
	if len(out) > 0 {
		return fail(ev + "\nout-of-scope file(s) touched:\n" + strings.Join(out, "\n"))
	}
	return pass(ev)
}

func inScope(file string, globs []string) bool {
	file = path.Clean(strings.ReplaceAll(file, "\\", "/"))
	for _, g := range globs {
		if ok, err := doublestar.Match(g, file); err == nil && ok {
			return true
		}
	}
	return false
}

// DiffSanity: a change task must change something; a read-only task must not;
// tests must not be deleted or shrunk.
type DiffSanity struct{}

func (DiffSanity) Name() string { return "diff_sanity" }

func (DiffSanity) Run(_ context.Context, in *Input) Result {
	if in.Capture == nil {
		return na("no workspace capture")
	}
	changes := in.Task.ExpectsChanges()
	n := len(in.Capture.ChangedFiles)
	if changes && n == 0 {
		return fail("empty diff: the task requires a code change but no file was modified")
	}
	if !changes && n > 0 {
		return fail(fmt.Sprintf("read-only task modified %d file(s):\n%s", n, strings.Join(in.Capture.ChangedFiles, "\n")))
	}
	var problems []string
	for _, s := range in.Capture.Stats {
		if !IsTestFile(s.Path) {
			continue
		}
		switch {
		case s.Status == "D":
			problems = append(problems, fmt.Sprintf("deleted test file %s", s.Path))
		case !s.Binary && s.Deleted > s.Added:
			problems = append(problems, fmt.Sprintf("test file %s shrank (+%d/-%d)", s.Path, s.Added, s.Deleted))
		}
	}
	if len(problems) > 0 {
		return fail("tests were deleted or weakened:\n" + strings.Join(problems, "\n"))
	}
	if n == 0 {
		return pass("no changes (read-only task)")
	}
	return pass(fmt.Sprintf("%d file(s) changed: %s", n, strings.Join(in.Capture.ChangedFiles, ", ")))
}

// IsTestFile recognises common test file conventions.
func IsTestFile(p string) bool {
	p = strings.ReplaceAll(p, "\\", "/")
	base := path.Base(p)
	for _, dir := range []string{"test/", "tests/", "__tests__/", "spec/"} {
		if strings.HasPrefix(p, dir) || strings.Contains(p, "/"+dir) {
			return true
		}
	}
	for _, pat := range []string{"*_test.*", "*.test.*", "*.spec.*", "test_*.py", "*Test.java", "*Tests.cs", "*_spec.rb"} {
		if ok, _ := path.Match(pat, base); ok {
			return true
		}
	}
	return false
}

var _ = workspace.FileStat{}
