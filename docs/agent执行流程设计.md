# Agent 执行流程设计

> M7 设计文档。解决「AI 模式只能执行简单任务」的问题：把 M4 的"围栏块 × 4 轮"循环重做成完整的 agent 执行流程。参考 `~/research/harness` 下 crush（Go）、codex（Rust）、opencode（TS）、deepseek-harness（TS）、grok-build（Rust）五个项目的 agent 执行流程，结合 rysh 自身定位（薄层、shell 优先、一屏一流、BYO OpenAI 兼容 provider 含本地模型）取舍后落地。
>
> **2026-08-30 现状同步（屏幕模型）**：[主屏方案](./主屏方案.md) 取消了底部状态行，本文所有提到"状态行"的呈现描述（§7.3 的 `等待确认`、§8.1/§9.1 的 `running · step N · n/m`、§11.1 的 `重试中 2/3`、§11.4 呈现表、§16.2/§16.5 的子任务与 ^C 提示）**均已失效，仅作历史记录保留**：这些瞬时状态现在要么落在内容**末行**的就地 spinner（`⠋ 正在思考...` / `⠋ 执行中...`，首个 token 到达即擦除），要么降级为写进滚动流的一条暗色 `noticeLine`；引擎侧的事件面（`OnStatus`）本身没变，只是 `streamSink`/`subSink` 的实现体成了空方法。执行流程、守卫、预算、审批、子任务语义不受影响。
>
> **2026-08-27 现状同步（已审核定案）**：预算管理一期（架构设计 §3.9 M1）已先行落地在现有围栏循环内——删除轮数上限、LoopGuard 死循环防护、触发即收尾（wrapping up）、工具结果 L1 折叠。审核结论：**不设 `max_steps` 步数预算**；**死循环守卫沿用现网参数（2 警 / 6 停 + 占比判定）**，M7.2 引擎化时原样移植（§6.4、§15 决策 5）。相关章节已按定案对齐。同日补充定案：经真实案例（某长会话 137 次上下文压缩后陷入"压缩→遗忘→重读"空转，死循环守卫判据抓不到）新增**读空转守卫**设计（§6.4 注二、§15 决策 11），M7.2 实施。

## 1. 现状与根因

当前实现（2026-08-27，`cmd/rysh/main.go` 的 AI 循环 `streamRound` + `cmd/rysh/loopguard.go`；engine 化见 §4）：

```
提交提示 → 组装上下文（cwd + env + 指令 + 统一时间线 + 历史；
            历史快照先经 L1 折叠，§10.2）
loop（无轮数上限）:
    流式取回复（markdown 渲染；请求错误即终止任务）
    用正则找回复里的 ```bash 围栏块
    无围栏块 → 终答，任务结束
    每块: bash -c 独立执行（30s 超时 / 单流 32KB 截断 / 非交互）
    每次执行记录签名（空白归一化命令 ∖0 exit code ∖0 输出前 8KB 的 sha256）:
      连续相同 ≥2 → 警告附在该结果尾部，随下次请求回喂（不上屏、不落日志）
      连续相同 ≥6，或滑窗 20 条饱和后同签名占比 >0.6 → 触发收尾
    结果作为 system 消息回喂，再次调用模型
收尾轮: 屏幕暗色行 `─── wrapping up (loop guard) ───`（会话日志 noti），
        注入一条用户消息强制纯文字总结；该轮围栏块不再执行
→ 整轮并入 history（≤40 条按整轮裁剪）
```

复杂任务做不了，根因逐条如下（括号内为对应设计章节与里程碑）：

| # | 根因 | 表现 | 修复 |
|---|---|---|---|
| R1 | **围栏块协议**：工具调用靠模型在 markdown 里写 ```bash 块，正则解析 | 弱模型经常不写/写错围栏；一轮只能表达"跑命令"一种动作，无法并行、无结构化参数、无参数校验 | 原生 function calling + 围栏兜底（§5，M7.1） |
| R2 | **4 轮上限**（`maxToolRounds=4`） | 真实任务普遍要 15~50 步（读代码→改→编译→测试→修→再测），4 轮必然中途断掉，正是用户反馈的"执行两个 cat 一个 ls 之后就没有然后了"的深层版本 | **已修复（2026-08-27）**：删除轮数上限，改由 LoopGuard 防发疯（架构设计 §3.9 / ADR 8；§6.4）；审核定案不设步数保险丝（§15 决策 5） |
| R3 | **只有 bash 一种工具** | 改文件只能 heredoc/sed，读文件只能 cat 全文（32KB 截断），找代码只能 grep 猜路径 | 工具集：bash/read/write/edit/glob/grep/ls/todo/job_*（§5.3，M7.2） |
| R4 | **30s 硬超时、无后台** | go test / make / npm install 跑不完；挂住的命令卡死任务 | 默认 60s + 单命令可配 + 超时自动转后台 job（§8，M7.3） |
| R5 | **无权限模型** | 所有命令静默自动执行——要么不敢放心用，要么误伤项目 | ask/always/never 三档审批 + 只读自动放行 + 允许规则（§7，M7.4） |
| R6 | **无任务规划** | 长任务中模型丢失目标与进度 | todo 工具 + 系统提示任务规范（§9，M7.5） |
| R7 | **上下文无 token 概念** | 40 条按条数裁剪、工具结果全量回喂；长任务撞上下文墙后直接失败 | **已修复（2026-08-28）**：L1 字符预算折叠（§10.2）、usage 记账（§10.1，M7.2）、checkpoint 压缩（§10.3 已实现说明，M7.6）全部落地 |
| R8 | **无重试/循环保护** | 一次 429/网络抖动整个任务报废；模型重复同一动作时无约束 | **部分修复（2026-08-27）**：LoopGuard 死循环防护已在围栏循环内实现（§6.4 已实现说明）；重试与孤儿修复未实现（§11.1/§11.2，M7.2） |
| R9 | **系统提示只有一句话** | 模型不知道工具使用纪律（每次 bash 是新 shell、改文件先读后改、大输出用 read 不用 cat） | 结构化系统提示（§9.2，M7.5） |

## 2. 参考项目对比

五个项目收敛出的"完整 agent 流程"的共同骨架：**原生 function calling 驱动 → 工具执行 → 结果回喂 → 循环，外加审批、长命令处理、上下文压缩、循环保护、重试、会话持久化六道配套**。

| 维度 | crush (Go) | codex (Rust) | opencode (TS) | deepseek-harness (TS) | grok-build (Rust) |
|---|---|---|---|---|---|
| 循环 | fantasy 库驱动 step 循环 + StopWhen 条件 | `run_turn` while 循环，模型每步回"函数调用或终答" | `runLoop` while(true) + 任务队列（subtask/compaction） | 插件事件管线 turn/start→step→tool/call→step/end | session actor + stop gate（惰性感/死循环恢复/max-turns） |
| 工具协议 | 原生 function calling | 原生 function calling | AI SDK 原生 function calling（各 provider） | 原生 function calling（另有 code mode：模型经 `run_code` 工具 + 生成 SDK 在程序内调工具） | 原生 function calling |
| 核心工具 | bash/view/edit/multiedit/write/ls/glob/grep/search | shell（tty/workdir/yield_time）+ apply_patch | bash/read/edit/write/glob/grep/apply_patch/todo/question/task | bash（含持久 PTY 变体）/read/write/edit/glob/grep/terminal_*/job_* | bash/read_file/search_replace/write/grep/list_dir/task_output/kill_task/todo/task(子代理) |
| 权限 | 只读 safe 命令自动放行；会话级持久允许（tool+action+path）；yolo | 审批策略（UnlessTrusted/OnRequest/Granular/Never）+ 前缀规则持久化 + 沙箱（seatbelt/landlock） | 每工具 allow/ask/deny 通配规则；bash 按命令解析出前缀规则（`git checkout *`）；拒绝=工具错误回喂 | fail-closed（非 grant 即拒）；preset=沙箱+审批两旋钮（workspace-write / danger-full-access） | Claude 风格规则 DSL（`Bash(git push:*)`）+ auto 模式 LLM 分类器（tree-sitter 切分命令）；yolo/Auto/Ask 三档 |
| 长命令 | 60s 自动转后台；50 job/8h；job_output/job_kill | unified_exec：64 进程、1MiB 输出、yield_time 250ms~30s | shell 后台 | jobs 运行时 + 完成通知以消息注入 | 默认 120s 超时/5m 上限；15s 前台预算超时自动转后台；工具流式进度增量 + monitor/scheduler |
| 上下文 | 自动摘要（阈值=窗口 20% 或 20k）；usage 记账，chars/4 兜底 | 自动压缩任务（turn 前/中溢出/手动/模型可调用）；摘要替换历史 | 压缩保留近期 tail（token 预算），隐藏 agent 总结头部；溢出→压缩→重试，**不重试溢出错误** | 先裁剪工具结果再摘要；pressure/overflow 双触发 | 85% 自动压缩；采样中溢出→压缩重发；工具结果超 50% 窗口裁剪（硬清为占位） |
| 循环保护 | 10 步窗口内 >5 次相同调用签名→停 | — | 同工具同参数 3 次→请求许可 | loop-hygiene 插件 | max-turns + 相同调用"停滞"先提醒后硬停 |
| 重试 | 库内重试；401 刷新凭据重试一次；流失败为未完成工具调用合成错误结果 | 指数退避 + 抖动，StreamError 显示"Reconnecting…" | 2s×2 退避 + 抖动，30s 上限，5 次，尊重 Retry-After；429/5xx/网络 | 同左 | 429/5xx 退避（≤15 次、30s 上限、尊重 Retry-After）；401 重认证重发；413 去图重试 |
| 会话 | SQLite 全量持久化 + 会话选择器 | rollout JSONL + 重放重建历史（含压缩窗口） | SQLite 全量 upsert + fork | JSONL（zstd 校验帧）/SQLite；崩溃补 synthetic `turn/end{interrupted}` | JSONL（chat_history+updates，容忍断行+隔离损坏文件）+ 重放恢复/fork |
| 规划/子代理 | todos 工具 + subagent 工具 | 角色注册表 + 委派子会话（强制 Never 审批）+ Review 任务 | plan 主 agent（禁 edit）+ task 子 agent（深度 1） | subagent 多 provider（进程内/fork/acp/codex/claude-code…）+ plan/todo/goal 工具 | plan mode 状态机（激活期禁写）+ todo + 子代理准入控制 + goal harness + Rhai workflow |

**对 rysh 的取舍**：rysh 是薄层产品（不做多任务、不做云、不引渲染框架），不抄重型基建（SQLite、沙箱、Rhai workflow、MCP、子代理准入），但**六道配套的骨架全部采纳**——它们正是"复杂任务做不了"的对症项。唯一差异化保留：**围栏块协议作为弱模型（本地 llama-server）的兜底**，五个参考项目都没有，这是 rysh "BYO 本地模型"定位的刚需。

## 3. 设计目标 / 非目标

**目标（M7 验收口径）**：

