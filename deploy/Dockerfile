# The harness binary itself (orchestrator, intake, egress proxy). Workers run
# in the separate, heavier worker/ image; this one only needs git and docker.
FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=compose" -o /out/harness ./cmd/harness

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git curl docker.io \
 && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/harness /usr/local/bin/harness
# The data root is a volume; workspaces and session snapshots live there.
RUN mkdir -p /var/lib/harness
WORKDIR /var/lib/harness
ENTRYPOINT ["harness"]
CMD ["serve"]
