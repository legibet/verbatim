package protocol

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// bodyModel implements the request-body questions shared by the protocols
// that carry the model name and the stream flag in the JSON body.
type bodyModel struct{}

func (bodyModel) ModelFromBody(body []byte) (string, error) {
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

func (bodyModel) SetModel(body []byte, model string) ([]byte, error) {
	out, err := sjson.SetBytes(body, "model", model)
	if err != nil {
		return nil, fmt.Errorf("set model: %w", err)
	}
	return out, nil
}

func (bodyModel) Stream(body []byte) bool {
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

// modelIDs reads the model ids of a provider's model list: the string at
// idPath of every element of the array at listPath, with trimPrefix removed.
// It returns nil when the body is not such a list, and a non-nil (possibly
// empty) slice when it is.
func modelIDs(body []byte, listPath, idPath, trimPrefix string) []string {
	if !gjson.ValidBytes(body) {
		return nil
	}
	list := gjson.GetBytes(body, listPath)
	if !list.IsArray() {
		return nil
	}
	ids := []string{}
	for _, m := range list.Array() {
		if id := m.Get(idPath); id.Type == gjson.String {
			if s := strings.TrimPrefix(id.Str, trimPrefix); s != "" {
				ids = append(ids, s)
			}
		}
	}
	return ids
}

// dataModelList reads an OpenAI- or Anthropic-style model list page: ids from
// data[].id, plus has_more and last_id where the provider paginates (Anthropic
// always, OpenAI-compatible relays sometimes). hasMore is reported only
// together with the last_id needed to fetch the next page.
func dataModelList(body []byte) (ids []string, hasMore bool, lastID string) {
	ids = modelIDs(body, "data", "id", "")
	if ids == nil {
		return nil, false, ""
	}
	lastID = gjson.GetBytes(body, "last_id").Str
	return ids, gjson.GetBytes(body, "has_more").Type == gjson.True && lastID != "", lastID
}
