// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package engine

import (
	"context"
	"fmt"
	"sync"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/spec"
)

// DefaultEngine is the reference implementation of Engine: it maintains a
// static registry of CloudProviders indexed by spec.Provider and delegates
// per-resource operations to them after domain validation.
type DefaultEngine struct {
	providers       map[spec.Provider]provider.CloudProvider
	targets         map[string]DeploymentTarget
	targetFactories map[spec.Provider]TargetProviderFactory

	// defaultProvider breaks a tie for agnostic resources when the
	// Specification states no preference (RFC 014 §2.4). It arrives as an
	// Option rather than being read from internal/config, which sits
	// above this package.
	defaultProvider spec.Provider

	// occupancy answers "does this scope still hold anything?" before a
	// shared network is torn down (RFC 016 §2.6). Nil means never reap.
	occupancy ScopeOccupancy

	targetCacheMu sync.Mutex
	targetCache   map[string]provider.CloudProvider
}

// Option configures optional aspects of a DefaultEngine built with New
// (RFC 004 §4): the DeploymentTarget registry and the factories needed to
// build a CloudProvider with credentials assumed for them.
type Option func(*DefaultEngine)

// WithDeploymentTargets registers the available DeploymentTargets,
// indexed by Name. The content is copied to isolate the Engine from
// subsequent changes to the map passed by the caller.
func WithDeploymentTargets(targets map[string]DeploymentTarget) Option {
	return func(e *DefaultEngine) {
		registry := make(map[string]DeploymentTarget, len(targets))
		for name, t := range targets {
			registry[name] = t
		}
		e.targets = registry
	}
}

// WithDefaultProvider sets the machine-wide tie-break for resources
// declaring Provider "agnostic" (RFC 014 §2.4). Absent, an ambiguous
// resource is refused rather than guessed at.
func WithDefaultProvider(p spec.Provider) Option {
	return func(e *DefaultEngine) { e.defaultProvider = p }
}

// WithTargetProviderFactory registers the TargetProviderFactory to use to
// build a CloudProvider with assumed credentials when a DeploymentTarget
// declares provider p.
func WithTargetProviderFactory(p spec.Provider, factory TargetProviderFactory) Option {
	return func(e *DefaultEngine) {
		e.targetFactories[p] = factory
	}
}

