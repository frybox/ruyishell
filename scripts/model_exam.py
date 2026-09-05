#!/usr/bin/env python3
"""模型试卷 (Model Exam)
=======================
Reusable capability + performance test for any OpenAI-compatible endpoint
(the same shape rysh's `internal/provider/openai.go` speaks). Stdlib only.

It runs a fixed battery of questions, auto-grades the ones with objective
answers, shows the ones that need human judgment, and measures throughput
and time-to-first-token. Results are printed as a score sheet.

Examples
--------
  # Default endpoint (the 8081 llama.cpp server)
  python3 model_exam.py

  # Any endpoint + explicit model
  python3 model_exam.py --url http://<host>:10000/v1 --model Qwen3.8-27B

  # A key-protected provider (reads the key from an env var, never prints it)
  python3 model_exam.py --url https://api.deepseek.com/v1 --model deepseek-v4-flash \
      --api-key-env DEEPSEEK_API_KEY

  # Fast capability-only run (skip the slow performance block)
  python3 model_exam.py --skip-perf

  # Also run the slow 500-word essay (extra long-task signal)
  python3 model_exam.py --long

Exit code: 0 if every auto-graded question passes, 1 otherwise (errors
count as failures), so it is usable in CI/scripts.
"""
import argparse
import datetime
import json
import os
import subprocess
import sys
import tempfile
import time
import urllib.request

DEFAULT_URL = "http://127.0.0.1:8081"


# --------------------------------------------------------------------------- #
# Endpoint client
# --------------------------------------------------------------------------- #
class Endpoint:
    def __init__(self, url, model=None, api_key=None):
        self.base = url.rstrip("/")
        if not self.base.endswith("/v1"):
            self.base = self.base + "/v1"
        self.api_key = api_key
        self.model = model  # may be None -> auto-detected before use

    def _auth(self):
        h = {"Content-Type": "application/json"}
        if self.api_key:
            h["Authorization"] = "Bearer " + self.api_key
        return h

    def get(self, path, timeout=30):
        req = urllib.request.Request(self.base + path, headers=self._auth())
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, json.loads(r.read().decode())

    def chat(self, messages, temp=0.2, tools=None, stream=False, timeout=600):
        """Returns (message_or_dict, usage_or_None, elapsed).

        Non-stream: ({"content":..,"reasoning_content":..}, usage, dt)
        Stream:     ({...,"_ttft_any":..,"_ttft_content":..}, None, dt)
        No max_tokens is sent (matches rysh): a thinking model is allowed to
        run to its natural stop token.
        """
        body = {"model": self.model, "messages": messages, "stream": stream,
                "temperature": temp}
        if tools:
            body["tools"] = tools
            body["tool_choice"] = "auto"
        data = json.dumps(body).encode()
        req = urllib.request.Request(self.base + "/chat/completions",
                                     data=data, headers=self._auth())
        t0 = time.time()
        if not stream:
            with urllib.request.urlopen(req, timeout=timeout) as r:
                j = json.loads(r.read().decode())
            dt = time.time() - t0
            m = j["choices"][0]["message"]
            return ({"content": m.get("content", "") or "",
                     "reasoning_content": m.get("reasoning_content", "") or ""},
                    (j.get("usage") or {}), dt)
        first_any = first_content = None
        content, reasoning = [], []
        with urllib.request.urlopen(req, timeout=timeout) as r:
            for raw in r:
                line = raw.decode(errors="ignore").strip()
                if not line.startswith("data:"):
                    continue
                payload = line[5:].strip()
                if payload == "[DONE]":
                    break
                try:
                    d = json.loads(payload)
                except Exception:
                    continue
                for ch in d.get("choices") or []:
                    dl = ch.get("delta") or {}
                    c, rc = dl.get("content"), dl.get("reasoning_content")
                    if c:
                        if first_content is None:
                            first_content = time.time() - t0
                        if first_any is None:
                            first_any = time.time() - t0
                        content.append(c)
                    if rc:
                        if first_any is None:
                            first_any = time.time() - t0
                        reasoning.append(rc)
        dt = time.time() - t0
        return ({"content": "".join(content), "reasoning_content": "".join(reasoning),
                 "_ttft_any": first_any, "_ttft_content": first_content}, None, dt)


