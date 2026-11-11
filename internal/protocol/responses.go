package protocol

import (
	"net/http"

	"github.com/tidwall/gjson"
)

// responsesProtocol is the OpenAI Responses API: creating a response, and
// the standalone web search Codex calls for models that get no hosted tools.
type responsesProtocol struct{ bodyModel }

func (responsesProtocol) ID() ID { return Responses }

func (responsesProtocol) Match(method, path string) (Operation, bool) {
	if method != http.MethodPost {
		return "", false
	}
	switch path {
	case "/v1/responses":
		return Create, true
	case "/v1/alpha/search":
		return Search, true
	}
	return "", false
}

func (responsesProtocol) Suffix(op Operation) string {
	if op == Search {
		return "/alpha/search"
	}
	return "/responses"
}

func (responsesProtocol) ModelsPath(after string) string { return openAIModelsPath(after) }

func (responsesProtocol) DefaultHeaders(http.Header) {}

func (responsesProtocol) PrefixForHash(body []byte) ([]byte, bool) {
	user := gjson.GetBytes(body, "input")
	if user.Type != gjson.String {
		user = firstWithRole(user, "user")
	}
	return hashPrefix(Responses, gjson.GetBytes(body, "instructions"), user)
}

// ContinuationRef returns previous_response_id, else the conversation id
// ("conversation" is either the id string or an object carrying "id").
func (responsesProtocol) ContinuationRef(body []byte) (string, bool) {
	if prev := gjson.GetBytes(body, "previous_response_id"); prev.Type == gjson.String && prev.Str != "" {
		return prev.Str, true
	}
	conv := gjson.GetBytes(body, "conversation")
	if conv.IsObject() {
		conv = conv.Get("id")
	}
	if conv.Type == gjson.String && conv.Str != "" {
		return conv.Str, true
	}
	return "", false
}

func (responsesProtocol) NewUsageParser(_ Operation, stream bool) UsageParser {
	return newUsageParser(stream, observeResponses)
}

// observeResponses reads a response object or a stream event. The response
// id comes from the first response object seen (response.created in a
// stream); usage comes from the terminal event, or from a top-level "usage"
// as in a non-streamed response.
func observeResponses(st *usageState, _ string, obj gjson.Result) {
	normalize := func(u gjson.Result) Usage { return openAIUsage(u, "input", "output") }
	typ := obj.Get("type").Str
	if resp := obj.Get("response"); resp.IsObject() {
		if id := resp.Get("id"); st.id == "" && id.Type == gjson.String {
			st.id = id.Str
		}
		switch typ {
		case "response.completed", "response.incomplete", "response.failed":
			st.set(resp.Get("usage"), normalize)
		}
	}
	if id := obj.Get("id"); st.id == "" && id.Type == gjson.String {
		st.id = id.Str
	}
	st.set(obj.Get("usage"), normalize)
	if typ == "response.failed" || typ == "error" {
		st.errorEvent = true
	}
}

func (responsesProtocol) ErrorBody(code, msg string) []byte { return openAIErrorBody(code, msg) }
