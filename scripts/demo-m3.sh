#!/usr/bin/env bash
# M3 ops-hardening demo (plan Steps 14–18).
#
# Default (offline, no tokens, no Docker): a seeded failing check exhausts the
# retries, the run is dead-lettered and paged, `harness requeue -list` shows
# it, `harness replay` prints its timeline, `harness budget` shows the spend,
# a kind over its daily ceiling is refused with 429, and a requeue with a
# working worker delivers.
#
#   scripts/demo-m3.sh
#
# With DOCKER=1 it instead runs one containerised worker on the internal
# network behind the egress allowlist proxy, which needs the worker image
# (`make worker-image`) and a worker credential in .env; that run spends tokens.
#
#   DOCKER=1 scripts/demo-m3.sh
set -euo pipefail
cd "$(dirname "$0")/.."
if [[ -f .env ]]; then set -a; source .env; set +a; fi

make build >/dev/null
DEMO=$(mktemp -d)
cleanup() {
  if [[ "${DOCKER:-0}" == "1" ]]; then
    docker rm -f harness-demo-egress >/dev/null 2>&1 || true
    docker network rm harness-demo-internal harness-demo-bridge >/dev/null 2>&1 || true
  fi
  rm -rf "$DEMO"
}
trap cleanup EXIT

say() { printf '\n\033[1m== %s\033[0m\n' "$*"; }

# 1. A sandbox origin. check.sh only passes once src/fixed exists, so the
#    first worker (which never creates it) fails every attempt.
SRC="$DEMO/src"; ORIGIN="$DEMO/origin.git"
mkdir -p "$SRC/src"
git -C "$SRC" init -q -b main
printf 'module demo\ngo 1.22\n' > "$SRC/go.mod"
printf 'package a\n' > "$SRC/src/a.go"
cat > "$SRC/check.sh" <<'SH'
#!/bin/sh
[ -f src/fixed ] && exit 0
echo "TestFixed failed: src/fixed missing" >&2
exit 1
SH
chmod +x "$SRC/check.sh"
git -C "$SRC" add -A
git -C "$SRC" -c user.name=demo -c user.email=demo@x commit -qm init
git clone -q --bare "$SRC" "$ORIGIN"

if [[ "${DOCKER:-0}" == "1" ]]; then
  # --- Containerised worker behind the egress allowlist (Step 14) ---
  say "docker mode: worker image + internal network + egress proxy"
  docker image inspect "harness/worker:cli-2.1.243" >/dev/null 2>&1 || {
    echo "worker image missing: run 'make worker-image' first" >&2; exit 1; }
  docker rm -f harness-demo-egress >/dev/null 2>&1 || true
  docker network rm harness-demo-internal harness-demo-bridge >/dev/null 2>&1 || true
  docker network create --internal harness-demo-internal >/dev/null
  docker network create harness-demo-bridge >/dev/null
  # The proxy must be a container joined to both networks: an --internal
  # network has no gateway, so a proxy listening on the host is unreachable
  # from a worker by design. This is the same shape as deploy/docker-compose.yaml.
  docker build -q -t harness-demo-egress:latest -f deploy/Dockerfile . >/dev/null
  cat > "$DEMO/egress.yaml" <<'YAML'
egress:
  addr: ":3128"
  allow: [api.anthropic.com, "*.anthropic.com"]
YAML
  docker run -d --name harness-demo-egress \
    --network harness-demo-internal --network-alias egress \
    -v "$DEMO/egress.yaml:/etc/harness/harness.yaml:ro" \
    harness-demo-egress:latest egress -config /etc/harness/harness.yaml >/dev/null
  docker network connect harness-demo-bridge harness-demo-egress
  for _ in $(seq 1 40); do
    docker logs harness-demo-egress 2>&1 | grep -q "egress proxy listening" && break
    sleep 0.5
  done
  cat > "$DEMO/harness.yaml" <<YAML
data_root: $DEMO/data
worker:
  mode: docker
  image: harness/worker:cli-2.1.243
  docker:
    network: harness-demo-internal
    user: ""
    egress_proxy: http://egress:3128
