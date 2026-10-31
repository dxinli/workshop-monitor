#!/usr/bin/env bash
# 建立 10 条虚拟串口对，模拟 5 台终端 × 每台 2 条 RS485 总线。
#
# 每条总线一对 pty：``-sim`` 端接模拟器，``-agent`` 端给采集应用（005）。
#
# 端点表写到 .pty/ports.json（模拟器端）与 .pty/agent-ports.json（采集端），
# 键都是 ``{terminalId}.{busId}``。busId 只有 A / B（接口规范 §2.2），5 台终端的
# A 总线必须分开，否则 10 条总线会塌缩成 2 条，从站地址必然串号。
#
# 终端与总线清单来自 config/terminals/*.json，脚本不另维护一份。
# 点表里的 port 字段本脚本不改——改点表属于 005 的事。
#
# 用法：scripts/setup-pty.sh [--force]
#   已存在的总线对保留（反复执行不会重复起 socat）；--force 先停掉再重建。
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
PTY_DIR=$ROOT/.pty
ENV_FILE=$ROOT/.env
PORTS_FILE=$PTY_DIR/ports.json
AGENT_PORTS_FILE=$PTY_DIR/agent-ports.json
SOCAT_VERSION=1.8.0.0

FORCE=0
for arg in "$@"; do
  case "$arg" in
    --force) FORCE=1 ;;
    -h|--help) sed -n '2,18p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "未知参数：$arg" >&2; exit 2 ;;
  esac
done

if ! command -v socat >/dev/null 2>&1; then
  echo "缺少 socat，请先安装（版本 ${SOCAT_VERSION}）" >&2
  exit 1
fi

shopt -s nullglob
terminal_files=("$ROOT"/config/terminals/*.json)
if [ ${#terminal_files[@]} -eq 0 ]; then
  echo "config/terminals 下没有点表" >&2
  exit 1
fi

mkdir -p "$PTY_DIR"

# 从点表里取 busId。点表是唯一来源，不在脚本里写死 A / B。
bus_ids() {
  sed -n 's/.*"busId": *"\([A-Z]\)".*/\1/p' "$1"
}

sim_entries=()
agent_entries=()
count=0

for file in "${terminal_files[@]}"; do
  terminal=$(basename "$file" .json)
  buses=$(bus_ids "$file")
  if [ -z "$buses" ]; then
    echo "点表 $file 里没有 busId" >&2
    exit 1
  fi
  for bus in $buses; do
    key="$terminal.$bus"
    sim_link="$PTY_DIR/$terminal-$bus-sim"
    agent_link="$PTY_DIR/$terminal-$bus-agent"
    pidfile="$PTY_DIR/$terminal-$bus.pid"
    logfile="$PTY_DIR/$terminal-$bus.log"

    if [ "$FORCE" = 1 ] && [ -f "$pidfile" ]; then
      kill "$(cat "$pidfile")" 2>/dev/null || true
      rm -f "$pidfile"
    fi

    if [ -f "$pidfile" ] && kill -0 "$(cat "$pidfile")" 2>/dev/null; then
      :
    else
      rm -f "$sim_link" "$agent_link"
      # 每对 pty 一个 socat，互不影响：一条总线的 socat 挂掉不波及其它 9 条。
      setsid socat \
        "pty,raw,echo=0,link=$sim_link" \
        "pty,raw,echo=0,link=$agent_link" \
        >>"$logfile" 2>&1 </dev/null &
      echo $! >"$pidfile"
    fi

    # 等 socat 把两个 link 建出来。缺一个就报错退出——静默继续会让模拟器
    # 在「端口不存在」上反复重连，看起来像总线故障。
    ready=0
    for _ in $(seq 1 50); do
      if [ -e "$sim_link" ] && [ -e "$agent_link" ]; then
        ready=1
        break
      fi
      sleep 0.1
    done
    if [ "$ready" != 1 ]; then
      echo "总线 $key 的 pty 未建立，查看 $logfile" >&2
      exit 1
    fi

    sim_entries+=("  \"$key\": \"$sim_link\"")
    agent_entries+=("  \"$key\": \"$agent_link\"")
    count=$((count + 1))
  done
done

write_json() {
  local out=$1
  shift
  {
    printf '{\n'
    local first=1 line
    for line in "$@"; do
      [ "$first" = 1 ] || printf ',\n'
      first=0
      printf '%s' "$line"
    done
    printf '\n}\n'
  } >"$out.tmp"
  mv "$out.tmp" "$out"
}

write_json "$PORTS_FILE" "${sim_entries[@]}"
write_json "$AGENT_PORTS_FILE" "${agent_entries[@]}"

# 端点写进 .env 的受管区块：键名按环境变量规则（大写，. 与 - 转下划线）。
if [ -f "$ENV_FILE" ]; then
  begin='# >>> setup-pty 生成，勿手改 >>>'
  end='# <<< setup-pty >>>'
  tmp=$(mktemp)
  sed "/^$begin$/,/^$end$/d" "$ENV_FILE" >"$tmp"
  {
    echo "$begin"
    for line in "${agent_entries[@]}"; do
      key=$(printf '%s' "$line" | sed -n 's/^  "\([^"]*\)":.*/\1/p')
      val=$(printf '%s' "$line" | sed -n 's/.*: "\([^"]*\)".*/\1/p')
      [ -n "$key" ] || continue
      varname="PTY_$(printf '%s' "$key" | tr '.:-' '___' | tr '[:lower:]' '[:upper:]')"
      echo "$varname=$val"
    done
    echo "$end"
  } >>"$tmp"
  mv "$tmp" "$ENV_FILE"
  echo "端点已写入 $ENV_FILE"
else
  echo "未找到 $ENV_FILE，跳过写环境变量（先 cp .env.example .env）" >&2
fi

echo "已建立 $count 条总线"
echo "模拟器端点表：$PORTS_FILE"
echo "采集应用端点表：$AGENT_PORTS_FILE"