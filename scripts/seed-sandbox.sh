#!/usr/bin/env bash
# Seed an empty GitHub sandbox repo with the demo Go module so the harness has
# something to work on, then print the commands that run a real task against it.
#
#   scripts/seed-sandbox.sh iceteahh/test-harness
#
# Pushes with GITHUB_TOKEN from .env when set (via GIT_ASKPASS, never on disk or
# in the URL); otherwise with your ambient git/gh credentials.
set -euo pipefail
REPO="${1:?usage: seed-sandbox.sh owner/repo}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
if [[ -f "$HERE/.env" ]]; then set -a; source "$HERE/.env"; set +a; fi
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
if [[ -n "${GITHUB_TOKEN:-}" ]]; then
  cat > "$WORK/askpass.sh" <<'SH'
#!/bin/sh
case "$1" in *sername*) printf 'x-access-token' ;; *) printf '%s' "$GITHUB_TOKEN" ;; esac
SH
  chmod 700 "$WORK/askpass.sh"
  # Stored credential helpers (Keychain, gh) would answer first with another
  # account; ignore the global config so GIT_ASKPASS is the only source.
  export GIT_ASKPASS="$WORK/askpass.sh" GIT_TERMINAL_PROMPT=0 GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
fi
cd "$WORK"
git init -q -b main
cat > CLAUDE.md <<'MD'
# test-harness sandbox

Tiny Go module used to exercise the agent harness.

- Run `go test ./...` before you finish; it must pass.
- Keep changes minimal and add a test for every new function.
- Do not touch files outside the module root.
MD
cat > go.mod <<'MOD'
module github.com/REPO_PLACEHOLDER

go 1.22
MOD
sed -i.bak "s#REPO_PLACEHOLDER#${REPO}#" go.mod && rm go.mod.bak
cat > greet.go <<'GO'
package sandbox

import "strings"

// Greet returns a greeting for name.
func Greet(name string) string { return "Hello, " + strings.TrimSpace(name) }
GO
cat > greet_test.go <<'GO'
package sandbox

import "testing"

func TestGreet(t *testing.T) {
	if got := Greet(" Ada "); got != "Hello, Ada" {
		t.Fatalf("Greet = %q", got)
	}
}
GO
cat > README.md <<'MD'
# test-harness

Sandbox repository for the `claude -p` agent harness. Issues labelled `harness`
become `code_fix` tasks; the harness opens draft PRs from `harness/<task_id>` branches.
MD
git add -A
git -c user.name=harness-seed -c user.email=seed@users.noreply.github.com commit -qm "seed: sandbox Go module"
git remote add origin "https://github.com/${REPO}.git"
git -c credential.helper= push -u origin main
echo
echo "Seeded https://github.com/${REPO}. Next, from the harness repo:"
echo "  REAL=1 REPO=${REPO} ./scripts/demo-m1.sh          # run-once → draft PR"
echo "  gh label create harness -R ${REPO} --color 5319e7  # for webhook-triggered issues"