budgets: { daily_usd: { code_fix: 2, global: 3 } }
observability: { enabled: true, prometheus_addr: "", progress: log }
YAML
  PROMPT="Create an empty file named src/fixed (use Write), then run sh check.sh to confirm it passes."
  JUDGE=false
else
  # --- Offline: fake CLI on the host (Steps 15–18) ---
  say "offline mode: fake CLI, no tokens spent"
  BROKEN="$DEMO/claude-broken"
  cat > "$BROKEN" <<SH
#!/bin/sh
case "\$1" in --version) echo "2.1.243 (Claude Code)"; exit 0;; esac
# Edits the workspace (so diff_sanity passes) but never satisfies check.sh.
printf 'package a\n// tried\n' > src/a.go
SID=""; prev=""
for a in "\$@"; do [ "\$prev" = "--session-id" ] && SID="\$a"; [ "\$prev" = "--resume" ] && SID="\$a"; prev="\$a"; done
mkdir -p "\$CLAUDE_CONFIG_DIR/projects/demo"
echo '{"type":"user"}' >> "\$CLAUDE_CONFIG_DIR/projects/demo/\$SID.jsonl"
cat "$PWD/testdata/events/result_success.json"
SH
  chmod +x "$BROKEN"
  FIXED="$DEMO/claude-fixed"
  sed 's|printf .package a.*$|touch src/fixed|' "$BROKEN" > "$FIXED"
  chmod +x "$FIXED"
  cat > "$DEMO/harness.yaml" <<YAML
data_root: $DEMO/data
worker: { claude_bin: $BROKEN, check_version: false }
budgets: { daily_usd: { code_fix: 2, global: 3 } }
observability: { enabled: true, prometheus_addr: "", progress: log }
YAML
  PROMPT="Create src/fixed so check.sh passes."
  JUDGE=false
fi

cat > "$DEMO/task.json" <<JSON
{
  "kind": "code_fix",
  "title": "M3 demo",
  "prompt": $(printf '%s' "$PROMPT" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))'),
  "workspace": { "type": "git", "repo": "$ORIGIN", "ref": "main" },
  "policy": { "max_turns": 8, "max_cost_usd": 0.5, "timeout_ms": 300000, "model": "haiku", "max_retries": 1,
              "allowed_tools": ["Read","Edit","Write","Glob","Grep","Bash(sh check.sh)","Bash(go test *)"],
              "judge": { "enabled": $JUDGE, "samples": 1, "threshold": 7 } },
  "acceptance": { "commands": ["sh check.sh"], "diff_scope": ["src/**", "*.go"] },
  "requested_by": "scripts/demo-m3.sh"
}
JSON

say "harness doctor"
bin/harness doctor -config "$DEMO/harness.yaml" || true

say "run-once: the worker cannot satisfy the check, so retries are exhausted"
set +e
bin/harness run-once -config "$DEMO/harness.yaml" -task "$DEMO/task.json" > "$DEMO/run.json" 2> "$DEMO/run.log"
set -e
tail -5 "$DEMO/run.log" || true
STATUS=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["status"])' "$DEMO/run.json")
RUN_ID=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["run_id"])' "$DEMO/run.json")
TASK_ID=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["task_id"])' "$DEMO/run.json")
echo "run $RUN_ID is $STATUS"

if [[ "${DOCKER:-0}" == "1" ]]; then
  # A containerised worker that does satisfy the check delivers straight away.
  [[ "$STATUS" == "delivered" ]] || { echo "docker-mode run did not deliver: $STATUS" >&2; exit 1; }
  say "the worker ran in a container on the internal network"
  grep -o '"launcher":"docker"' "$DEMO/run.log" | head -1
  say "egress proxy decisions (the container's only route out)"
  docker logs harness-demo-egress 2>&1 | grep -c "egress allowed" || true
  say "a non-allowlisted host is refused inside the worker container"
  docker run --rm --network harness-demo-internal \
    -e HTTPS_PROXY=http://egress:3128 -e HTTP_PROXY=http://egress:3128 \
    harness/worker:cli-2.1.243 curl -sS -m 20 https://example.com/ 2>&1 | tail -1 || true
  docker logs harness-demo-egress 2>&1 | grep "egress denied" | tail -1
  say "branch pushed to origin"
  git -C "$ORIGIN" log --oneline -n 1 "harness/$TASK_ID"
  say "budget after the run"
  bin/harness budget -config "$DEMO/harness.yaml"
  exit 0
