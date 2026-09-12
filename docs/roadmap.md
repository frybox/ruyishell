# Roadmap

## 下一步候选：任务溯源与模型 A/B 测评（/replay）

### 背景与动机

wukong 的核心卖点之一是"同一现场、同一任务、不同模型"的效率对比。触发时机是自然的：
任务执行得很好（很满意）或很不好（很不满意）时，想就同样的环境换模型再跑一遍做测试。

竞品参照（ds4-agent）：单进程、单 live session，`/save` 落 KV 快照到
`~/.ds4/kvcache`、`/switch` 切换——是**同一模型内**的会话持久化。跨模型对比它做不了，
wukong 的 A/B 分叉天然是**文本级**的（模型不同 KV 不通用），成本只是重 prefill。

核心设计判断：**不建独立 eval 模块**。给每个任务装上溯源记录后，
测评退化成"换个模型重放"：`/replay <task-id> --model <X>`。
平时每个任务自动成为可重放、可换模型对比的实验。

### 三个现场必须都复位

| 现场 | 复位手段 |
|---|---|
| 文件现场 | git（任务前后自动 commit） |
| 上下文现场 | transcript 树（任务边界分叉点） |
| 运行现场 | 进程显式清理（后台 job 杀掉） |

三者齐了，任意历史任务可重放，A/B 对比 = 同一 `/replay` 命令跑两遍。

### 需求拆解

#### R1. 任务溯源记录（最便宜，先做）

每个任务落一条记录，至少包含：

```json
{
  "task_id": "...",
  "repo_root": "/path/to/repo",
  "pre_commit": "...",
  "post_commit": "...",
  "model": "...",
  "sampling": {"temperature": 0, "seed": null},
  "transcript_branch": "main",
  "fork_point_msg_id": "...",
  "ts_start": "...", "ts_end": "...",
  "metrics": {"wall_s": 0, "tokens": 0, "turns": 0, "tools": {}},
  "outcome": "tests_passed"
}
```

commit 是环境溯源、model/sampling 是模型溯源、fork_point 是上下文溯源——三者缺一不可。
后续所有功能（/replay、/compare）都索引这条记录。

#### R2. 任务前后自动 git 提交

- 任务**开始**时：`git add -A && commit "wukong: pre-<task-id>"`
- 任务**结束**时：`git add -A && commit "wukong: post-<task-id>"`
- 每个任务对应 `[pre, post]` commit 区间；重放时 checkout 到 `pre` 即精确起点。
- 提交用固定 bot 身份（`wukong-bot`），与人类 commit 区分。
- 顺序约束：pre-commit 必须在任务第一行代码执行**前**打，
  否则用户手改的脏状态会混入下一个任务的 pre。

#### R3. transcript 树化 + 任务边界分叉点

- transcript 从列表改为带 parent 的树。
- 分叉点只允许在任务边界（任务开始/结束）——消息中间可能有在途 tool 状态
  （后台 job、半完成的 edit），恢复不了。
- 跨模型分叉是文本级的：同一份 transcript 前缀 + 同一个下一个任务 + 换模型，
  不需要任何 KV 快照。
- 同一时刻一条分支是活的（与单 session 设计一致），切分支 = 新起 run，老分支落盘。

#### R4. 后台进程重置策略

git 快照不了进程。run 结束或分叉时，杀掉该任务名下的所有后台 job；
replay 时从干净进程环境起。文件靠 git 复位，进程靠显式清理。

#### R5. 采样参数可锁定

跨模型对比默认 `temperature=0`（或固定 seed），记录里存参数，`/replay` 默认沿用原任务参数。

#### R6. /replay 命令

```
/replay <task-id> [--model <X>] [--sampling ...]
```

= git worktree/checkout 到 `pre_commit` + 从 `fork_point` 开 transcript 新分支
+ 原任务原 prompt 原参数、换模型 X + 干净进程环境跑一遍。

#### R7. /compare 视图

```
/compare <task-A> <task-B>
```

输出三样：**指标表**（wall_time/总token/轮数/工具调用/结果）
+ **两份 transcript 的 diff**（过程差异）
+ **两个 git 区间的 diff**（结果差异）。

对比维度优先级：结果正确性 > 墙钟时间 > 总 token 数 > 轮数/工具调用数 > token/s。
注意 token/s 是模型硬件吞吐指标，总 token 数和轮数是 agent 效率指标，分开看。

### 控制变量与实验纪律（/replay 内置）

| 变量 | 固定方式 |
|---|---|
| 目录状态 | fixture 的 git 快照（worktree） |
| 起点上下文 | 同一 transcript 分叉点 |
| 任务描述 | 原任务 prompt 逐字复用 |
| 采样参数 | temperature=0 或同 seed |
| 工具集 | 同版本 wukong 二进制、同工具配置 |
| **唯一变量** | 模型 |

- 同机不并行：两个 run 交替串行跑（显存/CPU 抢占会污染 token/s 与工具耗时）。
- 每个 (模型×任务) 至少 3 次，取中位数——agent 行为方差大，单次对比无意义。

### 落地顺序

1. R1 任务溯源记录（纯日志，最便宜）
2. R2 pre/post 自动 commit（依赖 R1 的 task_id）
3. R3 transcript 树化 + 任务边界分叉（改动最大的一步）
4. R4 后台进程清理
5. R5/R6 `/replay`（前三条就位后是读记录 + 起新 run，最薄）
6. R7 `/compare`

### 验收标准

- 任意历史任务可用 `/replay` 在干净环境重跑，文件/上下文/进程三现场与当时一致。
- 同一任务换两个模型各跑 3 次，`/compare` 能输出指标表 + transcript diff + git diff。
- 单模型内 `/replay` 与原任务结果一致（确定性采样下）。
