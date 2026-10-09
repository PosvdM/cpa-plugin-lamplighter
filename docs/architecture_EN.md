# Architecture

[中文](./architecture.md)

Lamplighter is a CPA native plugin written in Go and built with `-buildmode=c-shared`. CPA loads the shared library and calls `cliproxy_plugin_init`; after that, all traffic is RPC in JSON envelopes: CPA calls plugin methods, and the plugin calls CPA through host callbacks.

## Modules

| Path | Responsibility |
| --- | --- |
| `main.go` | cgo entry point: exports the C ABI functions, forwards RPC, implements host calls |
| `datadir_unix.go`, `datadir_windows.go` | the default data directory per platform |
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
| `GET /v0/management/lamplighter/history?range=1h\|3h\|6h\|12h\|24h\|26h\|4d\|8d\|15d\|1mo\|36d` | Quota history and ignition events, `24h` by default. In hours for the 5-hour quota and days for the 7-day quota, the ranges are: one unit; half a window plus one (`3h`, `4d`); a window plus one (`6h`, `8d`); half a day or month, which fits two whole windows (`12h`, `15d`); a day or a month (`24h`, `1mo`); five windows plus one (`26h`, `36d`). `1mo` runs from the same date of the previous month in the plugin's time zone, or its last day when that month is shorter, to now |
| `POST /v0/management/lamplighter/refresh` | Query now; `{"auth_index": "..."}` limits it to one account |
| `POST /v0/management/lamplighter/ignite` | Ignite now, `{"target": "<group key>"}` |
| `POST /v0/management/lamplighter/test-bark` | Send a test notification |
| `POST /v0/management/lamplighter/language` | Record the notification language, `{"language": "zh-CN"}`; any form of Chinese is stored as `zh`, every other language as `en` |

The page is the resource `GET /v0/resource/plugins/lamplighter/page` with the menu label `Lamplighter`. The resource itself needs no authentication; its data requests carry the management key. The page reads the key from `cli-proxy-auth` in `localStorage`, where the Management Center stores it with a reversible obfuscation derived from the host and user agent; otherwise it asks for the key and keeps it in `sessionStorage` only. Settings are saved through CPA's `PATCH /v0/management/plugins/lamplighter/config`, which merges top-level keys only, so the page sends `ignition`, `providers` and `codex_reset_updates` as complete objects.

The icon next to the page title is `assets/logo.png` loaded from the GitHub repository, so it can change without a release; it is hidden when it fails to load.

All page text lives in the `I18N` dictionaries in `page.js`, one for Chinese and one for English. Static text in `page.html` names its entry with `data-i18n` (and `data-i18n-placeholder`, `data-i18n-title`, `data-i18n-aria-label`); the Chinese text there only shows until the script applies the language. The language follows the Management Center: the page reads the `lang` attribute of the parent page's root element, which the Management Center updates on every switch, and watches it; opened on its own, it reads `cli-proxy-language` from `localStorage`, then falls back to the browser language. Languages starting with `zh` show Chinese, all others English. When the `language` in the status differs from the page language, the page reports it with `POST .../language` and retries on the next status refresh if that fails.

The page writes event text in its language from the event `params` (see Data below). Older events without `params` show their stored Chinese text. Error text returned by upstream services and notification titles are not translated; the titles are already in the notification language.

The page looks like the Management Center: its CSS variables reuse the names and values of the Management Center's `src/styles/themes.scss` (light, white and dark themes), quota bars and the chart are colored by the first two notification thresholds (by default green above 50% remaining, amber above 20%, red at 20% or less), matching the 🟡 and 🔴 notification titles, and telemetry numbers use a monospace font. The page is same-origin with the Management Center, so it reads `data-theme` from the parent page's root element and watches it for changes; opened on its own, it falls back to `cli-proxy-theme` that the Management Center stores in `localStorage`. When the Management Center changes its colors, update the variables in `internal/web/page.css`.

The page does all data handling for the quota chart:

- **Missing data**: when two samples are more than 3.5 poll intervals (or history buckets) apart, the gap is drawn with diagonal hatching instead of holding the previous value. With the passive skip enabled, the limit is at least `passive_skip_max_minutes` plus 1.5 poll intervals: windows missing from the response headers, such as an unused Fable quota, wait for the next active query during the skip, and that interval is not a gap.
- **Pooled accounts**: accounts with the same service and `source_label` share one row showing the mean of their remaining percentages. With equal plans this is the share of the total quota left; the API only reports percentages, so the mean cannot be weighted by quota size. The detail chart also shows the range from the lowest to the highest account.
- **Order**: rows follow `SERVICE_ORDER` (Claude, ChatGPT, Gemini, Fable, Claude / GPT) by default, with other quotas after them in API order; they can also be sorted by current remaining, lowest first. The status API orders the quota groups of each account the same way (`groupOrder` in `internal/engine/api.go`), so account cards show Gemini before Claude / GPT and Claude before Fable.
- **Resets**: a rise of 5 points or more between two samples is a reset. The line breaks there: the previous piece holds its last value up to the reset sample, the next piece starts at the value after the reset, and the reset time is labeled; two rises one sample apart count as one reset. One account resetting raises the pooled total by only its share, so the pooled line does not break when the rise is under 5 points.
- **Smoothing**: within one window a quota only falls, but active queries and response headers can differ by a point and round differently, so samples briefly step back up. Before drawing, each piece is fitted to the closest non-increasing sequence by pooling adjacent violators into their mean; otherwise every step back would leave a small notch in the line. Tooltips still show the raw samples. Lines use monotone cubic interpolation (Fritsch-Carlson), which never passes beyond the fitted values. Quotas report whole percents, so steps of 2 points or less are joined as a steady decline; a plateau followed by a larger drop stays flat and then bends at the drop.
- **Hover**: hovering either the overview or the detail chart snaps the crosshair to the sample of the detail row (the total line for pooled accounts) nearest the pointer; the tooltip time and every row's value are taken at that moment. Rows sampled at other times show their latest sample before it. When even the nearest sample is beyond the gap threshold, the crosshair stays at the pointer's time.

Host callbacks in use:

| Callback | Purpose |
| --- | --- |
| `host.auth.list` | Credential list with `auth_index`, runtime ID, email, disabled state and the CPA cooldown (`unavailable`, `next_retry_after`) |
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
- `instance.lock` is an exclusive lock on a file in the data directory (`flock` on Unix; on Windows the file is opened without sharing) that keeps only one instance querying, igniting and notifying at a time. A new instance that cannot take the lock retries every 10 seconds.

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
| Claude | `Anthropic-Ratelimit-Unified-7d_oi-Utilization` / `-7d_oi-Reset` | Fable's 7-day window, written to the same Fable quota group as the usage API (`quota.FableGroupKey`). CPA also treats `7d_oi` as the Fable-specific window. Regular (non-Fable) requests do not carry these headers; whether Fable requests do has not been verified on an account that can use Fable. The plugin logs the first one it reads after each start |
| Codex | `X-Codex-Primary-Used-Percent`, `-Reset-At`, `-Reset-After-Seconds`, `-Window-Minutes`, same for `Secondary` | Classified by window length: 300 minutes is 5-hour, 10080 is 7-day |

Passive data only updates the windows present in the headers; an active result replaces the whole group. `-Reset-After-Seconds` counts from `RequestedAt` of the usage record: the record arrives when the request completes, and counting from processing time would push the old cycle's reset time of a long request past the reset.

Skip rule (`shouldSkipActive`): a scheduled query is skipped when passive data arrived within `passive_skip_seconds` before it and the last active query is less than `passive_skip_max_minutes` old. Because only a short span before the slot counts, the longest gap between two data points is the interval plus `passive_skip_seconds`. Manual refreshes never skip.

## Ignition

### Timing

`ignite.Schedule.DueAt` returns the next ignition time:

1. Before `start_hour`: wait for `start_hour`.
2. After `end_hour + end_grace_minutes`: wait for `start_hour` the next day.
3. The 5-hour window is a rolling placeholder: now (subject to hold and retry times).
4. A future reset: `reset + grace_seconds`, or the next day's `start_hour` when that falls outside today's window.
5. A past or unknown reset: now (subject to hold and retry times).

While a window of the quota group other than the 5-hour one is used up (0.01% or less remaining) and before its reset, scheduled ignition skips the group: no attempt, no failure, no notification. The engine wakes at that window's reset, and the next query shows whether the quota is back. The ignition status names the blocking window (`blocked_window`, `blocked_until`). Manual ignition is not affected.

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