1. 对具备 function calling 的模型（deepseek/GLM/OpenAI/Grok 云端等），agent 能完成 20+ 步任务：读代码 → 规划 → 改文件 → 编译 → 跑测试 → 看失败 → 修 → 复跑，中途不无故中断。
2. 长命令（测试/构建/安装）不卡死：超时自动转后台，模型可查输出、可杀。
3. 审批默认安全（写操作先问，答一次"总是"后同类放行），且不打断只读探索流。
4. 长任务的上下文不撞墙：工具结果裁剪 + 压缩自动生效。
5. 对不支持/不可靠 function calling 的本地模型，围栏协议继续可用（能力降级不降级可用性）。

**非目标**：沙箱（用户 shell 即信任边界，后续评估）、MCP、子任务嵌套（深度 1，§16.2）、多任务/多会话并发、云同步、会话 /resume（列入二期，§12）。

## 4. 总体架构

```
                        ┌────────────────────────────────────────────┐
                        │              cmd/rysh/main.go              │
                        │  驱动循环：按键路由 / 模式切换 / 屏幕渲染     │
                        └──────┬─────────────────────────┬───────────┘
                     按键/取消/审批应答              事件→屏幕行+会话日志
                        └──────▼─────────────────────────▼───────────┘
                        ┌────────────────────────────────────────────┐
                        │              internal/agent                │
                        │                                            │
                        │  engine.go   Task 循环（§6）                │
                        │    │  消费   ▼            ▲ 产出            │
                        │  provider  ◄├─ 流式请求 ──┤  事件流 Sink     │
                        │  (tools)      │            │                │
                        │    │         ▼            │                │
                        │  policy.go 审批门（§7）    │                │
                        │    │         ▼            │                │
                        │  tools/    工具注册表（§5） │                │
                        │   bash read write edit glob grep ls todo   │
                        │   job_output job_kill                │     │
                        │    │         ▼                                │
                        │  exec.go + jobs.go 执行器/后台 job（§8）      │
                        │                                            │
                        │  context.go 上下文组装/预算/裁剪（§10）       │
                        │  compact.go 压缩（M7.6）                    │
                        │  fence.go  围栏协议兜底（保留 M4 实现）        │
                        └────────────────────────────────────────────┘
```

关键接缝：**engine 与 TUI 彻底解耦**。`main.go` 只实现事件 Sink（事件→屏幕行 + 会话日志）和审批应答入口；engine 不 import screen/aiui，用 mock provider + 假 Sink 即可单测整个循环（延续现有 httptest mock SSE 的测试路线）。

消息模型（`internal/provider` 扩展）：

```go
type Message struct {
    Role       string     // system | user | assistant | tool
    Content    string
    ToolCalls  []ToolCall // 仅 assistant：本轮请求的工具调用
    ToolCallID string     // 仅 tool：对应哪个调用
    Name       string     // 仅 tool（部分 provider 需要）
}
type ToolCall struct {
    ID     string // provider 生成的调用 id
    Name   string
    Args   string // JSON 字符串（流式拼接完成）
}
```

现有 `ctxMsg`（Message+时间戳）改为携带完整 `Message`；统一时间线（shell 事件 + AI 历史按时间混排）不变，仍是 rysh 的差异化上下文。

## 5. 工具系统

### 5.1 协议：原生 function calling 为主，围栏兜底

- 请求体加 `tools: [{type:"function", function:{name, description, parameters}}]`；流式响应解析 `delta.tool_calls[]`（`index` 分道、`id`/`name` 首片、`arguments` 增量拼接）；带 `stream_options:{include_usage:true}` 取最终 usage（记账用，§10.1）。
- 模型配置加 `tools = true|false`（缺省 true）。`tools = false` 或该 provider 返回"不支持 tools"的 400 时，走**围栏协议**：engine 对流式完成的回复跑 `FindShellBlocks`，把每个围栏块**合成为 bash 工具调用**，进入与原生协议完全相同的执行/回喂路径——引擎只有一条循环，两种"调用来源"。
- 弱模型韧性（本地模型现实）：`arguments` 非法 JSON → 不中止任务，回喂"[工具 bash 参数解析失败: …，请重新以合法 JSON 输出]"（crush 的 sanitizeToolInput 同款）；`name` 不在注册表 → 回喂"[未知工具]"。

### 5.2 工具接口

```go
type Tool struct {
    Name        string
    Description string   // 进入 tools 描述（含使用纪律）
    Schema      map[string]any // JSON Schema
    ReadOnly    bool     // 权限分类用（§7）
    Execute     func(ctx context.Context, task *Task, args map[string]any) (Result, error)
}
type Result struct {
    Output string   // 回喂模型的文本（已按工具限额截断）
    Meta   string   // 屏幕摘要行，如 "exit 0 · 1.2s · 45 行"
    Err    error    // 工具级错误 → 作为工具结果回喂（不杀任务）
}
```

### 5.3 v1 工具清单

| 工具 | 参数 | 行为 | 输出限额 |
|---|---|---|---|
| `bash` | `command`（必填）、`workdir`?、`timeout`?（秒，≤ `bash_max_timeout`）、`run_in_background`? | `bash -c` 独立执行器（沿用 M4 `exec.go`：进程组、cwd、超时）；**每次全新 shell，状态不继承**（目录靠 workdir 传递，写进工具描述） | 64KB（头 48KB + 尾 16KB，中间 `[… 省略 N 字节 …]`） |
| `read` | `path`、`offset`?（行，1 起）、`limit`?（行） | 读文件，**带行号**（`   10→content`，便于 edit 定位） | 缺省 200 行，上限 2000 行 / 50KB |
| `write` | `path`、`content` | 创建/整文件覆盖；父目录不存在则报错（不自动 mkdir） | — |
| `edit` | `path`、`old_string`、`new_string`、`replace_all`? | **精确唯一匹配**替换（old_string 在文件中必须恰好出现一次，除非 replace_all；多匹配/零匹配都报错回喂，让模型重读后重试） | — |
| `glob` | `pattern`、`path`?（目录） | 文件枚举（`**/*.go` 风格），尊重 .gitignore | 1000 个文件 |
| `grep` | `pattern`（正则）、`path`?（文件/目录）、`glob`? | 优先 shell 出 `rg --line-number`，无 rg 走内置扫描；输出 `path:line:内容` 风格 | 200 行 / 50KB |
| `ls` | `path`?、`depth`?（默认 2，≤4） | 目录树概览（含大小/目录标记） | 500 行 |
| `todo` | `todos: [{id, content, status}]` | 全量替换任务清单（§9.1）；status ∈ pending/in_progress/completed | — |
| `job_output` | `job_id`、`tail_lines`? | 取后台 job 状态 + 输出（§8） | 32KB（默认尾 200 行） |
| `job_kill` | `job_id` | 杀后台 job（进程组） | — |
| `task`（仅管理者，M7.7） | `description`、`prompt` | 派遣工人子任务（独立上下文的 engine run，§16）；并发 ≤3 | 状态行 |
| `task_output`（仅管理者，M7.7） | `id` | 查子任务紧凑状态/取有界报告（监控+验收通道，§16.3） | 报告 8KB |
| `task_kill`（仅管理者，M7.7） | `id` | 停掉发疯的子任务（cancel ctx，§16.3） | — |

（`ask_user_question`、plan mode、`skill` 列二期，§12；子智能体已升为 M7.7 一等能力，见 §16。）

### 5.4 结果回喂格式

工具结果以 `role: tool` 消息回喂（带 `tool_call_id`），统一前缀元数据：

```
[edit] /path/to/file.go: 替换 1 处（L42-L58）
[bash] exit 0 · 1.2s · cwd /home/u/proj
<输出>
```

错误也走工具结果（模型可见、可自纠）：`[bash] exit 2 · stderr 已附上`。屏幕上的观察行与 M4 风格一致（`[tool] $ make test` → `  ↳ exit 0 · 1.2s · 45 行`），§11.3。

## 6. 核心循环

### 6.1 状态机

```
                ┌──────────────────────────────────────────────┐
                │                                              │
提交提示 ──► 每步(step):                                        │
   │          ┌─ 组装请求 = 前缀 + 统一时间线 + history + 本任务消息
   │          ├─ 流式请求 ──► 事件: 思考/文本增量（屏幕实时渲染）
   │          ├─ 得到: 文本回复 (+ N 个工具调用)
   │          ├─ 无工具调用 ──► 终答，任务结束 ──────────────────┤
   │          └─ 有工具调用:                                    │
   │               for 每个调用（v1 顺序执行）:                  │
   │                 审批门（§7）── 拒绝 ──► 回喂"[用户拒绝]"，继续│
   │                 执行（§5/§8）──► 事件: 工具开始/结束         │
   │                 结果入消息                                 │
   │          追加 (assistant 含 tool_calls) + (tool 结果)       │
   │          步数 +1；检查停止条件 ──► 未触发则回到每步开头        │
   └───────────┴──────────────────────────────────────────────┘
```

### 6.2 engine 接口（Go）

```go
func (e *Engine) Run(ctx context.Context, in Input, sink Sink) Result

type Input struct {
    Client   provider.StreamingClient // 扩展后的 ChatStream（支持 tools）
    Tools    []Tool                   // 按模型 tools 开关解析后的注册表
    Base     []Message                // 前缀(cwd/env/指令) + 统一时间线 + history
    Prompt   string                   // 用户提交文本（成为首条 user 消息）
    Policy   *Policy                  // 审批策略快照
    Cfg      Config                   // 预算/限额（§13）
}

type Sink interface {          // main.go 实现：屏幕 + 会话日志
    OnText(delta string, reasoning bool)
    OnToolBegin(call ToolCall)
    OnToolEnd(call ToolCall, res Result)
    OnApproval(call ToolCall) (Decision, error) // 阻塞等用户 y/n/a
    OnTodo(todos []Todo)
    OnStep(n int)
    OnCompact(summary string)
}
```

### 6.3 工具执行细则

- v1 顺序执行（每步内多个调用按序跑）；二期：只读工具（read/grep/glob/ls/job_output）批内并发 4。
- 工具执行不持有 writeMu（沿用 M4 原则：慢命令不卡驱动）；屏幕行经 Sink 在锁内写出。
- 单工具硬超时（`bash` 见 §8；grep/glob 缺省 30s/60s）；超时 = 工具错误回喂，不杀任务。

### 6.4 停止条件（任一触发即停）

| 条件 | 行为 | 参考 |
|---|---|---|
| 模型终答（无工具调用） | 正常结束 | 全部 |
| 用户 ^C | 取消（§8.3 两级语义） | 全部 |
| 死循环守卫（LoopGuard，2026-08-27 围栏循环内已实现并定案，细则见下注） | 同一签名连续相同 ≥2 次 → 警告随下次请求回喂；连续相同 ≥6 次，或滑窗 20 条饱和后同签名占比 >0.6 → 收尾轮（wrapping up）强制纯文字总结后结束，不报错 | opencode 3 次问许可、crush 10 步窗口 5 次相同停 |
| 读空转守卫（2026-08-27 案例新增，M7.2 实施，细则见下注二） | 同一读签名滑窗内第 3 次出现 → 警告回喂；写类操作持续为零且读占比过高 → 先注入「停下重整」提醒，仍无改观 → 收尾轮（原因 `read churn`） | 案例驱动（grok-build 真实会话，见注二） |
| 上下文溢出 | provider 返回 context-overflow 类错误 → 走压缩后重试（§10.3），**不做普通重试** | opencode 明确不重试溢出 |
| 审批被拒 | 不直接停：回喂"[用户拒绝了该操作]"，模型自行改道（连续拒绝同一操作则触发死循环守卫） | opencode RejectedError |

