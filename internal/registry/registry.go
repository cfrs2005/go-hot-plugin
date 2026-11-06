// Package registry 是插件版本注册表：每个 vendor 多版本并存、atomic.Pointer 原子切换活跃版本、
// 引用计数排空后 Close。它只认 pluginapi.Plugin 接口，不关心插件如何加载（不许 import loader）。
//
// 版本状态机：
//
//	Install → standby ──Activate──▶ active ──(被别的版本 Activate 且非 keepPrevious)──▶ draining ──refs==0──▶ closed(移出注册表)
//	           ▲                       │
//	           └──(被别的版本 Activate 且 keepPrevious，即回滚时保留)◀─┘
//	standby ──Retire──▶ draining
package registry

import (
	"errors"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/cfrs2005/go-hot-plugin/pkg/pluginapi"
)

var (
	ErrNotInstalled = errors.New("plugin_not_installed")
	ErrExists       = errors.New("version_already_installed")
	ErrActive       = errors.New("version_is_active")
)

// State 是单个版本在注册表里的状态。
type State string

const (
	StateActive   State = "active"
	StateStandby  State = "standby"
	StateDraining State = "draining"
)

// Entry 是一个已安装的 vendor@version。
type Entry struct {
	Vendor, Version, SHA256 string
	Plugin                  pluginapi.Plugin

	refs      atomic.Int64
	draining  atomic.Bool
	closeOnce sync.Once
	closed    chan struct{}
	reg       *Registry
}

// Refs 返回当前在途请求数。
func (e *Entry) Refs() int64 { return e.refs.Load() }

// Closed 在插件 Close 完成并移出注册表后关闭。
func (e *Entry) Closed() <-chan struct{} { return e.closed }

func (e *Entry) release() {
	if e.refs.Add(-1) == 0 {
		e.tryClose()
	}
}

func (e *Entry) tryClose() {
	if !e.draining.Load() || e.refs.Load() != 0 {
		return
	}
	e.closeOnce.Do(func() {
		go func() { // Close 可能要等子进程退出，不阻塞释放引用的请求
			if err := e.Plugin.Close(); err != nil {
				e.reg.logf("关闭 %s@%s 出错: %v", e.Vendor, e.Version, err)
			}
			e.reg.forget(e)
			e.reg.logf("%s@%s 已排空并关闭", e.Vendor, e.Version)
			close(e.closed)
		}()
	})
}

type slot struct {
	active   atomic.Pointer[Entry]
	versions map[string]*Entry
}

// Registry 是并发安全的版本注册表。
type Registry struct {
	mu      sync.RWMutex
	vendors map[string]*slot
	logf    func(format string, args ...any)
}

// New 创建注册表；logf 可为 nil。
func New(logf func(string, ...any)) *Registry {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Registry{vendors: map[string]*slot{}, logf: logf}
}

// Install 登记一个新版本（standby）。同版本已存在且未在排空时返回 ErrExists。
func (r *Registry) Install(vendor, version, sha string, p pluginapi.Plugin) (*Entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.vendors[vendor]
	if s == nil {
		s = &slot{versions: map[string]*Entry{}}
		r.vendors[vendor] = s
	}
	if old := s.versions[version]; old != nil && !old.draining.Load() {
		return nil, ErrExists
	}
	e := &Entry{Vendor: vendor, Version: version, SHA256: sha, Plugin: p, closed: make(chan struct{}), reg: r}
	s.versions[version] = e // 若有同版本正在排空，替换之；排空完成后 forget 只删自己
	return e, nil
}

// Get 返回已安装且未在排空的版本，没有则 nil。
func (r *Registry) Get(vendor, version string) *Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if s := r.vendors[vendor]; s != nil {
		if e := s.versions[version]; e != nil && !e.draining.Load() {
			return e
		}
	}
	return nil
}

