# 架构

[English](./architecture_EN.md)

Lamplighter 是用 Go 编写、以 `-buildmode=c-shared` 构建的 CPA 原生插件。CPA 加载动态库后调用 `cliproxy_plugin_init`，此后所有交互都是 JSON 信封形式的 RPC：CPA 调用插件的方法，插件通过宿主回调调用 CPA。

## 模块

| 路径 | 职责 |
| --- | --- |
| `main.go` | cgo 入口：导出 C ABI 函数，转发 RPC，实现宿主回调 |
| `internal/plugin` | 插件 RPC 方法：注册、重新配置、暂停、管理接口、`usage.handle` |
| `internal/engine` | 后台循环：主动查询、被动数据、点火、通知、历史和状态页数据 |
| `internal/host` | 宿主回调的 Go 封装，以及 `host.http.do` 的超时控制 |
| `internal/config` | 插件配置解析、默认值和取值范围；读取 CPA 配置判断插件是否启用、计算默认数据目录 |
| `internal/providers` | 每个服务一个文件：额度请求和解析、响应头解析、点火请求、候选模型、默认设置；注册表，见下文“服务” |
| `internal/quota` | 额度窗口和上游请求的类型，窗口种类判断，时间解析 |
| `internal/egress` | 选择额度请求的网络出口 |
| `internal/models` | 读取 CPA 模型列表，提供按新旧排列候选模型的工具 |
| `internal/ignite` | 点火时间计算、滑动重置判断、失败分类和失败保护 |
| `internal/notify` | Bark 和 webhook 发送、额度提醒、Did Codex Reset 转发 |
| `internal/release` | 读取 GitHub 上的最新 Release，比较版本号 |
| `internal/store` | `state.json`、额度历史和实例锁 |
| `internal/web` | 管理页面，HTML、CSS、JS 分文件编辑，运行时合成一个文档 |

## 与 CPA 的接口

插件在 `plugin.register` 中返回 `schema_version` 6，并声明两项能力：

- `management_api`：注册管理接口和页面；
- `usage_plugin`：接收每个请求的用量记录，其中包含上游响应头。

管理接口（需要 CPA 管理密钥）：

| 方法和路径 | 作用 |
| --- | --- |
| `GET /v0/management/lamplighter/status` | 状态页数据，配置中的密钥只显示是否已设置。`providers` 按显示顺序列出支持的服务（`id`、`title`、能否点火 `ignite`、中英文的模型提示 `model_hint`、可勾选的点火额度组 `groups` 及说明 `groups_hint`），`group_order` 是额度组的显示顺序；每个窗口带 `kind`（`5h` 或 `7d`，其他长度的窗口没有这个字段） |
| `GET /v0/management/lamplighter/history?range=1h\|3h\|6h\|12h\|24h\|26h\|4d\|8d\|15d\|1mo\|36d&window=5h\|7d` | 额度历史和点火事件，默认 `24h`。`window` 为 `5h` 或 `7d` 时只返回这种窗口的系列，省略或为其他值时返回全部窗口；点火事件不按窗口筛选。5 小时额度按小时、7 天额度按天，依次是：1 个单位；半个窗口加 1（`3h`、`4d`）；一个窗口加 1（`6h`、`8d`）；半天或半月，能完整显示两个窗口（`12h`、`15d`）；一天或一个月（`24h`、`1mo`）；五个窗口加 1（`26h`、`36d`）。`1mo` 从插件时区上个月的同一日期（该月没有这一天时取最后一天）到现在 |
| `POST /v0/management/lamplighter/refresh` | 立即主动查询，`{"auth_index": "..."}` 只查一个账号 |
| `POST /v0/management/lamplighter/ignite` | 立即点火，`{"target": "<额度组 key>"}` |
| `POST /v0/management/lamplighter/test-notify` | 同时向已配置的渠道发送测试通知，`results` 中每个渠道一项：`channel`、`ok`、`error`，webhook 还有 `response`（响应的前 200 字）和 `unchecked`（没有设置 `success_json`，只检查了 HTTP 状态码） |
| `POST /v0/management/lamplighter/language` | 记录通知语言，`{"language": "zh-CN"}`；中文的各种写法记为 `zh`，其他语言记为 `en` |

页面注册为资源 `GET /v0/resource/plugins/lamplighter/page`，菜单名为 `Lamplighter`。资源本身不需要认证，页面中的数据请求都带管理密钥。页面从管理中心保存在 `localStorage` 的 `cli-proxy-auth` 中读取密钥（管理中心用主机名和 User-Agent 做了可逆混淆），读不到时让用户输入并只保存在 `sessionStorage`。设置通过 CPA 的 `PATCH /v0/management/plugins/lamplighter/config` 自动保存：表单的 `change` 事件触发保存，每次只提交改动字段所在的顶层键。该接口只合并顶层键，所以 `ignition`、`providers`、`recovery_notify` 等对象以已保存的对象为基础整体提交。保存依次进行，后一次以前一次写入的配置为基础；数字不满足输入框的范围或步长时不保存。浏览器会把为本站保存的密码（通常是管理密钥）填进密码输入框。设置中只有 `models_api_key` 是密码框，它的值按以下规则保存：输入框没有焦点时由浏览器填入（匹配 `:autofill` 或 `:-webkit-autofill`）的值来自页面加载时的自动填入，不保存也不提示；与当前管理密钥相同的值不保存，并提示这是管理密钥；其他非空值先用它请求 `GET /v1/models`（插件读取模型列表的请求），返回 401 或 403 时不保存并提示 CPA 不接受这个 key，其他结果（包括请求失败）照常保存。不保存时输入框恢复为已保存的值。该输入框还带有让 1Password、Bitwarden 和 LastPass 跳过它的属性。`models_api_key` 输入框设为 `autocomplete="one-time-code"`：Chrome 的密码管理不把这类字段当作密码，表单中没有其他密码框时只把它作为备用字段，不在页面加载时自动填入，也不在修改后弹出保存或更新密码的提示。`autocomplete="off"` 对密码框不起作用，`new-password` 会让 Chrome 提示生成强密码，所以都不使用。`webhook.headers` 和 `webhook.success_json` 在页面上以文本编辑（每行一个 `名称: 值`、JSON 对象），无法解析时不保存。状态接口只返回 webhook 的地址、请求头和请求体是否已设置，页面从 CPA 的插件配置接口读取原值填入表单。最后一次保存 3 秒后，页面重新读取状态，把保存过的字段填成插件应用后的值，正在编辑的字段不回填；保存失败时恢复为已保存的值。测试推送等待未完成的保存后再发送。