> **死循环守卫定案（2026-08-27 已实现并审核通过，`cmd/rysh/loopguard.go` + `main.go`）**：签名 = sha256(空白归一化命令 ∖0 exit code ∖0 输出前 8KB)，滑窗 20 条；**连续相同 ≥2 次** → 警告文本附在结果尾部随下次请求回喂，不上屏、不落日志；**连续相同 ≥6 次**或**窗口饱和后同签名占比 >0.6** → 触发收尾轮：屏幕 `─── wrapping up (loop guard) ───` + 会话日志 `noti`，注入一条强制纯文字总结的用户消息，该轮围栏块丢弃不执行。占比判定针对连续计数抓不到的 `A A A B` 交织型循环，且仅在窗口饱和（=20 条）后计算，避免任务前几步小窗口占比恒偏高而误杀。触发时不报错：由收尾轮产出「已完成/未完成/下一步」结构化总结，history 完整保留。M7.2 引擎化时原样移植这套参数（16 项单测钉死语义）。

> **读空转守卫（2026-08-27 案例新增，M7.2 实施；一期未实现）**：案例——grok-build TUI 一个 deepseek-v4-flash 会话连跑 9 小时：上下文压缩 137 次，陷入"压缩→遗忘→重读"循环，170 分钟 1025 次工具调用仅 57 次写操作，同一文件重读 20+ 遍（连自己维护的 plan.md 都读了 26 遍），最后 2 小时零文件产出。死循环守卫抓不住它：调用彼此不同（读 A、grep B、再读 A……），是**高频读 + 零写 + 同目标反复**的交织型空转，落在"连续相同 / 同签名占比"两个判据的盲区外。判据（共享 LoopGuard 滑窗；参数 M7.2 随配置面定）：① **同目标重复读**——读类动作（read/grep/glob/ls，及命中 §7.1 安全分类器的 bash 命令）按"工具 + 归一化参数"记读签名，同一读签名滑窗内第 3 次出现（不要求连续）→ 警告回喂不上屏：「同一内容已第 N 次读取，结果应仍在上下文中；若已丢失，请基于现有信息推进或先更新 todo」；② **产出停滞**——写类操作（write/edit，及未命中安全分类器的 bash）连续 30 步或 15 分钟为零、且读占比 >0.7 → 注入一次"停下重整"消息（要求先输出「已掌握 / 待办 / 下一步」再继续，对弱模型等于一次免费自我重整）；之后仍零写 → 收尾轮，原因记 `read churn`。误杀防护：一级反应是提醒而非停——大型重构前正当的长探索被打断的代价只是多写一次规划，无害且常有益；只有提醒后仍空转才收尾。与 §10.3 的关系：读空转的主因是压缩抹掉工作记忆（本案例即 137 次压缩所致），压缩摘要骨架（含已完成/改动文件/下一步）是对症第一道，本守卫是兜底第二道。

## 7. 权限与审批

### 7.1 风险分类

- **只读自动放行**（免审批）：read/glob/grep/ls/job_output 恒放行；`bash` 命中**安全命令分类器**放行。
- **需审批**（ask 模式下）：write/edit 每次询问；bash 未命中安全分类器询问。
- 安全命令分类器（crush `safe.go` 同款思路）：已知只读前缀白名单（`ls` `cat` `head` `tail` `less` `grep` `rg` `find` `pwd` `echo` `which` `file` `stat` `du` `df` `ps` `git status` `git diff` `git log` `git show` `git branch` `go list` `go env` …）且整条命令**不含** shell 元字符（`;` `|` `&` `$` `` ` `` `(` `)` `>` `<` `{` `}`）——一旦含元字符/重定向即视为写/组合操作走审批。白名单可配增补。

### 7.2 审批模式与规则

- `[agent] approval` 三值：`"ask"`（缺省，rysh 跑在用户真实项目目录里，写操作默认先问）/ `"always"`（全自动零打扰，等价 M4 现状）/ `"never"`（对所有需询问操作一律拒绝、不弹条，拒绝文本回喂模型自行改道，只读操作仍放行）。
- 会话级覆盖：启动标志 `--always-approve` / `--never-approve`（对该实例整个生命周期优先于配置），或 AI 模式 `/approve`——无参数查询当前模式，`/approve ask|always|never` 设置（下个任务起生效）。
- 审批 UI（§11.3）：`y`=本次放行，`n`=拒绝，`a`=总是放行（会话级持久规则）。
- 持久规则（会话内内存，二期落配置）：
  - bash → **前缀规则**（`git *`、`make *`；opencode 用 tree-sitter 解析命令取"命令词"做前缀，rysh 简化为取首个非选项 token 序列，够用且无依赖）；
  - 文件工具 → 按工具（`edit` 一次"总是"后本任务不再问文件编辑——写密集任务的体验关键）。
- `/allow <规则>`、`/permission` 斜杠命令查看/增删（二期；v1 只读展示 + y/n/a 即时规则）。

### 7.3 审批与流式锁的协同

M4 任务流式期间锁定输入（仅 ^C）。新增：**存在待审批时，流式锁放行 y/n/a 三键**（`aiui.HandleKey` 的 streaming 分支加一个 `pendingApproval` 钩子）。审批等待不占用流式状态行，状态行显示 `等待确认`。（主屏方案后无状态行：审批提示本身即整行琥珀色高亮条，直接进滚动流。**2026-08-31 修订**：提示必须独占一行——正文末行光标停在句中、或 spinner 仍占着待输出行时，先擦除 spinner 或换行再绘制 `screen.ApprovalBar`，绝不内联在正文后面。）

> **M7.4 审批落地注（2026-08-28）**：门控在 `internal/agent/policy.go`——`Approval.Gate(ctx, tool, args)` 返回 ""=放行、非 ""=拒绝文本（`denyText`，"[用户拒绝了该操作] …" 回喂代替工具结果）。放行面：bash/write/edit 之外的注册表工具全部直通（读家族、todo、job_output/job_kill——比 §7.1 列举的读家族宽，但均为无副作用或任务管理工具）；bash 命中安全分类器（复用 guard.go 的 `bashLooksReadOnly` + `safeShape` 元字符/禁词检查）；auto 模式。其余一律 ask；**ask 回调为 nil 时 fail-closed 直接拒绝**。"a" 记会话级规则：bash 按前缀（`bashPrefix` 取首个非选项 token，git/go/docker/npm 等双词头扩到第二个非选项 token；双词头后仅剩选项时返回 ""，防裸 `git` 铸出"放行所有 git"；`git -C path` 类带参选项会使规则收窄但绝不更宽），文件工具按工具名。Gate 内一次加锁快照（mode/增补词/既有规则/ask 回调）后即解锁再判定，ask 阻塞不持锁。^C 在待审批时按流式阶段处理：当前调用答 deny + 整任务取消（工具尚未启动，两级中断不适用）。

## 8. 长命令与后台 job

### 8.1 执行参数

| 参数 | 缺省 | 说明 |
|---|---|---|
| `bash_timeout`（配置） | 60s | 单命令缺省超时（M4 为 30s 硬编码） |
| `bash_max_timeout`（配置） | 600s | 模型可经 `timeout` 参数上调的上限 |
| `auto_background_after`（配置） | 60s（0=禁用） | 前台命令跑满该时长自动转后台 job，结果回喂"已转后台（job 3），用 job_output 查看" |

### 8.2 job 管理器（`internal/agent/jobs.go`）

- 进程组执行（沿用 `exec.go` 的 `configureProcessGroup`），上限 50 个（超了杀最老的空闲 job），输出环形缓冲 256KB（头 128KB + 尾 128KB），记录状态（running/done/exit code/耗时）。
- `job_output` 回喂：状态行 + 输出（默认尾 200 行，可配）；running 中返回"仍在运行 + 当前尾部"，模型可隔几步再查（替代"等命令跑完"的阻塞语义）。
- `job_kill` 杀进程组。
- 完成通知注入（二期）：job 结束以 `[job 3] make test 结束（exit 0）` 系统消息注入当前任务（deepseek-harness 的完成通知模式）；v1 靠模型在结果提示下主动查。
- job 生命周期随 rysh 进程；退出时 `forwardTermSignals` 一并清掉（进程组已隔离）。

### 8.3 ^C 三级语义

M4：^C 取消整个任务。M7：

- 模型**流式阶段**（thinking/writing）按 ^C → 取消任务（同 M4）。
- 工具**执行阶段**按 ^C → 只中断当前前台命令（杀其进程组），结果回喂"[被用户中断]"，**任务继续**，模型自行决定下一步（长命令下用户想"跳过这一步"而非报废整个任务的刚需）。
- **子任务运行中**按 ^C（M7.7，§16.5）→ 先停掉全部运行中的子任务，管理者带着"被终止"的报告续跑；再按一次才终止任务。

驱动已知任务阶段（`aiPhase` 扩展 exec 态），实现点在 main.go 的 `cancelAndWait` 拆分；子任务级在 main.go ^C 处理链首位判断 `subMgr.KillRunning()`。

## 9. 任务规划与系统提示

### 9.1 todo 工具

- 模型用 `todo` 维护任务清单（全量替换语义，grok-build `todo_write` 同款）；工具描述写明纪律：**3 步以上的任务先列 todo 再动手；每完成一项立即更新状态；开始新项前确认上一项 completed**。
- 屏幕：todo 变化时输出紧凑单行 `[todo] 2/5 · 当前: 实现 parser`（append-only 流内不重绘，二期评估原地重绘块）；状态行 running 态追加 `· 2/5`。
- 会话日志：`todo` 记录（完整清单快照），供回放。

### 9.2 系统提示（替换 M4 一句话 Instructions）

结构化模板（Go 模板渲染，静态文本 + 环境注入）：

```
你是 ruyishell 的 agent，运行在用户的终端中。用户是开发者，你替他操作本机。

环境
- OS: linux · shell: bash · 当前目录: /home/u/proj
- 工具: bash, read, write, edit, glob, grep, ls, todo, job_output, job_kill

工具纪律
- bash 每次都是全新 shell：cd/export 不跨命令保留，需要目录就传 workdir。
- 看文件用 read（带行号），找内容用 grep/glob，不要用 cat/ls -R。
- 改文件：先 read 相关片段，再用 edit 精确替换（old_string 必须唯一）；
  整文件重写才用 write。
- 命令输出很长时，让命令自己过滤（grep/head），不要全量打印。
- 多步任务（≥3 步）：先 todo 规划，逐项推进，完成即更新。
- 长命令（构建/测试/安装）：预期超过 1 分钟就用 run_in_background，
  随后 job_output 查看。

