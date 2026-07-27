package engine

import (
	"context"
	"fmt"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/spec"
)

// DefaultEngine è l'implementazione di riferimento di Engine: mantiene un
// registry statico di CloudProvider indicizzato per spec.Provider e delega
// ad essi le operazioni per-risorsa dopo la validazione di dominio.
type DefaultEngine struct {
	providers map[spec.Provider]provider.CloudProvider
}

// New costruisce un DefaultEngine con il registry di provider indicato.
// Il registry non viene mai mutato dopo la costruzione: New ne copia il
// contenuto per isolare l'Engine da modifiche successive alla mappa
// passata dal chiamante.
func New(providers map[spec.Provider]provider.CloudProvider) *DefaultEngine {
	registry := make(map[spec.Provider]provider.CloudProvider, len(providers))
	for name, p := range providers {
		registry[name] = p
	}
	return &DefaultEngine{providers: registry}
}

var _ Engine = (*DefaultEngine)(nil)

func (e *DefaultEngine) resolveProvider(r spec.Resource) (provider.CloudProvider, error) {
	if r.Provider == spec.ProviderAgnostic {
		return nil, fmt.Errorf("%w: resource %q", ErrAgnosticResolutionNotImplemented, r.ID)
	}
	p, ok := e.providers[r.Provider]
	if !ok {
		return nil, fmt.Errorf("%w: %q (resource %q)", ErrProviderNotFound, r.Provider, r.ID)
	}
	return p, nil
}

func (e *DefaultEngine) Validate(ctx context.Context, s spec.Specification) error {
	if err := spec.Validate(&s); err != nil {
		return err
	}
	for _, r := range s.Resources {
		p, err := e.resolveProvider(r)
		if err != nil {
			return err
		}
		if err := p.Validate(ctx, r); err != nil {
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
		p, err := e.resolveProvider(r)
		if err != nil {
			return nil, err
		}
		d, err := p.Plan(ctx, r)
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
		p, err := e.resolveProvider(r)
		if err != nil {
			return results, err
		}
		res, err := p.Apply(ctx, r)
		if err != nil {
			return results, fmt.Errorf("engine: apply failed for resource %q: %w", r.ID, err)
		}
		results = append(results, res)
	}
	return results, nil
}
