// platform 是「管理平台 A」：存插件 artifact、ed25519 签名、记录活跃版本、向订阅者推 webhook。
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/cfrs2005/go-hot-plugin/internal/artifact"
)

var (
	logger  = log.New(os.Stderr, fmt.Sprintf("[platform pid=%d] ", os.Getpid()), log.LstdFlags|log.Lmicroseconds)
	nameRE  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	webhook = &http.Client{Timeout: 15 * time.Second}
)

type server struct {
	db        *store
	priv      ed25519.PrivateKey
	pub       ed25519.PublicKey
	artifacts string
	publicURL string
}

func main() {
	dataDir := envOr("PLATFORM_DATA_DIR", "var/platform")
	addr := envOr("PLATFORM_ADDR", "127.0.0.1:9100")
	s := &server{artifacts: filepath.Join(dataDir, "artifacts"), publicURL: strings.TrimRight(envOr("PLATFORM_PUBLIC_URL", "http://"+addr), "/")}
	if err := os.MkdirAll(s.artifacts, 0o755); err != nil {
		logger.Fatal(err)
	}
	var err error
	if s.priv, err = loadOrCreateKey(dataDir); err != nil {
		logger.Fatal(err)
	}
	s.pub = s.priv.Public().(ed25519.PublicKey)
	if s.db, err = openStore(filepath.Join(dataDir, "platform.db")); err != nil {
		logger.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]bool{"ok": true}) })
	mux.HandleFunc("GET /api/pubkey", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"alg": "ed25519", "pubkey": base64.StdEncoding.EncodeToString(s.pub)})
	})
	mux.HandleFunc("POST /api/plugins", s.upload)
	mux.HandleFunc("GET /api/plugins", s.list)
	mux.HandleFunc("POST /api/plugins/{vendor}/{version}/activate", s.activate)
	mux.HandleFunc("POST /api/plugins/{vendor}/retire", s.retire)
	mux.HandleFunc("GET /api/plugins/{vendor}/{version}/download", s.download)
	mux.HandleFunc("POST /api/subscribe", s.subscribe)
	mux.HandleFunc("GET /api/desired", s.desired)
	logger.Printf("监听 %s，数据目录 %s，公钥 %s", addr, dataDir, base64.StdEncoding.EncodeToString(s.pub))
	logger.Fatal(http.ListenAndServe(addr, mux))
}

func loadOrCreateKey(dir string) (ed25519.PrivateKey, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	kp := filepath.Join(dir, "ed25519.key")
	if b, err := os.ReadFile(kp); err == nil {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(raw) != ed25519.PrivateKeySize {
			return nil, fmt.Errorf("私钥文件 %s 损坏", kp)
		}
		return ed25519.PrivateKey(raw), nil
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(kp, []byte(base64.StdEncoding.EncodeToString(priv)), 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "ed25519.pub"), []byte(base64.StdEncoding.EncodeToString(pub)), 0o644); err != nil {
		return nil, err
	}
	logger.Printf("首次启动，已生成 ed25519 密钥对")
	return priv, nil
}

func (s *server) upload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeErr(w, 400, "bad_multipart")
		return
	}
	vendor, version := r.FormValue("vendor"), r.FormValue("version")
	if !nameRE.MatchString(vendor) || !nameRE.MatchString(version) {
		writeErr(w, 400, "bad_vendor_or_version")
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		writeErr(w, 400, "missing_file")
		return
	}
	defer f.Close()
	if _, err := s.db.getPlugin(vendor, version); err == nil {
		writeErr(w, 409, "version_exists")
		return
	}
	dst := filepath.Join(s.artifacts, vendor, version, "plugin")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out, err := os.Create(dst)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, h), f)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	sha := hex.EncodeToString(h.Sum(nil))
	row := pluginRow{Vendor: vendor, Version: version, SHA256: sha, Signature: artifact.Sign(s.priv, sha, vendor, version), Path: dst}
	if err := s.db.insertPlugin(row); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	logger.Printf("上传 %s@%s sha256=%s", vendor, version, sha[:12])
	writeJSON(w, 201, row)
}

func (s *server) list(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.listPlugins()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, rows)
}

func (s *server) download(w http.ResponseWriter, r *http.Request) {
	p, err := s.db.getPlugin(r.PathValue("vendor"), r.PathValue("version"))
	if err != nil {
		writeErr(w, 404, "not_found")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, p.Path)
}

