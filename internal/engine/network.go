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
// Two things happen here and the order matters. The address ranges are
// checked for conflict first, across every scope in the Specification, so
// a Specification that cannot be addressed correctly fails before it has
// created half a network. Only then is each provider asked to build.
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

	if err := network.Check(addressScopes(scopes), addressPolicy(s.Policies)); err != nil {
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
		if err := p.EnsureNetwork(ctx, scope, s.Policies); err != nil {
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
		for _, region := range effectiveRegions(r.Scope) {
			scope := provider.NetworkScope{
				Provider:    r.Provider,
				Account:     r.Account,
				Environment: r.Scope.Environment,
				Region:      region,
				Sealed:      r.Scope.EffectiveSealed(),
			}
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

// addressScopes projects the engine's scopes onto the address model.
//
// The Provider field is deliberately dropped: two providers cannot share
// a network anyway, and including it would make the conflict check
// compare an AWS VPC against an Azure VNet, which can no more overlap
// than two accounts can.
func addressScopes(scopes []provider.NetworkScope) []network.Scope {
	out := make([]network.Scope, 0, len(scopes))
	for _, s := range scopes {
		out = append(out, network.Scope{
			Account:     s.Account,
			Environment: s.Environment,
			Region:      s.Region,
		})
	}
	return out
}

// addressPolicy translates the Specification's network policy into the
// address package's own, so that internal/provider/network stays free of
// spec types and testable on its own terms.
func addressPolicy(p spec.Policies) network.Policy {
	if p.Network == nil {
		return network.Policy{}
	}
	return network.Policy{
		BaseCIDR: p.Network.BaseCIDR,
		Scopes:   p.Network.Scopes,
	}
}

// scopeLabel renders a scope for an error message and for ordering.
func scopeLabel(s provider.NetworkScope) string {
	addr := network.Scope{Account: s.Account, Environment: s.Environment, Region: s.Region}
	label := addr.String()
	if label == "" {
		// An entirely unscoped resource still needs something printable,
		// or the error names an empty string and tells the reader
		// nothing.
		return string(s.Provider) + "::(default)"
	}
	return string(s.Provider) + network.ScopeSeparator + label
}
