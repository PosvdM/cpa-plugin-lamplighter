# 架构

[English](./architecture_EN.md)

Lamplighter 是用 Go 编写、以 `-buildmode=c-shared` 构建的 CPA 原生插件。CPA 加载动态库后调用 `cliproxy_plugin_init`，此后所有交互都是 JSON 信封形式的 RPC：CPA 调用插件的方法，插件通过宿主回调调用 CPA。

## 模块

| 路径 | 职责 |
| --- | --- |
| `main.go` | cgo 入口：导出 C ABI 函数，转发 RPC，实现宿主回调，计算默认数据目录 |
| `internal/plugin` | 插件 RPC 方法：注册、重新配置、暂停、管理接口、`usage.handle` |
| `internal/engine` | 后台循环：主动查询、被动数据、点火、通知、历史和状态页数据 |
| `internal/host` | 宿主回调的 Go 封装，以及 `host.http.do` 的超时控制 |
| `internal/config` | 插件配置解析、默认值和取值范围；读取 CPA 配置判断插件是否启用 |
| `internal/quota` | 三个服务的额度请求和解析，被动额度响应头解析 |
| `internal/egress` | 选择额度请求的网络出口 |
| `internal/models` | 读取 CPA 模型列表，排列点火候选模型 |
| `internal/ignite` | 点火时间计算、滑动重置判断、失败分类和失败保护 |
| `internal/notify` | Bark 发送、额度提醒、Did Codex Reset 转发 |
| `internal/store` | `state.json`、额度历史和实例锁 |
| `internal/web` | 管理页面，HTML、CSS、JS 分文件编辑，运行时合成一个文档 |

## 与 CPA 的接口

插件在 `plugin.register` 中返回 `schema_version` 6，并声明两项能力：

- `management_api`：注册管理接口和页面；
- `usage_plugin`：接收每个请求的用量记录，其中包含上游响应头。

管理接口（需要 CPA 管理密钥）：

| 方法和路径 | 作用 |
| --- | --- |
| `GET /v0/management/lamplighter/status` | 状态页数据，配置中的密钥只显示是否已设置 |
| `GET /v0/management/lamplighter/history?range=1h\|3h\|6h\|12h\|24h\|26h\|4d\|8d\|15d\|1mo\|36d` | 额度历史和点火事件，默认 `24h`。`1mo` 从插件时区上个月的同一日期（该月没有这一天时取最后一天）到现在 |
| `POST /v0/management/lamplighter/refresh` | 立即主动查询，`{"auth_index": "..."}` 只查一个账号 |
| `POST /v0/management/lamplighter/ignite` | 立即点火，`{"target": "<额度组 key>"}` |
| `POST /v0/management/lamplighter/test-bark` | 发送测试通知 |

页面注册为资源 `GET /v0/resource/plugins/lamplighter/page`，菜单名为 `Lamplighter`。资源本身不需要认证，页面中的数据请求都带管理密钥。页面从管理中心保存在 `localStorage` 的 `cli-proxy-auth` 中读取密钥（管理中心用主机名和 User-Agent 做了可逆混淆），读不到时让用户输入并只保存在 `sessionStorage`。设置通过 CPA 的 `PATCH /v0/management/plugins/lamplighter/config` 保存，该接口只合并顶层键，所以页面提交 `ignition`、`providers`、`codex_reset_updates` 时发送完整对象。

页面的视觉样式与管理中心一致：CSS 变量沿用管理中心 `src/styles/themes.scss` 的名称和取值（浅色、纯白、深色三套），额度条和图表按前两档提醒阈值着色（默认剩余高于 50% 绿色、高于 20% 黄色、其余红色），与通知标题的 🟡、🔴 一致，遥测数字使用等宽字体。页面与管理中心同源，主题直接读取父页面根元素的 `data-theme`，并监听其变化；单独打开时退回到管理中心保存在 `localStorage` 的 `cli-proxy-theme`。管理中心改了配色时，同步更新 `internal/web/page.css` 中的变量。

额度变化图表的数据处理都在页面中完成：

