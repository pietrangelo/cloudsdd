// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package azure

import (
	"encoding/base64"
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/pulumi/pulumi-azure/sdk/v5/go/azure/containerservice"
	"github.com/pulumi/pulumi-azure/sdk/v5/go/azure/core"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider/pipeline"
	"cloudsdd/internal/spec"
)

// build_pipeline on Azure is an ACR Task pushing to the Azure Container
// Registry it lives in (RFC 018 §2.8, §2.8.1).

// Registry settings that are not configurable, because they are the state
// of the art rather than a preference (CLAUDE.md, RFC 018 §2.2).
const (
	// acrSKUBasic is the tier every pipeline gets. RFC 018 §2.8.1 records
	// why the plan's original Premium did not survive: ACR's native
	// retention policy is Premium-only, age-based, and reaps *only untagged
	// manifests* — which a pipeline pushing one commit tag per build barely
	// produces. Premium would have cost roughly five times Basic for a
	// policy that deletes close to nothing. Retention is a purge task
	// instead (see declarePurgeTask).
	acrSKUBasic = "Basic"

	// acrLoginServerSuffix completes the registry host that ACR computes
	// from the registry's name. Both halves of RFC 018 §2.4.1 read the
	// login server back rather than assembling it, but the suffix is needed
	// to reason about the name's length budget.
	acrLoginServerSuffix = ".azurecr.io"

	// acrRegistryNameMaxLength is ACR's own limit. The floor is 5, and the
	// charset is letters and digits — not even hyphens.
	acrRegistryNameMaxLength = 50

	// acrRegistryNamePrefix marks the registry as CloudSDD's in a namespace
	// shared with every other Azure customer.
	acrRegistryNamePrefix = "cloudsdd"

	// acrRegistryNameDigestLength is the width of the hex digest that ends
	// a registry name, in characters.
	acrRegistryNameDigestLength = 16

	// acrResourceGroupPrefix leads the resource group both halves of RFC
	// 018 §2.4.1 derive from the image name.
	acrResourceGroupPrefix = "cloudsdd-"

	// acrResourceGroupTokenBudget caps the readable half of that name. A
	// resource group name may run to 90 characters, so this is a
	// legibility bound and not a hard limit.
	acrResourceGroupTokenBudget = 32
)

// The tasks themselves.
const (
	// acrTaskYAMLVersion is the ACR Tasks schema the task content is
	// written against.
	acrTaskYAMLVersion = "v1.1.0"

	// acrTaskPlatformOS is required for a non-system task. An unset
	// platform is rejected by ARM after the user approved the plan.
	acrTaskPlatformOS = "Linux"

	// acrTaskContextNone is what a task with no source context is given.
	// The build clones the repository itself, because the commit it must
	// build is the one the Engine resolved and showed (RFC 018 §2.4.1).
	acrTaskContextNone = "/dev/null"

	// gitImage is the image the checkout step runs.
	//
	// It is a dev-container base image, which needs explaining: MCR has no
	// small image carrying git. mcr.microsoft.com/acr/bash and
	// mcr.microsoft.com/acr/azure-cli ship none, and mcr.microsoft.com/acr/acb
	// carries git 2.15 from 2018. Docker Hub's alpine/git would be the
	// obvious answer and is the wrong one here: ACR Tasks agents pull from
	// shared Azure egress addresses, where Docker Hub's anonymous rate limit
	// is a build that fails for reasons the user cannot act on.
	//
	// So the choice is between an old git and a large image, and this
	// clones a repository CloudSDD does not control — which makes a
	// currently maintained git worth the pull.
	gitImage = "mcr.microsoft.com/devcontainers/base@sha256:03359a0274041de0ba5d4e667ef305678834799d2ffa2b0c49dd71356e33c7be"

	// acrCLIImage is ACR's own maintenance CLI, and the only thing that can
	// express RFC 018 §2.5 on this cloud.
	acrCLIImage = "mcr.microsoft.com/acr/acr-cli@sha256:684bec11aa0c4064d3663b624ba278e236e9abc8cfc3faf857aa214f7157a615"

	// acrBuildTimeoutSeconds matches the AWS and GCP twins' 30 minutes. A
	// build that has not finished by then has failed in a way retrying will
	// not fix, and an unbounded build is an unbounded invoice.
	acrBuildTimeoutSeconds = 1800

	// acrPurgeTimeoutSeconds is the bound on one retention sweep, following
	// the value Microsoft's own purge sample uses.
	acrPurgeTimeoutSeconds = 3600

	// acrPurgeSchedule runs the sweep nightly. Retention is a bound on what
	// accumulates, not a reaction to a build, so an hour nobody deploys in
	// is the right one.
	acrPurgeSchedule = "0 3 * * *"
)

