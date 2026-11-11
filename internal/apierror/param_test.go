package apierror

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWriteParam(t *testing.T) {
	tests := []struct {
		name     string
		param    string
		wantBody string
	}{
		{
			name:     "with param",
			param:    "messages",
			wantBody: `{"error":{"message":"Missing required parameter: 'messages'.","type":"invalid_request_error","param":"messages","code":"missing_required_parameter"}}` + "\n",
		},
		{
			name:     "empty param is null",
			wantBody: `{"error":{"message":"Missing required parameter: 'messages'.","type":"invalid_request_error","param":null,"code":"missing_required_parameter"}}` + "\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			WriteParam(rec, http.StatusBadRequest, TypeInvalidRequest, "missing_required_parameter",
				"Missing required parameter: 'messages'.", tt.param)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			if got := rec.Body.String(); got != tt.wantBody {
				t.Errorf("body = %s, want %s", got, tt.wantBody)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
				t.Errorf("Content-Type = %q", ct)
			}
		})
	}
}

type startedWriter struct{ *httptest.ResponseRecorder }

func (startedWriter) HeaderWritten() bool { return true }

func TestWriteParamAfterHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteParam(startedWriter{rec}, http.StatusBadRequest, TypeInvalidRequest, "c", "m", "p")
	if rec.Body.Len() != 0 {
		t.Errorf("wrote %q after headers were sent", rec.Body.String())
	}
}
