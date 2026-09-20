# checkout-api

Go 1.22 HTTP service for the checkout flow.

## Conventions
- `internal/cache` is an in-memory cache; it is not safe to share across goroutines
  unless the caller holds the mutex.
- Run `go test ./...` before finishing.
- Incidents are recorded under `docs/incidents/`.
