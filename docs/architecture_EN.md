# Architecture

[中文](./architecture.md)

Lamplighter is a CPA native plugin written in Go and built with `-buildmode=c-shared`. CPA loads the shared library and calls `cliproxy_plugin_init`; after that, all traffic is RPC in JSON envelopes: CPA calls plugin methods, and the plugin calls CPA through host callbacks.

## Modules

| Path | Responsibility |
| --- | --- |
| `main.go` | cgo entry point: exports the C ABI functions, forwards RPC, implements host calls |
| `internal/plugin` | Plugin RPC methods: register, reconfigure, quiesce, management API, `usage.handle` |
| `internal/engine` | Background loop: active queries, passive data, ignition, notifications, history and status data |
| `internal/host` | Go wrapper of the host callbacks and timeout handling for `host.http.do` |
| `internal/config` | Plugin config parsing, defaults and ranges; reads the CPA config to see whether the plugin is enabled and to find the default data directory |
| `internal/providers` | One file per service: quota request and parsing, response header parsing, ignition request, candidate models, defaults; the registry, see "Services" below |
| `internal/quota` | Types of quota windows and upstream requests, window kinds, time parsing |
| `internal/egress` | Picks the network exit of quota requests |
| `internal/models` | Reads the CPA model list and provides the helpers that order candidates by recency |
| `internal/ignite` | Ignition timing, rolling-reset detection, error classification and failure protection |
| `internal/notify` | Bark and webhook delivery, quota alerts, Did Codex Reset forwarding |
| `internal/release` | Reads the latest GitHub release and compares versions |
| `internal/store` | `state.json`, quota history and the instance lock |
| `internal/web` | Management page; HTML, CSS and JS are separate files combined into one document at runtime |

## Interface with CPA

`plugin.register` returns `schema_version` 6 and declares two capabilities:

- `management_api`: registers the management routes and the page;
- `usage_plugin`: receives one usage record per request, including the upstream response headers.

Management routes (require the CPA management key):

| Method and path | Purpose |
| --- | --- |
| `GET /v0/management/lamplighter/status` | Status page data; secrets in the config only show whether they are set. `providers` lists the supported services in display order (`id`, `title`, whether it can be ignited `ignite`, the model hint `model_hint` and the quota groups offered for ignition `groups` with their note `groups_hint`, in Chinese and English), and `group_order` is the display order of quota groups; each window carries `kind` (`5h` or `7d`; windows of other lengths have no such field) |
| `GET /v0/management/lamplighter/history?range=1h\|3h\|6h\|12h\|24h\|26h\|4d\|8d\|15d\|1mo\|36d&window=5h\|7d` | Quota history and ignition events, `24h` by default. With `window` set to `5h` or `7d` only series of that window kind are returned; without it or with another value, all windows are. Ignition events are not filtered by window. In hours for the 5-hour quota and days for the 7-day quota, the ranges are: one unit; half a window plus one (`3h`, `4d`); a window plus one (`6h`, `8d`); half a day or month, which fits two whole windows (`12h`, `15d`); a day or a month (`24h`, `1mo`); five windows plus one (`26h`, `36d`). `1mo` runs from the same date of the previous month in the plugin's time zone, or its last day when that month is shorter, to now |
| `POST /v0/management/lamplighter/refresh` | Query now; `{"auth_index": "..."}` limits it to one account |
| `POST /v0/management/lamplighter/ignite` | Ignite now, `{"target": "<group key>"}` |
| `POST /v0/management/lamplighter/test-notify` | Send a test notification to all configured channels at once. `results` holds one entry per channel: `channel`, `ok`, `error`, and for the webhook `response` (the first 200 characters of the response) and `unchecked` (no `success_json`, so only the HTTP status was checked) |
| `POST /v0/management/lamplighter/language` | Record the notification language, `{"language": "zh-CN"}`; any form of Chinese is stored as `zh`, every other language as `en` |
| `POST /v0/management/lamplighter/check-update` | Check for a newer release and return the status; the page calls it when it opens |

The page is the resource `GET /v0/resource/plugins/lamplighter/page` with the menu label `Lamplighter`. The resource itself needs no authentication; its data requests carry the management key. The page reads the key from `cli-proxy-auth` in `localStorage`, where the Management Center stores it with a reversible obfuscation derived from the host and user agent; otherwise it asks for the key and keeps it in `sessionStorage` only. Settings save automatically through CPA's `PATCH /v0/management/plugins/lamplighter/config`: the form's `change` event triggers a save that sends only the top-level key of the changed field. The endpoint merges top-level keys only, so objects such as `ignition`, `providers` and `recovery_notify` are sent whole, starting from the saved object. Saves run one at a time, each starting from the config the previous one wrote; a number outside its field's range or step is not saved. Browsers fill the password saved for the site, usually the management key, into password fields. `models_api_key` is the only password field in the settings, and its value is saved by these rules: a value the browser filled in (matching `:autofill` or `:-webkit-autofill`) while the field is not focused comes from autofill on page load and is neither saved nor reported; a value equal to the current management key is not saved, with a message that it is the management key; any other non-empty value is first used for `GET /v1/models`, the request the plugin makes for the model list, and is not saved when that returns 401 or 403, with a message that CPA does not accept the key; any other result, including a failed request, saves the value. A value that is not saved is replaced in the field by the saved one. The field also carries the attributes that make 1Password, Bitwarden and LastPass skip it. The `models_api_key` field is marked `autocomplete="one-time-code"`: Chrome's password manager does not treat such fields as passwords, and in a form with no other password field uses it only as a fallback, which it neither fills on page load nor follows with a prompt to save or update a password. `autocomplete="off"` has no effect on password fields, and `new-password` makes Chrome offer a generated password, so neither is used. `webhook.headers` and `webhook.success_json` are edited as text on the page (one `Name: value` line per header, a JSON object) and are not saved when they do not parse. The status API only reports whether the webhook URL, headers and body are set; the page fills the form with the saved values from CPA's plugin config endpoint. Three seconds after the last save, the page reads the status again and fills the saved fields with the values the plugin applied, leaving the field being edited alone; a failed save puts the saved values back. A test notification waits for pending saves.

