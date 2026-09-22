// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package gcp

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider/container"
	"cloudsdd/internal/provider/pipeline"
	"cloudsdd/internal/schedule"
	"cloudsdd/internal/spec"
)

const (
	cloudRunServiceToken   = "gcp:cloudrunv2/service:Service"
	cloudRunIamMemberToken = "gcp:cloudrunv2/serviceIamMember:ServiceIamMember"
	domainMappingToken     = "gcp:cloudrun/domainMapping:DomainMapping"

	getFilestoreInstanceToken = "gcp:filestore/getInstance:getInstance"
)

// testImage is a digest-pinned reference, which is what the image rules
// require when no registry is allow-listed.
const testImage = "ghcr.io/acme/api@sha256:" +
	"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func containerProps(mutate func(map[string]any)) map[string]any {
	props := map[string]any{
		"image": testImage,
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
		Provider:   spec.ProviderGCP,
		Scope:      spec.Scope{Region: "europe-west1"},
		Properties: containerProps(nil),
	}
	if mutate != nil {
		mutate(&r)
	}
	return r
}

func declaredContainer(t *testing.T, mutate func(map[string]any)) []recordedResource {
	t.Helper()

	return declaredContainerFor(t, containerResource(func(r *spec.Resource) {
		r.Properties = containerProps(mutate)
	}))
}

// declaredContainerFor declares one specific resource, which is what the
// pipeline path needs: the image reference is assembled from `Resolved`,
// and that lives on the resource rather than in its properties.
func declaredContainerFor(t *testing.T, r spec.Resource) []recordedResource {
	t.Helper()

	props, err := decodeContainerServiceProperties(r.Properties, []string{"ghcr.io"})
	if err != nil {
		t.Fatalf("decodeContainerServiceProperties() = %v", err)
	}
	return runProgram(t, func(ctx *pulumi.Context) error {
		_, err := declareContainerService(ctx, r, testNetworkName, *props)
		return err
	})
}

// TestDeclareContainerServiceIsPrivateByDefault covers RFC 017 §2.3: the
// one resource type that runs arbitrary user code is not the one exposed
// to the internet by default.
//
// Cloud Run has two independent gates, and this asserts both. The ingress
// setting decides what can reach the service on the network; the IAM
// policy decides who may invoke it. A service private on one and public on
// the other is a service whose posture depends on which gate you read.
func TestDeclareContainerServiceIsPrivateByDefault(t *testing.T) {
	recorded := declaredContainer(t, nil)

	service := findResource(t, recorded, cloudRunServiceToken)
	if got := service.Inputs["ingress"].StringValue(); got != ingressInternal {
		t.Errorf("ingress = %q, want %q", got, ingressInternal)
	}
	if hasResource(recorded, cloudRunIamMemberToken) {
		t.Error("an IAM binding was declared on a private service; nothing outside should be able to invoke it")
	}
}

// TestDeclareContainerServicePublicIsHTTPSOnly covers the other half of
// §2.3. Cloud Run's built-in endpoint is HTTPS with a Google-managed
// certificate and no plain-HTTP listener exists to disable, so what has to
// be asserted is that "public" opens *both* gates — a service with
// INGRESS_TRAFFIC_ALL and no invoker binding is reachable and answers 403
// to everyone, which is a deployment that looks done and serves nobody.
func TestDeclareContainerServicePublicIsHTTPSOnly(t *testing.T) {
	recorded := declaredContainer(t, func(p map[string]any) { p["public"] = true })

	service := findResource(t, recorded, cloudRunServiceToken)
	if got := service.Inputs["ingress"].StringValue(); got != ingressAll {
		t.Errorf("ingress = %q, want %q", got, ingressAll)
	}

	binding := findResource(t, recorded, cloudRunIamMemberToken)
	if got := binding.Inputs["role"].StringValue(); got != invokerRole {
		t.Errorf("role = %q, want %q — the binding permits calling the service, nothing more", got, invokerRole)
	}
	if got := binding.Inputs["member"].StringValue(); got != allUsers {
		t.Errorf("member = %q, want %q", got, allUsers)
	}
}

