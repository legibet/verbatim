package protocol

import (
	"net/http"
	"slices"
	"testing"
)

func TestAllOrder(t *testing.T) {
	var ids []ID
	for _, p := range All {
		ids = append(ids, p.ID())
	}
	if want := []ID{Chat, Responses, Anthropic, Images}; !slices.Equal(ids, want) {
		t.Fatalf("All = %v, want %v", ids, want)
	}
	for _, id := range ids {
		if Lookup(id).ID() != id {
			t.Errorf("Lookup(%q) returned the wrong protocol", id)
		}
	}
	if Lookup("nope") != nil {
		t.Error("Lookup of an unknown id is not nil")
	}
}

func TestMatchPath(t *testing.T) {
	tests := []struct {
		method, path string
		id           ID
		op           Operation
	}{
		{"POST", "/v1/chat/completions", Chat, Create},
		{"POST", "/v1/responses", Responses, Create},
		{"POST", "/v1/alpha/search", Responses, Search},
		{"POST", "/v1/images/generations", Images, Generate},
		{"POST", "/v1/images/edits", Images, Edit},
		{"POST", "/v1/messages", Anthropic, Create},
		{"POST", "/v1/messages/count_tokens", Anthropic, CountTokens},

		{"GET", "/v1/chat/completions", "", ""},
		{"POST", "/v1/chat/completions/", "", ""},
		{"POST", "/v1/chat/completion", "", ""},
		{"POST", "/v1/responses/", "", ""},
		{"POST", "/v1/responses/abc", "", ""},
		{"GET", "/v1/responses/abc", "", ""},
		{"POST", "/v1/responses/compact", "", ""},
		{"GET", "/v1/alpha/search", "", ""},
		{"POST", "/alpha/search", "", ""},
		{"GET", "/v1/images/generations", "", ""},
		{"POST", "/v1/images/variations", "", ""},
		{"POST", "/v1/messages/", "", ""},
		{"GET", "/v1/messages", "", ""},
		{"POST", "/v1/messages/count_tokens/", "", ""},
		{"GET", "/v1/models", "", ""},
		{"POST", "/v1/models", "", ""},
		{"POST", "/v1/chat/completions?x=1", "", ""},
	}
	for _, tt := range tests {
		p, op, ok := MatchPath(tt.method, tt.path)
		if tt.id == "" {
			if ok {
				t.Errorf("%s %s matched %s/%s, want no match", tt.method, tt.path, p.ID(), op)
			}
			continue
		}
		if !ok || p.ID() != tt.id || op != tt.op {
			t.Errorf("%s %s = (%v, %q, %v), want (%s, %q)", tt.method, tt.path, p, op, ok, tt.id, tt.op)
		}
	}
}

func TestSuffix(t *testing.T) {
	tests := []struct {
		id   ID
		op   Operation
		want string
	}{
		{Chat, Create, "/chat/completions"},
		{Responses, Create, "/responses"},
		{Responses, Search, "/alpha/search"},
		{Images, Generate, "/images/generations"},
		{Images, Edit, "/images/edits"},
		{Anthropic, Create, "/v1/messages"},
		{Anthropic, CountTokens, "/v1/messages/count_tokens"},
	}
	for _, tt := range tests {
		if got := Lookup(tt.id).Suffix(tt.op); got != tt.want {
			t.Errorf("%s Suffix(%s) = %q, want %q", tt.id, tt.op, got, tt.want)
		}
	}
}

func TestDefaultHeaders(t *testing.T) {
	h := http.Header{}
	Lookup(Anthropic).DefaultHeaders(h)
	if got := h.Get("Anthropic-Version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want 2023-06-01", got)
	}

	h = http.Header{"Anthropic-Version": {"2024-10-22"}}
	Lookup(Anthropic).DefaultHeaders(h)
	if got := h.Values("Anthropic-Version"); !slices.Equal(got, []string{"2024-10-22"}) {
		t.Errorf("client anthropic-version replaced: %q", got)
	}

	for _, id := range []ID{Chat, Responses, Images} {
		h := http.Header{}
		Lookup(id).DefaultHeaders(h)
		if len(h) != 0 {
			t.Errorf("%s added headers %v", id, h)
		}
	}
}