安全
- 删除、覆盖无关文件、git push --force、rm -rf 等破坏性操作，
  执行前必须征得用户同意（审批机制会拦截；即使自动放行也应先说明）。

输出
- 跟随用户的语言（用户说中文就用中文）。
- 过程简洁，终答用 markdown 结构化；不要复述命令输出全文。
```

二期：AGENTS.md/CLAUDE.md 上下文发现（仓库根 + 用户级 + 子目录分层）与 skill 发现/`skill` 工具（目录约定与注入策略见 §12 二期细化；crush `context_paths` / codex world-state 同款思路，带字节预算）。

## 10. 上下文管理

### 10.1 token 记账

- 优先用 provider `usage`（`stream_options.include_usage` 最终 chunk 的 `prompt_tokens`）；provider 不支持时按 `chars/4` 估算（crush/opencode 同款兜底）。
- 模型配置已有 `context_window` / `max_tokens` 字段——M7 起真正使用：软上限 = `context_window × context_soft_limit`（缺省 0.85，与 grok-build 的 85% 自动压缩阈值一致）。

### 10.2 每请求组装（在 M4 统一时间线之上叠加）

```
[系统前缀: 环境 + 系统提示(§9.2)]          ← 固定
[统一时间线: shell 事件(8KB 预算) 与 history 混排]  ← M4 不变
[本任务消息: user 提示 → assistant/tool 交替]
```

工具结果裁剪（M7.2，廉价先行）：本任务消息中，**最近 8 条工具结果保留全量**，更早的结果超 4KB 的替换为 `[结果已裁剪: <tool> <参数摘要>，原 N 字节]`——保住近期细节、释放头部空间（opencode prune / deepseek tool-result-pruner 同款）。

> **已实现说明（2026-08-27，先行版 `collapseOldTools`）**：落地时把"最近 8 条 / 4KB"改为**字符累计预算**（`l1PreserveBytes = 64KB`）：本任务消息从最新往旧累计工具结果原文，预算内保留原样；一旦累计溢出，其后更旧的工具结果折叠为一行占位 `[已省略] $ <命令> (exit N)，原始输出 N KB 已折叠；如需详情可重新运行该命令`。恰好装满不折叠、无溢出一条不动、最新超大结果永不折叠、非工具消息不动；turn/history 本体不修改，只折叠请求视图（跨任务历史并入请求时同样先折叠）。"最近 N 条全量"语义由预算自然给出（装得下即全量）；usage 记账（§10.1）落地前以字符预算兜底，M7.2 引擎化时评估是否叠加条数阈值。

### 10.3 压缩（M7.6，压缩 = 状态交接；2026-08-28 定案并实施，取代原"最简稳健版"）

> **已实现说明（2026-08-28，`internal/agent/compact.go`）**：常量硬编码——触发 85%（`compactSoftPct`）/ 回落 50%（`compactResumePct`）/ 尾部保留 ≤25%（`compactTailPct`，bytes≈token×4）/ 退化阈值 500 字符（`minSummaryRunes`）；未配 `context_window` 按 144k 估算（惯例同 §16.4）。触发点两处——轮界 `compactCheck`（req 组装后、与自我警觉同一检查点）与 provider 溢出路径 `compactForOverflow`（压缩→重试一次→仍溢出 fatal，错误类型见 §11.1）。摘要成功后 `RunResult.Compacted{Checkpoint, Cut}` 只交投影结果，history 本体不动（不变式①），驱动层据此收缩内存历史；磁盘 `messages.log` 仅追加 `compact` 事件（checkpoint 全文，回放可还原）。对不变式④的一处落地偏差：摘要失败/退化撤防后**不在下一轮界自动重试**——armed 保留 + 度量仍 hot 会让每个轮界都重发摘要请求（风暴），故失败即撤防触发门，本轮降级仅 L1 折叠续跑，门照常在回落 <50% 后重武装。

> 定案依据：pi / codex / opencode / grok-build / dsh 五家 harness 的源码级对比（[上下文管理对比分析](./上下文管理对比分析.md)）。核心结论——**压缩不是"把历史弄短"，而是状态交接**：把目标、进度、决策、已知文件、存活状态无损移交给一个没参与过对话的模型。把压缩当"截断+摘要"的都会掉进"上下文变重 → 压缩 → 忘了要干嘛 → 重读文件重新定位 → 又变重"的循环（真实案例：9 小时任务、137 次压缩后失焦空转）。原案的五步流程与架构设计 §3.9② 的 75% 软线由本节取代。

**四条不变式**：

1. **磁盘与 history 本体永不修改**——压缩只改请求视图投影（L1 折叠同款原则）：engine 记住 checkpoint 与切点，切点之前投影为 checkpoint，之后逐字保留（pi "context never reads past a compaction"）。
2. **切点只在整轮边界、永不拆 tool call/result 对**（五家共识，tool-pair-safe；grok-build select 同款）。
3. **摘要管"过去"，机械快照管"现在"**——文件清单、todo、存活 job/子任务由 harness 渲染进 checkpoint 附件，不靠摘要模型回忆（回忆会漏、会编）。
4. **压缩永不杀死任务**：摘要失败或退化 → 撤防触发门、本轮降级为仅 L1 折叠继续跑；门照常在回落 <50% 后重武装（不在每轮界硬重试，防退化摘要器请求风暴，见本节已实现说明）。

**触发**（轮界，与自我警觉 §16.4 同一检查点、req 组装后评估）：

- 请求 token 估算 > 窗口 × **85%**（grok-build/dsh 同款；未配 `context_window` 按 144k 估算兜底，惯例同 §16.4）；
- 或 provider 返回 context-overflow：先压缩再重试一次，仍溢出才 fatal（原案保留）；
- 与自我警觉的分层：50%/75% 两级提示是"劝"（模型自敛、优先委派，L2 文案随实施微调为"准备收尾或委派"），85% 是"动手"（机制接管）；迟滞——压缩后回落到 ~50% 以下才可能再次触发，防每轮反复压缩（opencode 同款）。

**checkpoint 组成**（一次摘要调用 + 两段机械渲染）：

- **摘要调用**：当前模型、`tools` 关闭、复用系统前缀（省一次全量 prefill，dsh KV 对齐思路）；输入 = 系统前缀 + 被压缩段（先过 L1 折叠，摘要请求自身不撞墙，原案第 5 条保留）。提示词用交接视角（"为接手模型写交接摘要，它只看得到原始提问与本摘要"，codex/grok-build 同款），骨架七段：**目标**（逐字引用用户关键约束）/ 约束与偏好 / 进度（已完成·进行中·阻塞）/ 关键决策 / 下一步 / 关键上下文 / 备注。
- **迭代更新**：二次压缩把旧 checkpoint 全文一并交给摘要器，附迭代规则"保留目标、进行中→已完成、丢弃已失效信息"（pi/opencode/dsh 共识）——不重新摘要原始历史，摘要链不丢早期信息。
- **机械段（harness 渲染，不进摘要调用）**：① 已读文件 / 已改文件——engine 级累积集合，read/edit/write 工具执行时追加，二次压缩合并旧清单（pi extractFileOperations 同款）；② 存活状态快照 `RenderStateSnapshot(todo, jobs, subagents)`——todo 清单（`[pending]/[in_progress]/…` 标签，有未完成项才注入）+ 运行中后台 job（id/命令/状态）+ 运行中子任务（id/描述/已跑秒数）——grok-build reminder 独有设计，正面治"压缩后不知道还有什么在跑"。

**压缩后请求视图**（grok-build assemble 顺序的简化）：

```
[系统前缀: env + 系统提示]
[mission: 第一条用户消息原文]     ← 逐字保留，不靠摘要转述（抗目标漂移）
[checkpoint: user 消息]           ← 前缀「以下 checkpoint 概括了之前的对话，直接继续任务，不要复述」（dsh）；
                                    正文 = 摘要七段 + 机械段（文件清单 + 存活状态快照）