"Name" on an account card writes the credential's `note` with CPA's `PATCH /v0/management/auth-files/fields`: it replaces the first line of the note, keeps the other lines, then calls `refresh` for the account so that the plugin reads the new name at once. CPA trims the note, and a note whose first line is empty takes its name from the next line, so clearing the name of a note with more lines deletes the whole note after a confirmation. A failed quota query does not undo the rename, so the result of `refresh` only updates the page and is not shown as the result of the rename. When a service has several accounts, the card also has "Weight": after a number above 0 is entered, the page saves the whole `account_weights` object through the plugin config API (CPA merges top-level keys only), an empty answer deletes the account's weight, and the chart is redrawn with the new value at once; `config.Parse` drops weights that are not above 0 or infinite.

The icon next to the page title is `assets/logo.png` loaded from the GitHub repository, so it can change without a release; it is hidden when it fails to load.

All page text lives in the `I18N` dictionaries in `page.js`, one for Chinese and one for English. Static text in `page.html` names its entry with `data-i18n` (and `data-i18n-placeholder`, `data-i18n-title`, `data-i18n-aria-label`); the Chinese text there only shows until the script applies the language. The language follows the Management Center: the page reads the `lang` attribute of the parent page's root element, which the Management Center updates on every switch, and watches it; opened on its own, it reads `cli-proxy-language` from `localStorage`, then falls back to the browser language. Languages starting with `zh` show Chinese, all others English. When the `language` in the status differs from the page language, the page reports it with `POST .../language` and retries on the next status refresh if that fails.

The page writes event text in its language from the event `params` (see Data below). Older events without `params` show their stored Chinese text. Error text returned by upstream services and notification titles are not translated; the titles are already in the notification language.

The page looks like the Management Center: its CSS variables reuse the names and values of the Management Center's `src/styles/themes.scss` (light, white and dark themes), quota bars and the chart are colored by the first two notification thresholds (by default green above 50% remaining, amber above 20%, red at 20% or less), matching the 🟡 and 🔴 notification titles, and telemetry numbers use a monospace font. The page is same-origin with the Management Center, so it reads `data-theme` from the parent page's root element and watches it for changes; opened on its own, it falls back to `cli-proxy-theme` that the Management Center stores in `localStorage`. When the Management Center changes its colors, update the variables in `internal/web/page.css`.

The Management Center fills its content area with the plugin page's iframe, with no space at the top, and its floating toolbar (178 × 52 px, 24 px from the top and right edges of the window) covers the top right corner of the iframe. At a window width of 768 px or less the Management Center uses its phone layout: the sidebar is hidden, the iframe is as wide as the window, the toolbar is 162 × 48 px at 12 px from the edges, and a 48 × 48 px menu button sits in the top left corner. Opened in an iframe (`window.self !== window.top`, which needs no access to the parent, so it also works when the Management Center is on another origin), the page adds the `embedded` class to its root element: the header leaves room for the toolbar on the right and wraps when it runs out of space; "Refresh all" sits at the right of the quota heading, whose row starts 8 px below the toolbar; at 768 px wide or less the whole page starts below the toolbar and the menu button; wider than 1584 px the toolbar falls into the page margin and nothing changes. The values come from `.main-header .header-actions` and `.mobile-sidebar-actions` in the Management Center's `src/styles/layout.scss`; when the Management Center moves or resizes these controls, update the matching rules in `page.css`.

The page does all data handling for the quota chart:

- **Time axis**: for 1 and 3 hours and for 1 and 4 days, the axis ends now. From 6 hours and 8 days on (6, 12, 24 and 26 hours for the 5-hour quota; 8 and 15 days, 1 month and 36 days for the 7-day quota), the axis still spans the chosen range but ends at the reset of the selected row's current window. A pooled row takes the next reset that comes first among its accounts, the time shown by ↻ at the end of the row; resets that have passed (a window not opened again, or not queried since) do not count. 6 hours, for example, shows the current 5-hour window and the last hour of the previous one. `24h` in `RANGES` is both the 24 hours of the 5-hour quota and the 1 day of the 7-day quota, and only the former extends (`extendsAhead`). The history API returns the range up to now, and the page leaves out what lies before the start of the axis. Selecting another row shifts the whole chart; as time passes the axis stays put, and after a reset it ends at the new window's reset. When the selected row has no reset, its reset has passed (a 5-hour window no request has opened yet), or the reset is further away than the chosen range, the axis ends now. The time after now is hatched like missing data, without a line marking now. The overview and the detail chart share this axis.
- **Missing data**: when two samples are more than 3.5 poll intervals (or history buckets) apart, the gap is drawn with diagonal hatching instead of holding the previous value. With the passive skip enabled, the limit is at least `passive_skip_max_minutes` plus 1.5 poll intervals: windows missing from the response headers, such as an unused Fable quota, wait for the next active query during the skip, and that interval is not a gap.
- **Pooled accounts**: accounts with the same service and `source_label` share one row showing the mean of their remaining percentages weighted by the accounts' weights (`pooled`); when the weights are proportional to the sizes of the quotas, this is the share of the total quota left. The current value, the line and the projection of a pooled row all use the weighted mean; the shade in the detail chart still spans the lowest to the highest account. The weight is the one set in `account_weights` for the credential file name (`name` of the account in the status API), then the account's `default_weight` from the status API, and 1 without either (`accountWeight`); with all weights 1 this is the plain mean.
- **Window kinds**: the chart puts history series into the "5-hour" and "7-day" tabs by their `kind` and offers only the kinds the accounts report (both while none are known), opening the first one; windows of other lengths are not charted. A window with its own label under the same quota name as the main window, such as Claude's `7 Day Opus`, gets a row of its own instead of being pooled with it.
- **Order**: rows follow the status API's `group_order` (Claude, ChatGPT, Gemini, Kimi, Grok, Fable, Claude / GPT, from `groupOrder` in `internal/providers/provider.go`) by default, with other quotas after them in API order; they can also be sorted by current remaining, lowest first. The status API orders accounts by the registration order of their service and the quota groups of each account the same way, so account cards show Gemini before Claude / GPT and Claude before Fable.
- **Remembered filters**: with `remember_chart_filters` on, the page keeps the selected window, the order and the range of each window in `lamplighter-chart-filters` in `localStorage` and restores them when it opens. The 5-hour and 7-day windows offer different ranges, so each keeps its own. A saved value that is not among the current choices falls back to the default. The page reads the switch from the settings form, so a change takes effect before it is saved; turning it off deletes the record.
- **Loading**: the page requests the history of the selected window (the `window` parameter) and caches responses by window and range. Choosing a cached combination draws the cached data at once and requests it again, redrawing when the response arrives; without a cached response the chart is grayed out until it arrives. Only the response of the latest load is drawn; an older one that arrives after a switch only goes into the cache. While a request for the same combination is under way, a load joins it, except the loads after a refresh or an ignition. When a request fails and there is no data for the chosen combination, the chart shows the error instead of another combination's old chart. When the page opens and the first chart has loaded, the page requests the other window's range in advance, so switching windows does not wait. The page reads the status every 30 seconds; when a window's `observed_at` is later than the last sample of that window in the chart and comes from an active query or has a different remaining (passive readings that repeat the value are not written to the history), it requests the drawn chart again (`refreshHistory`). While the pointer is inside the chart card, the new data is only kept and drawn once the pointer leaves, so the crosshair and tooltip under it are not replaced.
- **Resets**: a rise of 5 points or more between two samples is a reset. The line breaks there: the previous piece holds its last value up to the reset sample, the next piece starts at the value after the reset, and the reset time is labeled; two rises one sample apart count as one reset. One account resetting raises the pooled total by only its share, so the pooled line does not break when the rise is under 5 points.
- **Even-pace line**: the detail chart draws a dashed line for each cycle, falling linearly from 100% when the cycle starts to 0% at its reset, and the tooltip shows its value at the hovered time. The header of the detail chart shows the current remaining, the even-pace value now, the difference between the two (green above the pace, red below) and the time until the reset. When the axis ends now, the right of the plot shows the current remaining at the height of the end of the curve, the even-pace value at the height of the end of the even-pace line, and the difference halfway between them, with a vertical line joining the two ends; values less than a line (`LABEL_H`) from the difference move up and down away from it, which stays halfway. For a single account, a cycle ends at a reset found in the samples or at the current window's `reset` from the status API (the two count as one reset when they are less than half a window apart), and starts one window length (5 hours or 7 days) before its end, but not before the previous cycle ends. The start is not the previous reset: a 5-hour window starts with the first request after a reset, so there can be time between two windows that belongs to neither. A reset that falls in a gap is not found, and its cycle gets no line; cycles are not extrapolated at a fixed interval, which would misplace that time between windows. For a pooled row the even pace is the mean of its accounts' even pace weighted by their weights (`rowPace`), with each account's cycles found as above and only the accounts inside a cycle at that time counted, so the line bends or jumps where any account's cycle starts or ends. Joining the accounts' resets directly would bring the value close to 0% before every reset and make the difference meaningless. Resets found in the total line are not used: one account resetting can raise the total by less than 5 points.
- **Smoothing**: within one window a quota only falls, but active queries and response headers can differ by a point and round differently, so samples briefly step back up. Before drawing, each piece is fitted to the closest non-increasing sequence by pooling adjacent violators into their mean; otherwise every step back would leave a small notch in the line. Tooltips still show the raw samples. Lines use monotone cubic interpolation (Fritsch-Carlson), which never passes beyond the fitted values. Quotas report whole percents, so steps of 2 points or less are joined as a steady decline; a plateau followed by a larger drop stays flat and then bends at the drop.
- **Overview band color**: the band is colored by the lowest remaining so far in each cycle: a sample never counts higher than the lowest one before it, and a reset starts again. Claude's response headers always report a point more than its active queries (50 active, 51 passive), so near a threshold the raw samples would flip the band's color for a moment and leave a thin seam. The non-increasing fit of the line is not used here: it averages 50 and 51 to 50.5, which keeps the other color even longer. The detail line and the tooltips are not affected; tooltips still show the raw samples.
- **Projection**: when the axis ends at a reset and the latest data has no gap, the detail chart draws a dotted line from the current point (`projection`). When it reaches 0% first, it stops there, and the time it runs out, for the 7-day quota with the date, is marked on the time axis in place of the tick labels it covers; when the reset comes first, it stops at the reset, and what is left is marked right of the plot; a rate that does not fall gives a level line. The 5-hour and 7-day quotas use different rates, for the reasons under "Evaluation of the projection" below.
  - The 5-hour quota uses the current rate (`currentRate`), showing where use at that rate would lead. The rate comes from the samples of this cycle (after the last rise of `floor`): first the lowest remaining so far, as for the overview band, then a bottom-up piecewise linear fit, whose last piece gives the slope (`recentSlope`). The fit starts with one piece per pair of neighbouring samples and merges two neighbouring pieces into one least-squares line while every sample stays within 2 points of it (`QUANTUM`), the closest fit first. Quotas report whole percents, so the samples form a staircase, and the slope between two neighbouring samples is either zero or very steep; the samples are therefore not differentiated directly. How far back the rate looks depends on the pieces, which split where the use changes. Passive samples are written up to a minute late, so only when the last passive sample has not changed for more than 2 minutes (`HOLD_AFTER`) does its value count until now in the fit; after use stops, the projection gradually levels out. Active samples are written once per poll interval, so the last active sample counts until now only after 1.5 sample steps; otherwise the projection would level out in the second half of every poll interval. Until the last sample counts until now, the curve stops at it, and the projection and the hatching start there; a curve held level until now, followed by a projection still falling at the earlier rate, would look shifted to the right. Once it counts, the curve reaches now and the projection starts now. In the first minutes after a sudden burst of use the last piece has only a few samples, and the projected time runs out early before it gradually moves later.
  - The 7-day quota uses this cycle's daily average (`cycleRate`): what the cycle has used so far (100 less the last value of `floor`) over the time since it began. The cycle begins at the current window's `reset` from the status API less 7 days; resets found in the samples are not used. Without a reset time, with a reset time that has passed, with a cycle that has not begun, or with the last sample before the cycle began, there is no projection. A pooled row takes the mean of its accounts' daily averages weighted by their weights, each from its own reset, counting only the accounts that meet these conditions.
  - Evaluation of the projection: the whole history from 2026-10-03 to 10-20 was replayed cycle by cycle, predicting at each moment, from the samples available then, what would be left at the reset, and comparing that with the actual value. For the 7-day quota (two cycles), the mean error was 20.0 points with the slope of the last piece, 13.5 and 12.3 with fixed look-backs of 24 and 48 hours, and 9.9 with this cycle's daily average. The 7-day quota is usually used by day and not at night, so the last piece reflects only an evening's burst of use and projects the quota to run out too early; a fixed look-back depends on the rhythm of use as well, so it is not used. For the 5-hour quota (69 cycles), no extrapolation was more accurate than assuming no further use; how much a window will still be used is essentially unpredictable, so the current rate is kept. The quota never reached 0% during the replay, so the accuracy of the projected run-out time was not evaluated.
