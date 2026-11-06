#!/usr/bin/env bash
# go-hot-plugin 一键分步演示。用法：./demo.sh up | 1..6 | all | status | down
# 依赖：bash、curl、python3（只用来解析 JSON，不依赖 jq）。
set -uo pipefail
cd "$(dirname "$0")"
ROOT=$(pwd)
P=http://127.0.0.1:9100   # platform
V=http://127.0.0.1:9101   # vendorsim
D=http://127.0.0.1:9102   # demo-go
LOGS=var/logs
MAINPID_FILE=${LOGS}/demo-go.pid
TMP=$(mktemp -d); trap 'rm -rf "${TMP}"' EXIT

# ---------- JSON（python3） ----------
PYLIB='
import json, sys
def load(s):
    try: return json.loads(s)
    except Exception: return None
cmd, args = sys.argv[1], sys.argv[2:]
d = load(args[0]) if args else None
if cmd == "get":            # get <json> a.b.c  → 值（缺失为空）
    for k in args[1].split("."):
        d = d.get(k) if isinstance(d, dict) else None
    print("" if d is None else (json.dumps(d, ensure_ascii=False) if isinstance(d, (dict, list)) else (str(d).lower() if isinstance(d, bool) else d)))
elif cmd == "ver":          # ver <status> A v1 field  → 某版本字段（不存在为空）
    vs = ((d or {}).get("vendors", {}).get(args[1]) or {}).get("versions") or []
    hit = [x for x in vs if x.get("version") == args[2]]
    print(hit[0].get(args[3], "") if hit else "")
elif cmd == "plugin":       # plugin <list> A v1 field → 平台上某版本字段（不存在为空）
    hit = [x for x in (d or []) if x.get("vendor") == args[1] and x.get("version") == args[2]]
    v = hit[0].get(args[3], "") if hit else ""
    print(str(v).lower() if isinstance(v, bool) else v)
elif cmd == "notified":     # notified <activate 响应> → 投递结果中文
    n = (d or {}).get("notified") or []
    if not n: print("平台未投递 webhook（notify=false 或无订阅者）")
    for x in n:
        b = load(x.get("body") or "") or {}
        if x.get("error"): print("webhook → %s 失败：%s" % (x["url"], x["error"]))
        else: print("webhook → %s 返回 %s，demo-go 回执：活跃=%s 上一版本=%s 新装=%s" % (x["url"], x.get("status"), b.get("active", b.get("retired", "-")), b.get("previous") or "-", {True: "是（下载+spawn 了新进程）", False: "否（已装，只切指针）"}.get(b.get("installed"), "-")))
elif cmd == "table":
    import unicodedata
    def pad(x, w):
        x = str(x); n = sum(2 if unicodedata.east_asian_width(c) in "WF" else 1 for c in x)
        return x + " " * max(1, w - n)        # table <status> → 中文表格
    if d is None: print("  （demo-go 无响应）"); sys.exit()
    print("  demo-go 主进程 pid：%s    reconcile 间隔：%s" % (d.get("pid"), d.get("reconcile_interval")))
    vendors = d.get("vendors") or {}
    if not vendors: print("  （尚未加载任何厂商插件）"); sys.exit()
    names = {"active": "活跃", "standby": "待命(可回滚)", "draining": "排空中"}
    W = (6, 10, 6, 14, 11, 10, 6, 8)
    row = lambda cells: print("  " + "".join(pad(c, w) for c, w in zip(cells, W)).rstrip())
    row(("厂商", "活跃版本", "版本", "状态", "子进程pid", "在途refs", "可用", "重启次数"))
    for v in sorted(vendors):
        for x in vendors[v]["versions"]:
            row((v, vendors[v].get("active") or "-", x["version"], names.get(x["state"], x["state"]), x["pid"], x["refs"], "是" if x["up"] else "否", x["restarts"]))
'
py() { python3 -c "${PYLIB}" "$@"; }

