// Package artifact 负责插件二进制的签名消息格式、下载与校验。
// 平台用 SignedMessage 签名；宿主用 VerifySignature + Fetch 校验。
package artifact

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// 校验失败的错误码（宿主原样回给调用方，便于定位）。
var (
	ErrBadSignature = errors.New("signature_invalid")
	ErrShaMismatch  = errors.New("sha256_mismatch")
)

// SignedMessage 是被签名的字节：sha256 + "\n" + vendor + "\n" + version。
func SignedMessage(sha, vendor, version string) []byte {
	return []byte(sha + "\n" + vendor + "\n" + version)
}

// Sign 返回 base64 签名。
func Sign(priv ed25519.PrivateKey, sha, vendor, version string) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, SignedMessage(sha, vendor, version)))
}

// VerifySignature 校验平台对 (sha, vendor, version) 的签名。
func VerifySignature(pub ed25519.PublicKey, sha, vendor, version, sigB64 string) error {
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil || len(sig) != ed25519.SignatureSize || !ed25519.Verify(pub, SignedMessage(sha, vendor, version), sig) {
		return ErrBadSignature
	}
	return nil
}

// SHA256File 计算文件 sha256（hex）。
func SHA256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Fetch 下载 url 到 dest：先写同目录临时文件，sha256 与 wantSha 一致才 chmod +x 并 rename 到 dest。
// 不一致时删除临时文件，dest 不会出现。
func Fetch(ctx context.Context, url, dest, wantSha string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: http %d", resp.StatusCode)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // rename 成功后这是 no-op
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		tmp.Close()
		return fmt.Errorf("download: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSha {
		return fmt.Errorf("%w: want %s got %s", ErrShaMismatch, short(wantSha), short(got))
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dest)
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// Event 是平台 → 宿主的 webhook 载荷（POST callback_url），也是 /api/desired 里每个 vendor 的目标描述。
type Event struct {
	Event        string `json:"event"` // "activate" | "retire"
	Vendor       string `json:"vendor"`
	Version      string `json:"version"`
	SHA256       string `json:"sha256,omitempty"`
	Signature    string `json:"signature,omitempty"`
	DownloadURL  string `json:"download_url,omitempty"`
	KeepPrevious bool   `json:"keep_previous,omitempty"` // 回滚（目标版本比当前活跃版本更早上传）时为 true：宿主保留旧活跃版本为 standby
}

// Desired 是 GET /api/desired 的返回：每个 vendor 期望的活跃版本 + 平台已退役的版本。
type Desired struct {
	Vendors map[string]Event    `json:"vendors"`
	Retired map[string][]string `json:"retired"`
}
