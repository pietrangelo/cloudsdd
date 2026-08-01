// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package gcp

import (
	"fmt"

	gcpcompute "github.com/pulumi/pulumi-gcp/sdk/v7/go/gcp/compute"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider/compute"
	"cloudsdd/internal/schedule"
)

// defaultNetwork is the VPC new instances attach to. Modelling a VPC of
// our own is a separate resource type; until then the instance is isolated
// by the deny rule in declareComputeFirewall rather than by its network.
const defaultNetwork = "default"

// machineType maps a cloud-agnostic size onto a Compute Engine machine
// type.
func machineType(size compute.Size) (string, error) {
	switch size {
	case compute.SizeSmall:
		return "e2-small", nil
	case compute.SizeMedium:
		return "e2-medium", nil
	case compute.SizeLarge:
		return "e2-standard-2", nil
	default:
		return "", fmt.Errorf("gcp: %w: %q", ErrUnsupportedSize, size)
	}
}

// imageFamily maps a cloud-agnostic OS onto a Compute Engine image family.
// A family always resolves to its latest patched image, which is what a
// freshly created instance should boot.
func imageFamily(os compute.OS) (string, error) {
	switch os {
	case compute.OSUbuntu2204:
		return "ubuntu-os-cloud/ubuntu-2204-lts", nil
	case compute.OSUbuntu2404:
		return "ubuntu-os-cloud/ubuntu-2404-lts-amd64", nil
	case compute.OSDebian12:
		return "debian-cloud/debian-12", nil
	default:
		return "", fmt.Errorf("gcp: %w: %q", ErrUnsupportedOS, os)
	}
}

// decodeComputeInstanceProperties decodes and validates Properties as a
// cloud-agnostic compute.Properties through the shared strict decoder.
func decodeComputeInstanceProperties(props map[string]any) (*compute.Properties, error) {
	var p compute.Properties
	if err := dec.Properties(props, &p); err != nil {
		return nil, err
	}
	if _, err := machineType(p.Size); err != nil {
		return nil, err
	}
	if _, err := imageFamily(p.OS); err != nil {
		return nil, err
	}
	return &p, nil
}

// instanceZone resolves the zone an instance is created in.
//
// Compute Engine instances are zonal, and CloudSDD's Scope carries a
// region. When no zone is pinned, the first zone of the region is used —
// stated here rather than left to the provider's ambient configuration,
// which CloudSDD deliberately does not set.
func instanceZone(region, zone string) string {
	if zone != "" {
		return zone
	}
	return region + "-a"
}

// declareComputeInstance registers the Compute Engine instance, the
// firewall rule that keeps it unreachable, and — when scheduled — the
// native instance schedule policy (RFC 013).
func declareComputeInstance(
	ctx *pulumi.Context,
	id, region, zone string,
	p compute.Properties,
	rules []schedule.Rule,
) (*gcpcompute.Instance, error) {
	machine, err := machineType(p.Size)
	if err != nil {
		return nil, err
	}
	image, err := imageFamily(p.OS)
	if err != nil {
		return nil, err
	}

	networkTag := id + "-cloudsdd"
	if err := declareComputeFirewall(ctx, id, networkTag); err != nil {
		return nil, err
	}

	// A public address is opt-in and, thanks to the deny rule above,
	// still reaches nothing.
	networkInterface := &gcpcompute.InstanceNetworkInterfaceArgs{
		Network: pulumi.String(defaultNetwork),
	}
	if p.EffectivePublicIP() {
		networkInterface.AccessConfigs = gcpcompute.InstanceNetworkInterfaceAccessConfigArray{
			gcpcompute.InstanceNetworkInterfaceAccessConfigArgs{},
		}
	}

	args := &gcpcompute.InstanceArgs{
		MachineType: pulumi.String(machine),
		Zone:        pulumi.String(instanceZone(region, zone)),
		Tags:        pulumi.StringArray{pulumi.String(networkTag)},
		BootDisk: &gcpcompute.InstanceBootDiskArgs{
			InitializeParams: &gcpcompute.InstanceBootDiskInitializeParamsArgs{
				Image: pulumi.String(image),
				Size:  pulumi.Int(p.EffectiveDiskSizeGB()),
			},
		},
		NetworkInterfaces: gcpcompute.InstanceNetworkInterfaceArray{networkInterface},
		// Shielded VM: verified boot, a virtual TPM, and integrity
		// monitoring, so a rootkit in the boot chain is detectable rather
		// than invisible (RFC 013 §2.2).
		ShieldedInstanceConfig: &gcpcompute.InstanceShieldedInstanceConfigArgs{
			EnableSecureBoot:          pulumi.Bool(true),
			EnableVtpm:                pulumi.Bool(true),
			EnableIntegrityMonitoring: pulumi.Bool(true),
		},
		Metadata: pulumi.StringMap{
			// OS Login authenticates shells against IAM and leaves an
			// audit trail, instead of whatever keys happen to sit in
			// project metadata — which the next entry stops being
			// consulted at all.
			"enable-oslogin":         pulumi.String("TRUE"),
			"block-project-ssh-keys": pulumi.String("TRUE"),
			"serial-port-enable":     pulumi.String("FALSE"),
		},
	}

	// No service account block: omitting it attaches no identity at all.
	// The default compute service account, which GCE would otherwise
	// attach with broad scopes, is a standing credential on a machine
	// running arbitrary code (RFC 013 §4).

	if len(rules) > 0 {
		policy, err := declareInstanceSchedule(ctx, id, region, rules)
		if err != nil {
			return nil, err
		}
		args.ResourcePolicies = policy.SelfLink
	}

	instance, err := gcpcompute.NewInstance(ctx, id, args)
	if err != nil {
		return nil, fmt.Errorf("gcp: failed to declare instance %q: %w", id, err)
	}
	return instance, nil
}

