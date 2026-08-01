// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package azure

import (
	"fmt"

	"github.com/pulumi/pulumi-azure/sdk/v5/go/azure/core"
	"github.com/pulumi/pulumi-azure/sdk/v5/go/azure/storage"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// objectStorageProperties represents the Properties of a Resource with
// Type: "object_storage" on Azure (RFC 008), mirroring the AWS and GCP
// shapes so one cloud-agnostic Specification maps onto any provider.
//
// BucketName carries the azurestorageaccount tag because Azure's naming
// rule is stricter than S3's or GCS's (3-24, lowercase alphanumeric only).
// RFC 011 §1.1A4: the account name was previously derived from the
// Resource ID via strings.ReplaceAll(id, "-", "") — which neither
// lowercased nor stripped the underscores spec permits in IDs, and
// truncated at 24 characters so two distinct resources could silently
// collide onto one account — while the decoded, required BucketName was
// never used at all.
type objectStorageProperties struct {
	BucketName        string `json:"bucket_name" validate:"required,azurestorageaccount"`
	Versioning        *bool  `json:"versioning,omitempty"`
	Encryption        *bool  `json:"encryption,omitempty"`
	BlockPublicAccess *bool  `json:"block_public_access,omitempty"`
}

// EffectiveVersioning returns Versioning, or false if absent.
func (p objectStorageProperties) EffectiveVersioning() bool {
	return boolOrDefault(p.Versioning, false)
}

// EffectiveEncryption returns Encryption, or true if absent.
func (p objectStorageProperties) EffectiveEncryption() bool {
	return boolOrDefault(p.Encryption, true)
}

// EffectiveBlockPublicAccess returns BlockPublicAccess, or true if absent
// (secure by default).
func (p objectStorageProperties) EffectiveBlockPublicAccess() bool {
	return boolOrDefault(p.BlockPublicAccess, true)
}

// decodeObjectStorageProperties decodes and validates Properties via the
// shared strict decoder (RFC 011 §2.1).
func decodeObjectStorageProperties(props map[string]any) (*objectStorageProperties, error) {
	var p objectStorageProperties
	if err := dec.Properties(props, &p); err != nil {
		return nil, err
	}

	// Azure Storage encrypts at rest unconditionally; there is no "off".
	// Rejecting an explicit encryption:false is more honest than silently
	// ignoring it.
	if p.Encryption != nil && !*p.Encryption {
		return nil, fmt.Errorf("azure: encryption cannot be disabled on Azure Storage (it is always enabled at rest)")
	}
	return &p, nil
}

func declareObjectStorage(ctx *pulumi.Context, id, location string, p objectStorageProperties) error {
	rg, err := core.NewResourceGroup(ctx, id+"-rg", &core.ResourceGroupArgs{
		Location: pulumi.String(location),
	})
	if err != nil {
		return fmt.Errorf("azure: failed to declare resource group for %q: %w", id, err)
	}

	accountArgs := &storage.AccountArgs{
		// The user's requested name, validated against Azure's rule —
		// not a lossy transformation of the Resource ID.
		Name:                       pulumi.String(p.BucketName),
		ResourceGroupName:          rg.Name,
		Location:                   rg.Location,
		AccountTier:                pulumi.String("Standard"),
		AccountReplicationType:     pulumi.String("LRS"),
		HttpsTrafficOnlyEnabled:    pulumi.Bool(true), // Secure transfer required
		MinTlsVersion:              pulumi.String("TLS1_2"),
		AllowNestedItemsToBePublic: pulumi.Bool(!p.EffectiveBlockPublicAccess()),
		// Double encryption at rest, on top of the unconditional
		// service-level encryption.
		InfrastructureEncryptionEnabled: pulumi.Bool(true),
		BlobProperties: &storage.AccountBlobPropertiesArgs{
			VersioningEnabled: pulumi.Bool(p.EffectiveVersioning()),
		},
	}

	// Default-deny at the network layer when public access is blocked.
	// AllowNestedItemsToBePublic only governs anonymous blob access; it
	// leaves the account reachable from any network, so the posture is
	// not complete without this (RFC 011 §2.5).
	if p.EffectiveBlockPublicAccess() {
		accountArgs.NetworkRules = &storage.AccountNetworkRulesTypeArgs{
			DefaultAction: pulumi.String("Deny"),
			Bypasses:      pulumi.StringArray{pulumi.String("AzureServices")},
		}
	}

	acc, err := storage.NewAccount(ctx, id, accountArgs)
	if err != nil {
		return fmt.Errorf("azure: failed to declare storage account for %q: %w", id, err)
	}

	if _, err := storage.NewContainer(ctx, id+"-container", &storage.ContainerArgs{
		StorageAccountName:  acc.Name,
		ContainerAccessType: pulumi.String("private"),
	}); err != nil {
		return fmt.Errorf("azure: failed to declare storage container for %q: %w", id, err)
	}
	return nil
}
