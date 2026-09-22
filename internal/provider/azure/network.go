// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package azure

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"net/netip"
	"strings"

	"github.com/pulumi/pulumi-azure/sdk/v5/go/azure/core"
	"github.com/pulumi/pulumi-azure/sdk/v5/go/azure/network"
	"github.com/pulumi/pulumi-azure/sdk/v5/go/azure/privatedns"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider"
	addr "cloudsdd/internal/provider/network"
	"cloudsdd/internal/spec"
)

// Subnet indices within the scope's range (RFC 016 §2.3).
//
// A subnet delegated to a database service cannot host a VM, and a
// delegation names one service, so the scope carries one general subnet
// plus one per database engine. Both engines' subnets are declared
// whether or not a database of that engine exists: the network is built
// before the resources in it, so it cannot know which engines the scope
// will hold, and an empty subnet costs nothing.
const (
	subnetIndexGeneral  = 0
	subnetIndexPostgres = 1
	subnetIndexMySQL    = 2
	// A Container Apps environment needs a subnet of its own, delegated to
	// Microsoft.App/environments, for the same reason the databases do: a
	// delegation names one service and excludes every other occupant (RFC
	// 017 §2.6). Added at index 3, so nothing already deployed is
	// renumbered.
	subnetIndexContainerApps = 3
)

// subnetJoinAction is the only action the delegation needs: it lets the
// database service place its resources in our subnet, and nothing else.
const subnetJoinAction = "Microsoft.Network/virtualNetworks/subnets/join/action"

// NAT gateway configuration (RFC 017 §2.7).
const (
	// A NAT gateway accepts only a Standard-SKU, statically allocated
	// address. Both are stated rather than defaulted: Azure's default
	// allocation is dynamic, which the gateway rejects at create time.
	natSkuStandard          = "Standard"
	natIPAllocationStatic   = "Static"
	natPublicIPSkuStandard  = "Standard"
	natIdleTimeoutInMinutes = 4
)

// engineNetworking carries the two values that differ between engines.
//
// A table rather than a branch in each declare function: RFC 015 §2.2
// made both engines share one resource graph, and a shape that made it
// easy to give them different ones again would undo that.
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

// EnsureNetwork provisions the shared VNet for a scope (RFC 016 §2.2).
//
// It replaces the per-resource networks RFC 015 built. Those gave each
// database a VNet containing nothing but itself, which is private and
// also useless: there was no way to put an application next to the
// database it was meant to talk to.
//
// The scope's contents are what the network is built for: the volumes its
// resources mount become shares in a storage account here (RFC 020 §2.5).
func (p *AzureProvider) EnsureNetwork(ctx context.Context, s provider.NetworkScope, contents provider.ScopeContents, policies spec.Policies) error {
	// object_storage reaches here without a region on some scopes and
	// lives in no network.
	if s.Region == "" {
		return nil
	}

	cidr, err := addr.Derive(provider.NetworkScopeOf(s), provider.AddressPolicyOf(policies))
	if err != nil {
		return err
	}

	program := func(pctx *pulumi.Context) error {
		return declareScopeNetwork(pctx, s, cidr, contents)
	}

	stack, err := p.upsertStack(ctx, s.Account, s.Environment, s.Region, "", program)
	if err != nil {
		return fmt.Errorf("azure: failed to prepare the network stack for scope %q: %w", provider.ScopeName(s), err)
	}
	if _, err := stack.Up(ctx); err != nil {
		return fmt.Errorf("azure: failed to provision the network for scope %q: %w", provider.ScopeName(s), err)
	}
	return nil
}

