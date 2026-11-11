package protocol

import (
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// anthropicVersion is sent when the client did not choose one; the Anthropic
// API rejects requests without the header.
const anthropicVersion = "2023-06-01"

// anthropicProtocol is Anthropic Messages, including count_tokens.
type anthropicProtocol struct{ bodyModel }

func (anthropicProtocol) ID() ID { return Anthropic }

func (anthropicProtocol) Match(method, path string) (Operation, bool) {
	if method != http.MethodPost {
		return "", false
	}
	switch path {
	case "/v1/messages":
		return Create, true
	case "/v1/messages/count_tokens":
		return CountTokens, true
	}
	return "", false
}

func (anthropicProtocol) Suffix(op Operation) string {
	if op == CountTokens {
		return "/v1/messages/count_tokens"
	}
	return "/v1/messages"
}

func (anthropicProtocol) ModelsPath(after string) string {
	q := url.Values{"limit": {"1000"}}
	if after != "" {
		q.Set("after_id", after)
	}
	return "/v1/models?" + q.Encode()
}

func (anthropicProtocol) DefaultHeaders(h http.Header) {
	if len(h.Values("anthropic-version")) == 0 {
		h.Set("anthropic-version", anthropicVersion)
	}
}

func (anthropicProtocol) PrefixForHash(body []byte) ([]byte, bool) {
	return hashPrefix(Anthropic, gjson.GetBytes(body, "system"), firstWithRole(gjson.GetBytes(body, "messages"), "user"))
}

func (anthropicProtocol) ContinuationRef([]byte) (string, bool) { return "", false }

// NewUsageParser ignores stream for count_tokens, which never streams.
func (anthropicProtocol) NewUsageParser(op Operation, stream bool) UsageParser {
	if op == CountTokens {
		return newUsageParser(false, observeAnthropicCount)
	}
	return newUsageParser(stream, observeAnthropic)
}

// observeAnthropic reads a message object or a stream event. A stream's usage
// is message_start's message.usage merged with every message_delta usage.
// Delta values are cumulative and replace earlier ones field by field; a
// field a delta omits or sends as null (older servers send only
// output_tokens) keeps its earlier value, as in the Anthropic SDK. Usage.Raw
// is merged the same way, one top-level field at a time, so a nested object
// such as cache_creation is replaced whole: the result has the shape of a
// non-streamed message's usage.
func observeAnthropic(st *usageState, event string, obj gjson.Result) {
	typ := obj.Get("type").Str
	if typ == "" {
		typ = event
	}
	switch typ {
	case "message_start":
		msg := obj.Get("message")
		st.id = msg.Get("id").Str
		mergeAnthropicUsage(st, msg.Get("usage"))
	case "message_delta":
		mergeAnthropicUsage(st, obj.Get("usage"))
	case "message":
		st.id = obj.Get("id").Str
		mergeAnthropicUsage(st, obj.Get("usage"))
	case "error":
		st.errorEvent = true
	}
}

func mergeAnthropicUsage(st *usageState, u gjson.Result) {
	if !u.IsObject() {
		return
	}
	replace := func(dst **int64, path string) {
		if v := num(u.Get(path)); v != nil {
			*dst = v
		}
	}
	replace(&st.usage.Input, "input_tokens")
	replace(&st.usage.CacheRead, "cache_read_input_tokens")
	replace(&st.usage.CacheWrite, "cache_creation_input_tokens")
	replace(&st.usage.CacheWrite1h, "cache_creation.ephemeral_1h_input_tokens")
	replace(&st.usage.Output, "output_tokens")
	if st.usage.Raw == nil {
		st.usage.Raw = []byte(u.Raw)
	} else {
		u.ForEach(func(k, v gjson.Result) bool {
			if v.Type == gjson.Null {
				return true
			}
			if raw, err := sjson.SetRawBytes(st.usage.Raw, gjson.Escape(k.Str), []byte(v.Raw)); err == nil {
				st.usage.Raw = raw
			}
			return true
		})
	}
	st.ok = true
}

// observeAnthropicCount reads a count_tokens response: {"input_tokens": n}.
func observeAnthropicCount(st *usageState, _ string, obj gjson.Result) {
	if obj.Get("input_tokens").Type != gjson.Number {
		return
	}
	st.set(obj, func(u gjson.Result) Usage { return Usage{Input: num(u.Get("input_tokens"))} })
}

func (anthropicProtocol) ErrorBody(code, msg string) []byte {
	type detail struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	b, _ := json.Marshal(struct {
		Type  string `json:"type"`
		Error detail `json:"error"`
	}{"error", detail{"api_error", code + ": " + msg}})
	return b
}
