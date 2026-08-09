// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package engine

import (
	"context"
	"fmt"
	"sort"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/provider/network"
	"cloudsdd/internal/spec"
)

// ensureNetworks provisions the shared network of every scope the
// Specification touches, before any resource is applied (RFC 016 §2.2).
//
// The order matters. Everything the whole Specification says about its
// scopes is settled first — the address ranges are checked for conflict,
// and the volumes are merged — so a Specification that cannot be built
// fails before it has built half of one. Only then is each provider asked
// to build.
//
// This runs from Apply and not from Plan: Plan is side-effect free
// (RFC 011 §2.4 made that explicit for intent, and it holds for
// infrastructure too), and creating a VPC to preview a database would be
// a change made by a command documented as making none.
func (e *DefaultEngine) ensureNetworks(ctx context.Context, s spec.Specification) error {
	scopes := networkScopes(s)
	if len(scopes) == 0 {
		return nil
	}

	if err := network.Check(addressScopes(scopes), provider.AddressPolicyOf(s.Policies)); err != nil {
		return err
	}
	contents, err := scopeVolumes(s)
	if err != nil {
		return err
	}

	for _, scope := range scopes {
		p, ok := e.providers[scope.Provider]
		if !ok {
			// Unreachable: every scope came from a resource whose
			// provider was already resolved. Kept so a future caller
			// cannot reach a nil provider through this path.
			return fmt.Errorf("%w: %q", ErrProviderNotFound, scope.Provider)
		}
		if err := p.EnsureNetwork(ctx, scope, contents[scope], s.Policies); err != nil {
			return fmt.Errorf("engine: failed to provision the network for scope %q: %w",
				scopeLabel(scope), err)
		}
	}
	return nil
}

// networkScopes collects the distinct scopes a Specification touches, in
// a stable order.
//
// Distinct, because a scope's network is shared: ten resources in one
// environment need one network, and calling EnsureNetwork ten times would
// be ten Pulumi operations to reach the same state. Stable, because
// ranging over a map is randomised and a Specification that provisioned
// its networks in a different order on every run would be impossible to
// reason about when one of them failed.
//
// A resource bound to a DeploymentTarget keeps its Account here: that is
// the whole point of the account being part of the scope (RFC 016 §2.1).
func networkScopes(s spec.Specification) []provider.NetworkScope {
	seen := make(map[provider.NetworkScope]struct{})
	var scopes []provider.NetworkScope

	for _, r := range s.Resources {
		for _, scope := range resourceScopes(r) {
			if _, dup := seen[scope]; dup {
				continue
			}
			seen[scope] = struct{}{}
			scopes = append(scopes, scope)
		}
	}

	sort.Slice(scopes, func(i, j int) bool {
		return scopeLabel(scopes[i]) < scopeLabel(scopes[j])
	})
	return scopes
}

// resourceScopes yields the scopes one resource occupies: one per
// effective region, since a multi-region resource has a separate network
// in each of them (RFC 005 §2.4.2).
func resourceScopes(r spec.Resource) []provider.NetworkScope {
	regions := effectiveRegions(r.Scope)
	scopes := make([]provider.NetworkScope, 0, len(regions))
	for _, region := range regions {
		scopes = append(scopes, provider.NetworkScope{
			Provider:    r.Provider,
			Account:     r.Account,
			Environment: r.Scope.Environment,
			Region:      region,
			Sealed:      r.Scope.EffectiveSealed(),
		})
	}
	return scopes
}

// scopeVolumes collects the filesystems each scope owns, merged by name
// (RFC 020 §2.3).
//
// Within a scope a volume name identifies one filesystem, so two services
// naming "uploads" describe one share rather than two empty ones. The
// merged record deliberately carries no MountPath: a path is where the
// filesystem appears inside one container, and a shared record has no
// business holding whichever container happened to be read first.
//
// A scope whose resources mount nothing is absent from the result rather
// than present with an empty slice, so the caller's zero value is already
// the right answer.
func scopeVolumes(s spec.Specification) (map[provider.NetworkScope]provider.ScopeContents, error) {
	declared := make(map[provider.NetworkScope]map[string]declaredVolume)

	for _, r := range s.Resources {
		for _, scope := range resourceScopes(r) {
			for _, v := range r.Volumes {
				byName, ok := declared[scope]
				if !ok {
					byName = make(map[string]declaredVolume)
					declared[scope] = byName
				}

				merged, err := byName[v.Name].merge(v, r.ID)
				if err != nil {
					return nil, fmt.Errorf("%w: scope %q: %w", ErrVolumeConflict, scopeLabel(scope), err)
				}
				byName[v.Name] = merged
			}
		}
	}
	return scopeContents(declared), nil
}

// declaredVolume is one volume as merged so far. It remembers which
// resource supplied the size, because a later contradiction has to name
// both sides for the operator to know where either number was written.
type declaredVolume struct {
	sizeGB int

	// sizedBy is the id of the resource that declared sizeGB, empty while
	// no resource has declared one. That emptiness is the model of "let
	// the provider decide": it is the absence of an answer, not an answer
	// of zero.
	sizedBy string
}