func TestModelFromBody(t *testing.T) {
	tests := []struct {
		body    string
		want    string
		wantErr bool
	}{
		{`{"model":"kimi-k3","messages":[]}`, "kimi-k3", false},
		{`{"messages":[], "model" : "anthropic/claude-opus-5-5"}`, "anthropic/claude-opus-5-5", false},
		{`{"messages":[]}`, "", true},
		{`{"model":42}`, "", true},
		{`{"model":null}`, "", true},
		{`{"model":""}`, "", true},
		{`{"metadata":{"model":"x"}}`, "", true},
		{``, "", true},
	}
	for _, id := range []ID{Chat, Responses, Anthropic, Images} {
		for _, tt := range tests {
			got, err := Lookup(id).ModelFromBody([]byte(tt.body), "application/json")
			if got != tt.want || (err != nil) != tt.wantErr {
				t.Errorf("%s ModelFromBody(%s) = %q, %v; want %q, error %v", id, tt.body, got, err, tt.want, tt.wantErr)
			}
		}
	}
}

func TestSetModel(t *testing.T) {
	tests := []struct{ body, want string }{
		{
			"{\n  \"model\" :  \"kimi-k3\",\n  \"x_vendor\": {\"model\": \"inner\"},\n\t\"messages\": [ {\"role\":\"user\",\"content\":\"hé \\u00e9\"} ],\n  \"z\": 1.50\n}\n",
			"{\n  \"model\" :  \"kimi-k3-0905\",\n  \"x_vendor\": {\"model\": \"inner\"},\n\t\"messages\": [ {\"role\":\"user\",\"content\":\"hé \\u00e9\"} ],\n  \"z\": 1.50\n}\n",
		},
		{
			`{"stream":true,"model":"a","unknown_field":[1,2,{"b":null}]}`,
			`{"stream":true,"model":"kimi-k3-0905","unknown_field":[1,2,{"b":null}]}`,
		},
	}
	for _, id := range []ID{Chat, Responses, Anthropic, Images} {
		for _, tt := range tests {
			orig := []byte(tt.body)
			got, err := Lookup(id).SetModel(orig, "application/json", "kimi-k3-0905")
			if err != nil || string(got) != tt.want {
				t.Errorf("%s SetModel = %q, %v; want %q", id, got, err, tt.want)
			}
			if string(orig) != tt.body {
				t.Errorf("%s SetModel modified its input", id)
			}
		}
	}
}

func TestStream(t *testing.T) {
	tests := []struct {
		body string
		want bool
	}{
		{`{"model":"m","stream":true}`, true},
		{`{"model":"m", "stream" : true }`, true},
		{`{"model":"m","stream":false}`, false},
		{`{"model":"m","stream":"true"}`, false},
		{`{"model":"m","stream":1}`, false},
		{`{"model":"m"}`, false},
		{`{"model":"m","options":{"stream":true}}`, false},
	}
	for _, id := range []ID{Chat, Responses, Anthropic, Images} {
		for _, tt := range tests {
			if got := Lookup(id).Stream([]byte(tt.body), "application/json"); got != tt.want {
				t.Errorf("%s Stream(%s) = %v, want %v", id, tt.body, got, tt.want)
			}
		}
	}
}

