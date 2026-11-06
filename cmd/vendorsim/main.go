// vendorsim 是假厂商 API：A v1 / A v2（不同形状）/ B，支持 ?delay_ms= 延时返回。
package main

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

// item 是同一个 id 在厂商侧的「真实数据」，各接口只是形状不同。
func item(vendor, id string) (name string, cents int64) {
	h := fnv.New32a()
	h.Write([]byte(vendor + "/" + id))
	return fmt.Sprintf("%s 商品 #%s", vendor, id), int64(100 + h.Sum32()%99900)
}

func yuan(cents int64) string { return fmt.Sprintf("%d.%02d", cents/100, cents%100) }

func delay(r *http.Request) {
	if ms, err := strconv.Atoi(r.URL.Query().Get("delay_ms")); err == nil && ms > 0 {
		select {
		case <-time.After(time.Duration(ms) * time.Millisecond):
		case <-r.Context().Done():
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func main() {
	addr := envOr("VENDORSIM_ADDR", "127.0.0.1:9101")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, map[string]bool{"ok": true}) })
	mux.HandleFunc("GET /vendorA/v1/items/{id}", func(w http.ResponseWriter, r *http.Request) {
		delay(r)
		n, c := item("A", r.PathValue("id"))
		writeJSON(w, map[string]string{"title": n, "amount": yuan(c)})
	})
	mux.HandleFunc("GET /vendorA/v2/items/{id}", func(w http.ResponseWriter, r *http.Request) {
		delay(r)
		n, c := item("A", r.PathValue("id"))
		writeJSON(w, map[string]any{"name": n, "price_cents": c, "currency": "CNY"})
	})
	mux.HandleFunc("GET /vendorB/goods/{id}", func(w http.ResponseWriter, r *http.Request) {
		delay(r)
		n, c := item("B", r.PathValue("id"))
		writeJSON(w, map[string]string{"goods_name": n, "price": yuan(c)})
	})
	log.Printf("[vendorsim pid=%d] 监听 %s", os.Getpid(), addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
