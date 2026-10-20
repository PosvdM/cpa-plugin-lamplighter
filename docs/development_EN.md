# Development

[中文](./development.md)

## Environment

- Go 1.26, matching CPA v8.
- Building the shared library needs cgo and a C compiler for the target. Linux builds run in `golang:1.26-bookworm`, which has the same glibc as the official CPA image (Debian 12); macOS builds use Xcode's clang and Windows builds MinGW-w64 gcc.
- Tests need no cgo. Everything under `internal/` is pure Go and can be tested on Windows, macOS or Linux; only `main.go` in the root needs cgo.

## Tests

```bash
go vet ./internal/...
go test ./internal/...
node --check internal/web/page.js
```

Tests use a fake host and local HTTP servers and never send real requests. CI runs the tests on Linux, macOS and Windows, and on Linux also requires `gofmt -l .` to print nothing.

Coverage:

- active quota parsing, passive header parsing and window periods for each service (Codex, Claude, Antigravity, Kimi, Grok), window kinds, and the choice of Kimi endpoint;
- network exit selection, including that an invalid `proxy_url` never falls back to a direct connection;
- ignition timing, rolling-reset detection, error classes and failure protection;
- candidate model order and model list reading;
- quota alerts, per-window recovery notifications and reset reminders, the Bark request format, and Did Codex Reset deduplication, filtering and schedule times;
- webhook placeholder escaping, the built-in body, `success_json`, the settings check and hiding secrets in errors, that one successful channel delivers a message, and that no channel turns notifications off while an invalid webhook alone fails and retries;
- the engine: account suffixes and names, the passive skip rule, that passive data adds no windows, queries around resets, ignition confirmation, retry with the next model, recognizing a Kimi alias account, that Kimi and Grok are not ignited, pause notifications, history sampling, the history window filter and bucket lengths, the provider list and newer release in the status, and account plans with the Claude profile read at most once a day;
- version comparison, and reading the latest release when it succeeds, is rate limited, returns an invalid response or has a non-numeric tag;
- the check of `account_weights` in the config, and the plan names and default weights of Codex and Claude;
- plugin RPC: registration, management routes, the page, and that the status contains no secrets;
- the instance lock being exclusive, and reading the plugin directory from the CPA config.

## Building

With Docker available, build for the CPU architecture of the Docker host:

```bash
sh scripts/build.sh 0.1.0
```

The result is `dist/lamplighter-v0.1.0.so`. The `-v<version>` suffix lets CPA read the plugin ID (`lamplighter`) and version from the file name.

## Releasing

Pushing a `v*` tag runs GitHub Actions (`.github/workflows/build.yml`):

1. run the tests;
2. build five platforms with `scripts/ci-build.sh`: Linux amd64 and arm64 in a `golang:1.26-bookworm` container on `ubuntu-latest` and `ubuntu-24.04-arm`; macOS amd64 and arm64 both on `macos-latest` (arm64), with clang cross-compiling amd64; Windows amd64 on `windows-latest`;
3. package each platform as `lamplighter_<version>_<os>_<arch>.zip` with only `lamplighter-v<version>.<so|dylib|dll>` at the root;
4. create a draft GitHub Release with the five zips and `checksums.txt` in `sha256sum` format, with generated notes listing the commits.

A draft is not public, and the CPA plugin store cannot see it. Replace its notes with user-facing release notes, then publish it, so the release goes public with its final notes.

The CPA plugin store installs from the latest release: it looks up the zip and `checksums.txt` by these names and checks the SHA-256 before installing. The store requires all five platforms in every release; a plugin missing any of them is not listed.

The version comes from the tag (`v0.1.0` gives `0.1.0`) and is set with `-ldflags "-X main.version=..."`; the Management Center and the page show it.

Pushes to `main` only build and upload workflow artifacts (the unpackaged libraries), versioned `0.0.0-dev.<first 7 characters of the commit>`.

## Conventions

- Before changing host callbacks, RPC fields or the registration, check `sdk/pluginabi`, `sdk/pluginapi` and `internal/pluginhost` in the CPA source. The CPA version in `go.mod` matches the oldest supported CPA release.
- When raising the CPA version in `go.mod`, update the Codex and Claude user agent constants in `internal/providers` from CPA's `internal/runtime/executor`. The plugin cannot read the constants built into CPA at run time.
- When changing quota URLs or headers, compare with `src/utils/quota/constants.ts` of the CPA Management Center and update the table in [Architecture](./architecture_EN.md).
- When changing polling, the skip rule, ignition timing or failure protection, update the tests, the READMEs and the architecture docs.
- Every new goroutine in the background loop recovers from panics, and every network request has a timeout.
- Keep the page in separate HTML, CSS and JS files, and insert text into the DOM with `textContent`. New or changed page text goes into both the Chinese and the English `I18N` entries in `page.js`; new or changed notification text goes into both columns of `internal/notify/text.go`. A new language adds one dictionary in each place. Chart colors only show how much quota is left, using the same three variables as the quota bars.
- User-visible behavior, settings and limits go into the READMEs (Chinese and English together); implementation details go into `docs/`.

## Testing against a real CPA

Ignition sends real model requests and starts new 5-hour windows. On a first deployment to a production CPA, set `ignition.enabled` to `false`, check quota, passive data, the page and notifications, and enable ignition afterwards.
