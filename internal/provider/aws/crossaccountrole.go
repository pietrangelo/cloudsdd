package aws

import (
	"encoding/json"
	"fmt"

	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/iam"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// CrossAccountRoleProperties represents the Properties of a Resource with
// Type: "cross_account_role" on AWS (RFC 003 §2.2): grants an external AWS
// account the ability to assume a role with restricted permissions toward
// specific resources.
type CrossAccountRoleProperties struct {
	// Enabled is the explicit toggle needed to suspend access without
	// removing the block from the Specification (RFC 003 §2.2). Defaults
	// to true if absent: declaring the resource already means wanting it
	// active; the field is there to disable it later with a minimal
	// change.
	Enabled          *bool  `json:"enabled,omitempty"`
	RoleName         string `json:"role_name" validate:"required,min=1,max=64"`
	TrustedAccountID string `json:"trusted_account_id" validate:"required,numeric,len=12"`
	// ExternalID is mandatory (not optional): the standard AWS mitigation
	// for the confused deputy problem in cross-account trust policies
	// (RFC 003 §2.2).
	ExternalID string `json:"external_id" validate:"required,min=8"`
	// Permissions does not allow the full wildcard action "*" (validator
	// ne=*): service-scoped wildcards are allowed (e.g. "s3:Get*"), not a
	// fully open action. Cap of 20 entries as a defense against
	// Unrestricted Resource Consumption.
	Permissions []string `json:"permissions" validate:"required,min=1,max=20,dive,required,ne=*"`
	// ResourceARNs is mandatory: no implicit Resource: "*", the user must
	// explicitly state which resources the grant applies to.
	ResourceARNs []string `json:"resource_arns" validate:"required,min=1,max=20,dive,required,startswith=arn:"`
}

// EffectiveEnabled returns the value of Enabled, or true if absent
// (RFC 003 §2.2).
func (p CrossAccountRoleProperties) EffectiveEnabled() bool { return boolOrDefault(p.Enabled, true) }

func decodeCrossAccountRoleProperties(props map[string]any) (*CrossAccountRoleProperties, error) {
	var p CrossAccountRoleProperties
	if err := decodeProperties(props, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// iamPolicyDocument is the minimal representation of an IAM policy
// document needed to build the trust policy and permission policy (RFC
// 003 §2.3). It is not meant to express the entire IAM language: only the
// subset required by this ResourceType.
type iamPolicyDocument struct {
	Version   string         `json:"Version"`
	Statement []iamStatement `json:"Statement"`
}

type iamStatement struct {
	Effect    string                    `json:"Effect"`
	Principal *iamPrincipal             `json:"Principal,omitempty"`
	Action    []string                  `json:"Action"`
	Resource  []string                  `json:"Resource,omitempty"`
	Condition map[string]map[string]any `json:"Condition,omitempty"`
}

type iamPrincipal struct {
	AWS string `json:"AWS"`
}

// buildTrustPolicy builds the role's AssumeRolePolicyDocument: it trusts
// only the TrustedAccountID account, and only if the request carries the
// correct sts:ExternalId (RFC 003 §2.2, confused deputy mitigation).
func buildTrustPolicy(p CrossAccountRoleProperties) (string, error) {
	doc := iamPolicyDocument{
		Version: "2012-10-17",
		Statement: []iamStatement{
			{
				Effect:    "Allow",
				Principal: &iamPrincipal{AWS: fmt.Sprintf("arn:aws:iam::%s:root", p.TrustedAccountID)},
				Action:    []string{"sts:AssumeRole"},
				Condition: map[string]map[string]any{
					"StringEquals": {"sts:ExternalId": p.ExternalID},
				},
			},
		},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("aws: failed to build trust policy: %w", err)
	}
	return string(b), nil
}

// buildPermissionPolicy builds the inline policy attached to the role:
// Permissions on ResourceARNs, with an aws:RequestedRegion constraint if
// allowedRegions is non-empty (RFC 003 §2.3, which translates the "same
// region" constraint — IAM is global — into the Policies.AllowedRegions
// perimeter). An empty allowedRegions is a domain error checked elsewhere
// (Validate, RFC 003 §2.3): this function simply builds the document
// consistently with the input it receives.
func buildPermissionPolicy(p CrossAccountRoleProperties, allowedRegions []string) (string, error) {
	stmt := iamStatement{
		Effect:   "Allow",
		Action:   p.Permissions,
		Resource: p.ResourceARNs,
	}
	if len(allowedRegions) > 0 {
		regions := make([]any, len(allowedRegions))
		for i, r := range allowedRegions {
			regions[i] = r
		}
		stmt.Condition = map[string]map[string]any{
			"StringEquals": {"aws:RequestedRegion": regions},
		}
	}

	doc := iamPolicyDocument{Version: "2012-10-17", Statement: []iamStatement{stmt}}
	b, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("aws: failed to build permission policy: %w", err)
	}
	return string(b), nil
}

// declareCrossAccountRole registers, in the inline Pulumi program, the
// cross-account IAM role and its inline policy (RFC 003 §2.3). Nothing is
// declared if p.EffectiveEnabled() is false: this is the kill switch from
// RFC 003 §2.2, which removes the underlying IAM resources while keeping
// the block in the Specification.
func declareCrossAccountRole(ctx *pulumi.Context, resourceID string, p CrossAccountRoleProperties, allowedRegions []string, opts ...pulumi.ResourceOption) error {
	if !p.EffectiveEnabled() {
		return nil
	}

	trustPolicy, err := buildTrustPolicy(p)
	if err != nil {
		return err
	}
	permissionPolicy, err := buildPermissionPolicy(p, allowedRegions)
	if err != nil {
		return err
	}

	role, err := iam.NewRole(ctx, resourceID, &iam.RoleArgs{
		Name:             pulumi.String(p.RoleName),
		AssumeRolePolicy: pulumi.String(trustPolicy),
	}, opts...)
	if err != nil {
		return fmt.Errorf("aws: failed to declare cross-account role %q: %w", resourceID, err)
	}

	if _, err := iam.NewRolePolicy(ctx, resourceID+"-permissions", &iam.RolePolicyArgs{
		Role:   role.ID(),
		Policy: pulumi.String(permissionPolicy),
	}, opts...); err != nil {
		return fmt.Errorf("aws: failed to declare permission policy for role %q: %w", resourceID, err)
	}

	return nil
}