- **缺数据**：相邻样本间隔超过 3.5 个查询间隔（或接口的分段长度）时视为缺数据，画成斜线纹理，不延续前一个值。
- **多账号合计**：同一服务、同一 `source_label` 的多个账号合成一行，取各账号剩余百分比的平均值。各账号套餐相同时，这等于总额度的剩余比例；接口只提供百分比，无法按额度大小加权。详情图同时画出各账号的最低到最高范围。
- **排序**：默认按 `SERVICE_ORDER`（Claude、ChatGPT、Gemini、Fable、Claude / GPT）排列，其他额度按接口顺序排在后面；也可按当前剩余从低到高排列。
- **重置**：相邻样本的剩余上升 5 个百分点及以上视为重置。曲线在重置处断开，新的一段从重置后的值开始，图上标出重置时间；相隔一个样本的两次上升算一次重置。单个账号重置只让合计上升一部分，上升不足 5 个百分点时合计曲线不断开。
- **平滑**：同一窗口内额度只会下降，但主动查询和响应头的数值可能差 1 个百分点，取整方式也不同，样本会短暂回升。画线前把每段样本拟合成最接近的非递增序列（相邻违例合并取平均），否则每次回升都会在曲线上留下一个小缺口；悬停提示仍显示原始样本。曲线使用单调三次插值（Fritsch-Carlson），不会越过拟合后的值。额度按整数百分比上报，所以变化不超过 2 个百分点的台阶按连续下降连线；平台之后是更大的下降时，曲线先保持水平，再在下降处弯折。

使用的宿主回调：

| 回调 | 用途 |
| --- | --- |
| `host.auth.list` | 凭证列表，含 `auth_index`、运行时 ID、邮箱、停用状态，以及 CPA 冷却状态（`unavailable`、`next_retry_after`） |
| `host.auth.get` | 凭证文件 JSON，取 access token、Codex 账号 ID、Antigravity `project_id`、`proxy_url` |
| `host.model.execute` | 点火，带 `auth_id` 和 `forced_provider` |
| `host.http.operation_open` / `host.http.do` / `host.http.cancel` | 没有账号代理时的额度请求 |
| `host.log` | 写入 CPA 日志 |

宿主回调绑定在插件实例上，不依赖某次 RPC，所以后台循环可以随时调用。

## 后台循环

`engine.Engine` 用一个 goroutine 完成所有网络工作，避免查询、点火和通知交叉修改状态：

1. 插件注册后启动，先等 20 秒，让 CPA 加载凭证和执行器。
2. 打开数据目录，取得 `instance.lock`。
3. 每 30 秒读取一次 CPA 的 `config.yaml`，判断 `plugins.enabled` 和本插件的 `enabled`。CPA 停用插件时不会通知插件，所以插件自己检查；停用期间不做任何工作。读不到配置文件时视为启用。
4. 依次处理被动数据、Did Codex Reset、到点的主动查询、到期的点火、历史清理，并保存状态。
5. 计算下一次需要醒来的时间，在这之前等待新的被动数据、管理操作或配置变更。

`usage.handle` 和管理接口不直接执行网络请求：前者只解析响应头并放入队列（队列满时丢弃，丢一个被动样本没有影响），后者把操作交给后台循环并等待结果，最多 3 分钟。

后台循环崩溃时，`supervise` 记录日志并在 30 秒后重启。插件中任何未恢复的 panic 都会结束整个 CPA 进程，所以每个 goroutine 和每个 RPC 方法都有 `recover`。

### 生命周期

- `plugin.register` 和 `plugin.reconfigure` 解析配置并应用。无法解析的配置不生效，状态页显示错误，插件继续使用上一份配置。
- 新版本动态库加载后，CPA 对旧实例调用 `plugin.quiesce`，但不会卸载它。插件收到 `plugin.quiesce` 或 `plugin.shutdown` 后停止后台循环且不再启动。
- `instance.lock` 是数据目录中的 `flock` 文件锁，保证同一时间只有一个实例在查询、点火和推送。新实例拿不到锁时每 10 秒重试一次。

## 额度获取

### 主动查询

