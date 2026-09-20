# ledger

Go 1.22 money ledger. No third-party dependencies.

## Conventions
- Money is integer cents. Never use a float for an amount.
- Run `go test ./...` before finishing. It must pass.
- Never change a test to make it pass; fix the code the test describes.
