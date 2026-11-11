package protocol

import (
	"fmt"
	"strings"
	"testing"
)

// show renders a Usage compactly; "-" is a nil field.
func show(u Usage) string {
	f := func(p *int64) string {
		if p == nil {
			return "-"
		}
		return fmt.Sprint(*p)
	}
	return fmt.Sprintf("in=%s cr=%s cw=%s cw1h=%s out=%s rs=%s",
		f(u.Input), f(u.CacheRead), f(u.CacheWrite), f(u.CacheWrite1h), f(u.Output), f(u.Reasoning))
}

// feed writes s to p in pieces of n bytes (all at once when n is 0).
func feed(p UsageParser, s string, n int) {
	if n == 0 {
		p.Write([]byte(s))
		return
	}
	for len(s) > 0 {
		k := min(n, len(s))
		p.Write([]byte(s[:k]))
		s = s[k:]
	}
}

const chatStream = `data: {"id":"chatcmpl-CXs1PqUq9aRxJ4","object":"chat.completion.chunk","created":1760531000,"model":"gpt-5.6-2026-08-01","service_tier":"default","system_fingerprint":null,"choices":[{"index":0,"delta":{"role":"assistant","content":"","refusal":null},"logprobs":null,"finish_reason":null}],"usage":null,"obfuscation":"x1"}

data: {"id":"chatcmpl-CXs1PqUq9aRxJ4","object":"chat.completion.chunk","created":1760531000,"model":"gpt-5.6-2026-08-01","service_tier":"default","system_fingerprint":null,"choices":[{"index":0,"delta":{"content":"Hello"},"logprobs":null,"finish_reason":null}],"usage":null,"obfuscation":"q"}

data: {"id":"chatcmpl-CXs1PqUq9aRxJ4","object":"chat.completion.chunk","created":1760531000,"model":"gpt-5.6-2026-08-01","service_tier":"default","system_fingerprint":null,"choices":[{"index":0,"delta":{},"logprobs":null,"finish_reason":"stop"}],"usage":null,"obfuscation":"Zt"}

data: {"id":"chatcmpl-CXs1PqUq9aRxJ4","object":"chat.completion.chunk","created":1760531000,"model":"gpt-5.6-2026-08-01","service_tier":"default","system_fingerprint":null,"choices":[],"usage":{"prompt_tokens":5210,"completion_tokens":428,"total_tokens":5638,"prompt_tokens_details":{"cached_tokens":4096,"cache_write_tokens":1024,"audio_tokens":0},"completion_tokens_details":{"reasoning_tokens":256,"audio_tokens":0,"accepted_prediction_tokens":0,"rejected_prediction_tokens":0}},"obfuscation":"o"}

data: [DONE]

`

const chatStreamUsage = `{"prompt_tokens":5210,"completion_tokens":428,"total_tokens":5638,"prompt_tokens_details":{"cached_tokens":4096,"cache_write_tokens":1024,"audio_tokens":0},"completion_tokens_details":{"reasoning_tokens":256,"audio_tokens":0,"accepted_prediction_tokens":0,"rejected_prediction_tokens":0}}`

// DeepSeek's older dialect: usage on the finish chunk, cache hits beside the
// prompt count, an SSE keep-alive comment, no details object.
const deepseekStream = `: keep-alive

data: {"id":"8a1f3c2e-5b9d-4c1a-9e7f-2d6b0c4e8a11","object":"chat.completion.chunk","created":1760531100,"model":"deepseek-chat","system_fingerprint":"fp_ffc7281d48_prod0820_fp8_kvcache","choices":[{"index":0,"delta":{"role":"assistant","content":""},"logprobs":null,"finish_reason":null}]}

data: {"id":"8a1f3c2e-5b9d-4c1a-9e7f-2d6b0c4e8a11","object":"chat.completion.chunk","created":1760531100,"model":"deepseek-chat","system_fingerprint":"fp_ffc7281d48_prod0820_fp8_kvcache","choices":[{"index":0,"delta":{"content":"Hi"},"logprobs":null,"finish_reason":null}]}

data: {"id":"8a1f3c2e-5b9d-4c1a-9e7f-2d6b0c4e8a11","object":"chat.completion.chunk","created":1760531100,"model":"deepseek-chat","system_fingerprint":"fp_ffc7281d48_prod0820_fp8_kvcache","choices":[{"index":0,"delta":{"content":""},"logprobs":null,"finish_reason":"stop"}],"usage":{"prompt_tokens":1200,"completion_tokens":50,"total_tokens":1250,"prompt_cache_hit_tokens":1024,"prompt_cache_miss_tokens":176}}

data: [DONE]

`

