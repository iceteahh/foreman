# strutil

A tiny Go string helper library. Go 1.22, no third-party dependencies.

## Conventions
- Every exported function has a test in the same package.
- Run `go test ./...` before finishing. It must pass.
- `gofmt` formatting is required.
- Keep changes minimal: this library is used by other services.
