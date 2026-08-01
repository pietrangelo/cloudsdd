// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package azure

import (
	"fmt"
	"strings"

	"github.com/pulumi/pulumi-azure/sdk/v5/go/azure/core"
	"github.com/pulumi/pulumi-azure/sdk/v5/go/azure/network"
	"github.com/pulumi/pulumi-azure/sdk/v5/go/azure/privatedns"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// Address space for a database's virtual network (RFC 015 §2.2).
//
// Deliberately not 10.0.0.0/16, which RFC 013 gave compute_instance.
// The two occupy separate VNets today and could overlap indefinitely
// without breaking anything, but the moment something peers them —
// which is the point of the network model RFC 015 §1.1 defers —
// identical ranges make peering impossible. One constant now removes a
// migration later.
const (
	databaseVNetAddressSpace  = "10.1.0.0/16"
	databaseSubnetAddressSpec = "10.1.1.0/24"
)

// subnetJoinAction is the only action the delegation needs: it lets the
// database service place its resources in our subnet, and nothing else.
const subnetJoinAction = "Microsoft.Network/virtualNetworks/subnets/join/action"

// engineNetworking carries the two values that differ between engines.
//
// A table rather than a branch inside each declare function: the whole
// point of RFC 015 §2.2 is that PostgreSQL and MySQL now have the same
// resource graph, and a shape that made it easy to give them different
// ones again would undo that.
type engineNetworking struct {
	// delegation is the Azure service the subnet is delegated to.
	delegation string
	// dnsSuffix is the mandatory tail of the private DNS zone name;
	// Azure validates it and rejects anything else.
	dnsSuffix string
}

var networkingByEngine = map[string]engineNetworking{
	"postgres": {
		delegation: "Microsoft.DBforPostgreSQL/flexibleServers",
		dnsSuffix:  "postgres.database.azure.com",
	},
	"mysql": {
		delegation: "Microsoft.DBforMySQL/flexibleServers",
		dnsSuffix:  "mysql.database.azure.com",
	},
}

// databaseNetwork is what a declared database network exposes to the
// server that will sit inside it.
type databaseNetwork struct {
	subnetID     pulumi.IDOutput
	privateDNSID pulumi.IDOutput
}

// declareDatabaseNetwork builds the VNet, the delegated subnet, the
// private DNS zone and its link to the VNet (RFC 015 §2.2).
//
// This is what makes an Azure database private. The posture it replaces
// differed by engine and neither half was satisfying: MySQL had a public
// endpoint reachable the moment anyone added a firewall rule, and
// PostgreSQL had its public endpoint disabled with no private path at
// all, which is secure the way an unplugged server is secure. With VNet
// integration there is no public endpoint for a firewall rule to open,
// and there is a network object to attach or peer to.
func declareDatabaseNetwork(
	ctx *pulumi.Context,
	id string,
	rg *core.ResourceGroup,
	p relationalDatabaseProperties,
) (databaseNetwork, error) {
	cfg, ok := networkingByEngine[strings.ToLower(p.Engine)]
	if !ok {
		// Unreachable via decode, like the engine switch itself; kept so
		// an engine added to the validator tag cannot silently reach a
		// server declaration with no networking configured for it.
		return databaseNetwork{}, fmt.Errorf(
			"azure: no network configuration for database engine %q", p.Engine)
	}

	vnet, err := network.NewVirtualNetwork(ctx, id+"-db-vnet", &network.VirtualNetworkArgs{
		ResourceGroupName: rg.Name,
		Location:          rg.Location,
		AddressSpaces:     pulumi.StringArray{pulumi.String(databaseVNetAddressSpace)},
	})
	if err != nil {
		return databaseNetwork{}, fmt.Errorf("azure: failed to declare the database network for %q: %w", id, err)
	}

	subnet, err := network.NewSubnet(ctx, id+"-db-subnet", &network.SubnetArgs{
		ResourceGroupName:  rg.Name,
		VirtualNetworkName: vnet.Name,
		AddressPrefixes:    pulumi.StringArray{pulumi.String(databaseSubnetAddressSpec)},
		Delegations: network.SubnetDelegationArray{
			&network.SubnetDelegationArgs{
				Name: pulumi.String("database"),
				ServiceDelegation: &network.SubnetDelegationServiceDelegationArgs{
					Name:    pulumi.String(cfg.delegation),
					Actions: pulumi.StringArray{pulumi.String(subnetJoinAction)},
				},
			},
		},
	})
	if err != nil {
		return databaseNetwork{}, fmt.Errorf("azure: failed to declare the database subnet for %q: %w", id, err)
	}

	// The zone name's suffix is not cosmetic: Azure rejects a private DNS
	// zone for a Flexible Server whose name does not end in the engine's
	// own domain.
	zone, err := privatedns.NewZone(ctx, id+"-db-dns", &privatedns.ZoneArgs{
		Name:              pulumi.String(fmt.Sprintf("%s.%s", id, cfg.dnsSuffix)),
		ResourceGroupName: rg.Name,
	})
	if err != nil {
		return databaseNetwork{}, fmt.Errorf("azure: failed to declare the private DNS zone for %q: %w", id, err)
	}

	// Without the link the zone exists but resolves for nobody, and the
	// server's own hostname does not resolve inside its VNet.
	if _, err := privatedns.NewZoneVirtualNetworkLink(ctx, id+"-db-dns-link", &privatedns.ZoneVirtualNetworkLinkArgs{
		PrivateDnsZoneName: zone.Name,
		ResourceGroupName:  rg.Name,
		VirtualNetworkId:   vnet.ID(),
		// Databases are not virtual machines: nothing here should be
		// auto-registering records in the zone.
		RegistrationEnabled: pulumi.Bool(false),
	}); err != nil {
		return databaseNetwork{}, fmt.Errorf("azure: failed to link the private DNS zone for %q: %w", id, err)
	}

	return databaseNetwork{subnetID: subnet.ID(), privateDNSID: zone.ID()}, nil
}
