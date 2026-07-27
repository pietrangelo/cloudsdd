// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package engine

import (
	"context"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/spec"
)

// DeploymentTarget describes an external account/project/subscription that
// CloudSDD can reach to apply resources on its behalf (RFC 004 §2.1). It
// is not part of the SDD Specification validated by RFC 001: it is loaded
// from a separate configuration, managed by the Engine's caller (e.g. from
// a local file with restricted permissions), precisely to keep "what to
// create" separate from "how to authenticate" (DeploymentTarget) — see RFC
// 004 §2.1 for the security rationale.
type DeploymentTarget struct {
	// Name is the identifier referenced by spec.Resource.Account.
	Name string
	// Provider indicates which CloudProvider this target applies to.
	Provider spec.Provider
	// Enabled is the explicit toggle (RFC 004 §2.2): when false, the
	// Engine rejects any operation toward this target without contacting
	// real infrastructure, regardless of the underlying trust
	// relationship.
	Enabled bool
	// AWS holds the configuration for assuming credentials when
	// Provider == spec.ProviderAWS.
	AWS *AWSTargetConfig
}

// AWSTargetConfig describes how to obtain assumed credentials for a
// target AWS account (RFC 004 §2.1, §3).
type AWSTargetConfig struct {
	// AccountID is the 12-digit ID of the target AWS account.
	AccountID string
	// RoleARN is the role to assume in that account (it must already
	// exist: CloudSDD does not create it, to avoid the chicken-and-egg
	// problem described in RFC 003 §2.4).
	RoleARN string
	// ExternalID mitigates the confused deputy problem (RFC 004 §3.3),
	// analogous to RFC 003 §2.2.
	ExternalID string
	// SessionDurationSeconds caps the lifetime of the assumed credentials
	// (RFC 004 §3.1: never long-lived credentials). Zero => the AWS
	// provider uses its own default.
	SessionDurationSeconds int32
}

// TargetProviderFactory builds (or retrieves from cache) a CloudProvider
// configured with the credentials assumed for a specific DeploymentTarget.
// It is implemented by the concrete provider package (e.g.
// internal/provider/aws), never by the Engine itself: the Engine
// orchestrates but knows nothing about any cloud's credential assumption
// mechanism (consistent with RFC 001 §2.3, the Engine stays
// cloud-agnostic).
type TargetProviderFactory func(ctx context.Context, target DeploymentTarget) (provider.CloudProvider, error)