# --------------------------------------------------------------------------- #
# Report collector
# --------------------------------------------------------------------------- #
class Report:
    PASS, FAIL, ERROR, SHOW, INFO = "PASS", "FAIL", "ERROR", "SHOW", "INFO"

    def __init__(self):
        self.rows = []  # (name, status, detail)

    def add(self, name, status, detail=""):
        self.rows.append((name, status, detail))

    @property
    def scored(self):
        return [r for r in self.rows if r[1] in (self.PASS, self.FAIL, self.ERROR)]

    @property
    def passed(self):
        return [r for r in self.rows if r[1] == self.PASS]


def detect_model(ep):
    try:
        _, j = ep.get("/models")
    except Exception:
        _, j = ep.get("/v1/models")
    data = j.get("data") or j.get("models") or []
    if not data:
        return None, None
    d = data[0]
    meta = d.get("meta") or {}
    return d.get("id") or d.get("name"), meta


# --------------------------------------------------------------------------- #
# Test cases
# --------------------------------------------------------------------------- #
def t_health(ep, rep):
    try:
        status, _ = ep.get("/health")
        rep.add("health /health=ok", Report.PASS if status == 200 else Report.FAIL,
                f"http {status}")
    except Exception as e:
        rep.add("health /health=ok", Report.ERROR, repr(e)[:80])
    try:
        status, j = ep.get("/models")
        ids = [m.get("id") or m.get("name") for m in (j.get("data") or j.get("models") or [])]
        rep.add("models 可枚举", Report.PASS if status == 200 and ids else Report.FAIL,
                ", ".join(ids))
    except Exception as e:
        rep.add("models 可枚举", Report.ERROR, repr(e)[:80])


def t_math(ep, rep):
    want = "32468928"
    m, u, dt = ep.chat([{"role": "user",
                         "content": "Compute 123456 * 789, then divide the result by 3. "
                                    "Reply with ONLY the final integer."}])
    ok = want in m["content"]
    rep.add("数学 123456×789÷3", Report.PASS if ok else Report.FAIL,
            f'{m["content"].strip()[:60]!r}  expect {want}  ({dt:.1f}s, {u.get("completion_tokens")}tok)')


def t_logic(ep, rep):
    m, u, dt = ep.chat([{"role": "user",
                         "content": "Sue is older than Mike. Mike is older than John. "
                                    "Tom is younger than Sue but older than Mike. "
                                    "Who is the youngest? Reply with only the name."}])
    ok = m["content"].strip().lower() == "john" or "john" in m["content"].lower()
    rep.add("逻辑 谁最年轻", Report.PASS if ok else Report.FAIL,
            f'{m["content"].strip()[:40]!r}  expect John  ({dt:.1f}s)')


def t_coding(ep, rep):
    m, u, dt = ep.chat([{"role": "user",
                         "content": "Write a Python function fib(n) returning the n-th "
                                    "Fibonacci number (fib(0)=0, fib(1)=1). "
                                    "Return only the code, no explanation."}])
    src = m["content"].replace("```python", "").replace("```", "").strip()
    want = [0, 1, 1, 2, 3, 5, 8, 13, 21, 34, 55, 89]
    try:
        with tempfile.NamedTemporaryFile("w", suffix=".py", delete=False) as f:
            f.write(src + "\nprint(' '.join(map(str,[fib(i) for i in range(12)])))\n")
            path = f.name
        r = subprocess.run([sys.executable, path], capture_output=True, text=True, timeout=30)
        got = [int(x) for x in r.stdout.split()]
        ok = got == want
        detail = f"fib(0..11)={got}"
        if not ok and r.returncode != 0:
            detail += "  err:" + r.stderr.strip().splitlines()[-1][:80] if r.stderr else ""
    except Exception as e:
        ok, detail = False, repr(e)[:80]
    finally:
        try:
            os.unlink(path)
        except Exception:
            pass
    rep.add("编码 fib 实际执行", Report.PASS if ok else Report.FAIL, f"{detail}  ({dt:.1f}s)")


def t_json(ep, rep):
    m, u, dt = ep.chat([{"role": "user",
                         "content": 'Return valid JSON only (no markdown, no code fences): '
                                    'an object with keys "city" and "country" for 北京.'}])
    raw = m["content"].strip().strip("`").replace("```json", "").strip()
    try:
        obj = json.loads(raw)
        ok = isinstance(obj, dict) and "city" in obj and "country" in obj
        rep.add("JSON 格式(北京)", Report.PASS if ok else Report.FAIL,
                f"{raw[:60]!r}  ({dt:.1f}s)")
    except Exception as e:
        rep.add("JSON 格式(北京)", Report.FAIL, f"parse error {e}; raw={raw[:40]!r}")


