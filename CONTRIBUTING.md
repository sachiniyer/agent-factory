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
instead protects whatever this project publishes to its users as a contract.
That is the test; the list is illustration, not its boundary. It covers CLI
behavior and output, config keys, the public HTTP surface wherever the docs
specify it — including the authentication and CORS guarantees documented outside
the route catalogue, and excluding the routes `docs/http-api.md` lists as not
public — durable on-disk state af owns, the documented install path and the
plugin and skill distribution, and behavior that user scripts depend on. This
boundary keeps dead-code cleanup possible; it does not permit user-visible
breakage.