fi

[[ "$STATUS" == "dead" ]] || { echo "expected the run to be dead, got $STATUS" >&2; exit 1; }

say "the page that fired (Step 18)"
grep -o '"msg":"PAGE[^"]*"' "$DEMO/run.log" | head -2
grep -A1 '"msg":"run dead-lettered"' "$DEMO/run.log" | head -2 || true

say "harness requeue -list: the dead-letter queue"
bin/harness requeue -config "$DEMO/harness.yaml" -list

say "harness replay: the run's timeline from the audit log (Step 17)"
bin/harness replay -config "$DEMO/harness.yaml" "$RUN_ID" | tail -12

say "harness budget: spend against the daily ceilings (Step 15)"
bin/harness budget -config "$DEMO/harness.yaml"

say "a kind over its ceiling is refused at intake with 429"
# Drop the ceiling below what has already been spent and submit again. The
# port is picked here because 8080 is often taken on a developer machine.
PORT=${DEMO_PORT:-18099}
sed -i.bak "s/code_fix: 2/code_fix: 0.001/" "$DEMO/harness.yaml"
# Loopback: the API refuses to serve open on a routable address. If .env
# carries HARNESS_API_TOKEN the request below sends it.
printf 'server: { addr: "127.0.0.1:%s" }\n' "$PORT" >> "$DEMO/harness.yaml"
AUTH=()
[[ -n "${HARNESS_API_TOKEN:-}" ]] && AUTH=(-H "authorization: Bearer $HARNESS_API_TOKEN")
bin/harness serve -config "$DEMO/harness.yaml" > "$DEMO/serve.log" 2>&1 &
SERVE_PID=$!
for _ in $(seq 1 40); do curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null && break; sleep 0.25; done
CODE=$(curl -sS -o "$DEMO/refused.json" -w '%{http_code}' -X POST "http://127.0.0.1:$PORT/tasks" \
  -H 'content-type: application/json' "${AUTH[@]}" --data-binary @"$DEMO/task.json")
kill $SERVE_PID 2>/dev/null || true
wait $SERVE_PID 2>/dev/null || true
echo "POST /tasks -> HTTP $CODE"
cat "$DEMO/refused.json"; echo
[[ "$CODE" == "429" ]] || { echo "expected 429, got $CODE" >&2; tail -5 "$DEMO/serve.log"; exit 1; }
mv "$DEMO/harness.yaml.bak" "$DEMO/harness.yaml"

say "harness requeue: a fresh attempt with a worker that satisfies the check"
sed -i.bak "s|claude_bin: $BROKEN|claude_bin: $FIXED|" "$DEMO/harness.yaml"
bin/harness requeue -config "$DEMO/harness.yaml" -note "create the file src/fixed" -process "$RUN_ID" \
  > "$DEMO/requeue.json" 2> "$DEMO/requeue.log"
FINAL=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["status"])' "$DEMO/requeue.json")
FINAL_ID=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["run_id"])' "$DEMO/requeue.json")
FINAL_ATTEMPT=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["attempt"])' "$DEMO/requeue.json")
echo "requeued run $FINAL_ID is $FINAL (attempt $FINAL_ATTEMPT: the counter restarts)"
[[ "$FINAL" == "delivered" ]] || { echo "requeued run did not deliver: $FINAL" >&2; exit 1; }

say "the dead-letter entry is closed and the branch is on origin"
bin/harness requeue -config "$DEMO/harness.yaml" -list
git -C "$ORIGIN" log --oneline -n 1 "harness/$TASK_ID"

say "spend after both runs"
bin/harness budget -config "$DEMO/harness.yaml"

echo
echo "M3 demo complete: dead letter → page → replay → requeue → delivered, with budgets enforced."