def t_chinese(ep, rep):
    m, u, dt = ep.chat([{"role": "user",
                         "content": "用一句不超过30字的话解释什么是闭包（closure）。"}])
    rep.add("中文 解释闭包(人工判分)", Report.SHOW, f'{m["content"].strip()[:120]!r}  ({dt:.1f}s)')


def t_reasoning(ep, rep):
    m, u, dt = ep.chat([{"role": "user",
                         "content": "A train leaves Station A at 60 km/h. Two hours later a "
                                    "second train leaves the same station in the same direction "
                                    "at 90 km/h. After how many hours (from when the second train "
                                    "left) does it catch up? Show reasoning, then a final line "
                                    "'Answer: <n>'."}])
    ok = "Answer: 4" in m["content"] or m["content"].strip().endswith("Answer: 4") \
        or ("\n4" in m["content"] and "30" in m["content"])
    rep.add("推理 追及问题=4小时", Report.PASS if ok else Report.FAIL,
            f'content 末尾: {m["content"].strip()[-50:]!r}  ({dt:.1f}s)')


def t_tools(ep, rep):
    tools = [{"type": "function", "function": {
        "name": "run_command", "description": "Run a shell command.",
        "parameters": {"type": "object",
                       "properties": {"command": {"type": "string"}},
                       "required": ["command"]}}}]
    # A thinking model can spend a whole turn on reasoning and emit no tool
    # call (empty output). That is flaky, not "no support" — retry up to 3x
    # (temp=0 for determinism) before concluding it fails.
    tcs, dt, attempt = [], 0.0, 0
    for attempt in range(1, 4):
        m, u, dt = ep.chat([{"role": "user", "content": "List the files in the current directory."}],
                           tools=tools, temp=0.0)
        tcs = m.get("tool_calls") or []
        if tcs:
            break
    if tcs:
        fn = tcs[0]["function"]
        try:
            args = json.loads(fn["arguments"])
        except Exception:
            args = {}
        ok = fn.get("name") == "run_command" and "ls" in args.get("command", "")
        note = f"  (尝试 {attempt} 次)" if attempt > 1 else ""
        rep.add("工具调用 单轮 ls", Report.PASS if ok else Report.FAIL,
                f"{fn.get('name')}({fn.get('arguments')})  ({dt:.1f}s){note}")
    else:
        rep.add("工具调用 单轮 ls", Report.FAIL,
                "3 次均未发 tool call(思考型模型偶发空响应,或该端点不支持原生工具)")
        return
    # multi-turn: feed the tool result back, must use it
    m2, u2, dt2 = ep.chat([
        {"role": "user", "content": "How much free disk space is there? Check it."},
        {"role": "assistant", "content": "", "tool_calls": [tcs[0]]},
        {"role": "tool", "tool_call_id": tcs[0].get("id", "call_1"),
         "content": "Filesystem  Size  Used Avail Use%\n/dev/sda1   500G  200G  300G  40%"},
    ], tools=tools)
    ok2 = "300" in m2.get("content", "")
    rep.add("工具调用 多轮 df -h 解读", Report.PASS if ok2 else Report.FAIL,
            f'{m2["content"].strip()[:90]!r}  ({dt2:.1f}s)')