- **Hover**: hovering either the overview or the detail chart snaps the crosshair to the sample of the detail row (the total line for pooled accounts) nearest the pointer; the tooltip time and every row's value are taken at that moment. Rows sampled at other times show their latest sample before it. When even the nearest sample is beyond the gap threshold, the crosshair stays at the pointer's time. To the right of now the crosshair follows the pointer; the detail chart's tooltip shows the projection and the even-pace value at that time, and the overview shows no tooltip.

Host callbacks in use:

| Callback | Purpose |
| --- | --- |
| `host.auth.list` | Credential list with `auth_index`, runtime ID, email, note (`note`), disabled state and the CPA cooldown (`unavailable`, `next_retry_after`) |
| `host.auth.get` | Credential file JSON: access token, Codex account ID, Antigravity `project_id` and `user_agent`, Kimi `domain` and `base_url`, Grok `sub`, `proxy_url` |
| `host.model.execute` | Ignition, with `auth_id` and `forced_provider` |
| `host.http.operation_open` / `host.http.do` / `host.http.cancel` | Quota requests of credentials without their own proxy, and reading the Antigravity Hub update manifest |
| `host.log` | Writes to the CPA log |

Host callbacks are bound to the plugin instance, not to one RPC, so the background loop can call them at any time.

At the start of each active poll, `engine.loadCreds` reads the credential list and recomputes each account suffix (`Cred.Suffix`). When the first line of the note (`accountName`, at most `maxNameRunes` characters) is not empty, the suffix is `#` and the name, followed by the email suffix when another account of the service has the same suffix, such as the same name. An unnamed account gets the email suffix (`maskedSuffix`) only when its service has several accounts. The `Label` of each quota group follows. Alert state is keyed by quota group key, so renaming an account sends no new notifications.

## Services

Each service is a `providers.Provider` in `internal/providers`, registered in display order in `registry` in `provider.go`: ChatGPT (`codex`), Claude, Antigravity, Kimi, Grok (`xai`). A service defines:

| Field | Content |
| --- | --- |
| `ID`, `Aliases`, `Title` | The provider name used in config keys and quota group keys; other names CPA uses (`kimi-ai`, `kimi.ai`, `kimi.com` for Kimi; `x-ai`, `x.ai`, `grok` for Grok); the display name |
| `Monitor`, `Ignite` | Whether the service is monitored and ignited by default |
| `Fetch` | The active query, returning quota groups |
| `Headers` | Parses the response headers of a model request; nil when the headers carry no quota, which also turns the passive skip off for the service |
| `Ignition` | Ignition protocol, body, candidate models, model hint and the quota groups offered as checkboxes; nil when the service is not ignited, and the settings show "No ignition" |

`providers.Lookup` finds a service by name or alias (case-insensitive, `_` counts as `-`). The provider name of a credential is stored as the `ID` in `Cred.Provider` and as reported in `Cred.HostProvider`. A provider key written with an alias in the config is stored under the `ID`.

**Window kinds**: `quota.Window.Period` is the window length, filled by each service from its data: `limit_window_seconds` and `Window-Minutes` for Codex, the field name for Claude, `window` for Antigravity, `duration` and `timeUnit` for Kimi, and the period's start and end for Grok. `quota.Kind` decides by the period (within 5% of 5 hours or 7 days) and, without one, by ID and label. 5-hour and 7-day windows use fixed IDs and labels (`five-hour`, `seven-day`); windows of other lengths keep their own ID, with the length from `quota.PeriodLabel` as label, such as `30d`; Grok, whose period length can vary by month, always uses `credits`. Ignition, the cooldown check, the chart and notifications all go by kind, so accounts without a 5-hour window (such as ChatGPT Pro) are not ignited and the chart shows no 5-hour tab for them.

**Adding a service**: create a file in `internal/providers`; implement `Fetch` after the request of the CPA Management Center quota page, `Headers` when the provider sends quota headers, and set `Period` on the windows; fill in `Ignition` when the service can be ignited; add the service to `registry`, and its main quota group name to `groupOrder` when it needs a fixed position; add the Management Center's badge colors of the service to `internal/web/page.css` (`--type-<id>-bg` and so on). Update the service table in the README and the active query, passive data, ignition request and model selection sections of this document.

## Background loop

`engine.Engine` does all network work on one goroutine, so queries, ignition and notifications never modify the state concurrently:

1. It starts after plugin registration and waits 20 seconds so that CPA can load credentials and executors.
2. It opens the data directory and takes `instance.lock`.
3. Every 30 seconds it reads CPA's `config.yaml` and checks `plugins.enabled` and the plugin's own `enabled`. CPA does not notify a plugin that it disabled, so the plugin checks itself and does nothing while disabled. An unreadable config file counts as enabled.
4. It applies passive data, polls Did Codex Reset, runs due active queries and due ignitions, prunes history, checks for a new version and saves the state.
5. It computes the next wake-up time and until then waits for passive data, management actions or config changes.