// New builds a DefaultEngine with the given provider registry. The
// registry is never mutated after construction: New copies its content to
// isolate the Engine from subsequent changes to the map passed by the
// caller.
func New(providers map[spec.Provider]provider.CloudProvider, opts ...Option) *DefaultEngine {
	registry := make(map[spec.Provider]provider.CloudProvider, len(providers))
	for name, p := range providers {
		registry[name] = p
	}
	e := &DefaultEngine{
		providers:       registry,
		targets:         map[string]DeploymentTarget{},
		targetFactories: map[spec.Provider]TargetProviderFactory{},
		targetCache:     map[string]provider.CloudProvider{},
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

var _ Engine = (*DefaultEngine)(nil)

// resolveProvider determines which CloudProvider to use for a resource. If
// Resource.Account is set, it resolves a DeploymentTarget (RFC 004
// §2.2/§4) instead of the default static provider registry; otherwise the
// behavior is that of RFC 002 §2.5, unchanged.
func (e *DefaultEngine) resolveProvider(ctx context.Context, r spec.Resource) (provider.CloudProvider, error) {
	if r.Account != "" {
		return e.resolveTargetProvider(ctx, r)
	}
	p, ok := e.providers[r.Provider]
	if !ok {
		return nil, fmt.Errorf("%w: %q (resource %q)", ErrProviderNotFound, r.Provider, r.ID)
	}
	return p, nil
}

func (e *DefaultEngine) resolveTargetProvider(ctx context.Context, r spec.Resource) (provider.CloudProvider, error) {
	target, ok := e.targets[r.Account]
	if !ok {
		return nil, fmt.Errorf("%w: %q (resource %q)", ErrDeploymentTargetNotFound, r.Account, r.ID)
	}
	if !target.Enabled {
		return nil, fmt.Errorf("%w: %q (resource %q)", ErrDeploymentTargetDisabled, r.Account, r.ID)
	}
	if target.Provider != r.Provider {
		return nil, fmt.Errorf("%w: target %q is %q, resource %q declares %q", ErrDeploymentTargetProviderMismatch, r.Account, target.Provider, r.ID, r.Provider)
	}

	e.targetCacheMu.Lock()
	defer e.targetCacheMu.Unlock()
	if p, ok := e.targetCache[r.Account]; ok {
		return p, nil
	}

	factory, ok := e.targetFactories[target.Provider]
	if !ok {
		return nil, fmt.Errorf("%w: %q (resource %q)", ErrNoTargetProviderFactory, target.Provider, r.ID)
	}
	p, err := factory(ctx, target)
	if err != nil {
		return nil, fmt.Errorf("engine: failed to build provider for target %q: %w", r.Account, err)
	}
	e.targetCache[r.Account] = p
	return p, nil
}

// Validate checks the Specification and, for each resource, delegates to
// the resolved provider once per effective region (RFC 005 §2.4.2): an
// unscoped resource or one with a single Scope.Region still validates
// exactly once, preserving pre-RFC-005 behavior.
func (e *DefaultEngine) Validate(ctx context.Context, s spec.Specification) error {
	_, err := e.validated(ctx, s)
	return err
}

// validated runs every check Validate runs and returns the Specification
// the checks were run against: the same one, except that any resource
// declaring Provider "agnostic" has been bound to a concrete provider.
//
// Plan, Apply and Destroy must operate on *that* Specification, not on
// their own copy. Resolve takes a Specification by value, so a caller that
// validated and then walked its own resources would still be holding
// "agnostic" and would fail on a provider lookup that can never succeed —
// the registry is keyed by concrete providers only.
func (e *DefaultEngine) validated(ctx context.Context, s spec.Specification) (spec.Specification, error) {
	if err := spec.Validate(&s); err != nil {
		return spec.Specification{}, err
	}

	// Bind any agnostic resource before delegating, so no CloudProvider
	// is ever handed one (RFC 014 §2.6). The CLI resolves first and shows
	// the user the concrete result; this call makes an Engine driven
	// directly behave the same, and is a no-op once nothing is agnostic.
	s, _, err := e.Resolve(ctx, s)
	if err != nil {
		return spec.Specification{}, err
	}
	for _, r := range s.Resources {
		p, err := e.resolveProvider(ctx, r)
		if err != nil {
			return spec.Specification{}, err
		}
		// The power schedule is compiled here, centrally, for the same
		// reason AllowedRegions is (RFC 011 §2.3): a check that lives
		// only inside providers is a check the next provider forgets.
		// Providers keep their own, for the case where one is driven
		// directly rather than through the Engine.
		if err := validateSchedule(r, s.Policies); err != nil {
			return spec.Specification{}, err
		}
		for _, region := range effectiveRegions(r.Scope) {
			// Policies.AllowedRegions is enforced here, centrally, as
			// well as inside each provider (RFC 011 §2.3). It was
			// previously implemented only by the AWS provider, so a
			// Specification pinning allowed_regions deployed anywhere at
			// all on GCP and Azure. Enforcing it at the Engine means a
			// new provider cannot silently omit the check.
			if region != "" {
				if err := provider.ValidateRegionAllowed(region, s.Policies.AllowedRegions); err != nil {
					return spec.Specification{}, fmt.Errorf("engine: resource %q: %w", r.ID, err)
				}
			}
			if err := p.Validate(ctx, scopedResource(r, region), s.Policies); err != nil {
				return spec.Specification{}, fmt.Errorf("engine: resource %q: %w", r.ID, err)
			}
		}
	}
	return s, nil
}

// checkIntent enforces that the Specification's declared Intent permits
// the operation about to run (RFC 011 §2.4). Plan is exempt: it is
// side-effect free, so previewing any Specification is safe.
func checkIntent(s spec.Specification, allowed ...spec.Intent) error {
	for _, a := range allowed {
		if s.Intent == a {
			return nil
		}
	}
	return fmt.Errorf("%w: intent is %q, expected one of %v", ErrIntentMismatch, s.Intent, allowed)
}

func (e *DefaultEngine) Plan(ctx context.Context, s spec.Specification) ([]provider.Diff, error) {
	s, err := e.validated(ctx, s)
	if err != nil {
		return nil, err
	}

	diffs := make([]provider.Diff, 0, len(s.Resources))
	for _, r := range s.Resources {
		p, err := e.resolveProvider(ctx, r)
		if err != nil {
			return nil, err
		}
		for _, region := range effectiveRegions(r.Scope) {
			d, err := p.Plan(ctx, scopedResource(r, region), s.Policies)
			if err != nil {
				return nil, fmt.Errorf("engine: plan failed for resource %q: %w", r.ID, err)
			}
			d.Region = region
			diffs = append(diffs, d)
		}
	}
	return diffs, nil
}

// Apply applies the Specification. It requires an Intent of "deploy" or
// "update": applying a Specification the translator marked "destroy" or
// "plan" is a mismatch the user must see, not something to silently
// coerce (RFC 011 §2.4).
func (e *DefaultEngine) Apply(ctx context.Context, s spec.Specification) ([]provider.Result, error) {
	if err := checkIntent(s, spec.IntentDeploy, spec.IntentUpdate); err != nil {
		return nil, err
	}
	s, err := e.validated(ctx, s)
	if err != nil {
		return nil, err
	}

	// Every network a resource in this Specification will sit in has to
	// exist before any of them is applied (RFC 016 §2.2). Doing it here,
	// once, rather than inside each provider is what keeps three
	// concurrent resource programs from racing to create one VPC.
	if err := e.ensureNetworks(ctx, s); err != nil {
		return nil, err
	}

	results := make([]provider.Result, 0, len(s.Resources))
	for _, r := range s.Resources {
		p, err := e.resolveProvider(ctx, r)
		if err != nil {
			return results, err
		}
		for _, region := range effectiveRegions(r.Scope) {
			res, err := p.Apply(ctx, scopedResource(r, region), s.Policies)
			if err != nil {
				return results, fmt.Errorf("engine: apply failed for resource %q: %w", r.ID, err)
			}
			res.Region = region
			results = append(results, res)
		}
	}
	return results, nil
}

// Destroy removes the Specification's resources. It requires an Intent of
// "destroy" (RFC 011 §2.4), so a prompt the translator rendered as
// "deploy" cannot reach a destructive operation.
func (e *DefaultEngine) Destroy(ctx context.Context, s spec.Specification) ([]provider.Result, error) {
	if err := checkIntent(s, spec.IntentDestroy); err != nil {
		return nil, err
	}
	s, err := e.validated(ctx, s)
	if err != nil {
		return nil, err
	}

	results := make([]provider.Result, 0, len(s.Resources))
	// Typically destruction should be in reverse order, but dependencies are not yet implemented (RFC 001).
	for i := len(s.Resources) - 1; i >= 0; i-- {
		r := s.Resources[i]
		p, err := e.resolveProvider(ctx, r)
		if err != nil {
			return results, err
		}

		regions := effectiveRegions(r.Scope)
		for j := len(regions) - 1; j >= 0; j-- {
			region := regions[j]
			if err := p.Destroy(ctx, scopedResource(r, region), s.Policies); err != nil {
				return results, fmt.Errorf("engine: destroy failed for resource %q: %w", r.ID, err)
			}
			results = append(results, provider.Result{
				ResourceID: r.ID,
				Region:     region,
				Status:     provider.StatusDestroyed,
			})
		}
	}
	return results, nil
}
