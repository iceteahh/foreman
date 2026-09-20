#!/usr/bin/env bash
# M1 walking-skeleton demo: task → queue → worker → checks → (judge with REAL=1) → branch push (or PR).
#
# Without arguments it runs entirely locally against a throwaway bare repo and a
# fake `claude` that replays a captured fixture, so it spends no tokens.
# With REAL=1 it uses the installed CLI and ANTHROPIC_API_KEY against the same
# local repo. With REPO=org/name and GITHUB_TOKEN it opens a draft PR instead.
set -euo pipefail
cd "$(dirname "$0")/.."
if [[ -f .env ]]; then set -a; source .env; set +a; fi

make build >/dev/null
DEMO=$(mktemp -d)
trap 'rm -rf "$DEMO"' EXIT

# 1. A sandbox origin with a failing-free Go module.
SRC="$DEMO/src"; ORIGIN="$DEMO/origin.git"
mkdir -p "$SRC"
git -C "$SRC" init -q -b main
cat > "$SRC/CLAUDE.md" <<'MD'
# demo
Go module. Run `go test ./...` before finishing.
MD
cat > "$SRC/go.mod" <<'MOD'
module demo
go 1.22
MOD
cat > "$SRC/greet.go" <<'GO'
package demo

func Greet(name string) string { return "Hello, " + name }
GO
git -C "$SRC" add -A && git -C "$SRC" -c user.name=demo -c user.email=demo@x commit -qm init
git clone -q --bare "$SRC" "$ORIGIN"

# 2. A worker: the real CLI, or a fake that edits a file and replays a fixture.
if [[ "${REAL:-0}" == "1" ]]; then
  CLAUDE_BIN=claude
  PROMPT="Add a function Farewell(name string) string returning \"Goodbye, <name>\" in greet.go and a test for it in greet_test.go. Run go test ./... before finishing."
else
  CLAUDE_BIN="$DEMO/claude"
  cat > "$CLAUDE_BIN" <<SH
#!/bin/sh
# fake claude: answer --version like the real CLI, otherwise pretend to work
# in the workspace cwd and replay a captured result event
case "\$1" in --version) echo "2.1.243 (Claude Code)"; exit 0;; esac
cat >> greet.go <<'GO'

func Farewell(name string) string { return "Goodbye, " + name }
GO
cat > greet_test.go <<'GO'
package demo

import "testing"

func TestFarewell(t *testing.T) {
	if Farewell("x") != "Goodbye, x" { t.Fatal("wrong") }
}
GO
SID=""; prev=""
for a in "\$@"; do [ "\$prev" = "--session-id" ] && SID="\$a"; prev="\$a"; done
mkdir -p "\$CLAUDE_CONFIG_DIR/projects/demo" && echo '{"type":"user"}' > "\$CLAUDE_CONFIG_DIR/projects/demo/\$SID.jsonl"
cat "$PWD/testdata/events/result_success.json"
SH
  chmod +x "$CLAUDE_BIN"
  PROMPT="Add Farewell to greet.go with a test."
fi
# The fake worker cannot act as the LLM judge (M2), so the offline demo runs
# checks only; REAL=1 judges on haiku too.
JUDGE=true; [[ "${REAL:-0}" == "1" ]] || JUDGE=false

REPO="${REPO:-$ORIGIN}"
cat > "$DEMO/harness.yaml" <<YAML
data_root: $DEMO/data
worker: { claude_bin: $CLAUDE_BIN, check_version: false }   # doctor still reports drift; the live run is the real check
judge: { model: haiku, max_turns: 4, max_cost_usd: 0.15 }
env: { GOFLAGS: "-mod=mod", GOCACHE: "${GOCACHE:-$HOME/Library/Caches/go-build}", GOPATH: "${GOPATH:-$HOME/go}" }
YAML
cat > "$DEMO/task.json" <<JSON
{
  "kind": "code_fix",
  "prompt": $(printf '%s' "$PROMPT" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))'),
  "workspace": { "type": "git", "repo": "$REPO", "ref": "main" },
  "policy": { "max_turns": 15, "max_cost_usd": 0.5, "model": "haiku", "allowed_tools": ["Read","Edit","Write","Glob","Grep","Bash(go test *)","Bash(go build *)"],
              "judge": { "enabled": $JUDGE, "samples": 1, "threshold": 7 } },
  "acceptance": { "commands": ["go test ./..."], "diff_scope": ["*.go", "**/*.go"] },
  "requested_by": "scripts/demo-m1.sh"
}
JSON

echo "== harness doctor"
bin/harness doctor -config "$DEMO/harness.yaml" || true
echo
echo "== run-once"
bin/harness run-once -config "$DEMO/harness.yaml" -task "$DEMO/task.json" | tee "$DEMO/run.json"
echo
STATUS=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["status"])' "$DEMO/run.json")
echo "== run status: $STATUS"
if [[ "$REPO" == "$ORIGIN" ]]; then
  echo "== bot branch on origin:"
  git -C "$ORIGIN" log --oneline -n 2 "$(git -C "$ORIGIN" for-each-ref --format='%(refname:short)' 'refs/heads/harness/*' | head -1)"
fi
[[ "$STATUS" == "delivered" ]]
