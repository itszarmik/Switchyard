package gemini

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"strings"
)

// Thought signatures in tool-call ids (DESIGN §6.2).
//
// Gemini 3 and 4 put a thoughtSignature on the first functionCall part of
// each step, and reject the next request (400) unless it comes back on that
// part. OpenAI clients have no field for it, so the adapter carries it in
// the one value every client echoes: the tool-call id. The id is
//
//	gm1_ + base64url-without-padding(payload)
//
// where payload is
//
//	flags        1 byte: bit 0 a signature follows; bit 1 it is kept as
//	             its original text rather than its decoded bytes
//	model        6 bytes: the start of SHA-256 of the upstream model the
//	             call came from, so a signature is never sent to another
//	             model
//	id length    uvarint
//	id           Gemini's functionCall.id (or one the adapter made up
//	             when Gemini sent none)
//	signature    the rest, up to the check: the base64 thoughtSignature
//	             decoded to bytes when its standard encoding gives the
//	             same text back (so it returns byte for byte), else the
//	             text itself
//	check        4 bytes: the start of SHA-256 of everything before it,
//	             which catches an id a client cut short or edited
//
// The id uses only [A-Za-z0-9_-], which every OpenAI client and the
// Anthropic adapter's tool_use id pattern accept. No state is kept: an id
// decodes on its own, on any replica, for as long as the client keeps the
// conversation.
//
// The check is not a MAC: anyone can build a valid id. That is harmless,
// since a forged signature only makes Google reject the forger's own
// request.
const (
	toolIDPrefix   = "gm1_"
	flagSignature  = 1 << 0
	flagRawSig     = 1 << 1
	modelHashLen   = 6
	checkLen       = 4
	maxGeminiIDLen = 1 << 10
)

// SkipSignature is the value Google documents for the thoughtSignature of
// a function call the model did not produce (history from another
// provider or model), which skips the signature check at some cost in
// quality.
const SkipSignature = "skip_thought_signature_validator"

// modelHash is the start of SHA-256 of an upstream model name.
func modelHash(model string) []byte {
	sum := sha256.Sum256([]byte(strings.TrimPrefix(model, "models/")))
	return sum[:modelHashLen]
}

// encodeToolCallID returns the client-facing id of a function call that
// model returned with Gemini id geminiID and thought signature sig ("" if
// none).
func encodeToolCallID(model, geminiID, sig string) string {
	var flags byte
	var sigBytes []byte
	if sig != "" {
		flags |= flagSignature
		if b, err := base64.StdEncoding.DecodeString(sig); err == nil && base64.StdEncoding.EncodeToString(b) == sig {
			sigBytes = b
		} else {
			flags |= flagRawSig
			sigBytes = []byte(sig)
		}
	}
	p := make([]byte, 0, 1+modelHashLen+binary.MaxVarintLen64+len(geminiID)+len(sigBytes)+checkLen)
	p = append(p, flags)
	p = append(p, modelHash(model)...)
	p = binary.AppendUvarint(p, uint64(len(geminiID)))
	p = append(p, geminiID...)
	p = append(p, sigBytes...)
	sum := sha256.Sum256(p)
	p = append(p, sum[:checkLen]...)
	return toolIDPrefix + base64.RawURLEncoding.EncodeToString(p)
}

// decodedID is what a client's tool-call id means for a request to model.
type decodedID struct {
	// geminiID is the functionCall.id and functionResponse.id to send:
	// the Gemini id inside an id this adapter made, else the id as the
	// client sent it (another provider's, a hand-written one, or one
	// damaged in transit, which the check catches).
	geminiID string
	// sig is the call's thought signature, or "" if it had none or came
	// from another model: a signature is only ever sent back to the model
	// that made it.
	sig string
	// ours is set when this adapter made the id, for any model.
	ours bool
	// sameModel is set when it made the id for model.
	sameModel bool
}

// decodeToolCallID decodes a tool-call id the client sent back in a
// request to model.
func decodeToolCallID(id, model string) decodedID {
	foreign := decodedID{geminiID: id}
	rest, ok := strings.CutPrefix(id, toolIDPrefix)
	if !ok {
		return foreign
	}
	p, err := base64.RawURLEncoding.Strict().DecodeString(rest)
	if err != nil || len(p) < 1+modelHashLen+1+checkLen {
		return foreign
	}
	body, check := p[:len(p)-checkLen], p[len(p)-checkLen:]
	if sum := sha256.Sum256(body); !bytes.Equal(sum[:checkLen], check) {
		return foreign
	}
	flags, hash := body[0], body[1:1+modelHashLen]
	if flags&^(flagSignature|flagRawSig) != 0 {
		return foreign
	}
	n, k := binary.Uvarint(body[1+modelHashLen:])
	if k <= 0 || n > maxGeminiIDLen {
		return foreign
	}
	start := 1 + modelHashLen + k
	end := start + int(n) // n <= maxGeminiIDLen
	if end > len(body) {
		return foreign
	}
	out := decodedID{geminiID: string(body[start:end]), ours: true, sameModel: bytes.Equal(hash, modelHash(model))}
	sig := body[end:]
	switch {
	case flags&flagSignature == 0:
		if len(sig) != 0 {
			return foreign
		}
	case !out.sameModel:
	case flags&flagRawSig != 0:
		out.sig = string(sig)
	default:
		out.sig = base64.StdEncoding.EncodeToString(sig)
	}
	return out
}

// newCallID makes up a function call id for a call Gemini returned
// without one: older models do not set functionCall.id.
func newCallID() string {
	var b [12]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails
	return "call_" + hex.EncodeToString(b[:])
}
