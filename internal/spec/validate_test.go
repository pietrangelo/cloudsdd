// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package spec

import (
	"fmt"
	"testing"
)

func validSpec() Specification {
	return Specification{
		SDDVersion: "1.0",
		Intent:     IntentDeploy,
		Resources: []Resource{
			{
				ID:         "app-db",
				Type:       ResourceTypeRelationalDatabase,
				Provider:   ProviderAgnostic,
				Properties: map[string]any{"engine": "postgres"},
			},
		},
	}
}

func TestValidate(t *testing.T) {
	maxCost := 100.0
	negativeCost := -1.0

	tests := []struct {
		name    string
		mutate  func(s *Specification)
		wantErr bool
	}{
		{
			name:    "valid specification",
			mutate:  func(s *Specification) {},
			wantErr: false,
		},
		{
			name: "valid specification with policies",
			mutate: func(s *Specification) {
				s.Policies = Policies{MaxCostMonthly: &maxCost, AllowedRegions: []string{"eu-central-1"}}
			},
			wantErr: false,
		},
		{
			name:    "wrong sdd_version",
			mutate:  func(s *Specification) { s.SDDVersion = "2.0" },
			wantErr: true,
		},
		{
			name:    "missing sdd_version",
			mutate:  func(s *Specification) { s.SDDVersion = "" },
			wantErr: true,
		},
		{
			name:    "invalid intent",
			mutate:  func(s *Specification) { s.Intent = "annihilate" },
			wantErr: true,
		},
		{
			name:    "empty resources",
			mutate:  func(s *Specification) { s.Resources = nil },
			wantErr: true,
		},
		{
			name:    "invalid resource id with path traversal characters",
			mutate:  func(s *Specification) { s.Resources[0].ID = "../../etc/passwd" },
			wantErr: true,
		},
		{
			name:    "invalid resource id too long",
			mutate:  func(s *Specification) { s.Resources[0].ID = string(make([]byte, 64)) },
			wantErr: true,
		},
		{
			name:    "invalid resource type",
			mutate:  func(s *Specification) { s.Resources[0].Type = "nuclear_reactor" },
			wantErr: true,
		},
		{
			name:    "cross_account_role is a valid resource type",
			mutate:  func(s *Specification) { s.Resources[0].Type = ResourceTypeCrossAccountRole },
			wantErr: false,
		},
		{
			name:    "valid account reference",
			mutate:  func(s *Specification) { s.Resources[0].Account = "prod-target" },
			wantErr: false,
		},
		{
			name:    "account reference too long",
			mutate:  func(s *Specification) { s.Resources[0].Account = string(make([]byte, 65)) },
			wantErr: true,
		},
		{
			name:    "valid scope with environment and region",
			mutate:  func(s *Specification) { s.Resources[0].Scope = Scope{Environment: "staging", Region: "eu-central-1"} },
			wantErr: false,
		},
		{
			name:    "scope environment too long",
			mutate:  func(s *Specification) { s.Resources[0].Scope = Scope{Environment: string(make([]byte, 33))} },
			wantErr: true,
		},
		{
			name: "scope region and regions mutually exclusive",
			mutate: func(s *Specification) {
				s.Resources[0].Scope = Scope{Region: "eu-central-1", Regions: []string{"eu-west-1", "us-east-1"}}
			},
			wantErr: true,
		},
		{
			name:    "scope regions with a single entry rejected",
			mutate:  func(s *Specification) { s.Resources[0].Scope = Scope{Regions: []string{"eu-central-1"}} },
			wantErr: true,
		},
		{
			name: "scope regions with more than 10 entries rejected",
			mutate: func(s *Specification) {
				regions := make([]string, 11)
				for i := range regions {
					regions[i] = fmt.Sprintf("region-%d", i)
				}
				s.Resources[0].Scope = Scope{Regions: regions}
			},
			wantErr: true,
		},
		{
			name: "scope zones with more than 10 entries rejected",
			mutate: func(s *Specification) {
				zones := make([]string, 11)
				for i := range zones {
					zones[i] = fmt.Sprintf("zone-%d", i)
				}
				s.Resources[0].Scope = Scope{Zones: zones}
			},
			wantErr: true,
		},
		{
			name:    "invalid provider",
			mutate:  func(s *Specification) { s.Resources[0].Provider = "on-premise" },
			wantErr: true,
		},
		{
			name:    "nil properties",
			mutate:  func(s *Specification) { s.Resources[0].Properties = nil },
			wantErr: true,
		},
		{
			name:    "negative max_cost_monthly",
			mutate:  func(s *Specification) { s.Policies.MaxCostMonthly = &negativeCost },
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := validSpec()
			tt.mutate(&s)
			err := Validate(&s)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
