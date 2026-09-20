package deliver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/go-github/v80/github"
)

// GitHubPR pushes the bot branch and upserts a pull request keyed by head
// branch harness/<task_id>: an open PR is updated, otherwise one is created.
type GitHubPR struct {
	Client *github.Client
	Draft  bool
	Logger *slog.Logger
}

// NewGitHubPR builds an authenticated client. apiBase "" means api.github.com.
func NewGitHubPR(token, apiBase string, draft bool) (*GitHubPR, error) {
	c := github.NewClient(nil)
	if token != "" {
		c = c.WithAuthToken(token)
	}
	if apiBase != "" {
		u, err := url.Parse(strings.TrimSuffix(apiBase, "/") + "/")
		if err != nil {
			return nil, fmt.Errorf("github api_base: %w", err)
		}
		c.BaseURL = u
	}
	return &GitHubPR{Client: c, Draft: draft}, nil
}

func (GitHubPR) Name() string { return "github_pr" }

// OwnerRepo extracts "owner", "repo" from "owner/repo", "owner/repo.git",
// "https://github.com/owner/repo(.git)" or "git@github.com:owner/repo.git".
func OwnerRepo(repo string) (string, string, error) {
	s := strings.TrimSuffix(strings.TrimSpace(repo), ".git")
	s = strings.TrimSuffix(s, "/")
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
		if j := strings.Index(s, "/"); j >= 0 {
			s = s[j+1:]
		}
	} else if strings.HasPrefix(s, "git@") {
		if j := strings.Index(s, ":"); j >= 0 {
			s = s[j+1:]
		}
	}
	parts := strings.Split(s, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.HasPrefix(repo, "/") || strings.HasPrefix(repo, ".") {
		return "", "", fmt.Errorf("deliver: cannot derive owner/repo from %q", repo)
	}
	return parts[0], parts[1], nil
}

// Deliver implements Adapter.
func (g *GitHubPR) Deliver(ctx context.Context, in *Input) ([]string, error) {
	owner, repo, err := OwnerRepo(in.Task.Workspace.Repo)
	if err != nil {
		return nil, err
	}
	sha, err := PushBranch(ctx, in)
	if err != nil {
		return nil, err
	}
	head, base := in.Workspace.Branch(), in.Task.Workspace.Ref
	title, body := Title(in), Body(in)

	existing, _, err := g.Client.PullRequests.List(ctx, owner, repo, &github.PullRequestListOptions{
		Head: owner + ":" + head, Base: base, State: "open", ListOptions: github.ListOptions{PerPage: 5},
	})
	if err != nil {
		return nil, fmt.Errorf("deliver: list PRs: %w", err)
	}
	var pr *github.PullRequest
	if len(existing) > 0 {
		pr, _, err = g.Client.PullRequests.Edit(ctx, owner, repo, existing[0].GetNumber(), &github.PullRequest{Title: &title, Body: &body})
		if err != nil {
			return nil, fmt.Errorf("deliver: update PR #%d: %w", existing[0].GetNumber(), err)
		}
	} else {
		pr, _, err = g.Client.PullRequests.Create(ctx, owner, repo, &github.NewPullRequest{
			Title: &title, Head: &head, Base: &base, Body: &body, Draft: &g.Draft,
			MaintainerCanModify: github.Ptr(true),
		})
		if err != nil {
			var ghErr *github.ErrorResponse
			if errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusUnprocessableEntity {
				return nil, fmt.Errorf("deliver: create PR rejected (branch identical to base or PR exists): %w", err)
			}
			return nil, fmt.Errorf("deliver: create PR: %w", err)
		}
	}
	if g.Logger != nil {
		g.Logger.Info("pull request upserted", "repo", owner+"/"+repo, "number", pr.GetNumber(), "url", pr.GetHTMLURL(), "sha", sha)
	}
	return []string{fmt.Sprintf("pr:%s/%s#%d", owner, repo, pr.GetNumber()), fmt.Sprintf("branch:%s@%s", head, sha)}, nil
}

var (
	_ Adapter = (*GitHubPR)(nil)
	_ Adapter = BranchPush{}
)
