# Lamplighter

[English](./README_EN.md)

Lamplighter（点灯人）是 [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)（CPA）的原生插件。它监控 ChatGPT（Codex）、Claude 和 Antigravity 账号的额度，通过 [Bark](https://github.com/Finb/Bark) 推送提醒，并在 5 小时额度窗口重置后发送一个极小的请求，让下一个窗口立即开始计时。

## 功能

- **额度监控**：定时主动查询额度，同时读取 CPA 处理真实请求时上游返回的额度信息。
- **Bark 通知**：剩余额度降到 50%、20%、10% 和耗尽时提醒；可选的恢复通知和重置前提醒；可选转发 [Did Codex Reset](https://didcodexreset.com/zh.html) 的重置信号。
- **自动点火**：每天 07:00 起，在每次 5 小时窗口重置后 3 秒发送最小请求，最晚到 22:30。请求经过 CPA 自己的模型执行器，并锁定到指定账号。
- **管理页面**：在 CPA 管理中心查看各账号额度、点火计划、事件和额度变化图表，并修改设置。

| 服务 | 监控范围 | 默认点火 |
| --- | --- | --- |
| ChatGPT / Codex | 5 小时、7 天 | 开启 |
| Claude | 5 小时、7 天，以及 Fable 等单独计算的额度 | 开启，只点 5 小时窗口 |
| Antigravity | Gemini、Claude / GPT 等额度组 | 关闭；开启后默认只点 Gemini |

同一服务有多个账号时，通知和页面用邮箱用户名的最后两个字符区分，例如 `ChatGPT#rk`。

## 要求

- CPA v8.0.4 或更新版本，运行在 Linux amd64 或 arm64 上，并开启插件（`plugins.enabled: true`）。
- 一个专门给 Lamplighter 用的 CPA API key。插件用它读取模型列表来选择点火模型。
- 需要通知时，一台装有 Bark 的 iPhone。

## 安装

1. 在 [Releases](https://github.com/PosvdM/cpa-plugin-lamplighter/releases) 下载与服务器架构对应的 zip：`uname -m` 显示 `x86_64` 时选 `linux_amd64`，显示 `aarch64` 时选 `linux_arm64`。
2. 解压出 `lamplighter-v<版本>.so`，放进 CPA 插件目录下的 `linux/<架构>/` 子目录，例如 `plugins/linux/arm64/`。Docker 部署时，插件目录是挂载到容器 `/CLIProxyAPI/plugins` 的宿主机目录。
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

升级时，把新版本的 `.so` 放进同一目录，删除旧版本文件，再重启 CPA。

插件的状态和额度历史保存在插件目录下的 `data/lamplighter/`。Docker 部署时插件目录已经挂载在宿主机上，重建容器不会丢失这些数据。

## 额度怎么获取

**主动查询**：默认每 5 分钟一次，固定在每小时的 `00 / 05 / 10 / ... / 55` 分钟。请求地址和请求头与 CPA 管理中心额度页发出的请求相同。

**被动读取**：Claude 和 Codex 的模型请求经过 CPA 时，上游会在响应里返回当前额度，插件直接使用这些数据，不额外发请求。Antigravity 没有这类数据，只能主动查询。

如果某个账号在整点前 60 秒内刚有过被动数据，这一轮就跳过对它的主动查询。同一账号最多连续跳过 30 分钟，到时一定主动查询一次，以便更新 Claude 的 Fable 等只能主动查询的额度。

Claude 和 Codex 的额度无论主动还是被动获取，都精确到 1%；Antigravity 的主动查询结果带小数。

**网络出口**：主动查询和 CPA 发送模型请求时使用同一个出口，依次是：账号自己的 `proxy_url`、全局 `proxy-url`、环境变量中的代理、直连。账号配置的 `proxy_url` 无法解析时，插件跳过这次查询，不会改用直连。

## 自动点火

5 小时额度窗口从重置后的第一个请求开始计时。重置后没人使用时，窗口不会开始。Lamplighter 在每次重置后立即发一个请求，让窗口连续滚动：

- 时间均按插件时区计算，默认跟随 CPA 服务器。
- 每天 `07:00` 第一次点火，之后在每次重置后 3 秒点火，最晚到 `22:30`。晚于这个时间的重置顺延到第二天 07:00。
- 请求只要求模型回复 `OK`，不带工具，最多输出 4 个 token。
- 点火模型自动选择：ChatGPT 用最新的 Luna，Claude 用最新的 Haiku，Antigravity 的 Gemini 组用最新的 Flash。也可以在配置中指定。
- 请求发出后，插件确认 5 小时窗口有了一个固定的未来重置时间，才算点火成功。

**失败保护**：

- 认证失败、限流（429）、模型不可用，或请求成功但窗口没有开始，都会让这个额度组停止点火到第二天 07:00，并推送一次 Bark。
- 网络错误和服务端错误先在 5 分钟、15 分钟后重试；第三次仍失败，同样停到第二天。

## 通知示例

```text
⚠️ Claude · 7d 48% | 03d

5h：96% | 04h | 10/04 13:50
7d：48% | 03d | 10/07 14:00
```

正文每行依次是：窗口、剩余额度、距离重置的时间、重置时间。第一次看到某个额度窗口时只记录当前状态，不推送。

## 配置

所有配置都写在 `config.yaml` 的 `plugins.configs.lamplighter` 下，也可以在管理页面的“设置”中修改。修改后插件自动应用，不需要重启。

| 配置项 | 默认值 | 说明 |
| --- | --- | --- |
| `bark_url` | 空 | Bark 推送地址，写到 device key 为止；为空时不推送 |
| `bark_group` | `CPA` | Bark 通知分组 |
| `bark_icon` | CPA 图标 | 通知图标 |
| `notice_threshold` | `50` | 第一档提醒阈值（剩余百分比） |
| `low_threshold` | `20` | 第二档提醒阈值 |
| `critical_threshold` | `10` | 第三档提醒阈值 |
| `notify_recovery` | `false` | 额度恢复时是否通知 |
| `notify_reset_reminders` | `false` | 重置前是否提醒：任意窗口重置前 1 小时，7 天窗口重置前 1 天 |
| `timezone` | 空 | 显示时间和点火时段使用的时区，如 `Asia/Shanghai`；为空时跟随 CPA 服务器的时区（官方 Docker 镜像由 `TZ` 设置） |
| `timezone_offset_hours` | 空 | 固定的 UTC 偏移，例如 `8`；只在 `timezone` 为空时使用 |
| `poll_interval_seconds` | `300` | 主动查询间隔，最小 60 |
| `request_timeout_seconds` | `20` | 单次上游请求超时 |
| `passive_skip_seconds` | `60` | 整点前多少秒内有被动数据时跳过主动查询；`0` 表示不跳过 |
| `passive_skip_max_minutes` | `30` | 同一账号最多连续跳过多少分钟 |
| `models_api_key` | 空 | 读取模型列表用的 CPA API key；未设置时自动点火无法选择模型 |
| `cpa_base_url` | `http://127.0.0.1:8317` | 插件访问 CPA 自身的地址；CPA 改了端口或开启 TLS 时需要同步修改 |
| `history_retention_days` | `70` | 额度历史保留天数 |
| `data_dir` | 插件目录下的 `data/lamplighter` | 状态和历史目录 |

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

`codex_reset_updates` 控制 Did Codex Reset 转发：`enabled`（默认 `false`）、`poll_seconds`（默认 `300`，最小 300）、`notify_current_pending`（默认 `true`，第一次开启时推送当前尚未生效的排期）。Did Codex Reset 是第三方监测站，它的信号不代表 OpenAI 官方确认。

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

## 管理页面

页面包含：

- 各账号的额度窗口、重置时间和数据来源，可以单独刷新某个账号；
- 每个额度组的下次点火时间、最近一次结果和失败保护状态，可以立即点火；
- 额度变化图表：先选 5 小时或 7 天额度，5 小时额度可看最近 1、2、5、24 小时，7 天额度可看最近 1、7、14、35 天。上半部分每个服务一行色带，颜色与额度条相同，默认按 Claude、ChatGPT、Gemini、Fable、Claude / GPT 排列，也可按剩余最低排序；同一服务有多个账号时显示合计，点击可展开各账号。下半部分是所选行的曲线，标出重置时刻和点火结果；
- 最近的事件，按时间、类型、额度组和详情分列显示；
- 设置表单，通知设置中可以发送测试推送。

## 安全与风险

- 插件只在内存中读取凭证文件里的 access token 用于查询额度，不写入日志或磁盘。管理页面和接口都需要 CPA 管理密钥。
- `bark_url` 和 `models_api_key` 以明文保存在 CPA 的 `config.yaml` 中，有管理密钥的人可以看到。
- 各服务对第三方工具使用订阅账号有不同限制。通过 CPA 使用账号、定时发送点火请求和查询额度，都可能带来账号风险，请自行判断。

## 开发

插件结构、数据流和设计约束见 [架构文档](./docs/architecture.md)，构建、测试和发布见 [开发文档](./docs/development.md)。