`usage.handle` and the management routes never make network requests themselves. The former parses headers and queues them (dropping a sample when the queue is full is harmless); the latter hands the action to the loop and waits up to 3 minutes for the result. The one exception is `check-update`, described below.

The version check reads `releases/latest` from the GitHub API, which leaves out drafts and prereleases. It runs once the loop starts working and then every 6 hours, 30 minutes after a failure. Opening the page calls `check-update`, which reads it again unless a read was tried, successfully or not, less than a minute before (`releaseRecheckPeriod`): the unauthenticated GitHub API allows 60 requests an hour per address, and when GitHub does not answer, opening the page should not wait for the timeout every time. This read runs in the goroutine handling the request, outside the loop, so that it does not wait behind queries and ignitions; stopping the plugin cancels it. Only one read runs at a time (`releaseMu`): while one is under way, the page's request returns at once and the loop does not wait but tries again in 30 minutes; within a minute of a successful read, the loop does not read either. The check does not run while the plugin is disabled, and the status keeps the last result. The request uses the same exit as Did Codex Reset (the proxy from the environment, or a direct connection). Versions are compared by numeric segment; when the latest version is newer than the running one, the status endpoint returns `latest_version` and `release_url` and the page shows the update icon to the right of the version. A failure is logged at debug level only and keeps the previous result. A running version that is not purely numeric (development builds `dev` and `0.0.0-dev.<commit>`, and prereleases) is not checked; a latest tag that is not purely numeric counts as no newer version.

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
| Kimi | `GET https://api.kimi.com/coding/v1/usages`; like CPA's Kimi executor, the credential's `domain`, `base_url`, `type`, provider name and finally `kimi-ai` or `kimi.ai` in its runtime ID or file name decide, and kimi.ai uses `api.kimi.ai` |
| Grok | `GET https://cli-chat-proxy.grok.com/v1/billing?format=credits`, with the Grok CLI headers and the credential's `sub` as `x-userid`. A weekly `currentPeriod` is a 7-day window; the monthly `billingPeriodStart`/`End` is only used without a `currentPeriod`. A period of another length always uses the ID `credits`, so that history and alerts continue across periods |

The headers match the CPA Management Center quota page; the constants are in each service's file in `internal/providers`. The Codex user agent equals the one of CPA's Codex executor, and the Claude user agent equals CPA's default Claude Code fingerprint.

The Antigravity user agent follows the rules of CPA's Antigravity executor. Cloud Code tells clients apart by user agent and leaves the Gemini quota group out for old-style clients such as `antigravity/cli/1.0.13 (aidev_client; ...)`.

- When the credential sets `user_agent` (at the top level of the file, in `attributes` or in `metadata`), it is used; a value starting with `antigravity/` loses its ` google-api-nodejs-client/...` suffix.
- Otherwise the user agent is `antigravity/hub/<version> darwin/arm64`. The version comes from the Antigravity Hub update manifest (`AntigravityHubManifestURL`, with the same URL and headers as CPA) and is cached for 3 hours; a failed read keeps the cached version and is retried 30 minutes later. Until the first successful read, `2.23.0` is used, the same as the CPA Management Center. The manifest is read when quota is queried, without the credential proxy.

The token comes from the credential file. CPA refreshes tokens in the background and writes them back; the plugin never refreshes them. On HTTP 401 the plugin skips the round and waits for CPA's refresh.

Queries align to absolute time boundaries (`alignedAfter`): the default 300-second interval lands on minutes `:00 / :05 / ... / :55`. "Last run plus interval" is not used because it drifts with the start time.

### Network exit

`internal/egress` sends quota requests through the same exit as CPA's model requests:

- With a `proxy_url` on the credential, the connection is built with CPA's `sdk/proxyutil`, as CPA's executors do; `direct` / `none` means a direct connection. A value that cannot be parsed returns an error instead of falling back to another exit.
- Without a `proxy_url`, the request goes through `host.http.do`, where CPA uses the global `proxy-url`, then environment proxies, then a direct connection. `host.http.do` has no timeout, so the plugin opens an operation with `host.http.operation_open` and cancels it with `host.http.cancel` when the timeout expires.

CPA's `/api-call` (used by the Management Center quota page) ignores environment proxies, while model requests do not. The plugin follows the model request rules.

### Plans

Accounts in the status API carry `plan` (the plan name, empty when it is not known) and `default_weight` (the default weight in the total of several accounts), which `providers.PlanOf` derives from the stored raw plan value. Raw values are kept by `auth_index` in `plans` in `state.json`, so they do not wait for the next query after the plugin restarts. The default weight is the size of the account's quota as a multiple of the service's cheapest paid plan.

- Codex: the `plan_type` of the `wham/usage` response, updated with every active query, without a request of its own. The names follow the status page of the Codex CLI (`codex-rs/tui/src/subscription.rs`), and the default weight is the price divided by the $20 of Plus: `prolite` (Pro $100) and `self_serve_business_prolite` (Business Premium) are 5, `pro` (Pro $200) is 10, `promax` (Pro $500) is 25, `free` and `go` are 0.1, and the rest 1; Business (`team`), Enterprise and Edu seats have the quota of Plus. An unknown `plan_type` is shown as it is, with weight 1.
- Claude: the usage response has no plan. After a successful active query the engine requests `GET https://api.anthropic.com/api/oauth/profile` with the headers of the usage query, at most once every 24 hours per account (`checked` in `plans`). A failed request keeps the previous result, is only logged at debug level and does not affect the quota query. The raw value is `organization.organization_type` and `organization.rate_limit_tier` joined by a space: `claude_pro` is Pro (1); `claude_max` is Max 20x (20) with `20x` in the tier, Max 5x (5) with `5x`, and Max (5) with neither; `claude_team` and `claude_enterprise` are Team and Enterprise (1). Without `organization_type`, `account.has_claude_max` and `has_claude_pro` decide. On a Pro account on 2026-10-20 the values were `claude_pro` and `default_claude_ai`; the values for Max and Team come from known Claude Code usage and have not been measured.
- Other services have no plans and a default weight of 1.

