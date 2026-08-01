// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package engine

import "errors"

// ErrProviderNotFound indicates that no CloudProvider is registered for
// the provider requested by a resource.
var ErrProviderNotFound = errors.New("engine: provider not registered")

// ErrNoCandidateProvider indicates that no registered provider accepts a
// resource declaring Provider "agnostic" (RFC 014 §2.4). The wrapped
// message quotes each provider's own reason, which is usually more useful
// than any summary this package could write.
var ErrNoCandidateProvider = errors.New("engine: no provider can deploy this resource")

// ErrAmbiguousProvider indicates that several providers accept an
// agnostic resource and nothing says which to use.
//
// CloudSDD refuses rather than picking. A Specification that resolved to
// AWS in review and Azure in production would be a change of blast radius
// nobody approved (RFC 014 §2.1).
var ErrAmbiguousProvider = errors.New("engine: several providers could deploy this resource")

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

// ErrIntentMismatch indicates that the Specification's declared Intent
// does not permit the operation being performed (RFC 011 §2.4).
//
// Intent was previously parsed and validated but never consulted, so
// Apply would happily apply a destroy-intent Specification and the CLI's
// destroy command had to overwrite Intent after translation to compensate
// — which silently masked a translator that misread the user's request.
var ErrIntentMismatch = errors.New("engine: specification intent does not permit this operation")

// ErrResourceNotSchedulable indicates that a resource declared its own
// power schedule but its ResourceType has no power state (RFC 012 §3).
//
// Only an *explicit* Resource.Schedule is an error. A schedule inherited
// from Policies is skipped instead, because otherwise no Specification
// could contain both an object store and a database — which is most of
// them.
var ErrResourceNotSchedulable = errors.New("engine: resource type does not support a power schedule")