[切点之后的整轮…]                 ← 逐字（尾部保留：按 ≤25% 窗口从新往旧收集完整回合）
[当前任务进行中的消息…]
```

不照搬 codex"全部 user message 保留"（mission 锚点 + checkpoint 目标段已覆盖核心诉求，全量保留信噪比低）。

**质量护栏与呈现**：摘要清洗后 <500 字符视为退化（grok-build `MIN_SUMMARY_SEED_CHARS` 同款），重试 1 次；仍失败 → 本轮不压缩（仅 L1）+ 撤防触发门 + `OnNotice` 提示。屏幕压缩期间 `─── compacting ───` 暗色单行，完成后 `[已压缩上下文: 保留近 N 轮]`；会话日志记 `compact` 事件（含 checkpoint 全文，回放可还原）。

**验收**（对照"9 小时循环"四环节回归）：

- 引擎级单测：触发且迟滞内不重复触发、切点 tool-pair-safe、history/磁盘不变、二次压缩迭代合并、mission 逐字保留、溢出→压缩→重试→仍溢出 fatal、退化摘要降级续跑、机械段渲染（todo/job/子任务/文件清单，含跨代合并）。
- 长任务回归指标（研究文档 §8.3）：压缩后不重读已读文件（read 调用去重率）、下一动作与 todo 一致（不重述目标）、循环不复发（压缩次数与总时长）。沿用 §12 M7.6 验收手法：mock 小窗口模型（`context_window=4096`）驱动长任务触发压缩后继续完成。

> **验收落地（2026-08-28）**：引擎级单测 7 项（`compact_test.go`：触发/投影与不变式、迟滞与迭代合并、退化降级与撤防、溢出压缩一次/两次 fatal、文件清单机械段、chooseCut 整轮边界、状态快照）+ provider 溢出分类 3 项（`toolcall_test.go`）+ 集成测试 `TestAgentContextCompaction`（真实 rysh + PTY + mock SSE，`context_window=4096`：软触发→摘要请求（无 tools 字段）→投影（mission+checkpoint+近轮、旧内容不泄漏）→跨任务历史收缩→屏幕压缩横幅→磁盘仅追加 compact 事件且历史本体不动）全绿。长任务行为回归指标（read 去重率、下一动作与 todo 一致、循环不复发）是真实模型行为观察项，脚本化 mock 不可断言，留待真实使用观察。

**范围与配置**：M7.6 = checkpoint 压缩 + 压缩后重注入 + 文件清单累积（研究文档 P0+P1+P2）；P3 大输出 spill（`~/.rysh/spill/` + 路径引用）与 P4 计量统一列二期（§12）。`context_soft_limit` 等键随 `[agent]` 面二期开放，M7.6 先硬编码常量：触发 0.85 / 回落目标 0.50 / 尾部保留 ≤25% 窗口。

## 11. 健壮性

### 11.1 provider 重试

- 可重试：429 / 5xx / 网络错误（连接失败、读超时、SSE 断流）。策略：指数退避 2s×2 + ±25% 抖动，单步 30s 上限，最多 3 次，尊重 `Retry-After`（opencode 同款参数）。
- 不可重试：context-overflow（走 §10.3）、401/403（直接报错，提示检查密钥）。
- 重试期间屏幕状态行显示 `重试中 2/3`；流中断已渲染的半成品回复保留（标注 `[响应中断，重试中]`，crush OnRetry 语义的简化：不擦屏，append-only 流内自然保留）。

### 11.2 对话有效性修复

- 取消/出错发生在"assistant 已含 tool_calls、结果尚未齐"时，为每个未决 tool_call 补一条 `role: tool` 结果 `[中断: 未执行]`，保证 history 对下次请求合法（crush 孤儿修复同款）。
- 任务结束并入 history 时整体校验：连续两条 assistant、缺失 tool 结果等异常做同样修复（防御弱模型输出畸形）。

### 11.3 屏幕与日志映射（一屏一流不变）

任务是一段直接落在滚动流上的连续流式块（无头行——`─── <model> ───` 回复块头已于 2026-08-30 移除，当前模型名由提示行 `cwd · model · [AI]:` 承载），事件→行：

| 事件 | 屏幕行（暗色/样式） | 会话日志记录 |
|---|---|---|
| 文本/推理增量 | 同 M4（markdown 渲染 / 暗色） | `asw` / `rea` |
| 工具开始 | `[tool] $ make test`（bash）/ `[tool] read src/x.go 10-40`（文件类） | `tool_call`（name+args） |
| 工具结束 | `  ↳ exit 0 · 1.2s · 45 行`（快速命令与开始行合并为 M4 单行；长命令/后台用两行） | `tool_result`（ok/exit/bytes/ms） |
| 审批 | 独占一行的琥珀色高亮条 `? 运行 git push --force …（y 是 / n 否 / a 总是）`（整行铺底至右边界，不内联在正文后） | `approval`（asked/decided） |
| todo 变化 | `[todo] 2/5 · 当前: 实现 parser` | `todo`（快照） |
| 后台化 | `[job 3] make test 转后台` | `job`（id/status） |
| 压缩 | `[已压缩上下文: 保留近 6 轮]` | `compact` |
| 循环警告 / 收尾 | 警告不上屏（只随请求回喂）；收尾 `─── wrapping up (<原因>) ───`，原因 `loop guard`（已实现 2026-08-27）/ `read churn`（M7.2，§6.4 注二） | `noti`（wrapping up (<原因>)） |
| 步数 | 状态行 `running · step 12 · 2/5`（无步数上限，只显示当前步） | — |

现有日志类型（shl/shk/usr/rea/asw/tool/noti/sys）全部保留，`tool` 记录扩展为带 name/args 的结构，新增 `tool_result`/`approval`/`todo`/`job`/`compact`。

## 12. 里程碑计划

> 排序原则：R1~R4（M7.1~M7.3）是"复杂任务做不了"的直接阻断项，先做；M7.4~M7.6 是信任与长任务保障；二期另行排期。每个里程碑独立可发布（延续 M1~M6 惯例）。
>
> **进度（2026-08-27）**：预算管理一期（架构设计 §3.9 M1）已先行落地在现有围栏循环（未等 M7.1/M7.2）：删除轮数上限、LoopGuard（§6.4 已实现说明）、收尾轮、L1 折叠（§10.2 已实现说明），配 16 项单测 + 2 条 mock SSE 集成测试（`TestAgentToolLoopGuard` / `TestAgentToolCollapse`）。M7.2 engine 化时把这部分从 main.go 迁入 `internal/agent` 并与原生工具调用合流，死循环守卫按 §6.4 定案原样移植。

### M7.1 provider 原生工具调用

- [x] `ChatMessage` 扩展（ToolCalls/ToolCallID/role=tool）；`chatRequest` 加 `tools`/`stream_options`。
- [x] SSE 解析 `delta.tool_calls[]`（index 分道、arguments 增量拼接）与最终 usage。
- [x] 模型配置 `tools` 开关；tools 400 不支持 → 自动降级围栏模式并提示。（降级判定已在 provider 层就绪：400 且报文含 "tool" → 类型化 `*ToolsUnsupportedError`；降级后回退围栏与用户提示的接线随 M7.2 engine。）
- [x] 围栏协议保留为兜底（`FindShellBlocks` → 合成 bash 调用）。（✅ 2026-08-27 随 M7.2 engine 落地：engine 每轮先执行原生 `tool_calls`，剩余围栏块按序合成 bash 调用，ID `fence_N`；`NativeTools=false` 时不广播 tools、纯围栏驱动，行为同 M4。）
- 验收：mock SSE 多工具流式响应被正确解析成调用序列；`tools=false` 时走围栏路径行为同 M4。（✅ 2026-08-27 达成：7 个 provider 单测覆盖多工具分片流解析/请求体线型/400 识别/消息协议往返/装配器乱序；`tools=false` 与不带选项请求体逐字节同 M4。）

### M7.2 引擎 + 工具 v1（核心）

- [x] `internal/agent` 重构：engine（§6 循环 + 停止条件 + 重试 + 死循环守卫（原样移植 LoopGuard，§6.4 定案）+ 孤儿修复）、tools 注册表、result 截断。（✅ 2026-08-27：engine 与 TUI 解耦，Sink 七方法接缝；LoopGuard 参数原样移植（窗 20/2 警/6 停/占比 0.6 仅饱和窗计），配读空转守卫（§6.4 注二）；重试指数退避 ±25% 抖动、Retry-After 优先、attempt 上限 `RetryAttempts`（缺省 3），流中断已输出则续收、未输出且不可重试则 fatal 返回。）
- [x] 工具：bash（超时参数）/read/write/edit/glob/grep/ls/todo。（✅ 2026-08-27：8 件齐；bash 48KB/16KB 中段截断、超时整秒 clamp；read 行号 `%5d→`、50KB 字节上限续读提示；edit 精确唯一匹配四态；glob `**` 跨段、git 仓库内 ls-files 尊重 .gitignore；grep rg 优先 + 内置扫描兜底；todo 全列表替换 + 状态校验。）
- [x] 读空转守卫（§6.4 注二）：读签名计数 + 产出停滞两级判据，警告/重整/收尾三级反应；验收：mock「同文件反复重读、长期零写」序列依次触发警告 → 重整提醒 → 收尾，正常长探索任务不误收尾。（✅ 2026-08-27：`TestRunReadChurnRegroupThenWrap` 覆盖警告→重整→收尾三级；`TestRunHealthyExplorationNotKilled` 覆盖正常长探索不误杀。）
- [x] usage 记账；工具结果裁剪已有先行版（§10.2 已实现说明），评估是否叠加条数阈值。（✅ 2026-08-27：usage 逐轮累计——provider 上报优先（`IncludeUsage`），无上报按请求/补全字符数估算兜底；评估结论：不叠加条数阈值，64KB 字符预算已覆盖"最近 N 条全量"语义。）
- [x] main.go：现有 AI 循环（`streamRound` 闭包）改为实现 Sink、委托 engine；LoopGuard/L1 折叠/收尾轮一并迁入。（✅ 2026-08-27：`streamSink` 实现 Sink；history 以 `agent.TurnMsg` 承载进 `Input.Base`，L1 折叠因此横跨全请求视图。）
- 验收：mock provider 驱动 10+ 步任务（读→改→跑→看错→修→复跑）端到端跑通；弱参数/未知工具/重复调用三个韧性用例通过。（✅ 2026-08-27 达成：`TestRunLongTaskEndToEnd` 12 步含读→写→跑→看错→修→复跑；`TestRunResilienceWeakCalls` 覆盖未知工具报错回喂/坏 JSON/重复调用第二次警告；另有循环守卫收尾、降级围栏、重试与部分流保留、致命错误、^C 中断等 engine 级单测。落地偏差与修复见下方进度注。）
>
> **M7.2 进度注（2026-08-27）**：engine 化落地时的定案偏差与实施中修复——① `Input.Base` 从 `[]provider.ChatMessage` 改为 `[]TurnMsg`：L1 折叠必须横跨 Base+当前轮的完整请求视图，否则上一轮被折叠的大结果会在下一轮请求中原样复活（`TestAgentToolCollapse` 根因）；② LoopGuard 与读空转守卫共享同一滑窗，读空转分支仅在连续重复计数未达死循环警戒线时生效，避免双重警告；③ provider 400 不支持 tools 的降级在 engine 内完成：重试分类器识别 `ToolsUnsupportedError` 不可重试 → 下一轮去 tools 广播、历史中 role=tool 消息降格为 system（`degradedTurn`，只改请求视图不改 history）→ 围栏协议驱动；④ 流中断修复三处：重试无 attempt 上限会无限重试（补 `RetryAttempts` 上限）、`NativeTools` 未从 Input 传入导致 tools 从不广播、流中断且未输出任何内容时不可重试错误被静默吞成空成功（改为 fatal 返回）；⑤ bash `workdir` 相对路径按任务 cwd 解析（与 read/write 一致）。engine 级+工具级+守卫级单测合计 30+ 项，全量 build/vet/gofmt/test/-race 绿。

### M7.3 长命令与后台 job

- [x] `bash_timeout`/`bash_max_timeout`/`auto_background_after`；job 管理器 + job_output/job_kill 工具。（✅ 2026-08-27：三参数入 `[agent]` 配置，指针区分未配置（缺省 60/600/60s）与显式 0——bash 两参 <1s clamp 到 1s（超时不可禁用），`auto_background_after` ≤0 = 禁用；job 管理器进程组执行（复用 M7.2 `configureProcessGroup`），头 128KB + 尾 128KB head-tail 缓冲（中段以省略标记计大小），上限 50、驱逐优先最老 done、无 done 兜底杀最老 running（§8.2 写"杀最老的空闲"，无空闲时兜底以保证上限恒成立）；bash 工具投机启动 + 三路 select——窗口内完成走原内联渲染，runCtx 超时/中断则 kill+reap 后按超时/被中断渲染，窗口耗尽则 register 转后台回喂"已转后台 job N"；job_output/job_kill 的输出 stdout/stderr 合流（查进度用，不分流）。）
- [x] ^C 两级语义（§8.3）。（✅ 2026-08-27：engine 增 `ToolInterrupt` 槽位——工具执行前 Set(cancel)、执行后 takeFired() 取走标记；fire 时同轮剩余调用以 `[被用户中断: 未执行]` 应答保持 wire 合法，任务继续到下一轮；main.go 仅在 `aiPhase=="exec"` 时 Fire 并提示"前台命令已中断，任务继续"，流式阶段仍整任务取消（M4 语义）。）
- 验收：`sleep 120` 60s 自动转后台，模型 job_output 查到尾部并继续任务；^C 中断前台命令后任务存活。（✅ 2026-08-27 达成：单测覆盖 head-tail 缓冲、job 生命周期/kill 幂等、上限驱逐两场景、job_output 三态、job_kill、超时优先于转后台窗口且不留活 job、^C 中断后同轮剩余调用跳过任务继续；集成测试（真实 rysh 二进制 + mock SSE）`TestAgentAutoBackgroundJobs` 驱动 1s 窗口 `echo bg-live; sleep 20` 转后台 → 原生 tool_call job_output 查到仍在运行+输出 → job_kill 终止 → 终答收尾，`TestAgentToolInterruptKeepsTaskAlive` 前台 `sleep 20` 执行中 ^C → `[被用户中断]` 回喂 → 任务继续出终答。）

> **M7.3 进度注（2026-08-27）**：job 的执行 ctx 独立于任务 ctx（`context.Background()` + 独立 cancel）——后台 job 活过任务结束与任务 ^C，仅随 rysh 进程退出 `StopAll`（§8.2 生命周期语义）；bash 转后台判定用投机启动而非先跑后杀，窗口内完成的命令路径与无 job 模式输出形状一致。全量 build/vet/gofmt/test/-race 绿。

### M7.4 权限审批

- [x] policy：approval 模式、安全命令分类器、前缀/工具持久规则。（✅ 2026-08-28：`internal/agent/policy.go`——`Approval`（mode/ask 回调/会话规则表）+ `Gate`；放行面=非 bash/write/edit 工具直通 + bash 命中安全分类器（`bashLooksReadOnly`+`safeShape`）；write/edit 每问、bash 未命中即问；auto 仅精确 "auto" 生效、每次任务从配置热读（§7.2）；ask=nil fail-closed、ctx 取消判 deny；"a" 落 bash 前缀规则/按工具规则，`RuleSummary` 供 `/permission` 只读展示（v1）。policy 单测 9 项 + engine 级 6 项。）
- [x] 审批 UI（流式锁放行 y/n/a）+ 状态行 `等待确认`。（✅ 2026-08-28：aiui streaming 分支在 `pendingApproval` 时把 y/Y/n/N/a/A 映射为三个 Approve Action、其余键仍 ActionNone；main.go `askApproval` 闭包——琥珀色 `? 运行 <display>（y 是 / n 否 / a 总是）` 行 + 状态行 `等待确认` + 会话日志 `approval` 记录（asked/decided 各一条）+ select 应答通道/ctx.Done；mainloop 三个 Approve case 经 `pendingAsk` 通道投递，迟到的键（提示已关闭）丢弃；^C 前置判断：待审批时答 deny + 整任务取消。）
- 验收：ask 模式下 `ls` 免问、`rm x` 弹审批（y/n/a 三路径）；答 a 后同类 bash 前缀不再问；auto 模式零打扰。（✅ 2026-08-28 达成：engine 级单测覆盖安全 bash 不问/y 放行执行/n 拒绝回喂后任务继续/a 前缀规则二次免问/按工具 always/auto 零询问；集成测试（真实 rysh 二进制 + PTY + mock SSE）`TestAgentApprovalYesNoPaths`（y 路径命令执行进上下文、n 路径拒绝文本回喂且命令未执行）、`TestAgentApprovalAlwaysSkipsSecondPrompt`（a 后同类前缀第二次不再弹）、`TestAgentApprovalAutoModeZeroPrompts`（auto 全程零提示）。）

> **M7.4 进度注（2026-08-28）**：落地偏差与实施要点——① §7.1 "白名单可配增补"暂未接配置：`SetSafeExtra` 接缝已备（增补词只扩词表、不放宽形状规则），配置键随二期"审批规则落配置"一并做；② 三个 M7.4 之前的集成测试（ToolCollapse/AutoBackgroundJobs/ToolInterruptKeepsTaskAlive）依赖无门控路径（管道/分号/非白名单 sleep 会弹审批而无人应答），配置钉 `approval = "auto"` 固化原行为；③ 集成测试输入时序：审批提示打开期间逐字节提交的按键会被误读为 y/n/a 应答，测试须等 `· idle` 再提交下一任务；④ 待审批期间拒绝行的 [tool] 观察行由 `approvalDisplay` 渲染（与各工具 Display 同形），拒绝文本同时进 Display 与回喂；⑤ 审批应答通道 `pendingAsk` 由 writeMu 保护，`runStream` 每任务 `SetMode(配置)` 实现模式热更新。另修复既有缺陷：会话日志记录 ts 由毫秒改纳秒（`Record.Ts`），重建历史的 ts 不再因毫秒截断与 shell 事件的纳秒时刻同毫秒碰撞倒置（TestInterleavedShellAndAITimeline 偶发失败根因）；旧日志（毫秒 ts）按 Unix(0,ms) 读入仍单调有序，排序语义不变。全量 build/vet/gofmt/test/-race 绿。

### M7.5 任务呈现与系统提示

- [ ] 结构化系统提示（§9.2）；todo 屏幕行 + 状态行 `n/m`；会话日志 todo/approval/job 记录。
- 验收：多步任务屏幕上可见规划推进；日志回放含完整工具调用序列。

### M7.6 上下文压缩（§10.3，2026-08-28 已实施）

- [x] compact.go（§10.3 全流程：触发/摘要/机械段/视图投影）+ overflow 触发路径 + 文件清单累积。
- 设计定案见 §10.3（压缩 = 状态交接，ADR 13）：结构化 checkpoint + 机械快照重注入，取代原"最简稳健版"。
- 验收：mock 小窗口模型（context_window=4096）下 30 步任务触发压缩后继续完成；溢出错误自动压缩重试；压缩后不重读已读文件、下一动作与 todo 一致（§10.3 验收节）。

> 2026-08-28 进度（M7.6 完成）：① `internal/agent/compact.go`——轮界门 `compactCheck`（req 组装后、与自我警觉 §16.4 同检查点）：请求估算 > 窗口 85% 且 armed 触发，失败撤防后回落 <50% 重武装（迟滞）；溢出路径 `compactForOverflow`（provider `OverflowError` 分类：400 + 报文含溢出短语）压缩→重试一次→仍溢出 fatal。② 摘要 = 状态交接：当前模型关 tools 复用系统前缀，输入 = 系统前缀 + 被压缩段（先过 L1 折叠，摘要请求自身不撞墙）+ 交接提示词（七段骨架：目标逐字引用/约束与偏好/进度/关键决策/下一步/关键上下文/备注）；迭代压缩把旧 checkpoint 全文并入被压缩段（附迭代规则），不重摘要原始历史。③ 机械段不进摘要调用：文件清单（read/edit/write 累积集合渲染「已读/已修改」）+ `RenderStateSnapshot`（todo 标签清单 + 存活后台 job + 运行中子任务）。④ 切点 `chooseCut` 整轮边界 tool-pair-safe（尾部保留 ≤25% 窗口内、尾累计不超预算的最老合法边界 = 最大化摘要前缀）；压缩后视图 = [系统前缀, mission 逐字, checkpoint user 消息, 切点后整轮逐字]，history/磁盘本体永不修改（不变式①）。⑤ 交付与呈现：`RunResult.Compacted{Checkpoint, Cut}`，main.go 据此收缩内存历史；磁盘仅追加 `compact` 事件（checkpoint 全文，回放可还原）；屏幕 `─── compacting ───` → `[已压缩上下文: 保留近 N 轮]`。⑥ 质量护栏：摘要 <500 字符退化重试 1 次，仍失败撤防触发门 + 降级仅 L1 续跑 + notice（撤防而非下轮重试，防退化摘要器每轮界请求风暴，§10.3 已实现说明）。⑦ 验证：引擎级单测 7 项 + provider 溢出分类 3 项 + 集成 `TestAgentContextCompaction`（真实 rysh + PTY，context_window=4096 小窗口驱动全链路）全绿；行为回归指标属真实模型观察项（§10.3 验收落地注）。

### M7.7 子智能体与管理者模型（§16，2026-08-28 已实施）

- [x] SubagentManager + Subagent（并发 ≤3、完成保留 20、快照继承、StopAll）；task/task_output/task_kill 三工具仅管理者注册表携带（无嵌套）。
- [x] 提示词三分：管理者（上下文吝啬/委派判据/监控/验收/自我警觉纪律）、工人（简报内工作 + 500 字结构化报告）、一次性问答（chatOnce 无工具循环专用）。
- [x] 上下文隔离：子任务转录不进管理者请求、不进会话日志重建（s* 日志族）；只有紧凑状态与 8KB 有界报告过缝。
- [x] 自我警觉两级注入（窗口 50%/75% 或估算 96k/144k token 或消息数 150）+ task 三件守卫归类 other（监控节奏不误触读空转）。
- [x] ^C 第三级：子任务运行中先停子任务（管理者续跑），再按终止任务。
- 验收：委派隔离端到端（真实二进制 + mock SSE，`TestAgentSubagentDelegation`）+ 单测 12 项（生命周期/上限/kill/报告截断/收尾标记/防紧轮询/淘汰/StopAll/委派隔离/紧轮询分类/上限可见/自我警觉两级）；全量 build/vet/fmt/test/-race 绿。详见 §16.6。

### 二期（不定排期）

ask_user_question 工具（任务中向用户提问）；plan mode（只读规划→用户批准→执行）；只读 explore 子任务预设（M7.7 子智能体的受限变体，§16）；AGENTS.md/CLAUDE.md 上下文发现 + skill 发现与 `skill` 工具（细化设计见下）；工具批内并发；job 完成通知注入；`/resume` 任务/会话恢复（JSONL 回放，codex rollout 同款）；审批规则落配置 + `/allow`；MCP；持久 shell 会话（继承登录 shell 环境，README 既有开放问题）。

#### 二期细化：上下文发现与 skill

**AGENTS.md 上下文发现**（目录约定按生态标准，取代之前列项中的 "AGENTS.md/rysh.md" 表述）：

- 发现与叠加顺序（各层带字节预算，总长超限从最近层截断并注明）：
  1. **用户级**：`~/.config/agents/AGENTS.md`（agents.md 规范定义的全局指令位置，跨项目生效）；
  2. **项目级**：仓库根 `AGENTS.md`（从 shell cwd 向上找 `.git` 标记定位仓库根）；根目录找不到时兼容读 `CLAUDE.md`（Claude Code 惯例）；
  3. **子目录级**：monorepo 中，被操作文件（工具参数路径）所在子目录的 `AGENTS.md` 叠加生效，作用域限该目录。
- 注入方式：以 `[AGENTS.md <path>]` 分段追加进系统提示（§9.2），加载顺序"用户级 → 仓库根 → 最近子目录"与 agents.md 规范及主流工具行为一致。

**skill 发现 + `skill` 工具**：

- skill 形态（生态约定）：每个 skill 一个目录 `<name>/SKILL.md`，YAML frontmatter 带 `name`/`description`，可附脚本/参考资料/模板。
- 发现目录（项目级与用户级同名时项目级优先；同时兼容扫描 Claude Code 的 `.claude/` 等价位置）：

  | 层级 | 跨工具约定 | Claude Code 兼容 |
  |---|---|---|
  | 项目级 | `<repo>/.agents/skills/<name>/` | `<repo>/.claude/skills/<name>/` |
  | 用户级 | `~/.agents/skills/<name>/` | `~/.claude/skills/<name>/` |

  （生态另有 plugin 随插件分发 skill 的形态；rysh 无插件体系，不覆盖。）
- 注入策略：**不全量预载 SKILL.md 正文**——系统提示只列 `name + description + SKILL.md 路径` 索引（与 Claude Code 的 skills 机制同款）；模型按需调 `skill` 工具（参数 `name`），工具返回该 SKILL.md 全文（以 skill 目录为相对路径基准，附带的脚本经 `bash` 执行）。
- 理由：skill 可累积到数十个，全量预载既爆上下文又稀释模型注意力；按描述自取、用时加载是各 harness 的收敛做法。

## 13. 配置（`[agent]` 扩展）

```toml
[agent]
env_allowlist = [...]          # M4 已有
approval = "ask"               # "ask"（缺省）| "auto"
bash_timeout = 60              # 单命令缺省超时（秒）
bash_max_timeout = 600         # 模型可上调的上限（秒）
auto_background_after = 60     # 自动转后台阈值（秒，0=禁用）
max_jobs = 50
context_soft_limit = 0.85      # 压缩触发点（context_window 的比例）
disabled_tools = []            # 按名禁用工具（安全/弱模型场景）
# allowlist 二期落盘；v1 会话内即时规则
#
# 预算管理一期（2026-08-27 已实现）暂为硬编码常量（cmd/rysh/loopguard.go）：
#   loopWindow=20 · loopWarnConsec=2 · loopBreakConsec=6 · loopShareLimit=0.6
#   sigPrefixBytes=8KB · l1PreserveBytes=64KB；配置化随 [agent] 面一并落地

