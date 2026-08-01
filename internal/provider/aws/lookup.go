// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package aws

import (
	"fmt"
	"strings"

	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/ec2"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/spec"
)

// scopeNetwork is what a resource program needs to know about the network
// it is being placed into.
type scopeNetwork struct {
	vpcID        string
	cidr         string
	subnetIDs    []string
	dbSubnetName string
}

// lookupScopeNetwork finds the VPC the Engine already provisioned for
// this resource's scope (RFC 016 §2.2).
//
// The Engine guarantees ordering — EnsureNetwork runs for a scope before
// any resource in it is applied — so a missing VPC here is not a race but
// a broken invariant, and the error says so rather than silently falling
// back to the default VPC. Falling back is precisely the behaviour this
// RFC exists to remove: a resource quietly landing in a network shared
// with everything else in the account.
func lookupScopeNetwork(ctx *pulumi.Context, r spec.Resource) (scopeNetwork, error) {
	scope := provider.NetworkScope{
		Provider:    spec.ProviderAWS,
		Account:     r.Account,
		Environment: r.Scope.Environment,
		Region:      r.Scope.Region,
	}
	tag := scopeTag(scope)

	vpc, err := ec2.LookupVpc(ctx, &ec2.LookupVpcArgs{
		Tags: map[string]string{
			tagManagedBy: managedByValue,
			tagScope:     tag,
		},
	}, nil)
	if err != nil {
		return scopeNetwork{}, fmt.Errorf(
			"aws: no CloudSDD network found for scope %q. The network is created before "+
				"the resources in it, so this means it was removed out of band: %w", tag, err)
	}

	subnets, err := ec2.GetSubnets(ctx, &ec2.GetSubnetsArgs{
		Filters: []ec2.GetSubnetsFilter{
			{Name: "vpc-id", Values: []string{vpc.Id}},
			{Name: "tag:" + tagSubnetTier, Values: []string{tierPrivate}},
		},
	}, nil)
	if err != nil {
		return scopeNetwork{}, fmt.Errorf("aws: failed to find the private subnets of scope %q: %w", tag, err)
	}
	if len(subnets.Ids) == 0 {
		return scopeNetwork{}, fmt.Errorf("aws: the network for scope %q has no private subnets", tag)
	}

	return scopeNetwork{
		vpcID:     vpc.Id,
		cidr:      vpc.CidrBlock,
		subnetIDs: subnets.Ids,
		// Computed, not looked up. The network program gives the subnet
		// group an explicit physical name derived from the same scope, so
		// both halves reach it by arithmetic rather than by a third
		// invoke — and there is no way for them to disagree about a name
		// they each derive from the same input.
		dbSubnetName: dbSubnetGroupNameFor(scope),
	}, nil
}

// dbSubnetGroupNameFor is the physical name of a scope's DB subnet group.
//
// Explicit rather than Pulumi-autonamed, because a resource program in a
// *different* stack has to name it and cannot see the other stack's
// random suffix. Deriving it from the scope is what makes the two stacks
// agree without talking to each other.
//
// RDS accepts lowercase letters, digits and hyphens, so every other
// character in a scope label is folded to a hyphen. That is lossy —
// "a-b" and "a:b" fold together — which is harmless here because both
// halves fold identically and a scope only ever has to match itself.
func dbSubnetGroupNameFor(s provider.NetworkScope) string {
	var b strings.Builder
	b.WriteString("cloudsdd-")
	for _, r := range strings.ToLower(scopeTag(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// subnet picks the subnet a resource is placed in.
//
// The zone argument is honoured where it can be — a compute_instance
// names one availability zone (RFC 013 §2.3), and placing it in a subnet
// in a different one would be infrastructure that does not match what the
// user wrote. Zones are not known to the subnet IDs here, so selection is
// by index derived from the zone name, which is stable for a given
// network: the alternative, a fourth invoke to describe each subnet, buys
// nothing a deterministic choice does not already give.
//
// An empty zone takes the first subnet, which is the ordinary case for a
// resource that expressed no preference.
func (n scopeNetwork) subnet(zone string) string {
	if len(n.subnetIDs) == 0 {
		return ""
	}
	if zone == "" {
		return n.subnetIDs[0]
	}
	// The last character of an AZ name is its letter suffix ("a", "b"),
	// which orders the same way the subnets were created.
	last := zone[len(zone)-1]
	if last >= 'a' && last <= 'z' {
		if idx := int(last - 'a'); idx < len(n.subnetIDs) {
			return n.subnetIDs[idx]
		}
	}
	return n.subnetIDs[0]
}
