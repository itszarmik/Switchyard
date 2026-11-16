package evaltest

import (
	"encoding/json"
	"os"
	"testing"

	"switchyard/internal/eval"
)

// TestStarterPromptSet checks that the starter set loads and carries every
// prompt of the labeled sanity set it was built from, with its category.
func TestStarterPromptSet(t *testing.T) {
	name, prompts, err := eval.LoadPrompts("prompts/starter.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if name != "starter" {
		t.Errorf("set name %q, want starter", name)
	}
	data, err := os.ReadFile("../../internal/quality/testdata/sanity.json")
	if err != nil {
		t.Fatal(err)
	}
	var sanity []struct {
		ID       string `json:"id"`
		Category string `json:"category"`
	}
	if err := json.Unmarshal(data, &sanity); err != nil {
		t.Fatal(err)
	}
	if len(prompts) != len(sanity) {
		t.Fatalf("%d starter prompts, %d sanity prompts", len(prompts), len(sanity))
	}
	for i, p := range prompts {
		if p.ID != sanity[i].ID || p.Category != sanity[i].Category {
			t.Errorf("prompt %d: %s/%s, sanity %s/%s", i, p.ID, p.Category, sanity[i].ID, sanity[i].Category)
		}
	}
}
