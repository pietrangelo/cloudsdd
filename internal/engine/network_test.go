// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/provider/network"
	"cloudsdd/internal/spec"
)

// scopedSpec builds a deploy Specification whose resources carry the
// given (environment, region) pairs, all on one provider.
func scopedSpec(pairs ...[2]string) spec.Specification {
	s := spec.Specification{SDDVersion: "1.0", Intent: spec.IntentDeploy}
	for i, p := range pairs {
		s.Resources = append(s.Resources, spec.Resource{
			ID:         string(rune('a'+i)) + "-res",
			Type:       spec.ResourceTypeObjectStorage,
			Provider:   spec.ProviderAWS,
			Scope:      spec.Scope{Environment: p[0], Region: p[1]},
			Properties: map[string]any{"bucket_name": "b" + string(rune('a'+i))},
		})
	}
	return s
}

// TestEnsureNetworkOncePerScope covers RFC 016 §5.2 and §5.3. A scope's
// network is shared, so ten resources in one environment need one
// network: calling EnsureNetwork per resource would be ten Pulumi
// operations converging on the same state, and on a slow provider, ten
// chances to race.
func TestEnsureNetworkOncePerScope(t *testing.T) {
	tests := []struct {
		name  string
		spec  spec.Specification
		want  int
		scope string
	}{
		{
			name: "three resources in one scope share one network",
			spec: scopedSpec(
				[2]string{"dev", "eu-central-1"},
				[2]string{"dev", "eu-central-1"},
				[2]string{"dev", "eu-central-1"},
			),
			want: 1,
		},
		{
			name: "two environments in one region are two networks",
			spec: scopedSpec(
				[2]string{"dev", "eu-central-1"},
				[2]string{"prod", "eu-central-1"},
			),
			want: 2,
		},
		{
			name: "one environment in two regions is two networks",
			spec: scopedSpec(
				[2]string{"dev", "eu-central-1"},
				[2]string{"dev", "eu-west-1"},
			),
			want: 2,
		},
		{
			name: "an unscoped resource still has a network",
			spec: scopedSpec([2]string{"", ""}),
			want: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mp := &mockProvider{name: "aws"}
			e := New(map[spec.Provider]provider.CloudProvider{spec.ProviderAWS: mp})

			if _, err := e.Apply(context.Background(), tt.spec); err != nil {
				t.Fatalf("Apply() error = %v", err)
			}
			if got := len(mp.networkScopes); got != tt.want {
				t.Errorf("EnsureNetwork called %d times for %d scopes, want %d: %v",
					got, tt.want, tt.want, mp.networkScopes)
			}
		})
	}
}

// TestEnsureNetworkPrecedesApply is the ordering the whole design exists
// to guarantee. A resource applied before its network exists is a
// resource in the wrong network, or no network at all.
func TestEnsureNetworkPrecedesApply(t *testing.T) {
	var order []string

	mp := &orderingProvider{record: func(op string) { order = append(order, op) }}
	e := New(map[spec.Provider]provider.CloudProvider{spec.ProviderAWS: mp})

	if _, err := e.Apply(context.Background(), scopedSpec([2]string{"dev", "eu-central-1"})); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	if len(order) < 2 || order[0] != "ensure-network" {
		t.Fatalf("operation order = %v, want the network first", order)
	}
}

// TestPlanDoesNotProvisionNetworks pins the side-effect-free contract.
// Plan previews changes; creating a VPC to preview a database would be a
// change made by the one command documented as making none.
func TestPlanDoesNotProvisionNetworks(t *testing.T) {
	mp := &mockProvider{name: "aws"}
	e := New(map[spec.Provider]provider.CloudProvider{spec.ProviderAWS: mp})

	if _, err := e.Plan(context.Background(), scopedSpec([2]string{"dev", "eu-central-1"})); err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if len(mp.networkScopes) != 0 {
		t.Errorf("Plan provisioned %v; it must make no changes", mp.networkScopes)
	}
}

// TestEnsureNetworkCarriesTheScope checks that what reaches the provider
// is the scope, not a flattened approximation of it. The account in
// particular has to survive: it is the partition that RFC 016 §2.1 makes
// an invariant.
func TestEnsureNetworkCarriesTheScope(t *testing.T) {
	mp := &mockProvider{name: "aws"}
	e := New(
		map[spec.Provider]provider.CloudProvider{spec.ProviderAWS: mp},
		WithDeploymentTargets(map[string]DeploymentTarget{
			"prod-account": {Name: "prod-account", Provider: spec.ProviderAWS, Enabled: true},
		}),
		WithTargetProviderFactory(spec.ProviderAWS,
			func(context.Context, DeploymentTarget) (provider.CloudProvider, error) { return mp, nil }),
	)

	s := scopedSpec([2]string{"live", "eu-central-1"})
	s.Resources[0].Account = "prod-account"

	if _, err := e.Apply(context.Background(), s); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	if len(mp.networkScopes) != 1 {
		t.Fatalf("EnsureNetwork called %d times, want 1", len(mp.networkScopes))
	}
	got := mp.networkScopes[0]
	want := provider.NetworkScope{
		Provider:    spec.ProviderAWS,
		Account:     "prod-account",
		Environment: "live",
		Region:      "eu-central-1",
		Sealed:      true,
	}
	if got != want {
		t.Errorf("scope = %+v, want %+v", got, want)
	}
}

