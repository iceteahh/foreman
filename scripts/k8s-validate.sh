#!/usr/bin/env bash
# Offline validation of the scaled topology (plan Step 22). No cluster, no
# tokens, no cost:
#
#   1. the Helm chart lints and renders,
#   2. every rendered manifest is valid Kubernetes (kubeconform, schemas only),
#   3. the worker Job the *harness itself* builds is valid Kubernetes,
#   4. the harness.yaml the chart renders is one the binary actually accepts —
#      the check that catches a chart key the loader would reject at rollout.
#
# helm and kubeconform are used from the host when installed, otherwise from
# their images through docker.
set -euo pipefail
cd "$(dirname "$0")/.."

CHART=deploy/k8s
OUT=$(mktemp -d)
trap 'rm -rf "$OUT"' EXIT

helm_run() {
  if command -v helm >/dev/null 2>&1; then
    helm "$@"
  else
    docker run --rm -v "$PWD/$CHART:/chart" -w /chart "${HELM_IMAGE:-alpine/helm:latest}" "$@"
  fi
}

conform() { # reads stdin
  if command -v kubeconform >/dev/null 2>&1; then
    kubeconform -strict -summary -
  else
    docker run --rm -i "${KUBECONFORM_IMAGE:-ghcr.io/yannh/kubeconform:latest}" -strict -summary -
  fi
}

# helm from the host needs the chart path; from the image the chart is the cwd.
chart_arg() { command -v helm >/dev/null 2>&1 && echo "$CHART" || echo "."; }

echo "== helm lint"
helm_run lint "$(chart_arg)"

echo
echo "== helm template → kubeconform"
helm_run template harness "$(chart_arg)" --namespace harness > "$OUT/rendered.yaml"
conform < "$OUT/rendered.yaml"

echo
echo "== the worker Job the harness builds → kubeconform"
go test ./internal/k8s >/dev/null   # refuses a drifted golden manifest
conform < internal/k8s/testdata/worker-job.json

echo
echo "== the rendered harness.yaml is the checked-in one"
python3 - "$OUT/rendered.yaml" "$OUT/harness.yaml" <<'PY'
import re, sys
src = open(sys.argv[1]).read()
m = re.search(r'  harness\.yaml: \|\n((?:    .*\n|\n)+)', src)
if not m:
    sys.exit("the chart rendered no harness.yaml")
open(sys.argv[2], "w").write("\n".join(l[4:] for l in m.group(1).split("\n")))
PY
# The checked-in copy carries a comment header the render does not; compare the
# YAML bodies, which is what the harness actually loads.
if ! diff -u <(grep -v '^#' deploy/harness.k8s.yaml | sed '/^$/d') <(grep -v '^#' "$OUT/harness.yaml" | sed '/^$/d'); then
  echo
  echo "deploy/harness.k8s.yaml is stale; regenerate it with 'make helm-config'" >&2
  exit 1
fi
echo "deploy/harness.k8s.yaml matches the chart"

echo
echo "== the binary accepts it"
go test -run TestShippedConfigsLoad ./internal/config >/dev/null
echo "config loads and validates"

echo
echo "== k8s validation OK"
