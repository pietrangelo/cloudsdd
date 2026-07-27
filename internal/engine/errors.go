// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package engine

import "errors"

// ErrProviderNotFound indicates that no CloudProvider is registered for
// the provider requested by a resource.
var ErrProviderNotFound = errors.New("engine: provider not registered")

// ErrAgnosticResolutionNotImplemented indicates that the resource requests
// the "agnostic" provider but the automatic resolution policy has not
// been implemented yet (RFC 001 §5, open question 2).
var ErrAgnosticResolutionNotImplemented = errors.New("engine: agnostic provider resolution not implemented")

// ErrDeploymentTargetNotFound indicates that Resource.Account references a
// DeploymentTarget that is not registered in the Engine (RFC 004 §4).
var ErrDeploymentTargetNotFound = errors.New("engine: deployment target not registered")

// ErrDeploymentTargetDisabled indicates that the DeploymentTarget
// referenced by Resource.Account exists but has Enabled == false: this is
// the kill switch from RFC 004 §2.2, which blocks the operation without
// contacting real infrastructure.
var ErrDeploymentTargetDisabled = errors.New("engine: deployment target disabled")

// ErrDeploymentTargetProviderMismatch indicates that the DeploymentTarget's
// provider does not match the one declared by the resource.
var ErrDeploymentTargetProviderMismatch = errors.New("engine: deployment target provider mismatch")

// ErrNoTargetProviderFactory indicates that no TargetProviderFactory has
// been registered for the provider requested by a DeploymentTarget (RFC
// 004 §4): the Engine does not know how to build a CloudProvider with
// assumed credentials for that cloud.
var ErrNoTargetProviderFactory = errors.New("engine: no target provider factory registered")