// declareScopeNetwork builds the resource group, VNet, subnets, security
// group, private DNS zones and filesystems the scope's resources share.
func declareScopeNetwork(ctx *pulumi.Context, s provider.NetworkScope, cidr netip.Prefix, contents provider.ScopeContents) error {
	name := scopeNetworkName(s)

	// The network gets a resource group of its own, separate from the
	// per-resource ones. A shared network inside a resource's group would
	// be destroyed with that resource, taking the rest of the scope's
	// connectivity with it.
	rg, err := core.NewResourceGroup(ctx, name+"-rg", &core.ResourceGroupArgs{
		Name:     pulumi.String(scopeResourceGroupName(s)),
		Location: pulumi.String(s.Region),
	})
	if err != nil {
		return fmt.Errorf("azure: failed to declare the network resource group for scope %q: %w",
			provider.ScopeName(s), err)
	}

	vnet, err := network.NewVirtualNetwork(ctx, name+"-vnet", &network.VirtualNetworkArgs{
		Name:              pulumi.String(name),
		ResourceGroupName: rg.Name,
		Location:          rg.Location,
		AddressSpaces:     pulumi.StringArray{pulumi.String(cidr.String())},
	})
	if err != nil {
		return fmt.Errorf("azure: failed to declare the vnet for scope %q: %w", provider.ScopeName(s), err)
	}

	// No security rules at all: an Azure NSG carries a DenyAllInBound
	// default, and nothing here opens it.
	nsg, err := network.NewNetworkSecurityGroup(ctx, name+"-nsg", &network.NetworkSecurityGroupArgs{
		Name:              pulumi.String(name + "-nsg"),
		ResourceGroupName: rg.Name,
		Location:          rg.Location,
	})
	if err != nil {
		return fmt.Errorf("azure: failed to declare the security group for scope %q: %w", provider.ScopeName(s), err)
	}

	// The general subnet, where compute instances live.
	general, err := subnetBlock(cidr, subnetIndexGeneral)
	if err != nil {
		return fmt.Errorf("azure: scope %q: %w", provider.ScopeName(s), err)
	}
	generalSubnet, err := network.NewSubnet(ctx, name+"-subnet", &network.SubnetArgs{
		Name:               pulumi.String(generalSubnetName),
		ResourceGroupName:  rg.Name,
		VirtualNetworkName: vnet.Name,
		AddressPrefixes:    pulumi.StringArray{pulumi.String(general.String())},
	})
	if err != nil {
		return fmt.Errorf("azure: failed to declare the general subnet for scope %q: %w", provider.ScopeName(s), err)
	}

	// The security group is attached to the subnet rather than to each
	// network interface, so everything placed in the subnet inherits it.
	// RFC 013 associated an NSG per NIC, which was the only option when
	// the VM owned its whole network; at scope level the perimeter is a
	// property of the network, and a resource cannot be created outside
	// it by forgetting to attach one.
	if _, err := network.NewSubnetNetworkSecurityGroupAssociation(ctx, name+"-nsg-assoc",
		&network.SubnetNetworkSecurityGroupAssociationArgs{
			SubnetId:               generalSubnet.ID(),
			NetworkSecurityGroupId: nsg.ID(),
		}); err != nil {
		return fmt.Errorf("azure: failed to attach the security group for scope %q: %w", provider.ScopeName(s), err)
	}

	if err := declareScopeEgress(ctx, s, rg, generalSubnet); err != nil {
		return err
	}

	if err := declareContainerAppsSubnet(ctx, s, rg, vnet, cidr); err != nil {
		return err
	}

	// One delegated subnet and one private DNS zone per engine. A subnet
	// delegated to Microsoft.DBforMySQL cannot host a PostgreSQL server
	// or a VM, so they cannot be shared.
	for _, engine := range []string{"postgres", "mysql"} {
		if err := declareEngineNetwork(ctx, s, rg, vnet, cidr, engine); err != nil {
			return err
		}
	}

	return declareScopeFilesystems(ctx, s, rg, vnet, generalSubnet, contents.Volumes)
}

