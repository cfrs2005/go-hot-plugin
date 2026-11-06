# go-hot-plugin 设计补充

> **[README](../README.md) 是入口**：问题、架构图、发布时序、registry 并发模型、loader 生命周期、安全模型、设计裁定都在那里。本文只放 README 没有展开的细节：完整契约附录、状态转换条件表、平台与宿主 API 全表、数据库结构、`make verify` 各步的内部实现。

## 契约附录（接口 1 完整规定）

README §3.2 给出了端点和 JSON 结构，这里补齐实现者需要知道的全部约束。

**进程**

- 插件是一个可执行文件，宿主以 `exec(binary)` 启动，不带命令行参数，工作目录继承宿主。
- 环境变量只有 `PATH`、`PLUGIN_SOCKET`、`PLUGIN_INTERFACE`、`VENDOR_BASE_URL`；插件不能指望读到宿主的其他环境变量。
- stdout / stderr 直接接到宿主 stderr，建议每行带 `vendor@version pid` 前缀。
- 收到 SIGTERM（或 SIGINT）应在 2 秒内退出，否则宿主 SIGKILL。
- 应定期检查父进程是否还在（`pluginapi.Serve` 每秒比较 `getppid()`），宿主消失时自行退出。

**socket**

- 插件必须在 `PLUGIN_SOCKET` 指定的路径上 `listen("unix")`。宿主在启动前已删除同名旧文件；`Serve` 也会先删再听，退出时删除。
- 路径由宿主决定：`<DEMO_WORK_DIR>/sock/<vendor>-<version>.sock`。macOS 上 `sun_path` 上限 104 字节，宿主启动时若工作目录过深直接报错。

**HTTP**

- HTTP/1.1，`Content-Type: application/json`。宿主用 `http://plugin/...` 作为 URL，Host 头无意义。
- 宿主侧连接池：每个插件最多 32 个空闲连接，空闲 30 秒回收；子进程重启或关闭时宿主主动丢弃全部空闲连接。
- `GET /health`：必须 200，且 `interface` 等于宿主的 `InterfaceVersion`。宿主每 50ms 轮询，单次超时 500ms，总超时 5s。启动之后宿主不再周期性探活（进程存活由监督协程的 `Wait` 判断）。
- `POST /query`：请求体 `{"id": string, "delay_ms": int}`，`id` 为空时应返回 400。成功返回 200 + 统一结构；`vendor` 与 `plugin_version` 是插件自报值，宿主原样透传，不改写。
- 非 2xx 时响应体应为 `{"error": string}`，宿主把它放进 502 `plugin_error` 的 `detail`。
- 宿主把调用方的请求 context 透传给插件请求：调用方断开，宿主到插件的请求也随之取消。

**版本规则**

- 字段、路径、环境变量、语义任一变化都必须 bump `pluginapi.InterfaceVersion`。宿主只接受相等的版本（不做范围协商）。
- 需要平滑过渡时的做法：先发支持新接口的宿主（同时能 spawn 两种版本的插件需要宿主自己做多版本适配，本 demo 未实现），再让厂商重发插件。

## 状态转换条件表

registry 内部状态只有 `standby / active / draining`，`closed` 表示已移出注册表；`installing` 是宿主 `applyActivate()` 中尚未进入 registry 的阶段。

