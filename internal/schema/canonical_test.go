package schema

import (
	"testing"
)

func TestMembers(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []string // name=value pairs, in order
	}{
		{"original order", `{"model":"m","messages":[{"role":"user","content":"hi"}],"zeta":1,"alpha":{"b":2}}`,
			[]string{`model="m"`, `messages=[{"role":"user","content":"hi"}]`, `zeta=1`, `alpha={"b":2}`}},
		{"duplicate keeps first position and last value", `{"model":"m","x":1,"messages":[{"role":"user"}],"x":2}`,
			[]string{`model="m"`, `x=2`, `messages=[{"role":"user"}]`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := DecodeChatRequest([]byte(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for k, v := range r.Members() {
				got = append(got, k+"="+string(v))
			}
			if len(got) != len(tt.want) {
				t.Fatalf("Members() = %q, want %q", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("member %d = %s, want %s", i, got[i], tt.want[i])
				}
			}
		})
	}

	t.Run("early stop", func(t *testing.T) {
		r, err := DecodeChatRequest([]byte(`{"model":"m","messages":[{"role":"user"}],"a":1}`))
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for range r.Members() {
			n++
			break
		}
		if n != 1 {
			t.Errorf("iterated %d members after break, want 1", n)
		}
	})
}