// declareScopeEgress gives the general subnet a route out (RFC 017 §2.7).
//
// One NAT gateway for the scope, attached to the general subnet only. The
// delegated database subnets are deliberately left without one: a managed
// flexible server does not pull packages, and an egress path it never uses
// is a path an exfiltrating query could.
//
// Azure needs no public subnet for this — the gateway is a resource in its
// own right with an address attached, associated with a subnet rather than
// living in one — so unlike AWS there is nothing here a resource could be
// placed in by mistake.
func declareScopeEgress(
	ctx *pulumi.Context,
	s provider.NetworkScope,
	rg *core.ResourceGroup,
	subnet *network.Subnet,
) error {
	name := scopeNetworkName(s)

	address, err := network.NewPublicIp(ctx, name+"-nat-ip", &network.PublicIpArgs{
		Name:              pulumi.String(name + "-nat-ip"),
		ResourceGroupName: rg.Name,
		Location:          rg.Location,
		AllocationMethod:  pulumi.String(natIPAllocationStatic),
		Sku:               pulumi.String(natPublicIPSkuStandard),
	})
	if err != nil {
		return fmt.Errorf("azure: failed to declare the nat address for scope %q: %w", provider.ScopeName(s), err)
	}

	gateway, err := network.NewNatGateway(ctx, name+"-nat", &network.NatGatewayArgs{
		Name:                 pulumi.String(name + "-nat"),
		ResourceGroupName:    rg.Name,
		Location:             rg.Location,
		SkuName:              pulumi.String(natSkuStandard),
		IdleTimeoutInMinutes: pulumi.Int(natIdleTimeoutInMinutes),
	})
	if err != nil {
		return fmt.Errorf("azure: failed to declare the nat gateway for scope %q: %w", provider.ScopeName(s), err)
	}

	if _, err := network.NewNatGatewayPublicIpAssociation(ctx, name+"-nat-ip-assoc",
		&network.NatGatewayPublicIpAssociationArgs{
			NatGatewayId:      gateway.ID(),
			PublicIpAddressId: address.ID(),
		}); err != nil {
		return fmt.Errorf("azure: failed to attach the nat address for scope %q: %w", provider.ScopeName(s), err)
	}

	if _, err := network.NewSubnetNatGatewayAssociation(ctx, name+"-nat-subnet-assoc",
		&network.SubnetNatGatewayAssociationArgs{
			SubnetId:     subnet.ID(),
			NatGatewayId: gateway.ID(),
		}); err != nil {
		return fmt.Errorf("azure: failed to attach the nat gateway to the general subnet for scope %q: %w",
			provider.ScopeName(s), err)
	}
	return nil
}

// declareContainerAppsSubnet carves the subnet a Container Apps
// environment is injected into (RFC 017 §2.6).
//
// Declared with the network rather than with the first container service,
// for the reason the engine subnets already are: the network is built
// before the resources in it and cannot know what the scope will hold, and
// an empty subnet costs nothing.
//
// The delegation is what lets the Container Apps service place its
// infrastructure here, and — as with the database subnets — it is also
// what keeps anything else out.
func declareContainerAppsSubnet(
	ctx *pulumi.Context,
	s provider.NetworkScope,
	rg *core.ResourceGroup,
	vnet *network.VirtualNetwork,
	cidr netip.Prefix,
) error {
	name := scopeNetworkName(s)

	block, err := subnetBlock(cidr, subnetIndexContainerApps)
	if err != nil {
		return fmt.Errorf("azure: scope %q: %w", provider.ScopeName(s), err)
	}
	if _, err := network.NewSubnet(ctx, name+"-containerapps-subnet", &network.SubnetArgs{
		Name:               pulumi.String(containerAppsSubnetName),
		ResourceGroupName:  rg.Name,
		VirtualNetworkName: vnet.Name,
		AddressPrefixes:    pulumi.StringArray{pulumi.String(block.String())},
		Delegations: network.SubnetDelegationArray{
			&network.SubnetDelegationArgs{
				Name: pulumi.String("containerapps"),
				ServiceDelegation: &network.SubnetDelegationServiceDelegationArgs{
					Name:    pulumi.String(containerAppsDelegation),
					Actions: pulumi.StringArray{pulumi.String(subnetJoinAction)},
				},
			},
		},
	}); err != nil {
		return fmt.Errorf("azure: failed to declare the container apps subnet for scope %q: %w",
			provider.ScopeName(s), err)
	}
	return nil
}

