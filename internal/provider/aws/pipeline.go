// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package aws

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/codebuild"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/ecr"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/iam"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider/pipeline"
	"cloudsdd/internal/spec"
)

// build_pipeline on AWS is CodeBuild pushing to ECR (RFC 018 §2.8).

// CodeBuild source types. CodeBuild has no generic "git over https" source,
// so the repository host decides the type and an unrecognised host is
// refused rather than guessed at.
const (
	sourceTypeGitHub    = "GITHUB"
	sourceTypeGitLab    = "GITLAB"
	sourceTypeBitbucket = "BITBUCKET"
)

// The build environment. A managed, ephemeral container per build: nothing
// persists between them, which is what bounds the blast radius of running
// a repository's own build (RFC 018 §4).
const (
	buildComputeType = "BUILD_GENERAL1_SMALL"
	buildImage       = "aws/codebuild/amazonlinux2-x86_64-standard:5.0"
	buildImageType   = "LINUX_CONTAINER"
	buildTimeout     = 30
	artifactsNone    = "NO_ARTIFACTS"
)

// ECR settings that are not configurable, because they are the state of the
// art rather than a preference (CLAUDE.md, RFC 018 §2.2).
const (
	// ecrEncryption is server-side encryption with an ECR-managed key. It
	// costs nothing and cannot be turned off here.
	ecrEncryption = "AES256"

	// ecrTagImmutable refuses to move a tag once written. Every build is
	// tagged with its commit SHA (RFC 018 §2.4), so a tag that could be
	// repointed would be a commit that no longer means what it said.
	ecrTagImmutable = "IMMUTABLE"
)

// ErrUnsupportedSourceHost indicates a repository host CodeBuild has no
// source type for.
//
// Refused rather than approximated: CodeBuild's source types are named
// after specific hosts, and picking the wrong one produces a build that
// fails at clone time with a message about credentials, which is a long way
// from the actual cause.
var ErrUnsupportedSourceHost = errors.New("aws: CodeBuild has no source type for this repository host")

// decodeBuildPipelineProperties decodes and validates Properties as a
// cloud-agnostic pipeline.BuildPipelineProperties through the shared strict
// decoder.
//
// The source-type check stays here rather than moving into the shared
// helper: it is a real CodeBuild constraint and not a rule of the schema,
// and GCP and Azure are right not to have it (RFC 019 §2.2).
func decodeBuildPipelineProperties(props map[string]any) (*pipeline.BuildPipelineProperties, error) {
	p, err := pipeline.DecodeAndValidate(props, dec.Properties, "aws")
	if err != nil {
		return nil, err
	}
	if _, err := codeBuildSourceType(p.Source.Repository); err != nil {
		return nil, err
	}
	return p, nil
}

// codeBuildSourceType maps a repository URL onto the CodeBuild source type
// that can clone it.
func codeBuildSourceType(repository string) (string, error) {
	u, err := url.Parse(repository)
	if err != nil {
		return "", fmt.Errorf("%w: %q", ErrUnsupportedSourceHost, repository)
	}
	switch host := strings.ToLower(u.Hostname()); host {
	case "github.com", "www.github.com":
		return sourceTypeGitHub, nil
	case "gitlab.com", "www.gitlab.com":
		return sourceTypeGitLab, nil
	case "bitbucket.org", "www.bitbucket.org":
		return sourceTypeBitbucket, nil
	default:
		return "", fmt.Errorf("%w: %q (supported: github.com, gitlab.com, bitbucket.org)",
			ErrUnsupportedSourceHost, host)
	}
}

