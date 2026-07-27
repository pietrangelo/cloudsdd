// Package spec defines the typed representation of the SDD Specification,
// the "Single Source of Truth" described in docs/rfc/001-core-architecture-and-json-schema.md.
package spec

// Intent describes the operation requested on the Specification.
type Intent string

const (
	IntentDeploy  Intent = "deploy"
	IntentUpdate  Intent = "update"
	IntentDestroy Intent = "destroy"
	IntentPlan    Intent = "plan"
)

// ResourceType enumerates the resource types supported by the v1.0 schema.
type ResourceType string

const (
	ResourceTypeRelationalDatabase ResourceType = "relational_database"
	ResourceTypeObjectStorage      ResourceType = "object_storage"
	ResourceTypeComputeInstance    ResourceType = "compute_instance"
	ResourceTypeContainerService   ResourceType = "container_service"
	// ResourceTypeCrossAccountRole represents a cross-account IAM role
	// (RFC 003): grants an external AWS account the ability to assume a
	// role with restricted permissions toward specific resources.
	ResourceTypeCrossAccountRole ResourceType = "cross_account_role"
)

// Provider identifies the target cloud provider of a resource, or
// "agnostic" to delegate its resolution to the Engine.
type Provider string

const (
	ProviderAgnostic Provider = "agnostic"
	ProviderAWS      Provider = "aws"
	ProviderGCP      Provider = "gcp"
	ProviderAzure    Provider = "azure"
)

// Specification is the root representation of the incoming SDD Specification.
type Specification struct {
	SDDVersion string     `json:"sdd_version" validate:"required,eq=1.0"`
	Intent     Intent     `json:"intent" validate:"required,oneof=deploy update destroy plan"`
	Resources  []Resource `json:"resources" validate:"required,min=1,dive"`
	Policies   Policies   `json:"policies"`
}

// Resource represents a single resource requested in the Specification.
//
// Properties intentionally remains a generic JSON transport container:
// each concrete provider is responsible for decoding it into a typed and
// validated struct specific to its own ResourceType, so as to block Mass
// Assignment before the data reaches business logic (see RFC 001, section
// "Security Considerations").
type Resource struct {
	ID       string       `json:"id" validate:"required,resourceid"`
	Type     ResourceType `json:"type" validate:"required,oneof=relational_database object_storage compute_instance container_service cross_account_role"`
	Provider Provider     `json:"provider" validate:"required,oneof=agnostic aws gcp azure"`
	// Account references, by name, a DeploymentTarget configured at the
	// engine level (RFC 004 §2.2), used to apply the resource with
	// credentials assumed toward an external account/project/subscription
	// instead of the default credential chain. Optional: if absent,
	// behavior is unchanged from RFC 002 §2.4. Deliberately never
	// references a secret: only a symbolic name resolved by the engine,
	// never an ARN or a credential.
	Account    string         `json:"account,omitempty" validate:"omitempty,max=64"`
	Properties map[string]any `json:"properties" validate:"required"`
}

// Policies expresses the global constraints applied to all resources in the Specification.
type Policies struct {
	MaxCostMonthly *float64 `json:"max_cost_monthly,omitempty" validate:"omitempty,gt=0"`
	AllowedRegions []string `json:"allowed_regions,omitempty"`
}