# 子智能体（2026-08-28 M7.7 已实现）暂为硬编码常量（internal/agent/subagent.go 等）：
#   subagentMax=3（并发）· subagentKeepDone=20（完成保留）· taskReportHead/Tail=7KB/1KB（报告上限）
#   pollHintWindow=15s（防紧轮询提示）· selfCheck：窗口 50%/75% 或估算 96k/144k token 或消息数 150
#   配置键随 [agent] 面二期一并开放
```

模型级：`tools = true|false`（缺省 true；本地弱模型设 false 走围栏）。

## 14. 测试策略

延续现有两级路线（单测 + httptest mock SSE 集成），engine 与 TUI 解耦后大量验证可下沉到无 pty 单测：

| 层 | 覆盖 |
|---|---|
| provider 单测 | tool_calls 流式分道拼接（跨 chunk）、usage 解析、非法 arguments、tools 400 降级 |
| engine 单测（mock client） | 多步循环、死循环守卫（LoopGuard 语义已由 16 项单测钉死：TestLoopGuard* / TestCollapse*；引擎版原样沿用 2 警/6 停 + 占比判定）、读空转守卫（同读签名计数、零写停滞两级判据，§6.4 注二）、孤儿修复、重试退避、溢出→压缩、围栏兜底合成 |
| 工具单测 | read 行号/限额、edit 唯一匹配（零/多/一/replace_all）、grep 无 rg 回退、bash 超时/进程组、job 管理器（上限/环形缓冲/kill） |
| policy 单测 | 安全分类器（元字符否决、白名单）、前缀规则命中、模式切换 |
| 子智能体单测 | 管理器生命周期/并发上限/kill/StopAll/淘汰（TestSubagent*）；引擎级委派隔离（worker 中间产物不进 manager 请求）、task 轮询不触发读空转守卫、并发上限经工具回喂、自我警觉两级注入（TestRunSelfCheck* / TestManager*） |
| 集成（mock SSE + 真实 pty） | 10+ 步任务端到端、审批 y/n/a 三路径、自动转后台 + job_output、^C 三级语义、压缩触发、子任务委派与上下文隔离（已有先行：TestAgentToolLoopGuard / TestAgentToolCollapse / TestAgentSubagentDelegation） |

## 15. 关键设计决策（ADR 摘要）

1. **原生 function calling 为主、围栏协议为兜底**：五个参考项目全部原生协议；rysh 因 BYO 本地模型保留围栏（合成 bash 调用，单循环双来源）。本地模型可用性与云端模型能力兼得。
2. **edit 采用精确唯一匹配**（Claude Code/crush 同款）而非行号编辑：弱模型下行号易错，文本锚点自描述、错误可自纠。
3. **bash 每次全新 shell + workdir 参数**：不引入持久 PTY（deepseek-harness 的 persistent 变体列二期）——独立执行器是 M4 已验证的取舍（不污染共享屏），目录状态显式化反而对模型更友好。
4. **审批缺省 ask（只读免问 + 写操作问 + 总是放行持久化）**：rysh 直接操作用户真实项目，静默自动（M4 现状）不可接受；opencode/crush 的"一次 a、同类免问"把摩擦压到每命令族一次。
5. **无步数预算（2026-08-27 审核定案，弃用原案"步数预算 50"）**：不设 `max_steps`/轮数上限——"轮数不是病，重复才是病"，防发疯交给死循环守卫（§6.4，2 警/6 停 + 占比判定；触发走收尾轮而非报错，history 完整保留、可自然续跑）。步数预算只会给确实需要几十步的长任务人为设限；grok-build/codex 的 max-turns 在 rysh 由 LoopGuard 承担同一职责。曾评估"缺省关闭的保险丝"折中案，审核决定不保留。
6. **工具结果先裁剪、再压缩**：两档分别解决"任务内膨胀"与"跨任务膨胀"（窗口 85% 摘要，未实现），比单一压缩策略更省 token、更不易撞墙。任务内档已按字符累计预算实现（64KB，§10.2 已实现说明），取代原"8 条之外 4KB 裁剪"方案。
7. **不做沙箱**：rysh 是薄层透传产品，用户 shell 即信任边界；审批（ask 模式）替代沙箱提供第一道闸。codex 的 seatbelt/landlock 路线与"不改用户环境"的产品哲学冲突，明确不做（后续若面向 CI/headless 场景再评估）。
8. **engine 与 TUI 解耦（Sink 接缝）**：M4 的循环长在 main.go 里，工具/权限/压缩的扩展空间被 UI 状态锁死；M7 起循环、工具、策略、上下文各自独立可测，main.go 只做事件渲染与按键路由。
9. **^C 两级语义**（流式=杀任务，执行=杀命令续任务）：复杂任务下"跳过这一步"是高频需求，M4 的一刀切会把 50 步任务的第 20 步毁掉。
10. **会话持久化维持 JSONL**（不抄 SQLite）：rysh 单机单进程、无并发写者，append-only JSONL（M6 已建）+ 二期 /resume 回放足够；SQLite 的查询能力 rysh 用不上，运维面却变大。
11. **守卫判据按病症分型（2026-08-27 案例新增）**：完全相同的重复调用由死循环守卫管（连续签名 + 同签名占比）；"高频读、零写、同目标反复"的原地空转由读空转守卫管（同读签名计数 + 产出停滞，§6.4 注二）——真实案例中 137 次压缩把长会话拖入该病症，连续/同签名判据均无法捕获。两者共享"警告回喂 → 收尾轮"两级结构与"不报错、保留现场可续跑"的语义。
12. **AI 模式 = 管理者模型（2026-08-28 定案，§16）**：顶层 run 是管理者（manager），上下文当作最稀缺资源守财——只留使命、todo、子任务状态与报告；重活一律委派给工人子任务（subagent，独立上下文的 engine run），过程不进管理者上下文，只有有界报告过缝。管理者轮询监控（task_output 紧凑状态）、判疯即停（task_kill）、完成后验收（抽查关键论断）再总结。子任务复用现有 engine 全套守卫（死循环/读空转/收尾轮），"发疯"有双保险：管理者主动停 + 守卫强制收尾（报告标记"被强制收尾"，验收需格外严格）。^C 在子任务运行中先停子任务（管理者续跑），再按一次才终止任务。不做子任务嵌套（深度 1）、不做子任务独立模型/独立审批（会话级审批照旧、模型随管理者快照）。
13. **压缩 = 状态交接（2026-08-28 定案，§10.3）**：五家 harness 源码对比（docs/上下文管理对比分析.md）的收敛结论——结构化 checkpoint（目标逐字引用 + 进度/决策/下一步）+ 机械渲染的文件清单与存活状态快照（摘要管"过去"、快照管"现在"）；history 本体不动、只改请求视图投影；切点整轮边界 tool-pair-safe；二次压缩把旧 checkpoint 交摘要器迭代合并、不重摘要原始历史；85% 触发 / 50% 回落迟滞 / 摘要退化（<500 字符）重试一次后降级续跑 / 永不因压缩杀任务。已实施（2026-08-28，`internal/agent/compact.go`；落地偏差一处——摘要失败撤防而非下轮重试，见 §10.3 已实现说明）。不照搬：codex 全量 user message 保留（mission 锚点已覆盖）、dsh 事件溯源架构（当前规模过度设计）。

## 16. 子智能体与管理者模型（M7.7）

> 2026-08-28 定案并实施。用户定调：传统 shell 模式里 shell 本体永远是管理者和监控者，真正干活的是被它准备参数、环境、目录、等待并收尸的工具进程；AI 模式的核心是上下文管理——ruyishell 要像葛朗台一样吝啬，竭尽全力减少对自己上下文的污染。除极简任务与关系自身的任务外，都让子智能体干活；ruyishell 监控子智能体进展、时刻维持状态判断、发疯时停掉、干完后验收和总结；自己的上下文永远不忘初心、牢记使命；永远对自身状态保持警觉，任何一部分变得异常多和异常大时，都要看看哪里出问题了。本节即该定案的落地。

### 16.1 总原则：上下文吝啬

- **管理者上下文只保留三样**：使命（第一条用户消息）、todo 计划、各子任务的状态与报告。重活全部委派；子任务的完整过程（工具结果、试错、死胡同）留在子任务自己的上下文里，**永远不进管理者上下文**——管理者只拿到紧凑状态与有界报告。
- **委派判据（写进管理者系统提示）**：亲自做＝极简任务（一两条命令/一次读文件就够）、关系自身的事（对话、规划、汇报）、对子任务报告的验收核查；其余一切（探索、读代码、重构、构建、测试、调试等多步工作）＝`task` 委派。
- **简报纪律**：子任务提示词必须自包含（目标、相关路径、约束、要汇报什么）；不把大段内容粘给子任务——它自己能读文件。
- **验收纪律**：子任务报告完成后不盲信——对关键论断抽查（跑相关测试、看 diff、读改动过的文件片段）；通过才更新 todo，不通过带着修正说明重派（同一目标最多重派两次）。
- **报告纪律**：子任务结束以纯文字报告收束（已完成/关键发现与修改/未完成与建议，≤500 字）——这是管理者唯一能看到的子任务产出。

### 16.2 进程形态：子任务 = 同进程独立 engine run

```
用户提示
  │
  ▼
