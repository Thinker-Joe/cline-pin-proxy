# Contributing

Start with the [README](README.md) or [中文 README](README.zh-CN.md) for configuration and behavior. Keep changes focused and include validation that covers the behavior being changed.

## Development setup

The module requires Go 1.23 or later and has no third-party dependencies. CI and container builds currently use Go 1.25. Docker is optional for local development and required for the Linux container checks.

```bash
go build -o cline-pin-proxy ./cmd/cline-pin-proxy
go test ./...
go vet ./...
gofmt -l .
```

`gofmt -l .` must produce no output. For concurrency changes, run the race detector with cgo and a C compiler available:

```bash
go test -race ./...
```

To inspect coverage:

```bash
go test -cover ./...
```

Coverage percentages are not a substitute for tests of failure paths. Useful cases include interrupted responses, configuration changes during a request, encoded paths, existing routing fields, and file persistence failures.

## Linux checks

Development occurs on Windows as well as Linux. Changes to errno handling, file permissions, or path handling require Linux validation before pushing:

```bash
bash scripts/linux-check.sh
```

The script runs formatting, vet, build, and race tests in `golang:1.25-alpine`, installing GCC and musl headers for cgo. An alternative Alpine Go image can be selected with `GO_IMAGE`. Docker must be able to pull the image and install those packages.

This check was added after a Linux compilation failure: `syscall.ENOTSUP` and `syscall.EOPNOTSUPP` have the same value on Linux, so listing both in a switch creates duplicate cases. Windows checks did not catch it.

## Source layout

| Path | Responsibility |
|---|---|
| `cmd/cline-pin-proxy/` | CLI, process startup, logging, and shutdown |
| `internal/config/` | Defaults, validation, environment overrides, reloads, and rule persistence |
| `internal/pin/` | Model matching support and provider-field injection |
| `internal/proxy/` | HTTP routing, forwarding, streaming, and JSON envelope conversion |
| `internal/admin/` | Authenticated configuration and probe endpoints |
| `internal/probe/` | Provider discovery from gateway errors |
| `docs/VERIFICATION.md` | Historical gateway measurements and integration results, in Chinese |
| `docs/CODE_REVIEW.md` | Review findings, fixes, and implementation limits, in Chinese |

The `boundary_test.go` files contain regressions from the code review. `envelope_test.go` covers response conversion and its interaction with streaming.

## Change requirements

- Keep the zero-dependency design. Discuss a new dependency before adding it.
- Preserve the behavior documented in both READMEs, or explain the change and update both versions.
- Keep configuration examples consistent with built-in defaults. File rules replace the entire default list.
- Preserve request-scoped configuration, JSON number precision, streaming flushes, and upstream error signals.
- Keep admin credentials separate from Cline credentials. Do not commit keys, `config.json`, or `data/`.
- Before changing default providers, probe and test each affected model against the real gateway, then record the date, request settings, response metadata, and limitations in `docs/VERIFICATION.md`. A local injection test cannot prove upstream routing.
- Real gateway calls require suitable credentials and may incur charges, including probes when a model ignores the provider filter.

Use file paths and symbols in review notes rather than line numbers that become stale after edits. Keep historical test results clearly dated; do not present them as current service guarantees.

## Changelog and releases

`CHANGELOG.md` is the single source of release notes: the Release workflow extracts the section matching the pushed tag and uses it as the GitHub Release body, and `verify` fails — before any image is pushed — when that section is missing.

To cut a release:

1. Move the finished entries out of `[Unreleased]` into a new `## [x.y.z] - YYYY-MM-DD` section, keeping the category headings. Describe what changed for users rather than restating commits. Dates use the maintainer's local date (UTC+8).
2. Push that change to `main` and wait for CI.
3. Create the tag. The `v` prefix is required: the workflow matches `v*` and derives the `1.0`, `1.0.0`, `v1.0.0`, and `sha-<short>` image tags from it. The binary version comes from the tag name, so no source edit is needed.

When a previous version tag exists, the workflow appends a `Full Changelog` compare link to the release body.

## CI and releases

CI runs on pushes to `main`, pull requests, and manual dispatch. It checks module tidiness, formatting, vet, race tests with coverage, and compilation.

The Release workflow runs on pushes to `main`, `v*` tags, and manual dispatch. Its own `verify` job must pass before publishing images. It builds `linux/amd64` and `linux/arm64` images for GHCR. For version tags, the binary job runs after the image job and attaches binaries for Linux amd64/arm64, macOS amd64/arm64, and Windows amd64 to a GitHub Release.

A successful cross-build or image manifest does not establish runtime compatibility on that architecture. The historical verification record has no arm64 runtime test.
