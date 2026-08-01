// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package azure

import (
	"errors"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider/compute"
	"cloudsdd/internal/schedule"
	"cloudsdd/internal/spec"
)

const (
	linuxVMToken    = "azure:compute/linuxVirtualMachine:LinuxVirtualMachine"
	nsgToken        = "azure:network/networkSecurityGroup:NetworkSecurityGroup"
	nicToken        = "azure:network/networkInterface:NetworkInterface"
	publicIPToken   = "azure:network/publicIp:PublicIp"
	privateKeyToken = "tls:index/privateKey:PrivateKey"
)

func defaultVMProperties() compute.Properties {
	return compute.Properties{Size: compute.SizeSmall, OS: compute.OSUbuntu2204}
}

func declareVM(t *testing.T, p compute.Properties, zone string, rules []schedule.Rule) []recordedResource {
	t.Helper()
	return runProgram(t, func(ctx *pulumi.Context) error {
		_, err := declareComputeInstance(ctx, "build-agent", "westeurope", zone, testScope(), p, rules)
		return err
	})
}

func TestDeclareComputeInstanceSecureDefaults(t *testing.T) {
	recorded := declareVM(t, defaultVMProperties(), "", nil)
	vm := findResource(t, recorded, linuxVMToken).Inputs.Mappable()

	// Trusted Launch, the Azure equivalent of Shielded VM. Only available
	// on Gen2 images, which is why the image table pins Gen2 SKUs.
	if vm["secureBootEnabled"] != true {
		t.Errorf("secureBootEnabled = %v, want true", vm["secureBootEnabled"])
	}
	if vm["vtpmEnabled"] != true {
		t.Errorf("vtpmEnabled = %v, want true", vm["vtpmEnabled"])
	}
	// Covers the temp disk and caches, which platform-managed disk
	// encryption alone leaves out.
	if vm["encryptionAtHostEnabled"] != true {
		t.Errorf("encryptionAtHostEnabled = %v, want true", vm["encryptionAtHostEnabled"])
	}
	if vm["disablePasswordAuthentication"] != true {
		t.Errorf("disablePasswordAuthentication = %v, want true", vm["disablePasswordAuthentication"])
	}

	identity, _ := vm["identity"].(map[string]any)
	if identity == nil || identity["type"] != "SystemAssigned" {
		t.Errorf("identity = %v, want a system-assigned identity for AAD login", vm["identity"])
	}

	osDisk, _ := vm["osDisk"].(map[string]any)
	if got := osDisk["diskSizeGb"]; got != 20.0 {
		t.Errorf("diskSizeGb = %v, want the 20GB default", got)
	}

	// No public address by default, so no public IP resource at all.
	if hasResource(recorded, publicIPToken) {
		t.Error("a public IP was declared; the instance must be private by default")
	}
}

func TestDeclareComputeInstanceGeneratesItsOwnKey(t *testing.T) {
	// Azure rejects a Linux VM with neither an admin key nor password
	// auth, so a key must exist. It is generated here rather than asked
	// for, and its private half never leaves the encrypted state.
	recorded := declareVM(t, defaultVMProperties(), "", nil)

	key := findResource(t, recorded, privateKeyToken)
	if got := key.Inputs["algorithm"].StringValue(); got != "ED25519" {
		t.Errorf("algorithm = %q, want ED25519", got)
	}

	vm := findResource(t, recorded, linuxVMToken).Inputs.Mappable()
	keys, _ := vm["adminSshKeys"].([]any)
	if len(keys) != 1 {
		t.Fatalf("adminSshKeys = %v, want exactly the generated one", vm["adminSshKeys"])
	}
	entry, _ := keys[0].(map[string]any)
	pub, _ := entry["publicKey"].(string)
	if !strings.HasPrefix(pub, "ssh-ed25519") {
		t.Errorf("publicKey = %q, want the generated public half", pub)
	}
	// The private half must never reach the VM declaration.
	if strings.Contains(pub, "PRIVATE") {
		t.Error("the private key was passed to the virtual machine")
	}
}

