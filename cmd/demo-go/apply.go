package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/cfrs2005/go-hot-plugin/internal/artifact"
	"github.com/cfrs2005/go-hot-plugin/internal/loader"
	"github.com/cfrs2005/go-hot-plugin/internal/registry"
)

var nameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// applyErr 带 HTTP 状态码与机器可读原因，事件处理器原样回给调用方。
type applyErr struct {
	code   int
	reason string
	err    error
}

func (e *applyErr) Error() string { return fmt.Sprintf("%s: %v", e.reason, e.err) }

func fail(code int, reason string, err error) error { return &applyErr{code, reason, err} }

// applyActivate：验签 → （未装则）下载+校验 sha → spawn+health → Install → Activate。任一步失败当前版本不变。
func (h *host) applyActivate(ctx context.Context, ev artifact.Event) (map[string]any, error) {
	if !nameRE.MatchString(ev.Vendor) || !nameRE.MatchString(ev.Version) {
		return nil, fail(400, "bad_vendor_or_version", fmt.Errorf("%q@%q", ev.Vendor, ev.Version))
	}
	if err := artifact.VerifySignature(h.pub, ev.SHA256, ev.Vendor, ev.Version, ev.Signature); err != nil {
		return nil, fail(403, "signature_invalid", err)
	}
	logger.Printf("验签通过 %s@%s sha256=%.12s", ev.Vendor, ev.Version, ev.SHA256)
	h.applyMu.Lock()
	defer h.applyMu.Unlock()

	installed := false
	if e := h.reg.Get(ev.Vendor, ev.Version); e != nil {
		if e.SHA256 != ev.SHA256 { // 同版本号不同产物：拒绝，不能静默替换
			return nil, fail(409, "sha256_mismatch", fmt.Errorf("已装 %s@%s sha=%.12s，事件 sha=%.12s", ev.Vendor, ev.Version, e.SHA256, ev.SHA256))
		}
	} else {
		bin := filepath.Join(h.workDir, "plugins", ev.Vendor, ev.Version, "plugin")
		if err := artifact.Fetch(ctx, ev.DownloadURL, bin, ev.SHA256); err != nil {
			if errors.Is(err, artifact.ErrShaMismatch) {
				return nil, fail(422, "sha256_mismatch", err)
			}
			return nil, fail(502, "download_failed", err)
		}
		logger.Printf("下载完成且 sha256 校验通过 %s@%s → %s", ev.Vendor, ev.Version, bin)
		proc, err := loader.Start(ctx, loader.Spec{
			Vendor: ev.Vendor, Version: ev.Version, Binary: bin,
			SocketPath:  filepath.Join(h.workDir, "sock", ev.Vendor+"-"+ev.Version+".sock"),
			Env:         []string{"VENDOR_BASE_URL=" + h.vendorBase},
			MaxRestarts: 3, RestartBackoff: time.Second, StartTimeout: 5 * time.Second,
			Logf: logger.Printf,
		})
		if err != nil {
			if errors.Is(err, loader.ErrInterfaceMismatch) {
				return nil, fail(422, "interface_mismatch", err)
			}
			return nil, fail(502, "spawn_failed", err)
		}
		if _, err := h.reg.Install(ev.Vendor, ev.Version, ev.SHA256, proc); err != nil {
			_ = proc.Close()
			return nil, fail(409, "install_failed", err)
		}
		installed = true
	}
	prev, err := h.reg.Activate(ev.Vendor, ev.Version, ev.KeepPrevious)
	if err != nil {
		return nil, fail(500, "activate_failed", err)
	}
	logger.Printf("切换 %s: %s → %s（installed=%v keep_previous=%v）", ev.Vendor, orDash(prev), ev.Version, installed, ev.KeepPrevious)
	return map[string]any{"vendor": ev.Vendor, "active": ev.Version, "previous": prev, "installed": installed, "keep_previous": ev.KeepPrevious}, nil
}

func (h *host) applyRetire(ev artifact.Event) (map[string]any, error) {
	switch err := h.reg.Retire(ev.Vendor, ev.Version); {
	case errors.Is(err, registry.ErrActive):
		return nil, fail(409, "version_is_active", err)
	case errors.Is(err, registry.ErrNotInstalled):
		return map[string]any{"vendor": ev.Vendor, "retired": ev.Version, "noop": true}, nil
	case err != nil:
		return nil, fail(500, "retire_failed", err)
	}
	logger.Printf("退役 %s@%s", ev.Vendor, ev.Version)
	return map[string]any{"vendor": ev.Vendor, "retired": ev.Version}, nil
}

// reconcile 拉平台期望状态对齐：装缺的、切不一致的、退役平台已退役或不再期望的。
func (h *host) reconcile(ctx context.Context) ([]string, error) {
	var d artifact.Desired
	if err := getJSON(ctx, h.platform+"/api/desired", &d); err != nil {
		return nil, err
	}
	var actions []string
	for vendor, want := range d.Vendors {
		if cur := h.reg.Active(vendor); cur != nil && cur.Version == want.Version && cur.SHA256 == want.SHA256 {
			continue
		}
		want.KeepPrevious = false
		if _, err := h.applyActivate(ctx, want); err != nil {
			actions = append(actions, fmt.Sprintf("activate %s@%s 失败: %v", vendor, want.Version, err))
			continue
		}
		actions = append(actions, fmt.Sprintf("activate %s@%s", vendor, want.Version))
	}
	for _, vs := range h.reg.Snapshot() {
		if _, ok := d.Vendors[vs.Vendor]; !ok {
			h.reg.RemoveVendor(vs.Vendor)
			actions = append(actions, "remove vendor "+vs.Vendor)
			continue
		}
		for _, v := range d.Retired[vs.Vendor] {
			if h.reg.Retire(vs.Vendor, v) == nil {
				actions = append(actions, fmt.Sprintf("retire %s@%s", vs.Vendor, v))
			}
		}
	}
	if len(actions) > 0 {
		logger.Printf("reconcile: %v", actions)
	}
	return actions, nil
}

func getJSON(ctx context.Context, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("GET %s: http %d", url, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func mustAbs(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		logger.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(a, "sock"), 0o755); err != nil {
		logger.Fatal(err)
	}
	if len(filepath.Join(a, "sock", "XXXXXXXX-XXXXXXXX.sock")) > 100 {
		logger.Fatalf("工作目录 %s 太长，unix socket 路径会超过 104 字节上限", a)
	}
	return a
}
