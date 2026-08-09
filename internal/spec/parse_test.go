// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

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

// TestParseVolumes pins the JSON spelling of RFC 018 §2.7's declaration.
//
// Volumes is the first field the Resource declaration has gained that is
// itself a list of structs, so it is the first place DisallowUnknownFields
// has to reach two levels down. A misspelled JSON tag or a volume that
// quietly absorbs an unexpected key would both pass every test in
// validate_test.go, which builds its Volumes in Go.
func TestParseVolumes(t *testing.T) {
	resource := func(volumes string) string {
		return `{
			"sdd_version": "1.0",
			"intent": "deploy",
			"resources": [
				{
					"id": "api",
					"type": "container_service",
					"provider": "agnostic",
					"properties": {"image": "ghcr.io/acme/api@sha256:abc"},
					"volumes": ` + volumes + `
				}
			]
		}`
	}

	tests := []struct {
		name    string
		input   string
		want    *Volume
		wantErr bool
	}{
		{
			name:  "every field of a volume decodes",
			input: resource(`[{"name": "uploads", "mount_path": "/var/lib/uploads", "size_gb": 100}]`),
			want:  &Volume{Name: "uploads", MountPath: "/var/lib/uploads", SizeGB: 100},
		},
		{
			name:  "size_gb is optional",
			input: resource(`[{"name": "uploads", "mount_path": "/var/lib/uploads"}]`),
			want:  &Volume{Name: "uploads", MountPath: "/var/lib/uploads"},
		},
		{
			name:    "unknown field inside a volume rejected (mass assignment)",
			input:   resource(`[{"name": "uploads", "mount_path": "/var/lib/uploads", "read_only": false}]`),
			wantErr: true,
		},
		{
			name:    "a volume that is not an object",
			input:   resource(`["uploads"]`),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := Parse(strings.NewReader(tt.input))
			if (err != nil) != tt.wantErr {
				t.Fatalf("Parse() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.want == nil {
				return
			}
			got := s.Resources[0].Volumes
			if len(got) != 1 {
				t.Fatalf("Volumes = %v, want exactly one", got)
			}
			if got[0] != *tt.want {
				t.Fatalf("Volumes[0] = %+v, want %+v", got[0], *tt.want)
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