func TestPrefixForHash(t *testing.T) {
	tests := []struct {
		name string
		id   ID
		body string
		want string // "" means ok is false
	}{
		{
			name: "chat system and user, whitespace kept",
			id:   Chat,
			body: `{"model":"m","messages":[ {"role": "system",  "content":"Be brief."} ,{"role":"user","content": [ {"type":"text","text":"hi"} ]},{"role":"user","content":"again"}]}`,
			want: "chat\x00" + `{"role": "system",  "content":"Be brief."}` + "\x00" + `{"role":"user","content": [ {"type":"text","text":"hi"} ]}`,
		},
		{
			name: "chat developer message counts as system",
			id:   Chat,
			body: `{"messages":[{"role":"user","content":"q"},{"role":"developer","content":"d"},{"role":"system","content":"s"}]}`,
			want: "chat\x00" + `{"role":"developer","content":"d"}` + "\x00" + `{"role":"user","content":"q"}`,
		},
		{
			name: "chat without system",
			id:   Chat,
			body: `{"messages":[{"role":"user","content":"q"}]}`,
			want: "chat\x00\x00" + `{"role":"user","content":"q"}`,
		},
		{
			name: "chat without user",
			id:   Chat,
			body: `{"messages":[{"role":"system","content":"s"},{"role":"assistant","content":"a"}]}`,
		},
		{
			name: "chat messages not an array",
			id:   Chat,
			body: `{"messages":{"role":"user","content":"q"}}`,
		},
		{
			name: "responses string input",
			id:   Responses,
			body: `{"model":"gpt-5.6","instructions": "You are Codex.","input":"Write a haiku"}`,
			want: "responses\x00\"You are Codex.\"\x00\"Write a haiku\"",
		},
		{
			name: "responses item list",
			id:   Responses,
			body: `{"input":[{"type":"message","role":"developer","content":"d"},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
			want: "responses\x00\x00" + `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`,
		},
		{
			name: "responses without user",
			id:   Responses,
			body: `{"instructions":"x","input":[{"type":"function_call_output","call_id":"c","output":"o"}]}`,
		},
		{
			name: "anthropic system blocks",
			id:   Anthropic,
			body: `{"model":"claude-opus-5-5","system":[{"type":"text","text":"You are Claude Code.","cache_control":{"type":"ephemeral"}}],` + "\n" + `"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"}]}`,
			want: "anthropic\x00" + `[{"type":"text","text":"You are Claude Code.","cache_control":{"type":"ephemeral"}}]` + "\x00" + `{"role":"user","content":"hello"}`,
		},
		{
			name: "anthropic without user",
			id:   Anthropic,
			body: `{"system":"s","messages":[]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Lookup(tt.id).PrefixForHash([]byte(tt.body))
			if tt.want == "" {
				if ok {
					t.Fatalf("ok = true (%q), want false", got)
				}
				return
			}
			if !ok || string(got) != tt.want {
				t.Fatalf("got %q, %v; want %q", got, ok, tt.want)
			}
		})
	}
}

func TestContinuationRef(t *testing.T) {
	tests := []struct {
		body string
		want string
	}{
		{`{"previous_response_id":"resp_abc","input":"x"}`, "resp_abc"},
		{`{"previous_response_id":"resp_abc","conversation":"conv_1"}`, "resp_abc"},
		{`{"conversation":"conv_1"}`, "conv_1"},
		{`{"conversation":{"id":"conv_2"}}`, "conv_2"},
		{`{"previous_response_id":null,"conversation":{"id":"conv_2"}}`, "conv_2"},
		{`{"previous_response_id":""}`, ""},
		{`{"conversation":{}}`, ""},
		{`{"conversation":7}`, ""},
		{`{"input":"x"}`, ""},
	}
	for _, tt := range tests {
		got, ok := Lookup(Responses).ContinuationRef([]byte(tt.body))
		if got != tt.want || ok != (tt.want != "") {
			t.Errorf("ContinuationRef(%s) = %q, %v; want %q", tt.body, got, ok, tt.want)
		}
	}
	for _, id := range []ID{Chat, Anthropic, Images} {
		if got, ok := Lookup(id).ContinuationRef([]byte(`{"previous_response_id":"resp_abc"}`)); ok || got != "" {
			t.Errorf("%s ContinuationRef = %q, %v; want none", id, got, ok)
		}
	}
}

