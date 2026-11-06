package main

import (
	"strings"
	"syscall"
	"time"

	"github.com/cfrs2005/go-hot-plugin/pkg/pluginapi"
)

// expectA / expectB 直接问 vendorsim，得到厂商侧真实数据作为对照。
func expectA(id string) (string, int64) {
	var v struct{ Title, Amount string }
	getJSON(simURL+"/vendorA/v1/items/"+id, &v)
	c, _ := pluginapi.ParseDecimalCents(v.Amount)
	return v.Title, c
}

func expectB(id string) (string, int64) {
	var v struct {
		GoodsName string `json:"goods_name"`
		Price     string `json:"price"`
	}
	getJSON(simURL+"/vendorB/goods/"+id, &v)
	c, _ := pluginapi.ParseDecimalCents(v.Price)
	return v.GoodsName, c
}

type pluginMeta struct {
	Vendor, Version, SHA256, Signature string
}

func platformPlugin(vendor, version string) pluginMeta {
	var rows []pluginMeta
	getJSON(plaURL+"/api/plugins", &rows)
	for _, r := range rows {
		if r.Vendor == vendor && r.Version == version {
			return r
		}
	}
	return pluginMeta{}
}

func runSteps() {
	var demoPID int

	step(1, "demo-go 启动后未装插件，/query?vendor=A → 404 plugin_not_installed", func() {
		_, s := status()
		demoPID = s.PID
		code, q, raw := query("A", "1", 0)
		record(code == 404 && q.Error == "plugin_not_installed", "404 plugin_not_installed", "%d %s（demo-go pid=%d）", code, raw, demoPID)
	})

	step(2, "上传 A v1 + activate → 3s 内可查，数据与 vendorsim 一致", func() {
		c1, _ := upload("A", "v1", "vendor-a-v1")
		c2, raw := activate("A", "v1", true)
		record(c1 == 201 && c2 == 200 && strings.Contains(raw, `"status":200`), "上传 201、activate 200、webhook 投递 200", "upload=%d activate=%d %s", c1, c2, raw)
		var q queryResp
		var code int
		ok, took := poll(3*time.Second, func() bool { code, q, _ = query("A", "1", 0); return code == 200 })
		name, cents := expectA("1")
		record(ok && q.PluginVersion == "v1", "3s 内 200 且 plugin_version=v1", "%d plugin_version=%s 耗时 %s", code, q.PluginVersion, took.Round(time.Millisecond))
		record(q.Name == name && q.PriceCents == cents, "name/price_cents 与 vendorsim 一致", "插件 {%q,%d} vs vendorsim {%q,%d}", q.Name, q.PriceCents, name, cents)
	})

	step(3, "上传 B + activate → B 可查；A 仍 v1", func() {
		upload("B", "v1", "vendor-b")
		c, _ := activate("B", "v1", true)
		code, q, _ := query("B", "7", 0)
		name, cents := expectB("7")
		record(c == 200 && code == 200 && q.Vendor == "B" && q.Name == name && q.PriceCents == cents, "B 查询 200 且数据一致", "activate=%d query=%d %+v（vendorsim {%q,%d}）", c, code, q, name, cents)
		codeA, qa, _ := query("A", "1", 0)
		record(codeA == 200 && qa.PluginVersion == "v1", "A 仍为 v1", "%d plugin_version=%s", codeA, qa.PluginVersion)
	})

	step(4, "A v1→v2 热升级：在途慢请求走完 v1，新请求走 v2，v1 排空后退出", func() {
		upload("A", "v2", "vendor-a-v2")
		_, s := status()
		v1 := s.version("A", "v1")
		v1pid := 0
		if v1 != nil {
			v1pid = v1.PID
		}
		type res struct {
			code int
			q    queryResp
		}
		slow := make(chan res, 1)
		go func() { c, q, _ := query("A", "2", 3000); slow <- res{c, q} }()
		time.Sleep(200 * time.Millisecond)
		t0 := time.Now()
		ca, raw := activate("A", "v2", true)
		record(ca == 200 && strings.Contains(raw, `"status":200`), "activate v2 200 且 webhook 200", "%d 耗时 %s %s", ca, time.Since(t0).Round(time.Millisecond), raw)

		_, s = status()
		v1 = s.version("A", "v1")
		inflight := len(slow) == 0
		if v1 != nil {
			record(inflight && v1.Refs >= 1 && v1.State == "draining", "慢请求返回前 v1 refs≥1 且 draining", "慢请求在途=%v v1 refs=%d state=%s active=%s", inflight, v1.Refs, v1.State, s.Vendors["A"].Active)
		} else {
			record(false, "慢请求返回前 v1 refs≥1 且 draining", "status 里已无 v1（慢请求在途=%v）", inflight)
		}
		code, q, _ := query("A", "1", 0)
		name, cents := expectA("1")
		record(code == 200 && q.PluginVersion == "v2" && q.Name == name && q.PriceCents == cents, "新请求 plugin_version=v2 且数据一致（厂商新形状已映射）", "%d %+v", code, q)

		var r res
		select {
		case r = <-slow:
		case <-time.After(6 * time.Second):
			r = res{code: -1}
		}
		record(r.code == 200 && r.q.PluginVersion == "v1", "慢请求最终 200 且 plugin_version=v1", "%d plugin_version=%s", r.code, r.q.PluginVersion)

		ok, took := poll(2*time.Second, func() bool {
			_, s := status()
			return !alive(v1pid) && s.version("A", "v1") == nil
		})
		record(ok && v1pid > 0, "慢请求完成后 2s 内 v1 子进程退出且 status 不含 v1", "v1 pid=%d 退出=%v 耗时 %s", v1pid, !alive(v1pid), took.Round(time.Millisecond))
	})

	step(5, "回滚：activate A v1 → 返回 v1；v2 转 standby 仍在装；retire v2 → v2 进程退出", func() {
		c, raw := activate("A", "v1", true)
		record(c == 200 && strings.Contains(raw, `"keep_previous":true`), "activate v1 200 且平台判定为回滚（keep_previous）", "%d %s", c, raw)
		code, q, _ := query("A", "1", 0)
		record(code == 200 && q.PluginVersion == "v1", "/query 返回 v1", "%d plugin_version=%s", code, q.PluginVersion)
		_, s := status()
		v2 := s.version("A", "v2")
		v2pid := 0
		if v2 != nil {
			v2pid = v2.PID
		}
		record(v2 != nil && v2.State == "standby" && alive(v2pid), "v2 非活跃但仍在装、进程存活", "active=%s v2=%+v", s.Vendors["A"].Active, v2)
		cr, rawr := postJSON(plaURL+"/api/plugins/A/retire?version=v2", nil, nil)
		ok, took := poll(2*time.Second, func() bool { _, s := status(); return !alive(v2pid) && s.version("A", "v2") == nil })
		record(cr == 200 && ok, "retire v2 后 v2 进程退出且移出 status", "retire=%d %s；v2 pid=%d 退出=%v 耗时 %s", cr, rawr, v2pid, !alive(v2pid), took.Round(time.Millisecond))
	})

	step(6, "篡改：伪造 activate 事件直接打 demo-go，全部拒绝且当前版本不变", func() {
		v2 := platformPlugin("A", "v2")
		v1 := platformPlugin("A", "v1")
		url := plaURL + "/api/plugins/A/v2/download"
		cases := []struct {
			what, sha, sig, url, wantErr string
		}{
			{"sha256 被改（签名不覆盖该 sha）", strings.Repeat("0", 64), v2.Signature, url, "signature_invalid"},
			{"sha256/签名都真，但下载到的产物被换（指向 B 的二进制）", v2.SHA256, v2.Signature, plaURL + "/api/plugins/B/v1/download", "sha256_mismatch"},
			{"sha256 对、签名错（用 A v1 的签名）", v2.SHA256, v1.Signature, url, "signature_invalid"},
		}
		for _, tc := range cases {
			ev := map[string]any{"event": "activate", "vendor": "A", "version": "v2", "sha256": tc.sha, "signature": tc.sig, "download_url": tc.url}
			var er struct{ Error string }
			code, raw := postJSON(demURL+"/internal/plugin-event", ev, &er)
			qc, q, _ := query("A", "1", 0)
			_, s := status()
			record(code >= 400 && code < 500 && er.Error == tc.wantErr && qc == 200 && q.PluginVersion == "v1" && s.version("A", "v2") == nil,
				tc.what+" → 4xx "+tc.wantErr+"，/query 仍 v1", "%d %s；query=%d plugin_version=%s", code, raw, qc, q.PluginVersion)
		}
	})

	step(7, "崩溃隔离：kill -9 A 活跃插件，宿主不受影响，5s 内监督重启恢复", func() {
		_, s := status()
		act := s.version("A", s.Vendors["A"].Active)
		if act == nil {
			record(false, "取 A 活跃版本 pid", "status 无 A 活跃版本")
			return
		}
		old := act.PID
		err := syscall.Kill(old, syscall.SIGKILL)
		record(err == nil, "kill -9 插件进程", "A@%s pid=%d err=%v", act.Version, old, err)
		codes := map[int]int{}
		var q queryResp
		ok, took := poll(5*time.Second, func() bool {
			var c int
			c, q, _ = query("A", "1", 0)
			codes[c]++
			return c == 200
		})
		sc, s2 := status()
		record(sc == 200 && s2.PID == demoPID, "demo-go 仍活、pid 不变", "status=%d pid=%d（初始 %d）", sc, s2.PID, demoPID)
		nv := s2.version("A", act.Version)
		newPID, restarts := 0, 0
		if nv != nil {
			newPID, restarts = nv.PID, nv.Restarts
		}
		record(ok && q.PluginVersion == act.Version && newPID != old && restarts == 1, "5s 内 /query 恢复 200（新子进程）",
			"耗时 %s，期间状态码分布 %v，新 pid=%d restarts=%d", took.Round(time.Millisecond), codes, newPID, restarts)
	})

	step(8, "兜底：notify=false 发布 B v2（webhook 不发），POST /admin/reconcile 后 5s 内对齐", func() {
		c1, _ := upload("B", "v2", "vendor-b")
		c2, raw := activate("B", "v2", false)
		record(c1 == 201 && c2 == 200 && strings.Contains(raw, `"notified":[]`), "上传 201、activate 200 且未投递 webhook", "upload=%d activate=%d %s", c1, c2, raw)
		time.Sleep(300 * time.Millisecond)
		_, s := status()
		record(s.Vendors["B"].Active == "v1", "未 reconcile 前 B 仍为 v1（证明没走 webhook）", "B active=%s", s.Vendors["B"].Active)
		cr, rawr := postJSON(demURL+"/admin/reconcile", nil, nil)
		ok, took := poll(5*time.Second, func() bool { _, s = status(); return s.Vendors["B"].Active == "v2" })
		record(cr == 200 && ok, "reconcile 后 B 活跃版本 = v2", "reconcile=%d %s；B active=%s 耗时 %s", cr, rawr, s.Vendors["B"].Active, took.Round(time.Millisecond))
		code, q, _ := query("B", "7", 0)
		record(code == 200, "B 仍可查（plugin_version 由插件自报，复用 vendor-b 二进制故为 v1）", "%d plugin_version=%s", code, q.PluginVersion)
	})

	step(9, "全程 demo-go 自身 pid 不变", func() {
		_, s := status()
		record(s.PID == demoPID && demoPID > 0, "pid 与第 1 步一致", "第 1 步 %d，现在 %d", demoPID, s.PID)
	})
}
