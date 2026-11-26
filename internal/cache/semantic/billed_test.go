package semantic

import (
	"errors"
	"testing"

	"switchyard/internal/providers"
)

// TestMayHaveBilled: an embedding call's failure counts as billed (and is
// charged its estimate) as providers.MayHaveBilled says: an *Error whose
// upstream billed it whatever its kind, a kind that may have, and any
// other error.
func TestMayHaveBilled(t *testing.T) {
	for _, tt := range []struct {
		err  error
		want bool
	}{
		{&providers.Error{Kind: providers.KindUnsupported, Billed: true}, true},
		{&providers.Error{Kind: providers.KindUnsupported}, false},
		{&providers.Error{Kind: providers.KindClientError}, false},
		{&providers.Error{Kind: providers.KindServerError}, true},
		{errors.New("connection reset"), true},
	} {
		if got := mayHaveBilled(tt.err); got != tt.want {
			t.Errorf("mayHaveBilled(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}
