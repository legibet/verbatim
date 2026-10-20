package protocol

import (
	"errors"
	"fmt"
	"net/url"
	"slices"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// bodyModel implements the request-body questions shared by the protocols
// that carry the model name and the stream flag in the JSON body.
type bodyModel struct{}

func (bodyModel) ModelFromBody(body []byte, _ string) (string, error) {
	m := gjson.GetBytes(body, "model")
	switch {
	case !m.Exists():
		return "", errors.New(`request body has no "model" field`)
	case m.Type != gjson.String:
		return "", fmt.Errorf(`request body "model" must be a string, got %s`, m.Raw)
	case m.Str == "":
		return "", errors.New(`request body "model" is empty`)
	}
	return m.Str, nil
}

func (bodyModel) SetModel(body []byte, _, model string) ([]byte, error) {
	out, err := sjson.SetBytes(body, "model", model)
	if err != nil {
		return nil, fmt.Errorf("set model: %w", err)
	}
	return out, nil
}

func (bodyModel) Stream(body []byte, _ string) bool {
	return gjson.GetBytes(body, "stream").Type == gjson.True
}

// hashPrefix lays out the affinity hash input of design 7.1: the protocol id,
// the system field, and the first user message, as the exact bytes the client
// sent, separated by NUL bytes.
func hashPrefix(id ID, system, user gjson.Result) ([]byte, bool) {
	if !user.Exists() {
		return nil, false
	}
	b := make([]byte, 0, len(id)+len(system.Raw)+len(user.Raw)+2)
	b = append(b, id...)
	b = append(b, 0)
	b = append(b, system.Raw...)
	b = append(b, 0)
	b = append(b, user.Raw...)
	return b, true
}

// firstWithRole returns the first element of the array list whose "role" is
// one of roles.
func firstWithRole(list gjson.Result, roles ...string) gjson.Result {
	var found gjson.Result
	if !list.IsArray() {
		return found
	}
	list.ForEach(func(_, m gjson.Result) bool {
		if r := m.Get("role"); r.Type == gjson.String && slices.Contains(roles, r.Str) {
			found = m
			return false
		}
		return true
	})
	return found
}

// openAIModelsPath is the OpenAI-style model list path. OpenAI does not page
// it; an OpenAI-compatible relay that does takes after_id like Anthropic.
func openAIModelsPath(after string) string {
	if after == "" {
		return "/models"
	}
	return "/models?after_id=" + url.QueryEscape(after)
}

// dataModelList reads an OpenAI- or Anthropic-style model list page: ids from
// data[].id, and as the next cursor the last_id when has_more is true
// (Anthropic always pages, OpenAI-compatible relays sometimes). ids is nil
// when the body is not such a list, and non-nil (possibly empty) when it is.
func dataModelList(body []byte) (ids []string, next string) {
	if !gjson.ValidBytes(body) {
		return nil, ""
	}
	list := gjson.GetBytes(body, "data")
	if !list.IsArray() {
		return nil, ""
	}
	ids = []string{}
	for _, m := range list.Array() {
		if id := m.Get("id"); id.Type == gjson.String && id.Str != "" {
			ids = append(ids, id.Str)
		}
	}
	if gjson.GetBytes(body, "has_more").Type == gjson.True {
		next = gjson.GetBytes(body, "last_id").Str
	}
	return ids, next
}
