# 模型试卷 (Model Exam)

一套可复用的大模型能力 + 性能测试,用于任何 OpenAI 兼容端点(即 `rysh` 的
`internal/provider/openai.go` 所对接的那类服务)。脚本见同目录
[`model_exam.py`](./model_exam.py),只依赖 Python 3 标准库。

- **自动判分**:答案客观可判的题目(数学/逻辑/编码/JSON/推理/工具调用/连通性),脚本直接给 ✓/✗。
- **人工判分**:需要主观判断的题目(中文表达),脚本打印出来由你看。
- **性能**:吞吐(tok/s)、首字延迟(TTFT)、可选长任务。

---

## 运行方式

```bash
# 默认端点(127.0.0.1:8081 的 llama.cpp)
python3 scripts/model_exam.py

# 指定端点 + 模型
python3 scripts/model_exam.py --url http://<host>:10000/v1 --model Qwen3.8-27B

# 需要密钥的 provider(密钥从环境变量读,不打印)
python3 scripts/model_exam.py --url https://api.deepseek.com/v1 \
    --model deepseek-v4-flash --api-key-env DEEPSEEK_API_KEY

# 只测能力(跳过慢的性能块),快速回归
python3 scripts/model_exam.py --skip-perf

# 额外跑 500 词长文(更长的长任务信号,很慢)
python3 scripts/model_exam.py --long

# 自定义性能块目标词数
python3 scripts/model_exam.py --perf-words 300
```

退出码:自动判分题全过 = `0`,有失败/错误 = `1`,无法自动识别模型 = `2`。
因此可直接放进 CI / 脚本做回归。

---

## 试题与判分标准

| # | 题号 | 考什么 | 提示词(要旨) | 标准答案 / 判分 | 判分方式 |
|---|------|--------|--------------|----------------|----------|
| 1 | health | 连通性 | `GET /health` | `{"status":"ok"}` / HTTP 200 | 自动 |
| 2 | models | 可枚举模型 | `GET /models` | 至少返回 1 个 id | 自动 |
| 3 | 数学 | 多步算术 | 算 `123456×789÷3`,只回整数 | **32468928** | 自动 |
| 4 | 逻辑 | 关系推理 | Sue>Mike>John;Tom 在 Sue 与 Mike 之间,谁最年轻? | **John** | 自动 |
| 5 | 编码 | 写可运行代码 | 写 Python `fib(n)`(fib(0)=0) | 脚本**实际执行** `fib(0..11)` = `0 1 1 2 3 5 8 13 21 34 55 89` | 自动(执行验证) |
| 6 | JSON | 结构化输出 | 只回 JSON:`{"city","country"}` for 北京 | 可 `json.loads` 且含 city/country 键 | 自动 |
| 7 | 中文 | 中文理解/表达 | 用 ≤30 字解释闭包 | 语义正确即可(参考:「闭包让函数记住并访问其定义时的变量」) | **人工** |
| 8 | 推理 | 应用题/思维链 | 60km/h 与 2 小时后 90km/h 同向,何时追上? | **4 小时**(120÷30) | 自动(找 `Answer: 4`) |
| 9 | 工具-单轮 | 原生函数调用 | "列出当前目录文件" + 工具 `run_command` | 发出 `run_command({"command":"ls …"})`,参数为合法 JSON | 自动 |
| 10 | 工具-多轮 | 工具结果回传解读 | 先调 `df -h`,回传结果后追问空闲空间 | 回复里正确使用回传的 **300G** | 自动 |

**总分 = 自动判分 8 题(1、2、3、4、5、6、8、9、10 中去掉人工的第 7 题,共 8 个自动项)**;
第 7 题中文为人工判分,不计入自动分。成绩单底部会打印 `自动判分: X/8`。

---

## 性能基线(2026-09-02 实测,llama.cpp@8081,Qwen3.8-27B)

| 配置 | 吞吐 | 相对基线 | 说明 |
|------|------|----------|------|
| 31.4 GB 文件(旧) | ~28 tok/s | 1.0× | 大文件,疑似未整卡入显存(有 CPU offload) |
| 17.5 GB 文件,MTP 关 | **~45 tok/s** | ~1.6× | 17.5 GB 是 27B Q4 正常体量,整卡放下 |
| 17.5 GB 文件,MTP 开 | **~96 tok/s** | ~3.4× | MTP(多 token 预测/投机解码)再叠加 ~2.1× |

- 吞吐口径:`completion_tokens / 总耗时`(含 prefill + 思考 token),300 词段落、不带 max_tokens,可直接对比。
- TTFT(首字延迟)约 **0.7s**,很快。
- MTP 为**无损加速**(不改权重、不降输出质量),建议保持开启。

> 结论:3.4× 提速 ≈ 文件换小(×1.6) × MTP(×2.1)。两者都有效,MTP 占比更大。

---

## 备注 / 踩过的坑

1. **它是 Qwen3 thinking 模型**:每次回答前都会先输出 `reasoning_content`(思考),再给 `content`。
   - 测试**不要设小 `max_tokens`**——否则思考阶段就把预算用完,`content` 会返回空(这是脚本默认**不发 max_tokens** 的原因,与 rysh 行为一致)。
   - 想省 token / 提速可用 `/no_think` 软提示缩短思考,但只能缩短、不能完全关掉;要彻底关需在服务端(chat-template / `enable_thinking`)处理。
2. **工具调用偶发空响应**:思考型模型偶尔一整轮都在思考、最后既不输出内容也不发 tool call。试卷里工具调用题因此**最多重试 3 次**(temp=0),任一次成功即 PASS 并标注尝试次数;3 次全空才判 FAIL。
   - 注意:`temp=0` 下仍会偶发(同一模型几分钟内可能一会儿 3/3 全空、一会儿 5/5 全成),**很可能是服务端 MTP 投机解码的非确定性 + 思考阶段卡住**导致,而非端点不支持原生工具。看到工具题 FAIL 时,建议单独多跑几次确认,别直接判为不支持。
3. **长任务慢**:500 词文章 ≈ 8000 token,在 45 tok/s 下约 3 分钟;MTP 开时约 1 分多。交互式短任务(几秒)不受影响。
4. **rysh 侧兼容**:rysh 读取 `reasoning_content`(见 `openai.go`)并支持原生 tool calling,两条路径本试卷都覆盖。
5. **端点无鉴权**(8081/8080/sglang 内网服务):`--api-key-env` 留空即可;外网 provider 才需要密钥。