账号卡片上的“命名”用 CPA 的 `PATCH /v0/management/auth-files/fields` 写入凭证的 `note`：替换备注的第一行，保留其余各行，再调用 `refresh` 查询这个账号，使插件立即读到新名称。CPA 保存备注时去掉首尾空白，第一行为空的备注会以下一行为名称，所以有多行的备注清空名称时，确认后删除整条备注。额度查询失败不影响改名，只是不更新页面，所以 `refresh` 的结果不作为改名的结果显示。同一服务有多个账号时，卡片上还有“权重”：输入大于 0 的数字后，页面把整个 `account_weights` 对象通过插件配置接口保存（CPA 只合并顶层键），留空则删除这个账号的权重，并立即按新值重画；`config.Parse` 丢弃不大于 0 或无穷大的权重。

页面标题旁的图标从 GitHub 加载仓库中的 `assets/logo.png`，更换图标不需要发布新版本；加载失败时不显示。

页面文字都在 `page.js` 的 `I18N` 词典中，中文和英文各一份；`page.html` 中的静态文字用 `data-i18n`（以及 `data-i18n-placeholder`、`data-i18n-title`、`data-i18n-aria-label`）标出词条，其中的中文原文只在脚本应用语言前显示。语言跟随管理中心：先读父页面根元素的 `lang`（管理中心每次切换语言都会更新它）并监听变化，单独打开时读 `localStorage` 的 `cli-proxy-language`，再退回到浏览器语言；`zh` 开头的显示中文，其余显示英文。状态接口的 `language` 与页面语言不同时，页面调用 `POST .../language` 上报，失败时在下一次刷新状态时重试。

事件文字由页面按事件的 `params` 用当前语言拼出（见下文“数据”）。没有 `params` 的旧事件显示存下的中文原文。上游返回的错误原文和推送标题不翻译：推送标题本来就是按通知语言生成的。

页面的视觉样式与管理中心一致：CSS 变量沿用管理中心 `src/styles/themes.scss` 的名称和取值（浅色、纯白、深色三套），额度条和图表按前两档提醒阈值着色（默认剩余高于 50% 绿色、高于 20% 黄色、其余红色），与通知标题的 🟡、🔴 一致，遥测数字使用等宽字体。页面与管理中心同源，主题直接读取父页面根元素的 `data-theme`，并监听其变化；单独打开时退回到管理中心保存在 `localStorage` 的 `cli-proxy-theme`。管理中心改了配色时，同步更新 `internal/web/page.css` 中的变量。

管理中心把插件页面的 iframe 铺满内容区，顶部不留空白，右上角的浮动工具栏（178 × 52 px，距窗口顶边和右边各 24 px）盖在 iframe 上。窗口宽度在 768 px 及以下时管理中心使用手机布局：侧栏收起，iframe 与窗口同宽，工具栏为 162 × 48 px、距边缘 12 px，左上角另有一个 48 × 48 px 的菜单按钮。页面在 iframe 中打开时（`window.self !== window.top`，不需要读取父页面，所以管理中心与插件不同源时也生效）给根元素加 `embedded` 类：标题行右侧留出工具栏的宽度，放不下时换行；“立即刷新全部”放在“额度”标题行的右侧，这一行从工具栏下方 8 px 开始；宽度在 768 px 及以下时整个页面从工具栏和菜单按钮下方开始；宽度超过 1584 px 时工具栏落在页面边距中，不做调整。这些数值来自管理中心的 `src/styles/layout.scss`（`.main-header .header-actions`、`.mobile-sidebar-actions`），管理中心改了这些控件的位置或大小时，同步更新 `page.css` 中的对应规则。

额度变化图表的数据处理都在页面中完成：

