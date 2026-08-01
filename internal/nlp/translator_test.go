// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package nlp

import (
	"strings"
	"testing"
)

// Compile-time assertions that every implementation satisfies the contract.
var (
	_ Translator = (*AnthropicTranslator)(nil)
	_ Translator = (*OpenAITranslator)(nil)
	_ Translator = (*OllamaTranslator)(nil)
)

// validSpecJSON is the minimum a translator must produce to pass
// spec.ParseAndValidate.
const validSpecJSON = `{
  "sdd_version": "1.0",
  "intent": "deploy",
  "resources": [
    {
      "id": "my-bucket",
      "type": "object_storage",
      "provider": "aws",
      "scope": {"region": "eu-central-1"},
      "properties": {"bucket_name": "my-bucket"}
    }
  ]
}`

func TestStripMarkdownFence(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "bare json", in: `{"a":1}`, want: `{"a":1}`},
		{name: "json fence", in: "```json\n{\"a\":1}\n```", want: `{"a":1}`},
		{name: "plain fence", in: "```\n{\"a\":1}\n```", want: `{"a":1}`},
		{name: "surrounding whitespace", in: "  \n{\"a\":1}\n  ", want: `{"a":1}`},
		{name: "fence with whitespace", in: "  ```json\n{\"a\":1}\n```  ", want: `{"a":1}`},
		{name: "empty", in: "", want: ""},
		{name: "only a fence", in: "```", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripMarkdownFence(tt.in); got != tt.want {
				t.Errorf("stripMarkdownFence(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseSpecification(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "valid", raw: validSpecJSON},
		{name: "valid wrapped in a fence", raw: "```json\n" + validSpecJSON + "\n```"},
		{name: "empty response", raw: "", wantErr: "empty response"},
		{name: "whitespace only", raw: "   \n  ", wantErr: "empty response"},
		{name: "not json", raw: "I cannot help with that.", wantErr: "failed to parse or validate"},
		{name: "wrong sdd_version", raw: `{"sdd_version":"2.0","intent":"deploy","resources":[]}`, wantErr: "failed to parse or validate"},
		{name: "missing intent", raw: `{"sdd_version":"1.0","resources":[]}`, wantErr: "failed to parse or validate"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSpecification("test", tt.raw)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("parseSpecification() error = nil, want error containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseSpecification() error = %q, want it to contain %q", err, tt.wantErr)
				}
				if !strings.HasPrefix(err.Error(), "test: ") {
					t.Errorf("parseSpecification() error = %q, want it prefixed with the provider name", err)
				}
				return
			}

			if err != nil {
				t.Fatalf("parseSpecification() unexpected error: %v", err)
			}
			if len(got.Resources) != 1 || got.Resources[0].ID != "my-bucket" {
				t.Errorf("parseSpecification() = %+v, want the single bucket resource", got)
			}
		})
	}
}

// TestParseSpecificationTruncatesRawOutput covers RFC 011 §4: the raw
// model output derives from a prompt containing the ledger, so an
// unbounded echo can spill infrastructure detail into logs.
func TestParseSpecificationTruncatesRawOutput(t *testing.T) {
	huge := strings.Repeat("x", maxErrorOutputBytes*3)

	_, err := parseSpecification("test", huge)
	if err == nil {
		t.Fatal("parseSpecification() error = nil, want a parse failure")
	}
	if len(err.Error()) > maxErrorOutputBytes*2 {
		t.Errorf("error message is %d bytes; raw output was not truncated", len(err.Error()))
	}
	if !strings.Contains(err.Error(), "[truncated]") {
		t.Error("error message does not mark the output as truncated")
	}
}

func TestTruncateForError(t *testing.T) {
	tests := []struct {
		name         string
		in           string
		wantTrunc    bool
		wantContains string
	}{
		{name: "short passes through", in: "hello", wantContains: "hello"},
		{name: "exactly at limit", in: strings.Repeat("a", maxErrorOutputBytes)},
		{name: "over limit truncated", in: strings.Repeat("a", maxErrorOutputBytes+1), wantTrunc: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateForError(tt.in)

			if tt.wantTrunc && !strings.HasSuffix(got, "[truncated]") {
				t.Error("expected the value to be marked as truncated")
			}
			if !tt.wantTrunc && strings.HasSuffix(got, "[truncated]") {
				t.Error("value was truncated unexpectedly")
			}
			if tt.wantContains != "" && !strings.Contains(got, tt.wantContains) {
				t.Errorf("truncateForError() = %q, want it to contain %q", got, tt.wantContains)
			}
		})
	}
}

func TestNewTranslator(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("OPENAI_API_KEY", "test-key")

	tests := []struct {
		name     string
		provider string
		wantErr  string
	}{
		{name: "anthropic", provider: "anthropic"},
		{name: "openai", provider: "openai"},
		{name: "ollama", provider: "ollama"},
		{name: "unknown", provider: "mystery", wantErr: "unsupported AI provider"},
		{name: "empty", provider: "", wantErr: "unsupported AI provider"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewTranslator(tt.provider, "")

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("NewTranslator(%q) error = nil, want %q", tt.provider, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("NewTranslator() error = %q, want it to contain %q", err, tt.wantErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("NewTranslator(%q) unexpected error: %v", tt.provider, err)
			}
			if got == nil {
				t.Fatal("NewTranslator() returned a nil Translator")
			}
		})
	}
}

func TestConstructorsRequireAPIKeys(t *testing.T) {
	tests := []struct {
		name   string
		envVar string
		build  func(string) (Translator, error)
	}{
		{
			name:   "anthropic",
			envVar: "ANTHROPIC_API_KEY",
			build:  func(m string) (Translator, error) { return NewAnthropicTranslator(m) },
		},
		{
			name:   "openai",
			envVar: "OPENAI_API_KEY",
			build:  func(m string) (Translator, error) { return NewOpenAITranslator(m) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(tt.envVar, "")

			if _, err := tt.build(""); err == nil {
				t.Fatalf("constructor error = nil, want an error when %s is unset", tt.envVar)
			}
		})
	}
}

// TestDefaultModelsAreApplied pins the per-provider defaults, including
// the RFC 011 §1.1B3 fix away from the retired Claude 3.5 snapshot.
func TestDefaultModelsAreApplied(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("OPENAI_API_KEY", "test-key")

	a, err := NewAnthropicTranslator("")
	if err != nil {
		t.Fatalf("NewAnthropicTranslator() error: %v", err)
	}
	if a.model != DefaultAnthropicModel {
		t.Errorf("anthropic model = %q, want %q", a.model, DefaultAnthropicModel)
	}

	o, err := NewOpenAITranslator("")
	if err != nil {
		t.Fatalf("NewOpenAITranslator() error: %v", err)
	}
	if o.model != DefaultOpenAIModel {
		t.Errorf("openai model = %q, want %q", o.model, DefaultOpenAIModel)
	}

	ol, err := NewOllamaTranslator("")
	if err != nil {
		t.Fatalf("NewOllamaTranslator() error: %v", err)
	}
	if ol.model != DefaultOllamaModel {
		t.Errorf("ollama model = %q, want %q", ol.model, DefaultOllamaModel)
	}
}

func TestExplicitModelOverridesDefault(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "test-key")

	a, err := NewAnthropicTranslator("claude-sonnet-5")
	if err != nil {
		t.Fatalf("NewAnthropicTranslator() error: %v", err)
	}
	if a.model != "claude-sonnet-5" {
		t.Errorf("model = %q, want the explicitly requested one", a.model)
	}
}
