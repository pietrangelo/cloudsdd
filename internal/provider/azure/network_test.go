// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package azure

import (
	"context"
	"testing"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/spec"
)

// TestEnsureNetworkIsANoOpForNow pins the staged rollout RFC 016 §6
// prescribes: the interface and the Engine's sequencing land first, with
// every provider returning nil, so that ordering and address derivation
// are reviewable before any provider starts building VPCs.
//
// This test is meant to be replaced, not kept. When this provider's
// network lands it should assert what gets declared; until then it
// asserts the honest current answer, so that "returns nil" is a decision
// on record rather than an omission nobody noticed.
func TestEnsureNetworkIsANoOpForNow(t *testing.T) {
	p := &AzureProvider{}

	err := p.EnsureNetwork(context.Background(), provider.NetworkScope{
		Provider:    spec.Provider("azure"),
		Environment: "dev",
		Region:      "test-region",
		Sealed:      true,
	}, spec.Policies{})

	if err != nil {
		t.Errorf("EnsureNetwork() error = %v, want nil while unimplemented", err)
	}
}
