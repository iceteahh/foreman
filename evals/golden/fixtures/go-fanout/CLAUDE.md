# metrics

Three small, independent metric types, one per package. Go 1.22, no third-party dependencies.

## Conventions
- Each package is self-contained: `counter`, `gauge` and `timer` never import each other.
- Every exported method has a test in the same package.
- Run `go test ./...` before finishing. It must pass.
- `gofmt` formatting is required.

## Rendering format
A metric renders as `name{labels} value`, with labels sorted by key and joined by a comma:
`requests{method=GET,status=200} 42`. A metric with no labels renders as `name value`.
