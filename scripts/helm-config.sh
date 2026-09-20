#!/usr/bin/env bash
# Regenerate deploy/harness.k8s.yaml from the Helm chart's default values.
# The rendered config is checked in so a unit test can load it: a chart that
# renders a key the harness rejects fails in `go test`, not at rollout.
set -euo pipefail
cd "$(dirname "$0")/.."

CHART=deploy/k8s
OUT=deploy/harness.k8s.yaml
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

if command -v helm >/dev/null 2>&1; then
  helm template harness "$CHART" --namespace harness > "$TMP/rendered.yaml"
else
  docker run --rm -v "$PWD/$CHART:/chart" -w /chart "${HELM_IMAGE:-alpine/helm:latest}" \
    template harness . --namespace harness > "$TMP/rendered.yaml"
fi

python3 - "$TMP/rendered.yaml" "$TMP/body.yaml" <<'PY'
import re, sys
src = open(sys.argv[1]).read()
m = re.search(r'  harness\.yaml: \|\n((?:    .*\n|\n)+)', src)
if not m:
    sys.exit("the chart rendered no harness.yaml")
open(sys.argv[2], "w").write("\n".join(l[4:] for l in m.group(1).split("\n")))
PY

cat > "$OUT" <<'HDR'
# harness.yaml for the Kubernetes deployment, rendered from deploy/k8s with
# the chart's default values:
#
#   helm template harness deploy/k8s --namespace harness
#
# It is checked in so a test can load it (config_test.go): a chart that renders
# a key the loader rejects would otherwise only fail on a cluster, at rollout.
# Regenerate it with `make helm-config` after changing the chart.
#
# Nothing here is a secret. The Postgres DSN comes from $HARNESS_POSTGRES_DSN,
# the worker credential from the Secret named in worker.k8s.credential_secret,
# and the object-storage keys from the orchestrator's own Secret.
HDR
cat "$TMP/body.yaml" >> "$OUT"
echo "wrote $OUT"
