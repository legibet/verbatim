package protocol

import (
	"bytes"
	"mime/multipart"
	"net/textproto"
	"strconv"
	"strings"
	"testing"
)

// editForm builds an edits request the way the OpenAI SDK does: an image
// part first, then the text fields.
func editForm(t *testing.T, model string, extra ...string) (body []byte, contentType string) {
	t.Helper()
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="image[]"; filename="a.png"`)
	h.Set("Content-Type", "image/png")
	part, _ := w.CreatePart(h)
	part.Write([]byte("\x89PNG\r\n\r\n--not-a-boundary model\r\n"))
	w.WriteField("prompt", "make it blue")
	if model != "" {
		w.WriteField("model", model)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		w.WriteField(extra[i], extra[i+1])
	}
	w.Close()
	return b.Bytes(), w.FormDataContentType()
}

func TestImagesMultipart(t *testing.T) {
	p := Lookup(Images)
	body, ct := editForm(t, "gpt-image-2", "stream", "true")
	if m, err := p.ModelFromBody(body, ct); m != "gpt-image-2" || err != nil {
		t.Fatalf("ModelFromBody = %q, %v", m, err)
	}
	if !p.Stream(body, ct) {
		t.Error("Stream = false for stream=true")
	}

	// Renaming changes the model field's value and no other byte.
	got, err := p.SetModel(body, ct, "codex/gpt-image-2.5")
	want := bytes.Replace(body, []byte("\r\n\r\ngpt-image-2\r\n"), []byte("\r\n\r\ncodex/gpt-image-2.5\r\n"), 1)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("SetModel = %q, %v", got, err)
	}
	if m, _ := p.ModelFromBody(got, ct); m != "codex/gpt-image-2.5" {
		t.Errorf("model after SetModel = %q", m)
	}

	body, ct = editForm(t, "")
	if _, err := p.ModelFromBody(body, ct); err == nil {
		t.Error("no error for a form without a model field")
	}
	if p.Stream(body, ct) {
		t.Error("Stream = true without a stream field")
	}

	// A body opened by a CRLF before the first delimiter, and a part
	// without headers before the model.
	ct = "multipart/form-data; boundary=XyZ"
	body = []byte("\r\n--XyZ\r\n\r\nanonymous\r\n--XyZ  \r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nm1\r\n--XyZ--\r\n")
	if m, err := p.ModelFromBody(body, ct); m != "m1" || err != nil {
		t.Errorf("ModelFromBody = %q, %v", m, err)
	}

	// JSON bodies go through the shared JSON path.
	if m, _ := p.ModelFromBody([]byte(`{"model":"gpt-image-2","prompt":"x"}`), "application/json"); m != "gpt-image-2" {
		t.Errorf("JSON ModelFromBody = %q", m)
	}
}

func TestImagesUsage(t *testing.T) {
	pic := strings.Repeat("iVBORw0KGgo", 300000) // about 3 MB of base64
	details := `"usage":{"input_tokens":276,"input_tokens_details":{"image_tokens":256,"text_tokens":20},"output_tokens":515,"output_tokens_details":{"image_tokens":515,"text_tokens":0},"total_tokens":791}`
	bare := `"usage":{"input_tokens":12,"output_tokens":515,"total_tokens":527}`
	tests := []struct {
		name   string
		op     Operation
		stream bool
		body   string
		want   string // input, input image, output, output image
	}{
		{"edit with details", Edit, false, `{"created":1,"data":[{"b64_json":"` + pic + `"}],` + details + `}`, "276 256 515 515"},
		{"generate without details", Generate, false, `{"created":1,"data":[{"b64_json":"` + pic + `"}],"model":"codex/gpt-image-2",` + bare + `}`, "12 0 515 -"},
		{"edit without details", Edit, false, `{"data":[{"b64_json":"` + pic + `"}],` + bare + `}`, "12 - 515 -"},
		{"stream", Edit, true, "event: keepalive\ndata: {\"type\":\"keepalive\"}\n\n" +
			"event: image_edit.partial_image\ndata: {\"type\":\"image_edit.partial_image\",\"b64_json\":\"" + pic + "\"}\n\n" +
			"event: image_edit.completed\ndata: {\"type\":\"image_edit.completed\",\"b64_json\":\"" + pic + "\"," + details + "}\n\n", "276 256 515 515"},
	}
	opt := func(v *int64) string {
		if v == nil {
			return "-"
		}
		return strconv.FormatInt(*v, 10)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := Lookup(Images).NewUsageParser(tt.op, tt.stream)
			feed(p, tt.body, 1<<15)
			u, ok := p.Usage()
			got := strings.Join([]string{opt(u.Input), opt(u.InputImage), opt(u.Output), opt(u.OutputImage)}, " ")
			if !ok || got != tt.want || u.CacheRead != nil {
				t.Errorf("usage = %s (ok %v), want %s", got, ok, tt.want)
			}
			if p.ErrorEvent() {
				t.Error("error event")
			}
		})
	}

	p := Lookup(Images).NewUsageParser(Generate, true)
	feed(p, "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"moderation\"}}\n\n", 0)
	if !p.ErrorEvent() {
		t.Error("no error event for an error event")
	}
}
