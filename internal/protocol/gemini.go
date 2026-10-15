package protocol

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

// geminiProtocol is the Gemini generateContent API. The model lives in the
// URL, so the body is never edited.
type geminiProtocol struct{}

func (geminiProtocol) ID() ID { return Gemini }

// Match recognizes POST /v1beta/models/{model}:{method}. Both generateContent
// and streamGenerateContent are Create.
func (geminiProtocol) Match(method, path string) (Operation, string, bool) {
	if method != http.MethodPost {
		return "", "", false
	}
	rest, ok := strings.CutPrefix(path, "/v1beta/models/")
	if !ok {
		return "", "", false
	}
	model, rpc, _ := strings.Cut(rest, ":")
	if model == "" || strings.Contains(model, "/") {
		return "", "", false
	}
	switch rpc {
	case "generateContent", "streamGenerateContent":
		return Create, model, true
	case "countTokens":
		return CountTokens, model, true
	}
	return "", "", false
}

// Suffix always names the non-streaming method for Create; the interface
// does not carry whether the client called streamGenerateContent.
func (geminiProtocol) Suffix(op Operation, model string) string {
	if op == CountTokens {
		return "/v1beta/models/" + model + ":countTokens"
	}
	return "/v1beta/models/" + model + ":generateContent"
}

func (geminiProtocol) ModelsSuffix() string { return "/v1beta/models" }

// ModelFromBody returns "" and no error: the model is in the URL.
func (geminiProtocol) ModelFromBody([]byte) (string, error) { return "", nil }

// SetModel returns body unchanged: the model is in the URL.
func (geminiProtocol) SetModel(body []byte, _ string) ([]byte, error) { return body, nil }

// Stream is false: Gemini selects streaming by method name, not by body.
func (geminiProtocol) Stream([]byte) bool { return false }

func (geminiProtocol) DefaultAuth() AuthScheme { return AuthXGoogAPIKey }

func (geminiProtocol) DefaultHeaders(http.Header) {}

// PrefixForHash uses systemInstruction (the API also accepts the proto name
// system_instruction) and the first contents[] entry with role "user".
func (geminiProtocol) PrefixForHash(body []byte) ([]byte, bool) {
	system := gjson.GetBytes(body, "systemInstruction")
	if !system.Exists() {
		system = gjson.GetBytes(body, "system_instruction")
	}
	return hashPrefix(Gemini, system, firstWithRole(gjson.GetBytes(body, "contents"), "user"))
}

func (geminiProtocol) ContinuationRef([]byte) (string, bool) { return "", false }

// NewUsageParser parses streams in SSE form (?alt=sse) and ignores stream for
// countTokens, which never streams.
func (geminiProtocol) NewUsageParser(op Operation, stream bool) UsageParser {
	if op == CountTokens {
		return newUsageParser(false, observeGeminiCount)
	}
	return newUsageParser(stream, observeGemini)
}

// observeGemini reads a GenerateContentResponse, whole or one stream chunk;
// the last chunk's usageMetadata wins.
func observeGemini(st *usageState, _ string, obj gjson.Result) {
	if id := obj.Get("responseId"); id.Type == gjson.String {
		st.id = id.Str
	}
	st.set(obj.Get("usageMetadata"), geminiUsage)
	if obj.Get("error").Exists() {
		st.errorEvent = true
	}
}

// geminiUsage normalizes usageMetadata. promptTokenCount includes cached
// content; candidatesTokenCount excludes thoughts, which Usage counts as
// part of Output.
func geminiUsage(u gjson.Result) Usage {
	r := Usage{
		CacheRead: num(u.Get("cachedContentTokenCount")),
		Reasoning: num(u.Get("thoughtsTokenCount")),
	}
	if prompt := num(u.Get("promptTokenCount")); prompt != nil {
		r.Input = new(*prompt - valueOr0(r.CacheRead))
	}
	if candidates := num(u.Get("candidatesTokenCount")); candidates != nil || r.Reasoning != nil {
		r.Output = new(valueOr0(candidates) + valueOr0(r.Reasoning))
	}
	return r
}

// observeGeminiCount reads a countTokens response: {"totalTokens": n, ...}.
func observeGeminiCount(st *usageState, _ string, obj gjson.Result) {
	if obj.Get("totalTokens").Type != gjson.Number {
		return
	}
	st.set(obj, func(u gjson.Result) Usage { return Usage{Input: num(u.Get("totalTokens"))} })
}

func (geminiProtocol) ErrorBody(status int, code, msg string) []byte {
	type detail struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	}
	b, _ := json.Marshal(struct {
		Error detail `json:"error"`
	}{detail{status, code + ": " + msg, geminiStatus(status)}})
	return b
}

// geminiStatus is the google.rpc.Code name Google APIs pair with an HTTP
// status, for the statuses verbatim generates.
func geminiStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "INVALID_ARGUMENT"
	case http.StatusUnauthorized:
		return "UNAUTHENTICATED"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusRequestEntityTooLarge:
		return "FAILED_PRECONDITION"
	case http.StatusBadGateway, http.StatusServiceUnavailable:
		return "UNAVAILABLE"
	}
	return "UNKNOWN"
}

// ModelList reads a Gemini model list; names come as "models/<id>".
// Gemini paginates by nextPageToken, which is not followed.
func (geminiProtocol) ModelList(body []byte) ([]string, bool, string) {
	return modelIDs(body, "models", "name", "models/"), false, ""
}