- **时间轴**：1、3 小时和 1、4 天的横轴右端是现在。从 6 小时、8 天起（5 小时额度的 6、12、24、26 小时，7 天额度的 8、15 天、1 个月和 36 天），横轴总长仍等于所选范围，右端是所选行当前窗口的重置时间。合计行取各账号中最早的下一次重置，即行右侧 ↻ 的时间；已经过去的重置时间（窗口还没有重新开启，或之后没有查询到）不算。例如 6 小时显示当前 5 小时窗口和上个窗口的最后 1 小时。`RANGES` 中的 `24h` 既是 5 小时额度的 24 小时，也是 7 天额度的 1 天，只有前者延伸（`extendsAhead`）。历史接口返回到现在为止的范围，页面不画横轴左端之前的部分。切换所选行时整张图平移；时间流逝时横轴不动，窗口重置后右端移到新窗口的重置时间。所选行没有重置时间、重置时间已过（5 小时窗口还没有被新请求开启），或重置时间离现在超过所选范围时，右端是现在。现在之后的部分画成与缺数据相同的斜纹，不画表示现在的竖线。概览图和详情图共用这条横轴。
- **缺数据**：相邻样本间隔超过 3.5 个查询间隔（或接口的分段长度）时视为缺数据，画成斜线纹理，不延续前一个值。启用被动跳过时，阈值至少为 `passive_skip_max_minutes` 加 1.5 个查询间隔：响应头里没有的窗口（如没在使用的 Fable）在跳过期间只能等下一次主动查询，这段间隔不算缺数据。
- **多账号合计**：同一服务、同一 `source_label` 的多个账号合成一行，取各账号剩余百分比按权重的加权平均值（`pooled`），权重与各账号的额度大小成比例时，这等于总额度的剩余比例。合计行的当前值、曲线和延长线都用加权值；详情图的阴影仍是各账号的最低到最高。权重依次取 `account_weights` 中按凭证文件名（状态接口中账号的 `name`）设置的值、状态接口中账号的 `default_weight`，都没有时为 1（`accountWeight`）；权重全为 1 时就是简单平均。
- **窗口种类**：图表按历史系列的 `kind` 分到“5 小时”和“7 天”两个标签页，只显示账号实际报告的种类（一个都不知道时两个都显示），默认打开第一个；其他长度的窗口不画进图表。同一额度名下有自己标签的窗口（例如 Claude 的 `7 Day Opus`）单独成行，不与主窗口合并。
- **排序**：默认按状态接口的 `group_order`（Claude、ChatGPT、Gemini、Kimi、Grok、Fable、Claude / GPT，来自 `internal/providers/provider.go` 的 `groupOrder`）排列，其他额度按接口顺序排在后面；也可按当前剩余从低到高排列。状态接口中账号按服务的注册顺序排列，同一账号的额度组也按这个顺序排列，所以账号卡片上 Gemini 在 Claude / GPT 之前、Claude 在 Fable 之前。
- **记住筛选**：`remember_chart_filters` 开启时，页面把所选窗口、排序和每个窗口各自的时间范围存在 `localStorage` 的 `lamplighter-chart-filters` 中，打开页面时恢复；5 小时和 7 天的时间范围选项不同，所以分开保存。保存的值不在当前选项中时使用默认值。开关读取设置表单中的勾选状态，所以改动在保存前就生效；关闭时删除这项记录。
- **加载**：页面按所选窗口请求历史（`window` 参数），并按窗口和时间范围缓存响应。切换到缓存过的组合时立即画出缓存的数据，同时重新请求，响应到达后重画；没有缓存时图表变灰，等响应到达。只有最近一次加载的响应会画到图上，切换后才到达的旧响应只存入缓存；同一组合已有请求在进行时，加载沿用这个请求，刷新和点火后的加载除外。请求失败且没有所选组合的数据时，图表显示错误信息，不再显示其他组合的旧图。页面打开、第一张图加载成功后，预先请求另一个窗口的时间范围，切换窗口时不用等待。页面每 30 秒读取一次状态；某个窗口的 `observed_at` 晚于图表中该窗口的最后一个样本，并且来自主动查询或剩余值与最后一个样本不同时（数值不变的被动读数不写入历史），重新请求当前图表（`refreshHistory`）。鼠标在图表卡片内时只保存新数据，移开后再重画，以免替换鼠标下的竖线和提示。
- **重置**：相邻样本的剩余上升 5 个百分点及以上视为重置。曲线在重置处断开，前一段保持最后的值画到重置样本的时间，新的一段从重置后的值开始，图上标出重置时间；相隔一个样本的两次上升算一次重置。单个账号重置只让合计上升一部分，上升不足 5 个百分点时合计曲线不断开。
- **匀速参考线**：详情图为每个周期画一条虚线，从周期开始时的 100% 线性降到重置时的 0%，悬停提示同时给出该时刻的参考值。详情图标题行依次显示当前剩余、此刻的匀速参考值和两者的差值（高于参考值为绿色，低于为红色）、距重置的时间。横轴右端是现在时，图表右侧在曲线末端的高度标出当前剩余，在参考线末端的高度标出参考值，差值标在两者中间，一条竖线连接两个末端；两个数值离差值不足一行（`LABEL_H`）时向上下推开，差值保持在中间。单个账号的周期终点是图上检测到的重置和状态接口中当前窗口的 `reset`（两者相差不到半个窗口时算同一次重置），起点是终点减去窗口长度（5 小时或 7 天），但不早于上一个周期的终点。起点不取上一次重置的时间：5 小时窗口从重置后的第一个请求开始计时，两个窗口之间可能空出一段，这段时间不属于任何周期。重置落在缺数据的区间里时检测不到，这个周期不画线；按固定间隔推算周期会把空出的时间算错，所以不推算。合计行的参考值是各账号参考值按权重的平均值（`rowPace`），各账号的周期按上述方法得到，只计入该时刻处在周期内的账号，所以参考线在任一账号的周期起止处折弯或跳变。直接把各账号的重置时间连起来，每次重置前参考值都接近 0%，差值就失去了意义。合计曲线自己检测到的重置不用于参考线：一个账号重置时，合计可能上升不足 5 个百分点。
- **平滑**：同一窗口内额度只会下降，但主动查询和响应头的数值可能差 1 个百分点，取整方式也不同，样本会短暂回升。画线前把每段样本拟合成最接近的非递增序列（相邻违例合并取平均），否则每次回升都会在曲线上留下一个小缺口；悬停提示仍显示原始样本。曲线使用单调三次插值（Fritsch-Carlson），不会越过拟合后的值。额度按整数百分比上报，所以变化不超过 2 个百分点的台阶按连续下降连线；平台之后是更大的下降时，曲线先保持水平，再在下降处弯折。
- **概览条带的颜色**：条带按每个周期内的累计最低值着色，后面的样本不高于此前的最低值，遇到重置重新开始。Claude 响应头的数值总比主动查询高 1 个百分点（主动 50、被动 51），读数在阈值附近时，按原始样本着色会让条带短暂变色，留下细缝。这里不用平滑曲线的非递增拟合，它会把 50 和 51 平均成 50.5，变色的时间反而更长。详情曲线和悬停提示不受影响，悬停仍显示原始样本。
- **延长线**：横轴右端是重置时间、且最近的数据没有中断时，详情图从当前的点画一条点线（`projection`）。先降到 0% 时停在交点，在时间轴上标出用完的时间，7 天额度还标出日期，被它盖住的刻度标签不显示；先到重置时停在重置处，在图表右侧标出剩余额度；速度不是下降时画水平线。5 小时和 7 天额度用不同的速度，原因见下文“延长线的评估”。
  - 5 小时额度用当前速度（`currentRate`），表示照这个速度用下去的结果。速度取自本周期的样本（`floor` 最后一次上升之后）：先取累计最低值（与概览条带相同），再做自底向上的分段线性拟合，用最后一段的斜率（`recentSlope`）。拟合从每两个相邻样本一段开始，相邻两段合成一条最小二乘直线后，所有样本离直线都不超过 2 个百分点（`QUANTUM`）时合并，误差最小的先合并。额度按整数百分比上报，样本是台阶状的，相邻两个样本之间的斜率不是 0 就是很陡，所以不直接求导；回看多长由分段决定，用量变化处会切开。被动样本最多晚一分钟写入，最后一个被动样本超过 2 分钟（`HOLD_AFTER`）没有变化时，才把它的值延续到现在参与拟合，停用之后延长线逐渐变平。主动样本每个查询间隔写入一次，最后一个主动样本要超过 1.5 个采样间隔才延续到现在，否则每个查询间隔的后半段延长线都会变平。突然大量使用后的头几分钟，最后一段只有几个样本，估计的用完时间偏早，之后逐渐后移。
  - 7 天额度用本周期日均（`cycleRate`）：本周期已用的额度（100 减去 `floor` 的最后一个值）除以周期开始以来的时间。周期起点是状态接口中当前窗口的 `reset` 减去 7 天，不用样本中检测到的重置。没有重置时间、重置时间已过、周期还没开始，或最后一个样本早于周期起点时不画。合计行取各账号日均速度按权重的平均值，每个账号用自己的重置时间，只计入满足上述条件的账号。
  - 延长线的评估：用 2026-10-03 到 10-20 的全部历史逐周期回放，每个时刻只用当时已有的样本预测重置时的剩余额度，再与实际值比较。7 天额度（两个周期）用最后一段斜率的平均误差为 20.0 个百分点，用固定回看 24、48 小时为 13.5、12.3，用本周期日均为 9.9。7 天额度通常白天使用、夜间停用，最后一段只反映当晚的集中使用，用完时间估计得过早；固定回看时间同样受使用节奏影响，所以不用。5 小时额度（69 个周期）的各种外推都不比“之后不再使用”准确，之后还会用多少基本无法预测，所以保留当前速度。回放期间没有一次用到 0%，用完时间的准确度没有评估。
- **悬停**：在概览图或详情图上悬停时，竖线吸附到详情图当前行（多账号为合计线）离鼠标最近的采样点，提示中的时间和各行数值都取该时刻；采样时间不同的其他行显示该时刻之前最近的样本。最近的采样点也超过缺数据阈值时，竖线停在鼠标所在的时间。在现在右侧悬停时，竖线跟随鼠标，详情图的提示显示该时刻延长线的值和匀速参考值，概览图不显示提示。

