# Contributing

Thanks for your interest in contributing to Agent Factory!

## Guidelines

1. **Test your changes** — run `go build ./...` and `go test ./...` before submitting a PR.
2. **Explain why** — include the reason your change is useful in the PR description. What problem does it solve?

That's it. Keep PRs focused and small when possible.

## Compatibility

Agent Factory is distributed as the `af` application; its exported Go
identifiers are internal cross-package implementation, not a supported import
API, and may be removed without a major-version bump. Compatibility review
instead protects CLI behavior and output, config keys, the public HTTP API
catalog in `docs/http-api.md` (the routes that page lists as not in the catalog
are internal transport and may change), on-disk task and session state, and
behavior that user scripts depend on. This
boundary keeps dead-code cleanup possible; it does not permit user-visible
breakage.