// declareEngineNetwork builds the delegated subnet, private DNS zone and
// zone link one database engine needs.
func declareEngineNetwork(
	ctx *pulumi.Context,
	s provider.NetworkScope,
	rg *core.ResourceGroup,
	vnet *network.VirtualNetwork,
	cidr netip.Prefix,
	engine string,
) error {
	cfg, ok := networkingByEngine[engine]
	if !ok {
		return fmt.Errorf("azure: no network configuration for database engine %q", engine)
	}
	name := scopeNetworkName(s)

	block, err := subnetBlock(cidr, engineSubnetIndex(engine))
	if err != nil {
		return fmt.Errorf("azure: scope %q: %w", provider.ScopeName(s), err)
	}
	if _, err := network.NewSubnet(ctx, fmt.Sprintf("%s-%s-subnet", name, engine), &network.SubnetArgs{
		Name:               pulumi.String(engineSubnetName(engine)),
		ResourceGroupName:  rg.Name,
		VirtualNetworkName: vnet.Name,
		AddressPrefixes:    pulumi.StringArray{pulumi.String(block.String())},
		Delegations: network.SubnetDelegationArray{
			&network.SubnetDelegationArgs{
				Name: pulumi.String("database"),
				ServiceDelegation: &network.SubnetDelegationServiceDelegationArgs{
					Name:    pulumi.String(cfg.delegation),
					Actions: pulumi.StringArray{pulumi.String(subnetJoinAction)},
				},
			},
		},
	}); err != nil {
		return fmt.Errorf("azure: failed to declare the %s subnet for scope %q: %w", engine, provider.ScopeName(s), err)
	}

	// One zone per engine per scope, shared by every database of that
	// engine in the scope — which is the change from RFC 015, where each
	// database had a zone of its own. Azure validates the suffix.
	zone, err := privatedns.NewZone(ctx, fmt.Sprintf("%s-%s-dns", name, engine), &privatedns.ZoneArgs{
		Name:              pulumi.String(engineZoneName(s, engine)),
		ResourceGroupName: rg.Name,
	})
	if err != nil {
		return fmt.Errorf("azure: failed to declare the %s dns zone for scope %q: %w", engine, provider.ScopeName(s), err)
	}

	if _, err := privatedns.NewZoneVirtualNetworkLink(ctx,
		fmt.Sprintf("%s-%s-dns-link", name, engine),
		&privatedns.ZoneVirtualNetworkLinkArgs{
			Name:               pulumi.String(engine + "-link"),
			PrivateDnsZoneName: zone.Name,
			ResourceGroupName:  rg.Name,
			VirtualNetworkId:   vnet.ID(),
			// Databases are not virtual machines: nothing should be
			// auto-registering records here.
			RegistrationEnabled: pulumi.Bool(false),
		}); err != nil {
		return fmt.Errorf("azure: failed to link the %s dns zone for scope %q: %w", engine, provider.ScopeName(s), err)
	}
	return nil
}

// Subnet names inside the scope's VNet. Fixed rather than derived: they
// are scoped by the VNet they sit in, and a resource program has to name
// them to find them.
const generalSubnetName = "general"

// containerAppsSubnetName is the subnet a Container Apps environment is
// injected into, and containerAppsDelegation is the service it belongs to.
const (
	containerAppsSubnetName = "containerapps"
	containerAppsDelegation = "Microsoft.App/environments"
)

func engineSubnetName(engine string) string { return engine }

func engineSubnetIndex(engine string) int {
	if engine == "mysql" {
		return subnetIndexMySQL
	}
	return subnetIndexPostgres
}

