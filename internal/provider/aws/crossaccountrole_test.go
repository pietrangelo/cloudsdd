// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package aws

import (
	"encoding/json"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func validCrossAccountRoleProps() map[string]any {
	return map[string]any{
		"role_name":          "partner-read-access",
		"trusted_account_id": "123456789012",
		"external_id":        "a-shared-secret",
		"permissions":        []any{"s3:GetObject"},
		"resource_arns":      []any{"arn:aws:s3:::app-data/*"},
	}
}

func TestDecodeCrossAccountRoleProperties(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(m map[string]any)
		wantErr bool
	}{
		{name: "valid properties", mutate: func(m map[string]any) {}, wantErr: false},
		{
			name:    "missing external_id rejected",
			mutate:  func(m map[string]any) { delete(m, "external_id") },
			wantErr: true,
		},
		{
			name:    "external_id too short rejected",
			mutate:  func(m map[string]any) { m["external_id"] = "short" },
			wantErr: true,
		},
		{
			name:    "trusted_account_id not 12 digits rejected",
			mutate:  func(m map[string]any) { m["trusted_account_id"] = "123" },
			wantErr: true,
		},
		{
			name:    "trusted_account_id non numeric rejected",
			mutate:  func(m map[string]any) { m["trusted_account_id"] = "abcdefghijkl" },
			wantErr: true,
		},
		{
			name:    "wildcard permission rejected",
			mutate:  func(m map[string]any) { m["permissions"] = []any{"*"} },
			wantErr: true,
		},
		{
			name:    "service-scoped wildcard permission allowed",
			mutate:  func(m map[string]any) { m["permissions"] = []any{"s3:Get*"} },
			wantErr: false,
		},
		{
			name:    "empty permissions rejected",
			mutate:  func(m map[string]any) { m["permissions"] = []any{} },
			wantErr: true,
		},
		{
			name: "more than 20 permissions rejected",
			mutate: func(m map[string]any) {
				perms := make([]any, 21)
				for i := range perms {
					perms[i] = "s3:GetObject"
				}
				m["permissions"] = perms
			},
			wantErr: true,
		},
		{
			name:    "empty resource_arns rejected",
			mutate:  func(m map[string]any) { m["resource_arns"] = []any{} },
			wantErr: true,
		},
		{
			name:    "resource_arns not starting with arn: rejected",
			mutate:  func(m map[string]any) { m["resource_arns"] = []any{"app-data/*"} },
			wantErr: true,
		},
		{
			name:    "unknown field rejected",
			mutate:  func(m map[string]any) { m["extra"] = "field" },
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			props := validCrossAccountRoleProps()
			tt.mutate(props)
			_, err := decodeCrossAccountRoleProperties(props)
			if (err != nil) != tt.wantErr {
				t.Fatalf("decodeCrossAccountRoleProperties() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}

	t.Run("enabled defaults to true when absent", func(t *testing.T) {
		p, err := decodeCrossAccountRoleProperties(validCrossAccountRoleProps())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !p.EffectiveEnabled() {
			t.Errorf("EffectiveEnabled() = false, want true when absent")
		}
	})

	t.Run("enabled false is respected", func(t *testing.T) {
		props := validCrossAccountRoleProps()
		props["enabled"] = false
		p, err := decodeCrossAccountRoleProperties(props)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p.EffectiveEnabled() {
			t.Errorf("EffectiveEnabled() = true, want false when explicitly disabled")
		}
	})
}

func TestBuildTrustPolicy(t *testing.T) {
	p := CrossAccountRoleProperties{
		TrustedAccountID: "123456789012",
		ExternalID:       "a-shared-secret",
	}
	doc, err := buildTrustPolicy(p)
	if err != nil {
		t.Fatalf("buildTrustPolicy() unexpected error: %v", err)
	}

	var parsed iamPolicyDocument
	if err := json.Unmarshal([]byte(doc), &parsed); err != nil {
		t.Fatalf("buildTrustPolicy() produced invalid JSON: %v", err)
	}
	if len(parsed.Statement) != 1 {
		t.Fatalf("expected exactly one statement, got %d", len(parsed.Statement))
	}
	stmt := parsed.Statement[0]
	if stmt.Principal == nil || stmt.Principal.AWS != "arn:aws:iam::123456789012:root" {
		t.Errorf("unexpected principal: %+v", stmt.Principal)
	}
	if stmt.Condition["StringEquals"]["sts:ExternalId"] != "a-shared-secret" {
		t.Errorf("expected sts:ExternalId condition to mitigate confused deputy, got %+v", stmt.Condition)
	}
}

func TestBuildPermissionPolicy(t *testing.T) {
	p := CrossAccountRoleProperties{
		Permissions:  []string{"s3:GetObject"},
		ResourceARNs: []string{"arn:aws:s3:::app-data/*"},
	}

	t.Run("without allowed regions", func(t *testing.T) {
		doc, err := buildPermissionPolicy(p, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var parsed iamPolicyDocument
		if err := json.Unmarshal([]byte(doc), &parsed); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if parsed.Statement[0].Condition != nil {
			t.Errorf("expected no Condition when allowedRegions is empty, got %+v", parsed.Statement[0].Condition)
		}
	})

	t.Run("with allowed regions applies aws:RequestedRegion condition", func(t *testing.T) {
		doc, err := buildPermissionPolicy(p, []string{"eu-central-1"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var parsed iamPolicyDocument
		if err := json.Unmarshal([]byte(doc), &parsed); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		cond := parsed.Statement[0].Condition
		if cond == nil {
			t.Fatalf("expected aws:RequestedRegion condition, got none")
		}
		regions, ok := cond["StringEquals"]["aws:RequestedRegion"].([]any)
		if !ok || len(regions) != 1 || regions[0] != "eu-central-1" {
			t.Errorf("unexpected region condition: %+v", cond)
		}
	})
}

type roleMocks struct{}

func (roleMocks) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	return args.Name + "_id", args.Inputs, nil
}

func (roleMocks) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	return resource.PropertyMap{}, nil
}

func TestDeclareCrossAccountRole(t *testing.T) {
	p := CrossAccountRoleProperties{
		Enabled:          boolPtr(true),
		RoleName:         "partner-read-access",
		TrustedAccountID: "123456789012",
		ExternalID:       "a-shared-secret",
		Permissions:      []string{"s3:GetObject"},
		ResourceARNs:     []string{"arn:aws:s3:::app-data/*"},
	}

	t.Run("enabled declares role and policy", func(t *testing.T) {
		declared := 0
		err := pulumi.RunErr(func(ctx *pulumi.Context) error {
			err := declareCrossAccountRole(ctx, "partner-access", p, []string{"eu-central-1"})
			declared++
			return err
		}, pulumi.WithMocks("cloudsdd-test", "test-stack", roleMocks{}))
		if err != nil {
			t.Fatalf("declareCrossAccountRole() unexpected error: %v", err)
		}
		if declared != 1 {
			t.Fatalf("expected program to run exactly once, got %d", declared)
		}
	})

	t.Run("disabled declares nothing and does not error", func(t *testing.T) {
		disabled := p
		disabled.Enabled = boolPtr(false)
		err := pulumi.RunErr(func(ctx *pulumi.Context) error {
			return declareCrossAccountRole(ctx, "partner-access", disabled, []string{"eu-central-1"})
		}, pulumi.WithMocks("cloudsdd-test", "test-stack", roleMocks{}))
		if err != nil {
			t.Fatalf("declareCrossAccountRole() unexpected error: %v", err)
		}
	})
}
