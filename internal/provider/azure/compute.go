// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package azure

import (
	"fmt"

	azcompute "github.com/pulumi/pulumi-azure/sdk/v5/go/azure/compute"
	"github.com/pulumi/pulumi-azure/sdk/v5/go/azure/core"
	"github.com/pulumi/pulumi-azure/sdk/v5/go/azure/network"
	"github.com/pulumi/pulumi-tls/sdk/v5/go/tls"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider/compute"
	"cloudsdd/internal/schedule"
)

// adminUsername is the local account Azure requires on a Linux VM. It is
// never used to log in: interactive access goes through the AAD login
// extension, which authenticates against Entra ID (RFC 013 §2.4).
const adminUsername = "cloudsdd"

// Address space for a compute instance's virtual network. Named rather
// than inlined so that RFC 015 §2.2's requirement — that the database
// VNet not overlap this one — is checkable by a test instead of by
// somebody remembering.
const (
	computeVNetAddressSpace   = "10.0.0.0/16"
	computeSubnetAddressSpace = "10.0.1.0/24"
)

// vmSize maps a cloud-agnostic size onto an Azure VM size.
func vmSize(size compute.Size) (string, error) {
	switch size {
	case compute.SizeSmall:
		return "Standard_B1ms", nil
	case compute.SizeMedium:
		return "Standard_B2s", nil
	case compute.SizeLarge:
		return "Standard_B2ms", nil
	default:
		return "", fmt.Errorf("azure: %w: %q", ErrUnsupportedSize, size)
	}
}

// imageReference maps a cloud-agnostic OS onto an Azure marketplace image.
//
// Generation 2 images throughout: Trusted Launch — secure boot and vTPM —
// is only available on Gen2, so a Gen1 SKU here would silently disable the
// boot-integrity guarantees below.
func imageReference(os compute.OS) (publisher, offer, sku string, err error) {
	switch os {
	case compute.OSUbuntu2204:
		return "Canonical", "0001-com-ubuntu-server-jammy", "22_04-lts-gen2", nil
	case compute.OSUbuntu2404:
		return "Canonical", "ubuntu-24_04-lts", "server-gen1", nil
	case compute.OSDebian12:
		return "Debian", "debian-12", "12-gen2", nil
	default:
		return "", "", "", fmt.Errorf("azure: %w: %q", ErrUnsupportedOS, os)
	}
}

// decodeComputeInstanceProperties decodes and validates Properties as a
// cloud-agnostic compute.Properties through the shared strict decoder.
func decodeComputeInstanceProperties(props map[string]any) (*compute.Properties, error) {
	var p compute.Properties
	if err := dec.Properties(props, &p); err != nil {
		return nil, err
	}
	if _, err := vmSize(p.Size); err != nil {
		return nil, err
	}
	if _, _, _, err := imageReference(p.OS); err != nil {
		return nil, err
	}
	return &p, nil
}

// virtualMachine is what a declared VM exposes to the resources built on
// top of it.
type virtualMachine struct {
	id            pulumi.IDOutput
	resourceGroup *core.ResourceGroup
}

// powerTarget describes the VM for the scheduler.
//
// The stop verb is `deallocate`, not `stop`: an Azure VM in the `Stopped`
// state still bills for compute, and only deallocation releases the
// hardware (RFC 013 §2.5).
func (v virtualMachine) powerTarget() powerTarget {
	return powerTarget{
		id:            v.id,
		resourceGroup: v.resourceGroup,
		apiVersion:    virtualMachineAPIVersion,
		actions: []string{
			"Microsoft.Compute/virtualMachines/read",
			"Microsoft.Compute/virtualMachines/start/action",
			"Microsoft.Compute/virtualMachines/deallocate/action",
		},
		stopAction: "deallocate",
	}
}