// engineZoneName is the private DNS zone shared by a scope's databases of
// one engine. Azure rejects a zone whose name does not end in the
// engine's own domain.
func engineZoneName(s provider.NetworkScope, engine string) string {
	return fmt.Sprintf("%s.%s", scopeNetworkName(s), networkingByEngine[engine].dnsSuffix)
}

// scopeResourceGroupName is the resource group the scope's network lives
// in. Derived, so a resource program in another stack can name it.
func scopeResourceGroupName(s provider.NetworkScope) string {
	return scopeNetworkName(s) + "-net-rg"
}

// scopeNetworkName derives the VNet's name from the scope.
//
// Azure resources are addressable by (resource group, name), both of
// which derive from the scope, so the two stacks agree without a shared
// state backend — the same approach GCP takes, and the reason RFC 016
// §7.2 settled on discovery by name rather than by StackReference.
func scopeNetworkName(s provider.NetworkScope) string {
	var b strings.Builder
	b.WriteString("cloudsdd-")
	for _, r := range strings.ToLower(provider.ScopeName(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}

	name := b.String()
	// Azure allows 64 characters for a VNet, and the DNS zone built from
	// this name has to fit too. Replacing the tail with a hash rather
	// than truncating keeps two long scopes on distinct networks.
	if len(name) > 40 {
		name = name[:31] + "-" + provider.ShortHash(name)
	}
	return strings.TrimRight(name, "-")
}

// subnetBlock carves the index'th /24 out of the scope's range, using the
// whole-address arithmetic the AWS provider settled on: an octet
// increment is correct only while the scope is a /16 or smaller.
func subnetBlock(cidr netip.Prefix, index int) (netip.Prefix, error) {
	const subnetBits = 24

	if !cidr.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("scope range %s is not IPv4", cidr)
	}
	if cidr.Bits() > subnetBits {
		return netip.Prefix{}, fmt.Errorf("scope range %s is smaller than a /%d subnet", cidr, subnetBits)
	}
	available := uint64(1) << (subnetBits - cidr.Bits())
	if index < 0 || uint64(index) >= available {
		return netip.Prefix{}, fmt.Errorf("scope range %s holds %d subnets, asked for index %d",
			cidr, available, index)
	}

	baseAddr := cidr.Addr().As4()
	start := uint64(binary.BigEndian.Uint32(baseAddr[:]))
	a := start + uint64(index)*(1<<(32-subnetBits))
	if a > math.MaxUint32 {
		return netip.Prefix{}, fmt.Errorf("subnet %d does not fit in %s", index, cidr)
	}

	var raw [4]byte
	binary.BigEndian.PutUint32(raw[:], uint32(a))
	return netip.PrefixFrom(netip.AddrFrom4(raw), subnetBits), nil
}

// DestroyNetwork removes the scope's network stack (RFC 016 §2.6).
//
// The Engine establishes that the scope is empty before calling this.
// Nothing here can check: a scope's contents live in other stacks, and
// this one knows only about the network.
func (p *AzureProvider) DestroyNetwork(ctx context.Context, s provider.NetworkScope, contents provider.ScopeContents, policies spec.Policies) error {
	if s.Region == "" {
		return nil
	}

	cidr, err := addr.Derive(provider.NetworkScopeOf(s), provider.AddressPolicyOf(policies))
	if err != nil {
		return err
	}

	program := func(pctx *pulumi.Context) error {
		return declareScopeNetwork(pctx, s, cidr, contents)
	}
	stack, err := p.upsertStack(ctx, s.Account, s.Environment, s.Region, "", program)
	if err != nil {
		return fmt.Errorf("azure: failed to select the network stack for scope %q: %w", provider.ScopeName(s), err)
	}
	if _, err := stack.Destroy(ctx); err != nil {
		return fmt.Errorf("azure: failed to destroy the network for scope %q: %w", provider.ScopeName(s), err)
	}
	return nil
}
