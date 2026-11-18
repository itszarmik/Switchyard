package fakeupstream

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"slices"
)

// embeddingRequest is the part of a /v1/embeddings request the fake reads.
type embeddingRequest struct {
	model  string
	inputs []string
	base64 bool
}

// parseEmbeddingRequest validates an embeddings request as OpenAI does, for
// text inputs only: model a string, input a string or a non-empty array of
// strings, and encoding_format float or base64.
func parseEmbeddingRequest(body []byte) (embeddingRequest, *apiError) {
	bad := func(param, code, format string, args ...any) *apiError {
		return &apiError{status: http.StatusBadRequest, message: fmt.Sprintf(format, args...),
			typ: typeInvalidRequest, param: param, code: code}
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || raw == nil {
		return embeddingRequest{}, bad("", "", "We could not parse the JSON body of your request.")
	}
	present := func(k string) bool { v, ok := raw[k]; return ok && string(v) != "null" }

	var req embeddingRequest
	if !present("model") {
		return req, bad("", "", "you must provide a model parameter")
	}
	if json.Unmarshal(raw["model"], &req.model) != nil {
		return req, bad("model", "invalid_type", "Invalid type for 'model': expected a string.")
	}
	if !present("input") {
		return req, bad("input", "missing_required_parameter", "Missing required parameter: 'input'.")
	}
	var one string
	if json.Unmarshal(raw["input"], &one) == nil {
		req.inputs = []string{one}
	} else if json.Unmarshal(raw["input"], &req.inputs) != nil {
		return req, bad("input", "invalid_type", "Invalid type for 'input': the fake accepts a string or an array of strings.")
	}
	if len(req.inputs) == 0 {
		return req, bad("input", "empty_array", "Invalid 'input': empty array. Expected an array with minimum length 1, but got an empty array instead.")
	}
	if present("encoding_format") {
		var f string
		if json.Unmarshal(raw["encoding_format"], &f) != nil || (f != "float" && f != "base64") {
			return req, bad("encoding_format", "invalid_value", "Invalid 'encoding_format': expected one of float or base64.")
		}
		req.base64 = f == "base64"
	}
	return req, nil
}

// embeddings serves POST /v1/embeddings, which Options.Embed enables.
func (s *Server) embeddings(ctx context.Context, out *responder, body []byte) {
	b := s.nextBehavior()
	for name, values := range b.Header {
		out.w.Header()[name] = append([]string(nil), values...)
	}
	if !s.hold(ctx, b) {
		return
	}
	if b.Status != 0 {
		if !s.sleep(ctx, b.TTFT) {
			return
		}
		out.scripted(b.Status, b.Body)
		return
	}

	req, aerr := parseEmbeddingRequest(body)
	if aerr != nil {
		out.error(aerr.status, *aerr)
		return
	}
	if len(s.opts.Models) > 0 && !slices.Contains(s.opts.Models, req.model) {
		out.error(http.StatusNotFound, apiError{
			message: fmt.Sprintf("The model `%s` does not exist or you do not have access to it.", req.model),
			typ:     typeInvalidRequest,
			code:    "model_not_found",
		})
		return
	}
	if !s.sleep(ctx, b.TTFT) {
		return
	}
	if b.Fault != FaultNone {
		s.fault(ctx, out, b.Fault)
		return
	}

	type item struct {
		Object    string `json:"object"`
		Index     int    `json:"index"`
		Embedding any    `json:"embedding"`
	}
	type usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	}
	resp := struct {
		Object string `json:"object"`
		Data   []item `json:"data"`
		Model  string `json:"model"`
		Usage  usage  `json:"usage"`
	}{Object: "list", Data: make([]item, len(req.inputs)), Model: req.model}
	tokens := 0
	for i, in := range req.inputs {
		vec := s.opts.Embed(in)
		var enc any = vec
		if req.base64 {
			enc = encodeFloat32s(vec)
		}
		resp.Data[i] = item{Object: "embedding", Index: i, Embedding: enc}
		tokens += max((len(in)+3)/4, 1)
	}
	if b.Usage != nil {
		tokens = b.Usage.PromptTokens
	}
	resp.Usage = usage{PromptTokens: tokens, TotalTokens: tokens}
	data, err := json.Marshal(resp)
	if err != nil {
		out.error(http.StatusInternalServerError, apiError{message: "The fake could not encode the embedding.", typ: typeServer})
		return
	}
	out.w.Header().Set("Content-Type", "application/json")
	out.writeHeader(http.StatusOK)
	out.write(data)
}

// encodeFloat32s is OpenAI's base64 encoding of an embedding: the vector's
// little-endian float32 values.
func encodeFloat32s(vec []float32) string {
	buf := make([]byte, 0, 4*len(vec))
	for _, f := range vec {
		buf = binary.LittleEndian.AppendUint32(buf, math.Float32bits(f))
	}
	return base64.StdEncoding.EncodeToString(buf)
}
