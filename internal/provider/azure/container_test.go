// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package azure

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/provider/container"
	"cloudsdd/internal/provider/pipeline"
	"cloudsdd/internal/schedule"
	"cloudsdd/internal/spec"
)

const (
	containerAppToken     = "azure:containerapp/app:App"
	containerEnvToken     = "azure:containerapp/environment:Environment"
	customDomainToken     = "azure:containerapp/customDomain:CustomDomain"
	userAssignedIDToken   = "azure:authorization/userAssignedIdentity:UserAssignedIdentity"
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

	return declaredContainerFor(t, containerResource(func(r *spec.Resource) {
		r.Properties = containerProps(mutate)
	}), nil)
}

// declaredContainerFor declares one specific resource, which is what the
// pipeline path needs: the image reference is assembled from `Resolved`,
// and that lives on the resource rather than in its properties.
func declaredContainerFor(t *testing.T, r spec.Resource, rules []schedule.Rule) []recordedResource {
	t.Helper()

	props, err := decodeContainerServiceProperties(r.Properties, nil)
	if err != nil {
		t.Fatalf("decodeContainerServiceProperties() = %v", err)
	}
	net := scopeNetwork{subnetID: containerAppsSubnetID, addressSpace: "10.42.3.0/24"}

	return runProgram(t, func(ctx *pulumi.Context) error {
		_, err := declareContainerService(ctx, r, containerTestScope(), net, *props, rules)
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
	if hasResource(recorded, assignmentToken) {
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

// TestDeclareContainerServiceRunsThePipelineImage is the service half of
// RFC 018 §2.4.1: a service that names a `pipeline` instead of an `image`
// runs the Container Registry reference the resolved commit named.
//
// Without this the decode succeeds — `image` is optional since RFC 018 §3
// — and Container Apps is handed an empty image, which is a service that
// validates, plans, and then fails on apply for a reason the plan never
// showed.
func TestDeclareContainerServiceRunsThePipelineImage(t *testing.T) {
	r := containerResource(func(r *spec.Resource) {
		r.Properties = containerProps(func(p map[string]any) {
			delete(p, "image")
			p["pipeline"] = "api-build"
		})
		r.Resolved = testResolved
	})

	recorded := declaredContainerFor(t, r, nil)

	got := containerImage(t, findResource(t, recorded, containerAppToken))
	want := acrImageReference(testACRLoginServer, testResolved.ImageName, testResolved.Commit)
	if got != want {
		t.Errorf("container image = %q, want the pipeline's own %q", got, want)
	}
	// The commit, never a branch and never `latest`. ACR has no
	// registry-wide immutable-tags setting (RFC 018 §2.8.1), so this is a
	// weaker promise here than on the other two clouds — which is exactly
	// why the tag must at least be the one the plan displayed.
	if !strings.HasSuffix(got, ":"+testCommit) {
		t.Errorf("container image = %q, want it tagged with the resolved commit", got)
	}

	// The login server comes from the registry rather than from the name,
	// so the lookup must actually happen and must name the registry the
	// pipeline created — both derived from the image name, which is all the
	// Engine hands across.
	lookup := findResource(t, recorded, getAcrRegistryToken)
	if got, want := lookup.Inputs["name"].StringValue(), acrRegistryName(testSubscriptionID, testResolved.ImageName); got != want {
		t.Errorf("looked up registry %q, want the derived %q", got, want)
	}
	if got, want := lookup.Inputs["resourceGroupName"].StringValue(), acrResourceGroupName(testResolved.ImageName); got != want {
		t.Errorf("looked up resource group %q, want the derived %q", got, want)
	}
}

// TestDeclareContainerServiceWithAnImageAsksTheRegistryNothing: the
// pipeline path is entered only by a service that has no image of its own.
// A lookup on every service would fail every deployment that names a
// public image, since there is no registry to find.
func TestDeclareContainerServiceWithAnImageAsksTheRegistryNothing(t *testing.T) {
	recorded := declaredContainer(t, nil)

	if hasResource(recorded, getAcrRegistryToken) {
		t.Error("the registry was queried for a service that named its own image")
	}
	if got := containerImage(t, findResource(t, recorded, containerAppToken)); got != testContainerImage {
		t.Errorf("container image = %q, want the requested %q", got, testContainerImage)
	}
}

// TestDeclareContainerServiceRefusesAnUnresolvedPipeline: reaching the
// provider without the Engine's answer is refused rather than guessed at.
// The two plausible inventions — the pipeline's resource id, or `latest` —
// are respectively wrong and the one thing RFC 017 §2.4 refuses outright.
func TestDeclareContainerServiceRefusesAnUnresolvedPipeline(t *testing.T) {
	r := containerResource(func(r *spec.Resource) {
		r.Properties = containerProps(func(p map[string]any) {
			delete(p, "image")
			p["pipeline"] = "api-build"
		})
	})

	props, err := decodeContainerServiceProperties(r.Properties, nil)
	if err != nil {
		t.Fatalf("decodeContainerServiceProperties() = %v", err)
	}
	net := scopeNetwork{subnetID: containerAppsSubnetID, addressSpace: "10.42.3.0/24"}

	var got error
	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		_, got = declareContainerService(ctx, r, containerTestScope(), net, *props, nil)
		return nil
	})

	if !errors.Is(got, pipeline.ErrNotResolved) {
		t.Fatalf("declareContainerService() = %v, want %v", got, pipeline.ErrNotResolved)
	}
	// The refusal has to come before anything is registered, or the stack
	// holds an app pointed at nothing — and, on Azure, a resource group and
	// an environment around it.
	if hasResource(recorded, containerAppToken) {
		t.Error("a container app was declared despite the unresolved pipeline reference")
	}
	if hasResource(recorded, rgToken) {
		t.Error("a resource group was declared despite the unresolved pipeline reference")
	}
}

// containerImage reads the image off the single container of a declared
// Container Apps app.
func containerImage(t *testing.T, app recordedResource) string {
	t.Helper()

	containers := app.Inputs["template"].ObjectValue()["containers"].ArrayValue()
	if len(containers) != 1 {
		t.Fatalf("declared %d containers, want 1", len(containers))
	}
	return containers[0].ObjectValue()["image"].StringValue()
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

// declaredScheduledContainer runs a program declaring a container service
// and its power schedule.
func declaredScheduledContainer(t *testing.T) []recordedResource {
	t.Helper()

	rules, err := schedule.Compile(workWeekSchedule())
	if err != nil {
		t.Fatalf("Compile() = %v", err)
	}
	return declaredContainerFor(t, containerResource(nil), rules)
}

// TestDeclareContainerSchedule covers RFC 017 §2.5 on Azure.
//
// §2.5 proposed expressing "off" as min and max replicas both zero, which
// would have needed a PATCH with a body — something the runbook
// deliberately cannot do, since it POSTs an action and interpolates
// nothing from the Specification. Container Apps has `start` and `stop`
// actions of its own, so the same mechanism the databases use expresses
// the same intent.
func TestDeclareContainerSchedule(t *testing.T) {
	recorded := declaredScheduledContainer(t)

	schedules := resourcesOfType(recorded, automationScheduleToken)
	if len(schedules) != 2 {
		t.Fatalf("declared %d schedules, want 2 (one start, one stop)", len(schedules))
	}
	if n := len(resourcesOfType(recorded, jobScheduleToken)); n != 2 {
		t.Errorf("declared %d job schedules, want one per schedule — an unbound schedule runs nothing", n)
	}

	// The action reaches the runbook as a parameter, never as script text
	// (RFC 012 §4.3). Both verbs must appear, or the app is powered one
	// way and never the other.
	actions := map[string]bool{}
	for _, js := range resourcesOfType(recorded, jobScheduleToken) {
		params := js.Inputs["parameters"].ObjectValue()
		actions[params["action"].StringValue()] = true
		if got := params["apiversion"].StringValue(); got != containerAppAPIVersion {
			t.Errorf("api version = %q, want the pinned %q", got, containerAppAPIVersion)
		}
	}
	for _, want := range []string{"start", "stop"} {
		if !actions[want] {
			t.Errorf("no schedule invokes %q; got %v", want, actions)
		}
	}

	// The role the schedule acts through may power this one app and do
	// nothing else (RFC 012 §7).
	role := findResource(t, recorded, roleDefinitionToken)
	permissions := role.Inputs["permissions"].ArrayValue()
	if len(permissions) != 1 {
		t.Fatalf("role has %d permission blocks, want 1", len(permissions))
	}
	granted := map[string]bool{}
	for _, a := range permissions[0].ObjectValue()["actions"].ArrayValue() {
		granted[a.StringValue()] = true
	}
	for _, want := range []string{
		"Microsoft.App/containerApps/read",
		"Microsoft.App/containerApps/start/action",
		"Microsoft.App/containerApps/stop/action",
	} {
		if !granted[want] {
			t.Errorf("role does not grant %q; got %v", want, granted)
		}
	}
	if granted["*"] {
		t.Error("the schedule role grants *, want the three container app actions")
	}
}

// TestDeclareContainerServiceWithoutScheduleDeclaresNoScheduleResources:
// an unscheduled service carries no automation machinery at all.
func TestDeclareContainerServiceWithoutScheduleDeclaresNoScheduleResources(t *testing.T) {
	recorded := declaredContainer(t, nil)

	for _, token := range []string{automationAccountToken, automationScheduleToken, runbookToken} {
		if hasResource(recorded, token) {
			t.Errorf("%s was declared for an unscheduled service", token)
		}
	}
}

const (
	environmentStorageToken = "azure:containerapp/environmentStorage:EnvironmentStorage"
	roleAssignmentToken     = "azure:authorization/assignment:Assignment"
)

// mountingContainerResource is a container service in containerTestScope()
// that mounts volumes, so the account and share names it derives are the
// ones the network stack declared.
func mountingContainerResource(volumes []spec.Volume) spec.Resource {
	return containerResource(func(r *spec.Resource) {
		r.Scope.Environment = containerTestScope().Environment
		r.Volumes = volumes
	})
}

// TestDeclareContainerServiceMountsItsVolumes covers the Azure row of RFC
// 020 §2.8's mount table: environment storage, a template volume naming
// it, and a mount of that volume.
//
// Two volumes rather than one, because every assertion here is about
// *which* share a mount path reaches, joined through two names the user
// never sees. A single volume would pass just as well against an
// implementation that wired every mount to the first share — §1's two
// names resolving to one filesystem. One carries uppercase and an
// underscore, which the Specification admits and Azure does not.
func TestDeclareContainerServiceMountsItsVolumes(t *testing.T) {
	volumes := []spec.Volume{
		{Name: "uploads", MountPath: "/var/lib/uploads"},
		{Name: "Shared_Cache", MountPath: "/var/cache/app"},
	}
	recorded := declaredContainerFor(t, mountingContainerResource(volumes), nil)

	// Lookup, not create. An account or share declared here would be a
	// second one beside the scope's, reachable from the internet unless it
	// repeated every line of the network stack's hardening.
	for _, token := range []string{storageAccountToken, fileShareToken, privateEndpointToken} {
		if hasResource(recorded, token) {
			t.Errorf("the resource stack declared a %s; the scope's network stack owns it", token)
		}
	}

	account := assertScopeAccountLookedUp(t, recorded)

	// Every share is sought by the name the network stack gave it, in the
	// scope's account.
	shareOf := make(map[string]string, len(volumes))
	for _, v := range volumes {
		shareOf[v.MountPath] = fileShareNameFor(v.Name)
	}
	sought := map[string]bool{}
	for _, l := range resourcesOfType(recorded, getFileShareToken) {
		sought[stringOrEmpty(l.Inputs["name"])] = true
		if got := stringOrEmpty(l.Inputs["storageAccountName"]); got != account {
			t.Errorf("share %q was looked up in account %q, want the scope's %q",
				stringOrEmpty(l.Inputs["name"]), got, account)
		}
	}
	for _, v := range volumes {
		if !sought[fileShareNameFor(v.Name)] {
			t.Errorf("volume %q was not looked up by its share name %q; names sought = %v",
				v.Name, fileShareNameFor(v.Name), sought)
		}
	}

	environment := findResource(t, recorded, containerEnvToken)
	shareByStorage := assertEnvironmentStorage(t, recorded, environment, account)

	template := objectOrEmpty(findResource(t, recorded, containerAppToken).Inputs["template"])
	assertAzureFileMounts(t, template, shareByStorage, shareOf)

	// Azure Files is mounted with the account key the storage link holds,
	// not with the app's identity. RFC 017 §2.6's empty identity therefore
	// stays empty: a role granted here would be a second, standing path to
	// the same data.
	if hasResource(recorded, roleAssignmentToken) {
		t.Error("a role assignment was declared for a mount; the storage link authenticates with the account key")
	}
}

// assertScopeAccountLookedUp checks that the scope's one storage account
// was looked up, once, by the names the network stack derived, and returns
// the account name.
//
// Once, because the account is the scope's and not the volume's: its key
// is one credential, read once, and N reads would be N copies of it in
// flight for nothing.
func assertScopeAccountLookedUp(t *testing.T, recorded []recordedResource) string {
	t.Helper()

	lookups := resourcesOfType(recorded, getStorageAccountToken)
	if len(lookups) != 1 {
		t.Fatalf("looked up the storage account %d times, want once for the scope", len(lookups))
	}
	want := storageAccountNameFor(testSubscriptionID, containerTestScope())
	if got := stringOrEmpty(lookups[0].Inputs["name"]); got != want {
		t.Errorf("looked up storage account %q, want the scope's derived %q", got, want)
	}
	if got, want := stringOrEmpty(lookups[0].Inputs["resourceGroupName"]), scopeResourceGroupName(containerTestScope()); got != want {
		t.Errorf("looked up the account in resource group %q, want the scope's %q", got, want)
	}
	return want
}

// assertEnvironmentStorage checks each storage link the environment
// declares, and returns the share each one names, keyed by the link's
// name — the name a template volume refers to it by.
func assertEnvironmentStorage(
	t *testing.T,
	recorded []recordedResource,
	environment recordedResource,
	account string,
) map[string]string {
	t.Helper()

	links := resourcesOfType(recorded, environmentStorageToken)
	shareByStorage := make(map[string]string, len(links))
	for _, link := range links {
		name := stringOrEmpty(link.Inputs["name"])
		if name == "" {
			t.Errorf("an environment storage link has no name; a template volume cannot refer to it")
		}
		if _, seen := shareByStorage[name]; seen {
			t.Errorf("environment storage %q is declared twice; two volumes folded onto one link", name)
		}
		shareByStorage[name] = stringOrEmpty(link.Inputs["shareName"])

		// The link belongs to this service's environment (RFC 020 §2.6:
		// the environment is per-resource, so this is the one part of the
		// consumption side that is a declaration).
		if got, want := stringOrEmpty(link.Inputs["containerAppEnvironmentId"]), environment.Name+"-id"; got != want {
			t.Errorf("storage %q is linked to environment %q, want this service's %q", name, got, want)
		}
		if got := stringOrEmpty(link.Inputs["accountName"]); got != account {
			t.Errorf("storage %q names account %q, want the scope's %q", name, got, account)
		}
		// RFC 020 declares no read-only mode: a volume exists to be written.
		if got := stringOrEmpty(link.Inputs["accessMode"]); got != "ReadWrite" {
			t.Errorf("storage %q has accessMode %q, want ReadWrite", name, got)
		}

		key := link.Inputs["accessKey"]
		if !key.IsSecret() {
			t.Errorf("storage %q holds the account key in plain text; RFC 020 §4 requires a Pulumi secret", name)
			continue
		}
		if got, want := stringOrEmpty(key.SecretValue().Element), testStorageAccountKey(account); got != want {
			t.Errorf("storage %q holds key %q, want the scope account's own key", name, got)
		}
	}
	return shareByStorage
}

// assertAzureFileMounts ties each mount path, through the template volume
// and the storage link it names, to the share it reaches. shareOf maps
// the mount path the Specification asked for to the share its volume
// names.
func assertAzureFileMounts(
	t *testing.T,
	template resource.PropertyMap,
	shareByStorage map[string]string,
	shareOf map[string]string,
) {
	t.Helper()

	declared := arrayOrEmpty(template["volumes"])
	if len(declared) != len(shareOf) {
		t.Fatalf("the template declares %d volumes, want one per mount (%d)", len(declared), len(shareOf))
	}

	storageByVolume := make(map[string]string, len(declared))
	for _, entry := range declared {
		volume := objectOrEmpty(entry)
		name := stringOrEmpty(volume["name"])
		if _, seen := storageByVolume[name]; seen {
			t.Errorf("volume name %q is declared twice; two volumes folded onto one name", name)
		}
		storageByVolume[name] = stringOrEmpty(volume["storageName"])

		// Left unset, Container Apps defaults a volume to EmptyDir: an
		// ephemeral directory that accepts writes and loses them on the
		// next restart, which is the silent wrong answer in its purest form.
		if got := stringOrEmpty(volume["storageType"]); got != "AzureFile" {
			t.Errorf("volume %q has storageType %q, want AzureFile", name, got)
		}
	}

	containers := arrayOrEmpty(template["containers"])
	if len(containers) != 1 {
		t.Fatalf("declared %d containers, want 1", len(containers))
	}
	mounts := arrayOrEmpty(objectOrEmpty(containers[0])["volumeMounts"])
	if len(mounts) != len(shareOf) {
		t.Fatalf("the container declares %d volume mounts, want one per volume (%d)", len(mounts), len(shareOf))
	}

	for _, entry := range mounts {
		mount := objectOrEmpty(entry)
		path, name := stringOrEmpty(mount["path"]), stringOrEmpty(mount["name"])

		want, asked := shareOf[path]
		if !asked {
			t.Errorf("the container mounts %q at %q, a path the resource never asked for", name, path)
			continue
		}
		storage, declaredVolume := storageByVolume[name]
		if !declaredVolume {
			t.Errorf("%q mounts volume %q, which the template does not declare", path, name)
			continue
		}
		got, linked := shareByStorage[storage]
		if !linked {
			t.Errorf("volume %q names storage %q, which the environment does not declare", name, storage)
			continue
		}
		if got != want {
			t.Errorf("%q reaches share %q, want %q — the share its own volume names", path, got, want)
		}
	}
}

// TestDeclareContainerServiceKeepsTheStorageKeySecret covers RFC 020 §4:
// the account key is a credential in the flow, and it may exist in the
// recorded inputs only as a secret.
//
// The storage link is not the only place it could surface. A key passed
// on as an app secret, an environment variable or a tag would be printed
// in the plan and stored in state in the clear, so every input of every
// resource is searched, not just the one field meant to hold it.
func TestDeclareContainerServiceKeepsTheStorageKeySecret(t *testing.T) {
	recorded := declaredContainerFor(t, mountingContainerResource([]spec.Volume{
		{Name: "uploads", MountPath: "/var/lib/uploads"},
	}), nil)

	key := testStorageAccountKey(storageAccountNameFor(testSubscriptionID, containerTestScope()))
	if !hasResource(recorded, environmentStorageToken) {
		t.Fatal("no environment storage was declared, so the key's handling cannot be checked")
	}
	for _, r := range recorded {
		for field, v := range r.Inputs {
			for _, s := range plainStrings(v) {
				if strings.Contains(s, key) {
					t.Errorf("%s %q carries the account key in plain text in %q", r.Type, r.Name, field)
				}
			}
		}
	}
}

// plainStrings collects every string in v that is not under a secret. A
// secret, or an output marked secret, hides everything beneath it.
func plainStrings(v resource.PropertyValue) []string {
	switch {
	case v.IsSecret():
		return nil
	case v.IsOutput():
		if o := v.OutputValue(); !o.Secret {
			return plainStrings(o.Element)
		}
		return nil
	case v.IsString():
		return []string{v.StringValue()}
	case v.IsArray():
		var out []string
		for _, e := range v.ArrayValue() {
			out = append(out, plainStrings(e)...)
		}
		return out
	case v.IsObject():
		var out []string
		for _, e := range v.ObjectValue() {
			out = append(out, plainStrings(e)...)
		}
		return out
	}
	return nil
}

func arrayOrEmpty(v resource.PropertyValue) []resource.PropertyValue {
	if !v.IsArray() {
		return nil
	}
	return v.ArrayValue()
}

// TestDeclareContainerServiceWithoutVolumesMountsNothing keeps every
// service that mounts nothing exactly as RFC 017 left it: no lookup, no
// key read, no storage link and no volume.
//
// The lookup matters most. A service that mounts nothing and still reads
// the account key has a credential in its state for no reason, and fails
// to deploy in a scope that has no account at all.
func TestDeclareContainerServiceWithoutVolumesMountsNothing(t *testing.T) {
	recorded := declaredContainer(t, nil)

	for _, token := range []string{getStorageAccountToken, getFileShareToken, getClientConfigToken} {
		if hasResource(recorded, token) {
			t.Errorf("%s was invoked for a service with no volumes", token)
		}
	}
	if hasResource(recorded, environmentStorageToken) {
		t.Error("environment storage was declared for a service with no volumes")
	}

	template := objectOrEmpty(findResource(t, recorded, containerAppToken).Inputs["template"])
	if n := len(arrayOrEmpty(template["volumes"])); n != 0 {
		t.Errorf("the template declares %d volumes, want none", n)
	}
	for _, c := range arrayOrEmpty(template["containers"]) {
		if n := len(arrayOrEmpty(objectOrEmpty(c)["volumeMounts"])); n != 0 {
			t.Errorf("the container declares %d volume mounts, want none", n)
		}
	}
}

// TestDeclareContainerServiceRefusesAFilesystemTheScopeDoesNotHave covers
// the lookup-not-create discipline of RFC 020 §2.8.
//
// The Engine provisions a scope's network stack before any resource in it,
// so an account or share absent here was removed out of band rather than
// lost to a race. A storage link to a share that does not exist is one
// Container Apps accepts and a revision that fails at start, so the
// provider refuses first and names what it could not find.
func TestDeclareContainerServiceRefusesAFilesystemTheScopeDoesNotHave(t *testing.T) {
	tests := []struct {
		name    string
		files   fileScope
		wantMsg string
	}{
		{
			name:    "the scope has no storage account",
			files:   fileScope{missingAccount: true},
			wantMsg: storageAccountNameFor(testSubscriptionID, containerTestScope()),
		},
		{
			name:    "no share answers to the derived name",
			files:   fileScope{missingShare: fileShareNameFor("uploads")},
			wantMsg: "uploads",
		},
	}

	r := mountingContainerResource([]spec.Volume{{Name: "uploads", MountPath: "/var/lib/uploads"}})
	props, err := decodeContainerServiceProperties(r.Properties, nil)
	if err != nil {
		t.Fatalf("decodeContainerServiceProperties() = %v", err)
	}
	net := scopeNetwork{subnetID: containerAppsSubnetID, addressSpace: "10.42.3.0/24"}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recorder{}
			err := pulumi.RunErr(func(ctx *pulumi.Context) error {
				_, err := declareContainerService(ctx, r, containerTestScope(), net, *props, nil)
				return err
			}, pulumi.WithMocks("cloudsdd-azure", "test", mockMonitor{rec: rec, files: tt.files}))

			if !errors.Is(err, ErrFilesystemMissing) {
				t.Fatalf("declareContainerService() = %v, want ErrFilesystemMissing", err)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %q, want it to name %q", err, tt.wantMsg)
			}
			// The refusal comes before anything is registered, or the stack
			// holds a resource group, an identity and an environment around
			// a mount that cannot exist.
			for _, token := range []string{rgToken, userAssignedIDToken, containerEnvToken, containerAppToken, environmentStorageToken} {
				if hasResource(rec.snapshot(), token) {
					t.Errorf("%s was declared despite the missing filesystem", token)
				}
			}
		})
	}
}
