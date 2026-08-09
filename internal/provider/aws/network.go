// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package aws

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"net/netip"

	pulumiaws "github.com/pulumi/pulumi-aws/sdk/v6/go/aws"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/ec2"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/rds"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/provider/network"
	"cloudsdd/internal/spec"
)

// Tags every CloudSDD network carries. They are the contract between the
// network stack and the resource stacks that sit in it (RFC 016 §2.2).
//
// Resource programs find their network by tag rather than through a
// Pulumi StackReference, which is what RFC 016 §2.2 first proposed; §7.2
// flagged that mechanism as unverified and this is where it had to be
// settled. A StackReference couples a resource stack to the *state
// backend* and the stack naming of another stack, and CloudSDD's backend
// is a local directory a user can move, share or lose independently of
// the cloud. A tag lives in the account, next to the thing it describes,
// and answers the same question — "which VPC belongs to this scope?" —
// from whichever machine happens to be asking.
const (
	tagManagedBy   = "ManagedBy"
	tagScope       = "CloudSDDScope"
	tagSubnetTier  = "CloudSDDTier"
	managedByValue = "cloudsdd"

	// tierPrivate marks the subnets resources are placed in: everything
	// CloudSDD creates lands in one. The distinction from tierPublic is
	// the point of the layout — a database must never sit in a subnet
	// with a route to an internet gateway, and the tag is how a resource
	// program tells them apart.
	tierPrivate = "private"

	// tierPublic marks the one subnet holding the NAT gateway (RFC 017
	// §2.7). No CloudSDD resource is ever placed in it. It is tagged all
	// the same, so a resource program that went looking for a subnet by
	// tier can distinguish it rather than find it untagged and guess.
	tierPublic = "public"
)

// subnetCount is how many availability zones the network spans.
//
// Two, because RDS refuses a DB subnet group with fewer — it will not
// create an instance it could never fail over. So the network is multi-AZ
// whether or not the user asked for high availability, which costs
// nothing: an empty subnet is free.
const subnetCount = 2

// subnetIndexPublic is where the public tier starts in the scope's /20.
//
// It follows the private subnets, so adding it renumbers nothing already
// deployed: private subnets keep indices 0..subnetCount-1 and the /20 has
// room for sixteen /24s in total (RFC 016 §2.4).
const subnetIndexPublic = subnetCount

// publicSubnetCount is how many availability zones the public tier spans.
//
// Two, and for a reason that only appeared in RFC 017 step 3: an
// internet-facing Application Load Balancer requires subnets in at least
// two availability zones and refuses to be created with one. A single
// public subnet was enough for the NAT gateway and would have made every
// public container service fail on apply, after the user approved a plan
// that looked fine.
//
// The NAT still lives in the first subnet alone — this is not one gateway
// per zone, and the cost argument in RFC 016 §7.1 is unchanged. The second
// subnet holds nothing until a load balancer needs it, and an empty subnet
// is free.
const publicSubnetCount = 2

// defaultRoute is the destination a route table uses for "everything else".
const defaultRoute = "0.0.0.0/0"

// EnsureNetwork provisions the shared VPC for a scope (RFC 016 §2.2).
//
// Idempotent: the stack is upserted and the program is declarative, so a
// second call converges on what the first created. The Engine calls it
// once per scope before applying anything in that scope, so a resource
// program may assume the network exists.
//
// The scope's contents are not read yet: this stack declares no
// filesystem until RFC 020 §2.5 lands EFS here.
func (p *AWSProvider) EnsureNetwork(ctx context.Context, s provider.NetworkScope, _ provider.ScopeContents, policies spec.Policies) error {
	// A scope with no region has no VPC. object_storage and
	// cross_account_role are the only resources that reach here without
	// one, and neither lives in a network.
	if s.Region == "" {
		return nil
	}

	cidr, err := network.Derive(provider.NetworkScopeOf(s), provider.AddressPolicyOf(policies))
	if err != nil {
		return err
	}

	stack, err := p.upsertStack(ctx, s.Account, s.Environment, s.Region, "", p.networkProgram(s, cidr))
	if err != nil {
		return fmt.Errorf("aws: failed to prepare the network stack for scope %q: %w", scopeTag(s), err)
	}
	if err := p.setRegionConfig(ctx, stack, s.Region); err != nil {
		return err
	}
	if _, err := stack.Up(ctx); err != nil {
		return fmt.Errorf("aws: failed to provision the network for scope %q: %w", scopeTag(s), err)
	}
	return nil
}

