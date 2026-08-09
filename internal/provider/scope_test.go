// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package provider

import (
	"maps"
	"testing"

	"cloudsdd/internal/provider/network"
	"cloudsdd/internal/spec"
)

// TestNetworkScopeOf: the address model is deliberately narrower than a
// NetworkScope. What this conversion drops matters more than what it
// carries, so the table asserts the drops.
func TestNetworkScopeOf(t *testing.T) {
	tests := []struct {
		name  string
		scope NetworkScope
		want  network.Scope
	}{
		{
			name: "every dimension carries",
			scope: NetworkScope{
				Provider:    spec.ProviderAWS,
				Account:     "prod",
				Environment: "live",
				Region:      "eu-central-1",
			},
			want: network.Scope{Account: "prod", Environment: "live", Region: "eu-central-1"},
		},
		{
			// Legal, and it means "nobody said where this belongs"
			// (RFC 016 §7.4). It derives a scope rather than an error.
			name:  "unscoped",
			scope: NetworkScope{},
			want:  network.Scope{},
		},
		{
			// The provider is part of the network's identity but not of
			// its address: two clouds deriving the same range is the
			// correct outcome, because CloudSDD creates no route between
			// them. Were the provider in the key, moving a workload from
			// AWS to GCP would silently re-address it.
			name:  "the provider is not part of the address key",
			scope: NetworkScope{Provider: spec.ProviderGCP, Environment: "live"},
			want:  network.Scope{Environment: "live"},
		},
		{
			// Sealed is a posture, not a boundary: it governs what may be
			// peered to the network, not which addresses it occupies.
			name:  "sealed does not change the address key",
			scope: NetworkScope{Environment: "live", Sealed: true},
			want:  network.Scope{Environment: "live"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NetworkScopeOf(tt.scope); got != tt.want {
				t.Errorf("NetworkScopeOf(%+v) = %+v, want %+v", tt.scope, got, tt.want)
			}
		})
	}
}

// TestAddressPolicyOf: the address package stays free of spec types, so
// this is the one place the operator's plan crosses over. An entry lost
// here is a pin silently ignored, which lands two scopes on one range.
func TestAddressPolicyOf(t *testing.T) {
	tests := []struct {
		name         string
		policies     spec.Policies
		wantBaseCIDR string
		wantScopes   map[string]string
	}{
		{
			// The zero Policy is valid and means "derive everything from
			// the default block", so an absent plan needs no error path.
			name:     "no network plan yields the zero policy",
			policies: spec.Policies{},
		},
		{
			name: "base cidr and pins carry",
			policies: spec.Policies{Network: &spec.NetworkPolicy{
				BaseCIDR: "10.64.0.0/12",
				Scopes:   map[string]string{"prod::live::eu-central-1": "10.64.16.0/20"},
			}},
			wantBaseCIDR: "10.64.0.0/12",
			wantScopes:   map[string]string{"prod::live::eu-central-1": "10.64.16.0/20"},
		},
		{
			// An empty NetworkPolicy is not the same shape as an absent
			// one in the Specification, but it must mean the same thing
			// here.
			name:     "an empty network plan is the zero policy",
			policies: spec.Policies{Network: &spec.NetworkPolicy{}},
		},
		{
			// Everything else in Policies belongs to other checks. A
			// leak would hand the address arithmetic inputs it has no
			// business reading.
			name: "unrelated policies do not leak into the address plan",
			policies: spec.Policies{
				AllowedRegions:     []string{"eu-central-1"},
				AllowedRegistries:  []string{"ghcr.io"},
				ProviderPreference: []spec.Provider{spec.ProviderAWS},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AddressPolicyOf(tt.policies)

			if got.BaseCIDR != tt.wantBaseCIDR {
				t.Errorf("AddressPolicyOf().BaseCIDR = %q, want %q", got.BaseCIDR, tt.wantBaseCIDR)
			}
			if !maps.Equal(got.Scopes, tt.wantScopes) {
				t.Errorf("AddressPolicyOf().Scopes = %v, want %v", got.Scopes, tt.wantScopes)
			}
		})
	}
}

