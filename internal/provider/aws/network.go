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

	// tierPrivate marks the subnets resources are placed in. Named now,
	// with no public tier yet, because the distinction is the point of
	// the layout: a database must never land in a subnet with a route to
	// an internet gateway, and the tag is how a resource program tells
	// them apart.
	tierPrivate = "private"
)

// subnetCount is how many availability zones the network spans.
//
// Two, because RDS refuses a DB subnet group with fewer — it will not
// create an instance it could never fail over. So the network is multi-AZ
// whether or not the user asked for high availability, which costs
// nothing: an empty subnet is free.
const subnetCount = 2

// EnsureNetwork provisions the shared VPC for a scope (RFC 016 §2.2).
//
// Idempotent: the stack is upserted and the program is declarative, so a
// second call converges on what the first created. The Engine calls it
// once per scope before applying anything in that scope, so a resource
// program may assume the network exists.
func (p *AWSProvider) EnsureNetwork(ctx context.Context, s provider.NetworkScope, policies spec.Policies) error {
	// A scope with no region has no VPC. object_storage and
	// cross_account_role are the only resources that reach here without
	// one, and neither lives in a network.
	if s.Region == "" {
		return nil
	}

	cidr, err := network.Derive(networkScope(s), addressPolicy(policies))
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

// declareScopeNetwork builds the VPC, its private subnets and the DB
// subnet group.
//
// There is no internet gateway and no NAT: nothing here has a route out.
// That is deliberate for the resource types that exist today — a database
// and a VM reached through Session Manager both work without egress —
// and RFC 016 §7.1 leaves the question open for RFC 017, which is where
// pulling a container image makes it unavoidable.
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
		subnetIDs = append(subnetIDs, subnet.ID())
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
func scopeTag(s provider.NetworkScope) string {
	label := networkScope(s).String()
	if label == "" {
		// A scope with no account and no environment is still a scope;
		// an empty tag value would make it unfindable.
		return "default"
	}
	return label
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

// networkScope projects a provider scope onto the address model.
func networkScope(s provider.NetworkScope) network.Scope {
	return network.Scope{
		Account:     s.Account,
		Environment: s.Environment,
		Region:      s.Region,
	}
}

// addressPolicy translates the Specification's network policy for the
// address package, which stays free of spec types.
func addressPolicy(p spec.Policies) network.Policy {
	if p.Network == nil {
		return network.Policy{}
	}
	return network.Policy{BaseCIDR: p.Network.BaseCIDR, Scopes: p.Network.Scopes}
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
func (p *AWSProvider) DestroyNetwork(ctx context.Context, s provider.NetworkScope, policies spec.Policies) error {
	if s.Region == "" {
		return nil
	}

	cidr, err := network.Derive(networkScope(s), addressPolicy(policies))
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
