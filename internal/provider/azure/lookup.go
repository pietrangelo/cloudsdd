// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package azure

import (
	"fmt"

	"github.com/pulumi/pulumi-azure/sdk/v5/go/azure/network"
	"github.com/pulumi/pulumi-azure/sdk/v5/go/azure/privatedns"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider"
)

// scopeNetwork is what a resource program needs to know about the network
// it is being placed into.
//
// Azure resource IDs carry the subscription, which a resource program
// cannot compute, so the IDs are looked up rather than derived — unlike
// GCP, where a name is enough. The *names* are still derived from the
// scope, so the lookup asks for something both stacks agree on without
// consulting each other's state.
type scopeNetwork struct {
	subnetID     string
	dnsZoneID    string
	addressSpace string
}

// lookupScopeNetwork finds the subnet and DNS zone for a resource in its
// scope's network (RFC 016 §2.2).
//
// The Engine provisions the network before any resource in the scope, so
// a miss here is a broken invariant rather than a race, and it surfaces
// as an error naming the scope. There is deliberately no fallback to
// creating a network on the spot: that would recreate the per-resource
// VNets RFC 016 exists to replace, silently and one deploy at a time.
func lookupScopeNetwork(ctx *pulumi.Context, s provider.NetworkScope, subnet string) (scopeNetwork, error) {
	rg := scopeResourceGroupName(s)
	vnet := scopeNetworkName(s)

	found, err := network.LookupSubnet(ctx, &network.LookupSubnetArgs{
		Name:               subnet,
		ResourceGroupName:  rg,
		VirtualNetworkName: vnet,
	}, nil)
	if err != nil {
		return scopeNetwork{}, fmt.Errorf(
			"azure: no CloudSDD network found for scope %q. The network is created before the "+
				"resources in it, so this means it was removed out of band: %w", scopeName(s), err)
	}

	return scopeNetwork{
		subnetID:     found.Id,
		addressSpace: found.AddressPrefix,
	}, nil
}

// lookupEngineNetwork adds the private DNS zone a Flexible Server needs
// on top of its delegated subnet.
func lookupEngineNetwork(ctx *pulumi.Context, s provider.NetworkScope, engine string) (scopeNetwork, error) {
	net, err := lookupScopeNetwork(ctx, s, engineSubnetName(engine))
	if err != nil {
		return scopeNetwork{}, err
	}

	rg := scopeResourceGroupName(s)
	zone, err := privatedns.GetDnsZone(ctx, &privatedns.GetDnsZoneArgs{
		Name:              engineZoneName(s, engine),
		ResourceGroupName: &rg,
	}, nil)
	if err != nil {
		return scopeNetwork{}, fmt.Errorf(
			"azure: no private DNS zone found for %s in scope %q: %w", engine, scopeName(s), err)
	}

	net.dnsZoneID = zone.Id
	return net, nil
}
