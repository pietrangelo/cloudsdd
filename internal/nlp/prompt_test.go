// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package nlp

import (
	"strings"
	"testing"
)

// TestSystemPromptDocumentsTheSchema is the regression test for RFC 011
// §1.1H1. The prompt used to be triplicated, and only the Anthropic copy
// documented the resource types and per-type properties — the OpenAI and
// Ollama copies shipped an empty `"properties": {}` and no type list, so
// those providers emitted specifications that failed validation far more
// often. There is now one prompt, and it must stay complete.
func TestSystemPromptDocumentsTheSchema(t *testing.T) {
	required := []string{
		// Resource types
		"object_storage",
		"relational_database",
		"cross_account_role",
		// Providers
		"agnostic", "aws", "gcp", "azure",
		// Envelope
		"sdd_version", "intent", "resources", "policies", "scope",
		// Per-type properties
		"bucket_name", "versioning", "encryption", "block_public_access",
		"engine", "version", "high_availability",
		// RFC 011 §2.5 additions
		"force_destroy", "deletion_protection", "skip_final_snapshot",
		// Policy
		"allowed_regions",
	}

	for _, want := range required {
		if !strings.Contains(systemPrompt, want) {
			t.Errorf("systemPrompt does not document %q", want)
		}
	}
}

// TestSystemPromptDropsRemovedPolicy: max_cost_monthly was removed from
// the schema in RFC 011 §2.9, so the prompt must not invite it — a spec
// setting it is now rejected by DisallowUnknownFields.
func TestSystemPromptDropsRemovedPolicy(t *testing.T) {
	if strings.Contains(systemPrompt, "max_cost_monthly") {
		t.Error("systemPrompt still advertises max_cost_monthly, which was removed from the schema")
	}
}

func TestSystemPromptForbidsCredentials(t *testing.T) {
	if !strings.Contains(systemPrompt, "credential") {
		t.Error("systemPrompt does not warn against emitting credential-like properties")
	}
}

func TestWithLedgerContext(t *testing.T) {
	tests := []struct {
		name        string
		contextJSON string
		wantAppend  bool
	}{
		{name: "empty", contextJSON: "", wantAppend: false},
		{name: "whitespace", contextJSON: "  \n ", wantAppend: false},
		{name: "empty object", contextJSON: "{}", wantAppend: false},
		{name: "null", contextJSON: "null", wantAppend: false},
		{name: "real ledger", contextJSON: `{"resources":[{"id":"db"}]}`, wantAppend: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := withLedgerContext(systemPrompt, tt.contextJSON)

			if !strings.HasPrefix(got, systemPrompt) {
				t.Fatal("withLedgerContext() did not preserve the base prompt as a prefix")
			}

			appended := len(got) > len(systemPrompt)
			if appended != tt.wantAppend {
				t.Fatalf("appended = %v, want %v (input %q)", appended, tt.wantAppend, tt.contextJSON)
			}

			if tt.wantAppend {
				if !strings.Contains(got, tt.contextJSON) {
					t.Error("ledger content is missing from the prompt")
				}
				// RFC 011 §4: the ledger is populated from prior model
				// output, so it must be fenced and labelled as data.
				if !strings.Contains(got, "UNTRUSTED DATA") {
					t.Error("ledger context is not labelled as untrusted")
				}
				if !strings.Contains(got, "<deployed_infrastructure>") {
					t.Error("ledger context is not delimited")
				}
			}
		})
	}
}

// TestWithLedgerContextCapsSize covers the bound from RFC 011 §4: the
// ledger grows without limit as infrastructure accumulates.
func TestWithLedgerContextCapsSize(t *testing.T) {
	huge := `{"resources":[` + strings.Repeat(`{"id":"x"},`, 20000) + `{"id":"y"}]}`
	if len(huge) <= maxLedgerContextBytes {
		t.Fatalf("test fixture is only %d bytes; it must exceed the %d-byte cap", len(huge), maxLedgerContextBytes)
	}

	got := withLedgerContext(systemPrompt, huge)

	if len(got) > len(systemPrompt)+maxLedgerContextBytes+512 {
		t.Errorf("prompt grew to %d bytes; the ledger was not capped", len(got))
	}
	if !strings.Contains(got, "[truncated]") {
		t.Error("oversized ledger was not marked as truncated")
	}
}
