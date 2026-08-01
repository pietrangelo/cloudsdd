// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package provider

import (
	"errors"
	"testing"
)

func TestValidateRegionAllowed(t *testing.T) {
	tests := []struct {
		name    string
		region  string
		allowed []string
		wantErr bool
	}{
		{
			// An empty policy means "no constraint declared", not
			// "nothing is allowed".
			name:    "no policy allows anything",
			region:  "eu-central-1",
			allowed: nil,
		},
		{
			name:    "empty policy slice allows anything",
			region:  "eu-central-1",
			allowed: []string{},
		},
		{
			name:    "region in single-entry policy",
			region:  "eu-central-1",
			allowed: []string{"eu-central-1"},
		},
		{
			name:    "region in multi-entry policy",
			region:  "us-east-1",
			allowed: []string{"eu-central-1", "us-east-1", "ap-south-1"},
		},
		{
			name:    "region outside policy",
			region:  "us-east-1",
			allowed: []string{"eu-central-1"},
			wantErr: true,
		},
		{
			// Matching is exact: a region must not pass because it is a
			// prefix or substring of an allowed one.
			name:    "prefix does not match",
			region:  "eu-central",
			allowed: []string{"eu-central-1"},
			wantErr: true,
		},
		{
			name:    "case sensitive",
			region:  "EU-CENTRAL-1",
			allowed: []string{"eu-central-1"},
			wantErr: true,
		},
		{
			name:    "empty region against a policy",
			region:  "",
			allowed: []string{"eu-central-1"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRegionAllowed(tt.region, tt.allowed)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("ValidateRegionAllowed(%q, %v) = nil, want an error", tt.region, tt.allowed)
				}
				if !errors.Is(err, ErrRegionNotAllowed) {
					t.Errorf("error = %v, want it to wrap ErrRegionNotAllowed", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateRegionAllowed(%q, %v) unexpected error: %v", tt.region, tt.allowed, err)
			}
		})
	}
}