"Newest first" compares the numeric segments of model names, so new releases need no code change. Every ignition starts with the newest candidate. Older models usually stay in the list long after a new release, so the order does not depend on the last successful model. A configured `model` or `models` entry is the only candidate and must be in the list.

When the pinned credential rejects a candidate, CPA answers locally with `auth_not_found` (the credential does not serve the model) or `unknown provider for model` (no credential does) without calling the provider. The plugin then tries the next candidate, up to 6. If every candidate is rejected, the error counts as high risk. While the model list cannot be read, the configured or last successful model is used.

CPA's model list comes from its built-in model table, which it refreshes periodically, and every credential of a service lists the same models. A new model in the list may therefore not be usable by every account. When the provider refuses a candidate with HTTP 400 or 404 (`ignite.ModelRejected`), the plugin tries the next candidate in the same ignition, once per ignition; a second refusal points at the request or the account and counts as high risk, and the error text names the first refused model and its status. 401, 403 and 429 do not depend on the model and do not trigger another model.

When the next candidate works, the refusal concerns the model for that quota group's account: `engine.noteRefused` remembers the group and model in memory and records a `model_refused` event. For the next 24 hours (`refusedFor`), automatic selection for that group skips the model (`engine.skipRefused`), then tries it once more. Repeated requests for a model the account cannot use may add account risk, while new models can reach accounts in stages, so the model is retried daily. The record is not written to `state.json` and is cleared when the plugin restarts. Configured models are not affected.

The model list is read after the plugin starts, after the CPA address or `models_api_key` changes (`engine.maybeReadModels`), and for every ignition. Quota polls do not read it; when one of the first two reads fails, it is retried with each poll until it succeeds. After each read, `engine.noteModels` computes the first automatic candidate of each service, ignoring configured models. The status API carries them in `next_models` (`codex`, `claude`, `antigravity`), and the settings show each one after the hint of the ignition model field. Antigravity uses the Gemini group, matching the "newest Flash" hint. `next_models` is kept in memory only. A failed read keeps the previous result; when the CPA address or `models_api_key` changes, `next_models` and the model list error are cleared and the list is read with the new address and key. `next_models` is computed per service and does not reflect models skipped for one quota group.

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
| Model refused upstream | HTTP 400/404 among high-risk errors (`ignite.ModelRejected`) | Try another candidate once per ignition, then treat as high risk |
| High risk | `ErrNotConfirmed`, `ErrNoModel`, HTTP 400/401/403/404/409/422/429, `not found`, `unsupported` and similar | Pause until the next `start_hour` |
| Transient | CPA local cooldown (`are cooling down`), `auth_unavailable`, anything else | Retry after `failure_retry_seconds × multiplier^(n-1)`; pause until the next day at failure `max_transient_failures` |

When a CPA local cooldown carries a `reset_seconds` of 10 minutes or less (`ignite.CooldownWait`), it is not counted as a failure, and the retry runs when the cooldown ends plus `grace_seconds`. After a 429, CPA cools a credential down until the upstream reset plus a dozen or so seconds, so when the quota ran out before the reset, an ignition 3 seconds after the reset hits that cooldown. A second cooldown in a row counts as a normal transient error, so a cooldown that keeps renewing cannot retry forever.

**Stale CPA cooldowns**: after an account-level 429, CPA cools the credential down until the reset the provider reported, which can be days away when the 7-day quota is used up, and does not lift it when the quota comes back early, for example after a reset card. After each poll, `engine.checkCooldowns` checks every credential: when the CPA cooldown has more than 10 minutes left and a quota group with a 5-hour window shows, in data from the last 15 minutes, that no window is used up, the cooldown is stale. One notification is sent per cooldown end time (`cooldown_notices` in `state.json`) and a `cooldown_stale` event is recorded; accounts in the status API carry `cooldown_until` and `stale_cooldown`. The plugin does not clear the cooldown itself, because that needs the CPA management key and changes CPA's routing state.

A pause sends one Bark notification (`circuit_notified_until_epoch` prevents repeats). A success or a fixed future reset clears the failure state.

## Notifications

