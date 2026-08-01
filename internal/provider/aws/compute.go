// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package aws

import (
	"fmt"

	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/ec2"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/iam"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider/compute"
)

// ssmManagedInstancePolicy is the AWS-managed policy that lets Session
// Manager reach the instance.
//
// It is the whole grant: an instance profile exists here so an operator
// can open a shell through an IAM-authenticated, audited channel, not so
// the workload can call other services. Anything more would widen the
// blast radius of a machine running arbitrary code (RFC 013 §4).
const ssmManagedInstancePolicy = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"

// Image owner account IDs. Filtering on a name pattern alone would let any
// account publishing a public AMI with a matching name win the lookup,
// which is a direct path to running someone else's image (RFC 013 §4).
const (
	canonicalOwnerID = "099720109477"
	debianOwnerID    = "136693071363"
)

// machineType maps a cloud-agnostic size onto an EC2 instance type.
func machineType(size compute.Size) (string, error) {
	switch size {
	case compute.SizeSmall:
		return "t3.small", nil
	case compute.SizeMedium:
		return "t3.medium", nil
	case compute.SizeLarge:
		return "t3.large", nil
	default:
		return "", fmt.Errorf("aws: %w: %q", ErrUnsupportedSize, size)
	}
}

// imageFilter maps a cloud-agnostic OS onto the owner and name pattern
// that identify its AMI.
func imageFilter(os compute.OS) (owner, namePattern string, err error) {
	switch os {
	case compute.OSUbuntu2204:
		return canonicalOwnerID, "ubuntu/images/hvm-ssd/ubuntu-jammy-22.04-amd64-server-*", nil
	case compute.OSUbuntu2404:
		return canonicalOwnerID, "ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*", nil
	case compute.OSDebian12:
		return debianOwnerID, "debian-12-amd64-*", nil
	default:
		return "", "", fmt.Errorf("aws: %w: %q", ErrUnsupportedOS, os)
	}
}

// decodeComputeInstanceProperties decodes and validates Properties as a
// cloud-agnostic compute.Properties through the shared strict decoder.
func decodeComputeInstanceProperties(props map[string]any) (*compute.Properties, error) {
	var p compute.Properties
	if err := decodeProperties(props, &p); err != nil {
		return nil, err
	}
	// Fail at decode time rather than provisioning against an unmapped
	// size or image.
	if _, err := machineType(p.Size); err != nil {
		return nil, err
	}
	if _, _, err := imageFilter(p.OS); err != nil {
		return nil, err
	}
	return &p, nil
}

// declareComputeInstance registers the EC2 instance and everything it
// needs to be reachable without an inbound port: a security group with no
// ingress rules, and an instance profile for Session Manager (RFC 013).
func declareComputeInstance(
	ctx *pulumi.Context,
	id string,
	p compute.Properties,
	zone string,
	net scopeNetwork,
	invokeOpts []pulumi.InvokeOption,
	opts ...pulumi.ResourceOption,
) (*ec2.Instance, error) {
	instanceType, err := machineType(p.Size)
	if err != nil {
		return nil, err
	}

	ami, err := lookupImage(ctx, p.OS, invokeOpts)
	if err != nil {
		return nil, err
	}

	// No ingress rules at all. Interactive access arrives through Session
	// Manager, which the SSM agent establishes outbound, so nothing needs
	// to be reachable from outside.
	//
	// Egress stays open: the agent must reach the SSM endpoints and the
	// machine must be able to fetch its own security updates. Closing it
	// requires VPC interface endpoints, which needs a VPC this schema does
	// not yet model.
	sg, err := ec2.NewSecurityGroup(ctx, id+"-sg", &ec2.SecurityGroupArgs{
		VpcId:       pulumi.String(net.vpcID),
		Description: pulumi.String(fmt.Sprintf("CloudSDD %s: no inbound access", id)),
		Egress: ec2.SecurityGroupEgressArray{
			ec2.SecurityGroupEgressArgs{
				Protocol:   pulumi.String("-1"),
				FromPort:   pulumi.Int(0),
				ToPort:     pulumi.Int(0),
				CidrBlocks: pulumi.StringArray{pulumi.String("0.0.0.0/0")},
			},
		},
	}, opts...)
	if err != nil {
		return nil, fmt.Errorf("aws: failed to declare security group for %q: %w", id, err)
	}

	profile, err := declareInstanceProfile(ctx, id, opts...)
	if err != nil {
		return nil, err
	}

	args := &ec2.InstanceArgs{
		Ami:          pulumi.String(ami),
		InstanceType: pulumi.String(instanceType),
		// A private subnet in the scope's own VPC, not whatever the
		// account's default VPC would have supplied (RFC 016).
		SubnetId:            pulumi.String(net.subnet(zone)),
		VpcSecurityGroupIds: pulumi.StringArray{sg.ID()},
		IamInstanceProfile:  profile.Name,
		// Set explicitly rather than inherited from the subnet's
		// map_public_ip_on_launch, so "private by default" is a property
		// of the declaration and not of whatever VPC it lands in.
		AssociatePublicIpAddress: pulumi.Bool(p.EffectivePublicIP()),
		// IMDSv2 required, with a hop limit of 1. The classic EC2
		// compromise is an application SSRF that reaches the metadata
		// service and walks away with the role's credentials; requiring a
		// session token breaks that chain, and the hop limit stops a
		// container on the host from reaching it (RFC 013 §2.2).
		MetadataOptions: &ec2.InstanceMetadataOptionsArgs{
			HttpEndpoint:            pulumi.String("enabled"),
			HttpTokens:              pulumi.String("required"),
			HttpPutResponseHopLimit: pulumi.Int(1),
		},
		RootBlockDevice: &ec2.InstanceRootBlockDeviceArgs{
			Encrypted:  pulumi.Bool(true),
			VolumeSize: pulumi.Int(p.EffectiveDiskSizeGB()),
			VolumeType: pulumi.String("gp3"),
		},
		Tags: pulumi.StringMap{"Name": pulumi.String(id)},
	}
	if zone != "" {
		args.AvailabilityZone = pulumi.String(zone)
	}

	instance, err := ec2.NewInstance(ctx, id, args, opts...)
	if err != nil {
		return nil, fmt.Errorf("aws: failed to declare instance %q: %w", id, err)
	}

	// Surfacing the resolved AMI records which image was actually chosen,
	// since the lookup takes the most recent match rather than a pin.
	ctx.Export(id+"-ami", pulumi.String(ami))

	return instance, nil
}