// TestDeclareContainerServiceIdentityHasNothingAttached covers RFC 017
// §2.6's identity row.
//
// Omitting the service account block would not be equivalent here to what
// it is on a Compute Engine instance: Cloud Run falls back to the default
// compute service account, which carries Editor on the whole project. So
// the assertion is that an account is declared, that the service runs as
// it, and that nothing binds a role to it.
func TestDeclareContainerServiceIdentityHasNothingAttached(t *testing.T) {
	recorded := declaredContainer(t, nil)

	account := findResource(t, recorded, serviceAccountToken)
	if got := account.Inputs["accountId"].StringValue(); got != googleAccountID("api", "-run") {
		t.Errorf("accountId = %q, want the derived %q", got, googleAccountID("api", "-run"))
	}

	service := findResource(t, recorded, cloudRunServiceToken)
	runAs := service.Inputs["template"].ObjectValue()["serviceAccount"].StringValue()
	if !strings.Contains(runAs, googleAccountID("api", "-run")) {
		t.Errorf("service runs as %q, want the dedicated account", runAs)
	}

	// No project binding and no custom role. Both exist in this package for
	// the power scheduler, so a copy-paste that granted the container one
	// would compile and pass every other test here.
	for _, token := range []string{iamMemberToken, customRoleToken} {
		if hasResource(recorded, token) {
			t.Errorf("%s was declared; the service identity must carry no role", token)
		}
	}
}

// TestDeclareContainerServiceJoinsTheScopeNetwork: the service must sit in
// the scope's own subnet, beside the database it was deployed to talk to
// (RFC 016 §2.3), and route everything out through the scope's NAT.
//
// ALL_TRAFFIC rather than PRIVATE_RANGES_ONLY is the assertion worth
// having: the weaker setting still reaches the database and lets
// internet-bound traffic leave from Google's shared pool, which is the
// "reaches the internet unnoticed" trade RFC 017 §2.7 refused elsewhere.
func TestDeclareContainerServiceJoinsTheScopeNetwork(t *testing.T) {
	recorded := declaredContainer(t, nil)

	vpc := findResource(t, recorded, cloudRunServiceToken).
		Inputs["template"].ObjectValue()["vpcAccess"].ObjectValue()

	if got := vpc["egress"].StringValue(); got != vpcEgressAll {
		t.Errorf("vpc egress = %q, want %q so the scope leaves from one address", got, vpcEgressAll)
	}

	interfaces := vpc["networkInterfaces"].ArrayValue()
	if len(interfaces) != 1 {
		t.Fatalf("declared %d network interfaces, want 1", len(interfaces))
	}
	iface := interfaces[0].ObjectValue()
	if got := iface["network"].StringValue(); got != testNetworkName {
		t.Errorf("network = %q, want the scope network %q", got, testNetworkName)
	}
	if got := iface["subnetwork"].StringValue(); got != testNetworkName+"-subnet" {
		t.Errorf("subnetwork = %q, want the scope's workload subnet", got)
	}
}

// TestDeclareContainerServiceScaling: replicas is a ceiling on Cloud Run,
// and the floor is zero — which is the platform's whole point, and why a
// power schedule is largely redundant here (RFC 017 §2.5).
func TestDeclareContainerServiceScaling(t *testing.T) {
	tests := []struct {
		name     string
		replicas any
		wantMax  float64
	}{
		{name: "absent defaults to one", replicas: nil, wantMax: 1},
		{name: "explicit", replicas: 4, wantMax: 4},
		{name: "the ceiling", replicas: container.MaxReplicas, wantMax: float64(container.MaxReplicas)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorded := declaredContainer(t, func(p map[string]any) {
				if tt.replicas != nil {
					p["replicas"] = tt.replicas
				}
			})

			scaling := findResource(t, recorded, cloudRunServiceToken).
				Inputs["template"].ObjectValue()["scaling"].ObjectValue()

			if got := scaling["maxInstanceCount"].NumberValue(); got != tt.wantMax {
				t.Errorf("maxInstanceCount = %v, want %v", got, tt.wantMax)
			}
			if got := scaling["minInstanceCount"].NumberValue(); got != 0 {
				t.Errorf("minInstanceCount = %v, want 0 — Cloud Run bills per request", got)
			}
		})
	}
}

