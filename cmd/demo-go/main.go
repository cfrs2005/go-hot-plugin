// demo-go 是宿主服务：进程不重启，按平台 webhook / reconcile 热加载、热升级、回滚、退役厂商插件。
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cfrs2005/go-hot-plugin/internal/artifact"
	"github.com/cfrs2005/go-hot-plugin/internal/registry"
	"github.com/cfrs2005/go-hot-plugin/pkg/pluginapi"
)

var logger = log.New(os.Stderr, fmt.Sprintf("[demo-go pid=%d] ", os.Getpid()), log.LstdFlags|log.Lmicroseconds)

type host struct {
	reg        *registry.Registry
	pub        ed25519.PublicKey
	platform   string
	workDir    string
	vendorBase string
	interval   time.Duration
	applyMu    sync.Mutex  // 串行化安装/切换
	ready      atomic.Bool // 公钥就绪后置 true；读 pub 前先看它
}

func main() {
	addr := envOr("DEMO_ADDR", "127.0.0.1:9102")
	interval, err := time.ParseDuration(envOr("RECONCILE_INTERVAL", "60s"))
	if err != nil {
		logger.Fatalf("RECONCILE_INTERVAL: %v", err)
	}
	h := &host{
		reg:        registry.New(logger.Printf),
		platform:   envOr("PLATFORM_URL", "http://127.0.0.1:9100"),
		workDir:    mustAbs(envOr("DEMO_WORK_DIR", "var/demo-go")),
		vendorBase: envOr("VENDOR_BASE_URL", "http://127.0.0.1:9101"),
		interval:   interval,
	}
	callback := envOr("DEMO_CALLBACK_URL", "http://"+addr+"/internal/plugin-event")

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "ready": h.ready.Load()})
	})
	mux.HandleFunc("GET /query", h.query)
	mux.HandleFunc("POST /internal/plugin-event", h.event)
	mux.HandleFunc("GET /admin/status", h.status)
	mux.HandleFunc("POST /admin/reconcile", func(w http.ResponseWriter, r *http.Request) {
		acts, err := h.reconcile(r.Context())
		if err != nil {
			writeJSON(w, 502, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"actions": acts})
	})

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	logger.Printf("监听 %s，工作目录 %s，reconcile 间隔 %s", addr, h.workDir, interval)

	if err := h.bootstrap(callback); err != nil {
		logger.Fatalf("启动失败: %v", err)
	}
	go h.reconcileLoop()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	logger.Printf("收到退出信号，关闭全部插件")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	var wg sync.WaitGroup
	for _, vs := range h.reg.Snapshot() {
		for _, v := range vs.Versions {
			wg.Add(1)
			go func() { defer wg.Done(); _ = v.Entry.Plugin.Close() }()
		}
	}
	wg.Wait()
}

// bootstrap：取平台公钥（TOFU）→ 订阅 webhook → 首次 reconcile。
func (h *host) bootstrap(callback string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var pk struct{ Pubkey string }
	for {
		err := getJSON(ctx, h.platform+"/api/pubkey", &pk)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return fmt.Errorf("取平台公钥: %w", err)
		}
		time.Sleep(300 * time.Millisecond)
	}
	raw, err := base64.StdEncoding.DecodeString(pk.Pubkey)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return errors.New("平台公钥格式错误")
	}
	h.pub = ed25519.PublicKey(raw)
	h.ready.Store(true)
	logger.Printf("TOFU 信任平台公钥 %s（生产环境必须固定公钥）", pk.Pubkey)

	body, _ := json.Marshal(map[string]string{"callback_url": callback})
	resp, err := http.Post(h.platform+"/api/subscribe", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("订阅 webhook: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("订阅 webhook: http %d", resp.StatusCode)
	}
	logger.Printf("已订阅 webhook → %s", callback)
	if _, err := h.reconcile(ctx); err != nil {
		logger.Printf("首次 reconcile 失败（等下一轮）: %v", err)
	}
	return nil
}

func (h *host) reconcileLoop() {
	t := time.NewTicker(h.interval)
	defer t.Stop()
	for range t.C {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if _, err := h.reconcile(ctx); err != nil {
			logger.Printf("reconcile 失败: %v", err)
		}
		cancel()
	}
}

func (h *host) event(w http.ResponseWriter, r *http.Request) {
	var ev artifact.Event
	if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
		writeJSON(w, 400, map[string]string{"error": "bad_event"})
		return
	}
	if !h.ready.Load() {
		writeJSON(w, 503, map[string]string{"error": "not_ready"})
		return
	}
	var res map[string]any
	var err error
	switch ev.Event {
	case "activate":
		res, err = h.applyActivate(r.Context(), ev)
	case "retire":
		res, err = h.applyRetire(ev)
	default:
		writeJSON(w, 400, map[string]string{"error": "unknown_event"})
		return
	}
	var ae *applyErr
	if errors.As(err, &ae) {
		logger.Printf("拒绝 %s %s@%s: %v（当前版本保持不变）", ev.Event, ev.Vendor, ev.Version, err)
		writeJSON(w, ae.code, map[string]string{"error": ae.reason, "detail": ae.err.Error()})
		return
	}
	writeJSON(w, 200, res)
}

func (h *host) query(w http.ResponseWriter, r *http.Request) {
	vendor, id := r.URL.Query().Get("vendor"), r.URL.Query().Get("id")
	if vendor == "" || id == "" {
		writeJSON(w, 400, map[string]string{"error": "vendor_and_id_required"})
		return
	}
	delay, _ := strconv.Atoi(r.URL.Query().Get("delay_ms"))
	e, release, err := h.reg.Acquire(vendor)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "plugin_not_installed"})
		return
	}
	defer release()
	resp, err := e.Plugin.Query(r.Context(), pluginapi.QueryRequest{ID: id, DelayMS: delay})
	switch {
	case errors.Is(err, pluginapi.ErrUnavailable):
		writeJSON(w, 503, map[string]string{"error": "plugin_unavailable"})
	case err != nil:
		writeJSON(w, 502, map[string]string{"error": "plugin_error", "detail": err.Error()})
	default:
		writeJSON(w, 200, resp)
	}
}

type versionStatus struct {
	Version  string `json:"version"`
	State    string `json:"state"`
	PID      int    `json:"pid"`
	Refs     int64  `json:"refs"`
	Up       bool   `json:"up"`
	Restarts int    `json:"restarts"`
	SHA256   string `json:"sha256"`
}

type vendorStatus struct {
	Active   string          `json:"active"`
	Versions []versionStatus `json:"versions"`
}

// procInfo 是 loader.Proc 额外暴露的观测接口；status 只靠类型断言拿，不让 registry 依赖 loader。
type procInfo interface {
	PID() int
	Up() bool
	Restarts() int
}

func (h *host) status(w http.ResponseWriter, r *http.Request) {
	out := map[string]vendorStatus{}
	for _, vs := range h.reg.Snapshot() {
		s := vendorStatus{Active: vs.Active, Versions: []versionStatus{}}
		for _, v := range vs.Versions {
			st := versionStatus{Version: v.Entry.Version, State: string(v.State), Refs: v.Entry.Refs(), SHA256: v.Entry.SHA256}
			if p, ok := v.Entry.Plugin.(procInfo); ok {
				st.PID, st.Up, st.Restarts = p.PID(), p.Up(), p.Restarts()
			}
			s.Versions = append(s.Versions, st)
		}
		out[vs.Vendor] = s
	}
	writeJSON(w, 200, map[string]any{"pid": os.Getpid(), "reconcile_interval": h.interval.String(), "vendors": out})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