// TestApplyRefusesOverlappingScopes covers RFC 016 §5.11: an address
// conflict is reported before anything is created, not discovered after
// half a Specification has been applied.
func TestApplyRefusesOverlappingScopes(t *testing.T) {
	mp := &mockProvider{name: "aws"}
	e := New(map[spec.Provider]provider.CloudProvider{spec.ProviderAWS: mp})

	s := scopedSpec(
		[2]string{"dev", "eu-central-1"},
		[2]string{"test", "eu-central-1"},
	)
	// Force the overlap: derivation would not produce one for these two,
	// so pin both onto the same range and test what the Engine does about
	// it rather than what the hash happens to give.
	s.Policies.Network = &spec.NetworkPolicy{Scopes: map[string]string{
		"dev::eu-central-1":  "10.50.0.0/20",
		"test::eu-central-1": "10.50.0.0/20",
	}}

	_, err := e.Apply(context.Background(), s)
	if err == nil {
		t.Fatal("Apply() error = nil, want the overlapping scopes to be refused")
	}

	var conflict network.Conflict
	if !errors.As(err, &conflict) {
		t.Errorf("error = %v, want a network.Conflict", err)
	}
	if len(mp.networkScopes) != 0 {
		t.Errorf("provisioned %v before refusing; nothing may be created", mp.networkScopes)
	}
	if !strings.Contains(err.Error(), "policies.network.scopes") {
		t.Errorf("error = %q, want it to name the way out", err)
	}
}

// TestAccountsMayShareARange is the Engine-level half of RFC 016 §2.1's
// invariant, and the reason the conflict check partitions by account:
// two accounts holding the same range is correct, because CloudSDD builds
// no route between accounts. Refusing it would block a legitimate
// Specification.
func TestAccountsMayShareARange(t *testing.T) {
	mp := &mockProvider{name: "aws"}
	e := New(
		map[spec.Provider]provider.CloudProvider{spec.ProviderAWS: mp},
		WithDeploymentTargets(map[string]DeploymentTarget{
			"acct-a": {Name: "acct-a", Provider: spec.ProviderAWS, Enabled: true},
			"acct-b": {Name: "acct-b", Provider: spec.ProviderAWS, Enabled: true},
		}),
		WithTargetProviderFactory(spec.ProviderAWS,
			func(context.Context, DeploymentTarget) (provider.CloudProvider, error) { return mp, nil }),
	)

	s := scopedSpec(
		[2]string{"live", "eu-central-1"},
		[2]string{"live", "eu-central-1"},
	)
	s.Resources[0].Account = "acct-a"
	s.Resources[1].Account = "acct-b"
	s.Policies.Network = &spec.NetworkPolicy{Scopes: map[string]string{
		"acct-a::live::eu-central-1": "10.50.0.0/20",
		"acct-b::live::eu-central-1": "10.50.0.0/20",
	}}

	if _, err := e.Apply(context.Background(), s); err != nil {
		t.Fatalf("Apply() error = %v, want identical ranges in two accounts to be accepted", err)
	}
	if len(mp.networkScopes) != 2 {
		t.Errorf("EnsureNetwork called %d times, want one per account", len(mp.networkScopes))
	}
}

// TestEnsureNetworkFailureStopsApply: a network that could not be built
// must stop the deploy, not produce resources in whatever network they
// would otherwise land in.
func TestEnsureNetworkFailureStopsApply(t *testing.T) {
	mp := &mockProvider{name: "aws", networkErr: errors.New("vpc quota exceeded")}
	e := New(map[spec.Provider]provider.CloudProvider{spec.ProviderAWS: mp})

	_, err := e.Apply(context.Background(), scopedSpec([2]string{"dev", "eu-central-1"}))
	if err == nil {
		t.Fatal("Apply() error = nil, want the network failure to stop the deploy")
	}
	if !strings.Contains(err.Error(), "vpc quota exceeded") {
		t.Errorf("error = %q, want it to carry the provider's reason", err)
	}
	// The scope must be named: "failed to provision the network" alone
	// sends the operator looking through every environment.
	if !strings.Contains(err.Error(), "dev::eu-central-1") {
		t.Errorf("error = %q, want it to name the scope", err)
	}
}

// TestNetworkScopesAreOrdered guards against map iteration reaching the
// providers. A Specification that built its networks in a different order
// on every run is impossible to reason about when one of them fails.
func TestNetworkScopesAreOrdered(t *testing.T) {
	s := scopedSpec(
		[2]string{"prod", "eu-west-1"},
		[2]string{"dev", "eu-central-1"},
		[2]string{"test", "us-east-1"},
		[2]string{"dev", "eu-west-1"},
	)

	first := networkScopes(s)
	for i := 0; i < 50; i++ {
		again := networkScopes(s)
		for j := range first {
			if first[j] != again[j] {
				t.Fatalf("scope order changed between calls: %v then %v", first, again)
			}
		}
	}
}

// orderingProvider records the sequence of operations rather than their
// arguments.
type orderingProvider struct {
	record func(string)
}

func (p *orderingProvider) Name() string { return "aws" }

func (p *orderingProvider) Validate(context.Context, spec.Resource, spec.Policies) error {
	return nil
}

func (p *orderingProvider) Plan(context.Context, spec.Resource, spec.Policies) (provider.Diff, error) {
	p.record("plan")
	return provider.Diff{Action: provider.ActionCreate}, nil
}

func (p *orderingProvider) Apply(context.Context, spec.Resource, spec.Policies) (provider.Result, error) {
	p.record("apply")
	return provider.Result{Status: provider.StatusApplied}, nil
}

func (p *orderingProvider) Destroy(context.Context, spec.Resource, spec.Policies) error {
	p.record("destroy")
	return nil
}

func (p *orderingProvider) EnsureNetwork(context.Context, provider.NetworkScope, spec.Policies) error {
	p.record("ensure-network")
	return nil
}

var _ provider.CloudProvider = (*orderingProvider)(nil)
