// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package azure

import (
	"fmt"
	"strings"

	"github.com/pulumi/pulumi-azure/sdk/v5/go/azure/authorization"
	"github.com/pulumi/pulumi-azure/sdk/v5/go/azure/containerapp"
	"github.com/pulumi/pulumi-azure/sdk/v5/go/azure/core"
	"github.com/pulumi/pulumi-azure/sdk/v5/go/azure/dns"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider"
	"cloudsdd/internal/provider/container"
	"cloudsdd/internal/schedule"
	"cloudsdd/internal/spec"
)

// Container Apps configuration (RFC 017 §2.6).
const (
	// revisionModeSingle sends all traffic to the newest revision. The
	// alternative, Multiple, is a deployment strategy — weighted traffic
	// across revisions — and belongs to an RFC that can describe one.
	revisionModeSingle = "Single"

	// ingressTransportAuto lets the platform negotiate HTTP/1.1 or HTTP/2
	// with the container. It has no bearing on what is served to callers,
	// which is HTTPS either way.
	ingressTransportAuto = "auto"

	// A volume's storage link is read-write: RFC 020 declares no
	// read-only mode, because a volume exists to be written. AzureFile is
	// stated on every template volume, because left unset Container Apps
	// defaults to EmptyDir — a directory that accepts writes and loses
	// them on the next restart.
	storageAccessReadWrite = "ReadWrite"
	storageTypeAzureFile   = "AzureFile"

	// identityUserAssigned attaches an identity CloudSDD declares, rather
	// than one Azure generates. It matters because the identity has to
	// exist as a resource for a test to assert that nothing is assigned to
	// it (RFC 017 §2.6).
	identityUserAssigned = "UserAssigned"

	// workloadProfileConsumption is the scale-to-zero, pay-per-use
	// profile. Naming a profile at all is what lowers the environment's
	// subnet requirement from a /23 to a /27 — the scope's subnets are
	// /24s (RFC 016 §2.4), so a consumption-only environment would not
	// fit in the address plan at all.
	workloadProfileConsumption     = "Consumption"
	workloadProfileConsumptionName = "Consumption"

	// certificateBindingSNI serves the managed certificate for a custom
	// domain. Disabled would bind the domain and serve no certificate for
	// it, which is the plain-HTTP outcome RFC 017 §2.3 refuses.
	certificateBindingSNI = "SniEnabled"

	// domainVerificationPrefix is the label Azure requires on the TXT
	// record proving control of a custom domain.
	domainVerificationPrefix = "asuid."
)

// containerResources maps a cloud-agnostic size onto a Container Apps
// CPU/memory pair.
//
// Container Apps accepts only certain combinations — memory must be
// exactly twice the CPU count in GiB on the consumption profile — so an
// arbitrary-looking pair here is an app the API rejects after the user
// approved the plan.
func containerResources(size container.Size) (cpu float64, memory string, err error) {
	switch size {
	case container.SizeSmall:
		return 0.5, "1Gi", nil
	case container.SizeMedium:
		return 1.0, "2Gi", nil
	case container.SizeLarge:
		return 2.0, "4Gi", nil
	default:
		return 0, "", fmt.Errorf("azure: %w: %q", ErrUnsupportedContainerSize, size)
	}
}

// decodeContainerServiceProperties decodes and validates Properties as a
// cloud-agnostic container.Properties through the shared strict decoder.
func decodeContainerServiceProperties(props map[string]any, allowedRegistries []string) (*container.Properties, error) {
	var p container.Properties
	if err := dec.Properties(props, &p); err != nil {
		return nil, err
	}
	if _, _, err := containerResources(p.Size); err != nil {
		return nil, err
	}
	if err := p.ValidateImageSource(allowedRegistries); err != nil {
		return nil, fmt.Errorf("azure: %w", err)
	}
	if err := p.ValidateIngress(); err != nil {
		return nil, fmt.Errorf("azure: %w", err)
	}
	return &p, nil
}

