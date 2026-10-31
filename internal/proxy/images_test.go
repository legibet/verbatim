package proxy

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/legibet/verbatim/internal/pricing"
)

func TestImages(t *testing.T) {
	pic := strings.Repeat("iVBORw0KGgo", 300000) // about 3 MB of base64
	usage := `"usage":{"input_tokens":276,"input_tokens_details":{"image_tokens":256,"text_tokens":20},"output_tokens":515,"output_tokens_details":{"image_tokens":515,"text_tokens":0}}`
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		// Like magpie: one JSON body even when a stream was asked for, and
		// usage after the image data.
		body := `{"created":1,"data":[{"b64_json":"` + pic + `"}],"usage":{"input_tokens":12,"output_tokens":515}}`
		if r.URL.Path == "/v1/images/edits" && strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			body = `{"created":1,"data":[{"b64_json":"` + pic + `"}],` + usage + `}`
		}
		reply(200, body)(w, r)
	})
	g := newGateway(t, "providers:\n"+prov("img", "images", up.URL+"/v1")+`
routes:
  - model: gpt-image-*
    targets: [{provider: img, model: up-image}]
`, func(o *Options) {
		dir := t.TempDir()
		overrides := filepath.Join(dir, "overrides.json")
		os.WriteFile(overrides, []byte(`{"models":{"gpt-image-2":{"cost":{"input":5,"image_input":8,"image_output":30}},"gpt-image-text":{"cost":{"input":5,"output":30}}}}`), 0o644)
		prices, _, err := pricing.Load(dir, nil, overrides, nil)
		if err != nil {
			t.Fatal(err)
		}
		o.Pricing = prices
	})

	// Generations, JSON: the model is renamed, the image comes back whole,
	// and usage is read past it. A generation's input holds no image.
	res := g.post(t, "/v1/images/generations", `{"model":"gpt-image-2","prompt":"a red circle","stream":true}`)
	if res.status != 200 || len(res.body) < len(pic) {
		t.Fatalf("generations: %d, %d bytes", res.status, len(res.body))
	}
	if got := up.last(); got.path != "/v1/images/generations" || gjson.Get(got.body, "model").Str != "up-image" ||
		got.header.Get("Authorization") != "Bearer key-img" {
		t.Fatalf("upstream saw %s %s", got.path, got.body[:min(len(got.body), 80)])
	}
	row := g.row(t, res.rid)
	if row.Protocol != "images" || row.Operation != "generate" || !row.Stream ||
		deref(row.InputTokens) != int64(12) || deref(row.InputImageTokens) != int64(0) ||
		deref(row.OutputTokens) != int64(515) || row.OutputImageTokens != nil {
		t.Errorf("generate row: %s/%s in %v/%v out %v/%v", row.Protocol, row.Operation,
			deref(row.InputTokens), deref(row.InputImageTokens), deref(row.OutputTokens), deref(row.OutputImageTokens))
	}
	// No text output price, so all 515 output tokens are image tokens.
	if row.CostUSD == nil || *row.CostUSD != (12*5+515*30)/1e6 {
		t.Errorf("generate cost = %v", deref(row.CostUSD))
	}

	// Edits, multipart: only the model field's value changes on the way.
	var form bytes.Buffer
	mw := multipart.NewWriter(&form)
	part, _ := mw.CreateFormFile("image[]", "a.png")
	part.Write([]byte("\x89PNG\r\n\x1a\nmodel gpt-image-2\r\n"))
	mw.WriteField("model", "gpt-image-2")
	mw.WriteField("prompt", "make it blue")
	mw.Close()
	res = g.post(t, "/v1/images/edits", form.String(), "Content-Type", mw.FormDataContentType())
	if res.status != 200 {
		t.Fatalf("edits: %d %s", res.status, res.body[:min(len(res.body), 200)])
	}
	want := bytes.Replace(form.Bytes(), []byte("\r\n\r\ngpt-image-2\r\n"), []byte("\r\n\r\nup-image\r\n"), 1)
	if got := up.last(); got.body != string(want) || got.header.Get("Content-Type") != mw.FormDataContentType() {
		t.Fatalf("upstream edit body differs beyond the model:\n got %q\nwant %q", got.body, want)
	}
	row = g.row(t, res.rid)
	if row.Operation != "edit" || deref(row.InputImageTokens) != int64(256) || deref(row.OutputImageTokens) != int64(515) ||
		row.CostUSD == nil || *row.CostUSD != (20*5+256*8+515*30)/1e6 {
		t.Errorf("edit row: images in %v out %v, cost %v", deref(row.InputImageTokens), deref(row.OutputImageTokens), deref(row.CostUSD))
	}

	// An edit whose usage does not report its image tokens has all its input
	// priced as image input.
	res = g.post(t, "/v1/images/edits", `{"model":"gpt-image-2","prompt":"x","images":[{"image_url":"data:image/png;base64,AA=="}]}`)
	if row = g.row(t, res.rid); res.status != 200 || deref(row.InputImageTokens) != int64(12) ||
		row.CostUSD == nil || *row.CostUSD != (12*8+515*30)/1e6 {
		t.Errorf("edit without image tokens: %d, images in %v, cost %v", res.status, deref(row.InputImageTokens), deref(row.CostUSD))
	}

	// Image requests are priced only by an entry with image prices.
	res = g.post(t, "/v1/images/generations", `{"model":"gpt-image-text","prompt":"x"}`)
	if row = g.row(t, res.rid); res.status != 200 || row.CostUSD != nil || deref(row.PriceKey) != "gpt-image-text" {
		t.Errorf("text-only price: %d, cost %v, price key %v", res.status, deref(row.CostUSD), deref(row.PriceKey))
	}

	if g.h.pins.Len() != 0 {
		t.Errorf("image requests created %d pins", g.h.pins.Len())
	}
}

// An endpoint's header_timeout replaces server.header_timeout for it alone.
func TestEndpointHeaderTimeout(t *testing.T) {
	slow := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		reply(200, `{"from":"slow"}`)(w, r)
	})
	g := newGateway(t, "providers:\n"+
		fmt.Sprintf("  impatient:\n    key: k\n    endpoints:\n      chat: {base_url: %q, header_timeout: 50ms}\n", slow.URL)+
		prov("patient", "chat", slow.URL)+`
routes:
  - model: m
    targets: [{provider: impatient}, {provider: patient, priority: 2}]
`+noAffinity)
	res := g.post(t, "/v1/chat/completions", chatBody)
	if res.status != 200 || gjson.Get(res.body, "from").Str != "slow" {
		t.Fatalf("got %d %s", res.status, res.body)
	}
	attempts := g.row(t, res.rid).AttemptLog
	if gjson.Get(attempts, "0.provider").Str != "impatient" || !strings.Contains(gjson.Get(attempts, "0.error").Str, "timeout awaiting response headers") ||
		gjson.Get(attempts, "1.status").Int() != 200 {
		t.Errorf("attempts = %s", attempts)
	}
}