func TestErrorBody(t *testing.T) {
	tests := []struct {
		id     ID
		status int
		code   string
		want   string
	}{
		{Chat, 404, "no_route", `{"error":{"message":"no route for \"x\"","type":"verbatim_error","code":"no_route"}}`},
		{Responses, 404, "no_route", `{"error":{"message":"no route for \"x\"","type":"verbatim_error","code":"no_route"}}`},
		{Anthropic, 404, "no_route", `{"type":"error","error":{"type":"api_error","message":"no_route: no route for \"x\""}}`},
	}
	for _, tt := range tests {
		if got := string(Lookup(tt.id).ErrorBody(tt.status, tt.code, `no route for "x"`)); got != tt.want {
			t.Errorf("%s ErrorBody(%d) =\n %s\nwant\n %s", tt.id, tt.status, got, tt.want)
		}
	}
}

func TestModelsPath(t *testing.T) {
	tests := []struct {
		id           ID
		first, after string
	}{
		{Chat, "/models", "/models?after_id=a%2Fb"},
		{Anthropic, "/v1/models?limit=1000", "/v1/models?after_id=a%2Fb&limit=1000"},
	}
	for _, tt := range tests {
		p := Lookup(tt.id)
		if got := p.ModelsPath(""); got != tt.first {
			t.Errorf("%s first page = %q, want %q", tt.id, got, tt.first)
		}
		if got := p.ModelsPath("a/b"); got != tt.after {
			t.Errorf("%s next page = %q, want %q", tt.id, got, tt.after)
		}
	}
}

func TestModelList(t *testing.T) {
	tests := []struct {
		name string
		id   ID
		body string
		ids  []string
		next string
	}{
		{
			name: "openai",
			id:   Chat,
			body: `{"object":"list","data":[{"id":"gpt-5.6","object":"model","created":1686935002,"owned_by":"openai"},{"id":"gpt-5.6-mini","object":"model","created":1686935002,"owned_by":"openai"}]}`,
			ids:  []string{"gpt-5.6", "gpt-5.6-mini"},
		},
		{
			name: "openai-compatible page",
			id:   Responses,
			body: `{"data":[{"id":"a"},{"id":""},{"id":3}],"has_more":true,"last_id":"a"}`,
			ids:  []string{"a"},
			next: "a",
		},
		{
			name: "chat has_more without last_id",
			id:   Chat,
			body: `{"data":[{"id":"x"}],"has_more":true,"last_id":""}`,
			ids:  []string{"x"},
		},
		{
			name: "anthropic page",
			id:   Anthropic,
			body: `{"data":[{"type":"model","id":"claude-opus-5-5","display_name":"Claude Opus 5.5","created_at":"2026-03-01T00:00:00Z"},{"type":"model","id":"claude-sonnet-5-5","display_name":"Claude Sonnet 5.5","created_at":"2026-03-01T00:00:00Z"}],"has_more":true,"first_id":"claude-opus-5-5","last_id":"claude-sonnet-5-5"}`,
			ids:  []string{"claude-opus-5-5", "claude-sonnet-5-5"},
			next: "claude-sonnet-5-5",
		},
		{
			name: "anthropic last page",
			id:   Anthropic,
			body: `{"data":[{"type":"model","id":"claude-haiku-5-5"}],"has_more":false,"first_id":"claude-haiku-5-5","last_id":"claude-haiku-5-5"}`,
			ids:  []string{"claude-haiku-5-5"},
		},
		{name: "empty list", id: Chat, body: `{"object":"list","data":[]}`, ids: []string{}},
		{name: "not a list", id: Chat, body: `{"error":{"message":"bad key"}}`},
		{name: "truncated", id: Anthropic, body: `{"data":[{"id":"a"}`},
		{name: "html", id: Chat, body: `<html>502</html>`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ids, next := Lookup(tt.id).ModelList([]byte(tt.body))
			if !slices.Equal(ids, tt.ids) || (ids == nil) != (tt.ids == nil) || next != tt.next {
				t.Fatalf("got %#v, %q; want %#v, %q", ids, next, tt.ids, tt.next)
			}
		})
	}
}