// TestScopeName: the label appears in resource names and in every error
// message about a scope, so an unscoped deployment must read as something
// rather than as an empty pair of quotes.
func TestScopeName(t *testing.T) {
	tests := []struct {
		name  string
		scope NetworkScope
		want  string
	}{
		{
			name: "every dimension",
			scope: NetworkScope{
				Account: "prod", Environment: "live", Region: "eu-central-1",
			},
			want: "prod" + network.ScopeSeparator + "live" + network.ScopeSeparator + "eu-central-1",
		},
		{
			name:  "empty dimensions are omitted, not rendered blank",
			scope: NetworkScope{Environment: "live", Region: "eu-central-1"},
			want:  "live" + network.ScopeSeparator + "eu-central-1",
		},
		{
			name:  "unscoped has a name of its own",
			scope: NetworkScope{},
			want:  "default",
		},
		{
			// The provider is already in the stack name and in the error
			// prefix; repeating it in the label would say "aws" twice.
			name:  "the provider is not part of the label",
			scope: NetworkScope{Provider: spec.ProviderAzure},
			want:  "default",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ScopeName(tt.scope); got != tt.want {
				t.Errorf("ScopeName(%+v) = %q, want %q", tt.scope, got, tt.want)
			}
		})
	}
}

// TestShortHash pins the algorithm with golden values on purpose.
//
// This tag is what keeps two long scope names from colliding once GCP and
// Azure truncate them to fit their length limits, so it is baked into
// resource names that already exist in real infrastructure. Changing the
// hash renames every one of them — a replacement, not an update — which is
// why sha256 truncated to four bytes is a fixed decision and not an
// implementation detail free to drift.
func TestShortHash(t *testing.T) {
	// The width the two truncation sites budget for (name[:54] and
	// name[:31], each plus a separator).
	const wantLen = 8

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "empty input still yields a tag",
			input: "",
			want:  "e3b0c442",
		},
		{
			name:  "a scope label",
			input: "prod::live::eu-central-1",
			want:  "1e10f884",
		},
		{
			// One character apart, because that is the realistic
			// collision: two scopes differing only in their region
			// suffix, cut to the same prefix.
			name:  "a neighbouring scope label hashes elsewhere",
			input: "prod::live::eu-central-2",
			want:  "1f1904a2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ShortHash(tt.input)

			if got != tt.want {
				t.Errorf("ShortHash(%q) = %q, want %q — renaming every deployed network", tt.input, got, tt.want)
			}
			if len(got) != wantLen {
				t.Errorf("ShortHash(%q) is %d characters, want %d", tt.input, len(got), wantLen)
			}
			if ShortHash(tt.input) != got {
				t.Errorf("ShortHash(%q) is not stable within a process", tt.input)
			}
		})
	}
}

// TestResourceScope: the scope a resource looks its network up by has to
// be derived from the same fields the network stack was named for, or a
// resource looks for a network nobody built.
//
// The provider is a parameter rather than a per-package constant because
// it is the coarsest partition of all — an AWS account and an Azure
// subscription can never share a network — and a shared helper that
// guessed it would be guessing the one thing it cannot recover from the
// resource.
func TestResourceScope(t *testing.T) {
	providers := []spec.Provider{spec.ProviderAWS, spec.ProviderGCP, spec.ProviderAzure}

	tests := []struct {
		name     string
		resource spec.Resource
		want     NetworkScope
	}{
		{
			name: "fully scoped",
			resource: spec.Resource{
				Account: "prod",
				Scope:   spec.Scope{Environment: "live", Region: "eu-central-1"},
			},
			want: NetworkScope{Account: "prod", Environment: "live", Region: "eu-central-1"},
		},
		{
			// Legal, and it means "I have not told CloudSDD where this
			// belongs" (RFC 016 §7.4). It must still derive a scope
			// rather than an error.
			name:     "unscoped",
			resource: spec.Resource{},
			want:     NetworkScope{},
		},
		{
			// A network spans the region, not one zone within it.
			name: "zones do not narrow the network",
			resource: spec.Resource{
				Scope: spec.Scope{Region: "eu-central-1", Zones: []string{"a", "b"}},
			},
			want: NetworkScope{Region: "eu-central-1"},
		},
		{
			// Sealed is the resource's own posture toward other scopes.
			// The network it joins is the same either way, so carrying it
			// here would make one scope look like two.
			name: "sealed does not split the scope",
			resource: spec.Resource{
				Scope: spec.Scope{Environment: "live", Sealed: boolPtr(false)},
			},
			want: NetworkScope{Environment: "live"},
		},
	}

	for _, p := range providers {
		for _, tt := range tests {
			t.Run(string(p)+"/"+tt.name, func(t *testing.T) {
				want := tt.want
				want.Provider = p

				if got := ResourceScope(p, tt.resource); got != want {
					t.Errorf("ResourceScope(%q, %+v) = %+v, want %+v", p, tt.resource, got, want)
				}
			})
		}
	}
}

func boolPtr(b bool) *bool { return &b }
