# grok2api

本地开发用的 OpenAI Chat Completions 兼容服务。仓库：<https://github.com/bo-516/grok2api>。

每个请求启动一次你本机已登录的 `grok`，把回复转成 `/v1/chat/completions`。请求里的 `model` 固定写 `superllm`。

这是开发替身，不是生产服务。提示词会经 grok 发到 xAI。不要发送真实用户数据、密钥或未公开的代码。

## 准备

1. 安装 [Grok Build](https://x.ai) CLI，并执行一次 `grok login`（SuperGrok / X Premium+，按你自己的订阅）。
2. 安装 Go 1.25。
3. 不需要 OpenAI 或 xAI 的按量 API Key。子进程会去掉 `XAI_API_KEY`，避免静默改走按量计费。

```bash
git clone https://github.com/bo-516/grok2api.git
cd grok2api
go run ./cmd/agent-mock
```

要装到 `PATH` 里，在仓库目录执行 `go install ./cmd/agent-mock`，然后直接运行 `agent-mock`。

启动后终端会打印 grok 路径、登录状态、模型，以及下面两行环境变量：

```text
OPENAI_BASE_URL=http://127.0.0.1:8787/v1
OPENAI_API_KEY=dev
```

调用时 `model` 固定写 `superllm`。用法也可以直接读：

```bash
curl -sS http://127.0.0.1:8787/doc
```

`temperature`、`top_p`、`max_tokens`、`max_completion_tokens`、`stop`、`seed` 和 penalty 会被接受但不会传给 grok。响应头 `X-Agent-Mock-Ignored` 列出它们。`n>1` 和 `logprobs` 返回 400。

## 调用

普通文本、流式、`json_schema` / `json_object`、工具调用都走 `POST /v1/chat/completions`。

工具请求使用 `tools` 和 `tool_choice`（`auto`、`none`、`required`，或指定函数）。需要调用时 `finish_reason` 为 `tool_calls`，参数在 `function.arguments` 字符串里。把这条 assistant 消息原样放回，再追加 `role: "tool"` 的结果，然后再次 POST。

`strict: true` 时，参数 schema 必须是 object，`additionalProperties` 为 false，并且每个字段都在 `required` 里。旧参数 `functions` / `function_call` 也可以用，不要和 `tools` 同时写。

仓库里的对话例子是 `examples/go-openai`。对话里的图片、音频和文件内容仍返回 400。出图和出视频走下面的单独接口。

## 图片与视频

对话运行仍然是零工具。图片和视频各开一次 grok，只放开这一次需要的媒体工具（`image_gen`、`image_edit` 或 `reference_to_video`）。init 的工具集必须正好等于这次的白名单，模型给出的每个工具参数都会再检查一遍。不合规则杀掉进程组，这次请求不会返回产物。

图片字段与 OpenAI Images 相同。仓库里的例子是 `examples/go-media`：

```bash
curl -sS http://127.0.0.1:8787/v1/images/generations -H 'Content-Type: application/json' \
  -d '{"prompt":"A red apple","size":"1792x1024"}'
```

`n` 为 1 到 4。`size` 和 `aspect_ratio` 只能给一个。`response_format` 默认 `url`，指向 `GET /v1/media/<name>`，这个地址不要求 Bearer。编辑接受 multipart 的 `image` / `image[]`（最多 5 张），也接受 JSON 的 `image.url` 或 `images[].url`。来源可以是上传文件、`data:image/png|jpeg|webp;base64`、本服务自己的 `/v1/media` 链接，或公网 `https` 链接。每张不超过 20 MiB。

视频用 xAI 的异步形状。`POST /v1/videos/generations` 立刻返回 `request_id`，再 `GET /v1/videos/<id>` 直到 `done`。`duration` 为 1–15，默认 8。没有图片、参考图、关键帧和音色时，同一次运行里先出首帧再出视频。可复制的请求在 `GET /doc` 第 6、7 节。能跑的例子：`./examples/media.sh`（curl）和 `examples/go-media`（Go）。

`quality`、`style`、`background`、`output_compression`、图片 `resolution` 和 `user` 会被接受但不会改变画面，并出现在 `X-Agent-Mock-Ignored`。`1080p` 按 `720p` 生成。产物和视频任务只保留 `-media-ttl`（默认 1 小时），重启后即失效。

媒体运行使用 `--permission-mode bypassPermissions`。grok 1.0.41 在 `dontAsk` 下会取消 `image_gen`。白名单校验仍然拒绝任何名单之外的工具，所以自动批准的只有这次列出的媒体工具。对话运行仍是 `dontAsk`。

关掉媒体：`-media=false`。五个媒体路由都返回 404 `media_disabled`，启动时也不探测媒体工具。对话不受影响。

## 安全

对话运行使用固定的空目录、`--verbatim`、`--max-turns 1`、`--permission-mode dontAsk`，并且工具列表被收成空。流式运行的第一条记录必须是 `tools: []`，否则进程会被杀掉并返回 500 `unsafe_grok_toolset`。

媒体运行用单独的 cwd，`--tools` 只有这次的白名单，`--disallowed-tools` 为 `search_tool,use_tool`。工具调用里的图片路径只能是本请求暂存的输入，或这次运行自己产出的文件。越界路径返回 500 `unsafe_grok_tool_input`，并且不会把文件放进媒体仓库。关闭方式是 `-media=false`。

默认只监听 `127.0.0.1`。监听非 loopback 地址时必须设置 `-api-key`，否则进程以退出码 2 拒绝启动。

请求正文只写进权限 0600 的临时文件，运行结束就删除。grok 自己还会留下两样东西：每次运行一个 session，以及 `~/.grok/sessions/<URL 编码的 cwd>/prompt_history.jsonl`，里面逐条追加提示词原文。`grok sessions delete` 不清理这个文件，grok 也没有关闭它的配置项。所以 agent-mock 默认：

- 用 `--session-id` 给每次运行指定 session ID。grok 还没打印 ID 就退出时，也能删掉这个 session。
- 运行结束后在后台执行 `grok sessions delete`。退出时（包括在终端按 Ctrl-C）会等这些删除完成。
- 每次运行后，删除这次 `--cwd` 对应的 `prompt_history.jsonl`。对话目录是 `$TMPDIR/agent-mock/cwd`，媒体目录是 `$TMPDIR/agent-mock/mcwd`。启动时会再删对话目录的那份，并补删上次进程没清完的目录。只动这两个目录，不碰其它 grok 历史。遵循 `GROK_HOME`。
- 在 `$TMPDIR/agent-mock/pending/` 记下尚未删掉的 session。删除失败时终端会打印一行说明。进程被强杀或删除失败留下的 session，下次启动时补删。

加 `-keep-sessions` 后，以上清理全部关闭，session 和提示词历史都保留，方便排查。

每次调用都有固定的提示词开销（大约数千 input tokens）和数秒延迟。并发默认 4。额度用尽时返回 429。收到 429 后等几秒再试，不要立刻自动重试。

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
| `-keep-sessions` | 关 | 保留 grok session 和提示词历史 |
| `-log-prompts` | 关 | 在访问日志里记录提示词 |
| `-media` | 开 | 提供图片、视频和 `/v1/media` |
| `-media-ttl` | `1h0m0s` | 产物和视频任务的保留时间 |
| `-video-timeout` | `10m0s` | 单次视频 grok 运行上限 |
| `-max-video-jobs` | 2 | 排队加运行中的视频任务达到此数后 429 |

`-version` 打印版本后退出。

## 冒烟

另开一个终端，在仓库目录里：

```bash
./scripts/smoke.sh
OPENAI_BASE_URL=http://127.0.0.1:8787/v1 OPENAI_API_KEY=dev go run ./examples/go-openai
```
