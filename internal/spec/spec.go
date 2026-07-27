// Package spec definisce la rappresentazione tipizzata della Specifica SDD,
// la "Single Source of Truth" descritta in docs/rfc/001-core-architecture-and-json-schema.md.
package spec

// Intent descrive l'operazione richiesta sulla Specifica.
type Intent string

const (
	IntentDeploy  Intent = "deploy"
	IntentUpdate  Intent = "update"
	IntentDestroy Intent = "destroy"
	IntentPlan    Intent = "plan"
)

// ResourceType enumera i tipi di risorsa supportati dallo schema v1.0.
type ResourceType string

const (
	ResourceTypeRelationalDatabase ResourceType = "relational_database"
	ResourceTypeObjectStorage      ResourceType = "object_storage"
	ResourceTypeComputeInstance    ResourceType = "compute_instance"
	ResourceTypeContainerService   ResourceType = "container_service"
)

// Provider identifica il provider cloud target di una risorsa, oppure
// "agnostic" per delegarne la risoluzione all'Engine.
type Provider string

const (
	ProviderAgnostic Provider = "agnostic"
	ProviderAWS      Provider = "aws"
	ProviderGCP      Provider = "gcp"
	ProviderAzure    Provider = "azure"
)

// Specification è la rappresentazione root della Specifica SDD in ingresso.
type Specification struct {
	SDDVersion string     `json:"sdd_version" validate:"required,eq=1.0"`
	Intent     Intent     `json:"intent" validate:"required,oneof=deploy update destroy plan"`
	Resources  []Resource `json:"resources" validate:"required,min=1,dive"`
	Policies   Policies   `json:"policies"`
}

// Resource rappresenta una singola risorsa richiesta nella Specifica.
//
// Properties resta intenzionalmente un contenitore di trasporto JSON
// generico: ogni provider concreto è responsabile di decodificarlo in una
// struct tipizzata e validata specifica per il proprio ResourceType, per
// bloccare il Mass Assignment prima che i dati raggiungano la logica di
// business (vedi RFC 001, sezione "Considerazioni di Sicurezza").
type Resource struct {
	ID         string         `json:"id" validate:"required,resourceid"`
	Type       ResourceType   `json:"type" validate:"required,oneof=relational_database object_storage compute_instance container_service"`
	Provider   Provider       `json:"provider" validate:"required,oneof=agnostic aws gcp azure"`
	Properties map[string]any `json:"properties" validate:"required"`
}

// Policies esprime i vincoli globali applicati a tutte le risorse della Specifica.
type Policies struct {
	MaxCostMonthly *float64 `json:"max_cost_monthly,omitempty" validate:"omitempty,gt=0"`
	AllowedRegions []string `json:"allowed_regions,omitempty"`
}
