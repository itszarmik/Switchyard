package usageapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"switchyard/internal/apierror"
	"switchyard/internal/auth"
	"switchyard/internal/reqctx"
	"switchyard/internal/usage"
)

// Feedback limits.
const (
	// maxFeedbackBody bounds a feedback request body: a request ID, a score
	// and a comment of 500 characters of up to 4 bytes each, JSON-escaped,
	// fit well within it.
	maxFeedbackBody = 8 << 10
	// feedbackTimeout bounds storing one feedback.
	feedbackTimeout = 5 * time.Second
)

// Feedback error codes.
const (
	codeInvalidFeedback = "invalid_feedback"
	codeRequestNotFound = "request_not_found"
)

// FeedbackHandler serves POST /switchyard/v1/feedback (DESIGN §10.7): the
// authenticated caller scores one of its tenant's requests. The body is
//
//	{"request_id": "<the X-Request-Id of the response>", "score": -1|0|1, "comment": "<optional, at most 500 characters>"}
//
// with no other members. A valid feedback is stored with the caller's
// tenant and key, replacing the tenant's earlier feedback on the request,
// and answered 202. A malformed body is a 400 invalid_feedback naming the
// parameter; a request ID the store knows its tenant never made is a 404
// request_not_found, the same whether it does not exist or is another
// tenant's. A store that cannot tell stores the feedback anyway; reports
// join it only to the tenant's own requests, so a wrong ID reveals
// nothing. Messages never echo the body, and the comment is never logged.
func FeedbackHandler(s usage.FeedbackStore, opts Options) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		p, ok := auth.FromContext(req.Context())
		if !ok || p.TenantID == "" {
			apierror.Write(w, http.StatusUnauthorized, apierror.TypeInvalidRequest, codeAuth, "Incorrect API key provided.")
			return
		}
		f, param, msg := decodeFeedback(w, req)
		if msg != "" {
			apierror.WriteParam(w, http.StatusBadRequest, apierror.TypeInvalidRequest, codeInvalidFeedback, msg, param)
			return
		}
		f.Tenant, f.KeyID, f.Time = p.TenantID, p.KeyID, opts.now()
		ctx, cancel := context.WithTimeout(req.Context(), feedbackTimeout)
		defer cancel()
		switch err := s.RecordFeedback(ctx, f); {
		case errors.Is(err, usage.ErrUnknownRequest):
			apierror.WriteParam(w, http.StatusNotFound, apierror.TypeInvalidRequest, codeRequestNotFound,
				"No request with this request_id was found for your tenant.", "request_id")
			return
		case err != nil:
			rctx := req.Context()
			// Store errors never carry the stored values.
			reqctx.Logger(rctx).ErrorContext(rctx, "store feedback", "error", err)
			apierror.Write(w, http.StatusServiceUnavailable, apierror.TypeServer, codeUnavailable,
				"The feedback could not be stored. Please retry later.")
			return
		}
		h := w.Header()
		h.Set("Content-Type", "application/json")
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"object":"feedback","status":"accepted"}`+"\n")
	})
}

// feedbackBody is the wire shape of a feedback request. Raw members are
// validated by hand, so that a wrong type gets a message naming the member
// without echoing it.
type feedbackBody struct {
	RequestID json.RawMessage `json:"request_id"`
	Score     json.RawMessage `json:"score"`
	Comment   json.RawMessage `json:"comment"`
}

// decodeFeedback reads and validates the request body. On failure it
// returns the parameter at fault ("" for the body as a whole) and a
// message that never echoes the body.
func decodeFeedback(w http.ResponseWriter, req *http.Request) (f *usage.Feedback, param, msg string) {
	data, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxFeedbackBody))
	if err != nil {
		if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
			return nil, "", "The feedback body may be at most " + strconv.Itoa(maxFeedbackBody) + " bytes."
		}
		return nil, "", "The feedback body could not be read."
	}
	if !utf8.Valid(data) {
		return nil, "", "The feedback body must be valid UTF-8."
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var b feedbackBody
	if err := dec.Decode(&b); err != nil {
		return nil, "", "The feedback body must be a JSON object with request_id, score and an optional comment, and no other members."
	}
	if dec.More() {
		return nil, "", "The feedback body must be a single JSON object."
	}

	f = &usage.Feedback{}
	var id string
	if json.Unmarshal(b.RequestID, &id) != nil || !validRequestID(id) {
		return nil, "request_id", "request_id must be the request ID of one of your requests: a UUID as in the X-Request-Id response header."
	}
	f.RequestID = id
	score, err := strconv.Atoi(string(b.Score))
	if b.Score == nil || err != nil || score < usage.ScoreNegative || score > usage.ScorePositive {
		return nil, "score", "score must be -1, 0 or 1."
	}
	f.Score = score
	if b.Comment != nil && string(b.Comment) != "null" {
		var c string
		if json.Unmarshal(b.Comment, &c) != nil {
			return nil, "comment", "comment must be a string."
		}
		if utf8.RuneCountInString(c) > usage.MaxFeedbackComment {
			return nil, "comment", "comment may be at most " + strconv.Itoa(usage.MaxFeedbackComment) + " characters."
		}
		for _, r := range c {
			if r == utf8.RuneError || (unicode.IsControl(r) && r != '\n' && r != '\t' && r != '\r') {
				return nil, "comment", "comment must be text without control characters other than tabs and line breaks."
			}
		}
		f.Comment = c
	}
	return f, "", ""
}

// validRequestID reports whether id is a request ID as the gateway issues
// them: a UUID in its canonical lowercase form.
func validRequestID(id string) bool {
	u, err := uuid.Parse(id)
	return err == nil && u.String() == id
}