// decodeBuildPipelineProperties decodes and validates Properties as a
// cloud-agnostic pipeline.BuildPipelineProperties through the shared strict
// decoder.
//
// As on GCP and unlike the AWS twin there is nothing to add to the shared
// helper. CodeBuild's source types are named after specific hosts, so an
// unrecognised one has to be refused there; here the clone is a `git clone`
// in a task step, any host serving a public repository over HTTPS works, and
// refusing one would be an invented limit.
func decodeBuildPipelineProperties(props map[string]any) (*pipeline.BuildPipelineProperties, error) {
	return pipeline.DecodeAndValidate(props, dec.Properties, "azure")
}

// declareBuildPipeline registers the resource group, the Container Registry,
// the task that builds the image and the task that keeps the registry
// bounded (RFC 018 §2.8, §2.8.1).
//
// There is no build identity and no role assignment anywhere in this file,
// and that is the design rather than an omission: an ACR Task belongs to its
// registry and authenticates to that registry alone, so its authority is
// bounded structurally. Anything granted here could only widen it.
func declareBuildPipeline(
	ctx *pulumi.Context,
	id, location string,
	p pipeline.BuildPipelineProperties,
	resolved *spec.Resolved,
) (*containerservice.RegistryTask, error) {
	// An ACR name is drawn from a namespace shared with every other Azure
	// customer, unlike an ECR repository (account-scoped) or an Artifact
	// Registry repository id (project-scoped), so the subscription is part
	// of the derivation — and the subscription comes from the ambient
	// credentials rather than from a property.
	config, err := core.GetClientConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("azure: failed to resolve the subscription for %q: %w", id, err)
	}

	rg, err := core.NewResourceGroup(ctx, id+"-rg", &core.ResourceGroupArgs{
		// Named from the image name rather than from the resource id: the
		// service side of RFC 018 §2.4.1 has to find this group again, and
		// the image name is all the Engine hands it.
		Name:     pulumi.String(acrResourceGroupName(p.ImageName)),
		Location: pulumi.String(location),
	})
	if err != nil {
		return nil, fmt.Errorf("azure: failed to declare the resource group for %q: %w", id, err)
	}

	registry, err := declarePipelineRegistry(ctx, id, location, config.SubscriptionId, p, rg)
	if err != nil {
		return nil, err
	}

	dockerfile, err := pipeline.NewDockerfileGenerator().Generate(pipeline.BuildSpec{
		Stack: p.Stack,
		Ports: p.Ports,
	})
	if err != nil {
		return nil, fmt.Errorf("azure: failed to generate the Dockerfile for %q: %w", id, err)
	}

	// The commit the Engine resolved and showed, not the branch that was
	// written (RFC 018 §2.4.1).
	revision := resolved.CommitOr(p.Source.Revision)

	build, err := declareBuildTask(ctx, id, p, registry, revision, dockerfile)
	if err != nil {
		return nil, err
	}
	if err := declarePurgeTask(ctx, id, p, registry); err != nil {
		return nil, err
	}
	return build, nil
}