使用的宿主回调：

| 回调 | 用途 |
| --- | --- |
| `host.auth.list` | 凭证列表，含 `auth_index`、运行时 ID、邮箱、备注（`note`）、停用状态，以及 CPA 冷却状态（`unavailable`、`next_retry_after`） |
| `host.auth.get` | 凭证文件 JSON，取 access token、Codex 账号 ID、Antigravity 的 `project_id` 和 `user_agent`、Kimi 的 `domain` 和 `base_url`、Grok 的 `sub`、`proxy_url` |
| `host.model.execute` | 点火，带 `auth_id` 和 `forced_provider` |
| `host.http.operation_open` / `host.http.do` / `host.http.cancel` | 没有账号代理时的额度请求，以及读取 Antigravity Hub 更新清单 |
| `host.log` | 写入 CPA 日志 |

宿主回调绑定在插件实例上，不依赖某次 RPC，所以后台循环可以随时调用。

每轮主动查询开始时，`engine.loadCreds` 读取凭证列表，重新计算每个账号的后缀（`Cred.Suffix`）：备注第一行（`accountName`，最长 `maxNameRunes` 个字符）不为空时为 `#` 加名称，与同一服务另一个账号的后缀相同时（例如名称重复）再加邮箱后缀；没有名称时，同一服务有多个账号才加邮箱后缀（`maskedSuffix`）。额度组的 `Label` 随之更新。告警状态按额度组 key 记录，改名不会重新发送通知。

## 服务

`internal/providers` 中每个服务是一个 `providers.Provider`，在 `provider.go` 的 `registry` 中按显示顺序注册：ChatGPT（`codex`）、Claude、Antigravity、Kimi、Grok（`xai`）。服务定义包含：

| 字段 | 内容 |
| --- | --- |
| `ID`、`Aliases`、`Title` | 配置键和额度组 key 使用的服务名；CPA 中的其他写法（Kimi 的 `kimi-ai`、`kimi.ai`、`kimi.com`，Grok 的 `x-ai`、`x.ai`、`grok`）；显示名 |
| `Monitor`、`Ignite` | 默认是否监控、点火 |
| `Fetch` | 主动查询，返回额度组 |
| `Headers` | 解析模型请求的响应头；为空表示响应头里没有额度，被动跳过对该服务不生效 |
| `Ignition` | 点火协议、请求体、候选模型、模型提示、可勾选的额度组；为空表示不点火，设置页显示“不支持点火” |

`providers.Lookup` 按服务名或别名查找（忽略大小写，`_` 视同 `-`）。凭证列表中的服务名换成 `ID` 存进 `Cred.Provider`，原始写法存进 `Cred.HostProvider`。配置中用别名写的服务键在解析时换成 `ID`。

**窗口种类**：`quota.Window.Period` 是窗口长度，由各服务从接口数据填入：Codex 的 `limit_window_seconds` 和 `Window-Minutes`，Claude 的字段名，Antigravity 的 `window`，Kimi 的 `duration` 和 `timeUnit`，Grok 的周期起止时间。`quota.Kind` 先看周期（与 5 小时或 7 天相差 5% 以内），没有周期时按 ID 和标签判断。5 小时和 7 天窗口使用固定的 ID 和标签（`five-hour`、`seven-day`），其他长度的窗口带自己的 ID，标签为 `quota.PeriodLabel` 给出的长度，例如 `30d`；Grok 的周期长度会随月份变化，固定使用 `credits`。点火、冷却判断、图表和通知都按种类处理，所以没有 5 小时窗口的账号（例如 ChatGPT Pro）不点火，图表也不显示 5 小时标签页。

**新增服务**：在 `internal/providers` 中新建一个文件，参照 CPA 管理中心额度页的请求实现 `Fetch`，有额度响应头时实现 `Headers`，窗口填上 `Period`；能点火时填写 `Ignition`；把服务加入 `registry`，需要固定位置时把主额度组的名称加入 `groupOrder`；在 `internal/web/page.css` 中加上管理中心同名服务的标签颜色（`--type-<id>-bg` 等）。同步更新 README 的服务表和本文档的主动查询、被动数据、点火请求和模型选择各节。

## 后台循环

`engine.Engine` 用一个 goroutine 完成所有网络工作，避免查询、点火和通知交叉修改状态：

1. 插件注册后启动，先等 20 秒，让 CPA 加载凭证和执行器。
2. 打开数据目录，取得 `instance.lock`。
3. 每 30 秒读取一次 CPA 的 `config.yaml`，判断 `plugins.enabled` 和本插件的 `enabled`。CPA 停用插件时不会通知插件，所以插件自己检查；停用期间不做任何工作。读不到配置文件时视为启用。
4. 依次处理被动数据、Did Codex Reset、到点的主动查询、到期的点火、历史清理、新版本检查，并保存状态。
5. 计算下一次需要醒来的时间，在这之前等待新的被动数据、管理操作或配置变更。

`usage.handle` 和管理接口不直接执行网络请求：前者只解析响应头并放入队列（队列满时丢弃，丢一个被动样本没有影响），后者把操作交给后台循环并等待结果，最多 3 分钟。

新版本检查读取 GitHub API 的 `releases/latest`（不含草稿和预发布版本），后台循环开始工作后读一次，之后每 6 小时读一次，失败后 30 分钟重试；插件停用期间不检查，状态接口保留上一次的结果。请求使用 Did Codex Reset 相同的出口（环境变量中的代理或直连）。版本号按数字逐段比较；最新版本比当前版本新时，状态接口返回 `latest_version` 和 `release_url`，页面在版本号右边显示更新图标。失败只记 debug 日志，保留上一次的结果。当前版本不是纯数字版本时（开发版本 `dev`、`0.0.0-dev.<commit>` 和预发布版本）不检查；最新 Release 的标签不是纯数字版本时视为没有新版本。

后台循环崩溃时，`supervise` 记录日志并在 30 秒后重启。插件中任何未恢复的 panic 都会结束整个 CPA 进程，所以每个 goroutine 和每个 RPC 方法都有 `recover`。

### 生命周期

- `plugin.register` 和 `plugin.reconfigure` 解析配置并应用。无法解析的配置不生效，状态页显示错误，插件继续使用上一份配置。
- 新版本动态库加载后，CPA 对旧实例调用 `plugin.quiesce`，但不会卸载它。插件收到 `plugin.quiesce` 或 `plugin.shutdown` 后停止后台循环且不再启动。
- `instance.lock` 是数据目录中的独占文件锁（Unix 上用 `flock`，Windows 上以不共享的方式打开文件），保证同一时间只有一个实例在查询、点火和推送。新实例拿不到锁时每 10 秒重试一次。

## 额度获取

### 主动查询

