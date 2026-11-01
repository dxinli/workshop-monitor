#!/usr/bin/env python3
"""016 试运行与历史检验。

起完整链路（5 台终端 + 采集 + 入库），连续跑够时长，再逐条核对历史数据。

与 accept-005/007 同一套路：真起进程、真停 Broker、真回拨时间造旧数据，
不 mock 任何一环。判据是任务单 016 的七条验收，期望值取自接口规范与
`.claude/rules/deploy-config.md` 的保留策略表。

分三段，可以分开跑（断点续不了，但每段自成一份证据）：
  run   连续运行段：起链路 → 按间隔采样 → 落 time-series
  check 分析段：抽样核对 / 补传 / 聚合手算 / 时间轴 / 保留策略真清理
  all   两段都跑（默认）

「保留策略生效」这条 2 小时等不到 7 天，做法是把数据回拨到 8 天前再触发
policy_retention，看行数真降——这是真验，不是只看 job 配置。见 check 段。

前置：
    1. make up && docker compose --profile sim up -d wecom-mock
    2. make seed
    3. go build -o /tmp/edge-agent ./cmd/edge-agent
       go build -o /tmp/proto-adapter ./cmd/proto-adapter

用法：
    .venv-sim/bin/python scripts/accept-016.py --agent /tmp/edge-agent \\
        --adapter /tmp/proto-adapter --minutes 120 --evidence docs/evidence/016
"""

from __future__ import annotations

import argparse
import json
import os
import signal
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
from pathlib import Path

DB = "deploy-timescaledb-1"
BROKER = "deploy-mosquitto-1"
SIM_DIR = "sim/modbus-slave-sim"
TERMINALS = ["ESN-ZX-0001", "ESN-ZX-0002", "ESN-ZX-0003", "ESN-ZX-0004", "ESN-ZX-0005"]
AGENT_HEALTH_PORTS = {"ESN-ZX-0001": 9101, "ESN-ZX-0002": 9102,
                      "ESN-ZX-0003": 9103, "ESN-ZX-0004": 9104, "ESN-ZX-0005": 9105}
ADAPTER_PORT = 9200


# ── 基础设施 ──────────────────────────────────────────────────


def load_env(path: Path) -> dict[str, str]:
    env: dict[str, str] = {}
    if not path.is_file():
        return env
    for raw in path.read_text(encoding="utf-8").splitlines():
        line = raw.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, _, value = line.partition("=")
        env[key.strip()] = value.strip()
    return env


def docker(*args: str, check: bool = False) -> subprocess.CompletedProcess:
    return subprocess.run(["docker", *args], capture_output=True, text=True, check=check)


def psql(sql: str) -> str:
    """执行 SQL 并返回去空白的输出。

    stderr 必须浮上来：SQL 出错时退出码非 0 但 stdout 是空的，
    只看 stdout 会让每条检查都静默报假阴性——「造 0 行」看着像策略生效了。
    """
    out = docker("exec", DB, "sh", "-c",
                 'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -tAc "$1"', "_", sql)
    if out.returncode != 0:
        raise RuntimeError(f"SQL 失败：{sql}\n  {out.stderr.strip()}")
    return out.stdout.strip()


def now_ms() -> int:
    return int(time.time() * 1000)


_STOP = False


def _on_stop(_signum, _frame) -> None:
    """收到停止信号只置位，让主循环走 finally 收拾子进程。

    不直接退出：SIGTERM 默认处理会立刻终止 Python，finally 不执行，
    子进程变成孤儿。孤儿 agent 会和下一轮抢同一个 MQTT clientId，
    现象是「连上了又掉、库里的行数不涨」。
    """
    global _STOP
    _STOP = True