// declarePipelineRegistry registers the Container Registry the pipeline
// pushes to.
func declarePipelineRegistry(
	ctx *pulumi.Context,
	id, location, subscriptionID string,
	p pipeline.BuildPipelineProperties,
	rg *core.ResourceGroup,
) (*containerservice.Registry, error) {
	registry, err := containerservice.NewRegistry(ctx, id, &containerservice.RegistryArgs{
		Name:              pulumi.String(acrRegistryName(subscriptionID, p.ImageName)),
		ResourceGroupName: rg.Name,
		Location:          rg.Location,
		Sku:               pulumi.String(acrSKUBasic),
		// The admin account is a static username/password pair on the
		// registry itself, and enabling it is how a registry credential ends
		// up somewhere it can be read. Nothing here needs it: the task
		// authenticates as the registry's own task identity, and Container
		// Apps pulls with its managed identity.
		AdminEnabled: pulumi.Bool(false),
		// Explicit rather than left to the default, because the default is
		// the one thing that would make every image in the registry world
		// readable if it ever changed.
		AnonymousPullEnabled: pulumi.Bool(false),
		// No RetentionPolicy: it is Premium-only and reaps only untagged
		// manifests (RFC 018 §2.8.1). declarePurgeTask is what honours
		// `retain` here.
		//
		// ACR also has no registry-wide immutable-tags setting — the ECR and
		// Artifact Registry twins both have one — so §2.4.1's guarantee that
		// a commit tag names one artifact forever is weaker on this cloud.
		// The RFC records that rather than papering over it.
	})
	if err != nil {
		return nil, fmt.Errorf("azure: failed to declare the image registry for %q: %w", id, err)
	}
	return registry, nil
}

// declareBuildTask registers the task that produces the image.
//
// An encoded step rather than a docker step: a docker step can only build a
// Dockerfile the repository already has, and RFC 018 §2.2's whole premise is
// that CloudSDD writes it.
func declareBuildTask(
	ctx *pulumi.Context,
	id string,
	p pipeline.BuildPipelineProperties,
	registry *containerservice.Registry,
	revision, dockerfile string,
) (*containerservice.RegistryTask, error) {
	imageName := p.ImageName
	repository := p.Source.Repository

	content := registry.LoginServer.ApplyT(func(host string) string {
		return buildTaskContent(repository, revision, dockerfile, acrImageReference(host, imageName, revision))
	}).(pulumi.StringOutput)

	task, err := containerservice.NewRegistryTask(ctx, id, &containerservice.RegistryTaskArgs{
		Name: pulumi.String(id),
		// The task lives in this pipeline's own registry, which is what
		// bounds what it can reach.
		ContainerRegistryId: registry.ID(),
		Platform: &containerservice.RegistryTaskPlatformArgs{
			Os: pulumi.String(acrTaskPlatformOS),
		},
		EncodedStep: &containerservice.RegistryTaskEncodedStepArgs{
			TaskContent: content,
			ContextPath: pulumi.String(acrTaskContextNone),
			// No Values, ValueContent or SecretValues. There is no property
			// that could fill them (RFC 018 §2.2), and a build argument is
			// where a secret gets baked into a layer.
		},
		TimeoutInSeconds: pulumi.Int(acrBuildTimeoutSeconds),
		// No RegistryCredential: an ACR Task authenticates to the registry it
		// belongs to as itself, so a credential here would be a second, wider
		// way in.
	})
	if err != nil {
		return nil, fmt.Errorf("azure: failed to declare the build task for %q: %w", id, err)
	}
	return task, nil
}

// declarePurgeTask registers the timer-triggered sweep that keeps `retain`
// images (RFC 018 §2.5, §2.8.1).
//
// This is the whole of the property's effect on Azure. ACR cannot express
// retention natively at this tier, so a task that never runs would leave
// `retain` decoded and unhonoured — which is why the timer is not optional.
//
// RFC 018 §2.5 also says the image a live service references is excluded.
// **acr purge cannot express that**: it selects on a repository, a tag
// regular expression and an age, and there is no way to ask what is running.
// What holds instead is weaker and worth stating plainly — the sweep keeps
// the most recent tags, and a service deployed from this pipeline runs the
// newest, so the running image survives unless a Specification pins a commit
// more than `retain` builds old. That case is real and it is not protected
// here.
func declarePurgeTask(
	ctx *pulumi.Context,
	id string,
	p pipeline.BuildPipelineProperties,
	registry *containerservice.Registry,
) error {
	imageName := p.ImageName
	retain := p.EffectiveRetain()

	content := registry.LoginServer.ApplyT(func(host string) string {
		return purgeTaskContent(host, imageName, retain)
	}).(pulumi.StringOutput)

	if _, err := containerservice.NewRegistryTask(ctx, id+"-purge", &containerservice.RegistryTaskArgs{
		Name:                pulumi.String(id + "-purge"),
		ContainerRegistryId: registry.ID(),
		Platform: &containerservice.RegistryTaskPlatformArgs{
			Os: pulumi.String(acrTaskPlatformOS),
		},
		EncodedStep: &containerservice.RegistryTaskEncodedStepArgs{
			TaskContent: content,
			ContextPath: pulumi.String(acrTaskContextNone),
		},
		TimeoutInSeconds: pulumi.Int(acrPurgeTimeoutSeconds),
		TimerTriggers: containerservice.RegistryTaskTimerTriggerArray{
			&containerservice.RegistryTaskTimerTriggerArgs{
				Name:     pulumi.String("retention"),
				Schedule: pulumi.String(acrPurgeSchedule),
			},
		},
	}); err != nil {
		return fmt.Errorf("azure: failed to declare the retention task for %q: %w", id, err)
	}
	return nil
}

