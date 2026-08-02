// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package azure

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/provider/container"
	"cloudsdd/internal/spec"
)

const (
	containerAppToken     = "azure:containerapp/app:App"
	containerEnvToken     = "azure:containerapp/environment:Environment"
	customDomainToken     = "azure:containerapp/customDomain:CustomDomain"
	userAssignedIDToken   = "azure:authorization/userAssignedIdentity:UserAssignedIdentity"
	roleAssignmentToken   = "azure:authorization/assignment:Assignment"
	dnsCNameRecordToken   = "azure:dns/cNameRecord:CNameRecord"
	dnsTxtRecordToken     = "azure:dns/txtRecord:TxtRecord"
	containerAppsSubnetID = "/subscriptions/sub-id/resourceGroups/rg/providers/" +
		"Microsoft.Network/virtualNetworks/vnet/subnets/containerapps"
)

// testContainerImage is digest-pinned, which is what the image rules
// require when no registry is allow-listed.
const testContainerImage = "ghcr.io/acme/api@sha256:" +
	"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func containerProps(mutate func(map[string]any)) map[string]any {
	props := map[string]any{
		"image": testContainerImage,
		"port":  8080,
		"size":  "small",
	}
	if mutate != nil {
		mutate(props)
	}
	return props
}

func containerResource(mutate func(*spec.Resource)) spec.Resource {
	r := spec.Resource{
		ID:         "api",
		Type:       spec.ResourceTypeContainerService,
		Provider:   spec.ProviderAzure,
		Scope:      spec.Scope{Region: "westeurope"},
		Properties: containerProps(nil),
	}
	if mutate != nil {
		mutate(&r)
	}
	return r
}

func containerTestScope() provider.NetworkScope {
	return provider.NetworkScope{
		Provider:    spec.ProviderAzure,
		Environment: "dev",
		Region:      "westeurope",
		Sealed:      true,
	}
}

func declaredContainer(t *testing.T, mutate func(map[string]any)) []recordedResource {
	t.Helper()

	props, err := decodeContainerServiceProperties(containerProps(mutate), nil)
	if err != nil {
		t.Fatalf("decodeContainerServiceProperties() = %v", err)
	}
	net := scopeNetwork{subnetID: containerAppsSubnetID, addressSpace: "10.42.3.0/24"}

	return runProgram(t, func(ctx *pulumi.Context) error {
		_, err := declareContainerService(ctx, "api", "westeurope", containerTestScope(), net, *props)
		return err
	})
}

// TestDeclareContainerServiceIsPrivateByDefault covers RFC 017 §2.3.
//
// Two settings have to agree. The environment's internal load balancer
// decides whether an external endpoint exists at all; the app's ingress
// decides whether this app uses one. Setting only the second would leave
// an environment that a later app could take an external endpoint from by
// accident.
func TestDeclareContainerServiceIsPrivateByDefault(t *testing.T) {
	recorded := declaredContainer(t, nil)

	environment := findResource(t, recorded, containerEnvToken)
	if !environment.Inputs["internalLoadBalancerEnabled"].BoolValue() {
		t.Error("internalLoadBalancerEnabled = false; the environment would have an external endpoint")
	}

	ingress := findResource(t, recorded, containerAppToken).Inputs["ingress"].ObjectValue()
	if ingress["externalEnabled"].BoolValue() {
		t.Error("externalEnabled = true on a private service")
	}
	if hasResource(recorded, customDomainToken) {
		t.Error("a custom domain was bound to a private service")
	}
}

// TestDeclareContainerServiceIngressIsHTTPSOnly covers the §2.3 promise.
//
// AllowInsecureConnections is the line that carries it. Azure's default is
// true, which serves plain HTTP alongside HTTPS rather than redirecting —
// so the secure posture here comes from stating the value, not from
// leaving it alone.
func TestDeclareContainerServiceIngressIsHTTPSOnly(t *testing.T) {
	for _, public := range []bool{false, true} {
		name := "private"
		if public {
			name = "public"
		}
		t.Run(name, func(t *testing.T) {
			recorded := declaredContainer(t, func(p map[string]any) {
				if public {
					p["public"] = true
				}
			})

			ingress := findResource(t, recorded, containerAppToken).Inputs["ingress"].ObjectValue()
			if ingress["allowInsecureConnections"].BoolValue() {
				t.Error("allowInsecureConnections = true; plain HTTP would be served rather than redirected")
			}
			if got := ingress["targetPort"].NumberValue(); got != 8080 {
				t.Errorf("targetPort = %v, want the container port", got)
			}
			if got := ingress["externalEnabled"].BoolValue(); got != public {
				t.Errorf("externalEnabled = %v, want %v", got, public)
			}
		})
	}
}