| 服务 | 请求 |
| --- | --- |
| Codex | `GET https://chatgpt.com/backend-api/wham/usage`，带 `Chatgpt-Account-Id` |
| Claude | `GET https://api.anthropic.com/api/oauth/usage`，`anthropic-beta: oauth-2025-04-20` |
| Antigravity | `POST .../v1internal:retrieveUserQuotaSummary`，依次尝试三个地址，正文为 `{"project": "<project_id>"}` |

请求头与 CPA 管理中心额度页相同，常量在 `internal/quota/active.go`。Codex 的 User-Agent 与 CPA 的 Codex 执行器一致；Claude 的 User-Agent 与 CPA 默认的 Claude Code 指纹一致。

token 来自凭证文件。CPA 在后台刷新 token 并写回文件，插件不自行刷新；上游返回 401 时跳过本轮，等 CPA 刷新。

查询时间对齐到绝对时间边界（`alignedAfter`），默认 300 秒间隔落在每小时的 `:00 / :05 / ... / :55`。不使用“上次时间加间隔”，以免时间点随启动时间漂移。

### 网络出口

`internal/egress` 让额度请求和 CPA 的模型请求使用同一个出口：

- 凭证有 `proxy_url` 时，用 CPA 的 `sdk/proxyutil` 建立连接，和 CPA 执行器的做法一致；`direct` / `none` 表示直连。无法解析时返回错误，不退回其他出口。
- 没有 `proxy_url` 时调用 `host.http.do`。CPA 依次使用全局 `proxy-url`、环境变量代理、直连。`host.http.do` 本身没有超时，插件先用 `host.http.operation_open` 打开一个操作，超时后用 `host.http.cancel` 取消。

CPA 的 `/api-call`（管理中心额度页使用）会忽略环境变量代理，而模型请求不会。插件按模型请求的规则选择出口。

### 被动数据

CPA 在用量记录的 `ResponseHeaders` 中提供上游响应头：

| 服务 | 响应头 | 窗口 |
| --- | --- | --- |
| Claude | `Anthropic-Ratelimit-Unified-5h-Utilization` / `-5h-Reset`，`-7d-` 同理 | 5 小时、7 天；值是已用比例，如 `0.2` |
| Codex | `X-Codex-Primary-Used-Percent`、`-Reset-At`、`-Reset-After-Seconds`、`-Window-Minutes`，`Secondary` 同理 | 按窗口长度区分，300 分钟为 5 小时，10080 分钟为 7 天 |

被动数据只更新响应头里有的窗口，主动查询结果替换整个额度组。

跳过规则（`shouldSkipActive`）：查询时刻前 `passive_skip_seconds` 秒内有被动数据，且距上次主动查询不足 `passive_skip_max_minutes` 分钟时跳过。只看查询时刻之前的一小段时间，最坏情况下两次数据间隔为查询间隔加 `passive_skip_seconds`。手动刷新不跳过。

## 点火

### 时间

`ignite.Schedule.DueAt` 按以下规则给出下一次点火时间：

1. 当天 `start_hour` 之前：等到 `start_hour`。
2. 超过 `end_hour + end_grace_minutes`：等到第二天 `start_hour`。
3. 5 小时窗口是滑动占位：立即（受冷却和重试时间限制）。
4. 重置时间在未来：`reset + grace_seconds`；不在当天时段内时等到第二天。
5. 重置时间已过或未知：立即（受冷却和重试时间限制）。

同一额度组中有 5 小时以外的窗口用完（剩余 ≤ 0.01%）且尚未到重置时间时，定时点火跳过这个额度组，不尝试、不计失败、不推送；引擎在该窗口的重置时间醒来，由下一次查询确认额度是否恢复。页面在点火状态中显示被哪个窗口挡住（`blocked_window`、`blocked_until`）。手动点火不受此限制。

**滑动占位**：有些服务在窗口尚未开始时返回“当前时间加 5 小时”的重置时间，并且每次查询都向后移动。`ignite.Observe` 用连续两次主动观测判断：重置时间距当前约 5 小时（误差 120 秒），且两次观测间的移动量接近经过的时间（误差为经过时间的 2%，在 3 到 10 秒之间）。被动数据来自真实请求，请求本身会开始窗口，所以被动观测永远不是滑动占位。看到固定的未来重置时间会清空失败状态。

