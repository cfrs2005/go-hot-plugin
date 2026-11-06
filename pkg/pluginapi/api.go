// Package pluginapi 是宿主 demo-go 与厂商插件之间的「接口 1」契约。
//
// 插件是独立子进程，宿主 exec 它；两者之间走 HTTP over unix socket，载荷为 JSON。
// 任何字段、路径、语义的改动都必须 bump InterfaceVersion。
package pluginapi

import (
	"context"
	"errors"
)

// InterfaceVersion 是当前契约版本。插件 /health 自报的 interface 必须与之相等，否则宿主拒绝加载。
const InterfaceVersion = 1

// 插件启动时读取的环境变量。
const (
	EnvSocket        = "PLUGIN_SOCKET"    // 插件监听的 unix socket 路径
	EnvInterface     = "PLUGIN_INTERFACE" // 宿主期望的接口版本
	EnvVendorBaseURL = "VENDOR_BASE_URL"  // 厂商 API 根地址
)

// 插件必须提供的 HTTP 路径。
const (
	PathHealth = "/health" // GET  → HealthInfo
	PathQuery  = "/query"  // POST QueryRequest → QueryResponse（失败返回非 2xx + ErrorBody）
)

// QueryRequest 是 POST /query 的入参。
type QueryRequest struct {
	ID      string `json:"id"`
	DelayMS int    `json:"delay_ms"` // 透传给厂商 API，用于模拟慢请求
}

// QueryResponse 是所有厂商统一的出参结构。
type QueryResponse struct {
	Vendor        string `json:"vendor"`
	ID            string `json:"id"`
	Name          string `json:"name"`
	PriceCents    int64  `json:"price_cents"`
	PluginVersion string `json:"plugin_version"`
}

// HealthInfo 是 GET /health 的出参。
type HealthInfo struct {
	Interface int    `json:"interface"`
	Vendor    string `json:"vendor"`
	Version   string `json:"version"`
	PID       int    `json:"pid"`
}

// ErrorBody 是插件非 2xx 响应的载荷。
type ErrorBody struct {
	Error string `json:"error"`
}

// Plugin 是宿主侧看到的插件抽象。registry 只认这个接口，不关心插件如何加载。
type Plugin interface {
	Query(ctx context.Context, req QueryRequest) (QueryResponse, error)
	Health(ctx context.Context) (HealthInfo, error)
	Close() error
}

// ErrUnavailable 表示插件进程当前不可用（崩溃、重启中、已关闭）。宿主映射为 503。
var ErrUnavailable = errors.New("plugin_unavailable")