### Passive data

CPA passes the upstream response headers in `ResponseHeaders` of the usage record:

| Service | Headers | Windows |
| --- | --- | --- |
| Claude | `Anthropic-Ratelimit-Unified-5h-Utilization` / `-5h-Reset`, same for `-7d-` | 5-hour and 7-day; the value is the used fraction, for example `0.2` |
| Claude | `Anthropic-Ratelimit-Unified-7d_oi-Utilization` / `-7d_oi-Reset` | Fable's 7-day window, written to the same Fable quota group as the usage API (`providers.FableGroupKey`). CPA also treats `7d_oi` as the Fable-specific window. Regular (non-Fable) requests do not carry these headers; whether Fable requests do has not been verified on an account that can use Fable. The plugin logs the first one it reads after each start |
| Codex | `X-Codex-Primary-Used-Percent`, `-Reset-At`, `-Reset-After-Seconds`, `-Window-Minutes`, same for `Secondary` | Classified by window length: 300 minutes is 5-hour, 10080 is 7-day, any other length is a window of its own; without a length, Primary is 5-hour and Secondary 7-day |

Antigravity, Kimi and Grok send no quota in their headers.

Passive data only updates the windows present in the headers; an active result replaces the whole group. Once an active query has described a group, passive data only updates the windows that query reported and adds none: the usage API of a ChatGPT Pro account reports only a 7-day window, yet the page showed a 5-hour window without a reset time stuck at 100%, which can only have come from the headers. A group no active query has described yet (after a start, or while queries fail) still takes every window from the headers. A window a service adds later appears with the next active query. `-Reset-After-Seconds` counts from `RequestedAt` of the usage record: the record arrives when the request completes, and counting from processing time would push the old cycle's reset time of a long request past the reset.

Skip rule (`shouldSkipActive`): services without quota in their headers are never skipped. For the others, a scheduled query is skipped when passive data arrived within `passive_skip_seconds` before it and the last active query is less than `passive_skip_max_minutes` old. Because only a short span before the slot counts, the longest gap between two data points is the interval plus `passive_skip_seconds`. Manual refreshes never skip.

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

Ignition uses `host.model.execute` with `auth_id` set to the credential's runtime ID and `forced_provider` set to the provider name CPA reports (`Cred.HostProvider`). Each service's `Ignition.Body` gives the body, which `providers.Request` encodes. Kimi and Grok have no ignition: a Kimi subscription's 5-hour and 7-day windows switch at fixed times counted from the subscription start, used or not, and Grok's weekly period starts when the previous one ends. The requests of the other services:

| Service | Entry protocol | Notable fields |
| --- | --- | --- |
| Codex | `openai-response` | `reasoning.effort: none`, `tools: []`, `max_output_tokens: 4` |
| Claude | `claude` | `max_tokens: 4` |
| Antigravity | `openai` | `max_tokens: 4`, `temperature: 0`, `reasoning_effort: none` |

### Model selection

