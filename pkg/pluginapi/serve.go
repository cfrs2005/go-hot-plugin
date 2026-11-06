package pluginapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// QueryFunc 是插件作者唯一要写的逻辑：调厂商 API 并映射成统一结构。
// baseURL 来自 VENDOR_BASE_URL。Vendor / PluginVersion 字段由 Serve 自动补齐。
type QueryFunc func(ctx context.Context, baseURL string, req QueryRequest) (QueryResponse, error)

// Serve 是插件侧帮助函数：读环境变量、监听 unix socket、提供 /health 与 /query，
// 收到 SIGTERM 或父进程消失时退出。正常情况下不返回。
func Serve(vendor, version string, fn QueryFunc) {
	logger := log.New(os.Stderr, fmt.Sprintf("[plugin %s@%s pid=%d] ", vendor, version, os.Getpid()), log.LstdFlags|log.Lmicroseconds)
	sock := os.Getenv(EnvSocket)
	if sock == "" {
		logger.Fatalf("缺少环境变量 %s", EnvSocket)
	}
	if want := os.Getenv(EnvInterface); want != "" && want != strconv.Itoa(InterfaceVersion) {
		// 不直接退出：让宿主通过 /health 看到不一致并拒绝，日志里留痕即可。
		logger.Printf("宿主期望接口 %s，本插件实现接口 %d", want, InterfaceVersion)
	}
	base := strings.TrimRight(os.Getenv(EnvVendorBaseURL), "/")

	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		logger.Fatalf("监听 %s 失败: %v", sock, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+PathHealth, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, HealthInfo{Interface: InterfaceVersion, Vendor: vendor, Version: version, PID: os.Getpid()})
	})
	mux.HandleFunc("POST "+PathQuery, func(w http.ResponseWriter, r *http.Request) {
		var req QueryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == "" {
			writeJSON(w, http.StatusBadRequest, ErrorBody{Error: "bad_request"})
			return
		}
		resp, err := fn(r.Context(), base, req)
		if err != nil {
			logger.Printf("query id=%s 失败: %v", req.ID, err)
			writeJSON(w, http.StatusBadGateway, ErrorBody{Error: err.Error()})
			return
		}
		resp.Vendor, resp.PluginVersion = vendor, version
		if resp.ID == "" {
			resp.ID = req.ID
		}
		writeJSON(w, http.StatusOK, resp)
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("serve: %v", err)
		}
	}()
	logger.Printf("已启动，socket=%s base=%s", sock, base)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	ppid := os.Getppid()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
loop:
	for {
		select {
		case s := <-sig:
			logger.Printf("收到 %v，退出", s)
			break loop
		case <-tick.C:
			if os.Getppid() != ppid { // 宿主没了就自己退，不留孤儿
				logger.Printf("父进程 %d 已消失，退出", ppid)
				break loop
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	_ = os.Remove(sock)
}

// GetJSON 是给插件用的小工具：GET 厂商 API 并解码 JSON。
func GetJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("vendor http %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// DelayQuery 把 delay_ms 拼成查询串（为 0 时返回空串）。
func DelayQuery(ms int) string {
	if ms <= 0 {
		return ""
	}
	return "?delay_ms=" + strconv.Itoa(ms)
}

// ParseDecimalCents 把 "12.34" 这类十进制金额字符串转成分（1234）。
func ParseDecimalCents(s string) (int64, error) {
	s = strings.TrimSpace(s)
	whole, frac, _ := strings.Cut(s, ".")
	if len(frac) > 2 {
		return 0, fmt.Errorf("金额精度超过 2 位: %q", s)
	}
	frac = (frac + "00")[:2]
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("金额格式错误: %q", s)
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("金额格式错误: %q", s)
	}
	return w*100 + f, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
