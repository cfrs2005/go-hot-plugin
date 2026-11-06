# Go 服务厂商插件热插拔调研（2026-11-05）

背景：K8s pod 内的 Go 服务 `demo-go`，厂商数据查询插件由管理平台 A 分发。
需求：接入厂商 A v1 不重启加载；接入厂商 B 同理；厂商 A 发 v2 要热升级，最好 v1/v2 并存可回滚。
插件职责：收查询参数 → 调厂商 HTTP API → 映射成统一结构返回。

本地环境：Go 1.25.0 darwin/arm64（注意：Go 1.27 已于 2026-08-19 发布，1.25 已停止支持，demo 不受影响）。

## 一、五条路线总览

| 路线 | 热加载 | 热卸载 | v1/v2 并存回滚 | 厂商独立编译 | 隔离 | 单次调用开销 | 结论 |
|---|---|---|---|---|---|---|---|
| 标准库 `plugin`(.so) | 可 | **不可**（#20461，Go 团队 2025-12 明确不会加） | 只能叠加，内存只增 | **否**，必须同 toolchain/go.mod/flag | 无，同地址空间 | ~30ns | **否决** |
| yaegi 解释器 | 可 | 不可靠，无 Close，靠 GC | API 上可以，释放不可靠 | 源码 vendor，仅支持 Go 1.21/1.22 符号 | 无，`net/http`/`os` 默认可用 | 纯解释慢 ~100x | **否决** |
| hashicorp/go-plugin（子进程+gRPC） | 可 | 可，`Kill()` 真释放 | 可，每版本一个子进程 | 是，只约定 proto | 进程级（同 UID） | ~100–400µs | **可行，推荐做 demo** |
| wazero WASM | 可 | 可（有 #2558/#1913 坑） | 可，按名@版本 | 是，只约定 wasm ABI | 沙箱，内存可限、ctx 可打断 | 未找到权威数据 | 可行，但插件不能自己联网 |
| 每厂商一个 Deployment | K8s 滚动 | pod 级 | 蓝绿/金丝雀现成 | 是 | 容器级最强 | ~1ms 网络跳 | 运维最稳，但不满足「pod 内」 |

## 二、各路线要点与出处

### 标准库 plugin
- 不能卸载：https://github.com/golang/go/issues/20461 （Ian Lance Taylor 2025-12：unlikely that this will ever be added）
- 编译指纹：宿主与插件必须同 toolchain、同依赖、同 flag；`-trimpath`/`-race`/GOPATH 差异都会报 `plugin was built with a different version of package`（#51955、#26759、#31278、#58639）
- 同 pluginpath 二次加载报 `plugin already loaded`，v2 必须换文件路径且换 pluginpath（runtime/plugin.go 源码推断，未实测）
- 必须 CGO_ENABLED=1，scratch/distroless-static 镜像不可用
- 仅 linux/darwin/freebsd

### yaegi
- v0.16.3 于 2026-11-05 刚发，上一版 2024-04，低频维护；stdlib 符号只到 go1_22
- Traefik 换 Go 1.27 后全部 provider 插件 panic（traefik#14197）
- 无 Close/Dispose，cogentcore 曾尝试让其释放内存失败后于 2026-10 弃用（cogentcore#1589）
- Traefik 自己加插件也要改 static 配置，不是热加载；高 QPS 下内存/CPU 问题（traefik#12213）

### hashicorp/go-plugin
- v1.8.0（2026-04-29），近一年 2 个 release；README https://github.com/hashicorp/go-plugin
- 机制：host `plugin.NewClient` 起子进程 → stdout 握手行 → gRPC over unix socket → `Dispense`；`Kill()` 先 Close 等 2s 再强杀
- 多版本：一个 Client 一个子进程，v1/v2 天然并存；`VersionedPlugins` 可协商协议版本（Terraform protocol 5/6 走这个）
- 热升级可抄 Vault：注册多版本 → pin → reload；Vault 是先杀后起，无损切换要自己写（atomic 指针 + 引用计数）
  https://developer.hashicorp.com/vault/docs/plugins/upgrade
- 性能：uberswe/go-plugin-benchmark 约 413µs/op；gRPC over UDS 一般中位数 ~167µs
- K8s 落地：二进制下载到 emptyDir + chmod +x；`readOnlyRootFilesystem` 不冲突；UDS 目录用 `PLUGIN_UNIX_SOCKET_DIR` 指到 emptyDir；K8s 1.37 的 `bindMountOptions: [noexec]` 若启用需豁免插件目录
- 安全：`SecureConfig` 校验二进制 sha256；`AutoMTLS`；`SkipHostEnv: true` 防环境变量泄露；默认同 UID，降权需 CAP_SETUID（未验证）
- 存疑：每个子进程 RSS 无权威数据；benchmark 未注明传输方式