| 服务 | 请求 |
| --- | --- |
| Codex | `GET https://chatgpt.com/backend-api/wham/usage`，带 `Chatgpt-Account-Id` |
| Claude | `GET https://api.anthropic.com/api/oauth/usage`，`anthropic-beta: oauth-2025-04-20` |
| Antigravity | `POST .../v1internal:retrieveUserQuotaSummary`，依次尝试三个地址，正文为 `{"project": "<project_id>"}` |
| Kimi | `GET https://api.kimi.com/coding/v1/usages`；与 CPA 的 Kimi 执行器一样，依次看凭证的 `domain`、`base_url`、`type`、服务名，最后看运行时 ID 或文件名是否含 `kimi-ai`、`kimi.ai`，指向 kimi.ai 时用 `api.kimi.ai` |
| Grok | `GET https://cli-chat-proxy.grok.com/v1/billing?format=credits`，带 Grok CLI 的请求头和凭证 `sub` 作为 `x-userid`。`currentPeriod` 为每周时是 7 天窗口；没有 `currentPeriod` 时才用按月的 `billingPeriodStart`/`End`。其他长度的周期固定使用 ID `credits`，让历史和提醒在周期之间延续 |

请求头与 CPA 管理中心额度页相同，常量在 `internal/providers` 中各服务的文件里。Codex 的 User-Agent 与 CPA 的 Codex 执行器一致；Claude 的 User-Agent 与 CPA 默认的 Claude Code 指纹一致。

Antigravity 的 User-Agent 按 CPA 的 Antigravity 执行器的规则确定。Cloud Code 按 User-Agent 判断客户端，对旧格式的客户端（如 `antigravity/cli/1.0.13 (aidev_client; ...)`）不返回 Gemini 额度组。

- 凭证设置了 `user_agent`（文件顶层、`attributes` 或 `metadata` 中）时使用它；以 `antigravity/` 开头的值去掉 ` google-api-nodejs-client/...` 后缀。
- 否则使用 `antigravity/hub/<版本> darwin/arm64`。版本读自 Antigravity Hub 更新清单（`AntigravityHubManifestURL`，与 CPA 使用的地址和请求头相同），成功后缓存 3 小时；读取失败时保留已缓存的版本，30 分钟后重试。还没有读到时使用 `2.23.0`，与 CPA 管理中心一致。清单在查询额度时按需读取，不经过凭证代理。

token 来自凭证文件。CPA 在后台刷新 token 并写回文件，插件不自行刷新；上游返回 401 时跳过本轮，等 CPA 刷新。

查询时间对齐到绝对时间边界（`alignedAfter`），默认 300 秒间隔落在每小时的 `:00 / :05 / ... / :55`。不使用“上次时间加间隔”，以免时间点随启动时间漂移。

### 网络出口

`internal/egress` 让额度请求和 CPA 的模型请求使用同一个出口：

- 凭证有 `proxy_url` 时，用 CPA 的 `sdk/proxyutil` 建立连接，和 CPA 执行器的做法一致；`direct` / `none` 表示直连。无法解析时返回错误，不退回其他出口。
- 没有 `proxy_url` 时调用 `host.http.do`。CPA 依次使用全局 `proxy-url`、环境变量代理、直连。`host.http.do` 本身没有超时，插件先用 `host.http.operation_open` 打开一个操作，超时后用 `host.http.cancel` 取消。

CPA 的 `/api-call`（管理中心额度页使用）会忽略环境变量代理，而模型请求不会。插件按模型请求的规则选择出口。

### 套餐

状态接口的账号带 `plan`（套餐名，未识别时为空）和 `default_weight`（多账号合计中的默认权重），由 `providers.PlanOf` 按保存的原始套餐值得出。原始值按 `auth_index` 存在 `state.json` 的 `plans` 中，插件重启后不必等到下一次查询。默认权重是账号额度相对于该服务最便宜付费套餐的倍数。

- Codex：取 `wham/usage` 返回的 `plan_type`，每次主动查询都更新，不另发请求。套餐名与 Codex CLI 的状态页一致（`codex-rs/tui/src/subscription.rs`），默认权重为价格除以 Plus 的 20 美元：`prolite`（Pro $100）和 `self_serve_business_prolite`（Business Premium）为 5，`pro`（Pro $200）为 10，`promax`（Pro $500）为 25，`free` 和 `go` 为 0.1，其余为 1；Business（`team`）、Enterprise 和 Edu 席位的额度与 Plus 相同。未知的 `plan_type` 以原值作为套餐名，权重为 1。
- Claude：额度响应里没有套餐。主动查询成功后请求 `GET https://api.anthropic.com/api/oauth/profile`，请求头与额度查询相同，每个账号每 24 小时最多一次（`plans` 中的 `checked`）。请求失败时保留上次的结果，只记 debug 日志，不影响额度查询。原始值是 `organization.organization_type` 和 `organization.rate_limit_tier` 以空格连接：`claude_pro` 为 Pro（1）；`claude_max` 的 tier 含 `20x` 为 Max 20x（20），含 `5x` 为 Max 5x（5），都不含时为 Max（5）；`claude_team`、`claude_enterprise` 为 Team、Enterprise（1）。没有 `organization_type` 时按 `account.has_claude_max`、`has_claude_pro` 判断。2026-10-20 在 Pro 账号上实测为 `claude_pro` 和 `default_claude_ai`；Max 和 Team 的取值来自 Claude Code 的已知用法，未经实测。
- 其他服务没有套餐，默认权重为 1。

### 被动数据

CPA 在用量记录的 `ResponseHeaders` 中提供上游响应头：

| 服务 | 响应头 | 窗口 |
| --- | --- | --- |
| Claude | `Anthropic-Ratelimit-Unified-5h-Utilization` / `-5h-Reset`，`-7d-` 同理 | 5 小时、7 天；值是已用比例，如 `0.2` |
| Claude | `Anthropic-Ratelimit-Unified-7d_oi-Utilization` / `-7d_oi-Reset` | Fable 的 7 天窗口，写入与主动查询相同的 Fable 额度组（`providers.FableGroupKey`）。CPA 也把 `7d_oi` 当作 Fable 专用窗口。普通（非 Fable）请求的响应头里没有这组；Fable 请求时是否返回，还没有在能用 Fable 的账号上验证过。插件每次启动后第一次读到时记一条日志 |
| Codex | `X-Codex-Primary-Used-Percent`、`-Reset-At`、`-Reset-After-Seconds`、`-Window-Minutes`，`Secondary` 同理 | 按窗口长度区分，300 分钟为 5 小时，10080 分钟为 7 天，其他长度各自成为一个窗口；没有长度时 Primary 为 5 小时、Secondary 为 7 天 |

Antigravity、Kimi 和 Grok 的响应头里没有额度。

