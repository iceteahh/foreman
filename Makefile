GO      ?= go
GOLANGCI_LINT ?= golangci-lint   # LINT is avoided: some shells export it
BIN     := bin/harness
PKGS    := ./...

# Load local secrets (ANTHROPIC_API_KEY, GITHUB_TOKEN, …) if .env exists.
-include .env
export

WORKER_IMAGE ?= harness/worker:cli-$(shell sed -n 's/^const PinnedCLIVersion = "\(.*\)"$$/\1/p' internal/runner/version.go)

.PHONY: all build test lint vet tidy clean doctor live live-golden golden golden-validate worker-image docker-test compose-up compose-down postgres-test postgres-up postgres-down k8s-validate helm-config vulncheck

all: build test

build:
	$(GO) build -o $(BIN) ./cmd/harness

test:
	$(GO) test -race -count=1 $(PKGS)

live: ## live tests spend tokens; need a worker credential
	$(GO) test -tags live -count=1 -run 'Live' ./internal/runner/... ./internal/eval/judge/...

live-golden: ## a few golden cases through the real pipeline (spends a few cents)
	$(GO) test -tags live -count=1 -timeout 45m -run 'TestLiveGoldenSlice' -v ./cmd/harness/

golden-validate: ## free: every golden case parses, has its fixture, and builds a task
	$(GO) test -count=1 ./internal/eval/golden/... ./templates/...
	$(GO) run ./cmd/harness eval list evals/golden

golden: build ## the whole golden suite against the real CLI, under a cost cap (plan Step 19)
	$(BIN) eval run evals/golden -max-cost $(GOLDEN_MAX_COST)

GOLDEN_MAX_COST ?= 8

worker-image: ## build the pinned worker image (plan Step 14)
	docker build -t $(WORKER_IMAGE) --build-arg CLI_VERSION=$(subst harness/worker:cli-,,$(WORKER_IMAGE)) worker/

docker-test: ## container, egress and S3-session tests against a real daemon; no tokens
	$(GO) test -tags docker -count=1 -timeout 20m ./internal/container/... ./internal/session/...

# The harness runs in a container here, so worker containers are siblings and
# the daemon resolves their bind mounts on the host: the stack must know the
# host-side path of the data root.
vulncheck: ## known vulnerabilities in the dependency tree (same check as CI)
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./...

compose-up: ## local ops stack: harness, egress proxy, OTel collector, Prometheus, Grafana, MinIO
	mkdir -p deploy/.compose-data
	HARNESS_HOST_DATA=$(CURDIR)/deploy/.compose-data \
		docker compose -f deploy/docker-compose.yaml up -d --build

compose-down:
	docker compose -f deploy/docker-compose.yaml down -v

# --- scaled topology (plan Step 22) ---------------------------------------
# Postgres store and queue against a real database. No tokens; the container
# is throwaway.
PG_PORT ?= 55433
PG_DSN  ?= postgres://harness:harness@localhost:$(PG_PORT)/harness_test?sslmode=disable

postgres-up:
	docker rm -f harness-pg-test >/dev/null 2>&1 || true
	docker run -d --name harness-pg-test -e POSTGRES_PASSWORD=harness -e POSTGRES_USER=harness \
		-e POSTGRES_DB=harness_test -p $(PG_PORT):5432 postgres:17-alpine >/dev/null
	@for i in $$(seq 1 30); do docker exec harness-pg-test pg_isready -U harness >/dev/null 2>&1 && break; sleep 1; done
	@docker exec harness-pg-test pg_isready -U harness

postgres-down:
	docker rm -f harness-pg-test >/dev/null 2>&1 || true

postgres-test: postgres-up ## build-tagged `postgres` tests against a real database (no tokens)
	HARNESS_TEST_POSTGRES_DSN="$(PG_DSN)" $(GO) test -tags postgres -count=1 ./internal/store/postgres/... ./internal/queue/postgres/...

k8s-validate: ## free: the chart lints, renders, validates, and the binary accepts what it renders
	scripts/k8s-validate.sh

helm-config: ## regenerate deploy/harness.k8s.yaml from the chart
	scripts/helm-config.sh

lint:
	$(GOLANGCI_LINT) run $(PKGS)

vet:
	$(GO) vet $(PKGS)

tidy:
	$(GO) mod tidy

doctor: build
	$(BIN) doctor

clean:
	rm -rf bin