### 请求

点火通过 `host.model.execute` 发送，`auth_id` 为凭证的运行时 ID，`forced_provider` 为服务名。请求体见 `ignite.BuildRequest`：

| 服务 | 入口协议 | 请求要点 |
| --- | --- | --- |
| Codex | `openai-response` | `reasoning.effort: none`，`tools: []`，`max_output_tokens: 4` |
| Claude | `claude` | `max_tokens: 4` |
| Antigravity | `openai` | `max_tokens: 4`，`temperature: 0`，`reasoning_effort: none` |

### 模型选择

CPA 没有按凭证列出模型的宿主回调，插件用 `models_api_key` 读取 `GET /v1/models`。这个列表是所有凭证模型的合集，按模型 ID 去重，`owned_by` 在两个服务提供同一 ID 时可能指向另一个服务，所以候选只按名称挑选（`models.Candidates`）：

- Codex：名称含 `luna` 的模型由新到旧，然后其他 `gpt-` 模型；排除图片模型和 `gpt-oss`。
- Claude：`claude-` 开头的非 thinking Haiku 由新到旧，然后 Sonnet。
- Antigravity Gemini 组：Flash 由新到旧，然后其他非 Pro 模型，Pro 最后；排除图片模型。
- Antigravity Claude / GPT 组：Haiku、Sonnet、Opus（均为非 thinking）、GPT-OSS、其他非 thinking 模型。

“由新到旧”比较模型名中的数字段，新版本无需改代码。上次成功的模型排在最前。配置了 `model` 或 `models` 时只用指定模型，且它必须在列表中。

候选模型被锁定的凭证拒绝时，CPA 在本地返回 `auth_not_found`（凭证不支持该模型）或 `unknown provider for model`（没有凭证支持），不会请求上游；插件换下一个候选，最多 6 个。全部被拒绝时按高风险错误处理。模型列表暂时读不到时，使用指定模型或上次成功的模型。

### 确认

点火请求返回 2xx 后，插件确认 5 小时窗口在请求之后观测到了固定的未来重置时间：

1. Claude 的响应头里直接带新的重置时间，立即确认。
2. Codex 的额度信息通过同一请求的用量记录到达；等待期间继续处理被动数据。
3. 按 5、10、15、30、15 秒的间隔检查，每次检查前主动查询该凭证一次。

全部检查都没有确认时，返回 `ErrNotConfirmed`。

### 失败分类和保护

`ignite.Classify`：

| 类别 | 条件 | 处理 |
| --- | --- | --- |
| 换模型 | `auth_not_found`、`unknown provider for model` | 换下一个候选 |
| 高风险 | `ErrNotConfirmed`、`ErrNoModel`，HTTP 400/401/403/404/409/422/429，`not found`、`unsupported` 等 | 立即暂停到第二天 `start_hour` |
| 临时 | CPA 本地冷却（`are cooling down`）、`auth_unavailable`，其他错误 | 按 `failure_retry_seconds × multiplier^(n-1)` 重试，第 `max_transient_failures` 次暂停到第二天 |

CPA 本地冷却带有 `reset_seconds` 且不超过 10 分钟时（`ignite.CooldownWait`），不计入失败次数，在冷却结束后再等 `grace_seconds` 重试。CPA 在 429 之后把凭证冷却到上游重置时间再加十几秒，额度在重置前用完时，重置后 3 秒的点火会撞上这段冷却。连续第二次冷却按普通临时错误计数，避免冷却不断续期时无限重试。

**过期的 CPA 冷却**：CPA 收到账号级 429 后，把凭证冷却到上游报告的重置时间（7 天额度用完时可达数天），额度提前恢复（如重置卡）时不会自动解除。每轮查询后，`engine.checkCooldowns` 检查每个凭证：CPA 冷却还剩 10 分钟以上，且某个带 5 小时窗口的额度组在最近 15 分钟内的数据显示所有窗口都没有用完，就认为冷却已过期。每个冷却结束时间只推送一次（`state.json` 的 `cooldown_notices`），并记录 `cooldown_stale` 事件；状态接口的账号带 `cooldown_until` 和 `stale_cooldown`。插件不替用户清除冷却，因为那需要 CPA 管理密钥，并且会改变 CPA 的路由状态。

