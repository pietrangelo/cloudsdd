// Package provider definisce il contratto cloud-agnostico che ogni backend
// concreto (AWS, GCP, Azure, ...) deve implementare, come da
// docs/rfc/001-core-architecture-and-json-schema.md §2.3.
package provider

import (
	"context"

	"cloudsdd/internal/spec"
)

// Action descrive l'operazione che un Diff rappresenta per una risorsa.
type Action string

const (
	ActionCreate  Action = "create"
	ActionUpdate  Action = "update"
	ActionDestroy Action = "destroy"
	ActionNoop    Action = "noop"
)

// Diff rappresenta lo scarto tra lo stato desiderato (dalla Specifica) e lo
// stato attuale rilevato dal provider per una singola risorsa, prima
// dell'applicazione.
type Diff struct {
	ResourceID string         `json:"resource_id"`
	Action     Action         `json:"action"`
	Changes    map[string]any `json:"changes,omitempty"`
}

// Status descrive l'esito dell'applicazione di una risorsa.
type Status string

const (
	StatusApplied   Status = "applied"
	StatusFailed    Status = "failed"
	StatusDestroyed Status = "destroyed"
)

// Result rappresenta l'esito dell'applicazione (o distruzione) di una singola risorsa.
type Result struct {
	ResourceID string         `json:"resource_id"`
	Status     Status         `json:"status"`
	Details    map[string]any `json:"details,omitempty"`
}

// CloudProvider è il contratto implementato da ogni backend cloud concreto.
// Le implementazioni sono responsabili di decodificare Resource.Properties
// in una struct tipizzata propria del ResourceType gestito, validandola
// prima di ogni operazione (difesa da Mass Assignment a livello di provider).
type CloudProvider interface {
	// Name identifica il provider (es. "aws", "gcp", "azure").
	Name() string

	// Validate verifica che la risorsa sia esprimibile da questo provider,
	// senza effettuare alcuna chiamata verso l'infrastruttura reale.
	Validate(ctx context.Context, r spec.Resource) error

	// Plan calcola il Diff tra stato desiderato e stato attuale, senza
	// applicare alcuna modifica.
	Plan(ctx context.Context, r spec.Resource) (Diff, error)

	// Apply applica lo stato desiderato per la risorsa in modo idempotente.
	Apply(ctx context.Context, r spec.Resource) (Result, error)

	// Destroy rimuove la risorsa dall'infrastruttura reale.
	Destroy(ctx context.Context, r spec.Resource) error
}