// declareComputeInstance registers the Linux VM together with the network
// it lives in and the key pair Azure insists on (RFC 013).
func declareComputeInstance(
	ctx *pulumi.Context,
	id, location, zone string,
	p compute.Properties,
	rules []schedule.Rule,
) (virtualMachine, error) {
	size, err := vmSize(p.Size)
	if err != nil {
		return virtualMachine{}, err
	}
	publisher, offer, sku, err := imageReference(p.OS)
	if err != nil {
		return virtualMachine{}, err
	}

	rg, err := core.NewResourceGroup(ctx, id+"-rg", &core.ResourceGroupArgs{
		Location: pulumi.String(location),
	})
	if err != nil {
		return virtualMachine{}, fmt.Errorf("azure: failed to declare resource group for %q: %w", id, err)
	}

	nic, err := declareComputeNetwork(ctx, id, rg, p)
	if err != nil {
		return virtualMachine{}, err
	}

	// Azure rejects a Linux VM with neither an admin SSH key nor password
	// authentication, so a key pair has to exist. It is generated here and
	// its private half stays in the passphrase-encrypted Pulumi state —
	// the same shape as the RDS master password (RFC 007) — rather than
	// being asked for in the Specification, which RFC 001 §3 treats as
	// hostile input. Nothing logs in with it (RFC 013 §2.4).
	key, err := tls.NewPrivateKey(ctx, id+"-key", &tls.PrivateKeyArgs{
		Algorithm: pulumi.String("ED25519"),
	})
	if err != nil {
		return virtualMachine{}, fmt.Errorf("azure: failed to generate the host key for %q: %w", id, err)
	}

	args := &azcompute.LinuxVirtualMachineArgs{
		ResourceGroupName:   rg.Name,
		Location:            rg.Location,
		Size:                pulumi.String(size),
		AdminUsername:       pulumi.String(adminUsername),
		NetworkInterfaceIds: pulumi.StringArray{nic.ID()},
		AdminSshKeys: azcompute.LinuxVirtualMachineAdminSshKeyArray{
			azcompute.LinuxVirtualMachineAdminSshKeyArgs{
				Username:  pulumi.String(adminUsername),
				PublicKey: key.PublicKeyOpenssh,
			},
		},
		// Explicit rather than relying on the default, because it is the
		// property that keeps a guessed password from being a way in.
		DisablePasswordAuthentication: pulumi.Bool(true),
		SourceImageReference: &azcompute.LinuxVirtualMachineSourceImageReferenceArgs{
			Publisher: pulumi.String(publisher),
			Offer:     pulumi.String(offer),
			Sku:       pulumi.String(sku),
			Version:   pulumi.String("latest"),
		},
		OsDisk: &azcompute.LinuxVirtualMachineOsDiskArgs{
			Caching:            pulumi.String("ReadWrite"),
			StorageAccountType: pulumi.String("StandardSSD_LRS"),
			DiskSizeGb:         pulumi.Int(p.EffectiveDiskSizeGB()),
		},
		// Trusted Launch: verified boot and a virtual TPM, the Azure
		// equivalent of GCP's Shielded VM (RFC 013 §2.2).
		SecureBootEnabled: pulumi.Bool(true),
		VtpmEnabled:       pulumi.Bool(true),
		// Encryption at host covers the temp disk and the caches, which
		// platform-managed disk encryption alone leaves out. It requires
		// the Microsoft.Compute/EncryptionAtHost feature to be registered
		// on the subscription; when it is not, the deploy fails loudly
		// rather than quietly provisioning less protection.
		EncryptionAtHostEnabled: pulumi.Bool(true),
		// A managed identity is what the AAD login extension
		// authenticates against.
		Identity: &azcompute.LinuxVirtualMachineIdentityArgs{
			Type: pulumi.String("SystemAssigned"),
		},
	}
	if zone != "" {
		args.Zone = pulumi.String(zone)
	}

	vm, err := azcompute.NewLinuxVirtualMachine(ctx, id, args)
	if err != nil {
		return virtualMachine{}, fmt.Errorf("azure: failed to declare virtual machine %q: %w", id, err)
	}

	machine := virtualMachine{id: vm.ID(), resourceGroup: rg}

	if err := declarePowerSchedule(ctx, id, machine.powerTarget(), rules); err != nil {
		return virtualMachine{}, err
	}
	return machine, nil
}

// declareComputeNetwork builds the network the VM attaches to: a VNet, a
// subnet, a network security group with no inbound allow rules, and the
// interface itself.
//
// Azure has no default network, so unlike AWS and GCP this is not
// optional. The upside is that the perimeter is entirely ours: an NSG
// with no custom rules still carries Azure's DenyAllInBound default, and
// nothing has been added to open it.
func declareComputeNetwork(
	ctx *pulumi.Context,
	id string,
	rg *core.ResourceGroup,
	p compute.Properties,
) (*network.NetworkInterface, error) {
	vnet, err := network.NewVirtualNetwork(ctx, id+"-vnet", &network.VirtualNetworkArgs{
		ResourceGroupName: rg.Name,
		Location:          rg.Location,
		AddressSpaces:     pulumi.StringArray{pulumi.String(computeVNetAddressSpace)},
	})
	if err != nil {
		return nil, fmt.Errorf("azure: failed to declare the network for %q: %w", id, err)
	}

	subnet, err := network.NewSubnet(ctx, id+"-subnet", &network.SubnetArgs{
		ResourceGroupName:  rg.Name,
		VirtualNetworkName: vnet.Name,
		AddressPrefixes:    pulumi.StringArray{pulumi.String(computeSubnetAddressSpace)},
	})
	if err != nil {
		return nil, fmt.Errorf("azure: failed to declare the subnet for %q: %w", id, err)
	}

	// No security rules at all: the NSG's own defaults deny inbound from
	// the internet, and nothing here opens a hole in them.
	nsg, err := network.NewNetworkSecurityGroup(ctx, id+"-nsg", &network.NetworkSecurityGroupArgs{
		ResourceGroupName: rg.Name,
		Location:          rg.Location,
	})
	if err != nil {
		return nil, fmt.Errorf("azure: failed to declare the security group for %q: %w", id, err)
	}

	ipConfig := &network.NetworkInterfaceIpConfigurationArgs{
		Name:                       pulumi.String("internal"),
		SubnetId:                   subnet.ID(),
		PrivateIpAddressAllocation: pulumi.String("Dynamic"),
	}
	if p.EffectivePublicIP() {
		publicIP, err := network.NewPublicIp(ctx, id+"-ip", &network.PublicIpArgs{
			ResourceGroupName: rg.Name,
			Location:          rg.Location,
			AllocationMethod:  pulumi.String("Static"),
		})
		if err != nil {
			return nil, fmt.Errorf("azure: failed to declare the public address for %q: %w", id, err)
		}
		ipConfig.PublicIpAddressId = publicIP.ID()
	}

	nic, err := network.NewNetworkInterface(ctx, id+"-nic", &network.NetworkInterfaceArgs{
		ResourceGroupName: rg.Name,
		Location:          rg.Location,
		IpConfigurations:  network.NetworkInterfaceIpConfigurationArray{ipConfig},
	})
	if err != nil {
		return nil, fmt.Errorf("azure: failed to declare the network interface for %q: %w", id, err)
	}

	if _, err := network.NewNetworkInterfaceSecurityGroupAssociation(ctx, id+"-nsg-assoc",
		&network.NetworkInterfaceSecurityGroupAssociationArgs{
			NetworkInterfaceId:     nic.ID(),
			NetworkSecurityGroupId: nsg.ID(),
		}); err != nil {
		return nil, fmt.Errorf("azure: failed to attach the security group for %q: %w", id, err)
	}

	return nic, nil
}
