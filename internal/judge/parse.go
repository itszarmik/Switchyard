package judge

import (
	"encoding/json"
	"strings"
)

// Winners a judge run can name.
const (
	winnerA   = "A"
	winnerB   = "B"
	winnerTie = "tie"
)

// parseWinner extracts the winner from a judge reply. It accepts the JSON
// object alone, inside a code fence, or surrounded by prose: the last JSON
// object with a string "winner" member of A, B or tie (in any case)
// decides. It reports false when there is none.
//
// The last object, rather than the first or exactly one: the judge is told
// to reply with the object only, and with structured output it can reply
// nothing else, so one object is the norm. A judge that does write more
// gives its own verdict last, after any reasoning, and may have quoted an
// object from an answer before it (an injected {"winner": …} is the
// obvious attack). Taking the last reads the judge's verdict in both
// cases, where requiring exactly one would turn every quotation into a
// judge failure that an answer could provoke at will.
func parseWinner(text string) (string, bool) {
	winner, found := "", false
	for i := strings.IndexByte(text, '{'); i >= 0; {
		var obj map[string]json.RawMessage
		dec := json.NewDecoder(strings.NewReader(text[i:]))
		if dec.Decode(&obj) == nil {
			if raw, ok := obj["winner"]; ok {
				var w string
				if json.Unmarshal(raw, &w) == nil {
					if v, ok := normalizeWinner(w); ok {
						winner, found = v, true
					}
				}
			}
		}
		next := strings.IndexByte(text[i+1:], '{')
		if next < 0 {
			break
		}
		i += 1 + next
	}
	return winner, found
}

// normalizeWinner maps a winner value to winnerA, winnerB or winnerTie.
func normalizeWinner(w string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(w)) {
	case "a", "answer a", "answer_a":
		return winnerA, true
	case "b", "answer b", "answer_b":
		return winnerB, true
	case "tie":
		return winnerTie, true
	}
	return "", false
}