管理者 run（agent.Run，顶层）        ← 瘦上下文：使命 + todo + 子任务状态/报告
  │  工具 = 全量工具 + task / task_output / task_kill
  │
  ├─ task(description, prompt) ──► SubagentManager.Spawn（id 自增，≤3 并发）
  │                                  │ goroutine，独立 ctx（活过管理者任务结束，随 rysh 退出 StopAll）
  │                                  ▼
  │                                子任务 run（agent.Run，独立上下文）
  │                                base = cwd + env + WorkerInstructions（无 shell 时间线、无管理者历史）
  │                                prompt = 简报（首条 user 消息）
  │                                全量工具（共享会话 JobManager 与 Approval 门）
  │                                全套守卫（死循环/读空转/收尾轮）+ L1 折叠 + 自我警觉
  │                                sink → 屏幕（─── task N: <desc> ─── 头行 + 流式 + [task N] 工具行 + 终行）
  │                                结束 → 报告（RunResult.Report() = 末条无工具调用的 assistant 消息）
  │
  ├─ task_output(id) ──► 紧凑状态（运行中：步数/耗时/最近动作/todo；完成：报告；终止：部分报告）
  ├─ task_kill(id)  ──► cancel 子任务 ctx（管理者判疯即停）
  └─ 终答：验收 + 总结（概括子任务结论，不整段粘贴报告）
