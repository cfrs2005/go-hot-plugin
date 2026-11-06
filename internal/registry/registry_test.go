package registry

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cfrs2005/go-hot-plugin/pkg/pluginapi"
)

type fake struct {
	ver    string
	closed atomic.Bool
}

func (f *fake) Query(ctx context.Context, req pluginapi.QueryRequest) (pluginapi.QueryResponse, error) {
	if f.closed.Load() {
		return pluginapi.QueryResponse{}, pluginapi.ErrUnavailable
	}
	return pluginapi.QueryResponse{PluginVersion: f.ver}, nil
}
func (f *fake) Health(context.Context) (pluginapi.HealthInfo, error) {
	return pluginapi.HealthInfo{}, nil
}
func (f *fake) Close() error { f.closed.Store(true); return nil }

func waitClosed(t *testing.T, e *Entry) {
	t.Helper()
	select {
	case <-e.Closed():
	case <-time.After(2 * time.Second):
		t.Fatalf("%s 未在 2s 内关闭", e.Version)
	}
}

func TestDrainWaitsForInflight(t *testing.T) {
	r := New(nil)
	v1, v2 := &fake{ver: "v1"}, &fake{ver: "v2"}
	e1, _ := r.Install("A", "v1", "", v1)
	r.Install("A", "v2", "", v2)
	if _, err := r.Activate("A", "v1", false); err != nil {
		t.Fatal(err)
	}
	got, release, err := r.Acquire("A")
	if err != nil || got != e1 {
		t.Fatalf("acquire: %v", err)
	}
	r.Activate("A", "v2", false)
	time.Sleep(50 * time.Millisecond)
	if v1.closed.Load() {
		t.Fatal("在途请求未结束时 v1 被关闭")
	}
	release()
	waitClosed(t, e1)
	if r.Get("A", "v1") != nil {
		t.Fatal("v1 关闭后仍在注册表")
	}
}

func TestRollbackKeepsPrevious(t *testing.T) {
	r := New(nil)
	r.Install("A", "v1", "", &fake{ver: "v1"})
	e2, _ := r.Install("A", "v2", "", &fake{ver: "v2"})
	r.Activate("A", "v2", false)
	r.Activate("A", "v1", true)
	if r.Get("A", "v2") == nil {
		t.Fatal("回滚后 v2 应保留为 standby")
	}
	if err := r.Retire("A", "v1"); err != ErrActive {
		t.Fatalf("退役活跃版本应报 ErrActive，得到 %v", err)
	}
	if err := r.Retire("A", "v2"); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, e2)
}

// 高并发下反复切换，任何一次 Query 都不能落到已关闭的插件上。
func TestConcurrentSwapNeverHitsClosed(t *testing.T) {
	r := New(nil)
	r.Install("A", "v0", "", &fake{ver: "v0"})
	r.Activate("A", "v0", false)
	stop := make(chan struct{})
	var bad atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				e, release, err := r.Acquire("A")
				if err != nil {
					bad.Add(1)
					continue
				}
				if _, err := e.Plugin.Query(context.Background(), pluginapi.QueryRequest{}); err != nil {
					bad.Add(1)
				}
				release()
			}
		}()
	}
	for i := 1; i <= 200; i++ {
		v := "v" + string(rune('0'+i%10)) + string(rune('a'+i/10))
		r.Install("A", v, "", &fake{ver: v})
		r.Activate("A", v, false)
	}
	close(stop)
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatalf("%d 次请求落到了已关闭/缺失的插件上", bad.Load())
	}
}
