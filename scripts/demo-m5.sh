#!/usr/bin/env bash
# M5 demo (Step 21): planner → three parallel workers → synthesizer.
#
# One change is split across three independent packages. The planner emits a
# subtask list, the harness turns each subtask into its own task that forks the
# planner's session and runs in its own checkout, and once all three are
# terminal the synthesizer merges what they actually delivered into one report.
#
# Without arguments it runs offline against a throwaway bare repo with a fake
# `claude`, so it spends no tokens. With REAL=1 it uses the installed CLI and
# the worker credential from .env.
set -euo pipefail
cd "$(dirname "$0")/.."
if [[ -f .env ]]; then set -a; source .env; set +a; fi

make build >/dev/null
DEMO=$(mktemp -d)
trap 'rm -rf "$DEMO"' EXIT

# 1. A sandbox origin: three independent packages, each missing the same method.
SRC="$DEMO/src"; ORIGIN="$DEMO/origin.git"
mkdir -p "$SRC"
cp -R evals/golden/fixtures/go-fanout/. "$SRC/"
git -C "$SRC" init -q -b main
git -C "$SRC" add -A && git -C "$SRC" -c user.name=demo -c user.email=demo@x commit -qm init
git clone -q --bare "$SRC" "$ORIGIN"

# 2. The worker: the real CLI, or a fake that plays all three roles. The fake
#    tells them apart the way a real worker would — by what its prompt asks
#    for — and writes a transcript so the children have something to fork.
if [[ "${REAL:-0}" == "1" ]]; then
  CLAUDE_BIN=claude
  MODEL=haiku
else
  CLAUDE_BIN="$DEMO/claude"
  cat > "$CLAUDE_BIN" <<'SH'
#!/bin/sh
case "$1" in --version) echo "2.1.243 (Claude Code)"; exit 0;; esac
PROMPT=""; SID=""; EPHEMERAL=0; prev=""
for a in "$@"; do
  [ "$prev" = "-p" ] && PROMPT="$a"
  [ "$prev" = "--session-id" ] && SID="$a"
  [ "$prev" = "--resume" ] && [ -z "$SID" ] && SID="$a"
  [ "$a" = "--no-session-persistence" ] && EPHEMERAL=1
  prev="$a"