// declareComputeFirewall denies every inbound connection to the instance.
//
// This rule is not decoration. GCP's `default` network ships with
// `default-allow-ssh`, which permits port 22 from anywhere, so an instance
// placed there is reachable from the internet the moment it has a public
// address. Firewall rules in GCP are allow-only by default; a deny rule at
// priority 0 is what actually overrides that (RFC 013 §2.2).
func declareComputeFirewall(ctx *pulumi.Context, id, networkTag string) error {
	_, err := gcpcompute.NewFirewall(ctx, id+"-deny-ingress", &gcpcompute.FirewallArgs{
		Network:     pulumi.String(defaultNetwork),
		Description: pulumi.String(fmt.Sprintf("CloudSDD %s: no inbound access", id)),
		Direction:   pulumi.String("INGRESS"),
		Priority:    pulumi.Int(0),
		SourceRanges: pulumi.StringArray{
			pulumi.String("0.0.0.0/0"),
		},
		TargetTags: pulumi.StringArray{pulumi.String(networkTag)},
		Denies: gcpcompute.FirewallDenyArray{
			gcpcompute.FirewallDenyArgs{Protocol: pulumi.String("all")},
		},
	})
	if err != nil {
		return fmt.Errorf("gcp: failed to declare the deny-ingress rule for %q: %w", id, err)
	}
	return nil
}

// declareInstanceSchedule registers the native instance schedule policy
// that powers the VM on and off (RFC 013 §2.5).
//
// Compute Engine schedules instances itself, so unlike Cloud SQL this
// needs no Cloud Scheduler job, no service account and no custom role.
// What it cannot do is exception windows: an instance accepts one such
// policy, carrying one start/stop pair, and resourceSchedule refuses a
// bounded rule set before reaching here.
func declareInstanceSchedule(
	ctx *pulumi.Context,
	id, region string,
	rules []schedule.Rule,
) (*gcpcompute.ResourcePolicy, error) {
	policy := &gcpcompute.ResourcePolicyInstanceSchedulePolicyArgs{
		TimeZone: pulumi.String(rules[0].Timezone),
	}

	for _, rule := range rules {
		expression, err := unixCronExpression(rule)
		if err != nil {
			return nil, err
		}
		switch rule.Action {
		case schedule.ActionStart:
			policy.VmStartSchedule = &gcpcompute.ResourcePolicyInstanceSchedulePolicyVmStartScheduleArgs{
				Schedule: pulumi.String(expression),
			}
		case schedule.ActionStop:
			policy.VmStopSchedule = &gcpcompute.ResourcePolicyInstanceSchedulePolicyVmStopScheduleArgs{
				Schedule: pulumi.String(expression),
			}
		}
	}

	resourcePolicy, err := gcpcompute.NewResourcePolicy(ctx, id+"-power-schedule", &gcpcompute.ResourcePolicyArgs{
		Name:                   pulumi.String(id + "-power-schedule"),
		Region:                 pulumi.String(region),
		InstanceSchedulePolicy: policy,
	})
	if err != nil {
		return nil, fmt.Errorf("gcp: failed to declare the instance schedule for %q: %w", id, err)
	}
	return resourcePolicy, nil
}
