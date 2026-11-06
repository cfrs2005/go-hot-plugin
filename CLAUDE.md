# go-hot-plugin

开源仓库 `github.com/cfrs2005/go-hot-plugin`（Public，MIT；module 路径同名，本地目录名仍是 goplus）。Go 热插拔插件 demo：宿主 `demo-go` 进程不重启，按管理平台（`cmd/platform`）的 webhook + reconcile 热加载 / 热升级 / 回滚 / 退役厂商插件。插件 = 独立子进程，HTTP over unix socket，契约在 `pkg/pluginapi`。说明见 `README.md`（入口，面向开源读者），细节补充见 `docs/design.md`，选型调研见 `docs/research-hot-plugin-2026-11-05.md`。

## 铁律

- **改完必跑 `make verify`**（build → vet → `go test -race` → 9 条端到端断言起真进程）。只编译通过不算验证。
- **契约改动必须 bump `pluginapi.InterfaceVersion`**（字段、路径、语义、环境变量任何一项）。宿主拒绝 `/health` 自报版本不一致的插件。
- **`internal/registry` 不许 import `internal/loader`**：registry 只认 `pluginapi.Plugin`，进程细节（pid、重启次数）由宿主用类型断言取。
- 依赖只允许 `modernc.org/sqlite`；不用 `plugin` 包、hashicorp/go-plugin、WASM（理由见 README「设计裁定」）。
- `Makefile` 固定 `GOTOOLCHAIN=local`：modernc.org/sqlite 新版要求 Go 1.26，本项目钉在 v1.44.3 以兼容本机 Go 1.25；升级 sqlite 前先确认 go 版本。
- unix socket 路径上限 104 字节（macOS），工作目录过深时 demo-go 启动即报错。
- **README 里的 Go 代码摘录是从源码原样复制的**（`pluginapi/api.go` 契约、`vendor-a-v2/main.go`、`artifact.go` 的 SignedMessage/Sign/VerifySignature/Fetch、`platform/main.go` 的 activate、`demo-go/main.go` 的 query 调用段、`registry.go` 的 Acquire/Activate/drain/release/tryClose、`loader.go` 的 spawn/supervise/Close、`serve.go` 的父进程检查）。改这些函数要同步 README，不许留手写近似版。
- 公开仓库：不许出现本机绝对路径、邮箱、密钥；`var/`（含平台私钥）与 `bin/` 已 gitignore。
- 不做 git 操作除非明确要求。

## 目录

- `cmd/platform/`：管理平台（`main.go` HTTP + 签名 + webhook，`store.go` SQLite）。
- `cmd/demo-go/`：宿主（`main.go` HTTP/status/启动，`apply.go` activate/retire/reconcile）。
- `cmd/vendorsim/`：假厂商 API。
- `cmd/verify/`：端到端验收（`main.go` 进程与工具，`steps.go` 九条断言）。
- `plugins/vendor-a-v1|vendor-a-v2|vendor-b/`：三个插件 main。
- `pkg/pluginapi/`：接口 1 契约 + 插件侧 `Serve`。
- `internal/registry|loader|artifact/`：版本注册表 / 子进程生命周期 / 下载与签名校验。
- `demo.sh`：一键分步演示（up / 1..6 / all / status / down），给人看的；改了 HTTP 接口或 status 字段要同步它并跑 `./demo.sh down && ./demo.sh all`。
- `LICENSE`：MIT。
- `docs/`：设计补充与调研。`bin/`、`var/` 为生成物（gitignore）。