// declareContainerService registers the Container Apps environment, the
// app, the identity it runs as, and — when a domain is given — the DNS
// records and custom-domain binding (RFC 017 §2.6).
func declareContainerService(
	ctx *pulumi.Context,
	r spec.Resource,
	s provider.NetworkScope,
	net scopeNetwork,
	p container.Properties,
	rules []schedule.Rule,
) (*containerapp.App, error) {
	id := r.ID
	location := r.Scope.Region

	cpu, memory, err := containerResources(p.Size)
	if err != nil {
		return nil, err
	}

	// A service fed by a pipeline names no image: the reference is turned
	// into one here, where the registry's login server is knowable (RFC 018
	// §2.4.1). It happens before anything is registered, so an unresolved
	// reference is refused rather than leaving a stack holding a resource
	// group and an app pointed at nothing.
	image := p.Image
	if image == "" {
		if image, err = pipelineImage(ctx, r); err != nil {
			return nil, err
		}
	}

	// The scope's shares are found before anything is registered, for the
	// same reason: a missing one is refused rather than leaving a stack
	// holding an environment around a mount that cannot exist.
	shares, err := lookupMountedShares(ctx, s, r.Volumes)
	if err != nil {
		return nil, err
	}

	rg, err := core.NewResourceGroup(ctx, id+"-rg", &core.ResourceGroupArgs{
		Location: pulumi.String(location),
	})
	if err != nil {
		return nil, fmt.Errorf("azure: failed to declare the resource group for %q: %w", id, err)
	}

	// A user-assigned identity with no role assignments, following RFC
	// 013's choice to give a VM no standing credential. User-assigned
	// rather than system-assigned so the absence is a property of a
	// resource a test can point at, rather than of one Azure generates.
	identity, err := authorization.NewUserAssignedIdentity(ctx, id+"-identity",
		&authorization.UserAssignedIdentityArgs{
			Name:              pulumi.String(id + "-cloudsdd"),
			ResourceGroupName: rg.Name,
			Location:          rg.Location,
		})
	if err != nil {
		return nil, fmt.Errorf("azure: failed to declare the identity for %q: %w", id, err)
	}

	// The environment is injected into the scope's delegated subnet, so
	// the app sits beside the database it was deployed to talk to (RFC 016
	// §2.3) and leaves through the scope's NAT (RFC 017 §2.7).
	//
	// InternalLoadBalancerEnabled is the environment-level half of "not
	// public": with it set, the environment has no external endpoint at
	// all, so a later app that asked for external ingress could not get
	// one by accident.
	environment, err := containerapp.NewEnvironment(ctx, id+"-env", &containerapp.EnvironmentArgs{
		Name:                        pulumi.String(id + "-cloudsdd"),
		ResourceGroupName:           rg.Name,
		Location:                    rg.Location,
		InfrastructureSubnetId:      pulumi.String(net.subnetID),
		InternalLoadBalancerEnabled: pulumi.Bool(!p.EffectivePublic()),
		WorkloadProfiles: containerapp.EnvironmentWorkloadProfileArray{
			&containerapp.EnvironmentWorkloadProfileArgs{
				Name:                pulumi.String(workloadProfileConsumptionName),
				WorkloadProfileType: pulumi.String(workloadProfileConsumption),
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("azure: failed to declare the container environment for %q: %w", id, err)
	}

	links, err := declareEnvironmentStorage(ctx, id, environment, shares)
	if err != nil {
		return nil, err
	}

	app, err := containerapp.NewApp(ctx, id, &containerapp.AppArgs{
		Name:                      pulumi.String(id),
		ResourceGroupName:         rg.Name,
		ContainerAppEnvironmentId: environment.ID(),
		RevisionMode:              pulumi.String(revisionModeSingle),
		WorkloadProfileName:       pulumi.String(workloadProfileConsumptionName),
		Identity: &containerapp.AppIdentityArgs{
			Type:        pulumi.String(identityUserAssigned),
			IdentityIds: pulumi.StringArray{identity.ID()},
		},
		Ingress: &containerapp.AppIngressArgs{
			// External on a public service; on a private one the
			// environment's internal load balancer means "external" is
			// still only the VNet.
			ExternalEnabled: pulumi.Bool(p.EffectivePublic()),
			TargetPort:      pulumi.Int(p.Port),
			Transport:       pulumi.String(ingressTransportAuto),
			// The single most important line here. Left true — the Azure
			// default — the ingress would serve plain HTTP alongside
			// HTTPS instead of redirecting, which RFC 017 §2.3 refuses.
			AllowInsecureConnections: pulumi.Bool(false),
			TrafficWeights: containerapp.AppIngressTrafficWeightArray{
				&containerapp.AppIngressTrafficWeightArgs{
					LatestRevision: pulumi.Bool(true),
					Percentage:     pulumi.Int(100),
				},
			},
		},
		Template: &containerapp.AppTemplateArgs{
			// Both bounds are the replica count rather than a range.
			// Container Apps scales on request volume between them, and a
			// floor below the requested count would mean a Specification
			// asking for three replicas usually running one.
			MinReplicas: pulumi.Int(p.EffectiveReplicas()),
			MaxReplicas: pulumi.Int(p.EffectiveReplicas()),
			Volumes:     shares.templateVolumes(),
			Containers: containerapp.AppTemplateContainerArray{
				&containerapp.AppTemplateContainerArgs{
					Name:         pulumi.String(id),
					Image:        pulumi.String(image),
					Cpu:          pulumi.Float64(cpu),
					Memory:       pulumi.String(memory),
					VolumeMounts: shares.volumeMounts(),
				},
			},
		},
		// A template volume names its storage link by name, not by an
		// output, so nothing in the arguments orders the two.
	}, pulumi.DependsOn(links))
	if err != nil {
		return nil, fmt.Errorf("azure: failed to declare the container service %q: %w", id, err)
	}

	if p.Domain != "" {
		if err := declareContainerDomain(ctx, id, p.Domain, app); err != nil {
			return nil, err
		}
	}

	// The schedule shares the app's Pulumi program, and so its stack,
	// which is what makes the existing Destroy path tear both down
	// (RFC 012 §4.3).
	if err := declareContainerSchedule(ctx, id, app, rg, rules); err != nil {
		return nil, err
	}
	return app, nil
}

// declareEnvironmentStorage links each of the scope's shares to the
// service's environment (RFC 020 §2.8). The environment is per-resource,
// so this is the one part of mounting that is a declaration rather than a
// lookup.
//
// The link authenticates with the account key, so the app's identity
// stays as RFC 017 §2.6 left it: without a single role.
func declareEnvironmentStorage(
	ctx *pulumi.Context,
	id string,
	environment *containerapp.Environment,
	shares scopeShares,
) ([]pulumi.Resource, error) {
	links := make([]pulumi.Resource, 0, len(shares.mounts))
	for _, m := range shares.mounts {
		link, err := containerapp.NewEnvironmentStorage(ctx, id+"-storage-"+m.share, &containerapp.EnvironmentStorageArgs{
			Name:                      pulumi.String(m.share),
			ContainerAppEnvironmentId: environment.ID(),
			AccountName:               pulumi.String(shares.account),
			ShareName:                 pulumi.String(m.share),
			AccessKey:                 pulumi.ToSecret(pulumi.String(shares.key)).(pulumi.StringOutput),
			AccessMode:                pulumi.String(storageAccessReadWrite),
		})
		if err != nil {
			return nil, fmt.Errorf("azure: failed to link the share for volume %q to %q: %w", m.volume.Name, id, err)
		}
		links = append(links, link)
	}
	return links, nil
}

// templateVolumes declares one AzureFile volume per mounted share, named
// after the storage link it reads from. Nil when nothing is mounted, so a
// service without volumes declares exactly what RFC 017 left it with.
func (s scopeShares) templateVolumes() containerapp.AppTemplateVolumeArray {
	var volumes containerapp.AppTemplateVolumeArray
	for _, m := range s.mounts {
		volumes = append(volumes, &containerapp.AppTemplateVolumeArgs{
			Name:        pulumi.String(m.share),
			StorageName: pulumi.String(m.share),
			StorageType: pulumi.String(storageTypeAzureFile),
		})
	}
	return volumes
}

// volumeMounts mounts each volume at the path the Specification asked for.
func (s scopeShares) volumeMounts() containerapp.AppTemplateContainerVolumeMountArray {
	var mounts containerapp.AppTemplateContainerVolumeMountArray
	for _, m := range s.mounts {
		mounts = append(mounts, &containerapp.AppTemplateContainerVolumeMountArgs{
			Name: pulumi.String(m.share),
			Path: pulumi.String(m.volume.MountPath),
		})
	}
	return mounts
}

// declareContainerDomain binds a custom domain to the app and proves
// control of it (RFC 017 §2.3.1).
//
// Azure issues the managed certificate only once both records resolve: a
// CNAME pointing the hostname at the app, and a TXT record at
// `asuid.<hostname>` carrying the app's verification id. Creating them
// here rather than printing them is the same choice the AWS provider makes
// for ACM — records a user must add by hand would make `apply` wait on an
// action CloudSDD cannot observe.
//
// As on AWS, this requires the zone to be hosted in the target account.
func declareContainerDomain(
	ctx *pulumi.Context,
	id, domain string,
	app *containerapp.App,
) error {
	zoneName, record := splitDomain(domain)

	zone, err := dns.LookupZone(ctx, &dns.LookupZoneArgs{Name: zoneName}, nil)
	if err != nil {
		return fmt.Errorf(
			"azure: no DNS zone %q found for domain %q. A custom domain's managed certificate is "+
				"issued only after its verification records resolve, which requires the zone to be "+
				"in this subscription: %w", zoneName, domain, err)
	}

	if _, err := dns.NewCNameRecord(ctx, id+"-domain-cname", &dns.CNameRecordArgs{
		Name:              pulumi.String(record),
		ZoneName:          pulumi.String(zone.Name),
		ResourceGroupName: pulumi.String(zone.ResourceGroupName),
		Ttl:               pulumi.Int(300),
		Record:            app.Ingress.Fqdn().Elem(),
	}); err != nil {
		return fmt.Errorf("azure: failed to declare the cname for %q: %w", domain, err)
	}

	verification, err := dns.NewTxtRecord(ctx, id+"-domain-verification", &dns.TxtRecordArgs{
		Name:              pulumi.String(domainVerificationPrefix + record),
		ZoneName:          pulumi.String(zone.Name),
		ResourceGroupName: pulumi.String(zone.ResourceGroupName),
		Ttl:               pulumi.Int(300),
		Records: dns.TxtRecordRecordArray{
			&dns.TxtRecordRecordArgs{Value: app.CustomDomainVerificationId},
		},
	})
	if err != nil {
		return fmt.Errorf("azure: failed to declare the verification record for %q: %w", domain, err)
	}

	// No certificate id: omitting it is what asks Azure for a managed
	// certificate. DependsOn the verification record because the binding
	// fails until it resolves, and nothing in the arguments expresses that
	// ordering.
	if _, err := containerapp.NewCustomDomain(ctx, id+"-domain", &containerapp.CustomDomainArgs{
		Name:                   pulumi.String(domain),
		ContainerAppId:         app.ID(),
		CertificateBindingType: pulumi.String(certificateBindingSNI),
	}, pulumi.DependsOn([]pulumi.Resource{verification})); err != nil {
		return fmt.Errorf("azure: failed to bind the custom domain %q: %w", domain, err)
	}
	return nil
}

// splitDomain separates a hostname into the zone it is served from and the
// record within it.
//
// The same one-label-up heuristic the AWS provider uses, and stated as a
// heuristic for the same reason: a subdomain delegated to its own zone
// would not be found, and the error names the zone that was looked for.
func splitDomain(domain string) (zone, record string) {
	labels := strings.Split(strings.TrimSuffix(domain, "."), ".")
	if len(labels) <= 2 {
		// Already an apex. Azure spells "the zone itself" as "@".
		return strings.Join(labels, "."), "@"
	}
	return strings.Join(labels[1:], "."), labels[0]
}