func TestDeclareComputeInstanceUsesTheScopeNetwork(t *testing.T) {
	recorded := declareVM(t, defaultVMProperties(), "", nil)

	// RFC 013 built a VNet, subnet and NSG per virtual machine, because
	// Azure has no default network and there was nothing else to attach
	// to. RFC 016 moves all three to the scope, so the VM declares none
	// of them — asserting their absence is what pins the change.
	for _, token := range []string{
		vnetToken,
		subnetToken,
		nsgToken,
		"azure:network/networkInterfaceSecurityGroupAssociation:NetworkInterfaceSecurityGroupAssociation",
	} {
		if hasResource(recorded, token) {
			t.Errorf("%s was declared per instance; it belongs to the scope network", token)
		}
	}

	// It attaches to the scope's general subnet — not a delegated one,
	// which could not host a VM at all.
	lookup := findResource(t, recorded, getSubnetToken)
	if got := lookup.Inputs["name"].StringValue(); got != generalSubnetName {
		t.Errorf("looked up subnet %q, want the general subnet %q", got, generalSubnetName)
	}

	nic := findResource(t, recorded, nicToken)
	configs := nic.Inputs["ipConfigurations"].ArrayValue()
	if len(configs) != 1 {
		t.Fatalf("got %d ip configurations, want 1", len(configs))
	}
	if got := configs[0].ObjectValue()["subnetId"].StringValue(); got != testSubnetID {
		t.Errorf("subnetId = %q, want the looked-up subnet", got)
	}

	// The perimeter still exists; it is now the subnet's, which means an
	// instance cannot end up outside it by forgetting to attach one.
	if configs[0].ObjectValue()["publicIpAddressId"].IsNull() != true {
		if _, hasPublic := configs[0].ObjectValue()["publicIpAddressId"]; hasPublic {
			t.Error("a public address was attached without being asked for")
		}
	}
}

func TestDeclareComputeInstanceMapping(t *testing.T) {
	tests := []struct {
		name          string
		props         compute.Properties
		zone          string
		wantSize      string
		wantPublisher string
		wantSku       string
	}{
		{
			name:          "small ubuntu 22.04",
			props:         compute.Properties{Size: compute.SizeSmall, OS: compute.OSUbuntu2204},
			wantSize:      "Standard_B1ms",
			wantPublisher: "Canonical",
			wantSku:       "22_04-lts-gen2",
		},
		{
			name:          "medium debian",
			props:         compute.Properties{Size: compute.SizeMedium, OS: compute.OSDebian12},
			zone:          "2",
			wantSize:      "Standard_B2s",
			wantPublisher: "Debian",
			wantSku:       "12-gen2",
		},
		{
			name:          "large ubuntu 24.04",
			props:         compute.Properties{Size: compute.SizeLarge, OS: compute.OSUbuntu2404},
			wantSize:      "Standard_B2ms",
			wantPublisher: "Canonical",
			wantSku:       "server-gen1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorded := declareVM(t, tt.props, tt.zone, nil)
			vm := findResource(t, recorded, linuxVMToken).Inputs.Mappable()

			if got := vm["size"]; got != tt.wantSize {
				t.Errorf("size = %v, want %q", got, tt.wantSize)
			}
			image, _ := vm["sourceImageReference"].(map[string]any)
			if got := image["publisher"]; got != tt.wantPublisher {
				t.Errorf("publisher = %v, want %q", got, tt.wantPublisher)
			}
			if got := image["sku"]; got != tt.wantSku {
				t.Errorf("sku = %v, want %q", got, tt.wantSku)
			}
			if got := image["version"]; got != "latest" {
				t.Errorf("version = %v, want latest", got)
			}

			if tt.zone == "" {
				if _, ok := vm["zone"]; ok {
					t.Errorf("zone = %v, want none pinned", vm["zone"])
				}
			} else if got := vm["zone"]; got != tt.zone {
				t.Errorf("zone = %v, want %q", got, tt.zone)
			}
		})
	}
}