被动数据只更新响应头里有的窗口，主动查询结果替换整个额度组。额度组有过主动查询结果后，被动数据只更新那次结果中出现过的窗口，不新增窗口：ChatGPT Pro 账号的额度接口只返回 7 天窗口，页面上却出现过一个没有重置时间、一直是 100% 的 5 小时窗口，它只能来自响应头。从来没有主动查询结果的额度组（刚启动或查询一直失败）仍接受响应头中的所有窗口。服务新开的窗口要到下一次主动查询才出现。`-Reset-After-Seconds` 从用量记录的 `RequestedAt` 起算：用量记录在请求结束时才到达，从处理时刻起算会把长请求带回的上一周期重置时间推到重置之后。

跳过规则（`shouldSkipActive`）：响应头里没有额度的服务从不跳过。其他服务在查询时刻前 `passive_skip_seconds` 秒内有被动数据，且距上次主动查询不足 `passive_skip_max_minutes` 分钟时跳过。只看查询时刻之前的一小段时间，最坏情况下两次数据间隔为查询间隔加 `passive_skip_seconds`。手动刷新不跳过。

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

点火通过 `host.model.execute` 发送，`auth_id` 为凭证的运行时 ID，`forced_provider` 为 CPA 报告的服务名（`Cred.HostProvider`）。请求体由各服务的 `Ignition.Body` 给出，经 `providers.Request` 编码。Kimi 和 Grok 没有点火：Kimi 订阅的 5 小时和 7 天窗口从开通时间起按固定时间切换，不论是否使用；Grok 的每周额度在上一周期结束时自动开始。其余服务的请求如下：

| 服务 | 入口协议 | 请求要点 |
| --- | --- | --- |
| Codex | `openai-response` | `reasoning.effort: none`，`tools: []`，`max_output_tokens: 4` |
| Claude | `claude` | `max_tokens: 4` |
| Antigravity | `openai` | `max_tokens: 4`，`temperature: 0`，`reasoning_effort: none` |

### 模型选择

CPA 没有按凭证列出模型的宿主回调，插件用 `models_api_key` 读取 `GET /v1/models`。这个列表是所有凭证模型的合集，按模型 ID 去重，`owned_by` 在两个服务提供同一 ID 时可能指向另一个服务，所以候选只按名称挑选（各服务的 `Ignition.Candidates`，由 `providers.Candidates` 去重并处理指定模型）：

- Codex：名称含 `luna` 的模型由新到旧，然后其他 `gpt-` 模型；排除图片模型和 `gpt-oss`。
- Claude：`claude-` 开头的非 thinking Haiku 由新到旧，然后 Sonnet。
- Antigravity Gemini 组：Flash 由新到旧，然后其他非 Pro 模型，Pro 最后；排除图片模型。
- Antigravity Claude / GPT 组：Haiku、Sonnet、Opus（均为非 thinking）、GPT-OSS、其他非 thinking 模型。

“由新到旧”比较模型名中的数字段，新版本无需改代码。每次点火都从最新的候选开始。新模型发布后，旧模型通常还会在列表里保留很久，所以候选顺序不参考上次成功的模型。配置了 `model` 或 `models` 时只用指定模型，且它必须在列表中。

候选模型被锁定的凭证拒绝时，CPA 在本地返回 `auth_not_found`（凭证不支持该模型）或 `unknown provider for model`（没有凭证支持），不会请求上游；插件换下一个候选，最多 6 个。全部被拒绝时按高风险错误处理。模型列表暂时读不到时，使用指定模型或上次成功的模型。

CPA 的模型列表来自它内置并定期更新的模型表，同一服务的凭证都列出相同的模型，所以列表里的新模型不一定能被每个账号调用。上游以 HTTP 400 或 404 拒绝某个候选时（`ignite.ModelRejected`），插件在这次点火中换下一个候选，每次点火只换一次；再次被拒说明问题在请求或账号，按高风险错误处理，错误文字带上第一个被拒的模型和状态码；其余候选都被 CPA 本地拒绝时，带上被拒的完整错误。401、403 和 429 与模型无关，不换模型。

换到的候选成功时，被拒的模型确实与这个额度组的账号有关：`engine.noteRefused` 在内存中记下额度组和模型，并记录 `model_refused` 事件；之后 24 小时（`refusedFor`）内，这个额度组的自动选择跳过该模型（`engine.skipRefused`），到期后再试一次。反复请求账号没有权限的模型可能增加账号风险，而新模型可能分批开放，所以按天重试。记录不写入 `state.json`，插件重启后清空。指定模型不受影响。

模型列表在三种情况下读取：插件启动后、CPA 地址或 `models_api_key` 变化后（`engine.maybeReadModels`），以及每次点火时。额度查询不读取模型列表；前两种读取失败时，随之后每轮额度查询重试，直到成功。每次读取后，`engine.noteModels` 不考虑指定模型，按服务算出自动选择的第一个候选，放在状态接口的 `next_models` 中（每个能点火的服务一项），设置页在点火模型输入框的提示文字后显示它。额度组按服务的 `Ignition.HintGroup` 计算：Antigravity 按 Gemini 组计算，与提示文字“最新的 Flash”一致。`next_models` 只保存在内存中。读取失败时保留上次的结果；CPA 地址或 `models_api_key` 变化时清空 `next_models` 和模型列表错误，再按新的地址和 key 读取。`next_models` 按服务计算，不考虑某个额度组跳过的模型。

### 确认

点火请求返回 2xx 后，插件确认 5 小时窗口在请求之后观测到了固定的未来重置时间：

1. Claude 的响应头里直接带新的重置时间，立即确认。
2. Codex 的额度信息通过同一请求的用量记录到达；等待期间继续处理被动数据。
3. 按 5、10、15、30、15 秒的间隔检查，每次检查前主动查询该凭证一次。Antigravity 的响应头里没有额度，只靠这一步确认。

全部检查都没有确认时，返回 `ErrNotConfirmed`。

### 失败分类和保护

`ignite.Classify`：

| 类别 | 条件 | 处理 |
| --- | --- | --- |
| 换模型 | `auth_not_found`、`unknown provider for model` | 换下一个候选 |
| 上游拒绝模型 | 高风险错误中的 HTTP 400/404（`ignite.ModelRejected`） | 每次点火换一次候选，之后按高风险处理 |
| 高风险 | `ErrNotConfirmed`、`ErrNoModel`，HTTP 400/401/403/404/409/422/429，`not found`、`unsupported` 等 | 立即暂停到第二天 `start_hour` |
| 临时 | CPA 本地冷却（`are cooling down`）、`auth_unavailable`，其他错误 | 按 `failure_retry_seconds × multiplier^(n-1)` 重试，第 `max_transient_failures` 次暂停到第二天 |

CPA 本地冷却带有 `reset_seconds` 且不超过 10 分钟时（`ignite.CooldownWait`），不计入失败次数，在冷却结束后再等 `grace_seconds` 重试。CPA 在 429 之后把凭证冷却到上游重置时间再加十几秒，额度在重置前用完时，重置后 3 秒的点火会撞上这段冷却。连续第二次冷却按普通临时错误计数，避免冷却不断续期时无限重试。

