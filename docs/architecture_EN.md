# Architecture

[中文](./architecture.md)

Lamplighter is a CPA native plugin written in Go and built with `-buildmode=c-shared`. CPA loads the shared library and calls `cliproxy_plugin_init`; after that, all traffic is RPC in JSON envelopes: CPA calls plugin methods, and the plugin calls CPA through host callbacks.

## Modules

| Path | Responsibility |
| --- | --- |
| `main.go` | cgo entry point: exports the C ABI functions, forwards RPC, implements host calls, computes the default data directory |
| `internal/plugin` | Plugin RPC methods: register, reconfigure, quiesce, management API, `usage.handle` |
| `internal/engine` | Background loop: active queries, passive data, ignition, notifications, history and status data |
| `internal/host` | Go wrapper of the host callbacks and timeout handling for `host.http.do` |
| `internal/config` | Plugin config parsing, defaults and ranges; reads the CPA config to see whether the plugin is enabled |
| `internal/quota` | Quota requests and parsing for the three services; passive quota header parsing |
| `internal/egress` | Picks the network exit of quota requests |
| `internal/models` | Reads the CPA model list and orders ignition candidates |
| `internal/ignite` | Ignition timing, rolling-reset detection, error classification and failure protection |
| `internal/notify` | Bark delivery, quota alerts, Did Codex Reset forwarding |
| `internal/store` | `state.json`, quota history and the instance lock |
| `internal/web` | Management page; HTML, CSS and JS are separate files combined into one document at runtime |

## Interface with CPA

`plugin.register` returns `schema_version` 6 and declares two capabilities:

- `management_api`: registers the management routes and the page;
- `usage_plugin`: receives one usage record per request, including the upstream response headers.

Management routes (require the CPA management key):

| Method and path | Purpose |
| --- | --- |
| `GET /v0/management/lamplighter/status` | Status page data; secrets in the config only show whether they are set |
| `GET /v0/management/lamplighter/history?range=1h\|2h\|5h\|24h\|7d\|14d\|35d` | Quota history and ignition events, `24h` by default |
| `POST /v0/management/lamplighter/refresh` | Query now; `{"auth_index": "..."}` limits it to one account |
| `POST /v0/management/lamplighter/ignite` | Ignite now, `{"target": "<group key>"}` |
| `POST /v0/management/lamplighter/test-bark` | Send a test notification |

The page is the resource `GET /v0/resource/plugins/lamplighter/page` with the menu label `Lamplighter`. The resource itself needs no authentication; its data requests carry the management key. The page reads the key from `cli-proxy-auth` in `localStorage`, where the Management Center stores it with a reversible obfuscation derived from the host and user agent; otherwise it asks for the key and keeps it in `sessionStorage` only. Settings are saved through CPA's `PATCH /v0/management/plugins/lamplighter/config`, which merges top-level keys only, so the page sends `ignition`, `providers` and `codex_reset_updates` as complete objects.

The page looks like the Management Center: its CSS variables reuse the names and values of the Management Center's `src/styles/themes.scss` (light, white and dark themes), quota bars and the chart are colored by the first two notification thresholds (by default green above 50% remaining, amber above 20%, red at 20% or less), matching the 🟡 and 🔴 notification titles, and telemetry numbers use a monospace font. The page is same-origin with the Management Center, so it reads `data-theme` from the parent page's root element and watches it for changes; opened on its own, it falls back to `cli-proxy-theme` that the Management Center stores in `localStorage`. When the Management Center changes its colors, update the variables in `internal/web/page.css`.

The page does all data handling for the quota chart:

- **Missing data**: when two samples are more than 3.5 poll intervals (or history buckets) apart, the gap is drawn with diagonal hatching instead of holding the previous value.
- **Pooled accounts**: accounts with the same service and `source_label` share one row showing the mean of their remaining percentages. With equal plans this is the share of the total quota left; the API only reports percentages, so the mean cannot be weighted by quota size. The detail chart also shows the range from the lowest to the highest account.
- **Order**: rows follow `SERVICE_ORDER` (Claude, ChatGPT, Gemini, Fable, Claude / GPT) by default, with other quotas after them in API order; they can also be sorted by current remaining, lowest first.
- **Resets**: a rise of 5 points or more between two samples is a reset. The line breaks there, the next piece starts at the value after the reset, and the reset time is labeled; two rises one sample apart count as one reset. One account resetting raises the pooled total by only its share, so the pooled line does not break when the rise is under 5 points.
- **Smoothing**: within one window a quota only falls, but active queries and response headers can differ by a point and round differently, so samples briefly step back up. Before drawing, each piece is fitted to the closest non-increasing sequence by pooling adjacent violators into their mean; otherwise every step back would leave a small notch in the line. Tooltips still show the raw samples. Lines use monotone cubic interpolation (Fritsch-Carlson), which never passes beyond the fitted values. Quotas report whole percents, so steps of 2 points or less are joined as a steady decline; a plateau followed by a larger drop stays flat and then bends at the drop.

Host callbacks in use:

| Callback | Purpose |
| --- | --- |
| `host.auth.list` | Credential list with `auth_index`, runtime ID, email and disabled state |
| `host.auth.get` | Credential file JSON: access token, Codex account ID, Antigravity `project_id`, `proxy_url` |
| `host.model.execute` | Ignition, with `auth_id` and `forced_provider` |
| `host.http.operation_open` / `host.http.do` / `host.http.cancel` | Quota requests of credentials without their own proxy |
| `host.log` | Writes to the CPA log |

Host callbacks are bound to the plugin instance, not to one RPC, so the background loop can call them at any time.

## Background loop

`engine.Engine` does all network work on one goroutine, so queries, ignition and notifications never modify the state concurrently:

1. It starts after plugin registration and waits 20 seconds so that CPA can load credentials and executors.
2. It opens the data directory and takes `instance.lock`.
3. Every 30 seconds it reads CPA's `config.yaml` and checks `plugins.enabled` and the plugin's own `enabled`. CPA does not notify a plugin that it disabled, so the plugin checks itself and does nothing while disabled. An unreadable config file counts as enabled.
4. It applies passive data, polls Did Codex Reset, runs due active queries and due ignitions, prunes history and saves the state.
5. It computes the next wake-up time and until then waits for passive data, management actions or config changes.

`usage.handle` and the management routes never make network requests themselves. The former parses headers and queues them (dropping a sample when the queue is full is harmless); the latter hands the action to the loop and waits up to 3 minutes for the result.

If the loop panics, `supervise` logs it and restarts the loop after 30 seconds. An unrecovered panic anywhere in the plugin ends the whole CPA process, so every goroutine and every RPC method recovers.

### Lifecycle

- `plugin.register` and `plugin.reconfigure` parse and apply the config. A config that cannot be parsed is not applied; the status page shows the error and the previous config stays in effect.
- When a new library version loads, CPA calls `plugin.quiesce` on the old instance but does not unload it. After `plugin.quiesce` or `plugin.shutdown` the plugin stops its loop and never starts it again.
- `instance.lock` is an `flock` lock in the data directory that keeps only one instance querying, igniting and notifying at a time. A new instance that cannot take the lock retries every 10 seconds.

## Reading quota

### Active queries

| Service | Request |
| --- | --- |
| Codex | `GET https://chatgpt.com/backend-api/wham/usage` with `Chatgpt-Account-Id` |
| Claude | `GET https://api.anthropic.com/api/oauth/usage`, `anthropic-beta: oauth-2025-04-20` |
| Antigravity | `POST .../v1internal:retrieveUserQuotaSummary`, three URLs in order, body `{"project": "<project_id>"}` |

The headers match the CPA Management Center quota page; the constants are in `internal/quota/active.go`. The Codex user agent equals the one of CPA's Codex executor, and the Claude user agent equals CPA's default Claude Code fingerprint.

The token comes from the credential file. CPA refreshes tokens in the background and writes them back; the plugin never refreshes them. On HTTP 401 the plugin skips the round and waits for CPA's refresh.

Queries align to absolute time boundaries (`alignedAfter`): the default 300-second interval lands on minutes `:00 / :05 / ... / :55`. "Last run plus interval" is not used because it drifts with the start time.

### Network exit

`internal/egress` sends quota requests through the same exit as CPA's model requests:

- With a `proxy_url` on the credential, the connection is built with CPA's `sdk/proxyutil`, as CPA's executors do; `direct` / `none` means a direct connection. A value that cannot be parsed returns an error instead of falling back to another exit.
- Without a `proxy_url`, the request goes through `host.http.do`, where CPA uses the global `proxy-url`, then environment proxies, then a direct connection. `host.http.do` has no timeout, so the plugin opens an operation with `host.http.operation_open` and cancels it with `host.http.cancel` when the timeout expires.

CPA's `/api-call` (used by the Management Center quota page) ignores environment proxies, while model requests do not. The plugin follows the model request rules.

### Passive data

CPA passes the upstream response headers in `ResponseHeaders` of the usage record:

| Service | Headers | Windows |
| --- | --- | --- |
| Claude | `Anthropic-Ratelimit-Unified-5h-Utilization` / `-5h-Reset`, same for `-7d-` | 5-hour and 7-day; the value is the used fraction, for example `0.2` |
| Codex | `X-Codex-Primary-Used-Percent`, `-Reset-At`, `-Reset-After-Seconds`, `-Window-Minutes`, same for `Secondary` | Classified by window length: 300 minutes is 5-hour, 10080 is 7-day |

Passive data only updates the windows present in the headers; an active result replaces the whole group.

Skip rule (`shouldSkipActive`): a scheduled query is skipped when passive data arrived within `passive_skip_seconds` before it and the last active query is less than `passive_skip_max_minutes` old. Because only a short span before the slot counts, the longest gap between two data points is the interval plus `passive_skip_seconds`. Manual refreshes never skip.

## Ignition

### Timing

`ignite.Schedule.DueAt` returns the next ignition time:

1. Before `start_hour`: wait for `start_hour`.
2. After `end_hour + end_grace_minutes`: wait for `start_hour` the next day.
3. The 5-hour window is a rolling placeholder: now (subject to hold and retry times).
4. A future reset: `reset + grace_seconds`, or the next day's `start_hour` when that falls outside today's window.
5. A past or unknown reset: now (subject to hold and retry times).

**Rolling placeholder**: some services report an unstarted window with a reset "5 hours from now" that moves forward with every query. `ignite.Observe` detects it from two consecutive active observations: the reset is about 5 hours ahead (120-second tolerance), and it moved by about the elapsed time (2% of the elapsed time, between 3 and 10 seconds). Passive data comes from a real request, which itself starts the window, so a passive observation is never rolling. A fixed future reset clears the failure state.

### Request

Ignition uses `host.model.execute` with `auth_id` set to the credential's runtime ID and `forced_provider` set to the service. Bodies are built by `ignite.BuildRequest`:

| Service | Entry protocol | Notable fields |
| --- | --- | --- |
| Codex | `openai-response` | `reasoning.effort: none`, `tools: []`, `max_output_tokens: 4` |
| Claude | `claude` | `max_tokens: 4` |
| Antigravity | `openai` | `max_tokens: 4`, `temperature: 0`, `reasoning_effort: none` |

### Model selection

CPA has no host callback that lists the models of one credential, so the plugin reads `GET /v1/models` with `models_api_key`. That list is the union of all credentials, deduplicated by model ID, and `owned_by` can name another service when two services serve the same ID. Candidates are therefore picked by name (`models.Candidates`):

- Codex: models containing `luna`, newest first, then other `gpt-` models; image models and `gpt-oss` excluded.
- Claude: non-thinking Haiku models starting with `claude-`, newest first, then Sonnet.
- Antigravity Gemini group: Flash newest first, then other non-Pro models, Pro last; image models excluded.
- Antigravity Claude / GPT group: Haiku, Sonnet, Opus (all non-thinking), GPT-OSS, then other non-thinking models.

"Newest first" compares the numeric segments of model names, so new releases need no code change. The last successful model goes first. A configured `model` or `models` entry is the only candidate and must be in the list.

When the pinned credential rejects a candidate, CPA answers locally with `auth_not_found` (the credential does not serve the model) or `unknown provider for model` (no credential does) without calling the provider. The plugin then tries the next candidate, up to 6. If every candidate is rejected, the error counts as high risk. While the model list cannot be read, the configured or last successful model is used.

### Confirmation

After a 2xx response, the plugin confirms that the 5-hour window has a fixed future reset observed after the request:

1. Claude responses carry the new reset in their headers, so it is confirmed at once.
2. Codex quota arrives through the usage record of the same request; passive data keeps being applied while waiting.
3. Checks run after 5, 10, 15, 30 and 15 seconds, each preceded by one active query of the credential.

If no check confirms, the result is `ErrNotConfirmed`.

### Error classes and protection

`ignite.Classify`:

| Class | Condition | Handling |
| --- | --- | --- |
| Next model | `auth_not_found`, `unknown provider for model` | Try the next candidate |
| High risk | `ErrNotConfirmed`, `ErrNoModel`, HTTP 400/401/403/404/409/422/429, `not found`, `unsupported` and similar | Pause until the next `start_hour` |
| Transient | CPA local cooldown (`are cooling down`), `auth_unavailable`, anything else | Retry after `failure_retry_seconds × multiplier^(n-1)`; pause until the next day at failure `max_transient_failures` |

A pause sends one Bark notification (`circuit_notified_until_epoch` prevents repeats). A success or a fixed future reset clears the failure state.

## Notifications

`notify.Alerts.ProcessGroup` compares each window's remaining quota with the level last notified:

- Levels are normal, notice (≤50%), low (≤20%), critical (≤10%) and exhausted (≤0.01%). A worse level is notified; critical and exhausted use `timeSensitive`.
- A better level is notified only with `notify_recovery`; otherwise the baseline follows silently, so that the next drop alerts again.
- With `notify_reset_reminders`, every window gets one reminder 1 hour before its reset and 7-day windows one more 1 day before.
- Two reset times within 10 seconds belong to the same cycle.
- A window seen for the first time only records its baseline.
- A failed delivery leaves the notified level unchanged, so the next check retries.

A Bark request is `GET {bark_url}/{title}/{body}?group&level&icon&url`, with title and body percent-encoded except RFC 3986 unreserved characters. A JSON `code` other than 200 is a failure.

Did Codex Reset is read every `poll_seconds` seconds (at least 300, aligned to time boundaries), 10 latest records at a time. Records are deduplicated by ID, except `manual:` records, whose IDs can change and which are deduplicated by content; up to 100 seen keys are kept. The first run marks existing records as seen and notifies only the pending schedule when `notify_current_pending` is on.

## Data

The data directory defaults to `data/lamplighter` in the plugin directory. `main.go` finds the library path with `dladdr`; when the library sits in `<plugins>/<goos>/<goarch>/`, those two levels are removed.

| File | Content |
| --- | --- |
| `state.json` | `groups`: notification baselines per window; `scheduler`: ignition state per group; `codex_reset_updates`: seen records. Written to a temporary file that replaces the old one |
| `history/YYYY-MM-DD.jsonl` | Quota samples and events, one file per UTC day |
| `instance.lock` | Instance lock |

Each history line is one JSON object. A sample is `{"k":"s","t":seconds,"g":group,"w":window,"r":remaining,"x":reset,"s":"active|passive"}`, an event is `{"k":"e","t":seconds,"g":group,"e":type,"v":level,"m":text}`. Active samples are always written. Passive samples are written only when the value or reset changes, at most once per minute per window; a newer value inside that minute waits in `pendingSamp`. The history API keeps the last sample per 5-minute bucket for ranges over 2 days and per 30-minute bucket for ranges over 8 days. Each series has a `label` with the account suffix, such as `ChatGPT#rk`, and a `source_label` without it; the page merges the accounts of one quota by service and `source_label`.

Group keys are `<service>:<auth_index>:<group>`, for example `codex:3:codex:main`, `claude:3:claude:seven-day-fable` and `antigravity:4:antigravity:gemini-models`. The group key is also the ignition target ID.

## Design constraints

- Ignition goes only through `host.model.execute`, pinned to one credential; the plugin never builds a service's model request itself.
- Quota requests must leave through the same exit as CPA's model requests; when the exit cannot be determined, the request is skipped instead of connecting directly.
- Access tokens, `bark_url` and `models_api_key` never appear in logs, events, the status API or on disk.
- The JSON fields of host callbacks follow `sdk/pluginapi` and `internal/pluginhost` of CPA v8.0.4; check the CPA source before changing them.
- Changes to `state.json` stay backward compatible: new fields have defaults, and missing old fields do not stop the plugin.