`notify.Alerts.ProcessGroup` compares each window's remaining quota with the level last notified:

- Levels are normal, notice (≤50%), low (≤20%), critical (≤10%) and exhausted (≤0.01%). A worse level is notified; critical and exhausted use `timeSensitive`.
- A level counts as better only after a real recovery: the reset time moved to a new window, or the quota rose by 5 points or more since the previous reading (`RecoveryJump`). Usage endpoints and response headers can differ by a point, so a small rise within a window leaves the notified level unchanged, and a reading that bounces around a threshold alerts once. A better level only updates the baseline silently, so that the next drop alerts again.
- Recovery notifications and reset reminders take the mode for the window's kind from `recovery_notify` and `reset_reminder`. `notify.SevenDayClass` knows 5-hour and 7-day windows by their labels; any other window counts as a 7-day window once it was seen more than a day before its reset (`long` in `state.json`), and as a 5-hour window otherwise.
- Recovery notifications follow cycle changes only (`cycleEnded`). When both reset times are known, the reset time must move to a new cycle, and either the old reset time has passed or the quota rose by `RecoveryJump`. Codex returns a reset time that moves forward with every query while a window is unused, so a changed reset time alone would count every query as a reset. Without a new reset time, a passed old reset time or a rise by `RecoveryJump` counts.
- Usage records arrive when a request completes, so a long request started before a reset delivers the old cycle's headers after it. A reading whose reset time has passed and belongs to an ended cycle (`ended_reset`) or precedes the current one is ignored; otherwise it would pull the state back into the old cycle and trigger another recovery.
- A detected change is stored as a pending recovery (`pending_recovery`) with the last reading of the ended cycle and whether that cycle ran out (`exhausted`). The last reading is the last one taken before its reset time (`cycle_reset`, `cycle_remaining`, `cycle_seen`), because some providers keep reporting the passed reset time with the new cycle's value for a while. A reading more than 15 minutes before the reset is not reported; a used-up window stays at 0 until its reset, so its reading never gets old. A cycle still at 100% before its reset is not notified: after the plugin was stopped for longer than a window, the moving reset time of an unused Codex window also looks passed. `after_exhausted` sends only used-up cycles. A failed delivery is retried at the next check and dropped after an hour. Recoveries and drops are sent as separate notifications.
- For every window with a reset time, whatever the notification settings, `engine.probes` queries the account once 30 seconds before and once 30 seconds after the reset (`probeOffset`), once per account and time; times within 10 seconds count as one. A query is skipped when a later reading already exists. The query after the reset runs only within 10 minutes of it (`probeLate`); an old reset still in the view after a restart waits for the next poll. `probes` only computes and marks nothing, so the loop can use it for the wake-up time; `runProbes` records the queries it runs.
- Reset reminders: 5-hour-kind windows are reminded once 1 hour before the reset (`reset_notice_1h_for`), 7-day-kind windows once 1 day before (`reset_notice_1d_for`). `has_remaining` reminds only when more than `critical_threshold` is left.
- Two reset times within 10 seconds belong to the same cycle.
- A window seen for the first time only records its baseline.
- A failed delivery leaves the notified level unchanged, so the next check retries.

A Bark request is `GET {bark_url}/{title}/{body}?group&level&icon&url`, with title and body percent-encoded except RFC 3986 unreserved characters. A JSON `code` other than 200 is a failure. `icon` defaults to the repository's `assets/logo.png`; a `bark_icon` that still holds the old default (the CPA Management Center logo) is replaced with it on load.

Notification texts are in `internal/notify/text.go`, one column each for Chinese and English, chosen by `language` in `state.json`. The management page reports the language: notifications are sent without a page open, so they use the language reported last, and Chinese until one has been reported. Window names in notifications are always `5h` and `7d`. The Did Codex Reset link points to the Chinese or English history page.

Did Codex Reset is read every `poll_seconds` seconds (at least 300, aligned to time boundaries), 10 latest records at a time. Records are deduplicated by ID, except `manual:` records, whose IDs can change and which are deduplicated by content; up to 100 seen keys are kept. The first run marks existing records as seen and notifies only the pending schedule when `notify_current_pending` is on.

