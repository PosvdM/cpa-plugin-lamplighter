<p align="center"><img src="./assets/logo.png" alt="" width="128"></p>

# Lamplighter

[English](./README_EN.md)

> 名字来自《小王子》里的点灯人：他的星球每分钟转一圈，他就每分钟点一次灯、熄一次灯，从不误点。这个插件做的也是按时点灯：每个 5 小时额度窗口一重置，就点亮下一个窗口。

Lamplighter（点灯人）是 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)（CPA）的原生插件。它监控 ChatGPT（Codex）、Claude 和 Antigravity 账号的额度，通过 [Bark](https://github.com/Finb/Bark) 或自定义 webhook 推送提醒，并在 5 小时额度窗口重置后发送一个极小的请求，让下一个窗口立即开始计时。

![管理页面：各账号额度和自动点火计划](./docs/images/overview.png)

![额度变化图表：最近 12 小时的 5 小时额度，下方为 Claude 的额度曲线，标出重置和点火结果](./docs/images/chart.png)

![额度变化图表：多账号的 Claude 展开为各账号，下方为合计曲线和账号间的范围](./docs/images/chart-accounts.png)

## 功能

- **额度监控**：定时主动查询额度，同时读取 CPA 处理真实请求时上游返回的额度信息。
- **通知**：通过 Bark 或 webhook（飞书、ntfy、钉钉、Telegram 等）推送。剩余额度降到 50%、20%、10% 和耗尽时提醒；可选的恢复通知和重置前提醒；可选转发 [Did Codex Reset](https://didcodexreset.com/zh.html) 的重置信号。
- **自动点火**：每天 07:00 起，在每次 5 小时窗口重置后 3 秒发送最小请求，最晚到 22:30。请求经过 CPA 自己的模型执行器，并锁定到指定账号。
- **管理页面**：在 CPA 管理中心查看各账号额度、点火计划、事件和额度变化图表，并修改设置。

| 服务 | 监控范围 | 默认点火 |
| --- | --- | --- |
| ChatGPT / Codex | 5 小时、7 天 | 开启 |
| Claude | 5 小时、7 天，以及 Fable 等单独计算的额度 | 开启，只点 5 小时窗口 |
| Antigravity | Gemini、Claude / GPT 等额度组 | 关闭；开启后默认只点 Gemini |

同一服务有多个账号时，通知和页面用邮箱用户名的最后两个字符区分，例如 `ChatGPT#rk`。

## 要求

- CPA v8.0.4 或更新版本，并开启插件（`plugins.enabled: true`）。插件提供 Linux amd64/arm64、macOS amd64/arm64 和 Windows amd64 版本；macOS 版本只在 CI 中构建和测试，还没有在实际运行的 CPA 上验证。
- 一个专门给 Lamplighter 用的 CPA API key。插件用它读取模型列表来选择点火模型。
- 需要通知时，一台装有 Bark 的 iPhone，或者一个能接收 webhook 的服务。

## 安装

1. 在 [Releases](https://github.com/PosvdM/cpa-plugin-lamplighter/releases) 下载与 CPA 所在系统对应的 zip：Linux 上 `uname -m` 显示 `x86_64` 时选 `linux_amd64`，显示 `aarch64` 时选 `linux_arm64`；Apple 芯片的 Mac 选 `darwin_arm64`，Intel 芯片的 Mac 选 `darwin_amd64`；Windows 选 `windows_amd64`。
2. 解压出动态库（Linux 为 `lamplighter-v<版本>.so`，macOS 为 `.dylib`，Windows 为 `.dll`），放进 CPA 插件目录下的 `<系统>/<架构>/` 子目录，例如 `plugins/linux/arm64/` 或 `plugins/windows/amd64/`。Docker 部署时，插件目录是挂载到容器 `/CLIProxyAPI/plugins` 的宿主机目录。
3. 在 CPA 的 `config.yaml` 中新增一个 API key，并启用插件：

   ```yaml
   access:
     api-keys:
       - "已有的 key"
       - "给 Lamplighter 新建的 key"
   plugins:
     enabled: true
     configs:
       lamplighter:
         enabled: true
         models_api_key: "给 Lamplighter 新建的 key"
         bark_url: "https://api.day.app/你的device_key"
   ```

4. 重启 CPA（Docker 部署时运行 `docker compose restart`）。插件在 CPA 启动约 20 秒后开始第一次查询。
5. 打开管理中心侧边栏的 **Lamplighter** 页面。如果登录管理中心时勾选了“记住密码”，页面会直接使用该密钥，否则会请你输入。

升级时，把新版本的动态库放进同一目录，删除旧版本文件，再重启 CPA。

插件的状态和额度历史保存在插件目录下的 `data/lamplighter/`。Docker 部署时插件目录已经挂载在宿主机上，重建容器不会丢失这些数据。

## 额度怎么获取

**主动查询**：默认每 5 分钟一次，固定在每小时的 `00 / 05 / 10 / ... / 55` 分钟。此外，每个额度窗口重置前 30 秒和重置后 30 秒，插件各对这个账号再查询一次：前一次记录这个周期最后的剩余额度，后一次确认新周期并及时发出恢复通知。这段时间里已有新读数时不再查询。请求地址和请求头与 CPA 管理中心额度页发出的请求相同。

**被动读取**：Claude 和 Codex 的模型请求经过 CPA 时，上游会在响应里返回当前额度，插件直接使用这些数据，不额外发请求。Antigravity 没有这类数据，只能主动查询。

如果某个账号在整点前 60 秒内刚有过被动数据，这一轮就跳过对它的主动查询。同一账号最多连续跳过 30 分钟，到时一定主动查询一次，以便更新没有出现在响应头里的额度，例如没在使用的 Claude Fable 额度。图表不把这段计划内的间隔画成缺数据。

Claude 和 Codex 的额度无论主动还是被动获取，都精确到 1%；Antigravity 的主动查询结果带小数。

**网络出口**：主动查询和 CPA 发送模型请求时使用同一个出口，依次是：账号自己的 `proxy_url`、全局 `proxy-url`、环境变量中的代理、直连。账号配置的 `proxy_url` 无法解析时，插件跳过这次查询，不会改用直连。

## 自动点火

5 小时额度窗口从重置后的第一个请求开始计时。重置后没人使用时，窗口不会开始。Lamplighter 在每次重置后立即发一个请求，让窗口连续滚动：

- 时间均按插件时区计算，默认跟随 CPA 服务器。
- 每天 `07:00` 第一次点火，之后在每次重置后 3 秒点火，最晚到 `22:30`。晚于这个时间的重置顺延到第二天 07:00。
- 请求只要求模型回复 `OK`，不带工具，最多输出 4 个 token。
- 点火模型自动选择：ChatGPT 用最新的 Luna，Claude 用最新的 Haiku，Antigravity 的 Gemini 组用最新的 Flash。CPA 列出新模型后，下一次点火就会使用它；上游拒绝这个模型时，这次点火改用下一个候选模型；换用成功时在事件中记录，这个额度组 24 小时内不再尝试被拒的模型。也可以在配置中指定。
- 请求发出后，插件确认 5 小时窗口有了一个固定的未来重置时间，才算点火成功。

**失败保护**：

- 认证失败、限流（429）、模型不可用，或请求成功但窗口没有开始，都会让这个额度组停止点火到第二天 07:00，并推送一次通知。
- 网络错误和服务端错误先在 5 分钟、15 分钟后重试；第三次仍失败，同样停到第二天。
- 额度在重置前用完时，CPA 会把账号冷却到重置后十几秒。点火撞上这段冷却时，等冷却结束后立即重试，不算失败。
- 同一账号的 7 天额度（或其他非 5 小时窗口）用完时，这个额度组不再自动点火，等到该窗口重置、查询看到额度恢复后继续；页面状态显示“7 天额度已用完”。“立即点火”不受影响。
- 额度提前恢复（例如用了重置卡）而 CPA 仍按原重置时间冷却这个账号时，期间所有请求和点火都会被 CPA 拒绝。插件发现后推送一次 ⚠️ 提醒，并在账号卡片上提示，需要在 CPA 管理中心手动清除这个账号的冷却。

## 通知示例

通知使用最近一次打开管理页面时管理中心的语言（中文或英文），从没打开过时使用中文。

```text
🟡 Claude · 7d 48% | 03d

5h：96% | 04h | 10/04 13:50
7d：48% | 03d | 10/07 14:00
```

标题前的图标：🟡 剩余降到第一档（默认 50%），🔴 降到第二档（默认 20%）及以下，✅ 额度恢复，⏰ 重置提醒，⚠️ 点火暂停、CPA 冷却未解除等需要处理的问题。正文每行依次是：窗口、剩余额度、距离重置的时间、重置时间。第一次看到某个额度窗口时只记录当前状态，不推送。

## 配置

所有配置都写在 `config.yaml` 的 `plugins.configs.lamplighter` 下，也可以在管理页面的“设置”中修改。页面上的设置改动后自动保存：开关和勾选框点击后保存，文本和数字在离开输入框或按回车时保存。配置修改后插件自动应用，不需要重启。

| 配置项 | 默认值 | 说明 |
| --- | --- | --- |
| `bark_url` | 空 | Bark 推送地址，写到 device key 为止；为空时不通过 Bark 推送 |
| `bark_group` | `CPA` | Bark 通知分组 |
| `bark_icon` | Lamplighter 图标 | 通知图标 |
| `webhook` | 空 | 自定义 webhook，见 [Webhook](#webhook) |
| `notice_threshold` | `50` | 第一档提醒阈值（剩余百分比） |
| `low_threshold` | `20` | 第二档提醒阈值 |
| `critical_threshold` | `10` | 第三档提醒阈值 |
| `recovery_notify` | 5 小时 `after_exhausted`，7 天 `all` | 额度恢复时通知，见下文 |
| `reset_reminder` | 5 小时 `off`，7 天 `has_remaining` | 重置前提醒，见下文 |
| `timezone` | 空 | 显示时间和点火时段使用的时区，如 `Asia/Shanghai`；为空时跟随 CPA 服务器的时区（官方 Docker 镜像由 `TZ` 设置） |
| `timezone_offset_hours` | 空 | 固定的 UTC 偏移，例如 `8`；只在 `timezone` 为空时使用 |
| `poll_interval_seconds` | `300` | 主动查询间隔，最小 60 |
| `request_timeout_seconds` | `20` | 单次上游请求超时 |
| `passive_skip_seconds` | `60` | 整点前多少秒内有被动数据时跳过主动查询；`0` 表示不跳过 |
| `passive_skip_max_minutes` | `30` | 同一账号最多连续跳过多少分钟 |
| `models_api_key` | 空 | 读取模型列表用的 CPA API key；未设置时自动点火无法选择模型 |
| `cpa_base_url` | `http://127.0.0.1:8317` | 插件访问 CPA 自身的地址；CPA 改了端口或开启 TLS 时需要同步修改 |
| `history_retention_days` | `40` | 额度历史保留天数 |
| `data_dir` | 插件目录下的 `data/lamplighter` | 状态和历史目录 |

`bark_url` 和 `webhook.url` 都为空时通知关闭：不发送任何通知，也不在事件中记录推送失败。之后再填上地址，关闭期间的额度提醒不会补发；Did Codex Reset 的信号还没过期的会照常推送。

恢复通知和重置前提醒按窗口分别设置，`five_hour` 对应 5 小时窗口，`seven_day` 对应 7 天窗口：

| 配置项 | 取值 | 说明 |
| --- | --- | --- |
| `recovery_notify` | `off`、`all`、`after_exhausted` | `all`：窗口每次重置都通知；`after_exhausted`：窗口用完后，只在下一次重置时通知一次 |
| `reset_reminder` | `off`、`all`、`has_remaining` | 5 小时窗口在重置前 1 小时提醒，7 天窗口在重置前 1 天提醒；`has_remaining`：只在剩余高于 `critical_threshold` 时提醒 |

恢复通知的第一行是上一周期结束时的剩余额度。这个值来自重置前 30 秒的那次主动查询，最后 30 秒内的用量不计入。找不到重置前 15 分钟内的读数时不显示这一行。上一周期从没用过（重置前仍是 100%）时不通知。既不是 5 小时也不是 7 天的窗口，曾在距离重置超过 1 天时出现过的按 7 天窗口处理，其余按 5 小时窗口处理。

`notify_recovery` 和 `notify_reset_reminders` 为 `true` 时等同于对应配置的两个窗口都设为 `all`，为 `false` 时都设为 `off`，只在没有写 `recovery_notify` 或 `reset_reminder` 时生效；在管理页面修改对应的通知设置后会换成新的写法。

点火相关设置在 `ignition` 下：

| 配置项 | 默认值 | 说明 |
| --- | --- | --- |
| `enabled` | `true` | 是否自动点火 |
| `start_hour` | `7` | 每天第一次点火的时间（时） |
| `end_hour` | `22` | 每天停止点火的时间（时） |
| `end_grace_minutes` | `30` | `end_hour` 之后再顺延的分钟数 |
| `grace_seconds` | `3` | 重置后延迟多少秒点火 |
| `failure_retry_seconds` | `300` | 临时错误后第一次重试的等待时间 |
| `failure_backoff_multiplier` | `3` | 每次重试等待时间的倍数 |
| `max_transient_failures` | `3` | 临时错误达到这个次数后停到第二天 |
| `post_success_hold_seconds` | `60` | 点火成功后至少等待多久才可能再次点火 |

各服务的设置在 `providers` 下，键名为 `codex`、`claude`、`antigravity`：

| 配置项 | 说明 |
| --- | --- |
| `monitor` | 是否监控 |
| `ignite` | 是否自动点火 |
| `model` | 指定点火模型，留空时自动选择；必须出现在 CPA 的模型列表中 |
| `groups` | 仅 Antigravity：允许点火的额度组，默认 `["Gemini"]` |
| `models` | 仅 Antigravity：按额度组指定模型，例如 `{"Gemini": "gemini-3.5-flash-lite"}` |

`codex_reset_updates` 控制 Did Codex Reset 转发：`enabled`（默认 `false`）、`poll_seconds`（默认 `300`，最小 300）、`notify_current_pending`（默认 `true`，第一次开启时推送当前尚未生效的排期）。只转发尚未到期的排期和 48 小时内的完成记录，同一次排期有多条帖子时，只在置信度、时间或范围变化时作为排期更新再推送；排期时间按本地时区显示，只给出日期的排期显示为时间段。Did Codex Reset 是第三方监测站，它的信号不代表 OpenAI 官方确认。

完整示例：

```yaml
plugins:
  configs:
    lamplighter:
      enabled: true
      bark_url: "https://api.day.app/你的device_key"
      models_api_key: "给 Lamplighter 新建的 key"
      ignition:
        enabled: true
        start_hour: 7
        end_hour: 22
      providers:
        codex:
          ignite: true
        claude:
          ignite: true
        antigravity:
          ignite: true
          groups: ["Gemini"]
      codex_reset_updates:
        enabled: true
```

## Webhook

除了 Bark，还可以配置一个 webhook，把通知发到飞书、ntfy、钉钉、Telegram 等服务或者自己的程序。两个渠道都配置时，每条通知同时发给两边；只要有一边发送成功就算送达，不再重试，失败的一边在事件中记为“推送失败”。

| 配置项 | 默认值 | 说明 |
| --- | --- | --- |
| `url` | 空 | 请求地址；为空时不启用 |
| `method` | `POST` | `GET`、`POST` 或 `PUT` |
| `headers` | 空 | 请求头，例如 `Authorization: Bearer <token>` |
| `body` | 空 | 请求体；为空时发送内置的 JSON 消息。`GET` 请求不能设置 |
| `success_json` | 空 | 成功条件：JSON 响应中这些顶层字段必须等于给定的值，例如 `code: 0`；为空时只要求 HTTP 状态码为 2xx |

地址、请求头和请求体中可以使用占位符：

| 占位符 | 内容 |
| --- | --- |
| `{{title}}` | 通知标题 |
| `{{body}}` | 通知正文 |
| `{{text}}` | 标题和正文，中间换行 |
| `{{url}}` | 跳转链接，只有 Did Codex Reset 通知有，其余为空 |
| `{{priority}}` | 额度降到第三档或耗尽、点火暂停、CPA 冷却未解除时为 `high`，其余为 `normal` |
| `{{kind}}` | 通知类型：`quota` 额度下降、`recovery` 额度恢复、`reminder` 重置提醒、`cooldown` CPA 冷却未解除、`circuit` 点火暂停、`codex_reset` Did Codex Reset、`test` 测试推送 |
| `{{label}}` | 额度组，例如 `ChatGPT#rk`；与额度组无关的通知为空 |
| `{{time}}` | 发送时间，RFC 3339 格式，使用插件时区 |

占位符按所在位置自动转义：地址中做 URL 编码；JSON 请求体中做 JSON 字符串转义，所以要写在引号里；表单请求体中做表单编码；请求头中把换行换成空格。没有设置 `Content-Type` 请求头时，请求体是 JSON（占位符写在引号里）就按 JSON 发送，否则按纯文本发送。

不写 `body` 时发送的 JSON：

```json
{"source":"lamplighter","kind":"quota","title":"🔴 ChatGPT#rk · 5h 8% | 03h","body":"5h：8% | 03h | 10/10 19:00\n7d：60% | 04d | 10/14 09:00","text":"…","url":"","priority":"high","label":"ChatGPT#rk","time":"2026-10-10T16:00:00+08:00"}
```

常见服务的写法如下。这些是按各服务文档整理的格式示例，没有逐一实测。

| 服务 | `url` | `body` | `success_json` |
| --- | --- | --- | --- |
| ntfy | `https://ntfy.sh/` | `{"topic":"<topic>","title":"{{title}}","message":"{{body}}"}` | 不需要 |
| 飞书 | `https://open.feishu.cn/open-apis/bot/v2/hook/<token>` | `{"msg_type":"text","content":{"text":"{{text}}"}}` | `code: 0` |
| 钉钉 | `https://oapi.dingtalk.com/robot/send?access_token=<token>` | `{"msgtype":"text","text":{"content":"{{text}}"}}` | `errcode: 0` |
| 企业微信 | `https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=<key>` | `{"msgtype":"text","text":{"content":"{{text}}"}}` | `errcode: 0` |
| Discord | `https://discord.com/api/webhooks/<id>/<token>` | `{"content":"{{text}}"}` | 不需要 |
| Slack | `https://hooks.slack.com/services/<path>` | `{"text":"{{text}}"}` | 不需要 |
| Telegram | `https://api.telegram.org/bot<token>/sendMessage` | `{"chat_id":"<chat_id>","text":"{{text}}"}` | 不需要 |

以飞书为例，在 `config.yaml` 中写成下面这样。请求体含有 `{{`，要用单引号括起来：

```yaml
plugins:
  configs:
    lamplighter:
      webhook:
        url: "https://open.feishu.cn/open-apis/bot/v2/hook/<token>"
        body: '{"msg_type":"text","content":{"text":"{{text}}"}}'
        success_json:
          code: 0
```

- 飞书、钉钉和企业微信在发送失败时也返回 HTTP 200，错误码写在响应里，所以要设置 `success_json`，否则失败的推送会被当作成功。
- 插件不支持飞书和钉钉机器人的“签名校验”。请在机器人的安全设置中改用“自定义关键词”或 IP 白名单；使用关键词时，把它写进请求体，例如 `"text":"Lamplighter {{text}}"`。
- 管理页面的“测试推送”逐个显示每个渠道的结果和 webhook 响应的开头。没有设置 `success_json` 时，页面会提示只检查了 HTTP 状态码，需要到客户端确认是否收到。
- webhook 设置有误（例如占位符拼错）时，webhook 不启用，管理页面顶部显示原因，其他设置照常生效。

## 管理页面

页面包含：

- 各账号的额度窗口、重置时间和数据来源，可以单独刷新某个账号；
- 每个额度组的下次点火时间、最近一次结果和失败保护状态，可以立即点火；
- 额度变化图表：先选 5 小时或 7 天额度，5 小时额度可看最近 1、3、6、12、24、26 小时（默认 6 小时），7 天额度可看最近 1、4、8、15 天、1 个月（上月这一天到今天）和 36 天（默认 8 天）。上半部分每个服务一行色带，颜色与额度条相同，默认按 Claude、ChatGPT、Gemini、Fable、Claude / GPT 排列，也可按剩余最低排序；同一服务有多个账号时显示合计，点击可展开各账号。下半部分是所选行的曲线，标出重置时刻和点火结果；
- 最近的事件，按时间、类型、额度组和详情分列显示；
- 设置表单。通知设置中可以发送测试推送：发到所有已配置的渠道并逐个显示结果，使用插件已应用的设置，修改设置后等几秒再测试。点火模型留空时，输入框的提示文字显示下一次点火将使用的模型；Antigravity 显示 Gemini 组的模型。模型列表在插件启动、修改 `cpa_base_url` 或 `models_api_key` 和每次点火时读取，所以 CPA 列出的新模型在下一次点火后才显示，点火本身会直接使用新模型。

页面语言跟随 CPA 管理中心：简体或繁体中文显示中文，其他语言显示英文。

## 安全与风险

- 插件只在内存中读取凭证文件里的 access token 用于查询额度，不写入日志或磁盘。管理页面和接口都需要 CPA 管理密钥。
- `bark_url`、`webhook` 和 `models_api_key` 以明文保存在 CPA 的 `config.yaml` 中，有管理密钥的人可以看到。
- 各服务对第三方工具使用订阅账号有不同限制。通过 CPA 使用账号、定时发送点火请求和查询额度，都可能带来账号风险，请自行判断。

## 开发

插件结构、数据流和设计约束见 [架构文档](./docs/architecture.md)，构建、测试和发布见 [开发文档](./docs/development.md)。

## 许可证

[AGPL-3.0-or-later](./LICENSE)。分发修改后的版本，或通过网络向他人提供修改后的版本时，需要按同一许可证公开源代码。