func TestDeclareComputeInstanceExplicitPublicIP(t *testing.T) {
	yes := true
	p := defaultVMProperties()
	p.PublicIP = &yes

	recorded := declareVM(t, p, "", nil)
	if !hasResource(recorded, publicIPToken) {
		t.Error("no public IP was declared though one was requested")
	}

	// A public address must not imply an open door. Since RFC 016 the
	// perimeter belongs to the scope's subnet, so what this test can
	// assert is that asking for a public address does not make the
	// instance declare a network of its own with rules of its own —
	// which would sidestep the scope's deny-all entirely.
	for _, token := range []string{vnetToken, subnetToken, nsgToken} {
		if hasResource(recorded, token) {
			t.Errorf("a public instance declared %s, escaping the scope network's perimeter", token)
		}
	}
	if got := findResource(t, recorded, getSubnetToken).Inputs["name"].StringValue(); got != generalSubnetName {
		t.Errorf("a public instance attached to subnet %q, want the scope's %q", got, generalSubnetName)
	}
}

// TestComputeScheduleDeallocates is the load-bearing test of RFC 013 §2.5:
// an Azure VM in the Stopped state still bills for compute, so a schedule
// wired to `stop` would run correctly and save nothing.
func TestComputeScheduleDeallocates(t *testing.T) {
	freezeClock(t)

	rules, err := schedule.Compile(workWeekSchedule())
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}

	recorded := declareVM(t, defaultVMProperties(), "", rules)

	actions := map[string]bool{}
	for _, job := range resourcesOfType(recorded, jobScheduleToken) {
		params := job.Inputs["parameters"].ObjectValue()
		actions[params["action"].StringValue()] = true
		if got := params["apiversion"].StringValue(); got != virtualMachineAPIVersion {
			t.Errorf("apiversion = %q, want the pinned VM version %q", got, virtualMachineAPIVersion)
		}
	}

	if actions["stop"] {
		t.Error("the stop rule invokes `stop`, which leaves the VM allocated and still billing")
	}
	if !actions["deallocate"] {
		t.Errorf("actions = %v, want the stop rule to deallocate", actions)
	}
	if !actions["start"] {
		t.Errorf("actions = %v, want a start action", actions)
	}
}

func TestComputePowerTargetGrantsOnlyPowerActions(t *testing.T) {
	freezeClock(t)

	rules, err := schedule.Compile(workWeekSchedule())
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	recorded := declareVM(t, defaultVMProperties(), "", rules)

	role := findResource(t, recorded, roleDefinitionToken)
	perms := role.Inputs["permissions"].ArrayValue()
	actions := perms[0].ObjectValue()["actions"].ArrayValue()
	if len(actions) != 3 {
		t.Fatalf("role grants %d actions, want read, start and deallocate only", len(actions))
	}
	for _, a := range actions {
		got := a.StringValue()
		if !strings.HasPrefix(got, "Microsoft.Compute/virtualMachines/") {
			t.Errorf("action %q is not scoped to virtual machines", got)
		}
		if strings.Contains(got, "*") {
			t.Errorf("action %q contains a wildcard", got)
		}
	}
	// Scoped to this VM, not the resource group or the subscription.
	if got := role.Inputs["scope"].StringValue(); got != "build-agent-id" {
		t.Errorf("role scope = %q, want the machine alone", got)
	}
}

func TestDeclareComputeInstanceUnscheduledHasNoAutomation(t *testing.T) {
	recorded := declareVM(t, defaultVMProperties(), "", nil)
	for _, token := range []string{automationAccountToken, automationScheduleToken, runbookToken, roleDefinitionToken} {
		if hasResource(recorded, token) {
			t.Errorf("%s was declared for an unscheduled instance", token)
		}
	}
}