// buildTaskContent is the ACR Tasks template the build runs: fetch the one
// revision, materialize the generated Dockerfile beside it, build, push.
//
// The clone is a task step rather than the task's source context. A context
// is fetched by ACR at run time from a branch or a tag, and this has to build
// the commit the plan displayed (RFC 018 §2.4.1) — which is also why the
// fetch names a commit and never the revision as written.
//
// The generated Dockerfile is carried base64-encoded rather than inlined. It
// is a multi-line document being embedded in a shell command inside a YAML
// scalar, and every layer of that would need its own escaping; one of them
// getting it wrong would not be a syntax error but a *different Dockerfile*,
// which is the one outcome RFC 018 §2.2 cannot tolerate.
func buildTaskContent(repository, revision, dockerfile, image string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(dockerfile))

	// A single revision, which is what keeps a large repository's history
	// out of the task volume. `--depth 1` on an explicit commit is the only
	// form that fetches exactly what was approved.
	fetch := strings.Join([]string{
		"set -eu",
		"git init --quiet .",
		"git remote add origin " + repository,
		"git fetch --quiet --depth 1 origin " + revision,
		"git checkout --quiet FETCH_HEAD",
	}, " && ")

	return strings.Join([]string{
		"version: " + acrTaskYAMLVersion,
		"steps:",
		"  - id: fetch",
		"    cmd: " + gitImage + " sh -c \"" + fetch + "\"",
		"  - id: dockerfile",
		"    cmd: " + gitImage + " sh -c \"echo " + encoded + " | base64 -d > " + pipeline.GeneratedDockerfileName + "\"",
		"  - id: build",
		"    build: -t " + image + " -f " + pipeline.GeneratedDockerfileName + " .",
		"  - id: push",
		"    push:",
		"      - " + image,
		"",
	}, "\n")
}

// purgeTaskContent is the ACR Tasks template the retention sweep runs.
//
// `--ago 0d` makes every tag a candidate and `--keep` is what spares the most
// recent ones, which together are `retain`. `--untagged` removes the
// manifests left behind: keeping `retain` tags while leaving the manifests
// behind them is the storage bill the property exists to bound.
//
// The filter names this pipeline's own repository. A filter across the
// registry would be harmless today, when CloudSDD creates one registry per
// pipeline, and would stop being harmless the moment anything else is pushed
// there.
func purgeTaskContent(loginServer, imageName string, retain int) string {
	purge := strings.Join([]string{
		acrCLIImage,
		"purge",
		"--registry", loginServer,
		"--filter", imageName + ":.*",
		"--ago", "0d",
		"--keep", fmt.Sprintf("%d", retain),
		"--untagged",
	}, " ")

	return strings.Join([]string{
		"version: " + acrTaskYAMLVersion,
		"steps:",
		"  - id: purge",
		"    cmd: " + purge,
		// The sweep has no source and nothing to do in a working directory,
		// which is the shape Microsoft's own purge sample uses.
		"    disableWorkingDirectoryOverride: true",
		fmt.Sprintf("    timeout: %d", acrPurgeTimeoutSeconds),
		"",
	}, "\n")
}

