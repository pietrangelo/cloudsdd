// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package engine

import (
	"reflect"
	"testing"

	"cloudsdd/internal/spec"
)

func TestEffectiveRegions(t *testing.T) {
	tests := []struct {
		name string
		s    spec.Scope
		want []string
	}{
		{name: "unscoped", s: spec.Scope{}, want: []string{""}},
		{name: "single region", s: spec.Scope{Region: "eu-central-1"}, want: []string{"eu-central-1"}},
		{
			name: "multi-region",
			s:    spec.Scope{Regions: []string{"eu-central-1", "us-east-1"}},
			want: []string{"eu-central-1", "us-east-1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := effectiveRegions(tt.s); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("effectiveRegions() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestScopedResource(t *testing.T) {
	r := spec.Resource{
		ID:    "app-data",
		Scope: spec.Scope{Regions: []string{"eu-central-1", "us-east-1"}},
	}

	got := scopedResource(r, "eu-central-1")
	if got.Scope.Region != "eu-central-1" {
		t.Errorf("scopedResource().Scope.Region = %q, want %q", got.Scope.Region, "eu-central-1")
	}
	if got.Scope.Regions != nil {
		t.Errorf("scopedResource().Scope.Regions = %v, want nil", got.Scope.Regions)
	}
	if len(r.Scope.Regions) != 2 {
		t.Errorf("scopedResource() mutated the original resource's Scope.Regions: %v", r.Scope.Regions)
	}
}
