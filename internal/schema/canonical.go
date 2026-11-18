package schema

import (
	"encoding/json"
	"iter"
)

// Members returns the request's top-level members, name and raw value, in
// the order the client first sent them; a name that appeared more than once
// yields its last value once. The raw values must not be modified. The
// response cache derives its canonical form from them (internal/cache).
func (r *ChatRequest) Members() iter.Seq2[string, json.RawMessage] {
	return func(yield func(string, json.RawMessage) bool) {
		for _, k := range r.obj.keys {
			if !yield(k, r.obj.vals[k]) {
				return
			}
		}
	}
}