CPA has no host callback that lists the models of one credential, so the plugin reads `GET /v1/models` with `models_api_key`. That list is the union of all credentials, deduplicated by model ID, and `owned_by` can name another service when two services serve the same ID. Candidates are therefore picked by name (each service's `Ignition.Candidates`; `providers.Candidates` removes duplicates and handles configured models):

- Codex: models containing `luna`, newest first, then other `gpt-` models; image models and `gpt-oss` excluded.
- Claude: non-thinking Haiku models starting with `claude-`, newest first, then Sonnet.
- Antigravity Gemini group: Flash newest first, then other non-Pro models, Pro last; image models excluded.
- Antigravity Claude / GPT group: Haiku, Sonnet, Opus (all non-thinking), GPT-OSS, then other non-thinking models.

"Newest first" compares the numeric segments of model names, so new releases need no code change. Every ignition starts with the newest candidate. Older models usually stay in the list long after a new release, so the order does not depend on the last successful model. A configured `model` or `models` entry is the only candidate and must be in the list.

When the pinned credential rejects a candidate, CPA answers locally with `auth_not_found` (the credential does not serve the model) or `unknown provider for model` (no credential does) without calling the provider. The plugin then tries the next candidate, up to 6. If every candidate is rejected, the error counts as high risk. While the model list cannot be read, the configured or last successful model is used.

CPA's model list comes from its built-in model table, which it refreshes periodically, and every credential of a service lists the same models. A new model in the list may therefore not be usable by every account. When the provider refuses a candidate with HTTP 400 or 404 (`ignite.ModelRejected`), the plugin tries the next candidate in the same ignition, once per ignition; a second refusal points at the request or the account and counts as high risk, and the error text names the first refused model and its status; when CPA rejects every remaining candidate locally, it includes the full refusal. 401, 403 and 429 do not depend on the model and do not trigger another model.

When the next candidate works, the refusal concerns the model for that quota group's account: `engine.noteRefused` remembers the group and model in memory and records a `model_refused` event. For the next 24 hours (`refusedFor`), automatic selection for that group skips the model (`engine.skipRefused`), then tries it once more. Repeated requests for a model the account cannot use may add account risk, while new models can reach accounts in stages, so the model is retried daily. The record is not written to `state.json` and is cleared when the plugin restarts. Configured models are not affected.

The model list is read after the plugin starts, after the CPA address or `models_api_key` changes (`engine.maybeReadModels`), and for every ignition. Quota polls do not read it; when one of the first two reads fails, it is retried with each poll until it succeeds. After each read, `engine.noteModels` computes the first automatic candidate of each service, ignoring configured models. The status API carries them in `next_models` (one per service that can be ignited), and the settings show each one after the hint of the ignition model field. The quota group is the service's `Ignition.HintGroup`: Antigravity uses the Gemini group, matching the "newest Flash" hint. `next_models` is kept in memory only. A failed read keeps the previous result; when the CPA address or `models_api_key` changes, `next_models` and the model list error are cleared and the list is read with the new address and key. `next_models` is computed per service and does not reflect models skipped for one quota group.

### Confirmation

After a 2xx response, the plugin confirms that the 5-hour window has a fixed future reset observed after the request:

1. Claude responses carry the new reset in their headers, so it is confirmed at once.
2. Codex quota arrives through the usage record of the same request; passive data keeps being applied while waiting.
3. Checks run after 5, 10, 15, 30 and 15 seconds, each preceded by one active query of the credential. Antigravity sends no quota in its headers and is confirmed by this step only.

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

A pause sends one notification (`circuit_notified_until_epoch` prevents repeats). A success or a fixed future reset clears the failure state.

## Notifications

`notify.Alerts.ProcessGroup` compares each window's remaining quota with the level last notified:

- Levels are normal, notice (≤50%), low (≤20%), critical (≤10%) and exhausted (≤0.01%). A worse level is notified; critical and exhausted use `timeSensitive`.
- A level counts as better only after a real recovery: the reset time moved to a new window, or the quota rose by 5 points or more since the previous reading (`RecoveryJump`). Usage endpoints and response headers can differ by a point, so a small rise within a window leaves the notified level unchanged, and a reading that bounces around a threshold alerts once. A better level only updates the baseline silently, so that the next drop alerts again.
- Recovery notifications and reset reminders take the mode for the window's kind from `recovery_notify` and `reset_reminder`. `notify.SevenDayClass` knows 5-hour and 7-day windows by their kind (`quota.Kind`); any other window counts as a 7-day window once it was seen more than a day before its reset (`long` in `state.json`), and as a 5-hour window otherwise.
- Recovery notifications follow cycle changes only (`cycleEnded`). When both reset times are known, the reset time must move to a new cycle, and either the old reset time has passed or the quota rose by `RecoveryJump`. Codex returns a reset time that moves forward with every query while a window is unused, so a changed reset time alone would count every query as a reset. Without a new reset time, a passed old reset time or a rise by `RecoveryJump` counts.
- Usage records arrive when a request completes, so a long request started before a reset delivers the old cycle's headers after it. A reading whose reset time has passed and belongs to an ended cycle (`ended_reset`) or precedes the current one is ignored; otherwise it would pull the state back into the old cycle and trigger another recovery.
- A detected change is stored as a pending recovery (`pending_recovery`) with the last reading of the ended cycle and whether that cycle ran out (`exhausted`). The last reading is the last one taken before its reset time (`cycle_reset`, `cycle_remaining`, `cycle_seen`), because some providers keep reporting the passed reset time with the new cycle's value for a while. A reading more than 15 minutes before the reset is not reported; a used-up window stays at 0 until its reset, so its reading never gets old. A cycle still at 100% before its reset is not notified: after the plugin was stopped for longer than a window, the moving reset time of an unused Codex window also looks passed. `after_exhausted` sends only used-up cycles. A failed delivery is retried at the next check and dropped after an hour. Recoveries and drops are sent as separate notifications.
- For every window with a reset time, whatever the notification settings, `engine.probes` queries the account once 30 seconds before and once 30 seconds after the reset (`probeOffset`), once per account and time; times within 10 seconds count as one. A query is skipped when a later reading already exists. The query after the reset runs only within 10 minutes of it (`probeLate`); an old reset still in the view after a restart waits for the next poll. `probes` only computes and marks nothing, so the loop can use it for the wake-up time; `runProbes` records the queries it runs.
- Reset reminders: 5-hour-kind windows are reminded once 1 hour before the reset (`reset_notice_1h_for`), 7-day-kind windows once 1 day before (`reset_notice_1d_for`). `has_remaining` reminds only when more than `critical_threshold` is left.
- Two reset times within 10 seconds belong to the same cycle.
- A window seen for the first time only records its baseline.
- A failed delivery (every channel failed) leaves the notified level unchanged, so the next check retries.

A Bark request is `GET {bark_url}/{title}/{body}?group&level&icon&url`, with title and body percent-encoded except RFC 3986 unreserved characters. A JSON `code` other than 200 is a failure. `icon` defaults to the repository's `assets/logo.png`; a `bark_icon` that still holds the old default (the CPA Management Center logo) is replaced with it on load.

### Channels

`engine.applyRuntimeConfig` builds the channels from `bark_url` and `webhook.url` when the config changes and hands them to `notify.Fanout`. `Fanout` sends to all channels at once and waits for all of them. When at least one channel succeeds, the message counts as delivered and each failed channel gets a `notify_failed` event whose `error` starts with the channel name; the event carries the label but no quota group key. Only when every channel fails is the delivery a failure, which the caller retries under the rules above. A message is not retried once a channel has it, because a retry would repeat it on the channels that already received it. Without any channel, `Send` returns `notify.ErrNotConfigured`, which means notifications are off: quota alerts update the notified level and the pending recoveries and reset reminders as if delivered, so setting up a channel later does not send old alerts; ignition pauses and cooldown notices log no `notify_failed`; Did Codex Reset records are not marked as seen, so those still worth sending go out once a channel is set up. When only a webhook is configured and its settings are invalid, `Fanout.Invalid` makes the send return the settings error, which counts as a failed delivery and is retried under the rules above.

`notify.NewWebhook` checks the webhook settings: the URL must be http or https, the method `GET`, `POST` or `PUT`, a `GET` request has no body, header names must be valid and unique regardless of case, header values must not contain line breaks or control characters once trimmed (so the line break that ends a YAML block scalar is dropped), and every placeholder must be in `notify.Placeholders`. Settings that fail the check leave the webhook off; the reason goes into `notify_error` in the status API, the page shows it as a notice, and the test returns it as a failed result. The rest of the config still applies.

Placeholders are escaped per message for where they appear: in the URL with the same percent-encoding as Bark; in the body JSON-string-escaped or form-encoded according to `Content-Type`, and as they are for other types; in headers with line breaks replaced by spaces. Without a `Content-Type` header, a body that is valid JSON once every placeholder is replaced by a word is sent as JSON and anything else as plain text, so text such as `[Lamplighter] {{text}}` is not taken for JSON. An empty body sends the built-in JSON (`source` plus every placeholder, in a fixed order). `{{priority}}` comes from the Bark level, `high` for `timeSensitive` and `normal` otherwise; `{{kind}}` comes from `Message.Kind`.

At most 64 KB of a webhook response is read. A non-2xx status is a failure. With `success_json`, the response must also be a JSON object whose listed top-level fields equal the expected values. The expected values go through a JSON encode and decode before the comparison, so a YAML integer `0` equals `0.0` in a response. Errors and test results drop the request URL that `*url.Error` adds and replace the URL, longer path segments and query values, header values, and longer strings of a JSON body (such as an ntfy topic) with `[redacted]`, because tokens usually sit there.

Notification texts are in `internal/notify/text.go`, one column each for Chinese and English, chosen by `language` in `state.json`. The management page reports the language: notifications are sent without a page open, so they use the language reported last, and Chinese until one has been reported. Window names in notifications are always `5h` and `7d`. The Did Codex Reset link points to the Chinese or English history page.

Did Codex Reset is read every `poll_seconds` seconds (at least 300, aligned to time boundaries), 10 latest records at a time. Records are deduplicated by ID, except `manual:` records, whose IDs can change and which are deduplicated by content; up to 100 seen keys are kept. The first run marks existing records as seen and notifies only the pending schedule when `notify_current_pending` is on.

Only records that are still news are sent; the rest are marked as seen. A schedule must have `scheduleState = pending` and a window that has not ended; a completed reset must be at most 48 hours old, measured from the latest of `completedAt`, `effectiveAt` and `announcedAt`. A fulfilled schedule leaves the `kind=all` list and comes back as `elapsed` when it loses its completion link, so old records can appear at any time. Every X post is one record, and a post and its reply each create a record for the same schedule. Schedules with the same reset type, window start and scope (`scope.plans`, `scope.windows`) are one schedule. Only the start is compared, because some records of a schedule carry just `effectiveAt` and others the whole `scheduleWindow`. As on the Did Codex Reset homepage, within one poll only the notifiable post with the latest `announcedAt` is sent, ties broken by record key, and the rest are marked as seen. A reply can enter the latest 10 one or two polls late, so each sent schedule is kept in `sent_events` (up to 100) with the post's `announcedAt` and the notification content, built in UTC and English. A later post about the schedule is sent again, titled "schedule updated", only when its `announcedAt` is later and its content (confidence, time, plans) differs; otherwise it is marked as seen.

The schedule time comes from `scheduleWindow`, or `effectiveAt` without one, shown in the plugin time zone: one time when start and end match, "by end" when `scheduleConstraint = deadline`, and "start–end" otherwise (date-level schedules). A date-level window covers a whole day in the source time zone and usually starts before the announcement, so its start alone reads as a time already past.

## Data

The data directory defaults to `data/lamplighter` in the plugin directory. `config.DefaultDataDir` reads `plugins.dir` from CPA's `config.yaml` (default `plugins`; a leading `~` expands to the home directory, a relative path is relative to CPA's working directory), the same rules CPA uses for the plugin directory. On every platform CPA loads the library from a copy in the temp directory `cliproxy-pluginhost/pid-<pid>/`, which does not survive a CPA restart or a container rebuild, so the data directory cannot follow the library's location.