// declareBuildPipeline registers the ECR repository, the retention policy,
// the build identity and the CodeBuild project (RFC 018 §2.8).
func declareBuildPipeline(
	ctx *pulumi.Context,
	id string,
	p pipeline.BuildPipelineProperties,
	resolved *spec.Resolved,
	opts ...pulumi.ResourceOption,
) (*codebuild.Project, error) {
	repository, err := declarePipelineRepository(ctx, id, p, opts...)
	if err != nil {
		return nil, err
	}

	role, err := declareBuildIdentity(ctx, id, repository, opts...)
	if err != nil {
		return nil, err
	}

	dockerfile, err := pipeline.NewDockerfileGenerator().Generate(pipeline.BuildSpec{
		Stack: p.Stack,
		Ports: p.Ports,
	})
	if err != nil {
		return nil, fmt.Errorf("aws: failed to generate the Dockerfile for %q: %w", id, err)
	}

	sourceType, err := codeBuildSourceType(p.Source.Repository)
	if err != nil {
		return nil, err
	}

	project, err := codebuild.NewProject(ctx, id, &codebuild.ProjectArgs{
		Name:         pulumi.String(id),
		Description:  pulumi.String(fmt.Sprintf("CloudSDD %s: builds %s from %s", id, p.ImageName, p.Source.Repository)),
		ServiceRole:  role.Arn,
		BuildTimeout: pulumi.Int(buildTimeout),
		Source: &codebuild.ProjectSourceArgs{
			Type:     pulumi.String(sourceType),
			Location: pulumi.String(p.Source.Repository),
			// One commit is all a build needs. It is also what keeps a
			// large repository's history out of an ephemeral build volume.
			GitCloneDepth: pulumi.Int(1),
			Buildspec:     repository.RepositoryUrl.ApplyT(func(registry string) string { return buildspec(registry, dockerfile) }).(pulumi.StringOutput),
		},
		// The commit the Engine resolved and showed, not the branch that
		// was written (RFC 018 §2.4.1). Building the branch would build
		// whatever it points at when CodeBuild gets there, which may not be
		// what the plan displayed.
		SourceVersion: pulumi.String(resolved.CommitOr(p.Source.Revision)),
		Environment: &codebuild.ProjectEnvironmentArgs{
			ComputeType: pulumi.String(buildComputeType),
			Image:       pulumi.String(buildImage),
			Type:        pulumi.String(buildImageType),
			// Building an image needs a Docker daemon. It is the one
			// privilege this project holds, and it is confined to the
			// managed, ephemeral build container.
			PrivilegedMode: pulumi.Bool(true),
		},
		// Nothing is written to S3: the artifact of this build is the image
		// in ECR, and an artifacts bucket would be a second copy of it with
		// its own lifecycle and its own permissions.
		Artifacts: &codebuild.ProjectArtifactsArgs{Type: pulumi.String(artifactsNone)},
		Tags:      pulumi.StringMap{"Name": pulumi.String(id), tagManagedBy: pulumi.String(managedByValue)},
	}, opts...)
	if err != nil {
		return nil, fmt.Errorf("aws: failed to declare the build project for %q: %w", id, err)
	}
	return project, nil
}

// declarePipelineRepository registers the ECR repository and its retention
// policy.
func declarePipelineRepository(
	ctx *pulumi.Context,
	id string,
	p pipeline.BuildPipelineProperties,
	opts ...pulumi.ResourceOption,
) (*ecr.Repository, error) {
	repository, err := ecr.NewRepository(ctx, id, &ecr.RepositoryArgs{
		Name:               pulumi.String(p.ImageName),
		ImageTagMutability: pulumi.String(ecrTagImmutable),
		EncryptionConfigurations: ecr.RepositoryEncryptionConfigurationArray{
			ecr.RepositoryEncryptionConfigurationArgs{EncryptionType: pulumi.String(ecrEncryption)},
		},
		// Scanning on push is what makes a vulnerable base image visible
		// without anyone having to ask for a scan.
		ImageScanningConfiguration: &ecr.RepositoryImageScanningConfigurationArgs{ScanOnPush: pulumi.Bool(true)},
		// A destroy that stops at "the repository is not empty" leaves the
		// user with infrastructure CloudSDD created and will not remove.
		// The images are CloudSDD's own output, and the request was to
		// destroy them.
		ForceDelete: pulumi.Bool(true),
		Tags:        pulumi.StringMap{"Name": pulumi.String(id), tagManagedBy: pulumi.String(managedByValue)},
	}, opts...)
	if err != nil {
		return nil, fmt.Errorf("aws: failed to declare the image repository for %q: %w", id, err)
	}

	policy, err := retentionPolicy(p.EffectiveRetain())
	if err != nil {
		return nil, fmt.Errorf("aws: failed to build the retention policy for %q: %w", id, err)
	}
	if _, err := ecr.NewLifecyclePolicy(ctx, id+"-retention", &ecr.LifecyclePolicyArgs{
		Repository: repository.Name,
		Policy:     pulumi.String(policy),
	}, opts...); err != nil {
		return nil, fmt.Errorf("aws: failed to declare the retention policy for %q: %w", id, err)
	}
	return repository, nil
}