# ---------- HTTP ----------
# req METHOD URL [curl 额外参数...] → 设置 CODE / BODY
req() { local m=$1 u=$2; shift 2; CODE=$(curl -s -o "${TMP}/b" -w '%{http_code}' -X "${m}" "$@" "${u}"); BODY=$(cat "${TMP}/b" 2>/dev/null); }
status_json() { curl -s --max-time 3 "${D}/admin/status"; }
plugins_json() { curl -s --max-time 3 "${P}/api/plugins"; }
host_active() { py get "$(status_json)" "vendors.$1.active"; }
alive() { [ -n "$1" ] && kill -0 "$1" 2>/dev/null; }

# ---------- 输出 ----------
DO=(); RES=(); FAILS=()
do_() { DO+=("$*"); }
res() { RES+=("$*"); }
bad() { FAILS+=("$*"); }
begin() { DO=(); RES=(); FAILS=(); TITLE="$2"; STEP=$1; }
finish() {
  pid_line
  echo "===== 第 ${STEP} 步：${TITLE} ====="
  echo "做了什么："; for x in "${DO[@]}"; do echo "  - ${x}"; done
  echo "结果："; for x in "${RES[@]}"; do case "${x}" in "|"*) echo "      ${x#|}" ;; *) echo "  - ${x}" ;; esac; done
  if [ ${#FAILS[@]} -eq 0 ]; then echo "判定：通过"; else echo "判定：失败（$(IFS='；'; echo "${FAILS[*]}")）"; fi
  echo
  [ ${#FAILS[@]} -eq 0 ]
}
pid_line() {
  local now; now=$(py get "$(status_json)" pid)
  mkdir -p "${LOGS}"
  if [ -z "${now}" ]; then res "demo-go 主进程 pid：取不到（服务没响应）"; bad "demo-go 无响应"; return; fi
  if [ ! -s "${MAINPID_FILE}" ]; then echo "${now}" > "${MAINPID_FILE}"; res "demo-go 主进程 pid：${now}（首次记录到 ${MAINPID_FILE}）"; return; fi
  local was; was=$(cat "${MAINPID_FILE}")
  if [ "${was}" = "${now}" ]; then res "demo-go 主进程 pid：${now}（与之前记录一致，进程从未重启）"
  else res "demo-go 主进程 pid：${now}（之前记录 ${was}，不一致！）"; bad "主进程 pid 变了"; fi
}
query_desc() { # query_desc A 1 [delay] → 设置 QD（一行中文描述）、QV（plugin_version）、CODE、BODY
  req GET "${D}/query?vendor=$1&id=$2&delay_ms=${3:-0}"
  QV=$(py get "${BODY}" plugin_version)
  if [ "${CODE}" = 200 ]; then
    QD="查询厂商 $1 id=$2 → 200，返回由 ${QV} 插件产生，name=$(py get "${BODY}" name)，price_cents=$(py get "${BODY}" price_cents)"
  else QD="查询厂商 $1 id=$2 → ${CODE}，error=$(py get "${BODY}" error)"; fi
}

# ---------- 服务管理 ----------
up_one() { # up_one 名称 健康检查URL 环境变量...
  local name=$1 url=$2; shift 2
  if curl -s --max-time 1 -o /dev/null "${url}"; then echo "  ${name} 已在运行（${url} 有响应），跳过"; return; fi
  env "$@" nohup "./bin/${name}" > "${LOGS}/${name}.log" 2>&1 &
  echo $! > "${LOGS}/${name}.pid"
  for _ in $(seq 1 100); do curl -s --max-time 1 -o /dev/null "${url}" && break; sleep 0.1; done
  if curl -s --max-time 1 -o /dev/null "${url}"; then echo "  ${name} 已启动 pid=$(cat "${LOGS}/${name}.pid")，日志 ${LOGS}/${name}.log"
  else echo "  ${name} 启动失败，看 ${LOGS}/${name}.log"; return 1; fi
}
cmd_up() {
  echo "构建（make build）..."
  make -s build >/dev/null || { echo "构建失败"; return 1; }
  mkdir -p "${LOGS}"
  up_one vendorsim "${V}/healthz" || return 1
  up_one platform "${P}/healthz" || return 1
  up_one demo-go "${D}/healthz" || return 1
  for _ in $(seq 1 50); do [ "$(py get "$(curl -s "${D}/healthz")" ready)" = True ] && break; sleep 0.1; done
  echo "三个服务就绪：platform ${P} / vendorsim ${V} / demo-go ${D}"
}
services_up() { curl -s --max-time 1 -o /dev/null "${P}/healthz" && curl -s --max-time 1 -o /dev/null "${V}/healthz" && curl -s --max-time 1 -o /dev/null "${D}/healthz"; }
ensure_up() {
  if ! services_up; then
    do_ "检测到服务没起，先执行 ./demo.sh up"
    cmd_up >"${TMP}/up.log" 2>&1 || { cat "${TMP}/up.log"; echo "服务启动失败"; exit 1; }
  fi
}
port_pids() { lsof -ti "tcp:$1" -sTCP:LISTEN 2>/dev/null; }
cmd_down() {
  local pids
  # 先停 demo-go，让它自己回收插件子进程
  for spec in demo-go:9102 platform:9100 vendorsim:9101; do
    local name=${spec%%:*} port=${spec##*:}
    pids=$( { cat "${LOGS}/${name}.pid" 2>/dev/null; port_pids "${port}"; } | sort -u)
    for pid in ${pids}; do
      alive "${pid}" || continue
      kill -TERM "${pid}" 2>/dev/null
      for _ in $(seq 1 50); do alive "${pid}" || break; sleep 0.1; done
      alive "${pid}" && kill -KILL "${pid}" 2>/dev/null
      echo "  已停止 ${name} pid=${pid}"
    done
  done
  pids=$(pgrep -f "${ROOT}/var/demo-go/plugins/" 2>/dev/null)
  for pid in ${pids}; do kill -KILL "${pid}" 2>/dev/null && echo "  已清理残留插件子进程 pid=${pid}"; done
  rm -rf var
  echo "已清空 var/，下次 ./demo.sh up 从干净状态开始"
}
cmd_status() {
  echo "===== 当前状态（GET /admin/status）====="
  py table "$(status_json)"
}

# ---------- 前置补齐 ----------
BIN_A1=bin/plugins/vendor-a-v1; BIN_A2=bin/plugins/vendor-a-v2; BIN_B=bin/plugins/vendor-b
ensure_uploaded() { # vendor version bin
  if [ -z "$(py plugin "$(plugins_json)" "$1" "$2" sha256)" ]; then
    req POST "${P}/api/plugins" -F "vendor=$1" -F "version=$2" -F "file=@$3"
    do_ "上传 $1 $2 到平台：curl -F vendor=$1 -F version=$2 -F file=@$3 ${P}/api/plugins → ${CODE}，sha256=$(py get "${BODY}" sha256 | cut -c1-12)…"
  else do_ "平台上已有 $1 $2，跳过上传"; fi
}
ensure_a_v1() {
  if [ "$(host_active A)" != v1 ]; then
    do_ "检测到 demo-go 上 A 的活跃版本不是 v1（当前：$(host_active A | sed 's/^$/未装/')），先补装/切到 A v1"
    ensure_uploaded A v1 "${BIN_A1}"
    req POST "${P}/api/plugins/A/v1/activate"
    do_ "curl -X POST ${P}/api/plugins/A/v1/activate → ${CODE}"
  fi
}

# ---------- 六步 ----------
step1() {
  begin 1 "厂商 A 从无到有：上传 v1 + 发布，demo-go 不重启即可查询"
  ensure_up
  local before; before=$(host_active A)
  if [ -n "${before}" ]; then
    do_ "检测到 A 已装（当前 ${before}，非干净状态），跳过「未装 → 404」演示；想从头看：./demo.sh down && ./demo.sh up"
  else
    do_ "查询未装插件的厂商 A：curl '${D}/query?vendor=A&id=1'"
    query_desc A 1; res "${QD}（404 plugin_not_installed = 还没有 A 的插件）"
    [ "${CODE}" = 404 ] || bad "未装时应 404，实际 ${CODE}"
  fi
  ensure_uploaded A v1 "${BIN_A1}"
  local mark; mark=$(wc -l < "${LOGS}/demo-go.log" 2>/dev/null || echo 0)
  req POST "${P}/api/plugins/A/v1/activate"
  do_ "在平台发布 A v1：curl -X POST ${P}/api/plugins/A/v1/activate → ${CODE}"
  res "$(py notified "${BODY}")"
  if [ -f "${LOGS}/demo-go.log" ]; then
    res "demo-go 日志（这次发布触发的 验签 / 下载 / spawn / 切换）："
    while IFS= read -r l; do RES+=("|${l#*\] }"); done < <(tail -n +"$((mark + 1))" "${LOGS}/demo-go.log" | grep -E '验签|下载|spawn|已就绪|切换' | grep -v '^\[plugin' | tail -6)
  fi
  do_ "再查 A：curl '${D}/query?vendor=A&id=1'"
  query_desc A 1; res "${QD}"
  [ "${CODE}" = 200 ] && [ "${QV}" = v1 ] || bad "发布后应返回 v1，实际 ${CODE}/${QV}"
  finish
}

step2() {
  begin 2 "厂商 B 并存：装 B 不影响 A"
  ensure_up; ensure_a_v1
  ensure_uploaded B v1 "${BIN_B}"
  req POST "${P}/api/plugins/B/v1/activate"
  do_ "发布 B v1：curl -X POST ${P}/api/plugins/B/v1/activate → ${CODE}"
  res "$(py notified "${BODY}")"
  do_ "查 B：curl '${D}/query?vendor=B&id=7'"
  query_desc B 7; res "${QD}"
  [ "${CODE}" = 200 ] || bad "B 查询应 200，实际 ${CODE}"
  local vs; vs=$(curl -s "${V}/vendorB/goods/7")
  res "对照：vendorsim 原始返回 ${vs}（B 的接口形状不同，插件已映射成统一结构）"
  do_ "再查 A：curl '${D}/query?vendor=A&id=1'"
  query_desc A 1; res "${QD}，A 仍是 v1，说明两家厂商插件互不影响"
  [ "${QV}" = v1 ] || bad "A 应仍为 v1，实际 ${QV}"
  finish
}

step3() {
  begin 3 "A 热升级 v1→v2：在途慢请求在 v1 上走完，新请求立刻走 v2，v1 排空后进程退出"
  ensure_up; ensure_a_v1
  if [ -n "$(py ver "$(status_json)" A v2 pid)" ]; then
    req POST "${P}/api/plugins/A/retire?version=v2"
    do_ "检测到 demo-go 上残留 A v2（待命），先退役它以便完整演示：curl -X POST '${P}/api/plugins/A/retire?version=v2' → ${CODE}"
    sleep 0.5
  fi
  ensure_uploaded A v2 "${BIN_A2}"
  local v1pid; v1pid=$(py ver "$(status_json)" A v1 pid)
  curl -s "${D}/query?vendor=A&id=2&delay_ms=3000" > "${TMP}/slow" &
  local slowjob=$!
  do_ "后台发一个 3 秒的慢请求：curl '${D}/query?vendor=A&id=2&delay_ms=3000' &（此时 A 活跃是 v1，pid=${v1pid}）"
  sleep 0.3
  req POST "${P}/api/plugins/A/v2/activate"
  do_ "0.3 秒后发布 A v2：curl -X POST ${P}/api/plugins/A/v2/activate → ${CODE}"
  res "$(py notified "${BODY}")"
  do_ "立刻查 A：curl '${D}/query?vendor=A&id=1'"
  query_desc A 1; res "${QD}（新请求已经走 v2；v2 调的是厂商改过形状的新接口）"
  [ "${QV}" = v2 ] || bad "新请求应走 v2，实际 ${QV}"
  local st; st=$(status_json)
  do_ "慢请求还没回来时看状态：curl ${D}/admin/status"
  res "此刻 v1：状态=$(py ver "${st}" A v1 state)，在途 refs=$(py ver "${st}" A v1 refs)，pid=$(py ver "${st}" A v1 pid)（draining = 不再接新请求，等在途请求做完）"
  [ "$(py ver "${st}" A v1 state)" = draining ] && [ "$(py ver "${st}" A v1 refs)" -ge 1 ] 2>/dev/null || bad "慢请求在途时 v1 应 draining 且 refs≥1"
  wait "${slowjob}"
  local sb; sb=$(cat "${TMP}/slow")
  do_ "等后台慢请求返回"
  res "慢请求返回：plugin_version=$(py get "${sb}" plugin_version)，name=$(py get "${sb}" name)（升级前进来的请求由 v1 完整处理，没被打断）"
  [ "$(py get "${sb}" plugin_version)" = v1 ] || bad "慢请求应由 v1 产生"
  local gone=""
  for _ in $(seq 1 40); do
    st=$(status_json)
    if [ -z "$(py ver "${st}" A v1 state)" ] && ! alive "${v1pid}"; then gone=1; break; fi
    sleep 0.05
  done
  do_ "再看状态，并 kill -0 ${v1pid} 确认旧进程"
  res "状态里 A 的版本：$(py get "${st}" vendors.A.versions | python3 -c 'import json,sys; print(", ".join("%s(%s, pid=%s)" % (x["version"], x["state"], x["pid"]) for x in json.load(sys.stdin)))')"
  if alive "${v1pid}"; then res "kill -0 ${v1pid} 成功 → v1 进程仍在"; else res "kill -0 ${v1pid} 失败 → v1 进程已退出（排空后被关闭）"; fi
  [ -n "${gone}" ] || bad "v1 排空后应退出且从状态里消失"
  finish
}

step4() {
  begin 4 "回滚到 v1：v2 转为待命可再前滚；确认后退役 v2，v2 进程退出"
  ensure_up
  if [ "$(host_active A)" != v2 ]; then
    do_ "检测到 A 当前不是 v2（当前：$(host_active A | sed 's/^$/未装/')），先补齐「A 已升级到 v2」的前置"
    ensure_uploaded A v1 "${BIN_A1}"; ensure_uploaded A v2 "${BIN_A2}"
    req POST "${P}/api/plugins/A/v2/activate"
    do_ "curl -X POST ${P}/api/plugins/A/v2/activate → ${CODE}"
  fi
  req POST "${P}/api/plugins/A/v1/activate"
  do_ "回滚：curl -X POST ${P}/api/plugins/A/v1/activate → ${CODE}"
  res "平台判定为回滚（keep_previous=$(py get "${BODY}" keep_previous)，目标版本比当前更早上传）；$(py notified "${BODY}")"
  do_ "查 A：curl '${D}/query?vendor=A&id=1'"
  query_desc A 1; res "${QD}"
  [ "${QV}" = v1 ] || bad "回滚后应返回 v1，实际 ${QV}"
  local st v2pid; st=$(status_json); v2pid=$(py ver "${st}" A v2 pid)
  res "状态：A 活跃=$(py get "${st}" vendors.A.active)；v2 状态=$(py ver "${st}" A v2 state)、pid=${v2pid}（standby = 进程保留，可随时再切回）"
  [ "$(py ver "${st}" A v2 state)" = standby ] && alive "${v2pid}" || bad "回滚后 v2 应为 standby 且进程存活"
  req POST "${P}/api/plugins/A/retire?version=v2"
  do_ "确认回滚，退役 v2：curl -X POST '${P}/api/plugins/A/retire?version=v2' → ${CODE}"
  for _ in $(seq 1 40); do st=$(status_json); [ -z "$(py ver "${st}" A v2 state)" ] && ! alive "${v2pid}" && break; sleep 0.05; done
  res "状态里 A 只剩：$(py get "${st}" vendors.A.versions | python3 -c 'import json,sys; print(", ".join("%s(%s)" % (x["version"], x["state"]) for x in json.load(sys.stdin)))')"
  if alive "${v2pid}"; then res "kill -0 ${v2pid} 成功 → v2 进程仍在"; bad "v2 进程应已退出"; else res "kill -0 ${v2pid} 失败 → v2 进程已退出"; fi
  finish
}

step5() {
  begin 5 "篡改防护：伪造 sha256 的发布事件被拒，当前版本不受影响"
  ensure_up; ensure_a_v1
  ensure_uploaded A v2 "${BIN_A2}"
  local pl sig; pl=$(plugins_json); sig=$(py plugin "${pl}" A v2 signature)
  local ev; ev=$(printf '{"event":"activate","vendor":"A","version":"v2","sha256":"%s","signature":"%s","download_url":"%s/api/plugins/A/v2/download"}' "$(printf '0%.0s' $(seq 1 64))" "${sig}" "${P}")
  do_ "构造伪造事件：A v2，签名是平台的真签名，但 sha256 改成 64 个 0（模拟产物被换）"
  do_ "绕过平台直接打 demo-go：curl -X POST ${D}/internal/plugin-event -d '{\"event\":\"activate\",\"vendor\":\"A\",\"version\":\"v2\",\"sha256\":\"000…\",…}'"
  req POST "${D}/internal/plugin-event" -H 'Content-Type: application/json' -d "${ev}"
  res "demo-go 拒绝 → HTTP ${CODE}，原因 $(py get "${BODY}" error)（签名只覆盖真 sha256，改过的 sha256 验签不过，连下载都不会发生）"
  [ "${CODE}" = 403 ] && [ "$(py get "${BODY}" error)" = signature_invalid ] || bad "应 403 signature_invalid，实际 ${CODE} $(py get "${BODY}" error)"
  do_ "查 A：curl '${D}/query?vendor=A&id=1'"
  query_desc A 1; res "${QD}（仍是 v1，没被伪造事件切走）"
  [ "${QV}" = v1 ] || bad "A 应仍为 v1，实际 ${QV}"
  finish
}

step6() {
  begin 6 "崩溃隔离：kill -9 插件子进程，宿主照常服务，插件被自动拉起"
  ensure_up; ensure_a_v1
  local st old r0; st=$(status_json); old=$(py ver "${st}" A v1 pid); r0=$(py ver "${st}" A v1 restarts)
  kill -9 "${old}" 2>/dev/null
  do_ "kill -9 ${old}（A v1 插件子进程，当前已重启 ${r0} 次）"
  do_ "立刻查 A：curl '${D}/query?vendor=A&id=1'"
  query_desc A 1; res "崩溃后立刻：${QD}"
  local t0 ok=""; t0=$(python3 -c 'import time; print(time.time())')
  do_ "每 0.1 秒重查，最多等 5 秒直到 200"
  for _ in $(seq 1 50); do query_desc A 1; [ "${CODE}" = 200 ] && { ok=1; break; }; sleep 0.1; done
  local took; took=$(python3 -c "import time; print('%.1f' % (time.time()-${t0}))")
  if [ -n "${ok}" ]; then query_desc A 1; res "约 ${took}s 后恢复：${QD}"; else res "5 秒内没恢复，最后一次 ${CODE}"; bad "5s 内未恢复"; fi
  st=$(status_json)
  local new rs; new=$(py ver "${st}" A v1 pid); rs=$(py ver "${st}" A v1 restarts)
  res "状态：A v1 新 pid=${new}（旧 ${old}），restarts=${rs}（监督协程自动重启，最多 3 次，退避 1s）"
  [ "${new}" != "${old}" ] && [ "${rs}" = $((r0 + 1)) ] || bad "应换新 pid 且 restarts=$((r0 + 1))"
  if alive "${old}"; then bad "旧 pid 仍存活"; fi
  finish
}

case "${1:-}" in
  up) cmd_up ;;
  down) cmd_down ;;
  status) cmd_status ;;
  [1-6]) "step$1" ;;
  all) rc=0; for n in 1 2 3 4 5 6; do "step${n}" || rc=1; done; cmd_status; exit ${rc} ;;
  *) echo "用法：./demo.sh up | 1..6 | all | status | down"; echo "  up     构建并后台起 vendorsim/platform/demo-go（已在跑则跳过）"; echo "  1..6   分步演示（前置不满足会自动补齐）"; echo "  all    顺序跑 1~6"; echo "  status 中文表格显示 demo-go 状态"; echo "  down   停掉三个服务和插件子进程，清空 var/"; exit 2 ;;
esac
