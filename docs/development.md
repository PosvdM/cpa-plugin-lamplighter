# 开发

[English](./development_EN.md)

## 环境

- Go 1.26（与 CPA v8 一致）。
- 构建动态库需要 cgo 和目标平台的 C 编译器。Linux 版本在 `golang:1.26-bookworm` 中构建，与 CPA 官方镜像（Debian 12）使用相同的 glibc；macOS 版本用 Xcode 的 clang，Windows 版本用 MinGW-w64 的 gcc。
- 运行测试不需要 cgo。`internal/` 下的包都是纯 Go，可以在 Windows、macOS 或 Linux 上测试；只有根目录的 `main.go` 和 `datadir_*.go` 需要 cgo。

## 测试

```bash
go vet ./internal/...
go test ./internal/...
node --check internal/web/page.js
```

测试使用假的宿主和 HTTP 服务器，不会发送真实请求。CI 在 Linux、macOS 和 Windows 上运行测试，并在 Linux 上检查 `gofmt -l .` 的输出为空。

测试覆盖：

- 三个服务的主动额度解析和被动响应头解析；
- 网络出口选择，包括无效 `proxy_url` 不退回直连；
- 点火时间、滑动重置判断、错误分类和失败保护；
- 候选模型顺序和模型列表读取；
- 额度提醒、按窗口的恢复通知和重置提醒、Bark 请求格式，以及 Did Codex Reset 的去重、过滤和排期时间；
- webhook 的占位符转义、内置请求体、`success_json`、设置检查和错误中的密钥隐藏，多渠道发送时只要一个渠道成功就算送达，以及没有渠道时关闭推送、只有无效的 webhook 时按失败重试；
- 引擎：账号后缀、被动数据跳过规则、重置前后的查询、点火确认、换模型重试、暂停通知、历史采样；
- 插件 RPC：注册、管理接口、页面、状态中不含密钥；
- 实例锁互斥，以及从 CPA 配置读取插件目录。

## 构建

本地有 Docker 时，在当前 Docker 主机的 CPU 架构上构建：

```bash
sh scripts/build.sh 0.1.0
```

产物为 `dist/lamplighter-v0.1.0.so`。文件名中的 `-v<版本>` 让 CPA 识别插件 ID（`lamplighter`）和版本。

## 发布

推送 `v*` 标签后，GitHub Actions（`.github/workflows/build.yml`）：

1. 运行测试；
2. 用 `scripts/ci-build.sh` 构建五个平台：Linux amd64 和 arm64 分别在 `ubuntu-latest` 和 `ubuntu-24.04-arm` 上的 `golang:1.26-bookworm` 容器中构建；macOS amd64 和 arm64 都在 `macos-latest`（arm64）上构建，amd64 由 clang 交叉编译；Windows amd64 在 `windows-latest` 上构建；
3. 每个平台打包为 `lamplighter_<版本>_<系统>_<架构>.zip`，zip 根目录只有 `lamplighter-v<版本>.<so|dylib|dll>`；
4. 创建 GitHub Release，附带五个 zip 和 `sha256sum` 格式的 `checksums.txt`。

CPA 插件商店从最新的 Release 安装插件：它按上面的名称查找 zip 和 `checksums.txt`，安装前校验 SHA-256。商店要求每个版本都提供这五个平台，缺少任何一个都无法上架。

版本号来自标签（`v0.1.0` 对应 `0.1.0`），通过 `-ldflags "-X main.version=..."` 写入插件，并显示在管理中心和页面上。

推送到 `main` 时只构建并上传 Actions 产物（未打包的动态库），版本号为 `0.0.0-dev.<提交前 7 位>`。

## 修改约定

- 修改宿主回调、RPC 字段或注册内容前，对照 CPA 源码中的 `sdk/pluginabi`、`sdk/pluginapi` 和 `internal/pluginhost`。`go.mod` 中的 CPA 版本与线上最低支持版本保持一致。
- 修改额度请求的地址或请求头时，对照 CPA 管理中心的 `src/utils/quota/constants.ts`，并更新 [架构文档](./architecture.md) 中的表格。
- 修改轮询、跳过规则、点火时间或失败保护时，同步更新测试、README 和架构文档。
- 后台循环中新增的 goroutine 必须有 `recover`，网络请求必须有超时。
- 修改页面时保持 HTML、CSS、JS 分文件；文字插入 DOM 时使用 `textContent`。新增或修改页面文字时，同时修改 `page.js` 中 `I18N` 的中文和英文词条；新增或修改推送文字时，同时修改 `internal/notify/text.go` 的两列。增加一种语言时，两处各加一份。图表颜色只表示剩余额度的高低，使用与额度条相同的三档变量。
- 用户可见的行为、配置和限制写进 README（中英文同步），实现细节写进 `docs/`。

## 真实环境验证

点火会发送真实模型请求并开始新的 5 小时窗口。在生产 CPA 上首次部署时，先把 `ignition.enabled` 设为 `false`，确认额度、被动数据、页面和通知正常后再开启。
