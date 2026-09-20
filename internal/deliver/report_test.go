package deliver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/100xteam-ai/foreman/internal/eval/checks"
	"github.com/100xteam-ai/foreman/internal/task"
)

// readOnlyInput builds the delivery input of a finished read-only run. It has
// no workspace: a report kind changes nothing, so there is nothing to commit.
func readOnlyInput(t *testing.T, kind task.Kind, output string) *Input {
	t.Helper()
	tk := &task.Task{ID: "tsk_01J8000000000000000000TEST", Kind: kind, Title: "Review the price cache",
		Prompt:      "review it",
		Workspace:   task.WorkspaceSpec{Type: "git", Repo: "org/svc", Ref: "main"},
		Policy:      task.Policy{AllowedTools: []string{"Read"}, MaxTurns: 1, TimeoutMS: 1000, MaxCostUSD: 1},
		RequestedBy: "cron:nightly-review"}
	r := task.NewRun(tk, time.Now())
	if output != "" {
		r.Output = json.RawMessage(output)
	}
	rep := checks.Report{Results: []checks.Result{{Name: "diff_sanity", Status: checks.Pass, Evidence: "no changes (read-only task)"}}}
	return &Input{Task: tk, Run: r, Checks: rep}
}

func TestReportWritesJSONAndMarkdown(t *testing.T) {
	dir := t.TempDir()
	in := readOnlyInput(t, task.KindCodeReview, `{"verdict":"request_changes","findings":[{"file":"cache.go","severity":"blocker","category":"correctness","finding":"map written from several goroutines"}]}`)
	arts, err := (Report{Dir: dir}).Deliver(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 2 {
		t.Fatalf("artifacts %v", arts)
	}
	jsonPath := filepath.Join(dir, in.Run.ID+".json")
	b, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("the delivered report is not valid JSON: %v", err)
	}
	if back["verdict"] != "request_changes" {
		t.Errorf("verdict %v", back["verdict"])
	}
	md, err := os.ReadFile(filepath.Join(dir, in.Run.ID+".md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Review the price cache", "cache.go", in.Run.ID, "code_review", "diff_sanity", "no file was changed"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}
}

// A retried delivery must overwrite, never duplicate (design §9).
func TestReportDeliveryIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	in := readOnlyInput(t, task.KindTriage, `{"category":"bug"}`)
	first, err := (Report{Dir: dir}).Deliver(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := (Report{Dir: dir}).Deliver(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(first, ",") != strings.Join(second, ",") {
		t.Errorf("artifacts changed on redelivery: %v then %v", first, second)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("redelivery left %d files, want 2", len(entries))
	}
}

func TestReportWithNoOutputAndNoTextFails(t *testing.T) {
	in := readOnlyInput(t, task.KindReport, "")
	if _, err := (Report{Dir: t.TempDir()}).Deliver(context.Background(), in); err == nil {
		t.Fatal("a run with nothing to deliver was delivered")
	}
}

// notifier records what it was handed and can be made to fail.
type notifier struct {
	title, markdown string
	err             error
}

func (n *notifier) NotifyReport(_ context.Context, _ *task.Task, _ *task.Run, title, markdown string) (string, error) {
	n.title, n.markdown = title, markdown
	if n.err != nil {
		return "", n.err
	}
	return "slack:C1/171.1", nil
}

func TestReportNotifies(t *testing.T) {
	n := &notifier{}
	in := readOnlyInput(t, task.KindCodeReview, `{"verdict":"approve"}`)
	arts, err := (Report{Dir: t.TempDir(), Notify: n}).Deliver(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 3 || arts[2] != "slack:C1/171.1" {
		t.Errorf("artifacts %v", arts)
	}
	if !strings.Contains(n.title, "Review the price cache") || !strings.Contains(n.markdown, "approve") {
		t.Errorf("notifier got %q / %q", n.title, n.markdown)
	}
}

// The report is already on disk when the notification fails: the caller must
// still learn where it is, so a Slack outage cannot lose a paid run.
func TestReportKeepsArtifactsWhenTheNotificationFails(t *testing.T) {
	dir := t.TempDir()
	n := &notifier{err: errors.New("slack: channel_not_found")}
	in := readOnlyInput(t, task.KindReport, `{"title":"x"}`)
	arts, err := (Report{Dir: dir, Notify: n}).Deliver(context.Background(), in)
	if err == nil {
		t.Fatal("the notification failure was swallowed")
	}
	if len(arts) != 2 {
		t.Fatalf("artifacts lost on a notification failure: %v", arts)
	}
	if _, statErr := os.Stat(filepath.Join(dir, in.Run.ID+".json")); statErr != nil {
		t.Errorf("the report file is gone: %v", statErr)
	}
}

func TestByKindRoutesReadOnlyKindsToTheReportAdapter(t *testing.T) {
	dir := t.TempDir()
	reportAdapter := Report{Dir: dir}
	by := &ByKind{Default: BranchPush{}, Adapters: map[task.Kind]Adapter{
		task.KindCodeReview: reportAdapter, task.KindReport: reportAdapter, task.KindTriage: reportAdapter,
	}}
	for _, k := range []task.Kind{task.KindCodeReview, task.KindReport, task.KindTriage} {
		if got := by.For(k); got.Name() != "report" {
			t.Errorf("%s routed to %s", k, got.Name())
		}
	}
	for _, k := range []task.Kind{task.KindCodeFix, task.KindCodeFixPlanned, task.KindCustom} {
		if got := by.For(k); got.Name() != "branch_push" {
			t.Errorf("%s routed to %s", k, got.Name())
		}
	}
	// End to end: a code_review run delivers a report instead of failing with
	// "nothing to deliver", which is what a single adapter would have done.
	in := readOnlyInput(t, task.KindCodeReview, `{"verdict":"approve"}`)
	arts, err := by.Deliver(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) == 0 || !strings.HasPrefix(arts[0], "report:") {
		t.Errorf("artifacts %v", arts)
	}
}

func TestByKindWithoutAnAdapterFails(t *testing.T) {
	by := &ByKind{}
	in := readOnlyInput(t, task.KindCodeReview, `{}`)
	if _, err := by.Deliver(context.Background(), in); err == nil {
		t.Fatal("a kind with no adapter delivered")
	}
}
