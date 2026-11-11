package apierror

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestWrite(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		errType    string
		code       string
		message    string
		wantStatus int
		wantBody   string
	}{
		{
			name:       "with code",
			status:     http.StatusRequestEntityTooLarge,
			errType:    TypeInvalidRequest,
			code:       "request_too_large",
			message:    "Request body too large.",
			wantStatus: http.StatusRequestEntityTooLarge,
			wantBody:   `{"error":{"message":"Request body too large.","type":"invalid_request_error","param":null,"code":"request_too_large"}}` + "\n",
		},
		{
			name:       "empty code is null",
			status:     http.StatusNotFound,
			errType:    TypeInvalidRequest,
			code:       "",
			message:    "Invalid URL (GET /nope)",
			wantStatus: http.StatusNotFound,
			wantBody:   `{"error":{"message":"Invalid URL (GET /nope)","type":"invalid_request_error","param":null,"code":null}}` + "\n",
		},
		{
			name:       "server error",
			status:     http.StatusInternalServerError,
			errType:    TypeServer,
			code:       "internal_error",
			message:    "boom",
			wantStatus: http.StatusInternalServerError,
			wantBody:   `{"error":{"message":"boom","type":"server_error","param":null,"code":"internal_error"}}` + "\n",
		},
		{
			name:       "message is JSON-escaped",
			status:     http.StatusBadRequest,
			errType:    TypeInvalidRequest,
			code:       "x",
			message:    "quote \" backslash \\ newline \n",
			wantStatus: http.StatusBadRequest,
			wantBody:   `{"error":{"message":"quote \" backslash \\ newline \n","type":"invalid_request_error","param":null,"code":"x"}}` + "\n",
		},
		{
			name:       "invalid status becomes 500",
			status:     42,
			errType:    TypeServer,
			code:       "",
			message:    "m",
			wantStatus: http.StatusInternalServerError,
			wantBody:   `{"error":{"message":"m","type":"server_error","param":null,"code":null}}` + "\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			Write(rec, tt.status, tt.errType, tt.code, tt.message)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if got := rec.Body.String(); got != tt.wantBody {
				t.Errorf("body mismatch (-want +got):\n%s", cmp.Diff(tt.wantBody, got))
			}
			wantHeaders := map[string]string{
				"Content-Type":           "application/json; charset=utf-8",
				"X-Content-Type-Options": "nosniff",
				"Content-Length":         strconv.Itoa(len(tt.wantBody)),
			}
			for k, want := range wantHeaders {
				if got := rec.Header().Get(k); got != want {
					t.Errorf("header %s = %q, want %q", k, got, want)
				}
			}

			// The body must also decode into exactly the four envelope fields.
			var env map[string]map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("body is not JSON: %v", err)
			}
			if len(env) != 1 || len(env["error"]) != 4 {
				t.Errorf("envelope shape = %v, want one \"error\" object with 4 fields", env)
			}
			if v, ok := env["error"]["param"]; !ok || v != nil {
				t.Errorf("param = %v (present %v), want null", v, ok)
			}
		})
	}
}

// trackingWriter is a ResponseWriter that reports HeaderWritten and counts
// WriteHeader calls, like the server's middleware wrapper does.
type trackingWriter struct {
	http.ResponseWriter
	written      bool
	writeHeaders int
}

func (w *trackingWriter) WriteHeader(code int) {
	w.writeHeaders++
	w.written = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *trackingWriter) Write(b []byte) (int, error) {
	w.written = true
	return w.ResponseWriter.Write(b)
}

func (w *trackingWriter) HeaderWritten() bool { return w.written }

// unwrapper hides trackingWriter's HeaderWritten behind an Unwrap chain.
type unwrapper struct{ inner http.ResponseWriter }

func (u unwrapper) Header() http.Header         { return u.inner.Header() }
func (u unwrapper) Write(b []byte) (int, error) { return u.inner.Write(b) }
func (u unwrapper) WriteHeader(code int)        { u.inner.WriteHeader(code) }
func (u unwrapper) Unwrap() http.ResponseWriter { return u.inner }

func TestWriteAfterHeadersSent(t *testing.T) {
	tests := []struct {
		name string
		wrap func(*trackingWriter) http.ResponseWriter
	}{
		{name: "direct", wrap: func(tw *trackingWriter) http.ResponseWriter { return tw }},
		{name: "through Unwrap", wrap: func(tw *trackingWriter) http.ResponseWriter { return unwrapper{unwrapper{tw}} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tw := &trackingWriter{ResponseWriter: rec}
			w := tt.wrap(tw)

			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("partial"))
			Write(w, http.StatusInternalServerError, TypeServer, "internal_error", "late")

			if tw.writeHeaders != 1 {
				t.Errorf("WriteHeader called %d times, want 1", tw.writeHeaders)
			}
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", rec.Code)
			}
			if got := rec.Body.String(); got != "partial" {
				t.Errorf("body = %q, want %q", got, "partial")
			}
			if ct := rec.Header().Get("Content-Type"); ct != "" {
				t.Errorf("Content-Type = %q, want unset", ct)
			}
		})
	}
}

func TestWriteBeforeHeadersThroughWrapper(t *testing.T) {
	rec := httptest.NewRecorder()
	tw := &trackingWriter{ResponseWriter: rec}
	Write(unwrapper{tw}, http.StatusNotFound, TypeInvalidRequest, "", "x")
	if rec.Code != http.StatusNotFound || tw.writeHeaders != 1 {
		t.Fatalf("status = %d, WriteHeader calls = %d; want 404 and 1", rec.Code, tw.writeHeaders)
	}
}
