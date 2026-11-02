# 017 验收发现的缺陷记录

## DEF-01（B 级）seed 在活库上重播种失败

| 项 | 内容 |
|---|---|
| 发现方式 | AC-19 全链路演示（改阈值 → `make seed-sql` → `make seed` → 60 秒热加载）执行中断 |
| 级别 | **B**（功能缺失或口径错误——配置重建在活库上不可用） |
| 现象 | `make seed` 报 `update or delete on table "terminal" violates foreign key constraint "terminal_state_terminal_id_fkey"` |
| 根因 | `WriteSeedSQL` 用 `DELETE FROM terminal` 清空配置域投影，依赖级联清 bus/device/point；但 `terminal_state`（运行态表）外键引用 `terminal` 且**无级联**，链路跑过一次、运行态落库后，重播种永远失败 |
| 影响 | AGENTS.md 的命令约定「改配置后跑 `make seed-sql` + `make seed`」在活库上走不通；`WriteSeedSQL` 的「重复导入得到同一结果」幂等承诺不成立 |
| 修复 | `fix(config): seed 在活库上重播种被 terminal_state 外键挡住，terminal 改 upsert`（commit 24dc97d）：terminal 改 `INSERT ... ON CONFLICT DO UPDATE`；级联清理面从 terminal 换成 `DELETE FROM bus`（级联 device/point）；配置里已消失的终端单独 DELETE，仍被运行态引用时外键报错即为信号，**不静默连 terminal_state 一起删** |
| 未动 | 表结构零改动、业务表零触碰（`terminal_state` 5 行运行态在修复后的重播种中完整保留，见 AC-19 实测） |
| 回归验证 | `scripts/accept-017.py` 的 AC-19 用例——活库连播两次正是复现场景；修复后两连播均成功且行数一致（140 测点 / 95 阈值行） |

### 修复前后对比（活库连播两次，同库同配置）

| | 修复前 | 修复后 |
|---|---|---|
| 第 1 次导入 | 失败（外键冲突） | 成功，D-01~D-04 断言 140/5/50/95 |
| 第 2 次导入 | 未到达 | 成功，同断言一致（幂等） |
| terminal_state | — | 5 行运行态保留 |

### 备注

- 种子脚本自身的「D-01~D-04」是初始化断言编号（测点/终端/设备/阈值行数），
  与本文件的缺陷号 DEF-01 是两套编号，勿混淆。
- 本缺陷属「验收执行发现并当场闭环」，未进 `docs/07` 开放项——
  开放项登记的是待客户/现场确认的未决事项，已修复缺陷进测试报告缺陷明细（018）。
