#!/usr/bin/env bash
# 配置校验验收：逐条制造坏配置，断言 V-01~V-11 各自报出对应的字段与原始值。
#
# 只改临时副本，不碰仓库里的 config/。每条用例独立目录，互不干扰。
# 用法：scripts/verify-config.sh [config-tool 可执行文件]
# 不给参数时用 go run。
set -uo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT" || exit 1

if [ $# -ge 1 ]; then
  TOOL=$1
elif [ -x "$ROOT/bin/config-tool" ]; then
  TOOL=$ROOT/bin/config-tool
else
  TOOL=$ROOT/bin/config-tool
  CGO_ENABLED=0 go build -trimpath -o "$TOOL" ./cmd/config-tool || exit 1
fi

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

pass=0
fail=0

ok() { echo "  ok   $1"; pass=$((pass + 1)); }
ng() { echo "  FAIL $1"; fail=$((fail + 1)); }

# fresh 复制一份干净的配置到 $1，供用例修改。
fresh() {
  rm -rf "$1"
  mkdir -p "$1"
  cp -r config/terminals "$1/terminals"
  cp config/thresholds.json "$1/thresholds.json"
  cp config/topics.json "$1/topics.json"
}

# mutate <目录> <python 表达式，d 为点表字典>
mutate() {
  python3 -I - "$1/terminals/ESN-ZX-0003.json" "$2" <<'PY'
import json, sys
path, expr = sys.argv[1], sys.argv[2]
d = json.load(open(path, encoding="utf-8"))
p0 = d["buses"][0]["devices"][0]["blocks"][0]["points"][0]
exec(expr)
with open(path, "w", encoding="utf-8") as f:
    json.dump(d, f, ensure_ascii=False, indent=2)
PY
}

# expect <用例名> <目录> <期望退出码 0|非0> <输出须包含> [输出须不包含]
expect() {
  local name=$1 dir=$2 want_nonzero=$3 must=$4 mustnot=${5:-}
  local out rc
  out=$("$TOOL" verify -terminals "$dir/terminals" -thresholds "$dir/thresholds.json" \
    -topics "$dir/topics.json" 2>&1)
  rc=$?
  if [ "$want_nonzero" = "0" ]; then
    if [ "$rc" -ne 0 ]; then
      ng "$name（期望通过，实际退出 $rc）"; printf '%s\n' "$out" | sed 's/^/       /'
      return
    fi
  else
    if [ "$rc" -eq 0 ]; then ng "$name（期望失败，实际通过）"; return; fi
  fi
  if [ -n "$must" ] && ! printf '%s' "$out" | grep -qF -- "$must"; then
    ng "$name（输出缺少：$must）"; printf '%s\n' "$out" | sed 's/^/       /'
    return
  fi
  if [ -n "$mustnot" ] && printf '%s' "$out" | grep -qF -- "$mustnot"; then
    ng "$name（输出不应包含：$mustnot）"; printf '%s\n' "$out" | sed 's/^/       /'
    return
  fi
  ok "$name"
}

echo "配置校验 V-01~V-11："

# 正例：仓库里的配置必须通过。
expect "合法配置返回 0" "$ROOT/config" 0 "问题 0 条"

# V-01 必填字段缺失。
fresh "$WORK/v01"
mutate "$WORK/v01" 'd["buses"][1]["devices"][0]["name"] = ""'
expect "V-01 设备名为空" "$WORK/v01" 1 "V-01 buses[1].devices[0].name"

# V-02 从站地址越界与重复。
fresh "$WORK/v02"
mutate "$WORK/v02" 'd["buses"][0]["devices"][1]["slaveAddr"] = 300'
expect "V-02 从站地址越界" "$WORK/v02" 1 "V-02 buses[0].devices[1].slaveAddr = 300"
fresh "$WORK/v02b"
mutate "$WORK/v02b" 'd["buses"][0]["devices"][1]["slaveAddr"] = 1'
expect "V-02 从站地址重复" "$WORK/v02b" 1 "V-02 buses[0].devices[1].slaveAddr = 1"

# V-03 测点编码格式与唯一性。
fresh "$WORK/v03"
mutate "$WORK/v03" 'p0["code"] = "MotorTemp"'
expect "V-03 编码含大写" "$WORK/v03" 1 'V-03 buses[0].devices[0].blocks[0].points[0].code = "MotorTemp"'
fresh "$WORK/v03b"
mutate "$WORK/v03b" 'd["buses"][0]["devices"][0]["blocks"][0]["points"][1]["code"] = "motor_temp"'
expect "V-03 编码重复" "$WORK/v03b" 1 "V-03 buses[0].devices[0].blocks[0].points[1].code"

# V-04 数据类型与宽度不匹配。
fresh "$WORK/v04"
mutate "$WORK/v04" 'p0["dataType"] = "uint32"'
expect "V-04 uint32 用 1 个寄存器" "$WORK/v04" 1 'V-04 buses[0].devices[0].blocks[0].points[0].width = 1'

# V-05 缩放系数为 0 / 量程倒置。
fresh "$WORK/v05"
mutate "$WORK/v05" 'p0["scale"] = 0'
expect "V-05 缩放系数为 0 并指名测点" "$WORK/v05" 1 \
  "V-05 buses[0].devices[0].blocks[0].points[0].scale = 0"
fresh "$WORK/v05b"
mutate "$WORK/v05b" 'p0["minValue"], p0["maxValue"] = 200, -20'
expect "V-05 量程倒置" "$WORK/v05b" 1 "V-05 buses[0].devices[0].blocks[0].points[0].minValue"

# V-06 块内重叠与越界。
fresh "$WORK/v06"
mutate "$WORK/v06" 'd["buses"][0]["devices"][1]["blocks"][0]["points"][1]["addr"] = 0'
expect "V-06 块内地址重叠" "$WORK/v06" 1 "V-06 buses[0].devices[1].blocks[0]"
fresh "$WORK/v06b"
mutate "$WORK/v06b" 'd["buses"][0]["devices"][4]["blocks"][0]["points"][0]["addr"] = 1'
expect "V-06 超出块寄存器数" "$WORK/v06b" 1 "V-06 buses[0].devices[4].blocks[0]"

# V-07 寄存器空洞：块声明 2 个寄存器却只映射 1 个。
fresh "$WORK/v07"
mutate "$WORK/v07" 'd["buses"][0]["devices"][0]["blocks"][0]["registerCount"] = 3'
expect "V-07 块尾未映射" "$WORK/v07" 1 "V-07 buses[0].devices[0].blocks[0]"

# V-08 四遥类型非法。
fresh "$WORK/v08"
mutate "$WORK/v08" 'p0["siType"] = "YK"'
expect "V-08 四遥类型非 YC/YX" "$WORK/v08" 1 'V-08 buses[0].devices[0].blocks[0].points[0].siType = "YK"'

# V-09 单从站寄存器数超过 125。
fresh "$WORK/v09"
mutate "$WORK/v09" 'd["buses"][0]["devices"][1]["blocks"][0]["registerCount"] = 126'
expect "V-09 单块超过 125" "$WORK/v09" 1 "V-09 buses[0].devices[1].blocks[0].registerCount = 126"

# V-10 数量断言只告警不拒绝：删掉一台终端仍返回 0，但输出 V-10。
fresh "$WORK/v10"
rm "$WORK/v10/terminals/ESN-ZX-0005.json"
expect "V-10 数量不符只告警" "$WORK/v10" 0 "V-10"
expect "V-10 告警时仍返回 0" "$WORK/v10" 0 "测点数"

# V-11 阈值类型、级别与测点引用。
fresh "$WORK/v11"
python3 -I - "$WORK/v11/thresholds.json" <<'PY'
import json, sys
p = sys.argv[1]
d = json.load(open(p, encoding="utf-8"))
d["rules"][0]["alarmType"] = "TOO_HOT"
json.dump(d, open(p, "w", encoding="utf-8"), ensure_ascii=False, indent=2)
PY
expect "V-11 告警类型不在枚举内" "$WORK/v11" 1 'V-11 rules[0].alarmType = "TOO_HOT"'
fresh "$WORK/v11b"
python3 -I - "$WORK/v11b/thresholds.json" <<'PY'
import json, sys
p = sys.argv[1]
d = json.load(open(p, encoding="utf-8"))
d["rules"][1]["level"] = 5
json.dump(d, open(p, "w", encoding="utf-8"), ensure_ascii=False, indent=2)
PY
expect "V-11 级别越界" "$WORK/v11b" 1 "V-11 rules[1].level = 5"
fresh "$WORK/v11c"
python3 -I - "$WORK/v11c/thresholds.json" <<'PY'
import json, sys
p = sys.argv[1]
d = json.load(open(p, encoding="utf-8"))
d["rules"][0]["pointCodes"] = ["motor_tmp"]
json.dump(d, open(p, "w", encoding="utf-8"), ensure_ascii=False, indent=2)
PY
expect "V-11 引用了点表里没有的测点" "$WORK/v11c" 1 'rules[0].pointCodes[0] = "motor_tmp"'

# 主题模板：两套方案必须与 deploy/mosquitto/acl.<scheme> 的授权主题一致。
echo "主题模板："
for scheme in grid internal; do
  want=$(grep 'data/up' "deploy/mosquitto/acl.$scheme" | awk '{print $3}')
  got=$(python3 -I - "$scheme" <<'PY'
import json, sys
s = json.load(open("config/topics.json", encoding="utf-8"))["schemes"][sys.argv[1]]["dataUp"]
for esn in ("ESN-ZX-0001", "ESN-ZX-0002", "ESN-ZX-0003", "ESN-ZX-0004", "ESN-ZX-0005"):
    print(s.replace("{esn}", esn))
PY
)
  if [ "$want" = "$got" ]; then
    ok "$scheme 主题模板与 ACL 一致"
  else
    ng "$scheme 主题模板与 ACL 不一致"; diff <(printf '%s\n' "$want") <(printf '%s\n' "$got") | sed 's/^/       /'
  fi
done

fresh "$WORK/topic"
python3 -I - "$WORK/topic/topics.json" <<'PY'
import json, sys
p = sys.argv[1]
d = json.load(open(p, encoding="utf-8"))
d["schemes"]["grid"]["eventUp"] = "/edge/no-placeholder/event/up"
json.dump(d, open(p, "w", encoding="utf-8"), ensure_ascii=False, indent=2)
PY
expect "主题模板缺 {esn}" "$WORK/topic" 1 'schemes.grid.eventUp = "/edge/no-placeholder/event/up"'

echo "通过 $pass，失败 $fail"
[ "$fail" -eq 0 ]