package config

import (
	"os"
	"testing"

	"go.yaml.in/yaml/v3"
)

// TestGeminiProviderTypes checks that a google provider validates both as
// type gemini (the native API) and as type openai against Google's
// OpenAI-compatible endpoint.
func TestGeminiProviderTypes(t *testing.T) {
	for name, p := range map[string]string{
		"native":        "type: gemini\n    base_url: https://generativelanguage.googleapis.com/v1beta",
		"compatibility": "type: openai\n    base_url: https://generativelanguage.googleapis.com/v1beta/openai",
	} {
		t.Run(name, func(t *testing.T) {
			root := "version: 1\nproviders:\n  - id: google\n    " + p + "\n    api_key: ${GEMINI_API_KEY}\n"
			snap := mustLoad(t, root, map[string]string{"GEMINI_API_KEY": "AIzaSyDummyDummyDummyDummyDummyDummy000"}, nil)
			if got := snap.Config().Providers[0]; got.ID != "google" || got.APIKey.Value() == "" {
				t.Errorf("provider %+v", got)
			}
		})
	}
}

// TestShippedConfigsUseNativeGemini checks that every config the project
// ships (the example, the compose stack's and the Helm chart's) points the
// google provider at the native API, with a total timeout that fits the
// routing.request_timeout they ship (10m), which caps every call.
func TestShippedConfigsUseNativeGemini(t *testing.T) {
	for _, path := range []string{
		"../../configs/switchyard.example.yaml",
		"../../deploy/config/switchyard.yaml",
		"../../deploy/helm/switchyard/files/switchyard.yaml",
	} {
		t.Run(path, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var f struct {
				Providers []struct {
					ID       string `yaml:"id"`
					Type     string `yaml:"type"`
					BaseURL  string `yaml:"base_url"`
					Timeouts struct {
						Total string `yaml:"total"`
					} `yaml:"timeouts"`
				} `yaml:"providers"`
			}
			if err := yaml.Unmarshal(raw, &f); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, p := range f.Providers {
				if p.ID != "google" {
					continue
				}
				found = true
				if p.Type != "gemini" || p.BaseURL != "https://generativelanguage.googleapis.com/v1beta" || p.Timeouts.Total != "10m" {
					t.Errorf("google provider %+v, want type gemini, the v1beta root and total 10m", p)
				}
			}
			if !found {
				t.Error("no google provider")
			}
		})
	}
}