func (s *server) eventFor(kind string, p pluginRow) artifact.Event {
	return artifact.Event{Event: kind, Vendor: p.Vendor, Version: p.Version, SHA256: p.SHA256, Signature: p.Signature,
		DownloadURL: fmt.Sprintf("%s/api/plugins/%s/%s/download", s.publicURL, p.Vendor, p.Version)}
}

func (s *server) activate(w http.ResponseWriter, r *http.Request) {
	vendor, version := r.PathValue("vendor"), r.PathValue("version")
	p, err := s.db.getPlugin(vendor, version)
	if err != nil {
		writeErr(w, 404, "not_found")
		return
	}
	ev := s.eventFor("activate", p)
	// 回滚判定：目标版本比当前活跃版本更早上传 → 让宿主保留当前版本为 standby。
	if cur, err := s.db.activeRow(vendor); err == nil && cur.Version != version && p.ID < cur.ID {
		ev.KeepPrevious = true
	}
	if err := s.db.setActive(vendor, version); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	logger.Printf("activate %s@%s keep_previous=%v notify=%v", vendor, version, ev.KeepPrevious, notify(r))
	writeJSON(w, 200, map[string]any{"vendor": vendor, "version": version, "keep_previous": ev.KeepPrevious, "notified": s.maybeNotify(r, ev)})
}

func (s *server) retire(w http.ResponseWriter, r *http.Request) {
	vendor, version := r.PathValue("vendor"), r.URL.Query().Get("version")
	p, err := s.db.getPlugin(vendor, version)
	if err != nil {
		writeErr(w, 404, "not_found")
		return
	}
	if p.Active {
		writeErr(w, 409, "version_is_active")
		return
	}
	if err := s.db.retire(vendor, version); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	ev := artifact.Event{Event: "retire", Vendor: vendor, Version: version}
	logger.Printf("retire %s@%s notify=%v", vendor, version, notify(r))
	writeJSON(w, 200, map[string]any{"vendor": vendor, "version": version, "notified": s.maybeNotify(r, ev)})
}

func notify(r *http.Request) bool { return r.URL.Query().Get("notify") != "false" }

type delivery struct {
	URL    string `json:"url"`
	Status int    `json:"status"`
	Body   string `json:"body,omitempty"`
	Error  string `json:"error,omitempty"`
}

// maybeNotify 并行投递 webhook 并等待结果；失败只记日志不重试（宿主 reconcile 兜底）。
func (s *server) maybeNotify(r *http.Request, ev artifact.Event) []delivery {
	if !notify(r) {
		return []delivery{}
	}
	subs, err := s.db.subscribers()
	if err != nil {
		logger.Printf("读订阅者失败: %v", err)
		return []delivery{}
	}
	body, _ := json.Marshal(ev)
	out := make([]delivery, len(subs))
	var wg sync.WaitGroup
	for i, u := range subs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := delivery{URL: u}
			resp, err := webhook.Post(u, "application/json", bytes.NewReader(body))
			if err != nil {
				d.Error = err.Error()
			} else {
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
				resp.Body.Close()
				d.Status, d.Body = resp.StatusCode, strings.TrimSpace(string(b))
			}
			if d.Error != "" || d.Status >= 300 {
				logger.Printf("webhook %s %s@%s → %s 失败: status=%d err=%s body=%s", ev.Event, ev.Vendor, ev.Version, u, d.Status, d.Error, d.Body)
			}
			out[i] = d
		}()
	}
	wg.Wait()
	return out
}

func (s *server) subscribe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CallbackURL string `json:"callback_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !strings.HasPrefix(req.CallbackURL, "http") {
		writeErr(w, 400, "bad_callback_url")
		return
	}
	if err := s.db.subscribe(req.CallbackURL); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	logger.Printf("订阅 %s", req.CallbackURL)
	writeJSON(w, 200, map[string]string{"callback_url": req.CallbackURL})
}

func (s *server) desired(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.listPlugins()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	d := artifact.Desired{Vendors: map[string]artifact.Event{}, Retired: map[string][]string{}}
	for _, p := range rows {
		if p.Active {
			d.Vendors[p.Vendor] = s.eventFor("activate", p)
		}
		if p.RetiredAt != nil {
			d.Retired[p.Vendor] = append(d.Retired[p.Vendor], p.Version)
		}
	}
	writeJSON(w, 200, d)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
