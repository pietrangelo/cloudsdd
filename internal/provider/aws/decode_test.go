// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package aws

import "testing"

type decodeTarget struct {
	Name string `json:"name" validate:"required"`
}

func TestDecodeProperties(t *testing.T) {
	tests := []struct {
		name    string
		props   map[string]any
		wantErr bool
	}{
		{
			name:    "valid properties",
			props:   map[string]any{"name": "ok"},
			wantErr: false,
		},
		{
			name:    "unknown field rejected (mass assignment defense)",
			props:   map[string]any{"name": "ok", "unexpected": "field"},
			wantErr: true,
		},
		{
			name:    "missing required field rejected",
			props:   map[string]any{},
			wantErr: true,
		},
		{
			name:    "access_key-like field rejected regardless of schema",
			props:   map[string]any{"name": "ok", "access_key": "AKIA..."},
			wantErr: true,
		},
		{
			name:    "secret-like field rejected regardless of schema",
			props:   map[string]any{"name": "ok", "aws_secret": "shh"},
			wantErr: true,
		},
		{
			name:    "password-like field rejected regardless of schema",
			props:   map[string]any{"name": "ok", "db_password": "shh"},
			wantErr: true,
		},
		{
			name:    "token-like field rejected regardless of schema",
			props:   map[string]any{"name": "ok", "session_token": "shh"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out decodeTarget
			err := decodeProperties(tt.props, &out)
			if (err != nil) != tt.wantErr {
				t.Fatalf("decodeProperties() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestBoolOrDefault(t *testing.T) {
	yes := true
	no := false

	if got := boolOrDefault(nil, true); got != true {
		t.Fatalf("boolOrDefault(nil, true) = %v, want true", got)
	}
	if got := boolOrDefault(nil, false); got != false {
		t.Fatalf("boolOrDefault(nil, false) = %v, want false", got)
	}
	if got := boolOrDefault(&yes, false); got != true {
		t.Fatalf("boolOrDefault(&true, false) = %v, want true", got)
	}
	if got := boolOrDefault(&no, true); got != false {
		t.Fatalf("boolOrDefault(&false, true) = %v, want false", got)
	}
}