// merge folds one resource's declaration into what the scope already
// knows about that volume.
//
// An absent size yields to a present one in either direction, since it is
// not a competing answer. Two different sizes cannot both be honoured, so
// they are refused here rather than resolved.
func (d declaredVolume) merge(v spec.Volume, resourceID string) (declaredVolume, error) {
	if v.SizeGB == 0 {
		return d, nil
	}
	if d.sizedBy == "" {
		return declaredVolume{sizeGB: v.SizeGB, sizedBy: resourceID}, nil
	}
	if d.sizeGB != v.SizeGB {
		return d, fmt.Errorf("volume %q is declared with size_gb %d by resource %q and %d by resource %q",
			v.Name, d.sizeGB, d.sizedBy, v.SizeGB, resourceID)
	}
	return d, nil
}

// scopeContents renders the merged declarations as what a provider
// receives, with each scope's volumes sorted by name. Sorted because the
// declarations were accumulated in a map, and filesystems reaching the
// providers in a different order on every run would make one Pulumi
// program's diff depend on nothing the operator changed.
func scopeContents(declared map[provider.NetworkScope]map[string]declaredVolume) map[provider.NetworkScope]provider.ScopeContents {
	contents := make(map[provider.NetworkScope]provider.ScopeContents, len(declared))
	for scope, byName := range declared {
		volumes := make([]spec.Volume, 0, len(byName))
		for name, d := range byName {
			volumes = append(volumes, spec.Volume{Name: name, SizeGB: d.sizeGB})
		}
		sort.Slice(volumes, func(i, j int) bool { return volumes[i].Name < volumes[j].Name })
		contents[scope] = provider.ScopeContents{Volumes: volumes}
	}
	return contents
}

// addressScopes projects a run of scopes onto the address model, which is
// the shape network.Check consults. Each element goes through
// provider.NetworkScopeOf, whose doc comment holds the reason the Provider
// field is dropped along the way.
func addressScopes(scopes []provider.NetworkScope) []network.Scope {
	out := make([]network.Scope, 0, len(scopes))
	for _, s := range scopes {
		out = append(out, provider.NetworkScopeOf(s))
	}
	return out
}

// scopeLabel renders a scope for an error message and for ordering.
func scopeLabel(s provider.NetworkScope) string {
	label := provider.NetworkScopeOf(s).String()
	if label == "" {
		// An entirely unscoped resource still needs something printable,
		// or the error names an empty string and tells the reader
		// nothing.
		return string(s.Provider) + "::(default)"
	}
	return string(s.Provider) + network.ScopeSeparator + label
}

// ScopeOccupancy reports how many resources are still recorded in a
// scope. It is how the Engine learns whether a shared network is safe to
// remove (RFC 016 §2.6).
//
// Injected rather than read directly, following the pattern RFC 004 set
// for DeploymentTargets: the Engine orchestrates, and what it knows about
// the world arrives through its constructor. It also keeps
// internal/engine free of internal/state, so an Engine driven directly —
// by a test, or by something that is not the CLI — is not obliged to have
// a ledger on disk.
type ScopeOccupancy func(ctx context.Context, s provider.NetworkScope) (int, error)

// WithScopeOccupancy supplies the occupancy check used before tearing
// down a scope's network.
//
// Absent, ReapNetworks removes nothing. That default is deliberate and it
// is the safe direction: an Engine that cannot prove a scope is empty
// leaves the network standing. The failure mode is an orphaned network,
// which costs money and is fixable; the opposite default would cut live
// resources off from everything they talk to.
func WithScopeOccupancy(f ScopeOccupancy) Option {
	return func(e *DefaultEngine) { e.occupancy = f }
}

// ReapNetworks removes the shared network of every scope in s that is now
// empty (RFC 016 §2.6), returning the scopes whose networks were removed.
//
// It is called after a destroy has been recorded, not during one: the
// ledger is the thing being consulted, so it has to have been updated
// first. That ordering also makes the operation idempotent — a rerun
// finds the networks already gone and does nothing.
func (e *DefaultEngine) ReapNetworks(ctx context.Context, s spec.Specification) ([]provider.NetworkScope, error) {
	if e.occupancy == nil {
		return nil, nil
	}

	// The same contents EnsureNetwork was given: a stack is selected by
	// its program, so a teardown that described the scope differently
	// would be tearing down a different scope.
	contents, err := scopeVolumes(s)
	if err != nil {
		return nil, err
	}

	var reaped []provider.NetworkScope
	for _, scope := range networkScopes(s) {
		remaining, err := e.occupancy(ctx, scope)
		if err != nil {
			return reaped, fmt.Errorf("engine: failed to check whether scope %q is empty: %w",
				scopeLabel(scope), err)
		}
		if remaining > 0 {
			continue
		}

		p, ok := e.providers[scope.Provider]
		if !ok {
			return reaped, fmt.Errorf("%w: %q", ErrProviderNotFound, scope.Provider)
		}
		if err := p.DestroyNetwork(ctx, scope, contents[scope], s.Policies); err != nil {
			return reaped, fmt.Errorf("engine: failed to remove the network for scope %q: %w",
				scopeLabel(scope), err)
		}
		reaped = append(reaped, scope)
	}
	return reaped, nil
}
