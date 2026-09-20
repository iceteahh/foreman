#!/usr/bin/env bash
# M2 demo: plan → human approval → implement → checks → judge → branch push.
#
# Without arguments it runs offline against a throwaway bare repo with a fake
# `claude` that answers the planning phase with a structured plan and the
# implementation phase with a captured fixture, so it spends no tokens.
# With REAL=1 it uses the installed CLI and the worker credential from .env
# (the judge runs on haiku); approval still happens through `harness review`,
# exactly as an operator would do it without Slack.
set -euo pipefail
cd "$(dirname "$0")/.."
if [[ -f .env ]]; then set -a; source .env; set +a; fi

make build >/dev/null
DEMO=$(mktemp -d)
trap 'rm -rf "$DEMO"' EXIT

# 1. A sandbox origin with a tiny Go module.
SRC="$DEMO/src"; ORIGIN="$DEMO/origin.git"
mkdir -p "$SRC"
git -C "$SRC" init -q -b main
cat > "$SRC/CLAUDE.md" <<'MD'
# demo
Go module. Run `go test ./...` before finishing.
MD
printf 'module demo\ngo 1.22\n' > "$SRC/go.mod"
cat > "$SRC/greet.go" <<'GO'
package demo

func Greet(name string) string { return "Hello, " + name }
GO
git -C "$SRC" add -A && git -C "$SRC" -c user.name=demo -c user.email=demo@x commit -qm init
git clone -q --bare "$SRC" "$ORIGIN"

# 2. The worker: real CLI, or a fake that plans on a new session and implements on --resume.
if [[ "${REAL:-0}" == "1" ]]; then
  CLAUDE_BIN=claude
  JUDGE_MODEL=haiku
else
  CLAUDE_BIN="$DEMO/claude"
  cat > "$CLAUDE_BIN" <<SH
#!/bin/sh
# Fake CLI. Three invocations are told apart by their flags exactly as the
# args builder emits them: the judge is ephemeral (--no-session-persistence),
# the planning phase is a new session with --json-schema, the implementation
# phase is a --resume.
case "\$1" in --version) echo "2.1.243 (Claude Code)"; exit 0;; esac
SID=""; MODE=new; SCHEMA=0; EPHEMERAL=0; prev=""
for a in "\$@"; do
  [ "\$prev" = "--session-id" ] && SID="\$a"
  if [ "\$prev" = "--resume" ]; then SID="\$a"; MODE=resume; fi
  [ "\$a" = "--json-schema" ] && SCHEMA=1
  [ "\$a" = "--no-session-persistence" ] && EPHEMERAL=1
  prev="\$a"
done
if [ "\$EPHEMERAL" = 0 ]; then
  existing=\$(ls "\$CLAUDE_CONFIG_DIR"/projects/*/"\$SID".jsonl 2>/dev/null | head -1)
  [ -z "\$existing" ] && mkdir -p "\$CLAUDE_CONFIG_DIR/projects/demo" && existing="\$CLAUDE_CONFIG_DIR/projects/demo/\$SID.jsonl"
  echo '{"type":"user"}' >> "\$existing"
fi
if [ "\$EPHEMERAL" = 1 ]; then
  # the judge: read-only, ephemeral, rubric schema
  cat <<JSON
{"type":"result","subtype":"success","is_error":false,"duration_ms":1500,"duration_api_ms":1400,"num_turns":1,"result":"","session_id":"\$SID","total_cost_usd":0.01,"usage":{},"modelUsage":{},"permission_denials":[],"terminal_reason":"completed","stop_reason":"end_turn","structured_output":{"task_completion":9,"minimal_diff":9,"no_scope_creep":9,"code_quality":8,"gamed_checks":false,"verdict":"pass","reasoning":"Farewell matches the plan and is tested."},"uuid":"u"}
JSON
elif [ "\$MODE" = new ] && [ "\$SCHEMA" = 1 ]; then
  # phase 1: a plan, no edits
  cat <<JSON
{"type":"result","subtype":"success","is_error":false,"duration_ms":2100,"duration_api_ms":2000,"num_turns":3,"result":"plan","session_id":"\$SID","total_cost_usd":0.03,"usage":{},"modelUsage":{},"permission_denials":[],"terminal_reason":"completed","stop_reason":"tool_use","structured_output":{"summary":"Add Farewell next to Greet and cover it with a test","steps":[{"title":"Add Farewell","files":["greet.go"],"detail":"func Farewell(name string) string { return \\"Goodbye, \\" + name }"},{"title":"Test it","files":["greet_test.go"],"detail":"TestFarewell asserts Farewell(\\"x\\") == \\"Goodbye, x\\""}],"tests":["go test ./..."],"risks":["none"]},"uuid":"u"}
JSON
else
  # phase 2: implement the approved plan
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
  sed -e "s/\\"session_id\\":\\"[0-9a-f-]*\\"/\\"session_id\\":\\"\$SID\\"/" "$PWD/testdata/events/result_resumed.json"
fi
SH
  chmod +x "$CLAUDE_BIN"
  JUDGE_MODEL=""
fi

cat > "$DEMO/harness.yaml" <<YAML
data_root: $DEMO/data
worker: { claude_bin: $CLAUDE_BIN, check_version: false }
judge: { model: "$JUDGE_MODEL", max_turns: 4, max_cost_usd: 0.15 }
env: { GOFLAGS: "-mod=mod", GOCACHE: "${GOCACHE:-$HOME/Library/Caches/go-build}", GOPATH: "${GOPATH:-$HOME/go}" }
YAML
cat > "$DEMO/task.json" <<JSON
{
  "kind": "code_fix_planned",
  "title": "Add Farewell",
  "prompt": "Add a function Farewell(name string) string returning \\"Goodbye, <name>\\" in greet.go and a test for it in greet_test.go. Run go test ./... before finishing.",
  "workspace": { "type": "git", "repo": "$ORIGIN", "ref": "main" },
  "policy": { "max_turns": 15, "max_cost_usd": 0.5, "model": "haiku", "judge": { "enabled": true, "samples": 1, "threshold": 7 } },
  "acceptance": { "commands": ["go test ./..."], "diff_scope": ["*.go", "**/*.go"] },
  "requested_by": "scripts/demo-m2.sh"
}
JSON

echo "== phase 1: plan (run-once stops at needs_review)"
bin/harness run-once -config "$DEMO/harness.yaml" -task "$DEMO/task.json" | tee "$DEMO/run1.json"
STATUS=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["status"])' "$DEMO/run1.json")
RUN1=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["run_id"])' "$DEMO/run1.json")
echo "== phase 1 status: $STATUS ($RUN1)"
[[ "$STATUS" == "needs_review" ]]
echo
echo "== the plan waiting for approval:"
python3 -c 'import json,sys; print(json.dumps(json.load(open(sys.argv[1]))["output"], indent=2))' "$DEMO/run1.json"
echo
echo "== approve → phase 2: implement (checks → judge → deliver)"
bin/harness review -config "$DEMO/harness.yaml" -run "$RUN1" -by demo -comment "Looks right, keep it small" -process approve | tee "$DEMO/run2.json"
STATUS=$(python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print(d["status"])' "$DEMO/run2.json")
echo "== phase 2 status: $STATUS"
echo "== bot branch on origin:"
git -C "$ORIGIN" log --oneline -n 2 "$(git -C "$ORIGIN" for-each-ref --format='%(refname:short)' 'refs/heads/harness/*' | head -1)"
[[ "$STATUS" == "delivered" ]]
