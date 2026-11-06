package loader

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/cfrs2005/go-hot-plugin/pkg/pluginapi"
)

// client 是宿主侧的「接口 1」HTTP 客户端，走 unix socket。
type client struct {
	tr *http.Transport
	hc *http.Client
}

func newClient(sock string) *client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     30 * time.Second,
	}
	return &client{tr: tr, hc: &http.Client{Transport: tr}}
}

// reset 丢弃空闲连接（子进程重启后旧连接全部失效）。
func (c *client) reset() { c.tr.CloseIdleConnections() }

func (c *client) health(ctx context.Context) (pluginapi.HealthInfo, error) {
	var h pluginapi.HealthInfo
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://plugin"+pluginapi.PathHealth, nil)
	resp, err := c.hc.Do(req)
	if err != nil {
		return h, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return h, fmt.Errorf("health http %d", resp.StatusCode)
	}
	return h, json.NewDecoder(resp.Body).Decode(&h)
}

func (c *client) query(ctx context.Context, q pluginapi.QueryRequest) (pluginapi.QueryResponse, error) {
	var out pluginapi.QueryResponse
	body, _ := json.Marshal(q)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://plugin"+pluginapi.PathQuery, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		return out, fmt.Errorf("%w: %v", pluginapi.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var eb pluginapi.ErrorBody
		_ = json.NewDecoder(resp.Body).Decode(&eb)
		return out, fmt.Errorf("plugin http %d: %s", resp.StatusCode, eb.Error)
	}
	return out, json.NewDecoder(resp.Body).Decode(&out)
}
