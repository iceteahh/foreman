# CLI contract — verified behaviour

Claude Code CLI **2.1.270**, macOS. Originally verified on 2.1.243 (2026-09-13) with real `claude -p` calls
(model `haiku`, scratch workspaces); **re-verified on 2.1.270 on 2026-09-17** (§7). Fixtures captured from these
runs live in [`testdata/events/`](../testdata/events/). This file is the source of truth for the runner's args
builder, parser, and session store. Re-run the spike and update this file whenever the pinned CLI version changes.

## 1. Session findings

| # | Question | Observed | Consequence for the harness |
|---|---|---|---|
| 1 | Where does a session live? | One transcript at `$CLAUDE_CONFIG_DIR/projects/<cwd-slug>/<session_id>.jsonl` (default `~/.claude`). Side files: `session-env/<session_id>/`, `sessions/<pid>.json` (live-process lock). | The JSONL **is** the session. Nothing else is needed to resume. ~20–30 KB for a two-turn session. |
| 2 | Is `--session-id <uuid>` honoured? | Yes. `result.session_id` equals the minted id; transcript file is named after it. The file is created even when the run fails before any API turn (auth failure). | Harness mints the id and stores it **before** spawning. A crash before `result` still leaves a known, resumable id. |
| 3 | Can an id be reused for a cold retry? | No. `--session-id X` when `X.jsonl` exists → stderr `Error: Session ID X is already in use.`, exit 1, empty stdout. | Cold retry always mints a fresh UUID. |
| 4 | `--resume <id>` from the same cwd | Works. `result.session_id` unchanged; transcript appended in place. Cost ≈ 0.004 USD vs 0.019 USD for a fresh one-turn call (prompt cache hit). | Retry-with-feedback and phase 2 use `--resume` only. |
| 5 | `--resume <id>` from a **different** cwd | Works. The CLI searches every `projects/*/` folder. The transcript stays under its original slug. | Workspace path does **not** need to be stable across attempts. The design's "keyed by workspace path" claim only describes where the file is stored, not how it is looked up. |
| 6 | Transcript removed | `No conversation found with session ID: <id>`, exit 1, empty stdout. Restored (even under a different slug) → resume works and the codeword survives. | **Copy/restore is viable.** Snapshot the JSONL out after every run; restore it before `--resume`. |
| 7 | `--resume X --session-id Y` | `Error: --session-id can only be used with --continue or --resume if --fork-session is also specified.`, exit 1. | Args builder: `--session-id` XOR `--resume`, unless forking. |
| 8 | `--resume X --fork-session` | New id, new file, parent untouched, context retained. With `--session-id Y` added, the fork is created under the harness-minted `Y`. | Fan-out children: `--resume <planner> --fork-session --session-id <child>`. |
| 9 | `--no-session-persistence` | No transcript written; `result.session_id` still present. | Judge runs use it: nothing to clean up, no cross-run leakage. |
| 10 | SIGKILL mid tool call | Transcript exists (init → assistant thinking → assistant tool_use, no `result`). `--resume` afterwards works with the **same** id and the model sees the interrupted request. A stale `sessions/<pid>.json` lock remains and does not block resume. | Timeout and crash both retry **from the same session**. Cold retry only when the transcript is missing or corrupt (#6 error). |
| 11 | Child processes on SIGKILL | The tool's `sleep 8` kept running after `claude` died. | Runner must start `claude` in its own process group (`Setpgid`) and kill the group. Containers make this moot but the local runner needs it. |
| 12 | Fresh `CLAUDE_CONFIG_DIR`, no `ANTHROPIC_API_KEY` | `is_error: true`, **`subtype: "success"`**, `terminal_reason: "api_error"`, `api_error_status: null`, `result: "Not logged in · Please run /login"`, cost 0, exit 1. | Parser keys on `is_error` and `terminal_reason`, never on `subtype` alone. macOS login credentials live in the Keychain, so isolated workers **must** get `ANTHROPIC_API_KEY` via env. |
| 13 | `--max-budget-usd` breach | `subtype: "error_max_budget_usd"`, `terminal_reason: "budget_exhausted"`, `result: null`, exit 1. `total_cost_usd` 0.075 with a 0.05 cap: the cap is checked **after** the turn that crosses it. | Set the CLI cap slightly below the policy ceiling; keep the orchestrator kill as backstop. |
| 14 | Does `CLAUDE_CONFIG_DIR` relocate everything? | Yes: `projects/`, `sessions/`, `session-env/`, `backups/`, `.claude.json` all appear under it. | Use `CLAUDE_CONFIG_DIR=<per-run dir>` instead of overriding `HOME` (a `HOME` override breaks git/ssh config inside the worker). |
| 15 | Environment leakage | `sessions/<pid>.json` recorded `entrypoint: claude-vscode`, `kind: interactive`, a `messagingSocketPath`, and `output_style` inherited from the parent shell. | Env allowlist only (`PATH`, `CLAUDE_CONFIG_DIR`, `ANTHROPIC_API_KEY`, toolchain vars). Never inherit the orchestrator's env. |
| 16 | `--setting-sources project --strict-mcp-config` | `init.mcp_servers` empty, `apiKeySource: none`, but 29 tools, 20 skills, and 6 agent types still listed. | Those are CLI built-ins, not user config. Re-check under `--bare` (open). |
| 17 | `num_turns` on a resumed run | Counts the resumed invocation only (1 after one turn). | `--max-turns` is per invocation, not per session. |
| 18 | Cost visible mid-run? | No. `assistant` events carry `message.usage` (`input_tokens`, `output_tokens`, `cache_creation_input_tokens`, `cache_read_input_tokens`) and `message.model`, but no cost field. `total_cost_usd` appears only on `result`. | `--max-budget-usd` is the primary per-run cap. Any orchestrator mid-run kill must estimate from tokens × a pinned per-model price table; treat it as a backstop. |

## 2. Invocation modes

```
new        claude -p <prompt> --session-id <new-uuid>                       [common flags]
continue   claude -p <prompt> --resume <session_id>                         [common flags]
fork       claude -p <prompt> --resume <parent> --fork-session --session-id <new-uuid>
ephemeral  claude -p <prompt> --session-id <new-uuid> --no-session-persistence   (judge)

common flags:
  --output-format stream-json --verbose --include-partial-messages
  --permission-mode dontAsk --allowedTools <csv>
  --max-turns N --max-budget-usd X [--model m] [--json-schema <schema>]
  --setting-sources project --strict-mcp-config
env: CLAUDE_CONFIG_DIR=<per-run dir>  ANTHROPIC_API_KEY=<worker key>  PATH=…
```

## 3. Result event: fields and how to read them

| Field | Values seen | Use |
|---|---|---|
| `is_error` | bool | Primary failure flag |
| `subtype` | `success`, `error_max_budget_usd` | Secondary; `success` can accompany `is_error: true` (#12) |
| `terminal_reason` | `completed`, `budget_exhausted`, `api_error` | Primary classifier |
| `api_error_status` | `null` (even on auth failure) | Not reliable for auth errors |
| `session_id` | uuid | Confirm equals the id the harness passed |
| `num_turns`, `total_cost_usd`, `duration_ms`, `duration_api_ms`, `usage`, `modelUsage` | | Metrics |
| `permission_denials` | `[]` | Layer-1 check input |
| `result` | string or `null` | Plain-text deliverable; `null` on budget breach |

Also seen: `stop_reason`, `queued_turn_count`, `subagent_stats`, `fast_mode_state`, `ttft_ms`, `uuid`. Keep unknown fields as raw JSON.

`assistant` events: `{type, session_id, uuid, request_id, timestamp, parent_tool_use_id, message:{id, model, role, content[], stop_reason, usage}}`. `system` subtypes seen: `init`, `thinking_tokens`. No `permission_denied` subtype exists; denials are on `result.permission_denials` only.

Stream order observed: `system/init` → `rate_limit_event` → `system/thinking_tokens`… → `assistant`(×N) → `result`.
Fatal CLI argument or session errors produce **empty stdout** and a one-line stderr message (fixtures `stderr_*.txt`).

## 4. Still open

| Item | Blocked on |
|---|---|
| `--bare --add-dir <ws>` still loads workspace `CLAUDE.md`? Tool/skill list under `--bare`? | An `ANTHROPIC_API_KEY` (`--bare` disables Keychain auth) |
| stderr / event content on HTTP 429 | Same. **Assumption in the code until verified:** the runner and the progress emitter treat a `system/api_retry` with `error_status` 429 or 529 as rate limiting (exponential backoff + concurrency shed), reusing the field names from the verified 401 shape. A `rate_limit_event` whose `rate_limit_info.status` is anything but `allowed` is treated the same way; every captured fixture shows `allowed`. |
| ~~event content on an invalid key~~ | **Verified 2026-09-13** (fixture `stream_auth_failed_401.ndjson`): `system/status {status: requesting}` → `system/api_retry {attempt 1..10, max_retries: 10, retry_delay_ms, error_status: 401, error: authentication_failed}` (~3 min total) → synthetic `assistant` (`model: "<synthetic>"`) → `result` with `is_error: true`, `terminal_reason: api_error`, cost 0. The runner kills the group on the first 401/403 retry. |
| ~~Fixtures: `--max-turns` exhaustion, permission denial, `--json-schema` structured output~~ | **Captured 2026-09-15** on CLI 2.1.270 (§5). |
| ~~`CLAUDE_CODE_OAUTH_TOKEN` as an alternative worker credential~~ | **Verified 2026-09-13:** works with an isolated `CLAUDE_CONFIG_DIR` (`is_error: false`, cost 0.016). The same token in `ANTHROPIC_API_KEY` → HTTP 401. |

## 5. Fixtures captured 2026-09-15 (CLI 2.1.270, haiku, `--no-session-persistence`)

| # | Question | Observed | Consequence for the harness |
|---|---|---|---|
| 19 | `--json-schema` structured output | `result.structured_output` is the **object** (not a string); `result.result` carries the same JSON as text; `stop_reason: "tool_use"`, `subtype: "success"`, `is_error: false`, `terminal_reason: "completed"`. 3 turns, 0.048 USD. Fixtures `stream_structured_output.ndjson`, `result_structured_output.json`. | `SchemaValidation` validates `structured_output` directly; the judge accepts `result.result` as a fallback when the object is absent. |
| 20 | `--max-turns 1` exhaustion | `is_error: true`, `subtype: "error_max_turns"`, `terminal_reason: "max_turns"`, `errors: ["Reached maximum number of turns (1)"]`, `result: null`, **`num_turns: 2`** (one more than the cap), exit 1. Fixtures `stream_max_turns.ndjson`, `result_max_turns.json`. | `Classify` keys on `terminal_reason`; `TurnExhaustion` uses `num_turns >= max_turns`. |
| 21 | Permission denial under `--permission-mode dontAsk --allowedTools Read` | `Write` and `Bash(rm -f hello.txt)` both appear in `result.permission_denials[]` as `{tool_name, tool_use_id, tool_input}`; the run still ends `is_error: false`, `terminal_reason: "completed"`, exit 0, and the model reports "DENIED". Fixtures `stream_permission_denial.ndjson`, `result_permission_denial.json`. | Denials are a Layer 1 signal only (`PermissionDenials` check with `policy.critical_tools`); process health says nothing about them. |
| 22 | `Bash(echo hi)` with only `Read` allowed | **Ran anyway** (`permission_denials: []`, output `hi`): the CLI auto-approves its built-in list of read-only shell commands regardless of `--allowedTools`. Fixtures `stream_bash_safe_command_allowed.ndjson`, `result_bash_safe_command_allowed.json`. | Do not rely on `--allowedTools` to block harmless shell reads; the allowlist blocks mutating commands (#21). Container isolation (Step 14) remains the real boundary. |

## 6. Container findings 2026-09-15 (plan Step 14, image `harness/worker:cli-2.1.243`)

Verified with `make docker-test` against Docker 29.7.2 on macOS (no tokens spent) and one real
`claude -p` run through `DOCKER=1 scripts/demo-m3.sh`.

| # | Question | Observed | Consequence for the harness |
|---|---|---|---|
| 23 | Does the CLI run unprivileged in a container? | Yes, as uid 1000 with `--cap-drop ALL --security-opt no-new-privileges`. The bind-mounted workspace stays writable from inside, so the worker edits the host checkout directly and the runner snapshots the transcript from the host. | One container per run; no `docker cp`, no volume for the session store. |
| 24 | `HOME` inside the image | `--user <uid>` with no passwd entry leaves `HOME=/`, which git and npm cannot write to. | The container gets `HOME=/home/worker` explicitly (world-writable in the image so any uid works). This does not violate the "never override HOME" rule, which is about the host worker's git/ssh config. |
| 25 | Is `docker kill` immediately effective after `docker run`? | **No.** The client creates the container asynchronously, so a kill issued milliseconds after start fails with `Error response from daemon: No such container: …` and the container then runs to completion. | `container.Stop` retries the kill until docker confirms the container existed or the client exits. Both the worker and the acceptance-command path use it; regression tests cover both. |
| 26 | Egress from an `--internal` network | No route out at all: `curl https://example.com` fails, and a proxy listening on the *host* is unreachable (there is no gateway). | The egress proxy must be a container joined to both the internal network and a routable one. `deploy/docker-compose.yaml` and `scripts/demo-m3.sh` both do this. |
| 27 | CONNECT allowlist enforcement | With `HTTPS_PROXY=http://egress:3128`, an allowlisted host tunnels through and `example.com` fails with `curl: (56) CONNECT tunnel failed, response 403`; the proxy logs both decisions. A real worker run made 5 allowed tunnels. | `internal/egress` is the network boundary; `--allowedTools` is not (cli-contract #22). |
| 28 | Version pinning inside the image | `docker run --rm --network none --entrypoint claude <image> --version` prints the baked version, so drift is caught before any run. | `harness doctor` and startup check the image in docker mode and the host binary in local mode. |

---

## 7. Re-verification on 2.1.270 — 2026-09-17 (pin bumped from 2.1.243)

Every session finding in §1 that the harness depends on was re-run against 2.1.270 on macOS with `haiku` and an
isolated `CLAUDE_CONFIG_DIR`. Total spend ≈ $0.09.

| # | Check | 2.1.270 |
|---|---|---|
| 2 | `--session-id <uuid>` honoured, transcript named after it | unchanged |
| 3 | Reusing an id | unchanged: `Error: Session ID <id> is already in use.`, exit 1 — **but see the nuance below** |
| 4 | `--resume` from the same cwd | unchanged; id preserved, `num_turns` 1, cost $0.0046 vs $0.035 for the fresh call (cache hit) |
| 5 | `--resume` from a different cwd | unchanged; the codeword survived, the transcript stayed under its original slug |
| 6 | Transcript missing | unchanged: `No conversation found with session ID: <id>`, exit 1 |
| 7 | `--resume X --session-id Y` | unchanged: refused with the same message, exit 1 |
| 8 | `--resume X --fork-session --session-id Y` | unchanged: fork written under the harness-minted `Y`, context retained, parent untouched |
| 9 | `--no-session-persistence` | unchanged: no transcript, `result.session_id` still present |
| 12 | No credential, fresh config dir | unchanged: `is_error: true`, `subtype: "success"`, `terminal_reason: "api_error"`, `api_error_status: null`, `result: "Not logged in · Please run /login"`, cost 0, exit 1 |
| 13 | `--max-budget-usd` breach | unchanged: `subtype: "error_max_budget_usd"`, `terminal_reason: "budget_exhausted"`, `result: null`, exit 1. Overshoot is larger here — $0.029 against a $0.005 cap — which is the same "checked after the turn that crosses it" behaviour at a smaller cap |
| 17 | `num_turns` on a resumed run | unchanged: counts the invocation, not the session |

Three differences, none of which change the harness's behaviour:

1. **The "already in use" check is scoped to the cwd's slug, not global.** A transcript for the same id under a
   *different* `projects/<slug>/` folder does not trigger it — the id is silently reused and appended to a new
   file. Resume (#5) still searches every slug. This asymmetry was not recorded before. It costs the harness
   nothing, because ids are minted per run and never reused, but it means `result.SessionIDInUse` is a backstop
   that only fires when the retry happens in the same workspace path.
2. **New `result` fields:** `fast_mode_state`, `fast_mode_disabled_reason`, `subagent_stats`, `queued_turn_count`,
   `result_index`. The decoder unmarshals into a struct without `DisallowUnknownFields`, so they are ignored. None
   of them carries information the harness currently needs.
3. **A new stderr warning when stdin is an unredirected terminal:** `Warning: no stdin data received in 3s,
   proceeding without it.` — and a 3-second delay with it. The runner never hits this: `exec.Cmd` with a nil
   `Stdin` connects the child to `/dev/null`. It only appears when a human runs the CLI by hand.

Not re-run (already captured on 2.1.270 on 2026-09-15, §5): `--max-turns` exhaustion, permission denial,
`--json-schema` structured output. Not re-run on 2.1.270 at all: the container findings in §6 — the worker image
still has to be rebuilt (`make worker-image`) and `make docker-test` re-run before docker mode is trusted on the
new pin.
