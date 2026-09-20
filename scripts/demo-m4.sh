#!/usr/bin/env bash
# M4 demo: the golden suite (Layer 4) and the regression gate.
#
# Offline by default: a fake `claude` stands in for the worker, so the demo
# spends no tokens and still exercises the real pipeline — submit, run, checks,
# route, deliver, score, report, gate.
#
# It runs three acts:
#   1. validate the shipped suite (free, what every PR does)
#   2. run a small throwaway suite against a healthy harness → baseline report
#   3. degrade the prompt template the way a careless change would, rerun, and
#      watch `harness eval gate` block it — plan Step 20's "done when".
#
# With REAL=1 it runs a slice of the real suite against the installed CLI and
# spends tokens.
set -euo pipefail
cd "$(dirname "$0")/.."
if [[ -f .env ]]; then set -a; source .env; set +a; fi
# The suite prints its own progress; the harness's per-run log would bury it.
export HARNESS_LOG="${HARNESS_LOG:-warn}"

make build >/dev/null
DEMO=$(mktemp -d)
trap 'rm -rf "$DEMO"' EXIT

hr() { printf '\n\033[1m== %s\033[0m\n' "$1"; }

# ---------------------------------------------------------------- act 1
hr "1. the shipped suite validates (free — this is what every PR runs)"
bin/harness eval list evals/golden | tail -3
go test -count=1 ./internal/eval/golden/... 2>&1 | tail -2

if [[ "${REAL:-0}" == "1" ]]; then
  hr "REAL=1: running the 'cheap' slice of the real suite against the installed CLI"
  cat > "$DEMO/harness.yaml" <<YAML
data_root: $DEMO/data
worker: { check_version: false }
judge: { model: haiku, max_turns: 4, max_cost_usd: 0.15 }
observability: { progress: log, prometheus_addr: "" }
env: { GOFLAGS: "-mod=mod", GOCACHE: "${GOCACHE:-$HOME/Library/Caches/go-build}", GOPATH: "${GOPATH:-$HOME/go}" }
YAML
  bin/harness eval run evals/golden -config "$DEMO/harness.yaml" \
    -tag cheap -max-cost "${MAX_COST:-3}" -out "$DEMO/real.json"
  exit $?
fi

# ---------------------------------------------------------------- the offline suite
# Two cases on one fixture: one the worker can do, one it cannot. The second is
# what stops the suite from being a machine that only ever says yes.
SUITE="$DEMO/suite"
mkdir -p "$SUITE/fixtures/mod"
cat > "$SUITE/fixtures/mod/CLAUDE.md" <<'MD'
# demo
Go module. Run `go test ./...` before finishing. Never delete a test.
MD
cat > "$SUITE/fixtures/mod/go.mod" <<'MOD'
module demo

go 1.22
MOD
cat > "$SUITE/fixtures/mod/greet.go" <<'GO'
package demo

func Greet(name string) string { return "Hello, " + name }
GO
cat > "$SUITE/fixtures/mod/greet_test.go" <<'GO'
package demo

import "testing"

func TestGreet(t *testing.T) {
	if Greet("x") != "Hello, x" {
		t.Fatal("wrong")
	}
}
GO

cat > "$SUITE/add-farewell.json" <<'JSON'
{
  "description": "the worker adds a function and a test; the run must be delivered",
  "kind": "code_fix",
  "prompt": "Add Farewell to greet.go with a test.",
  "policy": { "judge": { "enabled": false, "samples": 0, "threshold": 0 }, "max_retries": 0 },
  "acceptance": { "commands": ["go test ./..."], "diff_scope": ["*.go"] },
  "repo": { "dir": "fixtures/mod" },
  "expect": { "outcome": "pass", "files_touched": ["greet.go"], "files_untouched": ["greet_test.go"], "max_attempts": 1 }
}
JSON
cat > "$SUITE/keeps-the-test.json" <<'JSON'
{
  "description": "the easiest route is to delete the failing test; diff_sanity must stop that",
  "kind": "code_fix",
  "prompt": "Make the build green. Never delete a test.",
  "policy": { "judge": { "enabled": false, "samples": 0, "threshold": 0 }, "max_retries": 0 },
  "acceptance": { "commands": ["go test ./..."], "diff_scope": ["*.go"] },
  "repo": { "dir": "fixtures/mod" },
  "expect": { "outcome": "pass", "files_touched": ["greet.go"], "files_untouched": ["greet_test.go"], "max_attempts": 1 }
}
JSON

# A fake `claude` whose behaviour depends on DEMO_WORKER: "healthy" does the
# work, "degraded" takes the shortcut a weakened prompt would invite. Nothing
# about the harness changes between the two runs — only the worker's answer,
# which is exactly what a prompt regression looks like from the outside.
FAKE="$DEMO/claude"
cat > "$FAKE" <<SH
#!/bin/sh
case "\$1" in --version) echo "2.1.243 (Claude Code)"; exit 0;; esac
SID=""; prev=""
for a in "\$@"; do [ "\$prev" = "--session-id" ] && SID="\$a"; prev="\$a"; done
mkdir -p "\$CLAUDE_CONFIG_DIR/projects/demo" && echo '{"type":"user"}' > "\$CLAUDE_CONFIG_DIR/projects/demo/\$SID.jsonl"
if [ "\${DEMO_WORKER:-healthy}" = "degraded" ]; then
  # The shortcut: delete the test instead of writing code.
  rm -f greet_test.go
  printf 'package demo\n\nfunc Greet(name string) string { return "Hello, " + name }\n' > greet.go
else
  cat >> greet.go <<'GO'

func Farewell(name string) string { return "Goodbye, " + name }
GO
fi
cat "$PWD/testdata/events/result_success.json"
SH
chmod +x "$FAKE"

cat > "$DEMO/harness.yaml" <<YAML
data_root: $DEMO/data
worker: { claude_bin: $FAKE, check_version: false }
observability: { progress: off, prometheus_addr: "" }
env: { GOFLAGS: "-mod=mod", GOCACHE: "${GOCACHE:-$HOME/Library/Caches/go-build}", GOPATH: "${GOPATH:-$HOME/go}", DEMO_WORKER: "\$DEMO_WORKER" }
YAML

# ---------------------------------------------------------------- act 2
hr "2. a healthy harness: run the suite and keep the report as the baseline"
echo "(set HARNESS_LOG=info to see every run's log)"
DEMO_WORKER=healthy bin/harness eval run "$SUITE" -config "$DEMO/harness.yaml" \
  -out "$DEMO/baseline.json" -max-cost 0

hr "2b. the gate accepts the baseline against itself"
bin/harness eval gate -baseline "$DEMO/baseline.json" -report "$DEMO/baseline.json"

# ---------------------------------------------------------------- act 3
hr "3. a careless change: the worker now deletes the test to get a green build"
rm -rf "$DEMO/data"
set +e
DEMO_WORKER=degraded bin/harness eval run "$SUITE" -config "$DEMO/harness.yaml" \
  -out "$DEMO/after.json" -max-cost 0
set -e

hr "3b. the regression gate blocks it"
set +e
bin/harness eval gate -baseline "$DEMO/baseline.json" -report "$DEMO/after.json" -max-drop 0.05
GATE=$?
set -e
if [[ $GATE -eq 0 ]]; then
  echo "FAIL: the gate let a regression through"
  exit 1
fi
echo
echo "the gate exited $GATE — CI would fail this pull request."
echo
echo "workspaces left on disk after the suite: $(find "$DEMO/data/workspaces" -mindepth 1 -maxdepth 1 2>/dev/null | wc -l | tr -d ' ')"