const chatStreamError = `data: {"id":"gen-1760531200-abc","object":"chat.completion.chunk","created":1760531200,"model":"kimi-k3","choices":[{"index":0,"delta":{"role":"assistant","content":"Par"},"finish_reason":null}]}

data: {"id":"gen-1760531200-abc","object":"chat.completion.chunk","created":1760531200,"model":"kimi-k3","error":{"code":502,"message":"Upstream provider disconnected"},"choices":[{"index":0,"delta":{"content":""},"finish_reason":"error"}]}

data: [DONE]

`

const responsesStream = `event: response.created
data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_68ef1a2b3c4d5e6f","object":"response","created_at":1760531300,"status":"in_progress","background":false,"error":null,"incomplete_details":null,"instructions":"You are Codex.","max_output_tokens":null,"model":"gpt-5.6-codex","output":[],"parallel_tool_calls":true,"previous_response_id":null,"prompt_cache_key":"019a-session","reasoning":{"effort":"medium","summary":"auto"},"store":false,"temperature":1.0,"text":{"format":{"type":"text"},"verbosity":"medium"},"tool_choice":"auto","tools":[],"top_p":1.0,"truncation":"disabled","usage":null,"user":null,"metadata":{}}}

event: response.in_progress
data: {"type":"response.in_progress","sequence_number":1,"response":{"id":"resp_68ef1a2b3c4d5e6f","object":"response","created_at":1760531300,"status":"in_progress","model":"gpt-5.6-codex","output":[],"usage":null}}

event: response.output_item.added
data: {"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"id":"msg_68ef1a2b9f8e7d6c","type":"message","status":"in_progress","content":[],"role":"assistant"}}

event: response.content_part.added
data: {"type":"response.content_part.added","sequence_number":3,"item_id":"msg_68ef1a2b9f8e7d6c","output_index":0,"content_index":0,"part":{"type":"output_text","annotations":[],"logprobs":[],"text":""}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","sequence_number":4,"item_id":"msg_68ef1a2b9f8e7d6c","output_index":0,"content_index":0,"delta":"Done.","logprobs":[],"obfuscation":"abc"}

event: response.output_text.done
data: {"type":"response.output_text.done","sequence_number":5,"item_id":"msg_68ef1a2b9f8e7d6c","output_index":0,"content_index":0,"text":"Done.","logprobs":[]}

event: response.completed
data: {"type":"response.completed","sequence_number":6,"response":{"id":"resp_68ef1a2b3c4d5e6f","object":"response","created_at":1760531300,"status":"completed","model":"gpt-5.6-codex","output":[{"id":"msg_68ef1a2b9f8e7d6c","type":"message","status":"completed","content":[{"type":"output_text","annotations":[],"logprobs":[],"text":"Done."}],"role":"assistant"}],"usage":{"input_tokens":18432,"input_tokens_details":{"cached_tokens":17920},"output_tokens":612,"output_tokens_details":{"reasoning_tokens":448},"total_tokens":19044}}}

`

const responsesFailedStream = `event: response.created
data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_failed1","object":"response","status":"in_progress","usage":null}}

event: response.failed
data: {"type":"response.failed","sequence_number":1,"response":{"id":"resp_failed1","object":"response","status":"failed","error":{"code":"server_error","message":"The model failed to generate a response."},"usage":{"input_tokens":900,"output_tokens":0}}}

`

