# 开发

[English](./development_EN.md)

## 环境

- Go 1.26（与 CPA v8 一致）。
- 构建动态库需要 cgo 和目标平台的 C 编译器。Linux 版本在 `golang:1.26-bookworm` 中构建，与 CPA 官方镜像（Debian 12）使用相同的 glibc。
- 运行测试不需要 cgo。`internal/` 下的包都是纯 Go，可以在 Windows、macOS 或 Linux 上测试；只有 `main.go` 需要 cgo。

## 测试

```bash
go vet ./internal/...
go test ./internal/...
node --check internal/web/page.js
```

测试使用假的宿主和 HTTP 服务器，不会发送真实请求。CI 还会检查 `gofmt -l .` 的输出为空。

测试覆盖：

- 三个服务的主动额度解析和被动响应头解析；
- 网络出口选择，包括无效 `proxy_url` 不退回直连；
- 点火时间、滑动重置判断、错误分类和失败保护；
- 候选模型顺序和模型列表读取；
- 额度提醒、重置提醒、Bark 请求格式和 Did Codex Reset 去重；
- 引擎：账号后缀、被动数据跳过规则、点火确认、换模型重试、暂停通知、历史采样；
- 插件 RPC：注册、管理接口、页面、状态中不含密钥。

## 构建

本地有 Docker 时，在当前 Docker 主机的 CPU 架构上构建：

```bash
sh scripts/build.sh 0.1.0
```

产物为 `dist/lamplighter-v0.1.0.so`。文件名中的 `-v<版本>` 让 CPA 识别插件 ID（`lamplighter`）和版本。

## 发布

推送 `v*` 标签后，GitHub Actions（`.github/workflows/build.yml`）：

1. 运行测试；
2. 在 `ubuntu-latest`（amd64）和 `ubuntu-24.04-arm`（arm64）上，用 `golang:1.26-bookworm` 容器构建；
3. 打包为 `lamplighter_<版本>_linux_<架构>.zip`，zip 根目录只有 `lamplighter-v<版本>.so`，这也是 CPA 插件商店要求的格式；
4. 创建 GitHub Release，附带 zip 和 `SHA256SUMS`。

版本号来自标签（`v0.1.0` 对应 `0.1.0`），通过 `-ldflags "-X main.version=..."` 写入插件，并显示在管理中心和页面上。

推送到 `main` 时只构建并上传 Actions 产物，版本号为 `0.0.0-dev.<提交前 7 位>`。

## 修改约定

- 修改宿主回调、RPC 字段或注册内容前，对照 CPA 源码中的 `sdk/pluginabi`、`sdk/pluginapi` 和 `internal/pluginhost`。`go.mod` 中的 CPA 版本与线上最低支持版本保持一致。
- 修改额度请求的地址或请求头时，对照 CPA 管理中心的 `src/utils/quota/constants.ts`，并更新 [架构文档](./architecture.md) 中的表格。
- 修改轮询、跳过规则、点火时间或失败保护时，同步更新测试、README 和架构文档。
- 后台循环中新增的 goroutine 必须有 `recover`，网络请求必须有超时。
- 修改页面时保持 HTML、CSS、JS 分文件；文字插入 DOM 时使用 `textContent`。图表颜色按实体固定分配，不随筛选变化。
- 用户可见的行为、配置和限制写进 README（中英文同步），实现细节写进 `docs/`。

## 真实环境验证

点火会发送真实模型请求并开始新的 5 小时窗口。在生产 CPA 上首次部署时，先把 `ignition.enabled` 设为 `false`，确认额度、被动数据、页面和通知正常后再开启。