func TestComputeMappingRejectsUnknownValues(t *testing.T) {
	if _, err := vmSize("gigantic"); !errors.Is(err, ErrUnsupportedSize) {
		t.Errorf("vmSize() error = %v, want %v", err, ErrUnsupportedSize)
	}
	if _, _, _, err := imageReference("plan9"); !errors.Is(err, ErrUnsupportedOS) {
		t.Errorf("imageReference() error = %v, want %v", err, ErrUnsupportedOS)
	}

	// Reachable from Destroy, which does not validate first — the shape
	// that made this provider panic in RFC 011 §1.1B1.
	for _, p := range []compute.Properties{
		{Size: "gigantic", OS: compute.OSUbuntu2204},
		{Size: compute.SizeSmall, OS: "plan9"},
	} {
		var declareErr error
		_ = runProgram(t, func(ctx *pulumi.Context) error {
			_, declareErr = declareComputeInstance(ctx, "vm", "westeurope", "", testScope(), p, nil)
			return nil
		})
		if declareErr == nil {
			t.Errorf("declareComputeInstance(%+v) = nil error, want a rejection", p)
		}
	}
}

func TestDecodeComputeInstanceProperties(t *testing.T) {
	tests := []struct {
		name    string
		props   map[string]any
		wantErr string
	}{
		{name: "valid", props: map[string]any{"size": "large", "os": "ubuntu-24.04"}},
		{
			name:    "unmapped size",
			props:   map[string]any{"size": "gigantic", "os": "ubuntu-22.04"},
			wantErr: "property validation failed",
		},
		{
			name:    "unknown property is rejected rather than dropped",
			props:   map[string]any{"size": "small", "os": "ubuntu-22.04", "vm_size": "Standard_D2s_v5"},
			wantErr: "unknown or malformed property",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeComputeInstanceProperties(tt.props)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("decode error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("decode error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestResourceProgramComputeInstance(t *testing.T) {
	p := &AzureProvider{stateDir: t.TempDir(), passphrase: "test"}

	vm := func(mutate func(*spec.Resource)) spec.Resource {
		r := spec.Resource{
			ID: "build-agent", Type: spec.ResourceTypeComputeInstance, Provider: spec.ProviderAzure,
			Scope:      spec.Scope{Region: "westeurope"},
			Properties: map[string]any{"size": "small", "os": "ubuntu-22.04"},
		}
		if mutate != nil {
			mutate(&r)
		}
		return r
	}

	t.Run("builds a program", func(t *testing.T) {
		program, region, err := p.resourceProgram(vm(nil), spec.Policies{})
		if err != nil {
			t.Fatalf("resourceProgram() error = %v", err)
		}
		if region != "westeurope" || program == nil {
			t.Fatalf("resourceProgram() = (%v, %q), want a program for westeurope", program != nil, region)
		}
	})

	t.Run("two zones are refused", func(t *testing.T) {
		_, _, err := p.resourceProgram(vm(func(r *spec.Resource) {
			r.Scope.Zones = []string{"1", "2"}
		}), spec.Policies{})
		if !errors.Is(err, compute.ErrMultipleZones) {
			t.Fatalf("resourceProgram() error = %v, want %v", err, compute.ErrMultipleZones)
		}
	})

	t.Run("decode errors propagate", func(t *testing.T) {
		program, _, err := p.resourceProgram(vm(func(r *spec.Resource) {
			r.Properties = map[string]any{}
		}), spec.Policies{})
		if err == nil {
			t.Fatal("resourceProgram() error = nil, want a decode error")
		}
		if program != nil {
			t.Error("resourceProgram() returned a program alongside an error")
		}
	})
}

func TestArmAction(t *testing.T) {
	tests := []struct {
		name       string
		action     schedule.Action
		stopAction string
		want       string
	}{
		{name: "start is universal", action: schedule.ActionStart, stopAction: "deallocate", want: "start"},
		{name: "database stops", action: schedule.ActionStop, stopAction: "stop", want: "stop"},
		{name: "machine deallocates", action: schedule.ActionStop, stopAction: "deallocate", want: "deallocate"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := armAction(tt.action, tt.stopAction); got != tt.want {
				t.Errorf("armAction() = %q, want %q", got, tt.want)
			}
		})
	}
}