// TestDeclareContainerServiceIdentityHasNothingAssigned covers RFC 017
// §2.6's identity row.
//
// User-assigned rather than system-assigned, so the absence of permissions
// is a property of a resource a test can point at rather than of one Azure
// generates.
func TestDeclareContainerServiceIdentityHasNothingAssigned(t *testing.T) {
	recorded := declaredContainer(t, nil)

	if !hasResource(recorded, userAssignedIDToken) {
		t.Fatal("no user-assigned identity; the app would run as whatever Azure attaches")
	}
	if hasResource(recorded, roleAssignmentToken) {
		t.Error("a role assignment was declared; the service identity must carry none")
	}

	identity := findResource(t, recorded, containerAppToken).Inputs["identity"].ObjectValue()
	if got := identity["type"].StringValue(); got != identityUserAssigned {
		t.Errorf("identity type = %q, want %q", got, identityUserAssigned)
	}
	if n := len(identity["identityIds"].ArrayValue()); n != 1 {
		t.Errorf("app carries %d identities, want exactly the declared one", n)
	}
}

// TestDeclareContainerServiceJoinsTheScopeNetwork: the environment is
// injected into the scope's delegated subnet, so the app sits beside the
// database it was deployed to talk to and leaves through the scope's NAT.
//
// The workload profile is the non-obvious half. Naming one lowers the
// environment's subnet requirement from a /23 to a /27; the scope's
// subnets are /24s, so a consumption-only environment would not fit the
// address plan at all.
func TestDeclareContainerServiceJoinsTheScopeNetwork(t *testing.T) {
	recorded := declaredContainer(t, nil)

	environment := findResource(t, recorded, containerEnvToken)
	if got := environment.Inputs["infrastructureSubnetId"].StringValue(); got != containerAppsSubnetID {
		t.Errorf("subnet = %q, want the scope's delegated container apps subnet", got)
	}

	profiles := environment.Inputs["workloadProfiles"].ArrayValue()
	if len(profiles) != 1 {
		t.Fatalf("declared %d workload profiles, want 1 — without one the subnet must be a /23", len(profiles))
	}
	if got := profiles[0].ObjectValue()["workloadProfileType"].StringValue(); got != workloadProfileConsumption {
		t.Errorf("workload profile = %q, want %q", got, workloadProfileConsumption)
	}
	if got := findResource(t, recorded, containerAppToken).
		Inputs["workloadProfileName"].StringValue(); got != workloadProfileConsumptionName {
		t.Errorf("app workload profile = %q, want the environment's", got)
	}
}

