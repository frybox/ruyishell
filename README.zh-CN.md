# ruyishell

[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

[English](README.md)

以增强本机 shell 的 AI 交互体验为目标的新一代交互式 shell（命令行命令：`rysh`）——以 shell 为默认、AI 为副驾，在同一行输入中一键切换两种模式。

## 为什么做 ruyishell

用 AI 智能体干活已经成了日常：codex、claude code、pi、grok，用自然语言描述需求确实很爽。但作为一个开发者，很多时候我其实只需要执行一条 shell 命令——比如 `ls` 一眼看看本地文件有何变化。可如果我用自然语言说"看看当前目录文件"，链路是：发给大模型 → 模型推理 → 产出一个 bash 工具调用 → 智能体执行 → 结果回灌给大模型总结 → 最终输出到我眼前。等看到这个结果，我会疯掉。

这些工具的 GUI 版本都内置了终端窗口，但"打开终端、执行命令、再关掉终端"的体验也不好；CLI 版本则要么临时退出工具、在 shell 里查看完再重新打开并恢复会话，要么开两个窗口——一个智能体、一个 shell。

于是有了 ruyishell：它看起来就是你日常使用的 bash/zsh/fish（Windows 下 cmd/pwsh），提示符前面只有一个小小的暗色 `(rysh)` 标记。但在提示符后没有任何键盘输入时按一下空格，提示符就换掉了——ruyishell 变成了一个自然语言交互的智能体。在使用智能体过程中，提示符后没有任何输入时再按空格，又切回熟悉的 shell，可以执行 `vi`、`ls` 等任意 shell 命令。当命令行上已经输入了部分内容时，用 `Shift+Tab` 组合键完成模式切换。

还有一点：ruyishell 的任何配置改动都是热改动——无需重启，实时生效。用 grok、claude code 这类工具时很难受的一点，就是改了配置必须重启、然后再 `/resume` 恢复会话；ruyishell 没有这种打断。

还有一个常见的别扭场景：使用中途中希望当前会话换个目录（`cd`）——但其他智能体软件里，我只能退出会话、切换目录、重启，而重启之后就是一个新会话了。在 ruyishell 里，你可以随时切到 shell 模式，`cd` 到指定目录，再切回 AI 模式：当前目录跟着变了，会话还是同一个会话！

## 快速上手

`rysh` = 你现有的 shell + 一个共享同一屏幕的 AI 副驾。它**不替换**你的终端和 shell 配置：rysh 启动你自己的 shell（bash/zsh/fish/…），你在同一行输入上随时切进 AI 模式——敲到一半的命令随切换带过去，AI 能读你的 shell 历史与当前目录，切回时草稿回到提示行。

```
curl -fsSL https://github.com/frybox/ruyishell/releases/latest/download/install.sh | bash    # 预编译二进制；其余安装方式见「安装」
rysh                                            # 启动——你落在自己的 shell 里
```

然后，在 shell 运行中：

1. 按 `Shift+Tab` 进入 AI 模式（或在行首敲一个空格）。
2. 用自然语言输入任务，回车——回复在原地流式显示，你留在 AI 模式可继续追问。
3. 再按 `Shift+Tab` 回到 shell；你刚敲的文本会回来。

## 典型场景

- **在 shell 里临时问一句 / 让 AI 写条命令**：正在敲 `grep` 想不起某个选项？`Shift+Tab`，把要解决的问题说给 AI，拿回完整命令，`Shift+Tab` 切回 shell 执行。不需要离开你正在做的活。
- **让 AI 执行多步任务并实时看输出**：「把 `docs/` 下所有 `.md` 里的 TODO 汇总成一个文件」——AI 自己调用 read/grep/write 等工具逐步完成，每步命令以灰色 `[tool]` 行上屏，可随时 `^C` 中断。
- **多会话并行**：一个终端里先后开多个 AI 会话（`/new` 新建、`/ls` 列、`/resume <编号>` 切回），每个会话的上下文、cwd 与草稿相互隔离；`rysh ls` 等一次性子命令在任意终端可用。
- **多行粘贴**：AI 草稿与 shell 输入都支持 bracketed paste——粘贴的整块换行保留、不会误触发提交，一次 `Ctrl+Z` 整块撤销。

## 与 Warp 类终端的差异

Warp 是「换掉你的终端」（一个独立的 GPU 渲染 GUI 应用，闭源，AI 绑定其云服务）；`rysh` 是「不换终端、增强你的 shell」——跑在任何现有终端里，退出后把终端原样还给你。核心差异：模型自由（BYO provider，任意 OpenAI 兼容端点、可全本地，`/model` 热切换，无账号无遥测），以及模式切换零摩擦（同一行输入、一键切换、内容不丢、AI 看得见 shell 历史、共享同一屏幕流）。

## 文档

- [产品方向](docs/产品方向.md)
- [需求分析](docs/需求分析.md)
- [架构设计](docs/架构设计.md)
- [主屏方案](docs/主屏方案.md)（屏幕模型：只用主屏、原生滚动历史、无状态行）
- [工作计划](docs/工作计划.md)

## 状态

以下条目按里程碑记录已交付的能力，含当时的界面形态。屏幕模型自 2026-08-29 起以[主屏方案](docs/主屏方案.md)为准：rysh 全程留在终端主屏，不再有底部状态行、滚动区裁剪与退出清屏，早期条目中相关描述已被取代。

M2（模式切换与视觉反馈）已实现并在 Linux 验证：shell/AI 一键切换、底部状态行、光标联动；单元 + 集成测试通过；Windows 交叉编译通过（运行未验证）；macOS 未验证。

M3（AI 模式 + provider 接入）已实现：provider/model 配置（参考 pi 的 models.json 结构，TOML 载体）+ `/model` 热切换 + agent loop（自然语言 → 流式回复 → 留在 AI 模式追问）。见[工作计划](docs/工作计划.md)。

M4（工具调用与会话上下文）已完成：AI 对话多轮记忆 + `/new` 新建会话 + cwd/环境变量跟踪 + Agent 工具调用 + 统一事件流全部落地（agent 能感知当前工作目录与 shell 环境，Linux 经 `/proc` 读取，环境按白名单过滤不泄露密钥；模型可在回复里输出 `bash` 围栏块，rysh 执行并把输出回灌上下文；shell 模式下用户执行的命令与输出也进入 AI 上下文，每条自带 cwd）。

M5+（TUI 增强，v4 一屏一流）已实现：AI 与 shell 共享同一屏幕、同一条终端流——切换模式不切备用屏、不清屏、不重建 shell 画面，只在原地把 shell 提示行换成 AI 提示行（底部状态行整行浅灰底色，`[SH]`/`[AI]` 徽标 + 光标形状联动，AI 模式下任务流式期间锁定切换、仅 `^C` 可终止；退出 rysh 时清空整屏再交还终端）。shell 与 AI 共用同一条输入行：Shift+Tab 切换时，shell 里敲到一半的命令带进 AI 输入行、AI 草稿在切回时回注到 shell 命令行，文本与光标位置都随切换延续（shell 光标所在列带进 AI 草稿，切回时用方向键把 shell 光标回注到同一列；仅对带行编辑器的 shell 如 bash/zsh/fish/ksh，纯 shell 如 dash 无行编辑器，光标回到行尾）。AI 提示行支持 bash-PS 风格配置（`[ai] prompt`）。回复流式渲染在共享流上：推理内容暗色显示、回复正文按 markdown 渲染（标题加粗下划线、加粗/斜体、内联代码青色、链接下划线+暗色 URL、列表与引用、围栏代码块灰色背景填充，未闭合的构造在关闭前不闪现原始标记）、工具调用显示为灰色 `[tool] $ <命令> (exit <N>)`。AI 模式期间 shell 输出缓冲并记录到会话日志（`~/.rysh/sessions/<id>/messages.log`，append-only JSONL），返回 shell 时一次性刷新，裸回车让 shell 自绘提示行完成重同步。shell 模式保持字节级直通，底部状态行 `[SH]` 徽标与滚动区保护不变。

Agent 预算管理一期（[架构设计](docs/架构设计.md) §3.9 的 M1）已实现并在 Linux 验证：删除 `maxToolRounds=4` 轮数上限（工具循环不再设轮数），新增 loop guard 循环防护——同签名连续重复 ≥6 次或饱和窗口占比 >60% 判定失控并注入收尾指令强制文字总结（屏幕 `─── wrapping up (loop guard) ───`，连续 ≥2 次先附警告）；L1 上下文瘦身——请求组装时 64KB 尾部预算外的旧工具输出折叠为一行占位符。Token 记账（M7.2）与 L2 摘要压缩（M7.6）已随后续里程碑实现；`[agent]` 配置面为三期。详见[工作计划](docs/工作计划.md)。

M7.1/M7.2（Agent 执行流程重做的前两步）已实现并在 Linux 验证（设计见[agent执行流程设计](docs/agent执行流程设计.md)）：工具调用升级为 provider 原生 function calling，markdown 围栏协议保留为兜底（模型不支持或配置 `tools = false` 时自动降级，行为同 M4）；执行循环从 main.go 迁入独立引擎（`internal/agent`），驱动八件工具——bash（单次超时可调、大输出中段折叠）、read（行号 + 续读）、write、edit（精确唯一匹配）、glob/grep（git 仓库内尊重 .gitignore）、ls、todo（模型自维护任务清单）。引擎内置重试（429/5xx 指数退避、服务端 Retry-After 优先）、死循环守卫（同一动作连续重复先警告后强制收尾总结）、读空转守卫（反复重读且长期零产出先提醒重整、仍无改观才收尾）；旧工具结果按 64KB 预算折叠出请求，token 用量按上报优先、估算兜底累计。弱参数/未知工具的报错回喂模型自纠而不是中断任务；`^C` 终止当前任务恢复输入。权限审批见下方 M7.4。详见[工作计划](docs/工作计划.md)。

M7.3（长命令与后台 job）已实现并在 Linux 验证：`[agent]` 配置新增 `bash_timeout`/`bash_max_timeout`/`auto_background_after`（缺省 60s/600s/60s）。前台命令跑满自动转后台窗口仍未结束时，rysh 把它收编为后台 job（独立进程组，上限 50 个，超出先清最老的已结束 job）并回喂「已转后台 job N」——命令继续跑，模型用 `job_output` 查看状态与尾部输出（默认尾 200 行，单 job 输出保留头尾各 128KB、中段折叠计数）、用 `job_kill` 终止；job 活过任务结束与 `^C`，仅随 rysh 退出清理。`^C` 升级为两级语义：命令执行阶段按 `^C` 只中断当前前台命令（结果回喂「被用户中断」，任务继续，模型自行决定下一步），流式回复阶段的 `^C` 仍终止整个任务。

M7.4（权限审批）已实现并在 Linux 验证：AI 任务执行工具前先过权限门——读类工具（read/glob/grep/ls/todo/job_*）与命中安全分类器的 bash（已知只读命令词 + 整条不含 shell 元字符/重定向）直接放行；write/edit 每次询问，未命中分类器的 bash 弹出独占一行、整行琥珀色高亮的审批条 `? 运行 <命令>（y 是 / n 否 / a 总是）`，状态行显示「等待确认」。`y` 放行本次；`n` 拒绝——拒绝文本回喂模型、任务不中断，由模型自行改道；`a` 放行并记会话级规则（bash 按命令前缀，批准 `git push` 不会连带 `git status`；文件工具按工具记忆，写密集任务只问一次）。`[agent] approval` 缺省 `ask`，设为 `auto` 全自动零打扰（等价 M7.3 之前的行为），改配置对下一个任务生效。待审批时 `^C` 视同拒绝并终止整个任务；每次询问与决定都留痕会话日志（`approval` 记录）。详见[工作计划](docs/工作计划.md)。

M7.6（上下文压缩）已实现并在 Linux 验证（设计见[agent执行流程设计 §10.3](docs/agent执行流程设计.md)）：上下文接近模型窗口 85%（未配置窗口按 144k token 估算兜底）或 provider 返回溢出错误时，引擎在轮界自动压缩——把尾部保留线（≤25% 窗口）之前的完整轮次交给当前模型做一次关 tools 的摘要调用，产出七段交接骨架（目标逐字引用/约束偏好/进度/关键决策/下一步/关键上下文/备注），叠加机械渲染的文件清单（已读/已修改）与存活状态快照（todo/后台 job/运行中子任务）合成 checkpoint；迭代压缩把旧 checkpoint 一并交给摘要器合并，不重摘要原始历史。压缩后请求视图 = 首条用户消息逐字 + checkpoint + 切点之后近轮逐字（切点只落在整轮边界、永不拆散工具调用与结果对），会话历史与磁盘日志本体永不修改（磁盘仅追加 `compact` 事件，回放可还原）。压缩后回落到 50% 以下才可能再次触发（迟滞防抖）；摘要过短视为退化，重试一次仍失败则撤防、本轮仅 L1 折叠续跑，永不因压缩杀死任务；provider 溢出错误先压缩再重试一次，仍溢出才报错。屏幕压缩期间显示 `─── compacting ───`，完成后提示 `[已压缩上下文: 保留近 N 轮]`。

M7.7（子智能体与管理者模型）已实现并在 Linux 验证（设计见[agent执行流程设计 §16](docs/agent执行流程设计.md)）：AI 模式定调为**管理者模型**——顶层任务的管理者把上下文当作最稀缺资源，像守财奴一样吝啬，除极简/关乎自身的任务外，重活一律用 `task` 工具委派给工人子智能体（同进程、独立上下文的引擎循环，复用全套守卫：死循环/读空转/强制收尾）。子任务的流式过程以 `[task N]` 前缀实时上屏并记入会话日志（`s*` 族事件），但只入日志、不回流管理者上下文；管理者用 `task_output` 轮询紧凑状态（步数/耗时/最近动作/todo，15s 无进展附防紧轮询提示），干完后取 8KB 有界报告做验收与总结；发现子任务发疯用 `task_kill` 立即停止（并发上限 3；被守卫强制收尾的子任务报告带「被强制收尾」标记，验收需格外严格）。任务在自身子任务运行中不能结束：模型想收尾时引擎先提醒一次（引擎不会在子任务完成时重新唤醒模型），仍想结束则引擎自己等子任务全部结束、把最终状态与报告回喂给模型做验收总结；守卫强制收尾轮同理——先收报告再收尾。自我警觉：上下文达模型窗口 50%/75%（未配置窗口按估算 96k/144k token 兜底，另有消息数 ≥150 判据）时注入两级提醒，促使管理者检视膨胀、优先委派。`^C` 升级为三级语义：待审批→拒绝并取消任务；命令执行→只中断当前命令、任务继续；子任务运行中→先停掉全部子任务、管理者带着部分报告续跑，再按一次才终止任务。子任务不嵌套（深度 1），随 rysh 退出全部停止。

v6（单进程多会话）已实现并在 Linux 验证：一次 `rysh` 运行可先后持有多个 AI 会话——每个会话是 `~/.rysh/sessions/<id>/messages.log` 一条磁盘流（历史永不覆盖），全局台账 `~/.rysh/created`（编号→会话）与 `~/.rysh/updated`（最近使用序）记录清单；切换会话 = 终止当前登录 shell（连同它派生的全部子进程，Windows 上经 launcher 桩启动的真实 shell 也在内）、在同一个 pty 上重启登录 shell，屏幕上打一条分隔行再接会话尾部回放（不清屏，见下条），草稿/AI 历史/事件环按会话隔离、互不串线。会话操作三条路可达：AI 模式斜杠命令 `/ls` `/new` `/resume`；shell 或任意终端下的 `rysh ls/new/resume/kill` 子命令（rysh 内执行时是一次性子进程，`new`/`resume` 经控制通道让当前实例切换，不嵌套第二份 pty）；顶层启动形态 `rysh new` / `rysh resume <标识>` / `rysh ai` 直接进入。另有 `rysh ai "<消息>"` 一次性问答、被其他 rysh 进程附加的会话拒绝切换（`rysh kill` 终止附加者）。详见「多会话」一节与[工作计划](docs/工作计划.md)。

主屏方案（2026-08-29 批准，设计见[主屏方案](docs/主屏方案.md)）已实现并在 Windows ConPTY 下经集成测试验证：rysh 全程留在终端**主屏**——启动打两行暗色横幅（`rysh · <model> · session <id>` + 一行切换键提示「`<mode_switch 键>（或行首空格）进入 AI 模式 · exit 退出 rysh`」，键名随 `mode_switch` 配置），不再进备用屏、不再裁剪滚动区（DECSTBM）、不再画底部状态行，shell 与 AI 的输出全部落入终端原生 scrollback，`Shift+PgUp` / 鼠标滚轮可直接回看；模式切换仍只在原地换提示行，但 AI 提示行成了模型的唯一落点（`cwd · model · [AI]:`，缺省模板新增 `\m` 转义），流式等待改为在内容**末行**就地显示 `⠋ 正在思考...` / `⠋ 执行中...`（spinner 前后各留一空行），从启动到登录 shell 首个输出之间同样是 `⠋ 启动中...`（首个输出到达即被替换，光标随之恢复显示）；退出与切换会话都不再清屏，历史原样留在终端；全屏程序（vim 等）期间仍暂停记录，但退出后不再重进备用屏、不重建界面，透传即可；`[tui] cursor_style` 保留解析但不再发送 DECSCUSR（已无状态行可联动）。常驻标识由提示符标记承担（缺省开启：终端标题接管 + bash/zsh 提示符 `(rysh)` 前缀，见下方「提示符标记（常驻标识）」；`[tui] prompt_marker = "off"` 可恢复纯透传）。

构建：

```
go build ./cmd/rysh
```

或用 Makefile（含交叉编译、测试、格式检查等）：

```
make build      # -> bin/rysh
make cross      # -> dist/ 下 8 个平台静态二进制 + checksums.txt
make test       # 全量测试
```

## 安装

**macOS / Linux — 预编译二进制**（推荐，无需 Go）：

```
curl -fsSL https://github.com/frybox/ruyishell/releases/latest/download/install.sh | bash
```

`install.sh` 随每个 release 一起发布；`releases/latest/download/...` 永远 302 到最新 release，所以这一行长期有效。脚本会从同一 release 下载匹配的 `rysh-<os>-<arch>.tar.gz` / `.zip` 并自动校验 SHA256。安装到 `~/.local/bin`（可用 `PREFIX=/usr/local` 覆盖）。

**macOS / Linux — 从源码**（需 Go 1.25+）：

```
./scripts/install.sh --source        # 装到 ~/.local/bin
PREFIX=/usr/local ./scripts/install.sh --source
```

或直接 `go install ./cmd/rysh`。

**Windows**：从 Release 下载 `rysh-windows-amd64.zip`（按 CPU 架构选择），解压后把 `rysh.exe` 放到 `PATH` 中的目录即可。

`rysh -v` / `rysh --version` 查看版本。不支持嵌套交互：在 rysh 的 shell 里再运行裸 `rysh` 会被拒绝（外层 rysh 导出 `RYSH_INSIDE` 标记，内层建 pty 前直接拒绝）；`rysh ai/new/resume/ls/kill/-v` 等一次性形式可正常执行（见「多会话」）。

测试：

```
go test ./...
```

## 配置

provider/model 配置在 `~/.rysh/config.toml`（`$RYSH_CONFIG` 可覆盖路径），结构对齐 pi 的 `models.json`：`providers` 表下每个 provider 含 `base_url` / `api` / `api_key` / `models[]`。示例：

```toml
default = "ollama/llama3.1:8b"

[providers.ollama]
base_url = "http://localhost:11434/v1"
api = "openai-completions"
api_key = "ollama"

[[providers.ollama.models]]
id = "llama3.1:8b"
name = "Llama 3.1 8B"

[[providers.ollama.models]]
id = "qwen2.5-coder:7b"

[providers.mycloud]
base_url = "https://api.example.com/v1"
api = "openai-completions"
api_key = "$MY_API_KEY" # 环境变量引用，不落盘明文
```

模型以 `provider/model` 引用（如 `ollama/qwen2.5-coder:7b`）。密钥可用 `$VAR` / `${VAR}` 引用环境变量（`$$` 转义字面 `$`）。**每次 `/model` 都直接重新读取配置文件**：编辑 `config.toml` 保存后无需重启 ruyishell，下一次 `/model` 即生效（参考 pi 的 `/model` 用法）。无参数 `/model` 以编号列表展示全部可用模型（当前模型行首标 `>`，与会话列表一致）；`/model <编号>` 或 `/model <provider/model>` 切换。模型可设 `tools = false` 关闭原生 function calling（缺省开启）。关闭时该模型只做纯文本回答、不执行任何工具；若服务端因 `tools` 参数返回 400，则视该模型不支持 function calling，AI 任务以明确报错结束——没有执行 markdown 代码块的兜底（不支持工具调用的模型只能做问答，不适合当智能体大脑）。

### 默认 shell

`shell = "auto"`（缺省）探测平台默认 shell：Linux/macOS 取 `$SHELL -l`（登录 shell，读取 `.profile`），Windows 按 `pwsh` → `powershell` → `cmd` 探测。也可显式指定路径：

```toml
shell = "/bin/zsh"
```

该值在**启动时**生效一次（shell 进程只创建一次），随后编辑需重启 ruyishell。

### 按键绑定

`[keys] mode_switch` 可更换 shell/AI 模式切换键（缺省 `shift-tab`，即 CSI Z）。很多终端（如 Windows Terminal）把 Ctrl+Tab 用于切换标签页，故默认改用 Shift+Tab（与 Claude Code / Codex 的模式切换惯例一致）；若终端占用 Shift+Tab 或转发不可靠，可改回 `ctrl-tab`，或用 `ctrl-space` / `ctrl-backslash`：

```toml
[keys]
mode_switch = "ctrl-space"
```

该值在**启动时**生效（输入读取器只创建一次）。下表中的 Shift+Tab 均指当前配置的 mode_switch 键。

### 提示符标记（常驻标识）

主屏方案下 shell 提示符由 shell 自绘，"正在 rysh 里"靠常驻标记标识。`[tui] prompt_marker` 缺省 `auto`（开启），两层：

- **终端标题**（所有 shell）：运行期间标题恒为 `rysh · <model> · session <id>`——rysh 会过滤掉子 shell 自己发的 `OSC 0/2` 标题序列（纯带外，屏幕与其余透传字节不受影响），`/model`、切换会话后随之更新；退出时尽力恢复启动前读到的原标题（`OSC 1046` 探测，终端不支持就不动，Windows 控制台只设不恢复）。
- **bash 提示符前缀**（仅 bash 子 shell）：rysh 向子 shell 的环境导出一个 `PROMPT_COMMAND` 一行——bash（login 与否都）从环境导入 `PROMPT_COMMAND` 并在每个提示符前执行，该一行把暗色 `(rysh) ` 幂等地前置到 PS1（包在 PS1 非打印标记里，readline 光标计算不受影响）。不改用户任何配置文件；启动效果：`(rysh) (base) user@host:~$`。
  - **尽力而为的边界**：用户自己的 rc（starship / direnv / nvm 等）若自己给 `PROMPT_COMMAND` 赋值，用户值胜出，标记静默退场——绝不破坏用户的提示符。这种情况想要前缀，按下面 `RYSH_INSIDE` 的方式在自己的 rc 里配置。
- **zsh 提示符前缀**（仅 zsh 子 shell）：zsh 没有 rcfile 参数也没有 `PROMPT_COMMAND`，唯一的环境注入点是 `ZDOTDIR`——rysh 把子进程的 `ZDOTDIR` 指向一个只含单个 `.zshenv` 的私有临时目录：该文件注册 precmd 钩子（把暗色 `(rysh) ` 幂等地前置到提示符，包在 `%{...%}` 非打印标记里，光标计算不受影响），随后把 `ZDOTDIR` 指回用户真实 dotdir（外层 `ZDOTDIR`，没有则 `$HOME`）并代跑用户的真 `.zshenv`（重定向会把它遮住）；用户的 `.zprofile` / `.zshrc` / `.zlogin` / 补全缓存照常解析，不改任何配置文件，rysh 退出时删除临时目录。钩子先于用户 rc 注册，所以用户自己的 precmd 钩子（starship / powerlevel10k 等）若整体重写提示符，用户胜出、标记静默退场。
- **fish / 其他 shell**：没有可靠的注入点，只做标题层；想要提示符前缀可在自己的 rc 里按 `RYSH_INSIDE` 条件化修改（rysh 启动子 shell 时导出 `RYSH_INSIDE=1`，裸终端里没有该变量）：

  ```bash
  if [ -n "${RYSH_INSIDE:-}" ]; then
    PS1="(rysh) $PS1"
  fi
  ```

设为 `"off"` 恢复原零侵入行为：标题不接管、`OSC 0/2` 原样透传、不注入 `PROMPT_COMMAND`、不重定向 `ZDOTDIR`：

```toml
[tui]
prompt_marker = "off"
```

### TUI 偏好

`[tui] cursor_style` 曾用于设置 shell 模式下的终端光标形状（DECSCUSR）：主屏方案去掉底部状态行后，rysh 不再改写终端光标，该键**仍被解析但不再有任何效果**：

```toml
[tui]
cursor_style = "bar"     # 已失效：rysh 不再发送 DECSCUSR
# cursor_style = "block"
# cursor_style = "default"
```

保留该键只是为了不改动已文档化的配置面：未知取值过去和现在都被静默忽略。

### AI 提示行

`[ai] prompt` 设置 AI 模式的输入提示行（bash-PS 风格，只影响 rysh 自己绘制的提示行，不改 shell 的 PS）。转义：`\u` 用户、`\h` 主机名、`\w` 当前目录（HOME 显示为 `~`）、`\m` 当前模型（如 `hy3/hy3`）、`\t` 时间（HH:MM:SS）、`\\` 字面反斜杠、`\[...\]` 非打印区（不占宽度）、`\x1b` / `\e` 转义字节（让 SGR 颜色序列生效）。缺省是暗物品红当前目录 + 暗色 `· <model>` + 品红 `[AI]:` 标记——冒号收尾让它读起来像自然语言输入框，而不是 shell 提示符：

```toml
[ai]
prompt = "\\[\\x1b[1;35m\\]\\w\\[\\x1b[0m\\] \\x1b[2m· \\m\\x1b[0m \\x1b[35m[AI]:\\x1b[0m "
```

未配置模型时缺省模板省掉 `· <model>` 段（不留悬空分隔符）。主屏下没有常驻状态行，这行提示词是 rysh 显示当前模型的唯一位置，`/model` 切换后随下一次绘制更新。**注意**：TOML 字符串里反斜杠要写成双反斜杠，否则 `\u`（unicode 转义）等会解析失败：

该值**每次进入 AI 模式**生效（模式切换时重读配置）。

### 会话日志

每个会话对应 `~/.rysh/sessions/<id>/messages.log` 一条磁盘流（append-only JSONL，事件类型：`sys` 会话/模式/任务事件、`shl` shell 输出行、`shk` shell 命令、`usr` AI 提问、`rea`/`asw` 推理/回复、`tool` 工具调用、`noti` 通知、子智能体流 `srea`/`sasw`/`stool`/`snoti`/`sub`（M7.7，带 `[task N]` 标注，只入日志、重建历史时不回流上下文）；单文件超过约 1MB 后自动轮转，保留两代：`messages.log.1.gz` / `messages.log.1`）。v6 起一次运行可先后持有多个会话（`/new`/`/resume`/`rysh resume` 切换，见「多会话」）：切换不删、不改任何会话的日志，全局台账 `~/.rysh/created`（`<时间> <编号> <id>`）与 `~/.rysh/updated`（`<时间> <id>`）记录会话清单与最近使用顺序（`/ls`、`rysh ls` 的编号即来自 created）。`$RYSH_SESSION_ID` 可覆盖会话 id（供测试等确定性场景）。日志是会话在磁盘上的真相来源，屏幕只是它的实时窗口。

### Agent 上下文

`[agent] env_allowlist` 配置暴露给模型的 shell 环境变量白名单（每次提问作为 `env:` system 消息前置）。缺省时用内置默认白名单（`HOME` / `USER` / `SHELL` / `TERM` / `LANG` / `EDITOR` / `PATH` 等常用项）；一旦显式配置，则**精确替换**默认列表——白名单外的变量（密钥、令牌）不进入模型上下文：

```toml
[agent]
env_allowlist = ["HOME", "USER", "PATH", "MY_PROJECT_VAR"]
# env_allowlist = []   # 空数组 = 完全不暴露任何环境变量
```

该值**每次提问**生效（配置按请求重读，与 `/model` 相同）。注意：环境读取走 `/proc/<pid>/environ`，只能看到 shell **启动时**的环境，会话内新 `export` 的变量不在其中（需 shell hook / OSC 上报，超出当前范围）。

### 权限审批

`[agent] approval` 控制 AI 任务执行工具的权限门：`"ask"`（缺省）写操作与未列入安全分类器的命令逐次询问，`"auto"` 全自动零打扰（等价无门控行为）：

```toml
[agent]
approval = "ask"   # "auto" = 不询问直接执行
```

该值**每次任务开始**生效（改配置即对下一个任务生效，无需重启）。ask 模式下读类工具与命中安全分类器的 bash 直接放行；其余弹出 `? 运行 <命令>（y 是 / n 否 / a 总是）`——`y` 本次放行、`n` 拒绝（拒绝文本回喂模型，任务继续）、`a` 放行并记会话级规则（bash 命令前缀 / 文件工具按工具名，规则仅存内存，重启即清）。答完 y/a 后命令真正执行，条下方显示 `⠋ 执行中...`（到 `[tool]` 结果行落地为止）——执行慢时它一直在动，不是按键没被接收。

## 按键（M2 / M3 部分）

| 按键 | 行为 |
| --- | --- |
| `Shift+Tab` | 双向切换 shell / AI 模式。shell 与 AI 共用同一条输入行：进入 AI 时 shell 未提交的命令带进 AI 输入行（shell 侧清空），退出时 AI 草稿回注到 shell 命令行（不带回车，回车后执行）；任一侧提交或清空都会消耗这一行。草稿以 `!` 开头时（AI 模式误输入的 shell 命令，见下行），切换会去掉行首 `!` 再回注 |
| `空格`（行首） | 双向切换 shell / AI 模式（首空格被吞掉，不会进入 shell） |
| `Esc` | AI 模式下清空草稿（继续留在 AI；不再用于返回 shell） |
| `←` / `→` / `Home` / `End` / `退格` | AI 模式下移动编辑光标 / 删除 |
| `↑` / `↓`（`Ctrl+P` / `Ctrl+N`） | AI 模式下轮换已提交的用户输入：自最近一条起依次选中上一条/下一条并回填草稿（可编辑后再提交）；继续编辑即脱离轮换，越过最近一条回到当前草稿 |
| `Ctrl+Z` | AI 模式下撤销最近一次草稿编辑（输入 / 删除 / 清空），可连续撤销 |
| `Enter`（草稿以 `/` 开头） | 提交为斜杠命令（`/model` `/new` `/ls` `/history` `/resume` `/help` `/quit`，见「多会话」），草稿清空，仍留在 AI 模式 |
| `Enter`（自然语言草稿） | 提交为提示发送给当前模型，回复流式渲染在共享流上；完成后留在 AI 模式可继续追问 |
| `Enter`（草稿以 `!` 开头） | 不发给模型——这是误输入到 AI 模式的 shell 命令：草稿保留，提示两条出路（按模式切换键切到 shell 并保留输入、去掉行首 `!` 后执行；或清空草稿后行首空格切换到 shell 再输入） |
| `Enter`（草稿为空） | no-op |
| `Ctrl+C`（任务流式渲染中） | 取消当前任务（流式期间唯一有效的键；模式切换被锁定） |
| `Tab` | AI 模式下补全首词：`/` 补全斜杠命令（多匹配先列出再静默循环）；`/model `、`/resume ` 后显示编号列表（模型列表/会话列表）供直接输编号；配置文件在磁盘上改动后（热加载），同草稿再按 Tab 会重新列出 |

AI 与 shell 共享同一屏幕、同一流：rysh 全程留在终端主屏（不进备用屏、不裁剪滚动区），进入 AI 模式不清屏、不重建 shell 画面，只在原地把 shell 提示行换成 AI 提示行；shell 与 AI 共用同一条输入行，切换时文本与光标随行延续（shell 敲到一半的命令进 AI 草稿，AI 草稿切回时回注 shell 命令行，见上方按键表）。误敲进 AI 模式的 `!` 开头命令不会执行、也不会发给模型：提交时给出提示，按模式切换键即可带着这条命令（去掉行首 `!`）切回 shell 执行。所有输出都进入终端原生 scrollback，`Shift+PgUp` / 鼠标滚轮可随时回看，rysh 退出后内容也原样留在终端。没有常驻状态行：当前模型写在 AI 提示行上（`cwd · model · [AI]:`），`/model` 以编号列表列出全部可用模型（当前模型行首标 `>`，与会话列表一致）、`/model <编号>` 或 `/model <provider/model>` 立即切换并随下一次绘制更新；流式期间的等待状态挂在内容**末行**（前后各留一空行；等模型 token 为 `⠋ 正在思考...`，工具执行为 `⠋ 执行中...`——审批条答 y/a 之后到 `[tool]` 结果行落地之间同样是它），首个 token 到达即被正文替换；忙碌期（启动 spinner、从思考 spinner 到流式正文）隐藏终端光标、任务在新鲜提示符落地时恢复显示——spinner 与流式正文各自就是动态焦点，光标不必标记焦点或输入位。AI 模式下 pty 输出暂不转发到屏幕（缓冲 + 记入会话日志，返回 shell 时一次性刷新）；shell 模式下运行全屏程序（vim 等）时 rysh 暂停记录，退出后回到主屏原样续流，不做任何界面重建。在 shell 里输入 `exit`（或子 shell 退出）时，rysh 只做轻量复位（颜色与光标可见性），不清屏，把带完整历史的终端交还给外层 shell。

## AI 对话（M3 / M4 会话上下文）

在 AI 模式输入自然语言后回车，即发给当前模型（OpenAI 兼容 `chat/completions` SSE 流式接口），回复流式渲染在共享流上：先显示灰色标题 `─── <model> ───`，推理内容（`reasoning_content`）以暗色逐 token 追加、回复正文按 markdown 渲染（标题、加粗/斜体、内联代码、链接、列表、引用、围栏代码块均带样式，分块流式时未闭合的标记不会闪现），工具调用显示为灰色 `[tool] $ <命令> (exit <N>)` 行。生成期间按 `Ctrl+C` 立即取消（流式期间模式切换被锁定，仅 `^C` 有效）：请求被中断、恢复可输入（可继续追问）。回复结束后回到空草稿，可继续追问；`/model` 切换模型后对话继续使用新模型。多轮对话自带记忆：每次提问都会把此前的对话历史一并发给模型（上限 120 条消息，按整轮裁剪最旧内容），记忆按会话隔离——`/new` 新建并切换到全新会话（空历史，见「多会话」），`/resume` 切回旧会话即找回原有记忆，任何切换都不清空、不覆盖磁盘日志。

每次请求会把当前工作目录（cwd）和 shell 环境作为 `system` 消息一并发送（Linux 经 `/proc/<child>/cwd` 与 `/proc/<child>/environ` 读取，非 Linux 优雅降级为不发送），因此 AI 模式能感知 shell 模式下 `cd` 之后的位置和常用环境变量。环境只暴露白名单（`HOME`/`USER`/`SHELL`/`TERM`/`LANG`/`EDITOR`/`PATH` 等），密钥类变量（API key、token）不会进入模型上下文；`/proc` 只能读到 shell 启动时的环境，会话内新增的 `export` 不在其中。

最近在 shell 模式里执行的命令与输出也会进入 AI 上下文（M4 统一事件流）：rysh 记录每条命令（命令文本 + 跟随的终端输出 + 执行时的 cwd），每次提问时这些事件与 AI 对话历史**按发生时间交错合并**成一条时间线发送——每条事件是独立的 `system` 消息（`cwd: <dir>\n$ <cmd>\n<output>\n---`），两轮 AI 对话之间执行的命令就落在两轮之间，先后顺序不丢失（上限 20 条事件、单条输出 4KB、事件部分总长 8KB，ANSI 序列剥离）。因此切到 AI 模式后可以直接追问"我刚才那条命令输出里 xxx 是什么意思"。

### Agent 工具调用（M4）

模型返回原生 `bash` 工具调用时，rysh 会以 shell 的当前 cwd 用独立执行器运行该命令（非交互、stdin 关闭），捕获 stdout/stderr/exit_code，并在屏幕上方显示一行 `[tool] $ <命令> (exit <code>)` 观察；命令产出作为 `role:"tool"` 消息自动回喂给模型并再次调用，直到模型给出最终回答（同一任务共用一条 `─── <model> ───` 标题）。只执行原生工具调用：回复正文里的代码块（含 ` ```bash `）仅作展示、永不执行，所以模型只是"给用户看"一条命令时不会触发执行。工具轮数**不设上限**，防止发疯交给 loop guard 循环防护：同一签名（命令全文+退出码+输出前 8KB 的哈希）连续重复 ≥6 次、或在饱和的滑动窗口（20 条）中占比 >60% 时判定失控——屏幕显示暗色 `─── wrapping up (loop guard) ───` 并注入收尾指令，强制模型基于已有信息输出文字总结；连续重复 ≥2 次起还会在回喂结果尾部附加「请改变方法」警告促其自纠。上下文侧防膨胀：每次组装请求时，把超过 64KB 尾部预算的更早工具输出折叠为一行 `[已省略] $ …已折叠` 占位符（最新结果与预算内的旧结果保留原文），长任务的请求体积有界而历史不失忆。为防单点失控另有兜底：单流输出上限 32KB 截断、单命令 30s 超时并杀死整个进程组。每次请求前置一条 `system` 指令消息。

> 当前限制：命令执行采用独立 `bash -c` 子进程而非共享 pty（继承登录 shell 环境/交互式执行待后续）；shell 命令记录的近似性：记录用户实际敲入的字节（readline 历史回填/补全的最终命令不可见）、输出含随后的提示符、shell 模式下命令退出码不捕获（工具调用已捕获）；全屏程序（占用备用屏，如 vim）运行期间记录暂停——其按键与屏幕输出不进事件流，退出瞬间的重绘不归属任何命令，回到 shell 后恢复记录。

## 多会话（v6）

`rysh` 单进程只跑一个 pty，但可持有多个 AI 会话：每个会话是 `~/.rysh/sessions/<id>/` 下的一条磁盘流，`/ls` 的编号来自全局台账 `~/.rysh/created`（新会话未命名时显示 `新会话`）。切换会话 = 终止当前登录 shell（连同它派生的全部子进程）→ 在同一个 pty 上重启登录 shell（重放 profile）→ 打一条分隔行再接该会话的尾部回放（不清屏、不全量重绘，见上「主屏方案」条）——草稿、AI 历史、shell 事件环等每会话状态随之隔离重置，磁盘日志永不覆盖。

会话操作三条路等价：

| 入口 | 命令 | 说明 |
| --- | --- | --- |
| AI 斜杠命令 | `/ls [页号]` · `/new` · `/resume <编号或id>` | 分页列出（`>` 标当前，`[rysh <pid>]` 尾缀表示被其他 rysh 进程附加）/ 新建并切换 / 切换 |
| shell 或任意终端 | `rysh ls [页号]` · `rysh new` · `rysh resume <编号或id>` · `rysh kill <编号或id>` | rysh 内执行时是一次性子进程（不嵌套 pty），`new`/`resume` 经控制通道让当前实例执行切换 |
| 顶层启动形态 | `rysh new` · `rysh resume <标识>` · `rysh ai` | 直接以新建/指定会话进入，或启动即 AI 模式 |

其他行为：

- **附加互斥**：一个会话同一时刻只能被一个 rysh 实例附加；被附加的会话拒绝切换并提示可 `rysh kill`（对附加者 SIGTERM → 5s → SIGKILL）。
- **一次性问答**：`rysh ai "<消息>"` 加载 `$RYSH_SESSION_ID` 所指会话的历史，发送一条消息、流式打印回复后退出。
- AI 模式内 `/help` 列出全部斜杠命令；`/quit`、`/exit` 退出 rysh；任务流式期间收到的切换请求会在任务收尾后落地。

## 许可证

MIT——见 [LICENSE](LICENSE)。