// declareScopeNetwork builds the VPC, its private subnets, the egress
// path and the DB subnet group.
//
// The egress path is the RFC 017 §2.7 repair. RFC 016 shipped this network
// with no route out, arguing that a database and a VM reached through
// Session Manager both work without one. The database does; the VM does
// not, because the SSM agent is a client that has to reach the SSM
// endpoints before any session exists. An instance therefore booted into a
// subnet where nothing could talk to it and no package could be fetched.
func declareScopeNetwork(
	ctx *pulumi.Context,
	s provider.NetworkScope,
	cidr netip.Prefix,
	opts ...pulumi.ResourceOption,
) error {
	const name = "cloudsdd-net"

	vpc, err := ec2.NewVpc(ctx, name, &ec2.VpcArgs{
		CidrBlock: pulumi.String(cidr.String()),
		// Both are needed for RDS to hand out a resolvable endpoint, and
		// for anything in the VPC to resolve it.
		EnableDnsHostnames: pulumi.Bool(true),
		EnableDnsSupport:   pulumi.Bool(true),
		Tags:               scopeTags(s, nil),
	}, opts...)
	if err != nil {
		return fmt.Errorf("aws: failed to declare the vpc for scope %q: %w", scopeTag(s), err)
	}

	// The AZ list is an invoke, the second this provider makes (RFC 013
	// §4 recorded the AMI lookup as the first), and unavoidable for the
	// same reason: zone names are region-specific and cannot be known
	// without asking.
	state := "available"
	azs, err := pulumiaws.GetAvailabilityZones(ctx, &pulumiaws.GetAvailabilityZonesArgs{
		State: &state,
	}, nil)
	if err != nil {
		return fmt.Errorf("aws: failed to look up availability zones for scope %q: %w", scopeTag(s), err)
	}
	if len(azs.Names) < subnetCount {
		return fmt.Errorf("aws: region %q has %d availability zones, need %d for a database subnet group",
			s.Region, len(azs.Names), subnetCount)
	}

	private := make([]*ec2.Subnet, 0, subnetCount)
	subnetIDs := make(pulumi.StringArray, 0, subnetCount)
	for i := 0; i < subnetCount; i++ {
		block, err := subnetBlock(cidr, i)
		if err != nil {
			return fmt.Errorf("aws: scope %q: %w", scopeTag(s), err)
		}

		subnet, err := ec2.NewSubnet(ctx, fmt.Sprintf("%s-private-%d", name, i), &ec2.SubnetArgs{
			VpcId:            vpc.ID(),
			CidrBlock:        pulumi.String(block.String()),
			AvailabilityZone: pulumi.String(azs.Names[i]),
			// Explicit rather than left to the default: an instance that
			// acquired a public address by accident would be reachable
			// from the internet the moment any security group allowed it.
			MapPublicIpOnLaunch: pulumi.Bool(false),
			Tags:                scopeTags(s, map[string]string{tagSubnetTier: tierPrivate}),
		}, opts...)
		if err != nil {
			return fmt.Errorf("aws: failed to declare subnet %d for scope %q: %w", i, scopeTag(s), err)
		}
		private = append(private, subnet)
		subnetIDs = append(subnetIDs, subnet.ID())
	}

	if err := declareScopeEgress(ctx, s, vpc, cidr, azs.Names, private, opts...); err != nil {
		return err
	}

	// The DB subnet group is what allows an RDS instance into this VPC at
	// all. It belongs here, with the network, rather than with the
	// database: it describes the network and is shared by every database
	// in the scope.
	if _, err := rds.NewSubnetGroup(ctx, name+"-db", &rds.SubnetGroupArgs{
		// Explicit physical name: a database in a different stack has to
		// reference this group and cannot see a Pulumi-generated suffix.
		Name:      pulumi.String(dbSubnetGroupNameFor(s)),
		SubnetIds: subnetIDs,
		Tags:      scopeTags(s, nil),
	}, opts...); err != nil {
		return fmt.Errorf("aws: failed to declare the db subnet group for scope %q: %w", scopeTag(s), err)
	}

	return nil
}

