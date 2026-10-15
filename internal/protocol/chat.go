package protocol

import (
	"encoding/json"
	"net/http"

	"github.com/tidwall/gjson"
)

// chatProtocol is OpenAI Chat Completions.
type chatProtocol struct{ bodyModel }

func (chatProtocol) ID() ID { return Chat }

func (chatProtocol) Match(method, path string) (Operation, string, bool) {
	if method == http.MethodPost && path == "/v1/chat/completions" {
		return Create, "", true
	}
	return "", "", false
}

func (chatProtocol) Suffix(Operation, string) string { return "/chat/completions" }

func (chatProtocol) DefaultAuth() AuthScheme { return AuthBearer }

func (chatProtocol) DefaultHeaders(http.Header) {}

func (chatProtocol) PrefixForHash(body []byte) ([]byte, bool) {
	messages := gjson.GetBytes(body, "messages")
	return hashPrefix(Chat, firstWithRole(messages, "system", "developer"), firstWithRole(messages, "user"))
}

func (chatProtocol) ContinuationRef([]byte) (string, bool) { return "", false }

func (chatProtocol) NewUsageParser(_ Operation, stream bool) UsageParser {
	return newUsageParser(stream, observeChat)
}

// observeChat reads a completion object or a stream chunk. In a stream the
// last chunk with a non-null usage wins; it usually has "choices": [] and
// arrives just before [DONE].
func observeChat(st *usageState, _ string, obj gjson.Result) {
	if id := obj.Get("id"); id.Type == gjson.String {
		st.id = id.Str
	}
	st.set(obj.Get("usage"), func(u gjson.Result) Usage { return openAIUsage(u, "prompt", "completion") })
	if obj.Get("error").Exists() {
		st.errorEvent = true
	}
}

// openAIUsage normalizes a Chat Completions usage object (in "prompt", out
// "completion") or a Responses one (in "input", out "output"). The prompt
// count includes cached and cache-written tokens, which are subtracted when
// the details object reports them (design 11.3).
func openAIUsage(u gjson.Result, in, out string) Usage {
	r := Usage{
		Output:    num(u.Get(out + "_tokens")),
		Reasoning: num(u.Get(out + "_tokens_details.reasoning_tokens")),
	}
	prompt := num(u.Get(in + "_tokens"))
	details := u.Get(in + "_tokens_details")
	switch {
	case details.IsObject():
		r.CacheRead = num(details.Get("cached_tokens"))
		r.CacheWrite = num(details.Get("cache_write_tokens"))
		if prompt != nil {
			r.Input = new(*prompt - valueOr0(r.CacheRead) - valueOr0(r.CacheWrite))
		}
	case u.Get("prompt_cache_hit_tokens").Type == gjson.Number:
		// DeepSeek dialect: cache hits are reported beside the prompt count.
		r.CacheRead = num(u.Get("prompt_cache_hit_tokens"))
		if prompt != nil {
			r.Input = new(*prompt - *r.CacheRead)
		}
	default:
		r.Input = prompt
	}
	return r
}

func valueOr0(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func (chatProtocol) ErrorBody(_ int, code, msg string) []byte { return openAIErrorBody(code, msg) }

// openAIErrorBody is the error shape of Chat Completions and Responses.
func openAIErrorBody(code, msg string) []byte {
	type detail struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	}
	b, _ := json.Marshal(struct {
		Error detail `json:"error"`
	}{detail{msg, "verbatim_error", code}})
	return b
}

func (chatProtocol) ModelList(body []byte) ([]string, bool, string) { return dataModelList(body) }