| 从 | 到 | 触发 | 代码位置 | 附带效果 |
|---|---|---|---|---|
| — | installing | 收到 activate 事件，且该 vendor@version 未安装（或仅存在 draining 中的旧实例） | `apply.go` `applyActivate()` | 先验签，签名不过直接 403，不进入 installing |
| installing | （丢弃） | 下载失败（502）、sha256 不符（422）、spawn 失败 / health 超时 / 健康检查前退出（502）、接口版本不符（422） | `artifact.Fetch()`、`loader.Start()` | 已起的子进程被 Kill；临时文件删除；当前活跃版本不变 |
| installing | standby | `registry.Install()` 成功 | `registry.go` `Install()` | 若同版本有 draining 中的旧实例，map 中替换为新实例，旧实例排空后 `forget` 只删自己 |
| standby | active | `Activate(vendor, 本版本, *)` | `registry.go` `Activate()` | `atomic.Pointer.Swap`；返回旧活跃版本号 |
| active | draining | 别的版本 `Activate(..., keepPrevious=false)`（升级 / reconcile） | `Activate()` → `drain()` | 新请求立刻看不到它；在途请求继续 |
| active | standby | 别的版本 `Activate(..., keepPrevious=true)`（平台判定为回滚） | `Activate()` | 进程保留；再次 activate 它只做指针交换 |
| active | draining | `RemoveVendor(vendor)`（reconcile 发现平台不再期望该 vendor） | `RemoveVendor()` | 活跃指针置 nil，之后该 vendor 的查询 404 |
| active | （拒绝） | `Retire` 活跃版本 | `Retire()` → `ErrActive` | 宿主 409 `version_is_active`；平台侧同样 409 |
| standby | draining | `Retire(vendor, version)`（retire 事件或 reconcile 发现平台已退役） | `Retire()` → `drain()` | — |
| draining | closed | `refs == 0`（drain 当下已为 0，或最后一个在途请求 `release()`） | `tryClose()` | `closeOnce` 起协程：`Plugin.Close()` → `forget()` → `close(e.closed)` |
| draining | draining | 收到同版本 activate | `Get()` 视 draining 为未安装 | 走 installing 重新下载、spawn 新实例 |

与状态正交的「可用性」（`/admin/status` 的 `up` 字段）由 loader 维护：子进程意外退出 → `up=false`（查询 503）→ 重启成功 → `up=true`；重启额度用完则一直 `up=false`，registry 状态不变。

## 平台 API

| 方法 | 路径 | 请求 | 响应 |
|---|---|---|---|
| GET | `/healthz` | — | `{"ok":true}` |
| GET | `/api/pubkey` | — | `{"alg":"ed25519","pubkey":"<base64 32 字节>"}` |
| POST | `/api/plugins` | multipart：`vendor`、`version`（`^[A-Za-z0-9._-]{1,64}$`）、`file` | 201 插件行（含 `sha256`、`signature`）；400 参数错；409 `version_exists` |
| GET | `/api/plugins` | — | 插件行数组，含 `active`（bool）、`retired_at` |
| POST | `/api/plugins/{vendor}/{version}/activate?notify=true\|false` | — | `{"vendor","version","keep_previous","notified":[{"url","status","body","error"}]}`；404 `not_found` |
| POST | `/api/plugins/{vendor}/retire?version=` | — | `{"vendor","version","notified":[...]}`；404；活跃版本 409 `version_is_active` |
| GET | `/api/plugins/{vendor}/{version}/download` | — | 二进制（`application/octet-stream`） |
| POST | `/api/subscribe` | `{"callback_url":"http..."}` | `{"callback_url"}`，按 URL 去重 |
| GET | `/api/desired` | — | `{"vendors":{"A":<activate 事件>},"retired":{"A":["v2"]}}` |

`notify` 缺省为 true。activate 会清掉目标版本的 `retired_at`（退役后又重新启用）。webhook 投递超时 15 秒，各订阅者并行，投递结果原样返回给调用方，不重试。

**webhook 载荷**（`internal/artifact.Event`）：

```json
{"event":"activate","vendor":"A","version":"v2","sha256":"<hex>","signature":"<base64>","download_url":"http://127.0.0.1:9100/api/plugins/A/v2/download","keep_previous":false}
```

retire 事件只有 `event`、`vendor`、`version`。`download_url` 的前缀来自 `PLATFORM_PUBLIC_URL`（默认 `http://<PLATFORM_ADDR>`）。

**SQLite 结构**（`cmd/platform/store.go`，WAL，单连接）：

```sql
plugins(id INTEGER PRIMARY KEY AUTOINCREMENT, vendor, version, sha256, signature, path,
        created_at, retired_at, UNIQUE(vendor, version))
active(vendor PRIMARY KEY, version, updated_at)
subscribers(id INTEGER PRIMARY KEY AUTOINCREMENT, callback_url UNIQUE, created_at)
```

`keep_previous` 用 `plugins.id`（上传顺序）比较，不解析版本字符串。私钥存 `<PLATFORM_DATA_DIR>/ed25519.key`（base64，0600），首次启动生成。

