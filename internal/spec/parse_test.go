package spec

import (
	"errors"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{
			name: "valid minimal specification",
			input: `{
				"sdd_version": "1.0",
				"intent": "deploy",
				"resources": [
					{
						"id": "app-db",
						"type": "relational_database",
						"provider": "agnostic",
						"properties": {"engine": "postgres"}
					}
				]
			}`,
			wantErr: false,
		},
		{
			name:    "malformed json",
			input:   `{"sdd_version": "1.0",`,
			wantErr: true,
		},
		{
			name: "unknown top-level field rejected (mass assignment)",
			input: `{
				"sdd_version": "1.0",
				"intent": "deploy",
				"resources": [],
				"admin_override": true
			}`,
			wantErr: true,
		},
		{
			name: "unknown resource field rejected (mass assignment)",
			input: `{
				"sdd_version": "1.0",
				"intent": "deploy",
				"resources": [
					{
						"id": "app-db",
						"type": "relational_database",
						"provider": "agnostic",
						"properties": {},
						"internal_state": "should-not-exist"
					}
				]
			}`,
			wantErr: true,
		},
		{
			name:    "trailing data rejected",
			input:   `{"sdd_version": "1.0", "intent": "deploy", "resources": []} {"extra": true}`,
			wantErr: true,
		},
		{
			name:    "empty body",
			input:   ``,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(tt.input))
			if (err != nil) != tt.wantErr {
				t.Fatalf("Parse() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestParse_TrailingDataSentinel(t *testing.T) {
	_, err := Parse(strings.NewReader(`{"sdd_version": "1.0", "intent": "deploy", "resources": []} {}`))
	if !errors.Is(err, ErrTrailingData) {
		t.Fatalf("expected ErrTrailingData, got %v", err)
	}
}

func TestParseAndValidate(t *testing.T) {
	valid := `{
		"sdd_version": "1.0",
		"intent": "deploy",
		"resources": [
			{
				"id": "app-db",
				"type": "relational_database",
				"provider": "agnostic",
				"properties": {"engine": "postgres"}
			}
		]
	}`
	s, err := ParseAndValidate(strings.NewReader(valid))
	if err != nil {
		t.Fatalf("ParseAndValidate() unexpected error: %v", err)
	}
	if s.SDDVersion != "1.0" {
		t.Fatalf("SDDVersion = %q, want 1.0", s.SDDVersion)
	}

	invalid := `{"sdd_version": "2.0", "intent": "deploy", "resources": []}`
	if _, err := ParseAndValidate(strings.NewReader(invalid)); err == nil {
		t.Fatal("ParseAndValidate() expected validation error, got nil")
	}
}
