package quality

import (
	"strings"
	"testing"
)

func TestAppendContent(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		limit int
		want  string
	}{
		{"string", `"Hello World"`, 100, "hello world"},
		{"escapes", `"a\nb\tc\"d\\e\/f\bg"`, 100, "a\nb\tc\"d\\e/f g"},
		{"unicode escape", `"café A"`, 100, "café a"},
		{"surrogate pair", `"😀!"`, 100, "😀!"},
		{"unpaired surrogate", `"\ud83dx"`, 100, "�x"},
		{"utf8 passes through", `"Grüße"`, 100, "grüße"},
		{"parts", `[{"type":"text","text":"One"},{"type":"image_url","image_url":{"url":"x"}},{"type":"text","text":"Two"}]`, 100, "one\ntwo\n"},
		{"part with nested text key is skipped", `[{"type":"image_url","image_url":{"text":"no"}}]`, 100, ""},
		{"non-object parts are skipped", `["x", 1, null, {"text":"yes"}]`, 100, "yes\n"},
		{"non-string text is skipped", `[{"text":5},{"text":"ok"}]`, 100, "ok\n"},
		{"null", `null`, 100, ""},
		{"number", `12`, 100, ""},
		{"empty", ``, 100, ""},
		{"whitespace", "  \n ", 100, ""},
		{"limit", `"abcdefgh"`, 3, "abc"},
		{"limit across parts", `[{"text":"abcd"},{"text":"efgh"}]`, 6, "abcd\ne"},
		{"truncated string", `"abc`, 100, "abc"},
		{"truncated escape", `"abc\`, 100, "abc"},
		{"bad escape", `"a\qb"`, 100, "a"},
		{"bad unicode", `"a\u12zz"`, 100, "a"},
		{"truncated array", `[{"text":"ab"`, 100, "ab\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := appendContent(nil, []byte(tt.raw), tt.limit)
			if string(got) != tt.want {
				t.Errorf("appendContent(%s) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// TestAppendContentCount checks the text length appendContent reports: the
// decoded text of the string or of the text parts, whatever the limit,
// without separators, image parts or JSON syntax.
func TestAppendContentCount(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int
	}{
		{"string", `"Hello World"`, 11},
		{"escapes decode", `"a\nb\u00e9"`, 5}, // é is 2 bytes
		{"surrogate pair", `"\ud83d\ude00"`, 4},
		{"text parts only", `[{"type":"text","text":"What color?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,` + strings.Repeat("A", 1000) + `"}}]`, 11},
		{"several parts", `[{"text":"abcd"},{"text":"efgh"}]`, 8},
		{"beyond the limit", `"` + strings.Repeat("x", 500) + `"`, 500},
		{"null", `null`, 0},
		{"truncated", `"abc`, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, got := appendContent(nil, []byte(tt.raw), 16); got != tt.want {
				t.Errorf("appendContent(%.40s) counts %d, want %d", tt.raw, got, tt.want)
			}
		})
	}
}

func TestCountElements(t *testing.T) {
	tests := []struct {
		raw  string
		want int
	}{
		{`[]`, 0},
		{` [ ] `, 0},
		{`[1]`, 1},
		{`[{"a":[1,2,{"b":"]"}]}, "x", true, null, 3.5e2]`, 5},
		{`{"a":1}`, 0},
		{`null`, 0},
		{``, 0},
		{`[1,`, 1},
		{`[{"a":`, 0},
	}
	for _, tt := range tests {
		if got := countElements([]byte(tt.raw)); got != tt.want {
			t.Errorf("countElements(%s) = %d, want %d", tt.raw, got, tt.want)
		}
	}
}

func TestStringMember(t *testing.T) {
	tests := []struct {
		raw    string
		key    string
		want   string
		wantOK bool
	}{
		{`{"type":"json_schema"}`, "type", "json_schema", true},
		{` { "a" : {"type":"no"} , "type" : "yes" } `, "type", "yes", true},
		{`{"type":1}`, "type", "", false},
		{`{"TYPE":"x"}`, "type", "", false},
		{`{"type":"a","type":"b"}`, "type", "b", true},
		{`{"type":"a","type":2}`, "type", "", false},
		{`{}`, "type", "", false},
		{`[]`, "type", "", false},
		{`{"type":`, "type", "", false},
	}
	for _, tt := range tests {
		got, ok := stringMember([]byte(tt.raw), tt.key)
		if string(got) != tt.want || ok != tt.wantOK {
			t.Errorf("stringMember(%s, %s) = %q, %v; want %q, %v", tt.raw, tt.key, got, ok, tt.want, tt.wantOK)
		}
	}
}