## 宿主 API

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/healthz` | `{"ok":true,"ready":bool}`，ready = 已取到平台公钥 |
| GET | `/query?vendor=&id=&delay_ms=` | 200 统一结构；400 缺参数；404 `plugin_not_installed`；503 `plugin_unavailable`；502 `plugin_error` |
| POST | `/internal/plugin-event` | 接收 webhook。200 回执 `{"vendor","active","previous","installed","keep_previous"}`；拒绝时 `{"error","detail"}`，状态码见下 |
| GET | `/admin/status` | `{"pid","reconcile_interval","vendors":{"A":{"active","versions":[{"version","state","pid","refs","up","restarts","sha256"}]}}}` |
| POST | `/admin/reconcile` | 立即对齐一次，`{"actions":[...]}` |

事件拒绝码：400 `bad_event` / `bad_vendor_or_version` / `unknown_event`；403 `signature_invalid`；409 `sha256_mismatch`（已装同版本产物不同）/ `version_is_active` / `install_failed`；422 `sha256_mismatch`（下载产物不符）/ `interface_mismatch`；502 `download_failed` / `spawn_failed`；503 `not_ready`。

启动顺序：监听端口 → 取公钥（重试 30 秒）→ 订阅 webhook → 首次 reconcile → 周期 reconcile。收到 SIGTERM 时并行 Close 全部插件后退出。

## `make verify` 的内部实现

`cmd/verify/main.go` 负责进程与 HTTP 工具，`cmd/verify/steps.go` 负责断言。

**环境**：先试 `listen` 三个端口，被占则退出码 2；清空 `var/verify/`；按 vendorsim → platform → demo-go 顺序 `exec` 并等各自 `/healthz`（demo-go 等到 `ready=true`）。三个进程的输出写 `var/verify/logs/*.log`。每次调 `/admin/status` 都把看到的插件 pid 记进 `seenPID`。

**对照数据**：vendorsim 的商品名为 `"<vendor> 商品 #<id>"`，价格为 `100 + fnv32a("<vendor>/<id>") % 99900` 分，A v1 / A v2 两个接口返回同一份数据的不同形状。断言里的「与 vendorsim 一致」是 verify 直接请求 vendorsim 原始接口、自己解析后比较。

| 步 | 内部做法 |
|---|---|
| 1 | 读 status 记下宿主 pid，查 A 期望 404 |
| 2 | multipart 上传 → activate，断言响应 JSON 含 `"status":200`（webhook 回执）；每 50ms 轮询查询，最多 3 秒 |
| 3 | 上传并发布 B，查 B id=7 与 vendorsim 比对；再查 A |
| 4 | 读 status 拿 v1 pid → 协程发 `delay_ms=3000` 查询（结果进 channel）→ sleep 200ms → activate v2 → 立即读 status，用「channel 仍为空」证明慢请求在途，同时断言 v1 `refs≥1`、`draining` → 查 id=1 期望 v2 → 等 channel（最多 6 秒）→ 每 50ms 轮询最多 2 秒：`kill(pid, 0)` 失败且 status 里没有 v1 |
| 5 | activate v1，断言响应含 `"keep_previous":true` → 查询 v1 → status 中 v2 为 standby 且 `kill -0` 成功 → retire v2 → 轮询 2 秒等进程消失 |
| 6 | 从 `/api/plugins` 取 A v2 的真 sha256 与签名、A v1 的签名，构造三种事件直接 POST 到宿主：①sha 全 0 + v2 真签名（期望 403 `signature_invalid`）②真 sha + 真签名 + `download_url` 指向 B v1 的二进制（期望 422 `sha256_mismatch`）③真 sha + A v1 的签名（期望 403）。每次之后查 A 仍 v1，且 status 里没有 v2 |
| 7 | 取 A 活跃版本 pid，`syscall.Kill(pid, SIGKILL)`；每 50ms 查询最多 5 秒，统计状态码分布；断言宿主 pid 不变、新 pid ≠ 旧 pid、`restarts == 1` |
| 8 | 以 `version=v2` 再上传 vendor-b 二进制，`notify=false` 发布，断言响应含 `"notified":[]`；sleep 300ms 断言 B 仍 v1；POST `/admin/reconcile`，轮询 5 秒等 B 活跃为 v2 |
| 9 | 比较宿主 pid |

**收尾**：逆序 SIGTERM（先 demo-go，让它回收插件），每个进程最多等 5 秒再 SIGKILL；sleep 300ms 后对 `seenPID` 逐个 `kill -0`，仍存活的记为 FAIL 并强杀。全部通过则删 `var/verify/`（`VERIFY_KEEP=1` 时保留），失败则保留日志并以退出码 1 结束。
