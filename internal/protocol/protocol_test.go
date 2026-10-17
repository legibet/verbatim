package protocol

import (
	"bytes"
	"net/http"
	"slices"
	"testing"
)

func TestAllOrder(t *testing.T) {
	var ids []ID
	for _, p := range All {
		ids = append(ids, p.ID())
	}
	if want := []ID{Chat, Responses, Anthropic, Gemini, Images}; !slices.Equal(ids, want) {
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
		model        string
	}{
		{"POST", "/v1/chat/completions", Chat, Create, ""},
		{"POST", "/v1/responses", Responses, Create, ""},
		{"POST", "/v1/alpha/search", Responses, Search, ""},
		{"POST", "/v1/images/generations", Images, Generate, ""},
		{"POST", "/v1/images/edits", Images, Edit, ""},
		{"POST", "/v1/messages", Anthropic, Create, ""},
		{"POST", "/v1/messages/count_tokens", Anthropic, CountTokens, ""},
		{"POST", "/v1beta/models/gemini-3.1-pro:generateContent", Gemini, Create, "gemini-3.1-pro"},
		{"POST", "/v1beta/models/gemini-3.1-pro:streamGenerateContent", Gemini, Create, "gemini-3.1-pro"},
		{"POST", "/v1beta/models/gemini-3.1-flash:countTokens", Gemini, CountTokens, "gemini-3.1-flash"},

		{"GET", "/v1/chat/completions", "", "", ""},
		{"POST", "/v1/chat/completions/", "", "", ""},
		{"POST", "/v1/chat/completion", "", "", ""},
		{"POST", "/v1/responses/", "", "", ""},
		{"POST", "/v1/responses/abc", "", "", ""},
		{"GET", "/v1/responses/abc", "", "", ""},
		{"POST", "/v1/responses/compact", "", "", ""},
		{"GET", "/v1/alpha/search", "", "", ""},
		{"POST", "/alpha/search", "", "", ""},
		{"GET", "/v1/images/generations", "", "", ""},
		{"POST", "/v1/images/variations", "", "", ""},
		{"POST", "/v1/messages/", "", "", ""},
		{"GET", "/v1/messages", "", "", ""},
		{"POST", "/v1/messages/count_tokens/", "", "", ""},
		{"GET", "/v1/models", "", "", ""},
		{"POST", "/v1/models", "", "", ""},
		{"GET", "/v1beta/models/gemini-3.1-pro:generateContent", "", "", ""},
		{"POST", "/v1beta/models/:generateContent", "", "", ""},
		{"POST", "/v1beta/models/gemini-3.1-pro", "", "", ""},
		{"POST", "/v1beta/models/gemini-3.1-pro:embedContent", "", "", ""},
		{"POST", "/v1beta/models/a/b:generateContent", "", "", ""},
		{"POST", "/v1beta/tunedModels/x:generateContent", "", "", ""},
		{"POST", "/v1/chat/completions?x=1", "", "", ""},
	}
	for _, tt := range tests {
		p, op, model, ok := MatchPath(tt.method, tt.path)
		if tt.id == "" {
			if ok {
				t.Errorf("%s %s matched %s/%s, want no match", tt.method, tt.path, p.ID(), op)
			}
			continue
		}
		if !ok || p.ID() != tt.id || op != tt.op || model != tt.model {
			t.Errorf("%s %s = (%v, %q, %q, %v), want (%s, %q, %q)", tt.method, tt.path, p, op, model, ok, tt.id, tt.op, tt.model)
		}
	}
}

func TestSuffix(t *testing.T) {
	tests := []struct {
		id    ID
		op    Operation
		model string
		want  string
	}{
		{Chat, Create, "", "/chat/completions"},
		{Responses, Create, "", "/responses"},
		{Responses, Search, "", "/alpha/search"},
		{Images, Generate, "", "/images/generations"},
		{Images, Edit, "", "/images/edits"},
		{Anthropic, Create, "", "/v1/messages"},
		{Anthropic, CountTokens, "", "/v1/messages/count_tokens"},
		{Gemini, Create, "gemini-3.1-pro", "/v1beta/models/gemini-3.1-pro:generateContent"},
		{Gemini, CountTokens, "gemini-3.1-pro", "/v1beta/models/gemini-3.1-pro:countTokens"},
	}
	for _, tt := range tests {
		if got := Lookup(tt.id).Suffix(tt.op, tt.model); got != tt.want {
			t.Errorf("%s Suffix(%s) = %q, want %q", tt.id, tt.op, got, tt.want)
		}
	}
}