**过期的 CPA 冷却**：CPA 收到账号级 429 后，把凭证冷却到上游报告的重置时间（7 天额度用完时可达数天），额度提前恢复（如重置卡）时不会自动解除。每轮查询后，`engine.checkCooldowns` 检查每个凭证：CPA 冷却还剩 10 分钟以上，且某个带 5 小时窗口的额度组在最近 15 分钟内的数据显示所有窗口都没有用完，就认为冷却已过期。每个冷却结束时间只推送一次（`state.json` 的 `cooldown_notices`），并记录 `cooldown_stale` 事件；状态接口的账号带 `cooldown_until` 和 `stale_cooldown`。插件不替用户清除冷却，因为那需要 CPA 管理密钥，并且会改变 CPA 的路由状态。

暂停时推送一次通知（`circuit_notified_until_epoch` 防止重复），成功或看到固定的未来重置时间后清空失败状态。

## 通知

`notify.Alerts.ProcessGroup` 比较每个窗口当前剩余额度和已通知等级：

- 等级为 normal、notice（≤50%）、low（≤20%）、critical（≤10%）、exhausted（≤0.01%）。等级变差时推送；critical 和 exhausted 使用 `timeSensitive`。
- 只有真正恢复才算等级变好：重置时间换到了新窗口，或者比上次读数高出 5 个百分点及以上（`RecoveryJump`）。主动查询和响应头的数值可能差 1 个百分点，同一窗口内的小幅回升不改变已通知等级，所以读数在阈值附近来回跳时只提醒一次。等级变好时只静默更新基线，否则下一轮的下降不会再提醒。
- 恢复通知和重置前提醒按窗口类别取 `recovery_notify`、`reset_reminder` 中的模式。`notify.SevenDayClass` 按窗口种类（`quota.Kind`）识别 5 小时和 7 天窗口；其他窗口在距离重置超过 1 天时被看到过（`state.json` 的 `long`）就算 7 天窗口，否则算 5 小时窗口。
- 恢复通知只看周期切换（`cycleEnded`）：两个重置时间都已知时，重置时间必须换到新周期，并且旧重置时间已过或额度回升了 `RecoveryJump`。Codex 未使用的窗口每次查询都返回向后顺延的重置时间，只看重置时间是否变化会把每次查询都当成重置。新读数没有重置时间时，旧重置时间已过或额度回升 `RecoveryJump` 即算切换。
- 用量记录在请求结束时才到达，重置前开始的长请求会在重置后带回上一周期的响应头。重置时间已过、并且属于已结束周期（`ended_reset`）或早于当前周期的读数直接忽略，否则会把状态拉回上一周期并再次触发恢复。
- 检测到切换时记下待发送的恢复（`pending_recovery`），连同上一周期最后的读数和该周期是否用完（`exhausted`）。上一周期的读数取重置时间之前的最后一次读数（`cycle_reset`、`cycle_remaining`、`cycle_seen`），因为有的服务在重置后一段时间内仍返回旧的重置时间和新周期的数值。这个读数距离重置超过 15 分钟时不报告，用完的窗口到重置前都是 0，读数不会过期。上一周期重置前仍是 100% 时不推送：插件停止超过一个窗口后，Codex 未使用窗口顺延的重置时间看起来也已过期。`after_exhausted` 模式下只发送用完的周期。推送失败时下一次检查重试，1 小时后放弃。恢复与额度下降分别推送。
- `engine.probes` 为每个带重置时间的窗口（不论通知设置）在重置前和重置后 30 秒（`probeOffset`）各主动查询一次对应账号，每个账号每个时间点只查一次，相差不超过 10 秒的时间点视为同一个；这个时间点之后已有读数时跳过。重置后的查询只在重置后 10 分钟内（`probeLate`）进行，插件刚启动时页面上残留的旧重置时间等下一轮查询。`probes` 只计算，不做标记，主循环用它计算唤醒时间；只有 `runProbes` 在查询时记录。
- 重置前提醒：5 小时类窗口在重置前 1 小时提醒一次（`reset_notice_1h_for`），7 天类窗口在重置前 1 天提醒一次（`reset_notice_1d_for`）。`has_remaining` 模式只在剩余高于 `critical_threshold` 时提醒。
- 两个重置时间相差不超过 10 秒视为同一周期。
- 第一次看到某个窗口只记录基线，不推送。
- 推送失败（所有渠道都失败）时不更新已通知等级，下一次检查会重试。

Bark 请求为 `GET {bark_url}/{标题}/{正文}?group&level&icon&url`，标题和正文除 RFC 3986 非保留字符外全部百分号编码，返回 JSON 中 `code` 不为 200 视为失败。`icon` 默认使用仓库中的 `assets/logo.png`；配置中的 `bark_icon` 仍为旧默认值（CPA Management Center 图标）时，载入时替换为该默认值。

### 渠道

`engine.applyRuntimeConfig` 在配置变化时按 `bark_url` 和 `webhook.url` 建立渠道，交给 `notify.Fanout`。`Fanout` 同时向所有渠道发送，等全部返回后：至少一个渠道成功就算送达，失败的渠道各记一条 `notify_failed` 事件，`error` 以渠道名开头，事件不带额度组 key，只带标签；所有渠道都失败才算推送失败，由调用方按上文的规则重试。只要有渠道成功就不重试，因为重试会让已经收到的渠道重复收到同一条通知。没有任何渠道时返回 `notify.ErrNotConfigured`，表示通知已关闭：额度提醒照常更新已通知等级、待发送的恢复和重置提醒，就像已经送达，这样之后再配置渠道时不会补发旧提醒；点火暂停和冷却提醒不记 `notify_failed`；Did Codex Reset 的记录不标为已见，配置渠道后仍可推送的照常推送。只配了 webhook 但设置无效时，`Fanout.Invalid` 让发送返回设置错误，按推送失败处理，并按上文的规则重试。

`notify.NewWebhook` 检查 webhook 设置：地址必须是 http 或 https，方法只能是 `GET`、`POST`、`PUT`，`GET` 不能带请求体，请求头名称必须合法且不区分大小写地不重复，请求头的值去掉首尾空白后不能含换行或控制字符（YAML 块标量末尾的换行会被去掉），占位符必须在 `notify.Placeholders` 中。检查不通过时 webhook 不启用，原因写进状态接口的 `notify_error`，页面显示为提示，测试推送也把它作为失败结果返回；配置的其他部分照常生效。

每条消息按位置转义占位符：地址中用与 Bark 相同的百分号编码，请求体按 `Content-Type` 做 JSON 字符串转义或表单编码，其他类型原样填入，请求头中把换行换成空格。没有写 `Content-Type` 时，把占位符换成一个单词后是合法 JSON 的请求体按 JSON 发送，其余按纯文本发送，所以 `[Lamplighter] {{text}}` 这样的文本不会被当成 JSON；请求体为空时发送内置 JSON（`source` 加上全部占位符，字段顺序固定）。`{{priority}}` 由 Bark 的级别换算：`timeSensitive` 为 `high`，其余为 `normal`；`{{kind}}` 取自 `Message.Kind`。

