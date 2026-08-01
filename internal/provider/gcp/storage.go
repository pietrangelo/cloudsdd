// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package gcp

import (
	"fmt"

	"github.com/pulumi/pulumi-gcp/sdk/v7/go/gcp/storage"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// objectStorageProperties represents the Properties of a Resource with
// Type: "object_storage" on GCP (RFC 008). It mirrors the AWS
// S3Properties shape so the same cloud-agnostic Specification maps onto
// either provider, with the same tri-state *bool defaults (RFC 011 §2.5).
//
// Location is deliberately absent: it derives from the cloud-agnostic
// spec.Resource.Scope.Region (RFC 005 §2.3). RFC 011 §1.1A1 found it
// hardcoded to "EU" here while Validate required a region, so a bucket
// requested in us-east1 was silently created in Europe.
type objectStorageProperties struct {
	BucketName        string `json:"bucket_name" validate:"required,min=3,max=63,gcsbucketname"`
	Versioning        *bool  `json:"versioning,omitempty"`
	Encryption        *bool  `json:"encryption,omitempty"`
	BlockPublicAccess *bool  `json:"block_public_access,omitempty"`

	// ForceDestroy allows destroying a bucket that still contains
	// objects. Defaults to false: RFC 011 §1.1E found it hardcoded true
	// "for simplified teardown", which silently converts an accidental
	// destroy into unrecoverable data loss.
	ForceDestroy *bool `json:"force_destroy,omitempty"`
}

// EffectiveVersioning returns Versioning, or false if absent: versioning
// carries a storage cost, not a security posture.
func (p objectStorageProperties) EffectiveVersioning() bool {
	return boolOrDefault(p.Versioning, false)
}

// EffectiveEncryption returns Encryption, or true if absent (secure by
// default).
func (p objectStorageProperties) EffectiveEncryption() bool {
	return boolOrDefault(p.Encryption, true)
}

// EffectiveBlockPublicAccess returns BlockPublicAccess, or true if absent
// (secure by default).
func (p objectStorageProperties) EffectiveBlockPublicAccess() bool {
	return boolOrDefault(p.BlockPublicAccess, true)
}

// EffectiveForceDestroy returns ForceDestroy, or false if absent (RFC 011
// §2.5: secure by default).
func (p objectStorageProperties) EffectiveForceDestroy() bool {
	return boolOrDefault(p.ForceDestroy, false)
}

// decodeObjectStorageProperties decodes and validates Properties as
// objectStorageProperties via the shared strict decoder (RFC 011 §2.1).
func decodeObjectStorageProperties(props map[string]any) (*objectStorageProperties, error) {
	var p objectStorageProperties
	if err := dec.Properties(props, &p); err != nil {
		return nil, err
	}

	// Cloud Storage encrypts at rest unconditionally with Google-managed
	// keys; there is no "off". Rejecting an explicit encryption:false is
	// more honest than silently ignoring it — the user asked for
	// something they cannot have on this provider.
	if p.Encryption != nil && !*p.Encryption {
		return nil, fmt.Errorf("gcp: encryption cannot be disabled on Cloud Storage (it is always enabled at rest)")
	}
	return &p, nil
}

func declareObjectStorage(ctx *pulumi.Context, id, region string, p objectStorageProperties) error {
	publicAccessPrevention := "inherited"
	if p.EffectiveBlockPublicAccess() {
		publicAccessPrevention = "enforced"
	}

	_, err := storage.NewBucket(ctx, id, &storage.BucketArgs{
		Name: pulumi.String(p.BucketName),
		// Region comes from the resource's Scope (RFC 005 §2.3), not a
		// hardcoded constant.
		Location:                 pulumi.String(region),
		UniformBucketLevelAccess: pulumi.Bool(true),
		PublicAccessPrevention:   pulumi.String(publicAccessPrevention),
		ForceDestroy:             pulumi.Bool(p.EffectiveForceDestroy()),
		Versioning: &storage.BucketVersioningArgs{
			Enabled: pulumi.Bool(p.EffectiveVersioning()),
		},
	})
	if err != nil {
		return fmt.Errorf("gcp: failed to declare storage bucket %q: %w", id, err)
	}
	return nil
}