// lookupImage resolves the AMI for an OS in the stack's region.
//
// This is the one Pulumi invoke in the AWS provider. RFC 012 §4.1 went out
// of its way to avoid them, but AMI IDs are region-specific and nothing
// already in the program carries the answer, so here it is unavoidable
// (RFC 013 §2.1).
func lookupImage(ctx *pulumi.Context, os compute.OS, invokeOpts []pulumi.InvokeOption) (string, error) {
	owner, namePattern, err := imageFilter(os)
	if err != nil {
		return "", err
	}

	image, err := ec2.LookupAmi(ctx, &ec2.LookupAmiArgs{
		MostRecent: pulumi.BoolRef(true),
		Owners:     []string{owner},
		Filters: []ec2.GetAmiFilter{
			{Name: "name", Values: []string{namePattern}},
			{Name: "architecture", Values: []string{"x86_64"}},
			{Name: "root-device-type", Values: []string{"ebs"}},
			{Name: "virtualization-type", Values: []string{"hvm"}},
		},
	}, invokeOpts...)
	if err != nil {
		return "", fmt.Errorf("aws: failed to resolve an image for %q: %w", os, err)
	}
	return image.Id, nil
}

// declareInstanceProfile builds the role Session Manager acts through.
func declareInstanceProfile(ctx *pulumi.Context, id string, opts ...pulumi.ResourceOption) (*iam.InstanceProfile, error) {
	trustPolicy, err := buildServiceTrustPolicy("ec2.amazonaws.com")
	if err != nil {
		return nil, err
	}

	role, err := iam.NewRole(ctx, id+"-instance-role", &iam.RoleArgs{
		AssumeRolePolicy: pulumi.String(trustPolicy),
		Description:      pulumi.String(fmt.Sprintf("CloudSDD Session Manager access for %s", id)),
	}, opts...)
	if err != nil {
		return nil, fmt.Errorf("aws: failed to declare instance role for %q: %w", id, err)
	}

	if _, err := iam.NewRolePolicyAttachment(ctx, id+"-ssm", &iam.RolePolicyAttachmentArgs{
		Role:      role.Name,
		PolicyArn: pulumi.String(ssmManagedInstancePolicy),
	}, opts...); err != nil {
		return nil, fmt.Errorf("aws: failed to attach the SSM policy for %q: %w", id, err)
	}

	profile, err := iam.NewInstanceProfile(ctx, id+"-instance-profile", &iam.InstanceProfileArgs{
		Role: role.Name,
	}, opts...)
	if err != nil {
		return nil, fmt.Errorf("aws: failed to declare instance profile for %q: %w", id, err)
	}
	return profile, nil
}
