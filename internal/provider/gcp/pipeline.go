// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package gcp

import (
	"encoding/base64"
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/pulumi/pulumi-gcp/sdk/v7/go/gcp/artifactregistry"
	"github.com/pulumi/pulumi-gcp/sdk/v7/go/gcp/cloudbuild"
	"github.com/pulumi/pulumi-gcp/sdk/v7/go/gcp/serviceaccount"
	"github.com/pulumi/pulumi-gcp/sdk/v7/go/gcp/storage"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider/pipeline"
	"cloudsdd/internal/spec"
)

// build_pipeline on GCP is Cloud Build pushing to Artifact Registry (RFC
// 018 §2.8).

// Artifact Registry settings that are not configurable, because they are
// the state of the art rather than a preference (CLAUDE.md, RFC 018 §2.2).
const (
	// artifactRegistryFormat is the only format a container image can be
	// stored in.
	artifactRegistryFormat = "DOCKER"

	// artifactRegistryHostSuffix completes the regional registry host:
	// "europe-west1" becomes "europe-west1-docker.pkg.dev".
	artifactRegistryHostSuffix = "-docker.pkg.dev"

	// artifactRepositoryIDMaxLength is Artifact Registry's own limit on a
	// repository id.
	artifactRepositoryIDMaxLength = 63

	// artifactWriterRole permits pushing to one repository. It is the whole
	// of the build identity's authority (RFC 018 §2.8).
	artifactWriterRole = "roles/artifactregistry.writer"
)

// Cleanup policy vocabulary (RFC 018 §2.5).
const (
	cleanupActionKeep   = "KEEP"
	cleanupActionDelete = "DELETE"

	// cleanupTagStateAny is what makes the DELETE rule count *images*. A
	// rule scoped to TAGGED would keep `retain` names and an unbounded
	// number of untagged manifests behind them, which is the storage bill
	// the property exists to bound.
	cleanupTagStateAny = "ANY"

	cleanupPolicyKeepID   = "cloudsdd-keep-recent"
	cleanupPolicyDeleteID = "cloudsdd-delete-older"
)

// The build itself.
const (
	// loggingGCSOnly writes build logs to the pipeline's own bucket and
	// nowhere else. The alternative, Cloud Logging, needs
	// roles/logging.logWriter on the *project* — the one thing the GCP row
	// of RFC 018 §2.8 promises the build identity does not get.
	loggingGCSOnly = "GCS_ONLY"

	// logObjectAdminRole lets the build write, read and replace objects in
	// its own log bucket, and carries nothing about the bucket itself. It
	// is granted on the bucket, never on the project.
	logObjectAdminRole = "roles/storage.objectAdmin"

	// bucketNameMaxLength is Cloud Storage's limit on a bucket name in the
	// single-component form. Unlike every other name this provider builds
	// it is drawn from a *global* namespace, which is why the project id is
	// part of it.
	bucketNameMaxLength = 63

	// buildLogRetentionDays bounds what the log bucket accumulates. Build
	// logs are diagnostic output with a short useful life, and a bucket
	// that only ever grows is a bill nobody asked for.
	buildLogRetentionDays = 30

	// buildTimeout matches the AWS twin's 30 minutes. A build that has not
	// finished by then has failed in a way that retrying will not fix, and
	// an unbounded build is an unbounded invoice.
	buildTimeout = "1800s"

	// The official builder images. Both are maintained by Google and
	// referenced by tag, which is the only form Cloud Build accepts here.
	gitBuilderImage    = "gcr.io/cloud-builders/git"
	dockerBuilderImage = "gcr.io/cloud-builders/docker"
)

// decodeBuildPipelineProperties decodes and validates Properties as a
// cloud-agnostic pipeline.BuildPipelineProperties through the shared strict
// decoder.
//
// Unlike the AWS twin there is nothing to add to the shared helper. CodeBuild's
// source types are named after specific hosts, so an unrecognised one has to be
// refused there; here the clone is a `git clone` in a build step, any host
// serving a public repository over HTTPS works, and refusing one would be
// an invented limit.
func decodeBuildPipelineProperties(props map[string]any) (*pipeline.BuildPipelineProperties, error) {
	return pipeline.DecodeAndValidate(props, dec.Properties, "gcp")
}