func TestDefaultAuth(t *testing.T) {
	want := map[ID]AuthScheme{Chat: AuthBearer, Responses: AuthBearer, Anthropic: AuthXAPIKey, Gemini: AuthXGoogAPIKey}
	for id, auth := range want {
		if got := Lookup(id).DefaultAuth(); got != auth {
			t.Errorf("%s DefaultAuth = %q, want %q", id, got, auth)
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

	for _, id := range []ID{Chat, Responses, Gemini} {
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
	if got, err := Lookup(Gemini).ModelFromBody([]byte(`{"model":"x"}`), "application/json"); got != "" || err != nil {
		t.Errorf("gemini ModelFromBody = %q, %v; want \"\", nil", got, err)
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
	body := []byte(`{"contents":[], "model":"x"}`)
	if got, err := Lookup(Gemini).SetModel(body, "application/json", "y"); err != nil || !bytes.Equal(got, body) {
		t.Errorf("gemini SetModel = %q, %v; want body unchanged", got, err)
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
	if Lookup(Gemini).Stream([]byte(`{"stream":true}`), "application/json") {
		t.Error("gemini Stream = true, want false")
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
		{
			name: "gemini systemInstruction",
			id:   Gemini,
			body: `{"systemInstruction":{"parts":[{"text":"Be terse."}]},"contents":[{"role":"user","parts":[{"text":"Hi"}]},{"role":"model","parts":[{"text":"Hello"}]}]}`,
			want: "gemini\x00" + `{"parts":[{"text":"Be terse."}]}` + "\x00" + `{"role":"user","parts":[{"text":"Hi"}]}`,
		},
		{
			name: "gemini system_instruction",
			id:   Gemini,
			body: `{"system_instruction":{"parts":[{"text":"S"}]},"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`,
			want: "gemini\x00" + `{"parts":[{"text":"S"}]}` + "\x00" + `{"role":"user","parts":[{"text":"Hi"}]}`,
		},
		{
			name: "gemini content without role",
			id:   Gemini,
			body: `{"contents":[{"parts":[{"text":"Hi"}]}]}`,
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
	for _, id := range []ID{Chat, Anthropic, Gemini} {
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
		{Gemini, 404, "no_route", `{"error":{"code":404,"message":"no_route: no route for \"x\"","status":"NOT_FOUND"}}`},
		{Gemini, 401, "no_route", `{"error":{"code":401,"message":"no_route: no route for \"x\"","status":"UNAUTHENTICATED"}}`},
		{Gemini, 413, "no_route", `{"error":{"code":413,"message":"no_route: no route for \"x\"","status":"FAILED_PRECONDITION"}}`},
		{Gemini, 502, "no_route", `{"error":{"code":502,"message":"no_route: no route for \"x\"","status":"UNAVAILABLE"}}`},
		{Gemini, 503, "no_route", `{"error":{"code":503,"message":"no_route: no route for \"x\"","status":"UNAVAILABLE"}}`},
	}
	for _, tt := range tests {
		if got := string(Lookup(tt.id).ErrorBody(tt.status, tt.code, `no route for "x"`)); got != tt.want {
			t.Errorf("%s ErrorBody(%d) =\n %s\nwant\n %s", tt.id, tt.status, got, tt.want)
		}
	}
}

func TestModelList(t *testing.T) {
	tests := []struct {
		name    string
		id      ID
		body    string
		ids     []string
		hasMore bool
		lastID  string
	}{
		{
			name: "openai",
			id:   Chat,
			body: `{"object":"list","data":[{"id":"gpt-5.6","object":"model","created":1686935002,"owned_by":"openai"},{"id":"gpt-5.6-mini","object":"model","created":1686935002,"owned_by":"openai"}]}`,
			ids:  []string{"gpt-5.6", "gpt-5.6-mini"},
		},
		{
			name:    "openai-compatible page",
			id:      Responses,
			body:    `{"data":[{"id":"a"},{"id":""},{"id":3}],"has_more":true,"last_id":"a"}`,
			ids:     []string{"a"},
			hasMore: true,
			lastID:  "a",
		},
		{
			name:    "chat page",
			id:      Chat,
			body:    `{"object":"list","data":[{"id":"x"},{"id":"y"}],"has_more":true,"last_id":"y"}`,
			ids:     []string{"x", "y"},
			hasMore: true,
			lastID:  "y",
		},
		{
			name: "chat has_more without last_id",
			id:   Chat,
			body: `{"data":[{"id":"x"}],"has_more":true,"last_id":""}`,
			ids:  []string{"x"},
		},
		{
			name:    "anthropic page",
			id:      Anthropic,
			body:    `{"data":[{"type":"model","id":"claude-opus-5-5","display_name":"Claude Opus 5.5","created_at":"2026-03-01T00:00:00Z"},{"type":"model","id":"claude-sonnet-5-5","display_name":"Claude Sonnet 5.5","created_at":"2026-03-01T00:00:00Z"}],"has_more":true,"first_id":"claude-opus-5-5","last_id":"claude-sonnet-5-5"}`,
			ids:     []string{"claude-opus-5-5", "claude-sonnet-5-5"},
			hasMore: true,
			lastID:  "claude-sonnet-5-5",
		},
		{
			name:   "anthropic last page",
			id:     Anthropic,
			body:   `{"data":[{"type":"model","id":"claude-haiku-5-5"}],"has_more":false,"first_id":"claude-haiku-5-5","last_id":"claude-haiku-5-5"}`,
			ids:    []string{"claude-haiku-5-5"},
			lastID: "claude-haiku-5-5",
		},
		{
			name: "anthropic has_more without last_id",
			id:   Anthropic,
			body: `{"data":[{"id":"m"}],"has_more":true}`,
			ids:  []string{"m"},
		},
		{
			name: "gemini",
			id:   Gemini,
			body: `{"models":[{"name":"models/gemini-3.1-pro","version":"3.1","displayName":"Gemini 3.1 Pro","supportedGenerationMethods":["generateContent","countTokens"]},{"name":"models/gemini-3.1-flash"}],"nextPageToken":"abc"}`,
			ids:  []string{"gemini-3.1-pro", "gemini-3.1-flash"},
		},
		{name: "empty list", id: Chat, body: `{"object":"list","data":[]}`, ids: []string{}},
		{name: "not a list", id: Chat, body: `{"error":{"message":"bad key"}}`},
		{name: "truncated", id: Anthropic, body: `{"data":[{"id":"a"}`},
		{name: "html", id: Gemini, body: `<html>502</html>`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ids, hasMore, lastID := Lookup(tt.id).ModelList([]byte(tt.body))
			if !slices.Equal(ids, tt.ids) || (ids == nil) != (tt.ids == nil) || hasMore != tt.hasMore || lastID != tt.lastID {
				t.Fatalf("got %#v, %v, %q; want %#v, %v, %q", ids, hasMore, lastID, tt.ids, tt.hasMore, tt.lastID)
			}
		})
	}
}