```

- **无嵌套**（深度 1）：子任务的注册表是普通全量工具，不含 task 三件。
- **共享面**：子任务与同会话共享 JobManager（子任务转后台的 job 管理者也能查/杀）与 Approval 门（写操作照旧弹审批，"a" 会话规则通吃；子任务审批等待期间 ^C = 拒答并终止该子任务）。
- **快照继承**：Spawn 时冻结当前管理者任务的 provider client / 模型 / cwd / env（SubagentSnapshot）；后续 `/model` 或目录切换不影响已在跑的子任务。
- **屏幕（一屏一流不变）**：子任务流式文本直出（markdown 渲染），身份由 `─── task N: <desc> ───` 头行、`[task N] $ cmd (exit n)` 工具行与 `[task N] done · N 步 · 耗时` 终行承载；状态行仍归管理者 sink 独占。多个子任务与管理者文本交织时按写出时序落屏（v1 可接受，不做分栏）。
- **会话日志**：子任务事件以 `s*` 族 kind 落盘（srea/sasw/stool/snoti，生命周期 sub）——`reconstructHistory` 只重建 usr/asw/tool，故会话切换/重启后**子任务转录永不回流管理者历史**（与上下文隔离同构的日志隔离）。
- **保留与淘汰**：完成的子任务保留 20 个供迟到的 task_output 查询，超出淘汰最老；并发上限 3（超上限 Spawn 返回工具错误，管理者自行调度）。
- **结束门（run 不能在自身子任务运行时结束）**：run 开始时尚在运行的子任务（外来遗留）不算，只 gate 本 run 派遣出的子任务（子任务自己的引擎看兄弟子任务，同理不受 gate）。模型想给终答但自身子任务仍在跑：第一次引擎注入一次性提醒（「全部子任务结束前不能收尾，引擎也不会在子任务完成时重新唤醒你，请继续用 task_output 轮询」）；模型仍要结束，引擎不再询问——自己等全部子任务结束（等待不花 token，run 停在轮界），把每个子任务的最终状态与报告作为一条 user 消息回喂，模型再收尾。守卫强制收尾轮同规则：收尾轮里模型被停但自身子任务仍在跑时，先收报告再收尾（wrap 可再调度，届时子任务已安定，该轮正常结束 run）。此门消灭「任务说完就结束、`[task N] done` 终行却无人验收」的孤儿终行。

### 16.3 监控与判断（管理者何时动手）

- `task_output` 回喂紧凑状态；运行中附**最近动作**（子任务最后一条工具行的截断）与 **todo 进度**，供管理者判断"在不在推进、有没有跑偏"。
- **防紧轮询**：15s 内对同一子任务的连续轮询若无步数进展，回喂附"（自上次检查无新进展；稍后再查，或先做别的事）"提示；提示与警告只进请求视图。
- **判疯停掉**：卡住/反复同一动作/偏离简报 → `task_kill`（ctx cancel，引擎带回部分交换，状态记"已终止"，部分报告若有一并给出）→ 修正简报重派。
- **双保险**：子任务自身跑在完整 engine 上——死循环守卫（2 警/6 停+占比）、读空转守卫（重读警告/重整/收尾）、收尾轮全数生效；守卫强制收尾的子任务，其状态带 `被强制收尾（loop guard / read churn）` 与"报告可疑，验收需格外严格"标记，管理者必须更严地验收或重派。
- **管理者自身也防紧轮询**：task 三件在守卫窗口里归类为 other（非读非写）——task_output 是状态轮询不是工作区读取，读空转守卫的"同目标重复读"判据（按工具+参数计数）不误伤正常监控节奏；但完全相同（同参数+同输出）的紧轮询仍受死循环守卫约束（2 次起回喂警告、6 次收尾）——管理者对着一个卡死的子任务无限紧轮询本身就是需要被停的行为。

### 16.4 自我警觉（对自身状态的警戒）

- **两级一次性注入**（engine 级，管理者与子任务同样生效；触发点在轮界、请求组装后）：
  - 当前请求 ≈ 窗口 50%（未配 `context_window` 时按 ≈96k 估算 token）→ 注入 level 1「上下文正在变重」：检查上下文是否还围绕使命；任何一部分变得异常多、异常大（陈旧细节、重复查询、超大输出）时，先定位来源再丢弃或压缩；todo 与子任务报告是权威进度记录，已读过的不要重读。
  - 再升至窗口 75%（≈144k 估算）→ 注入 level 2「接近上限」：立即整理——重写 todo 清单压缩进度、丢弃陈旧细节、已做的事情以子任务报告和 todo 为准。
  - 消息数 ≥150（Base+turn）作为"异常多"的独立触发（对应无窗口场景）。
  - 度量取「当前请求字符估算」与「上一轮上报 prompt_tokens」的较大值（usage 滞后一轮）；屏幕打 `─── self-check：… ───` 暗色行，会话日志 noti。
- **使命锚点**：使命 = 首条用户消息，注入文案反复指向它；L1 折叠不触碰 user 消息，历史裁剪（trimHistory 整轮）保持它存活。
- **有界输入**：报告回喂上限 8KB（头 7KB+尾 1KB）；子任务 todo/最近动作行截断（120 字符）；管理者自己的工具结果受既有工具限额与 L1 折叠约束。上下文膨胀因此只有两个合法入口（报告、状态），都自带边界。
- M7.6 的 LLM 摘要压缩落地后与本节叠加：自我警觉（模型自觉压缩）在前，自动压缩（机制兜底）在后。

### 16.5 ^C 语义（在 §8.3 原两级上再扩一级）

1. 有待审批（管理者或子任务发起）→ 拒答 + 终止该 run（既有语义；子任务发起的审批被拒后子任务以拒绝文本继续或自行收尾）。
2. 管理者正在执行前台工具 → 只中断该命令，任务继续（既有语义）。
3. **有子任务在运行** → 停止全部运行中子任务（`[task N] 已终止` 终行照落），管理者任务继续，其下一次 task_output 可见终止状态；状态行提示「已停止 N 个子任务，任务继续（再按一次终止任务）」。
4. 其余（流式中/空闲）→ 取消整个管理者任务（M4 语义）。
- 管理者任务结束时子任务不随死（与 job 同语义）：后台继续跑，下一个提示词里 task_output 仍可查；rysh 退出时 `subMgr.StopAll()` 一并清掉。

### 16.6 里程碑与验证

- **M7.7（本条，2026-08-28 已实施）**：
  - [x] `internal/agent/subagent.go`：SubagentManager/Spawn/Status/StopAll（并发上限 3、完成保留 20、淘汰最老）。
  - [x] `internal/agent/tools_task.go`：task/task_output/task_kill + ManagerTools（仅顶层注册表携带）。
  - [x] 提示词三分（prompts.go）：管理者（Instructions，委派/监控/验收/自我警觉纪律）、工人（WorkerInstructions，简报内工作 + 500 字报告）、一次性问答（ChatInstructions，chatOnce 无工具循环专用）。
  - [x] engine 自我警觉两级注入（§16.4）+ task 三件守卫归类 other。
  - [x] main.go 接线：subMgr 生命周期、subSink（s* 日志族）、^C 第三级、runStream 快照与 ManagerTools、ContextWindow 传入。
  - 验收：
    - 单测 15 项：子任务生命周期/上限/kill 幂等/报告 8KB 截断/守卫收尾标记/防紧轮询提示/淘汰/StopAll；引擎级委派隔离（中间输出不进管理者请求、报告过缝、双向隔离）、紧轮询不误触读空转、上限经工具错误可见（管理者先 task_kill 停掉全部工人再结束——有结束门时否则收不了尾）、自我警觉两级（窗口与估算两路）、结束门三路径（首次欲结束→提醒、再欲结束→引擎等待并回喂报告、守卫收尾先收报告再收尾）。
    - 集成测试 `TestAgentSubagentDelegation`：真实 rysh 二进制 + mock SSE 端到端——管理者派遣 → 子任务独立循环（其 cat 输出只进子任务上下文）→ 轮询 → 报告过缝 → 验收终答；服务端断言所有管理者请求体均不含子任务中间输出。
    - 全量 build/vet/fmt/test/-race 绿。
- 落地偏差与已知限制：① 子任务 base 不带 shell 统一时间线（简报承载上下文，保持子任务瘦；后续可评估按需附带）；② 子任务流式文本与管理者文本交织落屏（不分栏）；③ 防紧轮询提示的 15s 窗口内，若子任务卡死且管理者以亚秒级轮询，死循环守卫会在第 6 次相同轮询收尾管理者任务——有意保留（管理者空转也属"发疯"，且收尾保留现场可续跑；收尾时子任务仍在运行则结束门先收其报告再收尾，§16.2）；④ 配置面未开（并发上限/报告上限/自我警觉阈值暂为硬编码常量，随 `[agent]` 面二期开放）。

> **进度注（2026-08-31）**：请求 wire 形态合规修复——切换模型到严格 OpenAI 兼容端点（sglang）后每个请求 400 "System message must be at the beginning"。实测该端点要求 system 消息**恰好一条且位于 0 位**：rysh 内部上下文的多 system 形状（cwd/env/Instructions 三条基础 + shell 事件按时间线原位 + 历史工具记录重建为 system）在宽松端点（DeepSeek）一直可用，在严格端点全灭。定案：内部视图保持会话自身角色不动（显示回放/裁剪/压缩过滤依赖），收敛统一放 provider 出站口 `compliantSystem`（`ChatStream` 单一出口）——开头连续 system 合并为一条，首个非 system 之后的 system 降格为带标记的 user 消息；M7.2 注③的 `degradedTurn` 降格目标由 system 改 user（与 wire 规则一致，该降级本身只在无 tools 端点触发，但形状必须同样合法）。回归：provider 单测（合并/降格/不改写输入）、`TestAIRequestSystemPlacementOnStrictEndpoint`（sglang 规则 mock 端点 + 交错 shell 事件 + 带工具历史，修复前红、修复后绿）、真实 sglang 端点 pty 端到端（/model 切换 → 交错命令 → 提问得回复）。
