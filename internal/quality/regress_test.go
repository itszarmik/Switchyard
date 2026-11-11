package quality

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"switchyard/internal/schema"
)

// TestImageBytesAreNotText: the length and simple_task features count
// only the text of the content, so an inline image, or the JSON of content
// parts, does not make a short question look long.
func TestImageBytesAreNotText(t *testing.T) {
	h, err := NewHeuristic(nil)
	if err != nil {
		t.Fatal(err)
	}
	const q = "What color is the car?"
	parts := func(url string) string {
		return `{"model":"a","messages":[{"role":"user","content":[{"type":"text","text":"` + q + `"},{"type":"image_url","image_url":{"url":"` + url + `"}}]}]}`
	}
	bodies := map[string]string{
		"plain string":        `{"model":"a","messages":[{"role":"user","content":"` + q + `"}]}`,
		"image by URL":        parts("https://example.com/a.png"),
		"inline image 200 KB": parts("data:image/png;base64," + strings.Repeat("A", 200_000)),
		"inline system image": `{"model":"a","messages":[{"role":"system","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` +
			strings.Repeat("A", 200_000) + `"}}]},{"role":"user","content":"` + q + `"}]}`,
	}
	want := h.Assess(decode(t, bodies["plain string"]), Hints{})
	for name, body := range bodies {
		if got := h.Assess(decode(t, body), Hints{}); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %+v, want %+v as for the plain string", name, got, want)
		}
	}
}

// TestLengthCountsAllText: the length feature counts every byte of the
// last user message and of the system messages, beyond the 32 KiB the
// scan reads.
func TestLengthCountsAllText(t *testing.T) {
	h, err := NewHeuristic(nil)
	if err != nil {
		t.Fatal(err)
	}
	length := func(body string) float64 {
		buf := make([]byte, 0, maxText+utf8.UTFMax)
		f, _ := h.extract(decode(t, body), buf)
		return f.v[idxLength]
	}
	user := strings.Repeat("word ", 8_000) // 40 KB, past the cap alone
	sys := strings.Repeat("rule ", 8_000)
	onlyUser := length(`{"model":"a","messages":[{"role":"user","content":"` + user + `"}]}`)
	withSys := length(`{"model":"a","messages":[{"role":"system","content":"` + sys + `"},{"role":"user","content":"` + user + `"}]}`)
	if onlyUser <= 0 || withSys <= onlyUser {
		t.Errorf("length %v for the user message, %v with a system message past the cap; want the second larger", onlyUser, withSys)
	}
}

func decode(t *testing.T, body string) *schema.ChatRequest {
	t.Helper()
	req, err := schema.DecodeChatRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return req
}