// acrRegistryName derives a Container Registry name from the subscription
// and the image name.
//
// ACR does what neither ECR nor Artifact Registry does: a registry name is
// drawn from a namespace shared with every other Azure customer, and it
// admits letters and digits only — not even hyphens. The mapping has to be
// total, so every name `image_name` accepts produces one ACR accepts, and
// both halves of RFC 018 §2.4.1 have to compute the same one, which is why
// this is a function rather than an inline expression.
//
// The digest carries the subscription as well as the image name. Two
// subscriptions deploying the same Specification must not name the same
// registry, and only the global namespace makes that possible. It is not a
// collision *proof* — a 64-bit digest in a namespace this large is a
// vanishingly unlikely clash that surfaces as a name already taken at apply
// time, which is a plan that fails rather than a registry that is shared.
func acrRegistryName(subscriptionID, imageName string) string {
	digest := fnv.New64a()
	// Hash.Write never returns an error. The separator keeps a subscription
	// ending where an image name begins from hashing as another pair.
	_, _ = digest.Write([]byte(subscriptionID))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(imageName))

	budget := acrRegistryNameMaxLength - len(acrRegistryNamePrefix) - acrRegistryNameDigestLength
	return fmt.Sprintf("%s%s%016x", acrRegistryNamePrefix, acrAlphanumericToken(imageName, budget), digest.Sum64())
}

// acrResourceGroupName derives the resource group the registry lives in.
//
// From the image name alone, and deliberately: the service side of RFC 018
// §2.4.1 has to look this group up knowing only what the Engine handed it,
// which is the image name and the commit — never the pipeline's resource id.
func acrResourceGroupName(imageName string) string {
	digest := fnv.New32a()
	_, _ = digest.Write([]byte(imageName))

	token := strings.Trim(acrGroupToken(imageName, acrResourceGroupTokenBudget), "-")
	if token == "" {
		return fmt.Sprintf("%s%08x", acrResourceGroupPrefix, digest.Sum32())
	}
	return fmt.Sprintf("%s%s-%08x", acrResourceGroupPrefix, token, digest.Sum32())
}

// acrAlphanumericToken reduces s to the only charset an ACR name accepts,
// capped at budget characters. Everything else is dropped rather than
// replaced, because there is no separator to replace it with.
func acrAlphanumericToken(s string, budget int) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= budget {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		}
	}
	return b.String()
}

// acrGroupToken sanitizes s for a resource group name, where a hyphen is
// legal and is what keeps two components apart.
func acrGroupToken(s string, budget int) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= budget {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// acrImageReference assembles the reference a service runs (RFC 018 §2.4.1):
// the registry's login server, the image name as its repository within that
// registry, and the commit that named it.
func acrImageReference(loginServer, imageName, commit string) string {
	return fmt.Sprintf("%s/%s:%s", loginServer, imageName, commit)
}

// pipelineImage turns a resolved `pipeline` reference into the image
// reference Container Apps runs (RFC 018 §2.4.1).
//
// The login server is looked up rather than assembled from the registry name.
// The two agree today — ACR appends .azurecr.io — and they do not in every
// sovereign cloud, so asking the registry is how the answer stays the one the
// images were pushed under.
//
// It also fails usefully. The registry exists by the time this runs — the
// Engine applies a pipeline before anything that consumes it (RFC 018 §2.9) —
// so a lookup that misses means the two disagree, and saying so beats
// deploying a service pointed at a registry path that holds nothing.
func pipelineImage(ctx *pulumi.Context, r spec.Resource, opts ...pulumi.InvokeOption) (string, error) {
	if !r.Resolved.Complete() {
		return "", fmt.Errorf("azure: resource %q: %w", r.ID, pipeline.ErrNotResolved)
	}

	config, err := core.GetClientConfig(ctx, opts...)
	if err != nil {
		return "", fmt.Errorf("azure: resource %q: failed to resolve the subscription: %w", r.ID, err)
	}

	name := acrRegistryName(config.SubscriptionId, r.Resolved.ImageName)
	registry, err := containerservice.LookupRegistry(ctx, &containerservice.LookupRegistryArgs{
		Name:              name,
		ResourceGroupName: acrResourceGroupName(r.Resolved.ImageName),
	}, opts...)
	if err != nil {
		return "", fmt.Errorf("azure: resource %q: failed to look up the image registry %q: %w", r.ID, name, err)
	}
	if registry.LoginServer == "" {
		return "", fmt.Errorf("azure: resource %q: the image registry %q reported no login server", r.ID, name)
	}

	// The commit, never a branch and never `latest`.
	return acrImageReference(registry.LoginServer, r.Resolved.ImageName, r.Resolved.Commit), nil
}