// TestRunResources: the CPU and memory pairs are not free choices. Cloud
// Run refuses a container asking for more CPU than its memory supports, so
// a tidier-looking table would produce a service that fails on apply after
// the user approved the plan.
func TestRunResources(t *testing.T) {
	tests := []struct {
		size       container.Size
		cpu        string
		memory     string
		wantErr    bool
		minMemory  float64 // GiB Cloud Run requires for that CPU count
		cpuNumeric float64
	}{
		{size: container.SizeSmall, cpu: "1", memory: "512Mi", cpuNumeric: 1, minMemory: 0},
		{size: container.SizeMedium, cpu: "2", memory: "2Gi", cpuNumeric: 2, minMemory: 1},
		{size: container.SizeLarge, cpu: "4", memory: "4Gi", cpuNumeric: 4, minMemory: 2},
		{size: "enormous", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(string(tt.size), func(t *testing.T) {
			cpu, memory, err := runResources(tt.size)

			if tt.wantErr {
				if !errors.Is(err, ErrUnsupportedSize) {
					t.Fatalf("runResources(%q) = %v, want ErrUnsupportedSize", tt.size, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("runResources(%q) = %v", tt.size, err)
			}
			if cpu != tt.cpu || memory != tt.memory {
				t.Errorf("runResources(%q) = %q/%q, want %q/%q", tt.size, cpu, memory, tt.cpu, tt.memory)
			}
			if gib := memoryGiB(t, memory); gib < tt.minMemory {
				t.Errorf("%s: %s of memory is below the %v Gi Cloud Run requires for %v vCPU",
					tt.size, memory, tt.minMemory, tt.cpuNumeric)
			}
		})
	}
}

// memoryGiB parses a Cloud Run memory limit into GiB.
func memoryGiB(t *testing.T, limit string) float64 {
	t.Helper()

	var unit string
	switch {
	case strings.HasSuffix(limit, "Gi"):
		unit = "Gi"
	case strings.HasSuffix(limit, "Mi"):
		unit = "Mi"
	default:
		t.Fatalf("memory limit %q carries no recognised unit", limit)
	}

	n, err := strconv.ParseFloat(strings.TrimSuffix(limit, unit), 64)
	if err != nil {
		t.Fatalf("unparseable memory limit %q: %v", limit, err)
	}
	if unit == "Mi" {
		return n / 1024
	}
	return n
}

// TestDecodeContainerServiceProperties covers the decoding rules RFC 017
// §5.1 lists, plus the two GCP-specific refusals.
func TestDecodeContainerServiceProperties(t *testing.T) {
	tests := []struct {
		name    string
		props   map[string]any
		allowed []string
		wantErr error
		wantMsg string
	}{
		{
			name:    "a digest-pinned image with no allowlist",
			props:   containerProps(nil),
			allowed: nil,
		},
		{
			name:    "a tag from an allow-listed registry",
			props:   containerProps(func(p map[string]any) { p["image"] = "ghcr.io/acme/api:2.1" }),
			allowed: []string{"ghcr.io"},
		},
		{
			// The provider applies the image rules too, so one driven
			// directly — outside the Engine — is no less safe (RFC 011 §2.3).
			name:    "a tag with no allowlist is refused by the provider as well",
			props:   containerProps(func(p map[string]any) { p["image"] = "ghcr.io/acme/api:2.1" }),
			wantErr: container.ErrImageMutable,
		},
		{
			name:    "latest is refused here too",
			props:   containerProps(func(p map[string]any) { p["image"] = "ghcr.io/acme/api:latest" }),
			allowed: []string{"ghcr.io"},
			wantErr: container.ErrImageLatest,
		},
		{
			// Cloud Run reads a zero maxInstanceCount as unset and applies
			// its own default ceiling, so honouring the request would
			// deploy the opposite of what was written (RFC 012 §1.3).
			name:    "zero replicas is refused rather than silently uncapped",
			props:   containerProps(func(p map[string]any) { p["replicas"] = 0 }),
			wantErr: ErrZeroReplicasUnsupported,
		},
		{
			name:    "an unmapped size",
			props:   containerProps(func(p map[string]any) { p["size"] = "enormous" }),
			wantMsg: "property validation failed",
		},
		{
			// Since RFC 018 §3 `image` is optional in the schema and
			// required by ValidateSource, because `pipeline` is the other
			// way to name one. A service that names neither is still
			// refused, just by a rule rather than by a tag.
			name:    "neither an image nor a pipeline",
			props:   map[string]any{"port": 8080, "size": "small"},
			wantErr: container.ErrNoImageSource,
		},
		{
			name:    "a missing port",
			props:   map[string]any{"image": testImage, "size": "small"},
			wantMsg: "property validation failed",
		},
		{
			name:    "a port outside the range",
			props:   containerProps(func(p map[string]any) { p["port"] = 70000 }),
			wantMsg: "property validation failed",
		},
		{
			name:    "more replicas than the cap",
			props:   containerProps(func(p map[string]any) { p["replicas"] = container.MaxReplicas + 1 }),
			wantMsg: "property validation failed",
		},
		{
			// Mass Assignment: a property the provider does not understand
			// means the user asked for something they will not get.
			name:    "an unknown property",
			props:   containerProps(func(p map[string]any) { p["cpu_architecture"] = "arm64" }),
			wantMsg: "unknown or malformed property",
		},
		{
			// The credential-name denylist applies independently of any
			// resource's schema (RFC 002 §2.4). It matters here because a
			// container is the obvious place to try to paste one.
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

// TestValidateContainerServiceRejectsZones: Cloud Run is regional and
// places instances itself, so a pinned zone is a placement the user asked
// for and will not get.
func TestValidateContainerServiceRejectsZones(t *testing.T) {
	p := &GCPProvider{stateDir: t.TempDir(), passphrase: "test"}

	r := containerResource(func(r *spec.Resource) {
		r.Scope.Zones = []string{"europe-west1-b"}
	})

	err := p.Validate(context.Background(), r, spec.Policies{})
	if !errors.Is(err, ErrZonesNotSupported) {
		t.Fatalf("Validate() = %v, want ErrZonesNotSupported", err)
	}
}

// TestValidateContainerServiceAcceptsAWellFormedOne is the regression test
// for the defect RFC 017 §1 records: a container_service used to validate
// as a Specification, compile a schedule, render a plan, and then fail
// with "unsupported resource type".
func TestValidateContainerServiceAcceptsAWellFormedOne(t *testing.T) {
	p := &GCPProvider{stateDir: t.TempDir(), passphrase: "test"}

	if err := p.Validate(context.Background(), containerResource(nil), spec.Policies{}); err != nil {
		t.Fatalf("Validate() = %v, want a well-formed container_service to be accepted", err)
	}
}

// TestDeclareContainerServiceDomainIsOptional covers the GCP half of RFC
// 017 §2.3.1.
//
// A domain is optional here and required on AWS, which is a difference
// worth stating rather than papering over: Cloud Run serves the service on
// *.run.app with a Google-managed certificate, so absence means "use that
// endpoint" rather than "there is no HTTPS". ACM has no equivalent, which
// is why the same absence is an error there.
func TestDeclareContainerServiceDomainIsOptional(t *testing.T) {
	withoutDomain := declaredContainer(t, func(p map[string]any) { p["public"] = true })
	if hasResource(withoutDomain, domainMappingToken) {
		t.Error("a domain mapping was declared for a service that named no domain")
	}

	withDomain := declaredContainer(t, func(p map[string]any) {
		p["public"] = true
		p["domain"] = "api.acme.example"
	})

	mapping := findResource(t, withDomain, domainMappingToken)
	if got := mapping.Inputs["name"].StringValue(); got != "api.acme.example" {
		t.Errorf("domain mapping name = %q, want the requested domain", got)
	}
	// NONE would map the domain and serve no certificate for it, which is
	// the plain-HTTP outcome §2.3 refuses.
	spec := mapping.Inputs["spec"].ObjectValue()
	if got := spec["certificateMode"].StringValue(); got != certificateModeAutomatic {
		t.Errorf("certificate mode = %q, want %q", got, certificateModeAutomatic)
	}
	if !spec["routeName"].HasValue() {
		t.Error("the mapping names no route; it would point at nothing")
	}
}

// TestDecodeContainerServiceRejectsADomainWithoutPublic: a hostname on a
// service nothing outside can reach is a property the user asked for and
// will not get, and the rule holds on every provider.
func TestDecodeContainerServiceRejectsADomainWithoutPublic(t *testing.T) {
	_, err := decodeContainerServiceProperties(containerProps(func(p map[string]any) {
		p["domain"] = "api.acme.example"
	}), nil)

	if !errors.Is(err, container.ErrDomainWithoutPublic) {
		t.Fatalf("decodeContainerServiceProperties() = %v, want ErrDomainWithoutPublic", err)
	}
}

// TestDeclareContainerServiceRunsThePipelineImage is the service half of
// RFC 018 §2.4.1: a service that names a `pipeline` instead of an `image`
// runs the Artifact Registry reference the resolved commit named.
//
// Without this the decode succeeds — `image` is optional since RFC 018 §3
// — and Cloud Run is handed an empty image, which is a service that
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

	recorded := declaredContainerFor(t, r)

	got := containerImage(t, findResource(t, recorded, cloudRunServiceToken))
	want := artifactImageReference(r.Scope.Region, testProjectID, testResolved.ImageName, testResolved.Commit)
	if got != want {
		t.Errorf("container image = %q, want the pipeline's own %q", got, want)
	}
	// The commit, never a branch and never `latest` — the registry's
	// immutable tags are what make that as strong as a digest.
	if !strings.HasSuffix(got, ":"+testCommit) {
		t.Errorf("container image = %q, want it tagged with the resolved commit", got)
	}

	// The project comes from the registry rather than from configuration,
	// so the lookup must actually happen and must name the repository the
	// pipeline created.
	lookup := findResource(t, recorded, getArtifactRepositoryToken)
	if id := lookup.Inputs["repositoryId"].StringValue(); id != artifactRepositoryID(testResolved.ImageName) {
		t.Errorf("looked up repository %q, want %q", id, artifactRepositoryID(testResolved.ImageName))
	}
	if loc := lookup.Inputs["location"].StringValue(); loc != r.Scope.Region {
		t.Errorf("looked up location %q, want the resource's region %q", loc, r.Scope.Region)
	}
}

// TestDeclareContainerServiceWithAnImageAsksTheRegistryNothing: the
// pipeline path is entered only by a service that has no image of its own.
// A lookup on every service would fail every deployment that names a
// public image, since there is no repository to find.
func TestDeclareContainerServiceWithAnImageAsksTheRegistryNothing(t *testing.T) {
	recorded := declaredContainer(t, nil)

	if hasResource(recorded, getArtifactRepositoryToken) {
		t.Error("the registry was queried for a service that named its own image")
	}
	if got := containerImage(t, findResource(t, recorded, cloudRunServiceToken)); got != testImage {
		t.Errorf("container image = %q, want the requested %q", got, testImage)
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

	var got error
	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		_, got = declareContainerService(ctx, r, testNetworkName, *props)
		return nil
	})

	if !errors.Is(got, pipeline.ErrNotResolved) {
		t.Fatalf("declareContainerService() = %v, want %v", got, pipeline.ErrNotResolved)
	}
	// The refusal has to come before the service is registered, or the
	// stack holds a Cloud Run service pointed at nothing.
	if hasResource(recorded, cloudRunServiceToken) {
		t.Error("a Cloud Run service was declared despite the unresolved pipeline reference")
	}
}

// containerImage reads the image off the single container of a declared
// Cloud Run service.
func containerImage(t *testing.T, service recordedResource) string {
	t.Helper()

	containers := service.Inputs["template"].ObjectValue()["containers"].ArrayValue()
	if len(containers) != 1 {
		t.Fatalf("declared %d containers, want 1", len(containers))
	}
	return containers[0].ObjectValue()["image"].StringValue()
}

// TestCloudRunRefusesAnExplicitSchedule covers RFC 017 §2.5 on GCP.
//
// Not an implementation gap. Cloud Run bills per request and idles to zero
// between them, so there is no running state to switch off and no saving
// left to deliver. §2.5 proposed setting max instances to zero; step 2
// found that Cloud Run reads a zero ceiling as unset, which would uncap
// the service rather than stop it.
//
// The explicit/inherited distinction is the point. A schedule the user
// wrote on this resource is a request they will not get, and is refused.
// One inherited from policies is merely inapplicable — refusing it would
// break every Specification that sets an environment-wide schedule and
// happens to include a Cloud Run service.
func TestCloudRunRefusesAnExplicitSchedule(t *testing.T) {
	sch := &schedule.Schedule{
		Enabled:  boolPtr(true),
		Timezone: "Europe/Rome",
		Start:    "08:00",
		Stop:     "19:00",
	}

	t.Run("explicit is refused", func(t *testing.T) {
		r := containerResource(func(r *spec.Resource) { r.Schedule = sch })

		_, err := resourceSchedule(r, spec.Policies{})
		if !errors.Is(err, ErrCloudRunNotSchedulable) {
			t.Fatalf("resourceSchedule() = %v, want ErrCloudRunNotSchedulable", err)
		}
	})

	t.Run("inherited is dropped without error", func(t *testing.T) {
		rules, err := resourceSchedule(containerResource(nil), spec.Policies{Schedule: sch})
		if err != nil {
			t.Fatalf("resourceSchedule() = %v, want an inherited schedule to be tolerated", err)
		}
		if len(rules) != 0 {
			t.Errorf("compiled %d rules, want none — Cloud Run has nothing to schedule", len(rules))
		}
	})

	t.Run("Validate refuses an explicit schedule too", func(t *testing.T) {
		p := &GCPProvider{stateDir: t.TempDir(), passphrase: "test"}
		r := containerResource(func(r *spec.Resource) { r.Schedule = sch })

		if err := p.Validate(context.Background(), r, spec.Policies{}); !errors.Is(err, ErrCloudRunNotSchedulable) {
			t.Fatalf("Validate() = %v, want ErrCloudRunNotSchedulable", err)
		}
	})
}

// TestDeclareContainerServiceDeclaresNoScheduleResources: Cloud Run
// carries no scheduling machinery under any circumstances, unlike the
// Cloud SQL path in this same package.
func TestDeclareContainerServiceDeclaresNoScheduleResources(t *testing.T) {
	recorded := declaredContainer(t, nil)

	for _, token := range []string{schedulerJobToken, customRoleToken, iamMemberToken} {
		if hasResource(recorded, token) {
			t.Errorf("%s was declared for a Cloud Run service", token)
		}
	}
}

// mountingContainerResource is a container service in testScope() that
// mounts volumes, so the instance names it derives are the ones the
// network stack declared.
func mountingContainerResource(volumes []spec.Volume) spec.Resource {
	return containerResource(func(r *spec.Resource) {
		r.Scope.Environment = testScope().Environment
		r.Volumes = volumes
	})
}

// cloudRunVolumeName is the charset Cloud Run accepts for a volume's
// name: a DNS label. A Specification's volume name admits uppercase and
// underscores, so it cannot be passed through verbatim.
var cloudRunVolumeName = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)

// TestDeclareContainerServiceMountsItsVolumes covers the GCP row of RFC
// 020 §2.8's mount table.
//
// Two volumes rather than one, because every assertion here is about
// *which* instance a mount reaches. A single volume would pass just as
// well against an implementation that looked the first one up and wired
// every mount to it — which is the shape of the failure §1 describes, two
// names resolving to one share. One of the two carries uppercase and an
// underscore, which the Specification admits and Cloud Run does not.
func TestDeclareContainerServiceMountsItsVolumes(t *testing.T) {
	volumes := []spec.Volume{
		{Name: "uploads", MountPath: "/var/lib/uploads", SizeGB: 1024},
		{Name: "Shared_Cache", MountPath: "/var/cache/app", SizeGB: 2048},
	}
	r := mountingContainerResource(volumes)
	recorded := declaredContainerFor(t, r)

	// Lookup, not create. An instance declared here would be a second one
	// beside the scope's — a second terabyte on the invoice, and an empty
	// directory where a shared one was meant to be.
	if n := len(resourcesOfType(recorded, filestoreInstanceToken)); n != 0 {
		t.Errorf("the resource stack declared %d Filestore instances; the scope's network stack owns them", n)
	}

	lookedUp := map[string]string{}
	for _, l := range resourcesOfType(recorded, getFilestoreInstanceToken) {
		lookedUp[l.Inputs["name"].StringValue()] = stringOrEmpty(l.Inputs["location"])
	}
	if len(lookedUp) != len(volumes) {
		t.Fatalf("looked up %d distinct instances, want one per volume (%d)", len(lookedUp), len(volumes))
	}

	// Each volume is reached by the name the network stack gave it, in the
	// zone the network stack put it. Basic tiers are zonal, and a lookup
	// that fell back to the provider's default location would find nothing.
	serverOf := make(map[string]string, len(volumes))
	for _, v := range volumes {
		instance := filestoreInstanceNameFor(testScope(), v.Name)
		location, ok := lookedUp[instance]
		if !ok {
			t.Fatalf("volume %q was not looked up by its instance name %q; names sought = %v",
				v.Name, instance, lookedUp)
		}
		if want := instanceZone(r.Scope.Region, ""); location != want {
			t.Errorf("instance %q was looked up in %q, want the zone the network stack placed it in, %q",
				instance, location, want)
		}
		serverOf[v.MountPath] = testFilestoreAddress(instance)
	}

	template := findResource(t, recorded, cloudRunServiceToken).Inputs["template"].ObjectValue()
	assertNfsMounts(t, template, serverOf)

	// Cloud Run mounts NFS only in the second-generation environment. The
	// first accepts the template and fails the revision at start.
	if got := stringOrEmpty(template["executionEnvironment"]); got != "EXECUTION_ENVIRONMENT_GEN2" {
		t.Errorf("executionEnvironment = %q, want EXECUTION_ENVIRONMENT_GEN2; NFS mounts need it", got)
	}

	// Filestore's basic tiers have no data-plane IAM: access is decided by
	// the network (RFC 020 §4). Mounting therefore grants the service's
	// identity nothing, and RFC 017 §2.6's empty identity stays empty.
	for _, token := range []string{iamMemberToken, customRoleToken} {
		if hasResource(recorded, token) {
			t.Errorf("%s was declared for a mount; Filestore basic tiers authorize by network alone", token)
		}
	}
}

// stringOrEmpty reads an input that may have been left unset. An omitted
// input records as null, and StringValue on null panics — which would kill
// a mutation without ever printing the diagnostic written for it.
func stringOrEmpty(v resource.PropertyValue) string {
	if !v.IsString() {
		return ""
	}
	return v.StringValue()
}

// assertNfsMounts ties each mount path, through the volume name Cloud Run
// joins them by, to the NFS server it reaches. serverOf maps the mount
// path the Specification asked for to the address of the instance that
// volume names.
func assertNfsMounts(t *testing.T, template resource.PropertyMap, serverOf map[string]string) {
	t.Helper()

	declared := template["volumes"].ArrayValue()
	if len(declared) != len(serverOf) {
		t.Fatalf("the template declares %d volumes, want one per mount (%d)", len(declared), len(serverOf))
	}

	serverByVolume := make(map[string]string, len(declared))
	for _, entry := range declared {
		volume := entry.ObjectValue()
		name := volume["name"].StringValue()
		if !cloudRunVolumeName.MatchString(name) {
			t.Errorf("volume name %q is not a DNS label; Cloud Run refuses it", name)
		}
		if _, seen := serverByVolume[name]; seen {
			t.Errorf("volume name %q is declared twice; two volumes folded onto one name", name)
		}

		nfs := volume["nfs"].ObjectValue()
		serverByVolume[name] = nfs["server"].StringValue()
		// The instance carries one share, and the export is its name.
		if got, want := nfs["path"].StringValue(), "/"+filestoreShareName; got != want {
			t.Errorf("volume %q exports %q, want the instance's one share %q", name, got, want)
		}
		// RFC 020 declares no read-only mode: a volume exists to be written.
		if ro, ok := nfs["readOnly"]; ok && ro.IsBool() && ro.BoolValue() {
			t.Errorf("volume %q is mounted read-only; RFC 020 declares no read-only mode", name)
		}
	}

	containers := template["containers"].ArrayValue()
	if len(containers) != 1 {
		t.Fatalf("declared %d containers, want 1", len(containers))
	}
	mounts := containers[0].ObjectValue()["volumeMounts"].ArrayValue()
	if len(mounts) != len(serverOf) {
		t.Fatalf("the container declares %d volume mounts, want one per volume (%d)", len(mounts), len(serverOf))
	}

	// The mount path is what the user wrote, and the server is what the
	// network stack built. Joining the two through the volume name is what
	// proves each path reaches its own instance and not a neighbour's.
	for _, entry := range mounts {
		mount := entry.ObjectValue()
		path, name := mount["mountPath"].StringValue(), mount["name"].StringValue()

		want, asked := serverOf[path]
		if !asked {
			t.Errorf("the container mounts %q at %q, a path the resource never asked for", name, path)
			continue
		}
		got, declaredVolume := serverByVolume[name]
		if !declaredVolume {
			t.Errorf("%q mounts volume %q, which the template does not declare", path, name)
			continue
		}
		if got != want {
			t.Errorf("%q reaches the NFS server %q, want %q — the instance its own volume names", path, got, want)
		}
	}
}

// TestDeclareContainerServiceWithoutVolumesMountsNothing keeps every
// service that mounts nothing exactly as RFC 017 left it: no lookup, no
// volume, and no change of execution environment — which would roll a new
// revision of every existing service for a feature none of them use.
func TestDeclareContainerServiceWithoutVolumesMountsNothing(t *testing.T) {
	recorded := declaredContainer(t, nil)

	if hasResource(recorded, getFilestoreInstanceToken) {
		t.Error("Filestore was queried for a service with no volumes")
	}

	template := findResource(t, recorded, cloudRunServiceToken).Inputs["template"].ObjectValue()
	if volumes := template["volumes"]; volumes.IsArray() && len(volumes.ArrayValue()) != 0 {
		t.Errorf("the template declares %d volumes, want none", len(volumes.ArrayValue()))
	}
	if env := template["executionEnvironment"]; env.IsString() && env.StringValue() != "" {
		t.Errorf("executionEnvironment = %q for a service that mounts nothing, want it left unset", env.StringValue())
	}
}

// TestDeclareContainerServiceRefusesAFilesystemTheScopeDoesNotHave covers
// the lookup-not-create discipline of RFC 020 §2.8.
//
// The Engine provisions a scope's network stack before any resource in it,
// so an instance absent here was removed out of band rather than lost to a
// race. Creating a replacement would be §1's silent wrong answer — a
// second, empty terabyte where a shared one was meant to be — so the
// provider refuses and names what it could not find.
func TestDeclareContainerServiceRefusesAFilesystemTheScopeDoesNotHave(t *testing.T) {
	tests := []struct {
		name      string
		filestore filestoreScope
		wantMsg   string
	}{
		{
			name:      "no instance answers to the derived name",
			filestore: filestoreScope{missingInstance: filestoreInstanceNameFor(testScope(), "uploads")},
			wantMsg:   "uploads",
		},
		{
			// The instance is there and has no address to mount. An NFS
			// volume with an empty server is a template Cloud Run accepts
			// and a revision that fails at start.
			name:      "the instance has no private address",
			filestore: filestoreScope{withoutAddress: true},
			wantMsg:   "address",
		},
	}

	r := mountingContainerResource([]spec.Volume{{Name: "uploads", MountPath: "/var/lib/uploads", SizeGB: 1024}})
	props, err := decodeContainerServiceProperties(r.Properties, nil)
	if err != nil {
		t.Fatalf("decodeContainerServiceProperties() = %v", err)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &recorder{}
			err := pulumi.RunErr(func(ctx *pulumi.Context) error {
				_, err := declareContainerService(ctx, r, testNetworkName, *props)
				return err
			}, pulumi.WithMocks("cloudsdd-gcp", "test", mockMonitor{rec: rec, filestore: tt.filestore}))

			if !errors.Is(err, ErrFilesystemMissing) {
				t.Fatalf("declareContainerService() = %v, want ErrFilesystemMissing", err)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %q, want it to name %q", err, tt.wantMsg)
			}
			// The refusal comes before anything is registered, or the stack
			// holds a service and an identity for a mount that cannot exist.
			for _, token := range []string{cloudRunServiceToken, serviceAccountToken} {
				if hasResource(rec.snapshot(), token) {
					t.Errorf("%s was declared despite the missing filesystem", token)
				}
			}
		})
	}
}
