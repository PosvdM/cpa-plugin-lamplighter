# Development

[中文](./development.md)

## Environment

- Go 1.26, matching CPA v8.
- Building the shared library needs cgo and a C compiler for the target. Linux builds run in `golang:1.26-bookworm`, which has the same glibc as the official CPA image (Debian 12).
- Tests need no cgo. Everything under `internal/` is pure Go and can be tested on Windows, macOS or Linux; only `main.go` needs cgo.

## Tests

```bash
go vet ./internal/...
go test ./internal/...
node --check internal/web/page.js
```

Tests use a fake host and local HTTP servers and never send real requests. CI also requires `gofmt -l .` to print nothing.

Coverage:

- active quota parsing and passive header parsing for the three services;
- network exit selection, including that an invalid `proxy_url` never falls back to a direct connection;
- ignition timing, rolling-reset detection, error classes and failure protection;
- candidate model order and model list reading;
- quota alerts, reset reminders, the Bark request format and Did Codex Reset deduplication;
- the engine: account suffixes, the passive skip rule, ignition confirmation, retry with the next model, pause notifications and history sampling;
- plugin RPC: registration, management routes, the page, and that the status contains no secrets.

## Building

With Docker available, build for the CPU architecture of the Docker host:

```bash
sh scripts/build.sh 0.1.0
```

The result is `dist/lamplighter-v0.1.0.so`. The `-v<version>` suffix lets CPA read the plugin ID (`lamplighter`) and version from the file name.

## Releasing

Pushing a `v*` tag runs GitHub Actions (`.github/workflows/build.yml`):

1. run the tests;
2. build in a `golang:1.26-bookworm` container on `ubuntu-latest` (amd64) and `ubuntu-24.04-arm` (arm64);
3. package `lamplighter_<version>_linux_<arch>.zip` with only `lamplighter-v<version>.so` at the root, the format the CPA plugin store expects;
4. create a GitHub Release with the zips and `SHA256SUMS`.

The version comes from the tag (`v0.1.0` gives `0.1.0`) and is set with `-ldflags "-X main.version=..."`; the Management Center and the page show it.

Pushes to `main` only build and upload workflow artifacts, versioned `0.0.0-dev.<first 7 characters of the commit>`.

## Conventions

- Before changing host callbacks, RPC fields or the registration, check `sdk/pluginabi`, `sdk/pluginapi` and `internal/pluginhost` in the CPA source. The CPA version in `go.mod` matches the oldest supported CPA release.
- When changing quota URLs or headers, compare with `src/utils/quota/constants.ts` of the CPA Management Center and update the table in [Architecture](./architecture_EN.md).
- When changing polling, the skip rule, ignition timing or failure protection, update the tests, the READMEs and the architecture docs.
- Every new goroutine in the background loop recovers from panics, and every network request has a timeout.
- Keep the page in separate HTML, CSS and JS files, and insert text into the DOM with `textContent`. Chart colors only show how much quota is left, using the same three variables as the quota bars.
- User-visible behavior, settings and limits go into the READMEs (Chinese and English together); implementation details go into `docs/`.

## Testing against a real CPA

Ignition sends real model requests and starts new 5-hour windows. On a first deployment to a production CPA, set `ignition.enabled` to `false`, check quota, passive data, the page and notifications, and enable ignition afterwards.