| File | Content |
| --- | --- |
| `state.json` | `groups`: notification baselines per window; `scheduler`: ignition state per group; `codex_reset_updates`: seen records and sent schedules; `cooldown_notices`: stale cooldowns already notified; `language`: notification language; `plans`: the raw plan of each account and when it was last requested on its own. Written to a temporary file that replaces the old one |
| `history/YYYY-MM-DD.jsonl` | Quota samples and events, one file per UTC day |
| `instance.lock` | Instance lock |

Each history line is one JSON object. A sample is `{"k":"s","t":seconds,"g":group,"w":window,"r":remaining,"x":reset,"s":"active|passive"}`, an event is `{"k":"e","t":seconds,"g":group,"e":type,"v":level,"m":text,"l":label,"d":detail,"p":params}`. `m` and `d` are Chinese text for the CPA log; `p` holds the values the page builds the event text from: `model` and `reset` for `ignite` and `ignite_manual`, `retry_seconds`, `error` and an optional `cooldown` for `ignite_failed`, `until` and `error` for `ignite_paused`, `model`, `fallback` and `hours` for `model_refused`, `title` for `notify`, `title` and `error` for `notify_failed`, `until` for `cooldown_stale`, and `count` for `codex_reset`; times are RFC 3339 UTC. Active samples are always written. Passive samples are written only when the value or reset changes, at most once per minute per window; a newer value inside that minute waits in `pendingSamp`. For ranges over 2 days the history API keeps the last sample per bucket: 10 minutes over 2 days, 15 minutes over 4 days, 30 minutes over 8 days and one hour over 15 days, so that a series of these ranges has at most about 800 points (the chart is about 1000 pixels wide). Ranges up to 2 days are not bucketed. The response gives the bucket length in `bucket_seconds`. Each series has a `label` with the account suffix, such as `ChatGPT#rk`, and a `source_label` without it; the page merges the accounts of one quota by service and `source_label`.

Group keys are `<service>:<auth_index>:<group>`, for example `codex:3:codex:main`, `claude:3:claude:seven-day-fable` and `antigravity:4:antigravity:gemini-models`. The group key is also the ignition target ID.

## Design constraints

- Ignition goes only through `host.model.execute`, pinned to one credential; the plugin never builds a service's model request itself.
- Quota requests must leave through the same exit as CPA's model requests; when the exit cannot be determined, the request is skipped instead of connecting directly.
- Access tokens, `bark_url`, the `webhook` URL, headers and body, and `models_api_key` never appear in logs, events, the status API or on disk.
- The JSON fields of host callbacks follow `sdk/pluginapi` and `internal/pluginhost` of CPA v8.0.4; check the CPA source before changing them.
- Changes to `state.json` stay backward compatible: new fields have defaults, and missing old fields do not stop the plugin.