done
# Like the CLI: append to the transcript so a fork has a parent to resume.
# The judge is ephemeral and leaves nothing behind.
if [ -n "$SID" ] && [ "$EPHEMERAL" = 0 ]; then
  existing=$(ls "$CLAUDE_CONFIG_DIR"/projects/*/"$SID".jsonl 2>/dev/null | head -1)
  [ -z "$existing" ] && mkdir -p "$CLAUDE_CONFIG_DIR/projects/demo" && existing="$CLAUDE_CONFIG_DIR/projects/demo/$SID.jsonl"
  echo '{"type":"user"}' >> "$existing"
fi
emit() { # emit <structured_output json>
  printf '{"type":"result","subtype":"success","is_error":false,"duration_ms":1500,"duration_api_ms":1400,"num_turns":2,"result":"done","session_id":"%s","total_cost_usd":0.02,"usage":{},"modelUsage":{},"permission_denials":[],"terminal_reason":"completed","stop_reason":"end_turn","structured_output":%s,"uuid":"u"}\n' "$SID" "$1"
}
add_string() { # add_string <dir> <type> <recv> <body-expr>
  cat >> "$1/$1.go" <<GO

// String renders the metric as described in CLAUDE.md.
func ($3 *$2) String() string {
	return $4
}
GO
  cat >> "$1/${1}_test.go" <<GO

func TestString(t *testing.T) {
	m := &$2{Name: "m"}
	if m.String() == "" {
		t.Fatal("String is empty")
	}
}
GO
}
if [ "$EPHEMERAL" = 1 ]; then
  # Layer 2: the judge, on a child's diff.
  emit '{"task_completion":9,"minimal_diff":9,"no_scope_creep":9,"code_quality":8,"gamed_checks":false,"verdict":"pass","reasoning":"The String method matches the documented format and is tested."}'
  exit 0
fi
case "$PROMPT" in
  *"Merge their results"*)
    emit '{"title":"Metrics render themselves","summary":"All three metric packages gained a String method with a test.","delivered":[{"subtask":"counter","artifacts":[],"assessment":"delivered"},{"subtask":"gauge","artifacts":[],"assessment":"delivered"},{"subtask":"timer","artifacts":[],"assessment":"delivered"}],"gaps":[],"merge_order":[],"unknowns":["the branches themselves were not readable from here"]}'
    ;;
  *"counter/counter.go"*)
    add_string counter Counter c 'fmt.Sprintf("%s %d", c.Name, c.Value)'
    sed -i.bak 's|^package counter$|package counter\n\nimport "fmt"|' counter/counter.go && rm -f counter/counter.go.bak
    sed -i.bak 's|^import "testing"$|import "testing"|' counter/counter_test.go && rm -f counter/counter_test.go.bak
    emit 'null'
    ;;
  *"gauge/gauge.go"*)
    add_string gauge Gauge g 'fmt.Sprintf("%s %v", g.Name, g.Value)'
    sed -i.bak 's|^package gauge$|package gauge\n\nimport "fmt"|' gauge/gauge.go && rm -f gauge/gauge.go.bak
    emit 'null'
    ;;
  *"timer/timer.go"*)
    add_string timer Timer t 'fmt.Sprintf("%s %v", t.Name, t.Mean())'
    sed -i.bak 's|^import "time"$|import (\n\t"fmt"\n\t"time"\n)|' timer/timer.go && rm -f timer/timer.go.bak
    emit 'null'
    ;;
  *)
    emit '{"summary":"Give every metric type a String method","subtasks":[{"title":"counter","detail":"Add String() to Counter in counter/counter.go and a test in counter/counter_test.go.","files":["counter/counter.go","counter/counter_test.go"]},{"title":"gauge","detail":"Add String() to Gauge in gauge/gauge.go and a test in gauge/gauge_test.go.","files":["gauge/gauge.go","gauge/gauge_test.go"]},{"title":"timer","detail":"Add String() to Timer in timer/timer.go and a test in timer/timer_test.go.","files":["timer/timer.go","timer/timer_test.go"]}],"risks":["the three packages must not import each other"]}'
    ;;
esac
SH
  chmod +x "$CLAUDE_BIN"
  MODEL=""
fi

cat > "$DEMO/harness.yaml" <<YAML
data_root: $DEMO/data
concurrency: { global: 3, per_kind: { code_fix: 3, code_fix_fanout: 1 } }
worker: { claude_bin: $CLAUDE_BIN, check_version: false }
judge: { model: "$MODEL", max_turns: 4, max_cost_usd: 0.15 }
fanout: { max_children: 4, sweep_minutes: 1 }
env: { GOFLAGS: "-mod=mod", GOCACHE: "${GOCACHE:-$HOME/Library/Caches/go-build}", GOPATH: "${GOPATH:-$HOME/go}" }
YAML
cat > "$DEMO/task.json" <<JSON
{
  "kind": "code_fix_fanout",
  "title": "Give every metric type a String method",
  "prompt": "Add a String() string method to counter.Counter, gauge.Gauge and timer.Timer following the rendering format in CLAUDE.md, each with a test in its own package. The three packages are independent.",
  "workspace": { "type": "git", "repo": "$ORIGIN", "ref": "main" },
  "policy": { "max_turns": 15, "max_cost_usd": 0.6, "model": "$MODEL" },
  "acceptance": { "commands": ["go test ./..."] },
  "requested_by": "scripts/demo-m5.sh"
}
JSON

echo "== planner → children → synthesizer (run-once drives the whole tree)"
bin/harness run-once -config "$DEMO/harness.yaml" -task "$DEMO/task.json" | tee "$DEMO/final.json"
TASK=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["task_id"])' "$DEMO/final.json")
STATUS=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["status"])' "$DEMO/final.json")

echo
echo "== the fan-out tree"
bin/harness tree -config "$DEMO/harness.yaml" "$TASK"

echo
echo "== every child pushed its own branch, one per subtask:"
git -C "$ORIGIN" for-each-ref --format='%(refname:short)' 'refs/heads/harness/*'
BRANCHES=$(git -C "$ORIGIN" for-each-ref --format='%(refname:short)' 'refs/heads/harness/*' | wc -l | tr -d ' ')

echo
echo "== the synthesizer's merge report"
python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); print(json.dumps(d.get("output"), indent=2))' "$DEMO/final.json"

echo
echo "== synthesizer status: $STATUS, child branches: $BRANCHES"
[[ "$STATUS" == "delivered" ]]
[[ "$BRANCHES" == "3" ]]
echo "== M5 fan-out demo OK"