// declareScopeEgress gives the private subnets a route out (RFC 017 §2.7).
//
// One NAT gateway for the whole scope, not one per availability zone.
// Per-AZ NAT buys survival of a single-zone outage and avoids a cross-zone
// data charge; neither is worth doubling the standing cost of every
// environment CloudSDD creates, on a tool whose other headline feature is
// switching things off at night (RFC 016 §7.1 priced it).
//
// The public subnets exist to hold the gateway and, since RFC 017 §2.3.1,
// an internet-facing load balancer. Nothing else is ever placed in them,
// and they do not assign public addresses on launch either — the NAT
// gateway carries an elastic IP of its own and a load balancer brings its
// own addresses, so nothing here needs the automatic assignment that would
// make a stray instance internet-facing.
func declareScopeEgress(
	ctx *pulumi.Context,
	s provider.NetworkScope,
	vpc *ec2.Vpc,
	cidr netip.Prefix,
	zones []string,
	private []*ec2.Subnet,
	opts ...pulumi.ResourceOption,
) error {
	const name = "cloudsdd-net"

	if len(zones) < publicSubnetCount {
		return fmt.Errorf("aws: region %q has %d availability zones, need %d for a public load balancer",
			s.Region, len(zones), publicSubnetCount)
	}

	igw, err := ec2.NewInternetGateway(ctx, name+"-igw", &ec2.InternetGatewayArgs{
		VpcId: vpc.ID(),
		Tags:  scopeTags(s, nil),
	}, opts...)
	if err != nil {
		return fmt.Errorf("aws: failed to declare the internet gateway for scope %q: %w", scopeTag(s), err)
	}

	publicRoutes, err := ec2.NewRouteTable(ctx, name+"-public-rt", &ec2.RouteTableArgs{
		VpcId: vpc.ID(),
		Routes: ec2.RouteTableRouteArray{
			&ec2.RouteTableRouteArgs{
				CidrBlock: pulumi.String(defaultRoute),
				GatewayId: igw.ID(),
			},
		},
		Tags: scopeTags(s, map[string]string{tagSubnetTier: tierPublic}),
	}, opts...)
	if err != nil {
		return fmt.Errorf("aws: failed to declare the public route table for scope %q: %w", scopeTag(s), err)
	}

	// The public tier spans two zones because an internet-facing ALB
	// refuses to exist in one. Only the first holds the NAT gateway.
	public := make([]*ec2.Subnet, 0, publicSubnetCount)
	for i := 0; i < publicSubnetCount; i++ {
		block, err := subnetBlock(cidr, subnetIndexPublic+i)
		if err != nil {
			return fmt.Errorf("aws: scope %q: %w", scopeTag(s), err)
		}
		subnet, err := ec2.NewSubnet(ctx, fmt.Sprintf("%s-public-%d", name, i), &ec2.SubnetArgs{
			VpcId:               vpc.ID(),
			CidrBlock:           pulumi.String(block.String()),
			AvailabilityZone:    pulumi.String(zones[i]),
			MapPublicIpOnLaunch: pulumi.Bool(false),
			Tags:                scopeTags(s, map[string]string{tagSubnetTier: tierPublic}),
		}, opts...)
		if err != nil {
			return fmt.Errorf("aws: failed to declare public subnet %d for scope %q: %w", i, scopeTag(s), err)
		}
		if _, err := ec2.NewRouteTableAssociation(ctx, fmt.Sprintf("%s-public-rta-%d", name, i),
			&ec2.RouteTableAssociationArgs{
				SubnetId:     subnet.ID(),
				RouteTableId: publicRoutes.ID(),
			}, opts...); err != nil {
			return fmt.Errorf("aws: failed to associate the public route table for subnet %d in scope %q: %w",
				i, scopeTag(s), err)
		}
		public = append(public, subnet)
	}

	address, err := ec2.NewEip(ctx, name+"-nat-eip", &ec2.EipArgs{
		Domain: pulumi.String("vpc"),
		Tags:   scopeTags(s, nil),
	}, opts...)
	if err != nil {
		return fmt.Errorf("aws: failed to declare the nat address for scope %q: %w", scopeTag(s), err)
	}

	// DependsOn the gateway explicitly: AWS rejects a NAT gateway created
	// before the internet gateway is attached, and the dependency is not
	// implied by any argument here — the NAT references the subnet and the
	// address, never the gateway it needs.
	nat, err := ec2.NewNatGateway(ctx, name+"-nat", &ec2.NatGatewayArgs{
		SubnetId:     public[0].ID(),
		AllocationId: address.ID(),
		Tags:         scopeTags(s, nil),
	}, append(opts, pulumi.DependsOn([]pulumi.Resource{igw}))...)
	if err != nil {
		return fmt.Errorf("aws: failed to declare the nat gateway for scope %q: %w", scopeTag(s), err)
	}

	// One route table shared by every private subnet: they all leave
	// through the same gateway, so a table each would be three copies of
	// one decision.
	privateRoutes, err := ec2.NewRouteTable(ctx, name+"-private-rt", &ec2.RouteTableArgs{
		VpcId: vpc.ID(),
		Routes: ec2.RouteTableRouteArray{
			&ec2.RouteTableRouteArgs{
				CidrBlock:    pulumi.String(defaultRoute),
				NatGatewayId: nat.ID(),
			},
		},
		Tags: scopeTags(s, map[string]string{tagSubnetTier: tierPrivate}),
	}, opts...)
	if err != nil {
		return fmt.Errorf("aws: failed to declare the private route table for scope %q: %w", scopeTag(s), err)
	}
	for i, subnet := range private {
		if _, err := ec2.NewRouteTableAssociation(ctx, fmt.Sprintf("%s-private-rta-%d", name, i),
			&ec2.RouteTableAssociationArgs{
				SubnetId:     subnet.ID(),
				RouteTableId: privateRoutes.ID(),
			}, opts...); err != nil {
			return fmt.Errorf("aws: failed to associate the private route table for subnet %d in scope %q: %w",
				i, scopeTag(s), err)
		}
	}
	return nil
}

