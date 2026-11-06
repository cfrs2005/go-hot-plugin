// verify 是端到端验收程序：起 vendorsim / platform / demo-go 真进程，逐条断言热插拔行为。
// 用法（在仓库根目录）：make verify   或   go run ./cmd/verify（需先 make build）
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var (
	root    = mustCwd()
	binDir  = filepath.Join(root, "bin")
	varDir  = filepath.Join(root, "var", "verify")
	logDir  = filepath.Join(varDir, "logs")
	portSim = envOr("VERIFY_PORT_VENDORSIM", "19101")
	portPla = envOr("VERIFY_PORT_PLATFORM", "19100")
	portDem = envOr("VERIFY_PORT_DEMO", "19102")
	simURL  = "http://127.0.0.1:" + portSim
	plaURL  = "http://127.0.0.1:" + portPla
	demURL  = "http://127.0.0.1:" + portDem
	client  = &http.Client{Timeout: 20 * time.Second}
	procs   []*exec.Cmd
	seenPID = map[int]string{} // 观察到的所有插件子进程 pid，收尾时确认全部退出
)

func main() {
	for _, p := range []string{portSim, portPla, portDem} {
		ln, err := net.Listen("tcp", "127.0.0.1:"+p)
		if err != nil {
			fmt.Printf("端口 %s 被占用，无法验收（可用 VERIFY_PORT_* 覆盖）: %v\n", p, err)
			os.Exit(2)
		}
		ln.Close()
	}
	_ = os.RemoveAll(varDir)
	must(os.MkdirAll(logDir, 0o755))

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() { <-sig; stopAll(); os.Exit(130) }()

	start("vendorsim", "vendorsim", "VENDORSIM_ADDR=127.0.0.1:"+portSim)
	waitHTTP(simURL + "/healthz")
	start("platform", "platform", "PLATFORM_ADDR=127.0.0.1:"+portPla, "PLATFORM_DATA_DIR="+filepath.Join(varDir, "platform"))
	waitHTTP(plaURL + "/healthz")
	start("demo-go", "demo-go", "DEMO_ADDR=127.0.0.1:"+portDem, "DEMO_WORK_DIR="+filepath.Join(varDir, "demo-go"),
		"PLATFORM_URL="+plaURL, "VENDOR_BASE_URL="+simURL, "RECONCILE_INTERVAL=1h")
	waitReady()

	fmt.Printf("== go-hot-plugin 热插拔验收（vendorsim %s / platform %s / demo-go %s，RECONCILE_INTERVAL=1h）==\n\n", portSim, portPla, portDem)
	runSteps()
	stopAll()
	leaks := leakedPlugins()
	fmt.Println()
	if leaks != "" {
		record(false, "收尾：宿主退出后插件子进程全部回收", "仍存活: %s", leaks)
	} else {
		fmt.Printf("收尾：宿主退出后 %d 个观察到的插件子进程全部回收\n", len(seenPID))
	}
	fmt.Printf("\n结果：%d 条 PASS，%d 条 FAIL\n", passed, failed)
	if failed > 0 {
		fmt.Printf("保留日志供排查：%s\n", logDir)
		os.Exit(1)
	}
	if os.Getenv("VERIFY_KEEP") == "1" {
		fmt.Printf("VERIFY_KEEP=1，保留日志：%s\n", logDir)
		return
	}
	_ = os.RemoveAll(varDir)
	_ = os.Remove(filepath.Join(root, "var")) // 只在为空时成功
}

// ---------- 进程管理 ----------

func start(name, bin string, env ...string) {
	f, err := os.Create(filepath.Join(logDir, name+".log"))
	must(err)
	cmd := exec.Command(filepath.Join(binDir, bin))
	cmd.Dir = root
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = f, f
	must(cmd.Start())
	procs = append(procs, cmd)
}

func stopAll() {
	for i := len(procs) - 1; i >= 0; i-- { // 先停 demo-go，让它回收插件
		p := procs[i]
		_ = p.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = p.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = p.Process.Kill()
			<-done
		}
	}
	procs = nil
}