// retentionPolicy builds the ECR lifecycle policy keeping retain images
// (RFC 018 §2.5).
//
// `tagStatus: any` is what makes this count *images* rather than tags. A
// rule scoped to tagged images would keep `retain` names and an unbounded
// number of untagged manifests behind them, which is the storage bill the
// property exists to bound.
//
// RFC 018 §2.5 also says the digest a live service references is excluded
// from the policy. **ECR cannot express that**: a lifecycle rule selects on
// tag status, tag prefix and age or count, and there is no way to name a
// digest or to ask what is running. What holds instead is weaker and worth
// stating plainly — ECR expires the *oldest* images beyond the count, and a
// service deployed from this pipeline runs the newest, so the running image
// survives unless a Specification pins a digest more than `retain` builds
// old. That case is real and it is not protected here.
func retentionPolicy(retain int) (string, error) {
	type selection struct {
		TagStatus   string `json:"tagStatus"`
		CountType   string `json:"countType"`
		CountNumber int    `json:"countNumber"`
	}
	type action struct {
		Type string `json:"type"`
	}
	type rule struct {
		RulePriority int       `json:"rulePriority"`
		Description  string    `json:"description"`
		Selection    selection `json:"selection"`
		Action       action    `json:"action"`
	}

	document := struct {
		Rules []rule `json:"rules"`
	}{
		Rules: []rule{{
			RulePriority: 1,
			Description:  fmt.Sprintf("CloudSDD: keep the %d most recent images", retain),
			Selection: selection{
				TagStatus:   "any",
				CountType:   "imageCountMoreThan",
				CountNumber: retain,
			},
			Action: action{Type: "expire"},
		}},
	}

	encoded, err := json.Marshal(document)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// declareBuildIdentity registers the role the build runs as.
//
// Write access to one repository and nothing else, following the RFC 017
// §2.6 precedent: a build role that can push anywhere is a build role that
// can replace any image in the account (RFC 018 §2.8).
func declareBuildIdentity(
	ctx *pulumi.Context,
	id string,
	repository *ecr.Repository,
	opts ...pulumi.ResourceOption,
) (*iam.Role, error) {
	assume, err := json.Marshal(map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{{
			"Effect":    "Allow",
			"Principal": map[string]any{"Service": "codebuild.amazonaws.com"},
			"Action":    "sts:AssumeRole",
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("aws: failed to build the trust policy for %q: %w", id, err)
	}

	role, err := iam.NewRole(ctx, id+"-build-role", &iam.RoleArgs{
		AssumeRolePolicy: pulumi.String(assume),
		Description:      pulumi.String(fmt.Sprintf("CloudSDD %s: pushes to one repository and writes its own logs", id)),
		Tags:             pulumi.StringMap{"Name": pulumi.String(id), tagManagedBy: pulumi.String(managedByValue)},
	}, opts...)
	if err != nil {
		return nil, fmt.Errorf("aws: failed to declare the build role for %q: %w", id, err)
	}

	policy := repository.Arn.ApplyT(func(arn string) (string, error) {
		return buildRolePolicy(id, arn)
	}).(pulumi.StringOutput)

	if _, err := iam.NewRolePolicy(ctx, id+"-build-policy", &iam.RolePolicyArgs{
		Role:   role.Name,
		Policy: policy,
	}, opts...); err != nil {
		return nil, fmt.Errorf("aws: failed to declare the build policy for %q: %w", id, err)
	}
	return role, nil
}

// buildRolePolicy is the inline policy attached to the build role.
//
// Three statements, and the first is the one that cannot be narrowed:
// ecr:GetAuthorizationToken is an account-level call and AWS rejects a
// policy that scopes it to a resource. It grants a login token to the
// account's registry and no permission to do anything with it — every
// action that touches an image is in the second statement, scoped to this
// pipeline's one repository ARN.
func buildRolePolicy(id, repositoryARN string) (string, error) {
	document := map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{
			{
				"Sid":      "RegistryLogin",
				"Effect":   "Allow",
				"Action":   "ecr:GetAuthorizationToken",
				"Resource": "*",
			},
			{
				"Sid":    "PushToOwnRepositoryOnly",
				"Effect": "Allow",
				"Action": []string{
					"ecr:BatchCheckLayerAvailability",
					"ecr:BatchGetImage",
					"ecr:CompleteLayerUpload",
					"ecr:GetDownloadUrlForLayer",
					"ecr:InitiateLayerUpload",
					"ecr:PutImage",
					"ecr:UploadLayerPart",
				},
				"Resource": repositoryARN,
			},
			{
				// A build that cannot write its own log is a build whose
				// failure is unreadable. Scoped to this project's log
				// group: CodeBuild names it after the project.
				"Sid":    "OwnBuildLogs",
				"Effect": "Allow",
				"Action": []string{
					"logs:CreateLogGroup",
					"logs:CreateLogStream",
					"logs:PutLogEvents",
				},
				"Resource": fmt.Sprintf("arn:aws:logs:*:*:log-group:/aws/codebuild/%s:*", id),
			},
		},
	}

	encoded, err := json.Marshal(document)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// buildspec is the build definition CodeBuild runs.
//
// The generated Dockerfile is carried base64-encoded rather than inlined.
// It is a multi-line document being embedded in a YAML string inside a
// shell command, and every layer of that would need its own escaping; one
// of them getting it wrong would not be a syntax error but a *different
// Dockerfile*, which is the one outcome §2.2 cannot tolerate.
//
// It is written to a file named for CloudSDD rather than to `Dockerfile`,
// so a repository that has one of its own is not overwritten and the
// difference between what the repository builds and what CloudSDD builds
// stays visible.
//
// CODEBUILD_RESOLVED_SOURCE_VERSION is the commit CodeBuild actually
// checked out. It is used rather than the requested revision because a
// branch resolves to a commit at clone time, and the tag has to name what
// was built (RFC 018 §2.4).
func buildspec(registry, dockerfile string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(dockerfile))

	return strings.Join([]string{
		`version: 0.2`,
		`phases:`,
		`  pre_build:`,
		`    commands:`,
		`      - aws ecr get-login-password --region "$AWS_REGION" | docker login --username AWS --password-stdin "` + registry + `"`,
		`      - COMMIT="$CODEBUILD_RESOLVED_SOURCE_VERSION"`,
		`      - test -n "$COMMIT"`,
		`  build:`,
		`    commands:`,
		`      - echo "` + encoded + `" | base64 -d > ` + pipeline.GeneratedDockerfileName,
		`      - docker build -f ` + pipeline.GeneratedDockerfileName + ` -t "` + registry + `:$COMMIT" .`,
		`  post_build:`,
		`    commands:`,
		`      - docker push "` + registry + `:$COMMIT"`,
		`      - echo "pushed ` + registry + `:$COMMIT"`,
		``,
	}, "\n")
}

// pipelineImage turns a resolved `pipeline` reference into the image
// reference the task definition runs (RFC 018 §2.4.1).
//
// The registry host is looked up rather than assembled from an account id.
// An ECR host is "<account>.dkr.ecr.<region>.amazonaws.com", and the
// account is only obtainable through an aws:getCallerIdentity invoke —
// which internal/provider/aws/schedule.go already went out of its way to
// avoid, because it has to be threaded through assumed-credential
// providers. Looking the repository up asks the question that actually
// matters instead, and answers it in one call.
//
// It also fails usefully. The repository exists by the time this runs —
// the Engine applies a pipeline before anything that consumes it (RFC 018
// §2.9) — so a lookup that misses means the two disagree, and saying so
// beats deploying a service pointed at a registry path that holds nothing.
func pipelineImage(ctx *pulumi.Context, r spec.Resource, opts ...pulumi.InvokeOption) (string, error) {
	if !r.Resolved.Complete() {
		return "", fmt.Errorf("aws: resource %q: %w", r.ID, pipeline.ErrNotResolved)
	}

	repository, err := ecr.LookupRepository(ctx, &ecr.LookupRepositoryArgs{
		Name: r.Resolved.ImageName,
	}, opts...)
	if err != nil {
		return "", fmt.Errorf("aws: resource %q: failed to look up the image repository %q: %w",
			r.ID, r.Resolved.ImageName, err)
	}

	// The commit, never a branch and never `latest`. The repository is
	// created with immutable tags, so this names one artifact forever.
	return repository.RepositoryUrl + ":" + r.Resolved.Commit, nil
}