// subnetBlock carves the index'th subnet out of the scope's range.
//
// The scope gets a /20 and each subnet takes a /24, so there is room for
// sixteen — far more than the two used today, which is the point: a later
// resource type needing its own tier must not force the network to be
// renumbered.
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

	// Whole-address arithmetic in uint64, narrowed once after the bound
	// check. An earlier version incremented the third octet directly,
	// which is correct only while the scope is a /16 or smaller — for a
	// wider block the index exceeds 255 and the octet wraps, quietly
	// naming a subnet inside a different part of the range. gosec caught
	// the conversion; the conversion was the symptom.
	baseAddr := cidr.Addr().As4()
	start := uint64(binary.BigEndian.Uint32(baseAddr[:]))
	addr := start + uint64(index)*(1<<(32-subnetBits))
	if addr > math.MaxUint32 {
		return netip.Prefix{}, fmt.Errorf("subnet %d does not fit in %s", index, cidr)
	}

	var raw [4]byte
	binary.BigEndian.PutUint32(raw[:], uint32(addr))
	return netip.PrefixFrom(netip.AddrFrom4(raw), subnetBits), nil
}

// scopeTag renders the scope as the CloudSDDScope tag value, and as the
// label in error messages.
//
// It is provider.ScopeName under the name this provider needs: on AWS the
// label is not only prose, it is the value a resource program searches the
// account by. That is also why the shared form's "default" fallback
// matters more here than elsewhere — a scope with no account and no
// environment is still a scope, and an empty tag value would make its
// network unfindable.
func scopeTag(s provider.NetworkScope) string {
	return provider.ScopeName(s)
}

// scopeTags is the tag set every network resource carries, plus whatever
// extras the caller needs.
func scopeTags(s provider.NetworkScope, extra map[string]string) pulumi.StringMap {
	tags := pulumi.StringMap{
		tagManagedBy: pulumi.String(managedByValue),
		tagScope:     pulumi.String(scopeTag(s)),
	}
	for k, v := range extra {
		tags[k] = pulumi.String(v)
	}
	return tags
}

// networkProgram is the scope network's Pulumi program, shared by
// EnsureNetwork and DestroyNetwork. A destroy works from state rather
// than from the program, but a stack cannot be selected without one, and
// passing the same program keeps the two paths from describing different
// networks.
func (p *AWSProvider) networkProgram(s provider.NetworkScope, cidr netip.Prefix) pulumi.RunFunc {
	return func(pctx *pulumi.Context) error {
		opts, _, err := p.providerOpts(pctx, s.Region)
		if err != nil {
			return err
		}
		return declareScopeNetwork(pctx, s, cidr, opts...)
	}
}

// DestroyNetwork removes the scope's network stack (RFC 016 §2.6).
//
// The Engine establishes that the scope is empty before calling this.
// Nothing here can check: a scope's contents live in other stacks, and
// this one knows only about the network.
func (p *AWSProvider) DestroyNetwork(ctx context.Context, s provider.NetworkScope, _ provider.ScopeContents, policies spec.Policies) error {
	if s.Region == "" {
		return nil
	}

	cidr, err := network.Derive(provider.NetworkScopeOf(s), provider.AddressPolicyOf(policies))
	if err != nil {
		return err
	}

	stack, err := p.upsertStack(ctx, s.Account, s.Environment, s.Region, "", p.networkProgram(s, cidr))
	if err != nil {
		return fmt.Errorf("aws: failed to select the network stack for scope %q: %w", scopeTag(s), err)
	}
	if err := p.setRegionConfig(ctx, stack, s.Region); err != nil {
		return err
	}
	if _, err := stack.Destroy(ctx); err != nil {
		return fmt.Errorf("aws: failed to destroy the network for scope %q: %w", scopeTag(s), err)
	}
	return nil
}
