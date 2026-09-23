package server

import (
	"net/http"
	"strings"

	"github.com/shaoboli/agent-mock/internal/config"
)

// publicModel is the only model name callers should send.
// Other ids may appear from the backing runtime; the guide tells agents to ignore them.
const publicModel = "superllm"

// doc writes the caller guide. It does not require a bearer token, so an agent
// can read it before the first completion. The configured API key is included
// only for a loopback client, or when no key is configured (the value is then "dev").
// A remote client of a keyed server is told to send the key it was given, not the secret.
func (s *Server) doc(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	key, show := s.docKey(r)
	_, _ = w.Write([]byte(usageDoc(requestOrigin(r), key, show)))
}

// docKey picks the API key line for this client.
// show is false when a key is configured and the client is not on loopback,
// so the response must not contain Config.APIKey. An empty Config.APIKey
// returns "dev" and show true, matching the startup banner.
func (s *Server) docKey(r *http.Request) (key string, show bool) {
	if s.Config.APIKey == "" {
		return "dev", true
	}
	if loopbackRequest(r) {
		return s.Config.APIKey, true
	}
	return "", false
}

// loopbackRequest is true when the TCP peer is loopback.
// Host is ignored: the client can set it, RemoteAddr comes from the connection.
func loopbackRequest(r *http.Request) bool {
	if r == nil {
		return false
	}
	return config.IsLoopback(r.RemoteAddr)
}

// requestOrigin is scheme://host taken from the request, with no path.
// TLS or X-Forwarded-Proto: https selects https. A missing host falls back to 127.0.0.1:8787.
func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r != nil && (r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")) {
		scheme = "https"
	}
	host := ""
	if r != nil {
		host = r.Host
	}
	if host == "" {
		host = "127.0.0.1:8787"
	}
	return scheme + "://" + host
}

// usageDoc is the agent-facing guide for origin.
// showKey false omits apiKey and tells the caller to use the key it was given.
// origin has no trailing slash. The text is usage only: URLs, headers, and JSON bodies.
func usageDoc(origin, apiKey string, showKey bool) string {
	keyLine := "本服务要求 API key。请求头写：Authorization: Bearer <你拿到的密钥>"
	if showKey {
		keyLine = "OPENAI_API_KEY=" + apiKey + "\nAuthorization: Bearer " + apiKey
	}
	body := strings.ReplaceAll(usageDocText, "{{origin}}", origin)
	body = strings.ReplaceAll(body, "{{key}}", keyLine)
	return strings.ReplaceAll(body, "{{model}}", publicModel)
}

// usageDocText is the guide body. {{origin}} is scheme://host, {{key}} is the
// credential lines, and {{model}} is publicModel. No other placeholders are used.
const usageDocText = `superllm 调用说明

把下面两行配进 OpenAI 客户端。模型名固定写 {{model}}。

OPENAI_BASE_URL={{origin}}/v1
{{key}}

每次对话：
POST {{origin}}/v1/chat/completions
Content-Type: application/json
model 固定为 "{{model}}"。

1. 普通文本

{"model":"{{model}}","messages":[{"role":"user","content":"你好"}]}

回复正文在 choices[0].message.content。

2. 流式

{"model":"{{model}}","stream":true,"messages":[{"role":"user","content":"从 1 数到 5"}]}

响应是 SSE。每行 data: {JSON}，最后一行 data: [DONE]。
要在结束时拿到用量，加上 "stream_options":{"include_usage":true}。

3. 约束 JSON

要按 schema 返回时：

{"model":"{{model}}","messages":[{"role":"user","content":"法国首都是哪里？"}],"response_format":{"type":"json_schema","json_schema":{"name":"capital","schema":{"type":"object","additionalProperties":false,"required":["city"],"properties":{"city":{"type":"string"}}}}}}

choices[0].message.content 是 JSON 文本，按 schema 解析。
只要一个 JSON 对象、不限定字段时，response_format 用 {"type":"json_object"}。

4. 工具

请求里带 tools。每个工具：

{"type":"function","function":{"name":"get_weather","description":"查询天气","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}

tool_choice 可用 "auto"、"none"、"required"，或 {"type":"function","function":{"name":"get_weather"}}。
只要一个调用时加 "parallel_tool_calls":false。不写或写 true 时，指定了函数名也可以返回多次调用。
需要调用工具时 finish_reason 为 tool_calls，调用在 choices[0].message.tool_calls，参数是 function.arguments 字符串。
message.content 可能同时有一段说明；没有说明时 content 为 null。
流式时先给出函数名和空的 arguments，下一段才是完整 arguments 字符串，最后 finish_reason 为 tool_calls。
函数要严格符合 schema 时加 "strict": true。这时 parameters 必须是 object，additionalProperties 为 false，properties 里的每个字段都在 required 里，嵌套对象也一样，否则 400。
旧写法：用 functions 代替 tools，用 function_call 代替 tool_choice（"auto"、"none" 或 {"name":"get_weather"}）。不要和 tools 或 tool_choice 一起用。这种请求的回复是 message.function_call，finish_reason 为 function_call。
下一轮把这条 assistant 消息原样放回 messages，再追加：

{"role":"tool","tool_call_id":"<上一步的 id>","content":"<工具返回的文本>"}

旧写法的工具结果是 {"role":"function","name":"get_weather","content":"<工具返回的文本>"}。然后再次 POST。

5. 消息

role 可用 system、developer、user、assistant、tool、function。
content 用字符串，或 [{"type":"text","text":"..."}]。多段文本会按换行拼成一段。

可选 reasoning_effort：none、minimal、low、medium、high、xhigh、max、deep。

可以写但不会改变结果：temperature、top_p、max_tokens、max_completion_tokens、stop、seed、presence_penalty、frequency_penalty。

会返回 400：n 不是 1、logprobs、top_logprobs、图片、音频、文件、未知 role。

其它地址：
GET {{origin}}/v1/models
GET {{origin}}/healthz
GET {{origin}}/doc

调用时 model 仍写 {{model}}。响应里的 model 字段、以及 /v1/models 里的其它名字，下次请求都不要照着填。
单次请求常常要数秒，客户端超时请至少设 3 分钟。返回 429 时等几秒再试。
`
