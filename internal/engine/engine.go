// Package engine orchestra l'applicazione di una Specifica SDD sui
// CloudProvider registrati, come da
// docs/rfc/001-core-architecture-and-json-schema.md §2.3.
package engine

import (
	"context"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/spec"
)

// Engine espone il ciclo di vita di una Specifica: validazione, calcolo del
// piano (diff) e applicazione, senza conoscere i dettagli di alcun provider
// concreto.
type Engine interface {
	// Validate verifica la Specifica (validazione di dominio) e, per ogni
	// risorsa, delega la validazione specifica del provider risolto.
	Validate(ctx context.Context, s spec.Specification) error

	// Plan calcola il Diff per ogni risorsa della Specifica senza applicare
	// alcuna modifica.
	Plan(ctx context.Context, s spec.Specification) ([]provider.Diff, error)

	// Apply applica la Specifica, risorsa per risorsa, tramite i
	// CloudProvider registrati.
	Apply(ctx context.Context, s spec.Specification) ([]provider.Result, error)
}
