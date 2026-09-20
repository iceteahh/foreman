#!/usr/bin/env bash
# Build a git bundle fixture for the golden suite from a directory tree.
#
#   scripts/golden-bundle.sh evals/golden/fixtures/go-strutil evals/golden/fixtures/go-strutil.bundle [branch]
#
# Most cases use `repo.dir` instead, because a directory is reviewable in a pull
# request and a bundle is not. Reach for a bundle when the case needs real
# history: a fix that only makes sense against what came before it.
set -euo pipefail
SRC=${1:?usage: golden-bundle.sh <dir> <out.bundle> [branch]}
OUT=${2:?usage: golden-bundle.sh <dir> <out.bundle> [branch]}
BRANCH=${3:-main}

[[ -d "$SRC" ]] || { echo "no such directory: $SRC" >&2; exit 1; }
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# rsync is not assumed; tar keeps modes. The exclusions match golden's own
# copyTree: .git would bring unrelated history, the rest are cache droppings
# that would otherwise land in the fixture's base commit.
tar -C "$SRC" --exclude .git --exclude __pycache__ --exclude node_modules \
    --exclude .pytest_cache --exclude .venv -cf - . | tar -C "$WORK" -xf -

# A fixture must not inherit the operator's git identity, hooks or signing key:
# the suite has to behave the same on every machine.
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
git -C "$WORK" init -q -b "$BRANCH"
git -C "$WORK" -c user.name=golden-fixture -c user.email=golden@harness.invalid \
    -c commit.gpgsign=false add -A
git -C "$WORK" -c user.name=golden-fixture -c user.email=golden@harness.invalid \
    -c commit.gpgsign=false commit -qm "fixture: $(basename "$SRC")"
mkdir -p "$(dirname "$OUT")"
git -C "$WORK" bundle create "$OUT" "$BRANCH"
echo "wrote $OUT ($(git -C "$WORK" rev-parse --short "$BRANCH") on $BRANCH)"
