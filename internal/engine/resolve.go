// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/spec"
)

// Resolution records how one agnostic resource was bound to a concrete
// provider, so the CLI can show the user what was decided on their behalf
// before anything is applied (RFC 014 §2.1).
type Resolution struct {
	ResourceID string
	Provider   spec.Provider
	Reason     string
}

// Resolve binds every resource declaring Provider "agnostic" to a concrete
// provider, returning a copy of the Specification in which none remain.
//
// Resolution is deliberately *not* a guess. A candidate is a registered
// provider whose Validate accepts the resource, which reuses the one code
// path that already answers "can this provider express this, here, under
// these policies?" — so a provider cannot advertise a capability its own
// validation rejects (RFC 014 §2.2). Where that leaves more than one
// candidate, an explicitly declared preference decides; where it leaves
// none, CloudSDD reports every provider's reason rather than choosing.
func (e *DefaultEngine) Resolve(ctx context.Context, s spec.Specification) (spec.Specification, []Resolution, error) {
	resources := make([]spec.Resource, len(s.Resources))
	copy(resources, s.Resources)

	var resolutions []Resolution
	for i, r := range resources {
		// An Account already names its provider through the
		// DeploymentTarget it references (RFC 004 §2.2); that path is
		// untouched.
		if r.Provider != spec.ProviderAgnostic || r.Account != "" {
			continue
		}

		chosen, reason, err := e.resolveAgnostic(ctx, r, s.Policies)
		if err != nil {
			return spec.Specification{}, nil, err
		}
		resources[i].Provider = chosen
		resolutions = append(resolutions, Resolution{ResourceID: r.ID, Provider: chosen, Reason: reason})
	}

	s.Resources = resources
	return s, resolutions, nil
}

// resolveAgnostic picks the provider for a single agnostic resource.
func (e *DefaultEngine) resolveAgnostic(ctx context.Context, r spec.Resource, policies spec.Policies) (spec.Provider, string, error) {
	candidates, rejected := e.candidateProviders(ctx, r, policies)

	switch len(candidates) {
	case 1:
		return candidates[0], "the only provider that can express it", nil
	case 0:
		return "", "", fmt.Errorf("%w: resource %q:\n%s",
			ErrNoCandidateProvider, r.ID, formatRejections(rejected))
	}

	// More than one provider would take it, so something has to say which.
	// Both sources are explicit; neither is inferred from the environment.
	for _, preferred := range policies.ProviderPreference {
		if containsProvider(candidates, preferred) {
			return preferred, "first match in policies.provider_preference", nil
		}
	}
	if e.defaultProvider != "" && containsProvider(candidates, e.defaultProvider) {
		return e.defaultProvider, "the configured default provider", nil
	}

	return "", "", fmt.Errorf("%w: resource %q could be deployed to %s.\n"+
		"Name one in policies.provider_preference, set defaults.provider in the CloudSDD config, "+
		"or declare the provider on the resource",
		ErrAmbiguousProvider, r.ID, joinProviders(candidates))
}

// candidateProviders partitions the registry into those that accept the
// resource and those that do not, keeping the accepted ones in a stable
// order.
//
// The ordering matters more than it looks: DefaultEngine.providers is a
// map, and ranging over a Go map is randomised. A resolution that depended
// on iteration order would send the same Specification to a different
// cloud on different runs, which is the opposite of what RFC 014 §2.1
// requires — and a change of blast radius nobody approved.
func (e *DefaultEngine) candidateProviders(
	ctx context.Context,
	r spec.Resource,
	policies spec.Policies,
) ([]spec.Provider, map[spec.Provider]error) {
	names := make([]spec.Provider, 0, len(e.providers))
	for name := range e.providers {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })

	var accepted []spec.Provider
	rejected := make(map[spec.Provider]error, len(names))

	for _, name := range names {
		if err := e.acceptsResource(ctx, e.providers[name], name, r, policies); err != nil {
			rejected[name] = err
			continue
		}
		accepted = append(accepted, name)
	}
	return accepted, rejected
}

// acceptsResource reports whether p can take the resource in every region
// it fans out to (RFC 005 §2.4.2).
//
// All of them, not the first: a multi-region resource whose provider can
// only express some of its regions is not a candidate, and picking it
// would fail halfway through an apply.
func (e *DefaultEngine) acceptsResource(
	ctx context.Context,
	p provider.CloudProvider,
	name spec.Provider,
	r spec.Resource,
	policies spec.Policies,
) error {
	probe := r
	probe.Provider = name

	for _, region := range effectiveRegions(r.Scope) {
		if region != "" {
			if err := provider.ValidateRegionAllowed(region, policies.AllowedRegions); err != nil {
				return err
			}
		}
		if err := p.Validate(ctx, scopedResource(probe, region), policies); err != nil {
			return err
		}
	}
	return nil
}

// formatRejections renders every provider's own reason for refusing, in a
// stable order.
//
// Showing all of them is the point: a user whose resource no provider can
// take usually learns more from three specific complaints than from one.
func formatRejections(rejected map[spec.Provider]error) string {
	names := make([]spec.Provider, 0, len(rejected))
	for name := range rejected {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })

	lines := make([]string, 0, len(names))
	for _, name := range names {
		lines = append(lines, fmt.Sprintf("  %-6s %v", string(name)+":", rejected[name]))
	}
	return strings.Join(lines, "\n")
}

func containsProvider(list []spec.Provider, want spec.Provider) bool {
	for _, p := range list {
		if p == want {
			return true
		}
	}
	return false
}

func joinProviders(list []spec.Provider) string {
	names := make([]string, 0, len(list))
	for _, p := range list {
		names = append(names, string(p))
	}
	return strings.Join(names, ", ")
}