// Active 返回 vendor 当前活跃版本，没有则 nil。
func (r *Registry) Active(vendor string) *Entry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if s := r.vendors[vendor]; s != nil {
		return s.active.Load()
	}
	return nil
}

// Activate 把 vendor 的活跃版本原子切到 version。
// keepPrevious=false（升级）：旧活跃版本进入 draining，引用归零后 Close。
// keepPrevious=true（回滚）：旧活跃版本转 standby，留待再次前滚或显式 Retire。
func (r *Registry) Activate(vendor, version string, keepPrevious bool) (prev string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.vendors[vendor]
	if s == nil || s.versions[version] == nil || s.versions[version].draining.Load() {
		return "", ErrNotInstalled
	}
	e := s.versions[version]
	old := s.active.Swap(e)
	if old == nil || old == e {
		return "", nil
	}
	if !keepPrevious {
		r.drain(old)
	}
	return old.Version, nil
}

// Retire 排空并关闭一个非活跃版本。
func (r *Registry) Retire(vendor, version string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.vendors[vendor]
	if s == nil || s.versions[version] == nil || s.versions[version].draining.Load() {
		return ErrNotInstalled
	}
	e := s.versions[version]
	if s.active.Load() == e {
		return ErrActive
	}
	r.drain(e)
	return nil
}

// RemoveVendor 清空活跃指针并排空该 vendor 的全部版本（平台不再期望该 vendor 时用）。
func (r *Registry) RemoveVendor(vendor string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.vendors[vendor]; s != nil {
		s.active.Store(nil)
		for _, e := range s.versions {
			r.drain(e)
		}
	}
}

func (r *Registry) drain(e *Entry) {
	if e.draining.CompareAndSwap(false, true) {
		r.logf("%s@%s 进入 draining（在途 %d）", e.Vendor, e.Version, e.refs.Load())
	}
	e.tryClose()
}

func (r *Registry) forget(e *Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.vendors[e.Vendor]; s != nil && s.versions[e.Version] == e {
		delete(s.versions, e.Version)
		if len(s.versions) == 0 && s.active.Load() == nil {
			delete(r.vendors, e.Vendor)
		}
	}
}

// Acquire 取 vendor 的活跃版本并 refs+1；调用方用完必须调用返回的 release。
// 若取到的版本恰好在切换中进入 draining，则重试取新的活跃版本，保证不会在已关闭的插件上发请求。
func (r *Registry) Acquire(vendor string) (*Entry, func(), error) {
	r.mu.RLock()
	s := r.vendors[vendor]
	r.mu.RUnlock()
	if s == nil {
		return nil, nil, ErrNotInstalled
	}
	for {
		e := s.active.Load()
		if e == nil {
			return nil, nil, ErrNotInstalled
		}
		e.refs.Add(1)
		if !e.draining.Load() {
			var once sync.Once
			return e, func() { once.Do(e.release) }, nil
		}
		e.release()
	}
}

// VersionStatus / VendorStatus 是 Snapshot 的输出。
type VersionStatus struct {
	Entry *Entry
	State State
}

type VendorStatus struct {
	Vendor   string
	Active   string
	Versions []VersionStatus
}

// Snapshot 返回当前全部 vendor 的状态（按 vendor、version 排序）。
func (r *Registry) Snapshot() []VendorStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]VendorStatus, 0, len(r.vendors))
	for name, s := range r.vendors {
		vs := VendorStatus{Vendor: name}
		act := s.active.Load()
		if act != nil {
			vs.Active = act.Version
		}
		for _, e := range s.versions {
			st := StateStandby
			switch {
			case e.draining.Load():
				st = StateDraining
			case e == act:
				st = StateActive
			}
			vs.Versions = append(vs.Versions, VersionStatus{Entry: e, State: st})
		}
		sort.Slice(vs.Versions, func(i, j int) bool { return vs.Versions[i].Entry.Version < vs.Versions[j].Entry.Version })
		out = append(out, vs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Vendor < out[j].Vendor })
	return out
}