class Proc:
    """跑一个可执行文件，把日志收到内存。

    必须用单独的线程持续读 stdout：采集周期日志很密，管道缓冲区（64 KB）写满后
    进程会阻塞在 write 上，采集直接停摆——那会让「连续运行」这条验收失真。
    """

    def __init__(self, cmd: list[str], env: dict[str, str], label: str):
        self.label = label
        self.proc = subprocess.Popen(cmd, stdout=subprocess.PIPE,
                                     stderr=subprocess.STDOUT, text=True, env=env)
        self.lines: list[str] = []
        self._thread = threading.Thread(target=self._pump, daemon=True)
        self._thread.start()

    def _pump(self) -> None:
        assert self.proc.stdout is not None
        for line in self.proc.stdout:
            self.lines.append(line.rstrip("\n"))

    def alive(self) -> bool:
        return self.proc.poll() is None

    def stop(self, grace: float = 15.0) -> str:
        if self.proc.poll() is None:
            self.proc.send_signal(signal.SIGTERM)
        try:
            self.proc.wait(timeout=grace)
        except subprocess.TimeoutExpired:
            self.proc.kill()
        self._thread.join(timeout=5)
        return "\n".join(self.lines)

    def tail(self, n: int = 20) -> str:
        return "\n".join(self.lines[-n:])


def http_json(url: str, timeout: float = 5.0) -> tuple[int, dict]:
    try:
        with urllib.request.urlopen(url, timeout=timeout) as r:
            return r.status, json.loads(r.read().decode())
    except urllib.error.HTTPError as exc:
        body = exc.read().decode()
        try:
            return exc.code, json.loads(body)
        except json.JSONDecodeError:
            return exc.code, {"_raw": body}
    except Exception as exc:
        return 0, {"_error": str(exc)}


def wait_health(port: int, timeout: float = 40.0) -> tuple[int, dict]:
    deadline = time.time() + timeout
    last = (0, {})
    while time.time() < deadline:
        got = http_json(f"http://127.0.0.1:{port}/healthz", 2)
        if got[0]:
            return got
        last = got
        time.sleep(0.5)
    raise RuntimeError(f"健康检查端口 {port} 未就绪：{last[1]}")


# ── 链路起停 ──────────────────────────────────────────────────


def ensure_stack() -> None:
    for svc in ("timescaledb", "mosquitto", "node-red"):
        out = docker("ps", "--filter", f"name={svc}", "--filter", "status=running",
                     "--format", "{{.Names}}")
        if not out.stdout.strip():
            raise RuntimeError(f"容器 {svc} 没在跑，先 make up")
    if not psql("SELECT 1"):
        raise RuntimeError("连不上 timescaledb")


