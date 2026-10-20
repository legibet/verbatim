package protocol

import (
	"bufio"
	"bytes"
	"errors"
	"mime"
	"net/http"
	"net/textproto"
	"slices"

	"github.com/tidwall/gjson"
)

// imagesProtocol is the OpenAI Images API: generations, and edits sent as
// JSON or as multipart/form-data. A multipart body is never re-encoded: its
// "model" field is read and rewritten in place, and every other byte is
// forwarded as sent.
type imagesProtocol struct{}

func (imagesProtocol) ID() ID { return Images }

func (imagesProtocol) Match(method, path string) (Operation, bool) {
	if method != http.MethodPost {
		return "", false
	}
	switch path {
	case "/v1/images/generations":
		return Generate, true
	case "/v1/images/edits":
		return Edit, true
	}
	return "", false
}

func (imagesProtocol) Suffix(op Operation) string {
	if op == Edit {
		return "/images/edits"
	}
	return "/images/generations"
}

func (imagesProtocol) ModelsPath(after string) string { return openAIModelsPath(after) }

func (imagesProtocol) ModelFromBody(body []byte, contentType string) (string, error) {
	boundary, ok := multipartBoundary(contentType)
	if !ok {
		return bodyModel{}.ModelFromBody(body, contentType)
	}
	start, end, ok := formField(body, boundary, "model")
	switch {
	case !ok:
		return "", errors.New(`multipart body has no "model" field`)
	case start == end:
		return "", errors.New(`multipart "model" field is empty`)
	}
	return string(body[start:end]), nil
}

func (imagesProtocol) SetModel(body []byte, contentType, model string) ([]byte, error) {
	boundary, ok := multipartBoundary(contentType)
	if !ok {
		return bodyModel{}.SetModel(body, contentType, model)
	}
	start, end, ok := formField(body, boundary, "model")
	if !ok {
		return nil, errors.New(`multipart body has no "model" field`)
	}
	return slices.Concat(body[:start], []byte(model), body[end:]), nil
}

func (imagesProtocol) Stream(body []byte, contentType string) bool {
	boundary, ok := multipartBoundary(contentType)
	if !ok {
		return bodyModel{}.Stream(body, contentType)
	}
	start, end, ok := formField(body, boundary, "stream")
	return ok && string(body[start:end]) == "true"
}

func (imagesProtocol) DefaultHeaders(http.Header) {}

// PrefixForHash reports nothing: an image request belongs to no
// conversation, so without an affinity rule it gets no pin.
func (imagesProtocol) PrefixForHash([]byte) ([]byte, bool) { return nil, false }

func (imagesProtocol) ContinuationRef([]byte) (string, bool) { return "", false }

// NewUsageParser reads the top-level "usage" of a response body, or of the
// image_generation.completed / image_edit.completed events of a stream.
func (imagesProtocol) NewUsageParser(op Operation, stream bool) UsageParser {
	return newUsageParser(stream, func(st *usageState, _ string, obj gjson.Result) {
		st.set(obj.Get("usage"), func(u gjson.Result) Usage { return imagesUsage(u, op) })
		if obj.Get("type").Str == "error" || obj.Get("error").IsObject() {
			st.errorEvent = true
		}
	})
}

// imagesUsage normalizes an Images API usage object, which reports no cache
// tokens. When the input details are missing, a generations request, which
// takes only a text prompt, has no image input, and an edit's input is all
// taken as image input: its images outweigh its prompt (design 12.3).
func imagesUsage(u gjson.Result, op Operation) Usage {
	r := Usage{
		Input:       num(u.Get("input_tokens")),
		Output:      num(u.Get("output_tokens")),
		InputImage:  num(u.Get("input_tokens_details.image_tokens")),
		OutputImage: num(u.Get("output_tokens_details.image_tokens")),
	}
	if r.InputImage == nil {
		switch op {
		case Generate:
			r.InputImage = new(int64(0))
		case Edit:
			r.InputImage = r.Input
		}
	}
	return r
}

func (imagesProtocol) ErrorBody(_ int, code, msg string) []byte { return openAIErrorBody(code, msg) }

func (imagesProtocol) ModelList(body []byte) ([]string, string) { return dataModelList(body) }

// multipartBoundary returns the boundary of a multipart/form-data
// Content-Type; ok is false for any other content type.
func multipartBoundary(contentType string) (string, bool) {
	mt, params, err := mime.ParseMediaType(contentType)
	if err != nil || mt != "multipart/form-data" || params["boundary"] == "" {
		return "", false
	}
	return params["boundary"], true
}

// formField finds the first field called name in a multipart/form-data body
// (RFC 7578) and returns the byte range of its value: from the blank line
// that ends the part's headers to the CRLF before the next delimiter.
func formField(body []byte, boundary, name string) (start, end int, ok bool) {
	crlf := []byte("\r\n")
	delim := []byte("\r\n--" + boundary) // between parts
	dash := delim[2:]                    // the first delimiter may open the body
	i := 0
	if !bytes.HasPrefix(body, dash) {
		j := bytes.Index(body, delim)
		if j < 0 {
			return 0, 0, false
		}
		i = j + 2
	}
	for {
		i += len(dash)
		if bytes.HasPrefix(body[i:], []byte("--")) {
			return 0, 0, false // the close delimiter
		}
		eol := bytes.Index(body[i:], crlf) // after optional transport padding
		if eol < 0 {
			return 0, 0, false
		}
		headers := i + eol + 2
		// Searching from the delimiter line's CRLF finds an empty header
		// block too.
		blank := bytes.Index(body[headers-2:], []byte("\r\n\r\n"))
		if blank < 0 {
			return 0, 0, false
		}
		start = headers - 2 + blank + 4
		next := bytes.Index(body[start:], delim)
		if next < 0 {
			return 0, 0, false
		}
		end = start + next
		if partName(body[headers:start]) == name {
			return start, end, true
		}
		i = end + 2
	}
}

// partName is the field name in a part's Content-Disposition header.
func partName(headers []byte) string {
	h, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(headers))).ReadMIMEHeader()
	if err != nil {
		return ""
	}
	disposition, params, err := mime.ParseMediaType(h.Get("Content-Disposition"))
	if err != nil || disposition != "form-data" {
		return ""
	}
	return params["name"]
}
