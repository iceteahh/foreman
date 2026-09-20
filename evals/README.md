# Layer 4 — the golden suite

The golden suite evaluates **the harness**, not a single run. Layers 1–3 decide
whether one task was done correctly; Layer 4 asks whether the harness as a whole
still behaves the way it did yesterday, after someone changed a prompt template,
a tool policy, `CLAUDE.md`, or the pinned CLI.

```
evals/
  golden/
    <case>.json          one case: a task, a fixture repo, and the known-good outcome
    fixtures/<name>/     the repository a case runs against
  reports/
    baseline.json        what the gate measures a change against (written by the nightly run)
    <date>.json          each nightly run's report
```

## Running it

```sh
make golden-validate                 # free: every case parses and has its fixture
bin/harness eval list evals/golden   # what the suite covers
bin/harness eval run evals/golden -max-cost 8     # spends tokens
bin/harness eval gate -baseline evals/reports/baseline.json -report evals/reports/2026-09-16.json
make live-golden                     # a few cases against the real CLI (a few cents)
scripts/demo-m4.sh                   # the whole loop offline, no tokens
```

`eval run` stops once `-max-cost` is spent and marks the report
`budget_exhausted`. The gate refuses such a report: a partial pass rate can
neither block nor bless a change.

## Writing a case

```jsonc
{
  "description": "what behaviour this case pins down, and why it matters",
  "kind": "code_fix",
  "prompt": "…what the worker is asked to do…",
  "policy":     { "model": "haiku", "max_cost_usd": 0.5, "max_retries": 1 },
  "acceptance": { "commands": ["go test ./..."], "diff_scope": ["*.go"] },
  "repo":   { "dir": "fixtures/go-strutil", "ref": "main" },
  "expect": {
    "outcome": "pass",                      // pass | fail | needs_review
    "files_touched":   ["strutil.go"],      // globs that must each match a changed file
    "files_untouched": ["*_test.go"],       // globs nothing may match
    "max_attempts": 1,                      // a case that starts needing retries is a regression
    "output_schema": { }                    // for the read-only kinds: what the report must look like
  },
  "tags": ["cheap", "go", "code_fix"]
}
```

The id defaults to the file name and **must stay stable**: the regression gate
compares reports case by case, so renaming a case reads as deleting one and
adding another.

`repo.dir` is a plain directory tree, committed as one initial commit when the
case runs; the checked-in fixture is never itself a git repository. Use
`repo.bundle` instead when a case needs real history — the design's original
shape — and `scripts/golden-bundle.sh` to build one.

A fan-out case (`kind: code_fix_fanout`) drives the whole tree: the planner, one
child per subtask and the synthesizer. The executor follows it to the end —
`outcome` and `output_schema` describe the **synthesizer's** report, and
`max_attempts` counts the parent's runs, not the children's. It is the most
expensive shape in the suite, so there is one of it.

### What makes a case worth having

- It pins **one** behaviour, and the description says which.
- It would actually break if the thing it protects broke. A case that passes no
  matter what the worker does costs money and protects nothing.
- Negative cases matter most. A suite where every case expects `pass` cannot
  catch a harness that delivers everything it is handed, which is the failure
  mode that costs the most to discover in production.

## The regression gate

A pull request that touches `templates/`, `CLAUDE.md`, `evals/`,
`internal/eval/`, the pinned CLI version or the worker image runs the suite and
must clear the gate (`.github/workflows/golden.yml`):

- the pass rate may not fall more than 5 points below the baseline,
- no case that passed in the baseline may start failing (even at an unchanged
  rate — a swapped pass hides a real regression), and
- no baseline case may be missing, so deleting a failing case is not a way to
  go green.

Every other pull request runs only the free validation.

## Feeding human decisions back in

Every review override — approving a run the checks failed, or rejecting one
they passed — is a case the suite does not yet have. `harness eval
import-reviews` turns those decisions into case skeletons:

```sh
bin/harness eval import-reviews -since 720h -dry-run
bin/harness eval import-reviews -since 720h
```

Each skeleton lands with a `skip` note naming the repository and ref whose
fixture still has to be captured. The skip is deliberate: an imported case
scores as skipped, and therefore visible, until someone finishes it.