func TestForcesTool(t *testing.T) {
	tests := []struct {
		raw  string
		want bool
	}{
		{`"required"`, true},
		{` "required" `, true},
		{`{"type":"function","function":{"name":"f"}}`, true},
		{`"auto"`, false},
		{`"none"`, false},
		{`null`, false},
		{``, false},
		{`"requ`, false},
	}
	for _, tt := range tests {
		if got := forcesTool([]byte(tt.raw)); got != tt.want {
			t.Errorf("forcesTool(%s) = %v, want %v", tt.raw, got, tt.want)
		}
	}
}

func TestIsBullet(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{"- item", true},
		{"* item", true},
		{"  - nested", true},
		{"• dot", true},
		{"1. first", true},
		{"12) twelfth", true},
		{"-not", false},
		{"1.5 is a number", false},
		{"2024. was a year", false},
		{"plain", false},
		{"-", false},
	}
	for _, tt := range tests {
		if got := isBullet([]byte(tt.line), 0); got != tt.want {
			t.Errorf("isBullet(%q) = %v, want %v", tt.line, got, tt.want)
		}
	}
}

// FuzzAppendContent checks that the scanner never panics or exceeds its
// limit on arbitrary input.
func FuzzAppendContent(f *testing.F) {
	for _, s := range []string{`"x"`, `[{"text":"a"}]`, `"\ud83d`, `[{"text":"\u00`, `[[[[`, `{"a":"`, `[{"text":"a"},]`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		const limit = 64
		out, n := appendContent(make([]byte, 0, limit+4), raw, limit)
		if len(out) > limit+4 {
			t.Fatalf("output of %d bytes exceeds the limit", len(out))
		}
		// The count is of all of the text, whatever the limit.
		if _, all := appendContent(nil, raw, len(raw)*4+16); n != all {
			t.Fatalf("count %d with limit %d, %d without", n, limit, all)
		}
		countElements(raw)
		stringMember(raw, "type")
		forcesTool(raw)
	})
}

// TestStackEvidence checks each kind of stack-trace evidence the scanner
// recognizes, on lowercased text as scanText sees it.
func TestStackEvidence(t *testing.T) {
	tests := []struct {
		text string
		want uint16
	}{
		{"exception in thread \"main\"\n\tat com.acme.foo.bar(foo.java:12)", stackFrameAt | stackFileLine},
		{"    at object.<anonymous> (/app/x.js:10:5)", stackFrameAt | stackFileLine},
		{"/src/registry.go:48 +0x9c", stackFileLine},
		{"  file \"/app/cart.py\", line 61, in total", stackPyLine},
		{"panic: runtime error: index out of range", stackErrorColon},
		{"zerodivisionerror: division by zero", stackErrorColon},
		{"java.lang.nullpointerexception: cannot invoke", stackErrorColon},
		{"error[e0502]: cannot borrow", stackRustError},
		{"traceback (most recent call last):", stackTraceback},
		{"goroutine 1 [running]:", stackRunning},
		{"program received signal sigsegv, segmentation fault.", stackSegv | stackSegfault},
		// Not evidence.
		{"we hit an error yesterday and had to panic a little", 0},
		{"at the meeting we discussed the plan", 0},
		{"the ratio is 3:2 and the time is 10:30", 0},
		{"read chapter.go: 12 pages", 0},
		{"segmentation of the market", 0},
		{"line 3 of the poem", 0},
	}
	v := newVocab()
	for _, tt := range tests {
		if got := scanText([]byte(tt.text), len(tt.text), v).stack; got != tt.want {
			t.Errorf("scanText(%q).stack = %09b, want %09b", tt.text, got, tt.want)
		}
	}
}