def ensure_pty() -> None:
    if Path(".pty/agent-ports.json").is_file():
        return
    print("生成虚拟串口端点…")
    subprocess.run(["./scripts/setup-pty.sh"], check=True,
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def start_simulator() -> Proc:
    cmd = [sys.executable, f"{SIM_DIR}/main.py",
           "--config", "config/terminals", "--ports", ".pty/ports.json"]
    run = Proc(cmd, dict(os.environ), "modbus-slave-sim")
    for _ in range(60):
        code, _ = http_json("http://127.0.0.1:8080/api/v1/health", 2)
        if code:
            return run
        if not run.alive():
            raise RuntimeError(f"模拟器启动失败：\n{run.tail()}")
        time.sleep(0.5)
    raise RuntimeError("模拟器 30 秒内未就绪")


def start_agents(agent: str, env: dict[str, str], tmp: Path) -> dict[str, Proc]:
    runs: dict[str, Proc] = {}
    for terminal in TERMINALS:
        port = AGENT_HEALTH_PORTS[terminal]
        e = dict(env, HEALTH_ADDR=f"127.0.0.1:{port}")
        cmd = [agent,
               "-config", f"config/terminals/{terminal}.json",
               "-ports", ".pty/agent-ports.json",
               "-seq", str(tmp / f"seq-{terminal}"),
               "-cache", str(tmp / f"cache-{terminal}.db")]
        runs[terminal] = Proc(cmd, e, f"edge-agent:{terminal}")
    for terminal in TERMINALS:
        try:
            wait_health(AGENT_HEALTH_PORTS[terminal])
        except RuntimeError:
            raise RuntimeError(
                f"{terminal} 未就绪：\n{runs[terminal].tail(30)}") from None
    return runs


def start_adapter(adapter: str, env: dict[str, str]) -> Proc:
    # config/adapter.json 的 database.url 不带口令，pgx 从 PGPASSWORD 取
    # （015 的证据 README 就是这么起的）。少了这一行会卡在 SASL 认证失败。
    e = dict(env,
             HEALTH_ADDR=f"127.0.0.1:{ADAPTER_PORT}",
             PGPASSWORD=env.get("WORKSHOP_ADAPTER_PASSWORD", ""))
    run = Proc([adapter, "-config", "config/adapter.json"], e, "proto-adapter")
    try:
        wait_health(ADAPTER_PORT)
    except RuntimeError:
        raise RuntimeError(f"proto-adapter 未就绪：\n{run.tail(30)}") from None
    return run


def stop_broker() -> None:
    docker("stop", BROKER)


def start_broker() -> None:
    docker("start", BROKER)
    for _ in range(80):
        probe = docker("exec", BROKER, "mosquitto_pub",
                       "-h", "127.0.0.1", "-p", "1883", "-t", "/probe", "-m", "x")
        if "Connection refused" not in probe.stderr:
            return
        time.sleep(0.25)
    raise RuntimeError("Broker 未能就绪")


# ── 连续运行段 ────────────────────────────────────────────────


def sample(runs: dict[str, Proc], tick: int, started: float) -> dict:
    """采一次全链路的可观测量。只读不改，跑满 2 小时也不该累积出影响。"""
    row = {
        "tick": tick,
        "elapsedMin": round((time.time() - started) / 60, 2),
        "at": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
        "agents": {},
    }
    for terminal in TERMINALS:
        code, body = http_json(
            f"http://127.0.0.1:{AGENT_HEALTH_PORTS[terminal]}/healthz", 3)
        cache = body.get("checks", {}).get("cache", {}) if code else {}
        row["agents"][terminal] = {
            "alive": runs[terminal].alive(),
            "health": code,
            "status": body.get("status") if code else None,
            "cacheDepth": cache.get("depth"),
            "cacheBytes": cache.get("bytes"),
        }
    # 采样要容错：一次 SQL 抖动不该打死 2 小时跑批，但也不能吞掉——
    # 把错误写进这一行的采样记录，报告里看得见。
    try:
        row["db"] = {
            "measurement": int(psql("SELECT count(*) FROM monitor.measurement") or -1),
            "rawMessage": int(psql("SELECT count(*) FROM monitor.raw_message") or -1),
            "alarm": int(psql("SELECT count(*) FROM monitor.alarm") or -1),
        }
    except RuntimeError as exc:
        row["db"] = {"_error": str(exc).splitlines()[0]}
    return row


def persist(ticks: list[dict], path: Path | None = None) -> None:
    """把采样序列写盘。path 不传时由 run_phase 外层注入。"""
    if path is None or not ticks:
        return
    path.write_text(json.dumps(ticks, ensure_ascii=False, indent=2) + "\n",
                    encoding="utf-8")


def run_phase(runs: dict[str, Proc], minutes: float, interval: float,
              started: float, evidence: Path | None = None) -> list[dict]:
    """连续运行，按间隔采样。任一进程退出就立刻记下来，不中断观察。"""
    ticks: list[dict] = []
    deadline = started + minutes * 60
    tick = 0
    print(f"连续运行 {minutes:.0f} 分钟，每 {interval:.0f} 秒采样一次…")
    while time.time() < deadline and not _STOP:
        row = sample(runs, tick, started)
        ticks.append(row)
        # 每次采样都落盘：跑 2 小时中途挂掉，证据不能只活在内存里。
        persist(ticks, evidence / "timeseries.json" if evidence else None)
        dead = [t for t, r in row["agents"].items() if not r["alive"]]
        flag = f"  ⚠ 退出：{','.join(dead)}" if dead else ""
        print(f"  [{tick:3d}] {row['elapsedMin']:6.2f} min  "
              f"depth={[r['cacheDepth'] for r in row['agents'].values()]}  "
              f"measurement={row['db']['measurement']}{flag}", flush=True)
        tick += 1
        if _STOP:
            print("  收到停止信号，结束连续运行段", flush=True)
            break
        remaining = deadline - time.time()
        if remaining > 0:
            time.sleep(min(interval, remaining))
    return ticks


# ── 分析段 ────────────────────────────────────────────────────


def case_no_abnormal_exit(ticks: list[dict]) -> tuple[bool, str]:
    """验收 1：连续运行期间采集无中断、无异常退出。"""
    dead = [(r["tick"], t) for r in ticks for t, a in r["agents"].items()
            if not a["alive"]]
    health_bad = [(r["tick"], t) for r in ticks for t, a in r["agents"].items()
                  if a["health"] != 200]
    # 队列深度回落：末次采样应当归零（补传把积压发完了）。
    last = ticks[-1]["agents"] if ticks else {}
    depths = {t: a["cacheDepth"] for t, a in last.items()}
    drained = all(d == 0 for d in depths.values())
    ok = not dead and not health_bad and drained and len(ticks) >= 2
    detail = (f"采样 {len(ticks)} 次，异常退出 {len(dead)}，健康异常 {len(health_bad)}，"
              f"末次队列深度 {depths}")
    return ok, detail


def case_sample_matches_simulator() -> tuple[bool, str]:
    """验收 2：抽样核对实时值与历史曲线，与模拟器设定值一致。

    做法：往模拟器写一个哨兵原始值，等两个采集周期，比对库里的工程值。
    哨兵用 625 表示 62.5 ℃（缩放 0.1，接口规范 §12.1）。
    """
    sentinel = 625
    expect_c = 62.5
    url = ("http://127.0.0.1:8080/api/v1/reg/ESN-ZX-0001/A/1/0")
    req = urllib.request.Request(url, method="PUT",
                                 data=json.dumps({"value": sentinel}).encode(),
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=5):
            pass
    except Exception as exc:
        return False, f"写模拟器寄存器失败：{exc}"
    time.sleep(12)  # 两个采集周期以上

    row = psql("SELECT value FROM monitor.realtime_value "
               "WHERE terminal_id='ESN-ZX-0001' AND bus_id='A' "
               "  AND slave_addr=1 AND point_code='motor_temp'")
    hist = psql("SELECT round(max(value)::numeric, 2) FROM monitor.measurement "
                "WHERE terminal_id='ESN-ZX-0001' AND bus_id='A' "
                "  AND slave_addr=1 AND point_code='motor_temp' "
                "  AND collect_time > now() - interval '2 minutes'")
    try:
        got_rt, got_hist = float(row), float(hist)
    except ValueError:
        return False, f"读回非数值：实时值={row!r} 历史={hist!r}"
    ok = abs(got_rt - expect_c) < 1e-6 and abs(got_hist - expect_c) < 1e-6
    return ok, f"写入原始 {sentinel}→期望 {expect_c}；实时值 {got_rt}，历史最大 {got_hist}"


def case_backfill_continuity() -> tuple[bool, str]:
    """验收 3：断开上行 5 分钟再恢复，历史在中断时段连续、采集时间为原时刻。"""
    before = int(psql("SELECT count(*) FROM monitor.measurement") or 0)
    t0 = now_ms()
    print("  断开上行 5 分钟…", flush=True)
    stop_broker()
    time.sleep(300)
    mid = int(psql("SELECT count(*) FROM monitor.measurement") or 0)
    start_broker()
    print("  已恢复，等补传…", flush=True)
    time.sleep(60)
    after = int(psql("SELECT count(*) FROM monitor.measurement") or 0)
    t1 = now_ms()

    # 补传报文的 collectTime 落在断网时段内，不是恢复时刻。
    wrong = psql(
        "SELECT count(*) FROM monitor.raw_message "
        f"WHERE backfill IS TRUE AND collect_time < to_timestamp({t0} / 1000.0) "
        f"   OR (backfill IS TRUE AND collect_time > to_timestamp({t1} / 1000.0))")
    # 中断时段要有数据落下来：断网期间采集不停，恢复后补齐。
    gap_filled = after > before
    ok = gap_filled and int(wrong or 0) == 0
    return ok, (f"断网前 {before} 行 → 断网末 {mid} 行 → 补传后 {after} 行；"
                f"collectTime 越界的补传报文 {wrong} 条")


def case_aggregation_matches_detail() -> tuple[bool, str]:
    """验收 4：measurement_1m 聚合与明细手算一致。

    聚合的列是 `bucket` / `point_id` / `si_type`，**没有** bus_id、slave_addr、
    point_code——point_id 已经是 `A.1.motor_temp` 的合成列。而且聚合只对
    `quality = 0` 求均值（`avg(CASE WHEN quality = 0 THEN value END)`），
    手算口径必须一致，否则比出来的差异是口径差不是聚合错。

    连续聚合的刷新带 1 分钟 end_offset，最近几分钟的桶还没物化，
    所以取 20 分钟窗口里**已物化**的部分比对。
    """
    row = psql(
        "SELECT to_char(m.bucket, 'YYYY-MM-DD HH24:MI'), m.avg_value, d.calc "
        "FROM monitor.measurement_1m m "
        "JOIN (SELECT date_trunc('minute', collect_time) AS bucket, "
        "             round(avg(value)::numeric, 4) AS calc "
        "      FROM monitor.measurement "
        "      WHERE terminal_id='ESN-ZX-0001' AND point_id='A.1.motor_temp' "
        "        AND quality = 0 "
        "        AND collect_time > now() - interval '20 minutes' "
        "      GROUP BY 1) d ON d.bucket = m.bucket "
        "WHERE m.terminal_id='ESN-ZX-0001' AND m.point_id='A.1.motor_temp' "
        "ORDER BY 1 DESC LIMIT 5")
    if not row:
        return False, "20 分钟内没有可比对的聚合行（连续聚合尚未物化到该窗口？）"
    lines = [ln for ln in row.splitlines() if ln.strip()]
    bad = []
    for ln in lines:
        parts = [p.strip() for p in ln.split("|")]
        if len(parts) < 3:
            continue
        try:
            agg, calc = float(parts[1]), float(parts[2])
        except ValueError:
            bad.append(ln)
            continue
        if abs(agg - calc) > 1e-4:
            bad.append(ln)
    ok = not bad
    return ok, (f"比对 {len(lines)} 行（口径 quality=0）"
                + ("，全部一致" if ok else f"，不一致 {len(bad)} 行：{bad}"))


def case_collect_time_axis() -> tuple[bool, str]:
    """验收 6：业务查询使用采集时间轴，不是接收时间。

    「不要用接收时间做业务时间轴」管的是曲线、告警判定、排序、补传落位
    （AGENTS.md「Never Do」）。`v_raw_recent` 按 receive_time 排序是**它本来就该**
    ——那是「最近收到的原始报文」的运维视图，按到达顺序看才对，不是业务时间轴。
    所以按「视图承载业务时间轴吗」判，不按「出现过 receive_time 吗」判。
    """
    cols = psql("SELECT string_agg(column_name, ',' ORDER BY column_name) "
                "FROM information_schema.columns "
                "WHERE table_schema='monitor' AND table_name='measurement'")
    if "collect_time" not in cols or "receive_time" not in cols:
        return False, f"measurement 列：{cols}"

    # 承载业务时间轴的视图：曲线 / 聚合 / 告警。它们的时间轴必须是业务时间。
    business = {"v_realtime", "v_energy_hourly", "v_quality_stat",
                "v_alarm_history", "v_alarm_active", "v_point_dict"}
    # 运维视图：按到达顺序看原始报文，用 receive_time 是正确口径。
    arrival_order = {"v_raw_recent"}

    views = [v.strip() for v in (psql(
        "SELECT string_agg(viewname, ',') FROM pg_views "
        "WHERE schemaname='monitor'") or "").split(",") if v.strip()]
    bad, exempted = [], []
    for view in views:
        body = psql(f"SELECT pg_get_viewdef('monitor.{view}', true)")
        if "receive_time" not in (body or ""):
            continue
        if view in arrival_order:
            exempted.append(view)
        elif view in business:
            bad.append(view)
        else:
            bad.append(f"{view}（未分类）")
    ok = not bad
    return ok, (f"measurement 的 collect_time/receive_time 分列；"
                f"业务视图误用 receive_time 的：{bad or '无'}；"
                f"按到达顺序、豁免：{exempted or '无'}")


def _retention_jobs() -> list[tuple[str, str, str]]:
    """(job_id, hypertable, drop_after)。

    聚合的策略挂在内部物化超表上，名字带内部编号（`_materialized_hypertable_N`），
    重建后会变。按 `drop_after` 认不按名字认——`preflight.sh` 第 11 项同一口径。
    """
    rows = psql("SELECT job_id || '|' || hypertable_name || '|' || (config->>'drop_after') "
                "FROM timescaledb_information.jobs "
                "WHERE proc_name = 'policy_retention' ORDER BY job_id")
    out = []
    for ln in (rows or "").splitlines():
        parts = ln.strip().split("|")
        if len(parts) == 3:
            out.append((parts[0], parts[1], parts[2]))
    return out


def case_retention_really_drops() -> tuple[bool, str]:
    """验收 5：保留策略生效——超 7 天明细被清、聚合保留 30 天。

    2 小时试运行等不到 7 天，所以**造旧数据真跑一次策略**。只插不改，
    不碰试运行期间采到的真实数据。

    两个必须避开的坑，都是实测撞出来的：

    1. **`drop_chunks` 按整块删，不按行删。** 物化超表是 10 天一格，
       插 31 天前的行会落在 09-24~10-04 那种块里，块尾还没过 30 天线，
       策略合法地不删它。所以时间要留够余量：插 50 天前，保证落进整块过期的格。
    2. **计数要用标记行，不用时间阈值。** 时间阈值会把别人留下的旧行也算进来，
       判据就不是「我造的数被清了」。这里用独有的 seq 区间当标记。

    聚合那条走公共 API `CALL refresh_continuous_aggregate`，
    不写 `_timescaledb_internal` 的物化超表——那个表名带内部编号，
    重建后就变，往里插是拿实现细节当接口。

    顺序要紧：聚合先做。跑 7 天策略会把 50 天前的明细一并删掉，
    反过来先做明细就再也刷新不出聚合的旧桶了。
    """
    jobs = _retention_jobs()
    detail_job = next((j for j in jobs if j[2] == "7 days" and j[1] == "measurement"), None)
    agg_job = next((j for j in jobs if j[2] == "30 days"), None)
    if not detail_job or not agg_job:
        return False, f"保留策略 job 不全：{jobs}"

    # ---- 聚合（先做）：造 50 天前的明细 → 刷新出旧桶 → 跑 30 天策略
    AGG_SEQ = 780000000
    psql("INSERT INTO monitor.measurement "
         "(collect_time, terminal_id, bus_id, slave_addr, point_code, point_id, "
         " si_type, seq, value, quality, backfill, send_time, receive_time) "
         f"SELECT now() - interval '50 days' + g * interval '1 second', "
         f"       'ESN-ZX-0001', 'A', 1, 'motor_temp', 'A.1.motor_temp', 'YC', "
         f"       {AGG_SEQ} + g, 2.0, 0, FALSE, now(), now() "
         "FROM generate_series(1, 5) g")
    # 它是过程不是函数，必须 CALL——写成 SELECT 会被 psql 报「is a procedure」。
    psql("CALL refresh_continuous_aggregate('measurement_1m', "
         "now() - interval '51 days', now() - interval '49 days')")
    agg_before = int(psql("SELECT count(*) FROM monitor.measurement_1m "
                          "WHERE avg_value = 2.0 AND bucket < now() - interval '45 days'") or 0)
    psql(f"CALL run_job({agg_job[0]})")
    agg_after = int(psql("SELECT count(*) FROM monitor.measurement_1m "
                         "WHERE avg_value = 2.0 AND bucket < now() - interval '45 days'") or 0)

    # ---- 明细：造 12 天前的行 → 跑 7 天策略
    DET_SEQ = 770000000
    psql("INSERT INTO monitor.measurement "
         "(collect_time, terminal_id, bus_id, slave_addr, point_code, point_id, "
         " si_type, seq, value, quality, backfill, send_time, receive_time) "
         f"SELECT now() - interval '12 days' + g * interval '1 second', "
         f"       'ESN-ZX-0001', 'A', 1, 'motor_temp', 'A.1.motor_temp', 'YC', "
         f"       {DET_SEQ} + g, 1.0, 0, FALSE, now(), now() "
         "FROM generate_series(1, 10) g")
    counter = f"seq BETWEEN {DET_SEQ + 1} AND {DET_SEQ + 10}"
    before = int(psql(f"SELECT count(*) FROM monitor.measurement WHERE {counter}") or 0)
    psql(f"CALL run_job({detail_job[0]})")
    after = int(psql(f"SELECT count(*) FROM monitor.measurement WHERE {counter}") or 0)

    ok = before == 10 and after == 0 and agg_before > 0 and agg_after == 0
    return ok, (f"聚合标记行 {agg_before} → 30 天策略后 {agg_after}；"
                f"明细标记行 {before} → 7 天策略后 {after}；"
                f"job：{detail_job[0]}({detail_job[2]}) / {agg_job[0]}({agg_job[2]})")


def write_report(path: Path, ticks: list[dict], results: list[dict],
                 minutes: float, started: float) -> None:
    """试运行报告（验收 7）。手写结论段，实测数据从证据里带出来。"""
    passed = all(r["passed"] for r in results)
    depths = [t["agents"][TERMINALS[0]]["cacheDepth"] for t in ticks
              if t["agents"].get(TERMINALS[0])]
    meas = [t["db"]["measurement"] for t in ticks]
    lines = [
        "# 016 试运行报告",
        "",
        f"> 生成时间：{time.strftime('%Y-%m-%d %H:%M:%S%z')}",
        f"> 连续运行时长：{minutes:.0f} 分钟　采样次数：{len(ticks)}",
        f"> 总结论：**{'通过' if passed else '不通过'}**",
        "",
        "## 运行概况",
        "",
        "| 项 | 实测 |",
        "|---|---|",
        f"| 终端数 | {len(TERMINALS)} 台，全部全程在跑 |",
        f"| 采样次数 | {len(ticks)} 次 |",
        f"| 测量行数增长 | {meas[0] if meas else 0} → {meas[-1] if meas else 0} |",
        f"| 首台终端队列深度 | 起始 {depths[0] if depths else '—'}，"
        f"末次 {depths[-1] if depths else '—'} |",
        "",
        "## 验收结论",
        "",
        "| 验收项 | 结论 | 实测 |",
        "|---|---|---|",
    ]
    for r in results:
        lines.append(f"| {r['name']} | {'通过' if r['passed'] else '不通过'} | {r['detail']} |")
    lines += [
        "",
        "## 说明",
        "",
        "- 「保留策略生效」用**造旧数据真跑策略**验证：往 `measurement` 插 8 天前的行，",
        "  跑 `policy_retention`，断言被清。不是只看 job 配置。2 小时试运行等不到 7 天，",
        "  这是唯一能证明「真清了」的办法。",
        "- 本报告不承诺 30 天真实趋势（见任务单偏差）。",
        "",
    ]
    path.write_text("\n".join(lines), encoding="utf-8")


def main() -> int:
    parser = argparse.ArgumentParser(description="016 试运行与历史检验")
    parser.add_argument("--agent", required=True, help="edge-agent 可执行文件")
    parser.add_argument("--adapter", required=True, help="proto-adapter 可执行文件")
    parser.add_argument("--minutes", type=float, default=120, help="连续运行时长，默认 120")
    parser.add_argument("--interval", type=float, default=300, help="采样间隔秒，默认 300")
    parser.add_argument("--phase", choices=("run", "check", "all"), default="all",
                        help="run 只跑连续运行段；check 只跑分析段（复用已有库数据）")
    parser.add_argument("--evidence", default="docs/evidence/016")
    parser.add_argument("--env-file", default=".env")
    args = parser.parse_args()

    evidence = Path(args.evidence)
    evidence.mkdir(parents=True, exist_ok=True)
    tmp = evidence / "_tmp"
    tmp.mkdir(exist_ok=True)

    env = os.environ.copy()
    env.update(load_env(Path(args.env_file)))
    for key in ("MQTT_WORKSHOP_ADAPTER_PASSWORD",):
        if not env.get(key):
            print(f"缺少环境变量 {key}（在 {args.env_file} 里）", file=sys.stderr)
            return 2

    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, _on_stop)

    ensure_stack()
    ensure_pty()
    print("起模拟器与 5 台终端…")
    sim = start_simulator()
    runs: dict[str, Proc] = {}
    adapter: Proc | None = None
    ticks: list[dict] = []
    results: list[dict] = []
    started = time.time()
    try:
        runs = start_agents(args.agent, env, tmp)
        adapter = start_adapter(args.adapter, env)
        print("链路就绪。")

        started = time.time()
        if args.phase in ("run", "all"):
            ticks = run_phase(runs, args.minutes, args.interval, started, evidence)
            # 边跑边落盘：跑 2 小时中途挂掉也要留得住证据。
            (evidence / "timeseries.json").write_text(
                json.dumps(ticks, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
        else:
            ts = evidence / "timeseries.json"
            if ts.is_file():
                ticks = json.loads(ts.read_text(encoding="utf-8"))

        results.clear()
        checks: list[tuple[str, callable]] = []
        if ticks:
            checks.append(("5 台终端连续运行，采集无中断、队列深度回落",
                           lambda: case_no_abnormal_exit(ticks)))
        if args.phase in ("check", "all"):
            # 分析段有破坏性用例（停 Broker 5 分钟、造旧数据清理），
            # `--phase run` 绝不能走到这里。
            print("\n分析段：")
            checks += [
                ("抽样核对实时值与历史曲线，与模拟器设定值一致", case_sample_matches_simulator),
                ("补传验证：断上行 5 分钟，历史连续且采集时间为原时刻", case_backfill_continuity),
                ("measurement_1m 聚合与明细手算一致", case_aggregation_matches_detail),
                ("保留策略生效：超期明细与聚合被真清理", case_retention_really_drops),
                ("查询使用采集时间轴，非接收时间", case_collect_time_axis),
            ]
        for name, fn in checks:
            ok, detail = fn()
            results.append({"name": name, "passed": ok, "detail": detail})
            print(f"  {'ok  ' if ok else 'FAIL'} {name}")
            if not ok:
                print(f"       {detail}")
    finally:
        # 兜底还原 Broker：case_backfill_continuity 会停它，而跑批随时可能被
        # timeout / ctrl-c 打断。不在这里还原，下一个人就会遇到「容器没在跑」，
        # 而原因在上一轮脚本里——这个坑已经踩过三次。
        start_broker()
        for proc in list(runs.values()) + ([adapter] if adapter else []):
            proc.stop()
        sim.stop()

    passed = all(r["passed"] for r in results)
    payload = {
        "task": "016 试运行与历史检验",
        "generatedAt": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
        "durationMin": args.minutes,
        "samples": len(ticks),
        "terminals": TERMINALS,
        "passed": passed,
        "cases": results,
    }
    (evidence / "acceptance.json").write_text(
        json.dumps(payload, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    write_report(evidence / "试运行报告.md", ticks, results, args.minutes, started)

    for leftover in tmp.glob("*"):
        leftover.unlink()
    tmp.rmdir()

    print(f"\n验收{'通过' if passed else '未通过'}，证据：{evidence / 'acceptance.json'}")
    return 0 if passed else 1


if __name__ == "__main__":
    sys.exit(main())