webhook 响应最多读 64 KB。非 2xx 视为失败；设置了 `success_json` 时，响应还必须是 JSON 对象，并且其中每个指定的顶层字段都等于期望值。期望值先经过一次 JSON 编码和解码再比较，所以 YAML 中的整数 `0` 与响应中的 `0.0` 相等。错误和测试结果中的响应文字会去掉 `*url.Error` 带的请求地址，并把地址、较长的路径段和查询参数值、请求头的值，以及 JSON 请求体中较长的字符串（例如 ntfy 的 topic）替换为 `[redacted]`，因为这些位置常带有 token。

通知文字在 `internal/notify/text.go` 中，中文和英文各一列，按 `state.json` 的 `language` 选择。语言由管理页面上报：推送时没有打开的页面，所以使用最近一次上报的语言，没有上报过时使用中文。窗口名在通知中始终写作 `5h`、`7d`。Did Codex Reset 的跳转链接按语言指向中文版或英文版的历史页面。

Did Codex Reset 每 `poll_seconds` 秒（最少 300 秒，对齐时间边界）读取最新 10 条记录。非 `manual:` 记录按 ID 去重，`manual:` 记录的 ID 可能变化，按内容去重；最多保留 100 个已见记录。第一次运行把现有记录标为已见，只在 `notify_current_pending` 开启时推送当前待生效的排期。

只推送仍有意义的记录，其余只标为已见：排期必须是 `scheduleState = pending` 且窗口尚未结束；完成记录的时间（`completedAt`、`effectiveAt`、`announcedAt` 中最晚的一个）不能早于 48 小时前。已兑现的排期会离开 `kind=all` 列表，失去关联后又会以 `elapsed` 回到列表，旧记录因此可能在任意时间出现。每条 X 帖子是一条记录，主帖和回复会给同一次排期各建一条。重置种类、窗口起点和范围（`scope.plans`、`scope.windows`）相同的排期视为同一次排期。只用起点比较，因为同一次排期的记录有的只有 `effectiveAt`，有的带完整的 `scheduleWindow`。与 Did Codex Reset 首页一致，同一次查询中只推送其中 `announcedAt` 最晚、且本身可推送的一条，时间相同时按记录键取一条，其余只标为已见。回复可能晚一两次查询才进入最新 10 条，所以推送过的排期连同帖子的 `announcedAt` 和通知内容（按 UTC 和英文生成）记在 `sent_events` 中，最多 100 个。之后出现的同一排期帖子，只有 `announcedAt` 更晚且内容（置信度、时间、范围）有变化时，才以“排期更新”为标题再推送；其余只标为已见。

排期时间取 `scheduleWindow`，没有时取 `effectiveAt`，按插件时区显示：起止相同时显示时刻，`scheduleConstraint = deadline` 时显示“终点 前”，其余（日期级排期）显示“起点～终点”。日期级窗口覆盖来源时区的一整天，起点通常早于公布时间，只显示起点会被误读为已经过去的时刻。

## 数据

数据目录默认是插件目录下的 `data/lamplighter`。`config.DefaultDataDir` 从 CPA 的 `config.yaml` 读取 `plugins.dir`（默认 `plugins`，开头的 `~` 展开为用户主目录，相对路径以 CPA 的工作目录为准），与 CPA 解析插件目录的规则相同。CPA 在所有平台上都从临时目录 `cliproxy-pluginhost/pid-<进程号>/` 中的副本加载动态库，这个目录在 CPA 重启或容器重建后不保留，所以数据目录不能按动态库所在位置计算。

| 文件 | 内容 |
| --- | --- |
| `state.json` | `groups`：每个窗口的通知基线；`scheduler`：每个额度组的点火状态；`codex_reset_updates`：已见记录和已推送的排期；`cooldown_notices`：已提醒的过期冷却；`language`：通知语言；`plans`：各账号的原始套餐值和上次单独读取套餐的时间。写入临时文件后替换 |
| `history/YYYY-MM-DD.jsonl` | 按 UTC 日期分文件的额度样本和事件 |
| `instance.lock` | 实例锁 |

历史记录每行一个 JSON：样本为 `{"k":"s","t":秒,"g":额度组,"w":窗口,"r":剩余,"x":重置,"s":"active|passive"}`，事件为 `{"k":"e","t":秒,"g":额度组,"e":类型,"v":级别,"m":文本,"l":标签,"d":详情,"p":参数}`。`m` 和 `d` 是写给 CPA 日志的中文；`p` 是页面拼出事件文字所需的值：`ignite` 和 `ignite_manual` 为 `model`、`reset`，`ignite_failed` 为 `retry_seconds`、`error` 和可选的 `cooldown`，`ignite_paused` 为 `until`、`error`，`model_refused` 为 `model`、`fallback`、`hours`，`notify` 为 `title`，`notify_failed` 为 `title`、`error`，`cooldown_stale` 为 `until`，`codex_reset` 为 `count`；时间为 RFC 3339 UTC。主动样本每次都写；被动样本只在数值或重置时间变化时写，同一窗口每分钟最多一次，期间的新值暂存在 `pendingSamp`，满一分钟后写入。历史接口对超过 2 天的范围分段，每段取最后一个样本：超过 2 天按 10 分钟，超过 4 天按 15 分钟，超过 8 天按 30 分钟，超过 15 天按 1 小时，使这些范围的每个系列不超过约 800 个点（图表约 1000 像素宽）。2 天及以内不分段。分段长度在响应的 `bucket_seconds` 中。每个系列带有 `label`（含账号后缀，例如 `ChatGPT#rk`）和 `source_label`（不含后缀），页面按服务和 `source_label` 合并同一额度的多个账号。

额度组 key 为 `<服务>:<auth_index>:<分组>`，例如 `codex:3:codex:main`、`claude:3:claude:seven-day-fable`、`antigravity:4:antigravity:gemini-models`。它同时是点火目标 ID。

## 设计约束

- 点火只通过 `host.model.execute` 发送，并锁定到具体凭证；不要在插件中拼装服务的模型请求。
- 额度请求的出口必须与 CPA 模型请求一致；出口无法确定时跳过请求，不能退回直连。
- 不在日志、事件、状态接口或磁盘中输出 access token、`bark_url`、`webhook` 的地址、请求头和请求体，以及 `models_api_key`。
- 宿主回调的 JSON 字段以 CPA v8.0.4 的 `sdk/pluginapi` 和 `internal/pluginhost` 为准；修改前对照 CPA 源码。
- 修改 `state.json` 结构时保持向后兼容：新字段使用缺省值，旧字段缺失时继续运行。