暂停时推送一次 Bark（`circuit_notified_until_epoch` 防止重复），成功或看到固定的未来重置时间后清空失败状态。

## 通知

`notify.Alerts.ProcessGroup` 比较每个窗口当前剩余额度和已通知等级：

- 等级为 normal、notice（≤50%）、low（≤20%）、critical（≤10%）、exhausted（≤0.01%）。等级变差时推送；critical 和 exhausted 使用 `timeSensitive`。
- 等级变好时，开启 `notify_recovery` 才推送；关闭时静默更新基线，否则下一轮的下降不会再提醒。
- 开启 `notify_reset_reminders` 时，任意窗口重置前 1 小时、7 天窗口重置前 1 天各提醒一次。
- 两个重置时间相差不超过 10 秒视为同一周期。
- 第一次看到某个窗口只记录基线，不推送。
- 推送失败时不更新已通知等级，下一次检查会重试。

Bark 请求为 `GET {bark_url}/{标题}/{正文}?group&level&icon&url`，标题和正文除 RFC 3986 非保留字符外全部百分号编码，返回 JSON 中 `code` 不为 200 视为失败。

Did Codex Reset 每 `poll_seconds` 秒（最少 300 秒，对齐时间边界）读取最新 10 条记录。非 `manual:` 记录按 ID 去重，`manual:` 记录的 ID 可能变化，按内容去重；最多保留 100 个已见记录。第一次运行把现有记录标为已见，只在 `notify_current_pending` 开启时推送当前待生效的排期。

## 数据

数据目录默认是插件目录下的 `data/lamplighter`。`main.go` 用 `dladdr` 找到动态库路径；动态库位于 `<插件目录>/<goos>/<goarch>/` 时去掉这两层。

| 文件 | 内容 |
| --- | --- |
| `state.json` | `groups`：每个窗口的通知基线；`scheduler`：每个额度组的点火状态；`codex_reset_updates`：已见记录；`cooldown_notices`：已提醒的过期冷却。写入临时文件后替换 |
| `history/YYYY-MM-DD.jsonl` | 按 UTC 日期分文件的额度样本和事件 |
| `instance.lock` | 实例锁 |

历史记录每行一个 JSON：样本为 `{"k":"s","t":秒,"g":额度组,"w":窗口,"r":剩余,"x":重置,"s":"active|passive"}`，事件为 `{"k":"e","t":秒,"g":额度组,"e":类型,"v":级别,"m":文本}`。主动样本每次都写；被动样本只在数值或重置时间变化时写，同一窗口每分钟最多一次，期间的新值暂存在 `pendingSamp`，满一分钟后写入。历史接口对超过 2 天的范围按 5 分钟、超过 8 天的范围按 30 分钟取每段最后一个样本。每个系列带有 `label`（含账号后缀，例如 `ChatGPT#rk`）和 `source_label`（不含后缀），页面按服务和 `source_label` 合并同一额度的多个账号。

额度组 key 为 `<服务>:<auth_index>:<分组>`，例如 `codex:3:codex:main`、`claude:3:claude:seven-day-fable`、`antigravity:4:antigravity:gemini-models`。它同时是点火目标 ID。

## 设计约束

- 点火只通过 `host.model.execute` 发送，并锁定到具体凭证；不要在插件中拼装服务的模型请求。
- 额度请求的出口必须与 CPA 模型请求一致；出口无法确定时跳过请求，不能退回直连。
- 不在日志、事件、状态接口或磁盘中输出 access token、`bark_url` 和 `models_api_key`。
- 宿主回调的 JSON 字段以 CPA v8.0.4 的 `sdk/pluginapi` 和 `internal/pluginhost` 为准；修改前对照 CPA 源码。
- 修改 `state.json` 结构时保持向后兼容：新字段使用缺省值，旧字段缺失时继续运行。