Only records that are still news are sent; the rest are marked as seen. A schedule must have `scheduleState = pending` and a window that has not ended; a completed reset must be at most 48 hours old, measured from the latest of `completedAt`, `effectiveAt` and `announcedAt`. A fulfilled schedule leaves the `kind=all` list and comes back as `elapsed` when it loses its completion link, so old records can appear at any time. Every X post is one record, and a post and its reply each create a record for the same schedule. Schedules with the same reset type, window start and scope (`scope.plans`, `scope.windows`) are one schedule. Only the start is compared, because some records of a schedule carry just `effectiveAt` and others the whole `scheduleWindow`. As on the Did Codex Reset homepage, within one poll only the notifiable post with the latest `announcedAt` is sent, ties broken by record key, and the rest are marked as seen. A reply can enter the latest 10 one or two polls late, so each sent schedule is kept in `sent_events` (up to 100) with the post's `announcedAt` and the notification content, built in UTC and English. A later post about the schedule is sent again, titled "schedule updated", only when its `announcedAt` is later and its content (confidence, time, plans) differs; otherwise it is marked as seen.

The schedule time comes from `scheduleWindow`, or `effectiveAt` without one, shown in the plugin time zone: one time when start and end match, "by end" when `scheduleConstraint = deadline`, and "start–end" otherwise (date-level schedules). A date-level window covers a whole day in the source time zone and usually starts before the announcement, so its start alone reads as a time already past.

## Data

The data directory defaults to `data/lamplighter` in the plugin directory. On Linux and macOS, `datadir_unix.go` finds the library path with `dladdr`; when the library sits in `<plugins>/<goos>/<goarch>/`, those two levels are removed. On Windows CPA loads a copy of the DLL from the temp directory, so the library path does not point to the plugin directory; `datadir_windows.go` reads `plugins.dir` from CPA's `config.yaml` instead (default `plugins`, relative to CPA's working directory).

| File | Content |
| --- | --- |
| `state.json` | `groups`: notification baselines per window; `scheduler`: ignition state per group; `codex_reset_updates`: seen records and sent schedules; `cooldown_notices`: stale cooldowns already notified; `language`: notification language. Written to a temporary file that replaces the old one |
| `history/YYYY-MM-DD.jsonl` | Quota samples and events, one file per UTC day |
| `instance.lock` | Instance lock |

Each history line is one JSON object. A sample is `{"k":"s","t":seconds,"g":group,"w":window,"r":remaining,"x":reset,"s":"active|passive"}`, an event is `{"k":"e","t":seconds,"g":group,"e":type,"v":level,"m":text,"l":label,"d":detail,"p":params}`. `m` and `d` are Chinese text for the CPA log; `p` holds the values the page builds the event text from: `model` and `reset` for `ignite` and `ignite_manual`, `retry_seconds`, `error` and an optional `cooldown` for `ignite_failed`, `until` and `error` for `ignite_paused`, `model`, `fallback` and `hours` for `model_refused`, `title` for `notify`, `title` and `error` for `notify_failed`, `until` for `cooldown_stale`, and `count` for `codex_reset`; times are RFC 3339 UTC. Active samples are always written. Passive samples are written only when the value or reset changes, at most once per minute per window; a newer value inside that minute waits in `pendingSamp`. The history API keeps the last sample per 5-minute bucket for ranges over 2 days and per 30-minute bucket for ranges over 8 days. Each series has a `label` with the account suffix, such as `ChatGPT#rk`, and a `source_label` without it; the page merges the accounts of one quota by service and `source_label`.

Group keys are `<service>:<auth_index>:<group>`, for example `codex:3:codex:main`, `claude:3:claude:seven-day-fable` and `antigravity:4:antigravity:gemini-models`. The group key is also the ignition target ID.

## Design constraints

- Ignition goes only through `host.model.execute`, pinned to one credential; the plugin never builds a service's model request itself.
- Quota requests must leave through the same exit as CPA's model requests; when the exit cannot be determined, the request is skipped instead of connecting directly.
- Access tokens, `bark_url` and `models_api_key` never appear in logs, events, the status API or on disk.
- The JSON fields of host callbacks follow `sdk/pluginapi` and `internal/pluginhost` of CPA v8.0.4; check the CPA source before changing them.
- Changes to `state.json` stay backward compatible: new fields have defaults, and missing old fields do not stop the plugin.
