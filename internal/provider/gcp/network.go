// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package gcp

import (
	"context"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/spec"
)

// EnsureNetwork provisions the shared network for a scope (RFC 016 §2.2).
//
// Not yet implemented: RFC 016 §6 lands the interface and the Engine's
// sequencing first, with every provider a no-op, so that the ordering and
// the address derivation can be reviewed and tested before any provider
// starts building VPCs. Returning nil here means "this provider has
// nothing to share", which is the truthful answer today — resources still
// go where they went before.
func (p *GCPProvider) EnsureNetwork(ctx context.Context, s provider.NetworkScope, policies spec.Policies) error {
	return nil
}
