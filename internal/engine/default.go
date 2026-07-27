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
	if r.Provider == spec.ProviderAgnostic {
		return nil, fmt.Errorf("%w: resource %q", ErrAgnosticResolutionNotImplemented, r.ID)
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

func (e *DefaultEngine) Validate(ctx context.Context, s spec.Specification) error {
	if err := spec.Validate(&s); err != nil {
		return err
	}
	for _, r := range s.Resources {
		p, err := e.resolveProvider(ctx, r)
		if err != nil {
			return err
		}
		if err := p.Validate(ctx, r, s.Policies); err != nil {
			return fmt.Errorf("engine: resource %q: %w", r.ID, err)
		}
	}
	return nil
}

func (e *DefaultEngine) Plan(ctx context.Context, s spec.Specification) ([]provider.Diff, error) {
	if err := e.Validate(ctx, s); err != nil {
		return nil, err
	}

	diffs := make([]provider.Diff, 0, len(s.Resources))
	for _, r := range s.Resources {
		p, err := e.resolveProvider(ctx, r)
		if err != nil {
			return nil, err
		}
		d, err := p.Plan(ctx, r, s.Policies)
		if err != nil {
			return nil, fmt.Errorf("engine: plan failed for resource %q: %w", r.ID, err)
		}
		diffs = append(diffs, d)
	}
	return diffs, nil
}

func (e *DefaultEngine) Apply(ctx context.Context, s spec.Specification) ([]provider.Result, error) {
	if err := e.Validate(ctx, s); err != nil {
		return nil, err
	}

	results := make([]provider.Result, 0, len(s.Resources))
	for _, r := range s.Resources {
		p, err := e.resolveProvider(ctx, r)
		if err != nil {
			return results, err
		}
		res, err := p.Apply(ctx, r, s.Policies)
		if err != nil {
			return results, fmt.Errorf("engine: apply failed for resource %q: %w", r.ID, err)
		}
		results = append(results, res)
	}
	return results, nil
}