const responsesErrorEvent = `event: response.created
data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_err2","object":"response","status":"in_progress","usage":null}}

event: error
data: {"type":"error","sequence_number":1,"code":"rate_limit_exceeded","message":"Rate limit reached","param":null}

`

const anthropicStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_01XFDUDYJgAACzvnptvVoYEL","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":12,"cache_creation_input_tokens":2048,"cache_read_input_tokens":40960,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":2048},"output_tokens":1,"service_tier":"standard"}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}

event: ping
data: {"type": "ping"}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Let me look."}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"EqQBCgIYAhIM1gbcDa9GJwZA2b3hGgxBdjrkzLoky3dl1pkiMOYds"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":12,"cache_creation_input_tokens":2048,"cache_read_input_tokens":40960,"output_tokens":356}}

event: message_stop
data: {"type":"message_stop"}

`

// From the streaming docs: message_delta carries only output_tokens.
const anthropicStreamOld = `event: message_start
data: {"type": "message_start", "message": {"id": "msg_1nZdL29xx5MUA1yADyHTEsnR8uuvGzszyY", "type": "message", "role": "assistant", "content": [], "model": "claude-opus-5-5", "stop_reason": null, "stop_sequence": null, "usage": {"input_tokens": 25, "output_tokens": 1}}}

event: content_block_start
data: {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}}

event: content_block_delta
data: {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "Hello"}}

event: content_block_stop
data: {"type": "content_block_stop", "index": 0}

event: message_delta
data: {"type": "message_delta", "delta": {"stop_reason": "end_turn", "stop_sequence":null}, "usage": {"output_tokens": 15}}

event: message_stop
data: {"type": "message_stop"}

