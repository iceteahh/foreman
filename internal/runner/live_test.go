//go:build live

package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/100xteam-ai/harness-loop-platform-go/internal/audit"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/events"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/session"
	"github.com/100xteam-ai/harness-loop-platform-go/internal/task"
)

// TestLiveTrivialPrompt spends a few cents. Run with:
//
//	make live   (loads .env; needs ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN)
func TestLiveTrivialPrompt(t *testing.T) {
	key, oauth := os.Getenv("ANTHROPIC_API_KEY"), os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")
	if key == "" && oauth == "" {
		t.Skip("neither ANTHROPIC_API_KEY nor CLAUDE_CODE_OAUTH_TOKEN is set")
	}
	if err := CheckVersion(context.Background(), DefaultBin); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	sess, _ := session.NewLocal(filepath.Join(root, "sessions"))
	aud, _ := audit.NewFileSink(filepath.Join(root, "audit"))
	rn := &Runner{APIKey: key, OAuthToken: oauth, Sessions: sess, Audit: aud}
	tk := &task.Task{ID: task.NewTaskID(), Kind: task.KindCodeFix, Prompt: "Reply with exactly the word OK and nothing else.",
		Workspace: task.WorkspaceSpec{Type: "git", Repo: "x", Ref: "main"},
		Policy:    task.Policy{AllowedTools: []string{"Read"}, MaxTurns: 2, TimeoutMS: 120000, MaxCostUSD: 0.20, Model: "haiku"}}
	r := task.NewRun(tk, time.Now())
	ws := filepath.Join(root, "ws")
	_ = os.MkdirAll(ws, 0o750)
	res, err := rn.Run(context.Background(), tk, r, Spawn{Workspace: ws, ConfigDir: filepath.Join(root, "cfg")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != events.OutcomeCompleted || res.Result.SessionID != r.SessionID || res.SessionURI == "" {
		t.Fatalf("%+v stderr=%s", res, res.Stderr)
	}
	t.Logf("cost=%.4f est=%.4f turns=%d text=%q", res.Result.TotalCostUSD, res.EstCostUSD, res.Result.NumTurns, res.Result.Text())
}
