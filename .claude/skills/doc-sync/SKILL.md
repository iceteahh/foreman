---
name: doc-sync
description: Update this repo's documentation to match code that just changed. Use after editing anything under cmd/, internal/, templates/, deploy/, worker/, scripts/, evals/, the Makefile or harness.example.yaml — before reporting the change done, and whenever the user asks to sync, refresh or check the docs. Covers CLAUDE.md, README.md, IMPLEMENTATION_PLAN.md, docs/cli-contract.md and evals/README.md.
---

# doc-sync

Documentation in this repo is load-bearing: `CLAUDE.md` is injected into every session, and
`IMPLEMENTATION_PLAN.md` is how a milestone's state is known. Code that lands without its doc
edit makes both lie. Sync in the same change as the code, never as a follow-up.

## 1. See what actually changed

```
git status --short
git diff --stat HEAD          # unstaged + staged since the last commit
git diff HEAD -- cmd internal templates deploy worker scripts evals Makefile harness.example.yaml
```

If the tree is clean, diff against the last commit that touched code (`git log --oneline -5`).
Work from the diff, not from memory of what you meant to do.

## 2. Map the change to the docs it touches

| What changed | Doc to update |
|---|---|
| New/renamed package under `internal/`, or a moved responsibility | `CLAUDE.md` **Layout**, `README.md` `### Layout` |
| New `cmd/harness` subcommand, or a flag on one | `CLAUDE.md` **Commands**, `README.md` `## Commands`, the command's own usage string |
| New/changed HTTP route or webhook in `intake` | `README.md` `## HTTP API` |
| New task kind, phase template, or `templates/<kind>/` layout | `CLAUDE.md` **Layout** + kinds list, `README.md` `## Task kinds` |
| New rule someone could violate and break prod (a new invariant) | `CLAUDE.md` **Rules**; add it to `README.md` `### Invariants worth knowing before you edit` only if a first-time reader needs it |
| New `make` target, or a target's meaning changed | `CLAUDE.md` **Commands**, `README.md` `## Development` |
| New build tag (`live`, `docker`, `postgres`, …) or test-cost class | `CLAUDE.md` **Layout** (tags paragraph) + **Rules**, `README.md` `## Development` |
| CLI pin bump, or new verified `claude -p` behaviour | `docs/cli-contract.md` (new dated `##` section — never rewrite an old one), the fixture note in `CLAUDE.md` **Layout** |
| New/changed golden case, fixture, scoring or gate rule | `evals/README.md`, `CLAUDE.md` eval **Rules** |
| New deployment mode, compose service, or Helm value | `README.md` `## Deployment modes`, `deploy/` docs, `deploy/harness.k8s.yaml` via `make helm-config` |
| A plan Step finished, or its "done when" changed | `IMPLEMENTATION_PLAN.md` — the `### Step N` line **and** the top `## Status` block |
| New env var or secret | `CLAUDE.md` **Secrets**, `.env.example`, `README.md` `## Credentials` |
| Design decision superseded | `claude-p-agent-harness-design.md` + a note in `IMPLEMENTATION_PLAN.md` §0 |

Nothing in the table matches → no doc edit; say so instead of inventing one.

## 3. Edit rules

- **Smallest true edit.** Change the clause that is now wrong. Do not reflow paragraphs, reorder
  lists, or "improve" prose around it — a noisy doc diff hides the real one.
- **Keep both copies in step.** `CLAUDE.md` Layout/Rules and `README.md` Layout/Invariants
  describe the same system at different depths. Touch one, check the other.
- **Match the register.** These docs state facts in present tense and name the failure a rule
  prevents ("…which makes a just-enqueued job invisible"). A new rule without its consequence is
  half a rule.
- **Dates are absolute.** `2026-09-17`, never "today" or "recently".
- **Status lines are append-only.** Add a new `## Status — …` block above the previous one in
  `IMPLEMENTATION_PLAN.md`; keep the old blocks as the history they are. Same for the dated
  sections in `docs/cli-contract.md`.
- **Don't claim more than the code does.** If a step is built but never run against the real CLI
  or a cluster, say so in the same parenthetical style the plan already uses.

## 4. Verify before reporting done

```
make golden-validate        # free; catches golden-case/fixture drift
make k8s-validate           # free; only if deploy/ or the chart changed
grep -rn "<old name>" --include="*.md" .   # after any rename
```

Then re-read your own doc diff (`git diff -- '*.md'`) and check every command, path, package name
and flag you wrote actually exists in the tree.

## 5. Report

One line per doc touched, saying what claim changed — e.g.
`CLAUDE.md — added the k8s session-store rule; README Layout — new internal/egress entry.`
If you deliberately skipped a doc, say which and why.
