// Package protocol is the only place that knows protocol-specific JSON paths
// and URL layouts. It answers a fixed set of questions about a request or
// response body and contains no translation logic of any kind.
package protocol

import "net/http"

type ID string

const (
	Chat      ID = "chat"
	Responses ID = "responses"
	Anthropic ID = "anthropic"
	Gemini    ID = "gemini"
	Images    ID = "images"
)

// Operation is what a request asks of the upstream (design 4).
type Operation string

const (
	Create      Operation = "create"
	CountTokens Operation = "count_tokens"
	Search      Operation = "search"   // Codex's standalone web search, /alpha/search
	Generate    Operation = "generate" // images/generations
	Edit        Operation = "edit"     // images/edits
)

// Auxiliary reports whether op is count_tokens or search, which serve the
// create requests of a session: they follow a pin but never create or move
// one, never feed the breaker, and are neither priced nor aggregated as usage.
func (op Operation) Auxiliary() bool { return op == CountTokens || op == Search }

type AuthScheme string

const (
	AuthBearer      AuthScheme = "bearer"
	AuthXAPIKey     AuthScheme = "x-api-key"
	AuthXGoogAPIKey AuthScheme = "x-goog-api-key"
	AuthNone        AuthScheme = "none"
)

// Usage is the normalized token accounting of one response (design 11.3).
// A nil field means the provider did not report it; nothing is estimated.
type Usage struct {
	Input        *int64 // uncached input tokens
	CacheRead    *int64
	CacheWrite   *int64 // all cache writes
	CacheWrite1h *int64 // subset written with a 1-hour TTL (Anthropic only)
	Output       *int64 // includes reasoning
	Reasoning    *int64 // subset of Output, when reported
	InputImage   *int64 // image tokens, subset of Input (images only)
	OutputImage  *int64 // image tokens, subset of Output (images only)
	Raw          []byte // the provider's usage object exactly as received
}

// UsageParser observes a committed response body incrementally. Write must
// never block and must swallow its own errors; a parse failure leaves the
// result empty, it never affects forwarding.
type UsageParser interface {
	Write(p []byte)
	// Usage returns the last usage object seen, normalized; ok is false when
	// none was seen.
	Usage() (u Usage, ok bool)
	// ResponseID is the id of the response object, if the protocol has one
	// (Responses: "id" of the response; Anthropic: message "id"; Chat: "id").
	ResponseID() string
	// ErrorEvent reports whether an error object or event was seen inside
	// the body (outcome upstream_error_event).
	ErrorEvent() bool
}

// Protocol describes one wire protocol. Implementations are stateless.
type Protocol interface {
	ID() ID
	// Match recognizes an inbound request path. model is set only when the
	// model lives in the URL (gemini).
	Match(method, path string) (op Operation, model string, ok bool)
	// Suffix is the path appended to a provider's base_url for an operation.
	// base_url is what the protocol's official SDK takes as its base URL, so
	// the suffix carries the version for anthropic and gemini but not for
	// the OpenAI protocols, whose SDK base URL already ends in /v1.
	Suffix(op Operation, model string) string
	// ModelsSuffix is the path of the provider's model list.
	ModelsSuffix() string
	// ModelFromBody extracts the model name from a request body; contentType
	// is the request's Content-Type, which tells a multipart body (images).
	ModelFromBody(body []byte, contentType string) (string, error)
	// SetModel rewrites the model name in place (a point update; nothing else
	// in the body changes). A no-op for protocols whose model lives in the URL.
	SetModel(body []byte, contentType, model string) ([]byte, error)
	// Stream reports whether the request asks for a streamed response.
	Stream(body []byte, contentType string) bool
	DefaultAuth() AuthScheme
	// DefaultHeaders adds the headers the protocol requires when the client
	// did not send them (anthropic-version only).
	DefaultHeaders(h http.Header)
	// PrefixForHash returns the exact bytes of the system field and the first
	// user-role message (design 7.1); ok is false when there is no user message.
	PrefixForHash(body []byte) (prefix []byte, ok bool)
	// ContinuationRef returns a server-side state reference the request
	// continues from (Responses previous_response_id or conversation).
	ContinuationRef(body []byte) (id string, ok bool)
	NewUsageParser(op Operation, stream bool) UsageParser
	// ErrorBody renders a verbatim-generated error in the native shape (design 10).
	ErrorBody(status int, code, msg string) []byte
	// ModelList parses the body of the model list. hasMore and lastID
	// drive Anthropic-style pagination (?after_id=).
	ModelList(body []byte) (ids []string, hasMore bool, lastID string)
}

// All lists the protocols verbatim serves, in a fixed order.
var All = []Protocol{chatProtocol{}, responsesProtocol{}, anthropicProtocol{}, geminiProtocol{}, imagesProtocol{}}

// Lookup returns the protocol with the given id, or nil.
func Lookup(id ID) Protocol {
	for _, p := range All {
		if p.ID() == id {
			return p
		}
	}
	return nil
}

// MatchPath identifies the protocol and operation of an inbound request.
func MatchPath(method, path string) (p Protocol, op Operation, model string, ok bool) {
	for _, p := range All {
		if op, model, ok := p.Match(method, path); ok {
			return p, op, model, true
		}
	}
	return nil, "", "", false
}