// declareBuildPipeline registers the Artifact Registry repository and its
// cleanup policy, the identity the build runs as, the bucket its logs go
// to, and the Cloud Build trigger that produces the image (RFC 018 §2.8).
//
// The project is read back from the repository rather than configured. The
// provider resolves it from the ambient credentials, and reading it off a
// declared resource is how the two are guaranteed to agree — the approach
// declareContainerService already takes for a domain mapping's namespace.
func declareBuildPipeline(
	ctx *pulumi.Context,
	id, region string,
	p pipeline.BuildPipelineProperties,
	resolved *spec.Resolved,
) (*cloudbuild.Trigger, error) {
	repository, err := declarePipelineRepository(ctx, id, region, p)
	if err != nil {
		return nil, err
	}

	account, err := declareBuildIdentity(ctx, id, repository)
	if err != nil {
		return nil, err
	}
	member := pulumi.Sprintf("serviceAccount:%s", account.Email)

	logs, err := declareBuildLogsBucket(ctx, id, region, repository.Project, member)
	if err != nil {
		return nil, err
	}

	if _, err := artifactregistry.NewRepositoryIamMember(ctx, id+"-registry-writer",
		&artifactregistry.RepositoryIamMemberArgs{
			Location:   pulumi.String(region),
			Repository: repository.RepositoryId,
			Role:       pulumi.String(artifactWriterRole),
			Member:     member,
		}); err != nil {
		return nil, fmt.Errorf("gcp: failed to declare the registry grant for %q: %w", id, err)
	}

	dockerfile, err := pipeline.NewDockerfileGenerator().Generate(pipeline.BuildSpec{
		Stack: p.Stack,
		Ports: p.Ports,
	})
	if err != nil {
		return nil, fmt.Errorf("gcp: failed to generate the Dockerfile for %q: %w", id, err)
	}

	// The commit the Engine resolved and showed, not the branch that was
	// written (RFC 018 §2.4.1).
	revision := resolved.CommitOr(p.Source.Revision)
	image := repository.Project.ApplyT(func(project string) string {
		return artifactImageReference(region, project, p.ImageName, revision)
	}).(pulumi.StringOutput)

	trigger, err := cloudbuild.NewTrigger(ctx, id, &cloudbuild.TriggerArgs{
		Name:        pulumi.String(id),
		Location:    pulumi.String(region),
		Description: pulumi.String(fmt.Sprintf("CloudSDD %s: builds %s from %s", id, p.ImageName, p.Source.Repository)),
		// Without this the build falls back to the default Cloud Build
		// service account, which carries broad project roles: omitting the
		// identity is not a build that cannot act, it is a build that can
		// act across the project.
		ServiceAccount: pulumi.Sprintf("projects/%s/serviceAccounts/%s", repository.Project, account.Email),
		Build: &cloudbuild.TriggerBuildArgs{
			Steps:   buildSteps(p.Source.Repository, revision, dockerfile, image),
			Images:  pulumi.StringArray{image},
			Timeout: pulumi.String(buildTimeout),
			// The image is pushed by Cloud Build itself, from `images`,
			// using the scoped identity — so there is no registry login step
			// and no place for a credential to appear.
			LogsBucket: logs.Name.ApplyT(func(name string) string { return "gs://" + name }).(pulumi.StringOutput),
			Options: &cloudbuild.TriggerBuildOptionsArgs{
				Logging: pulumi.String(loggingGCSOnly),
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("gcp: failed to declare the build trigger for %q: %w", id, err)
	}
	return trigger, nil
}

// buildSteps is what Cloud Build runs: fetch the one revision, then build
// the generated Dockerfile against it.
//
// The clone is a build step rather than the trigger's own source. Cloud
// Build fetches source through a *repository connection*, which a public
// repository nobody has linked does not have — and a connection could not
// name a commit that a branch resolved to at plan time anyway (RFC 018
// §2.4.1).
//
// The generated Dockerfile is carried base64-encoded rather than inlined.
// It is a multi-line document being embedded in a shell command inside a
// JSON argument, and every layer of that would need its own escaping; one
// of them getting it wrong would not be a syntax error but a *different
// Dockerfile*, which is the one outcome §2.2 cannot tolerate.
func buildSteps(repository, revision, dockerfile string, image pulumi.StringOutput) cloudbuild.TriggerBuildStepArray {
	encoded := base64.StdEncoding.EncodeToString([]byte(dockerfile))

	fetch := strings.Join([]string{
		"set -eu",
		"git init --quiet .",
		"git remote add origin " + repository,
		// A single revision, which is what keeps a large repository's
		// history out of the build volume. `--depth 1` on an explicit
		// commit is the only form that fetches exactly what was approved.
		"git fetch --quiet --depth 1 origin " + revision,
		"git checkout --quiet FETCH_HEAD",
	}, "\n")

	build := pulumi.Sprintf(strings.Join([]string{
		"set -eu",
		"echo " + encoded + " | base64 -d > " + pipeline.GeneratedDockerfileName,
		"docker build -f " + pipeline.GeneratedDockerfileName + " -t %s .",
	}, "\n"), image)

	return cloudbuild.TriggerBuildStepArray{
		&cloudbuild.TriggerBuildStepArgs{
			Name:       pulumi.String(gitBuilderImage),
			Entrypoint: pulumi.String("bash"),
			Args:       pulumi.StringArray{pulumi.String("-c"), pulumi.String(fetch)},
		},
		&cloudbuild.TriggerBuildStepArgs{
			Name:       pulumi.String(dockerBuilderImage),
			Entrypoint: pulumi.String("bash"),
			Args:       pulumi.StringArray{pulumi.String("-c"), build},
		},
	}
}

// declarePipelineRepository registers the Artifact Registry repository and
// its cleanup policy.
func declarePipelineRepository(
	ctx *pulumi.Context,
	id, region string,
	p pipeline.BuildPipelineProperties,
) (*artifactregistry.Repository, error) {
	retain := p.EffectiveRetain()

	repository, err := artifactregistry.NewRepository(ctx, id, &artifactregistry.RepositoryArgs{
		RepositoryId: pulumi.String(artifactRepositoryID(p.ImageName)),
		Location:     pulumi.String(region),
		Format:       pulumi.String(artifactRegistryFormat),
		Description:  pulumi.String(fmt.Sprintf("CloudSDD %s: images built from %s", id, p.Source.Repository)),
		DockerConfig: &artifactregistry.RepositoryDockerConfigArgs{
			// Every build is tagged with its commit SHA (RFC 018 §2.4.1),
			// and that tag is the entire mechanism by which a service knows
			// what it runs. A tag that could be repointed would be a commit
			// that no longer means what it said.
			ImmutableTags: pulumi.Bool(true),
		},
		// Two policies, because KEEP alone keeps everything: Artifact
		// Registry only removes what a DELETE policy selects, and KEEP wins
		// where the two overlap (RFC 018 §2.5).
		//
		// RFC 018 §2.5 also says the image a live service references is
		// excluded from the policy. **Artifact Registry cannot express
		// that**: a cleanup policy selects on tag state, prefixes and age
		// or count, and there is no way to ask what is running. What holds
		// instead is weaker and worth stating plainly — the policy keeps
		// the most recent versions, and a service deployed from this
		// pipeline runs the newest, so the running image survives unless a
		// Specification pins a commit more than `retain` builds old. That
		// case is real and it is not protected here.
		CleanupPolicies: artifactregistry.RepositoryCleanupPolicyArray{
			&artifactregistry.RepositoryCleanupPolicyArgs{
				Id:     pulumi.String(cleanupPolicyKeepID),
				Action: pulumi.String(cleanupActionKeep),
				MostRecentVersions: &artifactregistry.RepositoryCleanupPolicyMostRecentVersionsArgs{
					KeepCount: pulumi.Int(retain),
				},
			},
			&artifactregistry.RepositoryCleanupPolicyArgs{
				Id:     pulumi.String(cleanupPolicyDeleteID),
				Action: pulumi.String(cleanupActionDelete),
				Condition: &artifactregistry.RepositoryCleanupPolicyConditionArgs{
					TagState: pulumi.String(cleanupTagStateAny),
				},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("gcp: failed to declare the image repository for %q: %w", id, err)
	}
	return repository, nil
}

// declareBuildIdentity registers the identity the build runs as: an account
// with **no project roles at all**, which every grant in this file then
// attaches to a single resource (RFC 018 §2.8).
func declareBuildIdentity(
	ctx *pulumi.Context,
	id string,
	repository *artifactregistry.Repository,
) (*serviceaccount.Account, error) {
	account, err := serviceaccount.NewAccount(ctx, id+"-build-sa", &serviceaccount.AccountArgs{
		AccountId:   pulumi.String(googleAccountID(id, "-build")),
		DisplayName: pulumi.String(fmt.Sprintf("CloudSDD %s build", id)),
		Description: pulumi.String("Runs the build; it may push to one repository and write its own logs."),
	}, pulumi.DependsOn([]pulumi.Resource{repository}))
	if err != nil {
		return nil, fmt.Errorf("gcp: failed to declare the build identity for %q: %w", id, err)
	}
	return account, nil
}

// declareBuildLogsBucket registers the bucket the build writes its logs to
// and the one grant that lets it, scoped to that bucket.
//
// A build that cannot write its own log is a build whose failure is
// unreadable. Cloud Logging would be the obvious place for it and is the
// reason this bucket exists at all: it needs roles/logging.logWriter on the
// project, and the GCP row of RFC 018 §2.8 promises no project role.
func declareBuildLogsBucket(
	ctx *pulumi.Context,
	id, region string,
	project pulumi.StringOutput,
	member pulumi.StringInput,
) (*storage.Bucket, error) {
	name := project.ApplyT(func(project string) string {
		return buildLogsBucketName(project, id)
	}).(pulumi.StringOutput)

	bucket, err := storage.NewBucket(ctx, id+"-build-logs", &storage.BucketArgs{
		Name:     name,
		Location: pulumi.String(region),
		// This bucket is CloudSDD's own output and holds the build's
		// diagnostic trail, which routinely names internal paths.
		PublicAccessPrevention:   pulumi.String("enforced"),
		UniformBucketLevelAccess: pulumi.Bool(true),
		// Unlike an object_storage bucket, whose contents are the user's
		// data (RFC 011 §2.5), these are CloudSDD's own build logs — and a
		// destroy that stops at "the bucket is not empty" leaves the user
		// with infrastructure CloudSDD created and will not remove.
		ForceDestroy: pulumi.Bool(true),
		LifecycleRules: storage.BucketLifecycleRuleArray{
			&storage.BucketLifecycleRuleArgs{
				Action:    &storage.BucketLifecycleRuleActionArgs{Type: pulumi.String("Delete")},
				Condition: &storage.BucketLifecycleRuleConditionArgs{Age: pulumi.Int(buildLogRetentionDays)},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("gcp: failed to declare the build log bucket for %q: %w", id, err)
	}

	if _, err := storage.NewBucketIAMMember(ctx, id+"-build-logs-writer", &storage.BucketIAMMemberArgs{
		Bucket: bucket.Name,
		Role:   pulumi.String(logObjectAdminRole),
		Member: member,
	}); err != nil {
		return nil, fmt.Errorf("gcp: failed to declare the log grant for %q: %w", id, err)
	}
	return bucket, nil
}

// artifactRepositoryID derives an Artifact Registry repository id from an
// image name.
//
// The two are not the same grammar: an OCI repository name admits slashes
// and dots, a repository id admits neither. The mapping has to be total —
// every name `image_name` accepts must produce an id Artifact Registry
// accepts — and both halves of RFC 018 §2.4.1 have to compute the same one,
// which is why the service side calls this function rather than
// reconstructing the name.
func artifactRepositoryID(imageName string) string {
	sanitized := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, imageName)

	sanitized = strings.Trim(sanitized, "-")
	// Must start with a letter, which a name like "2048/game" does not.
	if sanitized == "" || sanitized[0] < 'a' || sanitized[0] > 'z' {
		sanitized = "r" + sanitized
	}
	if len(sanitized) > artifactRepositoryIDMaxLength {
		sanitized = strings.TrimRight(sanitized[:artifactRepositoryIDMaxLength], "-")
	}
	return sanitized
}

// artifactImagePath is the image's name *within* its repository: the last
// component of the image name, since the rest of it named the repository.
func artifactImagePath(imageName string) string {
	if slash := strings.LastIndex(imageName, "/"); slash >= 0 {
		return imageName[slash+1:]
	}
	return imageName
}

// artifactImageReference assembles the reference a service runs (RFC 018
// §2.4.1): the regional registry host, the project, the repository, the
// image and the commit that named it.
func artifactImageReference(region, project, imageName, commit string) string {
	return fmt.Sprintf("%s%s/%s/%s/%s:%s",
		region, artifactRegistryHostSuffix,
		project,
		artifactRepositoryID(imageName),
		artifactImagePath(imageName),
		commit)
}

// buildLogsBucketName derives the log bucket's name from the project and
// the resource id.
//
// A bucket name is drawn from a namespace shared with every other Google
// Cloud customer, unlike every other name this provider builds, so the
// project id leads it and the length limit is a hard one. The digest is
// what survives truncation: two long resource ids in one project would
// otherwise produce the same name, and one build's failure would be written
// over another's.
func buildLogsBucketName(project, id string) string {
	const (
		projectBudget = 20
		idBudget      = 24
		suffix        = "-logs"
	)

	digest := fnv.New32a()
	// Hash.Write never returns an error.
	_, _ = digest.Write([]byte(id))

	name := fmt.Sprintf("%s-%s-%08x%s",
		bucketToken(project, projectBudget),
		bucketToken(id, idBudget),
		digest.Sum32(),
		suffix)

	// The project id leads, so a name that somehow starts with anything
	// other than a letter or a digit is a project id Cloud Storage would
	// already have refused — but the bucket is named here, not there.
	if name[0] < 'a' || name[0] > 'z' {
		name = "b" + name
	}
	return name
}

// bucketToken sanitizes one component of a bucket name to Cloud Storage's
// charset and caps it.
func bucketToken(s string, budget int) string {
	sanitized := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, s)

	if len(sanitized) > budget {
		sanitized = sanitized[:budget]
	}
	sanitized = strings.Trim(sanitized, "-")
	if sanitized == "" {
		sanitized = "x"
	}
	return sanitized
}

// pipelineImage turns a resolved `pipeline` reference into the image
// reference Cloud Run runs (RFC 018 §2.4.1).
//
// The project is looked up rather than configured, for the same reason
// declareBuildPipeline reads it off the repository: the provider resolves
// it from the ambient credentials, and asking the registry is how the
// answer is guaranteed to be the one the images were pushed under.
//
// It also fails usefully. The repository exists by the time this runs — the
// Engine applies a pipeline before anything that consumes it (RFC 018 §2.9)
// — so a lookup that misses means the two disagree, and saying so beats
// deploying a service pointed at a registry path that holds nothing.
func pipelineImage(ctx *pulumi.Context, r spec.Resource, region string, opts ...pulumi.InvokeOption) (string, error) {
	if !r.Resolved.Complete() {
		return "", fmt.Errorf("gcp: resource %q: %w", r.ID, pipeline.ErrNotResolved)
	}

	repositoryID := artifactRepositoryID(r.Resolved.ImageName)
	repository, err := artifactregistry.LookupRepository(ctx, &artifactregistry.LookupRepositoryArgs{
		Location:     region,
		RepositoryId: repositoryID,
	}, opts...)
	if err != nil {
		return "", fmt.Errorf("gcp: resource %q: failed to look up the image repository %q: %w",
			r.ID, repositoryID, err)
	}
	if repository.Project == nil || *repository.Project == "" {
		return "", fmt.Errorf("gcp: resource %q: the image repository %q named no project",
			r.ID, repositoryID)
	}

	// The commit, never a branch and never `latest`. The repository is
	// created with immutable tags, so this names one artifact forever.
	return artifactImageReference(region, *repository.Project, r.Resolved.ImageName, r.Resolved.Commit), nil
}
