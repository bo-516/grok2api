# agent-mock

本地开发用的 OpenAI Chat Completions 兼容服务。每个请求启动一次你本机已登录的 `grok`，把回复转成 `/v1/chat/completions`。

这是开发替身，不是生产服务。提示词会经 grok 发到 xAI。不要发送真实用户数据、密钥或未公开的代码。

## 准备

1. 安装 [Grok Build](https://x.ai) CLI，并执行一次 `grok login`（SuperGrok / X Premium+，按你自己的订阅）。
2. 不需要 OpenAI 或 xAI 的按量 API Key。子进程会去掉 `XAI_API_KEY`，避免静默改走按量计费。

```bash
go install github.com/shaoboli/agent-mock/cmd/agent-mock@latest
agent-mock
```

启动后终端会打印 grok 路径、登录状态、模型、工具集检查，以及要贴进后端的两行环境变量：

```text
OPENAI_BASE_URL=http://127.0.0.1:8787/v1
OPENAI_API_KEY=dev
```

`temperature`、`top_p`、`max_tokens`、`max_completion_tokens`、`stop`、`seed` 和 penalty 会被接受但不会传给 grok。响应头 `X-Agent-Mock-Ignored` 列出它们。`n>1` 和 `logprobs` 返回 400。

## 安全

每次运行都使用固定的空目录、`--verbatim`、`--max-turns 1`、`--permission-mode dontAsk`，并且工具列表被收成空。流式运行的第一条记录必须是 `tools: []`，否则进程会被杀掉并返回 500 `unsafe_grok_toolset`。

默认只监听 `127.0.0.1`。监听非 loopback 地址时必须设置 `-api-key`，否则进程以退出码 2 拒绝启动。

每次调用都有固定的提示词开销（大约数千 input tokens）和数秒延迟。并发默认 4。额度用尽时返回 429。后端如果会自动重试 429，建议把重试关掉。

## 常用参数

也可用环境变量 `AGENT_MOCK_<大写蛇形>`。两边都设置时，flag 优先。

| flag | 默认 | 含义 |
|---|---|---|
| `-addr` | `127.0.0.1:8787` | 监听地址 |
| `-api-key` | 空 | Bearer；非 loopback 时必填 |
| `-grok-bin` | `grok` | grok 可执行文件 |
| `-default-model` | grok 自己的默认 | 未知模型的回落 |
| `-model-map` | 无 | `gpt-4o=grok-4.7`，可重复 |
| `-reasoning-effort` | grok 自己的默认 | 传给 `--reasoning-effort` |
| `-max-concurrency` | 4 | 同时运行的 grok 数 |
| `-queue-timeout` | 30s | 排队超时后 429 |
| `-request-timeout` | 3m | 单次运行上限 |
| `-keep-sessions` | 关 | 保留 grok session |
| `-log-prompts` | 关 | 在访问日志里记录提示词 |

`-version` 打印版本后退出。

## 冒烟

```bash
agent-mock
./scripts/smoke.sh
OPENAI_BASE_URL=http://127.0.0.1:8787/v1 OPENAI_API_KEY=dev go run ./examples/go-openai
```

Go 后端只改 base URL 和 API key，继续用 `Chat.Completions.New` 和 `NewStreaming`。图片、音频和文件内容会返回 400。