### wazero WASM
- wazero v1.12.0（2026-05-29），纯 Go 零 cgo，不支持组件模型/WASI p2；extism go-sdk 半年无发版，仅作设计参考
- 插件用 Go 1.24+ `GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared`（`//go:wasmexport`，https://go.dev/blog/wasmexport），产物 ~2MB；TinyGo ~100KB 但 reflect/encoding/json 有缺口
- **硬伤**：wasip1 无 socket，插件调厂商 API 必须靠宿主 host function `http_call`，TLS/连接池/超时/白名单/密钥注入全在宿主，ABI 成长期契约；sqlc 维护者原话 "Codegen plugins work because they don't need to talk to the network"
- 单实例不可并发，需要实例池；线性内存只增不减
- 卸载 bug：wazero#2558（ctx 取消后 Close 无效）、#1913（相同字节码共用缓存，关一个另一个失效）、traefik#13235 热重载泄漏
- 性能：计算密集约原生 4.7x（https://00f.net/2026/06/23/webassembly-runtimes-2026/）；Go→wasm 调用 ~57ns
- 生产用户：Trivy、Dapr、sqlc、Helm v4、Redpanda Connect

### 每厂商一个 Deployment
- 升级/灰度/回滚/资源上限/NetworkPolicy 出口白名单全用 K8s 现成能力
- 代价：多 pod、多一跳网络、厂商多时 Service 膨胀
- 研究代理判断「pod 内热加载是伪需求」，除非单 pod 要服务几十上百个租户自助上架的厂商

## 三、管理平台 A → pod 分发机制

| 机制 | 延迟 | 可审计 | 签名 | 只读根 FS | 复杂度 |
|---|---|---|---|---|---|
| (a) pod 轮询平台 HTTP | 轮询间隔 10–60s | 中 | 进程内验 ed25519/cosign | 兼容，emptyDir | 低 |
| (b) 平台推 webhook | 秒级 | 中，会丢，需 (a) 兜底 | 同上 | 兼容 | 中，平台需知 pod 地址；推荐「推通知 + pod 自己拉」 |
| (c) ConfigMap + fsnotify | ~1 分钟 | 高（API 审计/GitOps） | 自己放签名进 CM | 兼容 | 坑：1MiB 上限、subPath 不更新、要监听 `..data` 符号链接 |
| (d) OCI artifact（oras-go v2） | 秒级 | 高（digest/tag 历史） | cosign 原生 | 兼容 | 中；K8s ImageVolume 是 pod 级不可变，做不了热更新 |
| (e) sidecar/initContainer | init 只跑一次；sidecar = 外包 (a)/(d) | 中到高 | sidecar 内验 | 兼容 | 中，要先写临时文件再 rename |

签名：单平台分发用 stdlib `crypto/ed25519` 签 `sha256(artifact)+vendor+version` 即可，几十行；cosign/sigstore-go 依赖重、内网要配 trusted root。

## 四、与加载技术无关的热切换模式（demo 核心抽象）

```go
type Plugin interface { Query(ctx, Req) (Resp, error); Close() error }
type Entry struct{ P Plugin; refs atomic.Int64; draining atomic.Bool }
type Route struct{ Stable, Canary *Entry; Pct uint32; Tenants map[string]bool }
// registry: map[vendor]map[version]*Entry + map[vendor]*atomic.Pointer[Route]
// Call: 选 Stable/Canary → refs+1 → Query → refs-1，归零且 draining 则 Close
// Promote: Canary→Stable，旧 Stable 标记 draining，refs 归零后 Close
// Rollback: 把旧 Route Store 回去；旧版本保留到确认后才 drain
```
没有现成 Go 库全包这三件事；最接近 edwingeng/hotswap（基于 plugin 包，旧版不释放）、v8fg/kit4go/hotreload（atomic 双缓冲）。

## 五、推荐
demo 走 **hashicorp/go-plugin**，Registry/Route 层与 Loader 解耦，后续换 WASM 或远程 Service 只换 Loader。
理由：插件可自己发 HTTP、厂商独立编译不绑 Go 版本、Kill 真释放、v1/v2 并存回滚天然成立、库活跃。
生产若厂商数量少且插件由第三方写，优先考虑每厂商一个 Deployment。
