# The harness binary itself (orchestrator, intake, egress proxy). Workers run
# in the separate, heavier worker/ image; this one only needs git and docker.
# Digest-pinned: a tag is mutable, so an unpinned rebuild quietly changes the
# toolchain and the runtime. Re-pin with `docker buildx imagetools inspect`.
FROM golang:1.27-bookworm@sha256:69a7b9788769bec032d238959b61854e9ae87f57be9029ec04e9885fabf99195 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=compose" -o /out/harness ./cmd/harness

FROM debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git curl docker.io \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/harness /usr/local/bin/harness
# The data root is a volume; workspaces and session snapshots live there.
RUN mkdir -p /var/lib/harness
WORKDIR /var/lib/harness
ENTRYPOINT ["harness"]
CMD ["serve"]
