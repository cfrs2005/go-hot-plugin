// Package loader 管插件子进程的生命周期：spawn → 等 /health（校验接口版本）→ 监督（意外退出自动重启）→ Close。
// *Proc 实现 pluginapi.Plugin，宿主把它交给 registry；registry 不知道它是子进程。
package loader

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cfrs2005/go-hot-plugin/pkg/pluginapi"
)

// ErrInterfaceMismatch 表示插件自报的接口版本与宿主不一致。
var ErrInterfaceMismatch = errors.New("interface_mismatch")

// Spec 描述如何启动一个插件。
type Spec struct {
	Vendor, Version string
	Binary          string   // 插件可执行文件
	SocketPath      string   // UDS 路径（macOS 上限 104 字节）
	Env             []string // 追加的环境变量，如 VENDOR_BASE_URL=...
	StartTimeout    time.Duration
	MaxRestarts     int
	RestartBackoff  time.Duration
	Logf            func(string, ...any)
}

// Proc 是一个受监督的插件子进程。
type Proc struct {
	spec   Spec
	client *client

	mu       sync.Mutex
	cmd      *exec.Cmd
	exited   chan struct{} // 当前子进程 Wait 返回后关闭
	restarts int

	up      atomic.Bool
	closing atomic.Bool
	done    chan struct{} // 监督协程结束后关闭
}

// Start 启动插件并等待 /health 通过；失败时子进程已被杀掉。
func Start(ctx context.Context, spec Spec) (*Proc, error) {
	if spec.StartTimeout == 0 {
		spec.StartTimeout = 5 * time.Second
	}
	if spec.RestartBackoff == 0 {
		spec.RestartBackoff = time.Second
	}
	if spec.Logf == nil {
		spec.Logf = func(string, ...any) {}
	}
	p := &Proc{spec: spec, client: newClient(spec.SocketPath), done: make(chan struct{})}
	if err := p.spawn(ctx); err != nil {
		return nil, err
	}
	go p.supervise()
	return p, nil
}

func (p *Proc) name() string { return p.spec.Vendor + "@" + p.spec.Version }

// spawn 起一个子进程并等健康；成功后才替换 p.cmd。
func (p *Proc) spawn(ctx context.Context) error {
	_ = os.Remove(p.spec.SocketPath)
	cmd := exec.Command(p.spec.Binary)
	// 不继承宿主环境（等价 go-plugin 的 SkipHostEnv），只给 PATH 与契约变量。
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		pluginapi.EnvSocket + "=" + p.spec.SocketPath,
		pluginapi.EnvInterface + "=" + strconv.Itoa(pluginapi.InterfaceVersion),
	}, p.spec.Env...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn %s: %w", p.name(), err)
	}
	p.spec.Logf("spawn %s 子进程 pid=%d，等待 /health", p.name(), cmd.Process.Pid)
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()

	kill := func() { _ = cmd.Process.Kill(); <-exited; _ = os.Remove(p.spec.SocketPath) }
	deadline := time.Now().Add(p.spec.StartTimeout)
	p.client.reset()
	for {
		hctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		h, err := p.client.health(hctx)
		cancel()
		if err == nil {
			if h.Interface != pluginapi.InterfaceVersion {
				kill()
				return fmt.Errorf("%w: 插件 %s 自报 interface=%d，宿主要求 %d", ErrInterfaceMismatch, p.name(), h.Interface, pluginapi.InterfaceVersion)
			}
			break
		}
		select {
		case <-exited:
			return fmt.Errorf("%s 在健康检查通过前退出", p.name())
		case <-ctx.Done():
			kill()
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			kill()
			return fmt.Errorf("%s 健康检查超时（%s）: %v", p.name(), p.spec.StartTimeout, err)
		}
	}

	p.mu.Lock()
	p.cmd, p.exited = cmd, exited
	p.mu.Unlock()
	p.up.Store(true)
	p.spec.Logf("%s 已就绪 pid=%d", p.name(), cmd.Process.Pid)
	return nil
}

// supervise 等子进程退出；非 Close 导致的退出最多重启 MaxRestarts 次，退避 RestartBackoff。
func (p *Proc) supervise() {
	defer close(p.done)
	for {
		p.mu.Lock()
		exited, pid := p.exited, p.cmd.Process.Pid
		p.mu.Unlock()
		<-exited
		p.up.Store(false)
		if p.closing.Load() {
			return
		}
		p.spec.Logf("%s 子进程 pid=%d 意外退出", p.name(), pid)
		for {
			if p.closing.Load() {
				return
			}
			p.mu.Lock()
			if p.restarts >= p.spec.MaxRestarts {
				p.mu.Unlock()
				p.spec.Logf("%s 已重启 %d 次，放弃；保持不可用", p.name(), p.spec.MaxRestarts)
				return
			}
			p.restarts++
			n := p.restarts
			p.mu.Unlock()
			time.Sleep(p.spec.RestartBackoff)
			if p.closing.Load() {
				return
			}
			p.spec.Logf("%s 第 %d 次重启", p.name(), n)
			if err := p.spawn(context.Background()); err != nil {
				p.spec.Logf("%s 重启失败: %v", p.name(), err)
				continue
			}
			// 重启期间被 Close 了：Close 可能拿到的是旧 cmd，这里补杀新进程。
			if p.closing.Load() {
				p.signal(syscall.SIGKILL)
			}
			break
		}
	}
}

func (p *Proc) signal(sig syscall.Signal) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(sig)
	}
}

// PID 返回当前子进程 pid（重启后会变）。
func (p *Proc) PID() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cmd.Process.Pid
}

// Restarts 返回已重启次数；Up 返回当前是否可用。
func (p *Proc) Restarts() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.restarts
}
func (p *Proc) Up() bool { return p.up.Load() }

// Query 实现 pluginapi.Plugin。进程不可用或连不上时返回 pluginapi.ErrUnavailable。
func (p *Proc) Query(ctx context.Context, req pluginapi.QueryRequest) (pluginapi.QueryResponse, error) {
	if !p.up.Load() {
		return pluginapi.QueryResponse{}, pluginapi.ErrUnavailable
	}
	return p.client.query(ctx, req)
}

// Health 实现 pluginapi.Plugin。
func (p *Proc) Health(ctx context.Context) (pluginapi.HealthInfo, error) {
	if !p.up.Load() {
		return pluginapi.HealthInfo{}, pluginapi.ErrUnavailable
	}
	return p.client.health(ctx)
}

// Close 先 SIGTERM，2s 内没退就 SIGKILL；等监督协程结束（子进程已被 Wait 回收）后删 socket。
func (p *Proc) Close() error {
	if !p.closing.CompareAndSwap(false, true) {
		<-p.done
		return nil
	}
	p.up.Store(false)
	p.signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(2 * time.Second):
		p.signal(syscall.SIGKILL)
		<-p.done
	}
	p.client.reset()
	_ = os.Remove(p.spec.SocketPath)
	return nil
}