def t_perf(ep, rep, words=250, do_long=False):
    print("  ... 性能测试生成中(思考型模型较慢,请稍候) ...", file=sys.stderr)
    m, u, dt = ep.chat([{"role": "user",
                         "content": f"Write a detailed {words}-word paragraph on the "
                                    "history of the printing press. Just the paragraph."}],
                       temp=0.7)
    ct = u.get("completion_tokens", 0) or max(1, len(m["content"]) // 4)
    tok_s = ct / dt if dt > 0 else 0
    rep.add(f"吞吐 ~{words}词段落", Report.INFO,
            f"{ct} tok / {dt:.1f}s = {tok_s:.1f} tok/s  "
            f"(content {len(m['content'].split())}词, reasoning {len(m['reasoning_content'])}ch)")
    # TTFT via a fresh streaming call
    try:
        sm, _, sdt = ep.chat([{"role": "user", "content": "Count up from 1 to 60."}],
                             stream=True, timeout=120)
        ttft = sm.get("_ttft_content", sm.get("_ttft_any"))
        rep.add("首字延迟 TTFT", Report.INFO,
                f"{('%.2fs' % ttft) if ttft is not None else 'n/a (纯推理未出content)'}")
    except Exception as e:
        rep.add("首字延迟 TTFT", Report.ERROR, repr(e)[:60])
    if do_long:
        lm, lu, ldt = ep.chat([{"role": "user", "content": "Write a 500-word essay on the "
                                                          "history of the printing press."}],
                              temp=0.5, timeout=900)
        rep.add("长任务 500词文章", Report.INFO,
                f"{lu.get('completion_tokens')} tok / {ldt:.1f}s = "
                f"{lu.get('completion_tokens', 0) / ldt:.1f} tok/s  ({len(lm['content'].split())}词)")


# --------------------------------------------------------------------------- #
# Main
# --------------------------------------------------------------------------- #
def main():
    ap = argparse.ArgumentParser(description="模型试卷: capability + performance test")
    ap.add_argument("--url", default=DEFAULT_URL, help="endpoint base (e.g. http://host:port or .../v1)")
    ap.add_argument("--model", default=None, help="model id (default: auto-detect from /models)")
    ap.add_argument("--api-key", default=None, help="Bearer API key literal")
    ap.add_argument("--api-key-env", default=None, help="name of env var holding the API key")
    ap.add_argument("--perf-words", type=int, default=250, help="target word count for throughput test")
    ap.add_argument("--skip-perf", action="store_true", help="skip the (slow) performance block")
    ap.add_argument("--long", action="store_true", help="also run the 500-word essay")
    args = ap.parse_args()

    api_key = args.api_key
    if args.api_key_env:
        api_key = os.environ.get(args.api_key_env, "")
        if not api_key:
            print(f"!! env var {args.api_key_env} is not set", file=sys.stderr)

    ep = Endpoint(args.url, args.model, api_key)
    model, meta = detect_model(ep)
    if model is None:
        print("!! could not auto-detect model; pass --model", file=sys.stderr)
        sys.exit(2)
    if ep.model is None:
        ep.model = model

    rep = Report()
    print("  [1/5] 连通性 ...")
    t_health(ep, rep)
    print("  [2/5] 数学 / 逻辑 ...")
    t_math(ep, rep)
    t_logic(ep, rep)
    print("  [3/5] 编码 / JSON / 中文 ...")
    t_coding(ep, rep)
    t_json(ep, rep)
    t_chinese(ep, rep)
    print("  [4/5] 推理 / 工具调用 ...")
    t_reasoning(ep, rep)
    t_tools(ep, rep)
    if args.skip_perf:
        print("  [5/5] 性能: 已跳过 (--skip-perf)")
    else:
        print("  [5/5] 性能 ...")
        t_perf(ep, rep, words=args.perf_words, do_long=args.long)

    # ---- score sheet ----
    bar = "=" * 70
    print("\n" + bar)
    print("  模 型 试 卷 · 成 绩 单")
    print(bar)
    print(f"  endpoint : {ep.base}")
    size = meta.get("size") if meta else None
    ftype = meta.get("ftype") if meta else None
    npar = meta.get("n_params") if meta else None
    ctx = meta.get("n_ctx") if meta else None
    print(f"  model    : {model}   ({ftype or '?'}"
          + (f", {npar/1e9:.1f}B" if npar else "")
          + (f", file {size/1e9:.1f}GB" if size else "")
          + (f", ctx {ctx}" if ctx else "") + ")")
    print(f"  时间     : {datetime.datetime.now().strftime('%Y-%m-%d %H:%M:%S')}")
    print(bar)

    def line(name, status, detail):
        mark = {Report.PASS: "✓ PASS", Report.FAIL: "✗ FAIL", Report.ERROR: "✗ ERROR",
                Report.SHOW: "• SHOW", Report.INFO: "· "}[status]
        print(f"  {mark:<8} {name}")
        if detail:
            print(f"           {detail}")

    for name, status, detail in rep.rows:
        line(name, status, detail)

    scored = rep.scored
    npass = len(rep.passed)
    nfail = len([r for r in scored if r[1] in (Report.FAIL, Report.ERROR)])
    print(bar)
    print(f"  自动判分: {npass}/{len(scored)}  通过"
          + (f"   (失败/错误 {nfail})" if nfail else "   全部通过"))
    print("  人工判分: 中文题(上表 SHOW 项)请自行判断")
    print(bar)
    sys.exit(0 if nfail == 0 else 1)


if __name__ == "__main__":
    main()