// TestDeclareContainerServiceReplicas: both bounds are the requested
// count.
//
// A floor below it would mean a Specification asking for three replicas
// usually running one, which is the kind of quiet substitution an
// intent-driven system cannot make.
func TestDeclareContainerServiceReplicas(t *testing.T) {
	tests := []struct {
		name     string
		replicas any
		want     float64
	}{
		{name: "absent defaults to one", replicas: nil, want: 1},
		{name: "explicit", replicas: 3, want: 3},
		{name: "zero is deployed and running nothing", replicas: 0, want: 0},
		{name: "the ceiling", replicas: container.MaxReplicas, want: float64(container.MaxReplicas)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorded := declaredContainer(t, func(p map[string]any) {
				if tt.replicas != nil {
					p["replicas"] = tt.replicas
				}
			})

			template := findResource(t, recorded, containerAppToken).Inputs["template"].ObjectValue()
			if got := template["minReplicas"].NumberValue(); got != tt.want {
				t.Errorf("minReplicas = %v, want %v", got, tt.want)
			}
			if got := template["maxReplicas"].NumberValue(); got != tt.want {
				t.Errorf("maxReplicas = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestDeclareContainerServiceCustomDomain covers RFC 017 §2.3.1 for Azure.
//
// Azure issues the managed certificate only once both records resolve: a
// CNAME pointing the hostname at the app, and a TXT record at
// `asuid.<hostname>` carrying the verification id. Either one missing
// leaves a binding that never completes.
func TestDeclareContainerServiceCustomDomain(t *testing.T) {
	recorded := declaredContainer(t, func(p map[string]any) {
		p["public"] = true
		p["domain"] = "api.acme.example"
	})

	cname := findResource(t, recorded, dnsCNameRecordToken)
	if got := cname.Inputs["name"].StringValue(); got != "api" {
		t.Errorf("cname name = %q, want the record within the zone", got)
	}
	if !strings.Contains(cname.Inputs["record"].StringValue(), "azurecontainerapps.io") {
		t.Errorf("cname target = %q, want the app's ingress FQDN", cname.Inputs["record"].StringValue())
	}

	txt := findResource(t, recorded, dnsTxtRecordToken)
	if got := txt.Inputs["name"].StringValue(); got != domainVerificationPrefix+"api" {
		t.Errorf("verification record = %q, want the asuid-prefixed name Azure requires", got)
	}

	domain := findResource(t, recorded, customDomainToken)
	if got := domain.Inputs["name"].StringValue(); got != "api.acme.example" {
		t.Errorf("custom domain = %q, want the requested one", got)
	}
	// Omitting the certificate id is what asks Azure for a managed
	// certificate; naming one would require the user to have uploaded it.
	if domain.Inputs["containerAppEnvironmentCertificateId"].HasValue() {
		t.Error("a certificate id was named; the managed certificate path omits it")
	}
	if got := domain.Inputs["certificateBindingType"].StringValue(); got != certificateBindingSNI {
		t.Errorf("binding type = %q, want %q — Disabled serves no certificate", got, certificateBindingSNI)
	}
}

// TestSplitDomain covers the zone/record split, including the apex case
// Azure spells "@".
func TestSplitDomain(t *testing.T) {
	tests := []struct {
		domain string
		zone   string
		record string
	}{
		{domain: "api.acme.example", zone: "acme.example", record: "api"},
		{domain: "api.eu.acme.example", zone: "eu.acme.example", record: "api"},
		{domain: "acme.example", zone: "acme.example", record: "@"},
		{domain: "api.acme.example.", zone: "acme.example", record: "api"},
	}

	for _, tt := range tests {
		t.Run(tt.domain, func(t *testing.T) {
			zone, record := splitDomain(tt.domain)
			if zone != tt.zone || record != tt.record {
				t.Errorf("splitDomain(%q) = %q/%q, want %q/%q", tt.domain, zone, record, tt.zone, tt.record)
			}
		})
	}
}

// TestContainerResources: Container Apps requires memory to be twice the
// CPU count in GiB on the consumption profile, so an arbitrary-looking
// pair is an app the API rejects after the user approved the plan.
func TestContainerResources(t *testing.T) {
	tests := []struct {
		size    container.Size
		cpu     float64
		memory  string
		wantErr bool
	}{
		{size: container.SizeSmall, cpu: 0.5, memory: "1Gi"},
		{size: container.SizeMedium, cpu: 1.0, memory: "2Gi"},
		{size: container.SizeLarge, cpu: 2.0, memory: "4Gi"},
		{size: "enormous", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(string(tt.size), func(t *testing.T) {
			cpu, memory, err := containerResources(tt.size)

			if tt.wantErr {
				if !errors.Is(err, ErrUnsupportedContainerSize) {
					t.Fatalf("containerResources(%q) = %v, want ErrUnsupportedContainerSize", tt.size, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("containerResources(%q) = %v", tt.size, err)
			}
			if cpu != tt.cpu || memory != tt.memory {
				t.Errorf("containerResources(%q) = %v/%q, want %v/%q", tt.size, cpu, memory, tt.cpu, tt.memory)
			}
			// The ratio Azure enforces, checked rather than trusted.
			if want := expectedMemory(tt.cpu); memory != want {
				t.Errorf("%s: %s of memory for %v CPU, want %s — Azure requires twice the CPU in GiB",
					tt.size, memory, cpu, want)
			}
		})
	}
}

// expectedMemory is the memory Azure requires for a given CPU count on
// the consumption profile: exactly twice, in GiB.
func expectedMemory(cpu float64) string {
	return strconv.FormatFloat(cpu*2, 'f', -1, 64) + "Gi"
}

// TestDecodeContainerServiceProperties covers the decoding rules and the
// cross-field ingress rule.
func TestDecodeContainerServiceProperties(t *testing.T) {
	tests := []struct {
		name    string
		props   map[string]any
		allowed []string
		wantErr error
		wantMsg string
	}{
		{name: "a digest-pinned private service", props: containerProps(nil)},
		{
			// Optional here, as on GCP and unlike AWS: the built-in
			// *.azurecontainerapps.io endpoint already carries a managed
			// certificate (RFC 017 §2.3.1).
			name:  "public without a domain",
			props: containerProps(func(p map[string]any) { p["public"] = true }),
		},
		{
			name: "public with a domain",
			props: containerProps(func(p map[string]any) {
				p["public"] = true
				p["domain"] = "api.acme.example"
			}),
		},
		{
			name:    "a domain without public",
			props:   containerProps(func(p map[string]any) { p["domain"] = "api.acme.example" }),
			wantErr: container.ErrDomainWithoutPublic,
		},
		{
			name:    "a tag with no allowlist",
			props:   containerProps(func(p map[string]any) { p["image"] = "ghcr.io/acme/api:2.1" }),
			wantErr: container.ErrImageMutable,
		},
		{
			name:    "latest",
			props:   containerProps(func(p map[string]any) { p["image"] = "ghcr.io/acme/api:latest" }),
			allowed: []string{"ghcr.io"},
			wantErr: container.ErrImageLatest,
		},
		{
			name:    "an unmapped size",
			props:   containerProps(func(p map[string]any) { p["size"] = "enormous" }),
			wantMsg: "property validation failed",
		},
		{
			name:    "an unknown property",
			props:   containerProps(func(p map[string]any) { p["cpu_architecture"] = "arm64" }),
			wantMsg: "unknown or malformed property",
		},
		{
			name:    "a credential-shaped property",
			props:   containerProps(func(p map[string]any) { p["password"] = "hunter2" }),
			wantMsg: "looks like a credential",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeContainerServiceProperties(tt.props, tt.allowed)

			if tt.wantErr == nil && tt.wantMsg == "" {
				if err != nil {
					t.Fatalf("decodeContainerServiceProperties() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("decodeContainerServiceProperties() = nil, want an error")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want it to wrap %v", err, tt.wantErr)
			}
			if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
				t.Fatalf("error = %q, want it to contain %q", err, tt.wantMsg)
			}
		})
	}
}

// TestValidateContainerServiceAcceptsAWellFormedOne is the regression test
// for the defect RFC 017 §1 records, and the last provider to close it:
// with this, no ResourceType in the schema is advertised and unimplemented.
func TestValidateContainerServiceAcceptsAWellFormedOne(t *testing.T) {
	p := &AzureProvider{stateDir: t.TempDir(), passphrase: "test"}

	if err := p.Validate(context.Background(), containerResource(nil), spec.Policies{}); err != nil {
		t.Fatalf("Validate() = %v, want a well-formed container_service to be accepted", err)
	}
}

// TestValidateContainerServiceRejectsZones: Container Apps places replicas
// itself across the environment, so a pinned zone is a placement the user
// asked for and will not get.
func TestValidateContainerServiceRejectsZones(t *testing.T) {
	p := &AzureProvider{stateDir: t.TempDir(), passphrase: "test"}

	r := containerResource(func(r *spec.Resource) {
		r.Scope.Zones = []string{"1"}
	})

	if err := p.Validate(context.Background(), r, spec.Policies{}); !errors.Is(err, ErrZonesNotSupported) {
		t.Fatalf("Validate() = %v, want ErrZonesNotSupported", err)
	}
}