`

// From the streaming docs (web search): the delta's cumulative input count
// exceeds message_start's because server tools ran.
const anthropicStreamServerTool = `event: message_start
data: {"type":"message_start","message":{"id":"msg_01G","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":2679,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":3}}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":10682,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":510,"server_tool_use":{"web_search_requests":1}}}

event: message_stop
data: {"type":"message_stop"}

`

const anthropicStreamError = `event: message_start
data: {"type":"message_start","message":{"id":"msg_01Overloaded","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":30,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: error
data: {"type": "error", "error": {"type": "overloaded_error", "message": "Overloaded"}}

`

func TestStreamUsage(t *testing.T) {
	tests := []struct {
		name    string
		id      ID
		stream  string
		want    string // show(Usage); "" means no usage
		raw     string // expected Usage.Raw when set
		respID  string
		errorEv bool
	}{
		{
			name:   "chat include_usage",
			id:     Chat,
			stream: chatStream,
			want:   "in=90 cr=4096 cw=1024 cw1h=- out=428 rs=256",
			raw:    chatStreamUsage,
			respID: "chatcmpl-CXs1PqUq9aRxJ4",
		},
		{
			name:   "chat CRLF",
			id:     Chat,
			stream: strings.ReplaceAll(chatStream, "\n", "\r\n"),
			want:   "in=90 cr=4096 cw=1024 cw1h=- out=428 rs=256",
			raw:    chatStreamUsage,
			respID: "chatcmpl-CXs1PqUq9aRxJ4",
		},
		{
			name:   "chat without usage",
			id:     Chat,
			stream: strings.Replace(chatStream, `"usage":{"prompt_tokens"`, `"usage":null,"x":{"prompt_tokens"`, 1),
			respID: "chatcmpl-CXs1PqUq9aRxJ4",
		},
		{
			name:   "deepseek dialect",
			id:     Chat,
			stream: deepseekStream,
			want:   "in=176 cr=1024 cw=- cw1h=- out=50 rs=-",
			respID: "8a1f3c2e-5b9d-4c1a-9e7f-2d6b0c4e8a11",
		},
		{
			name:    "chat error chunk",
			id:      Chat,
			stream:  chatStreamError,
			respID:  "gen-1760531200-abc",
			errorEv: true,
		},
		{
			name:   "chat null error field",
			id:     Chat,
			stream: "data: {\"id\":\"c1\",\"choices\":[],\"error\":null,\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n",
			want:   "in=3 cr=- cw=- cw1h=- out=2 rs=-",
			respID: "c1",
		},
		{
			name:   "responses completed",
			id:     Responses,
			stream: responsesStream,
			want:   "in=512 cr=17920 cw=- cw1h=- out=612 rs=448",
			raw:    `{"input_tokens":18432,"input_tokens_details":{"cached_tokens":17920},"output_tokens":612,"output_tokens_details":{"reasoning_tokens":448},"total_tokens":19044}`,
			respID: "resp_68ef1a2b3c4d5e6f",
		},
		{
			name:   "responses incomplete",
			id:     Responses,
			stream: "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_inc\",\"status\":\"in_progress\",\"usage\":null}}\n\nevent: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_inc\",\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"usage\":{\"input_tokens\":5,\"output_tokens\":3}}}\n\n",
			want:   "in=5 cr=- cw=- cw1h=- out=3 rs=-",
			respID: "resp_inc",
		},
		{
			name:    "responses failed",
			id:      Responses,
			stream:  responsesFailedStream,
			want:    "in=900 cr=- cw=- cw1h=- out=0 rs=-",
			respID:  "resp_failed1",
			errorEv: true,
		},
		{
			name:    "responses error event",
			id:      Responses,
			stream:  responsesErrorEvent,
			respID:  "resp_err2",
			errorEv: true,
		},
		{
			name:   "anthropic with cache",
			id:     Anthropic,
			stream: anthropicStream,
			want:   "in=12 cr=40960 cw=2048 cw1h=2048 out=356 rs=-",
			raw:    `{"input_tokens":12,"cache_creation_input_tokens":2048,"cache_read_input_tokens":40960,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":2048},"output_tokens":356,"service_tier":"standard"}`,
			respID: "msg_01XFDUDYJgAACzvnptvVoYEL",
		},
		{
			name:   "anthropic delta with output only",
			id:     Anthropic,
			stream: anthropicStreamOld,
			want:   "in=25 cr=- cw=- cw1h=- out=15 rs=-",
			raw:    `{"input_tokens": 25, "output_tokens": 15}`,
			respID: "msg_1nZdL29xx5MUA1yADyHTEsnR8uuvGzszyY",
		},
		{
			name:   "anthropic delta replaces input",
			id:     Anthropic,
			stream: anthropicStreamServerTool,
			want:   "in=10682 cr=0 cw=0 cw1h=- out=510 rs=-",
			raw:    `{"input_tokens":10682,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":510,"server_tool_use":{"web_search_requests":1}}`,
			respID: "msg_01G",
		},
		{
			name: "anthropic delta with null fields",
			id:   Anthropic,
			stream: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_null\",\"usage\":{\"input_tokens\":30,\"cache_read_input_tokens\":20,\"output_tokens\":1}}}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":null,\"cache_read_input_tokens\":null,\"server_tool_use\":null,\"output_tokens\":9}}\n\n",
			want:   "in=30 cr=20 cw=- cw1h=- out=9 rs=-",
			raw:    `{"input_tokens":30,"cache_read_input_tokens":20,"output_tokens":9}`,
			respID: "msg_null",
		},
		{
			name:    "anthropic error event",
			id:      Anthropic,
			stream:  anthropicStreamError,
			want:    "in=30 cr=- cw=- cw1h=- out=1 rs=-",
			respID:  "msg_01Overloaded",
			errorEv: true,
		},
		{
			name:    "anthropic error event named only by event line",
			id:      Anthropic,
			stream:  "event: error\ndata: {\"error\":{\"type\":\"api_error\",\"message\":\"x\"}}\n\n",
			errorEv: true,
		},
	}
	for _, tt := range tests {
		for _, n := range []int{0, 1, 7, 64} {
			t.Run(fmt.Sprintf("%s/%d", tt.name, n), func(t *testing.T) {
				p := Lookup(tt.id).NewUsageParser(Create, true)
				feed(p, tt.stream, n)
				u, ok := p.Usage()
				switch {
				case tt.want == "" && ok:
					t.Errorf("usage = %s, want none", show(u))
				case tt.want != "" && (!ok || show(u) != tt.want):
					t.Errorf("usage = %s (ok %v), want %s", show(u), ok, tt.want)
				}
				if tt.raw != "" && string(u.Raw) != tt.raw {
					t.Errorf("raw = %s, want %s", u.Raw, tt.raw)
				}
				if got := p.ResponseID(); got != tt.respID {
					t.Errorf("response id = %q, want %q", got, tt.respID)
				}
				if got := p.ErrorEvent(); got != tt.errorEv {
					t.Errorf("error event = %v, want %v", got, tt.errorEv)
				}
			})
		}
	}
}

func TestStreamBounds(t *testing.T) {
	small := `data: {"id":"a","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2}}` + "\n\n"
	// Over maxEventBytes in many small values: skipped.
	wide := `data: {"id":"wide","choices":[],"pad":[` + strings.Repeat("0,", 1<<20) + `0],"usage":{"prompt_tokens":99,"completion_tokens":99}}` + "\n\n"
	// Over maxEventBytes in one string, as an image is: the string is
	// dropped and the event read.
	long := `data: {"id":"long","choices":[],"b64_json":"` + strings.Repeat("x", 2<<20) + `","usage":{"prompt_tokens":5,"completion_tokens":5}}` + "\n\n"
	after := `data: {"id":"after","choices":[{"index":0,"delta":{}}],"usage":null}` + "\n\n"
	done := "data: [DONE]\n\n"
	late := `data: {"id":"late","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":7}}` + "\n\n"

	for _, n := range []int{0, 7, 4096} {
		p := Lookup(Chat).NewUsageParser(Create, true)
		feed(p, small+wide+long+after+done+late, n)
		u, ok := p.Usage()
		if !ok || show(u) != "in=5 cr=- cw=- cw1h=- out=5 rs=-" {
			t.Errorf("chunk %d: usage = %s (ok %v); want the long-string event's, not the wide one's", n, show(u), ok)
		}
		if got := p.ResponseID(); got != "after" {
			t.Errorf("chunk %d: response id = %q, want the last event before [DONE]", n, got)
		}
		sp := p.(*streamParser)
		if cap(sp.sse.line) > 2*maxEventBytes || cap(sp.sse.data) > 2*maxEventBytes || cap(sp.elider.content) > 2*maxStringBytes {
			t.Errorf("chunk %d: parser kept %d/%d/%d bytes", n, cap(sp.sse.line), cap(sp.sse.data), cap(sp.elider.content))
		}
	}
}

func TestSSEMultilineData(t *testing.T) {
	var got []string
	var s sseSplitter
	s.write([]byte("event: a\r\ndata: {\"x\":\r\ndata:1}\r\nid: 3\r\nretry: 10\r\n: comment\r\n\r\ndata\n\nevent: only\n\n"), func(event string, data []byte) {
		got = append(got, event+"|"+string(data))
	})
	want := []string{"a|{\"x\":\n1}", "|"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("events = %q, want %q", got, want)
	}
}

func TestBodyUsage(t *testing.T) {
	tests := []struct {
		name   string
		id     ID
		op     Operation
		body   string
		want   string
		raw    string
		respID string
	}{
		{
			name:   "chat",
			id:     Chat,
			body:   `{"id":"chatcmpl-B9MBs8CjcvOU2jLn4n570S5qMJKcT","object":"chat.completion","created":1741569952,"model":"gpt-5.6-2026-08-01","choices":[{"index":0,"message":{"role":"assistant","content":"Hello!","refusal":null,"annotations":[]},"logprobs":null,"finish_reason":"stop"}],"usage":{"prompt_tokens":19,"completion_tokens":10,"total_tokens":29,"prompt_tokens_details":{"cached_tokens":0,"audio_tokens":0},"completion_tokens_details":{"reasoning_tokens":0,"audio_tokens":0,"accepted_prediction_tokens":0,"rejected_prediction_tokens":0}},"service_tier":"default"}`,
			want:   "in=19 cr=0 cw=- cw1h=- out=10 rs=0",
			raw:    `{"prompt_tokens":19,"completion_tokens":10,"total_tokens":29,"prompt_tokens_details":{"cached_tokens":0,"audio_tokens":0},"completion_tokens_details":{"reasoning_tokens":0,"audio_tokens":0,"accepted_prediction_tokens":0,"rejected_prediction_tokens":0}}`,
			respID: "chatcmpl-B9MBs8CjcvOU2jLn4n570S5qMJKcT",
		},
		{
			name:   "chat without details",
			id:     Chat,
			body:   "{\n  \"id\": \"cmpl-local\",\n  \"usage\": {\"prompt_tokens\": 19, \"completion_tokens\": 10, \"total_tokens\": 29}\n}\n",
			want:   "in=19 cr=- cw=- cw1h=- out=10 rs=-",
			raw:    `{"prompt_tokens": 19, "completion_tokens": 10, "total_tokens": 29}`,
			respID: "cmpl-local",
		},
		{
			name:   "chat error body",
			id:     Chat,
			body:   `{"error":{"message":"Invalid 'messages[0].role'","type":"invalid_request_error","param":"messages[0].role","code":null}}`,
			respID: "",
		},
		{
			name:   "responses",
			id:     Responses,
			body:   `{"id":"resp_67ccd3a9da748190baa7f1570fe91ac604becb25c45c1d41","object":"response","created_at":1741476777,"status":"completed","model":"gpt-5.6","output":[{"type":"message","id":"msg_67ccd3acc8d48190a77525dc6de64b4104becb25c45c1d41","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Hi","annotations":[]}]}],"usage":{"input_tokens":328,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":256},"output_tokens":52,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":380}}`,
			want:   "in=72 cr=0 cw=256 cw1h=- out=52 rs=0",
			respID: "resp_67ccd3a9da748190baa7f1570fe91ac604becb25c45c1d41",
		},
		{
			name:   "anthropic",
			id:     Anthropic,
			body:   `{"id":"msg_013Zva2CMHLNnXjNJJKqJ2EF","type":"message","role":"assistant","model":"claude-opus-5-5","content":[{"type":"text","text":"Hi!"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":21,"cache_creation_input_tokens":188,"cache_read_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":148,"ephemeral_1h_input_tokens":40},"output_tokens":393,"service_tier":"standard"}}`,
			want:   "in=21 cr=0 cw=188 cw1h=40 out=393 rs=-",
			respID: "msg_013Zva2CMHLNnXjNJJKqJ2EF",
		},
		{
			name: "anthropic count_tokens",
			id:   Anthropic,
			op:   CountTokens,
			body: `{"input_tokens":2095}`,
			want: "in=2095 cr=- cw=- cw1h=- out=- rs=-",
			raw:  `{"input_tokens":2095}`,
		},
		{
			name: "anthropic count_tokens error",
			id:   Anthropic,
			op:   CountTokens,
			body: `{"type":"error","error":{"type":"not_found_error","message":"model: x"}}`,
		},
	}
	for _, tt := range tests {
		for _, n := range []int{0, 7} {
			t.Run(fmt.Sprintf("%s/%d", tt.name, n), func(t *testing.T) {
				op := tt.op
				if op == "" {
					op = Create
				}
				// count_tokens never streams, whatever the caller says.
				p := Lookup(tt.id).NewUsageParser(op, op == CountTokens)
				feed(p, tt.body, n)
				for range 2 { // Usage is idempotent
					u, ok := p.Usage()
					switch {
					case tt.want == "" && ok:
						t.Errorf("usage = %s, want none", show(u))
					case tt.want != "" && (!ok || show(u) != tt.want):
						t.Errorf("usage = %s (ok %v), want %s", show(u), ok, tt.want)
					}
					if tt.raw != "" && string(u.Raw) != tt.raw {
						t.Errorf("raw = %s, want %s", u.Raw, tt.raw)
					}
				}
				if got := p.ResponseID(); got != tt.respID {
					t.Errorf("response id = %q, want %q", got, tt.respID)
				}
				if p.ErrorEvent() {
					t.Error("error event on a whole body")
				}
			})
		}
	}
}

func TestBodyBounds(t *testing.T) {
	usage := `,"usage":{"prompt_tokens":1,"completion_tokens":1}}`

	// A body far over the limit in one string, as an image response is.
	p := Lookup(Chat).NewUsageParser(Create, false)
	feed(p, `{"id":"big","b64_json":"`+strings.Repeat("x", 2*maxBodyBytes)+`"`+usage, 1<<16)
	if u, ok := p.Usage(); !ok || show(u) != "in=1 cr=- cw=- cw1h=- out=1 rs=-" || p.ResponseID() != "big" {
		t.Errorf("usage from a body with a long string: %s (ok %v)", show(u), ok)
	}
	if n := len(p.(*bodyParser).buf); n > 1<<10 {
		t.Errorf("long string kept: %d bytes buffered", n)
	}

	// A body over the limit in many small values.
	p = Lookup(Chat).NewUsageParser(Create, false)
	feed(p, `{"id":"wide","pad":[`+strings.Repeat("0,", maxBodyBytes/2)+`0]`+usage, 1<<16)
	if u, ok := p.Usage(); ok {
		t.Errorf("usage from a body over the limit: %s", show(u))
	}
	if p.(*bodyParser).buf != nil {
		t.Error("over-limit body still buffered")
	}

	p = Lookup(Chat).NewUsageParser(Create, false)
	feed(p, `{"id":"cut"`+usage[:20], 0)
	if u, ok := p.Usage(); ok || p.ResponseID() != "" {
		t.Errorf("usage from a truncated body: %s", show(u))
	}
}

// TestGarbage feeds malformed input to every parser; none may panic or
// report usage.
func TestGarbage(t *testing.T) {
	inputs := []string{
		"",
		"\x00\xff\xfe",
		"data: {",
		"data: {\"usage\":{\"prompt_tokens\":1\n\n",
		"data: {\"usage\":\n\n",
		"data: [1,2\n\ndata: ]\n\n",
		"data: \"usage\"\n\n",
		"data: null\n\n",
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":7}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":[1]}\n\n",
		"data: {\"type\":\"response.completed\",\"response\":\"x\"}\n\n",
		"data: {\"usageMetadata\":\"x\",\"usage\":true}\n\n",
		"\n\n\n\r\n\r\n:::\n\n",
		"<html><body>502 Bad Gateway</body></html>",
		`{"usage":{"prompt_tokens":1`,
		`{"usage":` + strings.Repeat("[", 10000),
	}
	for _, proto := range All {
		for _, op := range []Operation{Create, CountTokens} {
			for _, stream := range []bool{false, true} {
				for _, in := range inputs {
					for _, n := range []int{0, 1, 3} {
						p := proto.NewUsageParser(op, stream)
						feed(p, in, n)
						if u, ok := p.Usage(); ok {
							t.Errorf("%s %s stream=%v %q: usage %s", proto.ID(), op, stream, in, show(u))
						}
						p.ResponseID()
						p.ErrorEvent()
					}
				}
			}
		}
	}
}

func TestUsageNonNumbers(t *testing.T) {
	p := Lookup(Chat).NewUsageParser(Create, false)
	feed(p, `{"usage":{"prompt_tokens":"many","completion_tokens":null,"prompt_tokens_details":{"cached_tokens":4}}}`, 0)
	u, ok := p.Usage()
	if !ok || show(u) != "in=- cr=4 cw=- cw1h=- out=- rs=-" {
		t.Errorf("usage = %s (ok %v)", show(u), ok)
	}
}