func leakedPlugins() string {
	time.Sleep(300 * time.Millisecond)
	var out []string
	for pid, name := range seenPID {
		if alive(pid) {
			out = append(out, fmt.Sprintf("%s pid=%d", name, pid))
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	return strings.Join(out, ", ")
}

func alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }

func waitHTTP(url string) {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := client.Get(url); err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Printf("等待 %s 超时，日志见 %s\n", url, logDir)
	stopAll()
	os.Exit(2)
}

func waitReady() {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var h struct{ Ready bool }
		if code, _ := getJSON(demURL+"/healthz", &h); code == 200 && h.Ready {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Printf("demo-go 未就绪，日志见 %s\n", logDir)
	stopAll()
	os.Exit(2)
}

// ---------- HTTP 小工具 ----------

func do(req *http.Request, out any) (int, string) {
	resp, err := client.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil {
		_ = json.Unmarshal(b, out)
	}
	return resp.StatusCode, strings.TrimSpace(string(b))
}

func getJSON(url string, out any) (int, string) {
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	return do(req, out)
}

func postJSON(url string, body any, out any) (int, string) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(http.MethodPost, url, rd)
	req.Header.Set("Content-Type", "application/json")
	return do(req, out)
}

func upload(vendor, version, binName string) (int, string) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("vendor", vendor)
	_ = mw.WriteField("version", version)
	fw, _ := mw.CreateFormFile("file", "plugin")
	f, err := os.Open(filepath.Join(binDir, "plugins", binName))
	must(err)
	_, _ = io.Copy(fw, f)
	f.Close()
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, plaURL+"/api/plugins", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return do(req, nil)
}

func activate(vendor, version string, notify bool) (int, string) {
	return postJSON(fmt.Sprintf("%s/api/plugins/%s/%s/activate?notify=%v", plaURL, vendor, version, notify), nil, nil)
}

type queryResp struct {
	Vendor        string `json:"vendor"`
	ID            string `json:"id"`
	Name          string `json:"name"`
	PriceCents    int64  `json:"price_cents"`
	PluginVersion string `json:"plugin_version"`
	Error         string `json:"error"`
}

func query(vendor, id string, delayMS int) (int, queryResp, string) {
	var q queryResp
	code, raw := getJSON(fmt.Sprintf("%s/query?vendor=%s&id=%s&delay_ms=%d", demURL, vendor, id, delayMS), &q)
	return code, q, raw
}

type verStatus struct {
	Version  string `json:"version"`
	State    string `json:"state"`
	PID      int    `json:"pid"`
	Refs     int64  `json:"refs"`
	Up       bool   `json:"up"`
	Restarts int    `json:"restarts"`
}

type statusResp struct {
	PID     int `json:"pid"`
	Vendors map[string]struct {
		Active   string      `json:"active"`
		Versions []verStatus `json:"versions"`
	} `json:"vendors"`
}

func status() (int, statusResp) {
	var s statusResp
	code, _ := getJSON(demURL+"/admin/status", &s)
	for vendor, vs := range s.Vendors {
		for _, v := range vs.Versions {
			if v.PID > 0 {
				seenPID[v.PID] = vendor + "@" + v.Version
			}
		}
	}
	return code, s
}

// version 返回 vendor@version 在 status 里的条目（不存在为 nil）。
func (s statusResp) version(vendor, version string) *verStatus {
	for _, v := range s.Vendors[vendor].Versions {
		if v.Version == version {
			return &v
		}
	}
	return nil
}

// poll 在 timeout 内反复执行 f 直到返回 true，返回是否成功与耗时。
func poll(timeout time.Duration, f func() bool) (bool, time.Duration) {
	t0 := time.Now()
	for {
		if f() {
			return true, time.Since(t0)
		}
		if time.Since(t0) > timeout {
			return false, time.Since(t0)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ---------- 断言记录 ----------

var passed, failed int

// record 打印一行子断言；step 汇总由 step() 完成。
func record(ok bool, what, format string, args ...any) bool {
	tag := "PASS"
	if ok {
		passed++
	} else {
		tag = "FAIL"
		failed++
	}
	fmt.Printf("    %s  %s — 实际: %s\n", tag, what, fmt.Sprintf(format, args...))
	return ok
}

func step(n int, title string, f func()) {
	before := failed
	fmt.Printf("[%d] %s\n", n, title)
	f()
	verdict := "PASS"
	if failed > before {
		verdict = "FAIL"
	}
	fmt.Printf("[%d] %s\n\n", n, verdict)
}

func must(err error) {
	if err != nil {
		fmt.Println("致命错误:", err)
		stopAll()
		os.Exit(2)
	}
}

func mustCwd() string {
	d, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	return d
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
