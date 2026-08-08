// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package gcp

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider/container"
	"cloudsdd/internal/provider/pipeline"
	"cloudsdd/internal/spec"
)

const (
	artifactRepositoryToken     = "gcp:artifactregistry/repository:Repository"
	artifactRepositoryIamToken  = "gcp:artifactregistry/repositoryIamMember:RepositoryIamMember"
	cloudBuildTriggerToken      = "gcp:cloudbuild/trigger:Trigger"
	storageBucketToken          = "gcp:storage/bucket:Bucket"
	storageBucketIamMemberToken = "gcp:storage/bucketIAMMember:BucketIAMMember"
	getArtifactRepositoryToken  = "gcp:artifactregistry/getRepository:getRepository"
)

// testCommit is the commit the Engine would have resolved `main` to.
const testCommit = "1c9e0aa5a5e14b6d34a3f9c1d0d0b1b9f0a1c2d3"

// testResolved is what the Engine hands a provider (RFC 018 §2.4.1).
var testResolved = &spec.Resolved{ImageName: "acme/api", Commit: testCommit}

func pipelineProperties() map[string]any {
	return map[string]any{
		"source": map[string]any{
			"repository": "https://github.com/acme/api",
			"revision":   "main",
		},
		"stack":      map[string]any{"runtime": "go", "version": "1.22"},
		"image_name": "acme/api",
		"ports":      []any{8080},
	}
}

func mustDecodePipeline(t *testing.T, props map[string]any) pipeline.BuildPipelineProperties {
	t.Helper()
	p, err := decodeBuildPipelineProperties(props)
	if err != nil {
		t.Fatalf("decodeBuildPipelineProperties() = %v, want nil", err)
	}
	return *p
}

func declaredPipeline(t *testing.T, props map[string]any) []recordedResource {
	t.Helper()
	return runProgram(t, func(ctx *pulumi.Context) error {
		_, err := declareBuildPipeline(ctx, "api-build", "europe-west1", mustDecodePipeline(t, props), testResolved)
		return err
	})
}

// TestGCPCloudBuildGeneration is RFC 018 §2.8's GCP row: the build runs as
// a dedicated service account carrying **no project roles at all**.
//
// The stakes are higher here than on AWS. Omitting the service account
// would not leave the build with nothing — Cloud Build falls back to the
// default Cloud Build service account, which carries broad project roles,
// so a missing identity is a build that can act across the project rather
// than one that cannot act at all. And every grant that does exist has to
// be attached to a *resource*: this package declares project-level
// bindings for the power scheduler, so a copy-paste that granted the build
// one would compile and pass every other test in this file.
func TestGCPCloudBuildGeneration(t *testing.T) {
	recorded := declaredPipeline(t, pipelineProperties())

	account := findResource(t, recorded, serviceAccountToken)
	wantAccountID := googleAccountID("api-build", "-build")
	if got := account.Inputs["accountId"].StringValue(); got != wantAccountID {
		t.Errorf("accountId = %q, want the derived %q", got, wantAccountID)
	}

	// Nothing project-wide. Both tokens exist in this package already, for
	// the power scheduler.
	for _, token := range []string{iamMemberToken, customRoleToken} {
		if hasResource(recorded, token) {
			t.Errorf("%s was declared; the build identity must carry no project role", token)
		}
	}

	// And nothing project-wide under any other name either: every grant
	// this program makes must be attached to one of the two resources the
	// pipeline owns.
	scoped := map[string]bool{
		artifactRepositoryIamToken:  true,
		storageBucketIamMemberToken: true,
	}
	for _, r := range recorded {
		if strings.Contains(strings.ToLower(r.Type), "iam") && !scoped[r.Type] {
			t.Errorf("%s grants a permission that is not scoped to a resource the pipeline owns", r.Type)
		}
	}

	member := "serviceAccount:" + account.Inputs["accountId"].StringValue() +
		"@" + testProjectID + ".iam.gserviceaccount.com"

	// Write access to one repository and nothing else: a build identity
	// that can push anywhere is one that can replace any image in the
	// project (RFC 018 §2.8).
	writer := findResource(t, recorded, artifactRepositoryIamToken)
	if got := writer.Inputs["role"].StringValue(); got != artifactWriterRole {
		t.Errorf("registry role = %q, want %q", got, artifactWriterRole)
	}
	if got := writer.Inputs["member"].StringValue(); got != member {
		t.Errorf("registry grant member = %q, want the dedicated account %q", got, member)
	}
	if got := writer.Inputs["repository"].StringValue(); got != artifactRepositoryID("acme/api") {
		t.Errorf("registry grant is scoped to %q, want this pipeline's own repository %q",
			got, artifactRepositoryID("acme/api"))
	}

	// The only other grant is on the pipeline's own log bucket, which is
	// what buys the "no project roles" promise: the alternative, Cloud
	// Logging, needs roles/logging.logWriter on the project.
	logs := findResource(t, recorded, storageBucketToken)
	logGrant := findResource(t, recorded, storageBucketIamMemberToken)
	if got := logGrant.Inputs["member"].StringValue(); got != member {
		t.Errorf("log grant member = %q, want the dedicated account %q", got, member)
	}
	if got, want := logGrant.Inputs["bucket"].StringValue(), logs.Inputs["name"].StringValue(); got != want {
		t.Errorf("log grant is scoped to bucket %q, want this pipeline's own %q", got, want)
	}

	// The trigger must actually run as that identity. A trigger with no
	// service account is the failure this whole test exists to catch.
	trigger := findResource(t, recorded, cloudBuildTriggerToken)
	runAs := trigger.Inputs["serviceAccount"].StringValue()
	if !strings.Contains(runAs, wantAccountID) {
		t.Errorf("the trigger runs as %q, want the dedicated account", runAs)
	}
}

// TestDeclarePipelineRegistry covers the Artifact Registry defaults that
// are not configurable because they are the state of the art rather than a
// preference (RFC 018 §2.2, §2.4.1).
func TestDeclarePipelineRegistry(t *testing.T) {
	recorded := declaredPipeline(t, pipelineProperties())

	repository := findResource(t, recorded, artifactRepositoryToken)

	if got := repository.Inputs["repositoryId"].StringValue(); got != artifactRepositoryID("acme/api") {
		t.Errorf("repositoryId = %q, want %q", got, artifactRepositoryID("acme/api"))
	}
	if got := repository.Inputs["format"].StringValue(); got != artifactRegistryFormat {
		t.Errorf("format = %q, want %q", got, artifactRegistryFormat)
	}
	if got := repository.Inputs["location"].StringValue(); got != "europe-west1" {
		t.Errorf("location = %q, want the requested region", got)
	}
	// A tag that could be repointed would be a commit SHA that no longer
	// means what it said, and the commit tag is the entire mechanism by
	// which a service knows what it runs (RFC 018 §2.4.1).
	docker := repository.Inputs["dockerConfig"].ObjectValue()
	if !docker["immutableTags"].BoolValue() {
		t.Error("immutableTags is off; a commit tag could be repointed at a different image")
	}
}

// TestPipelineRetention covers RFC 018 §2.5 on Artifact Registry: the
// policy keeps `retain` *versions*, which is what makes it count images
// rather than tags.
func TestPipelineRetention(t *testing.T) {
	tests := []struct {
		name       string
		properties map[string]any
		want       int
	}{
		{name: "the default", properties: pipelineProperties(), want: pipeline.DefaultRetain},
		{
			name: "an explicit retain",
			properties: func() map[string]any {
				p := pipelineProperties()
				p["retain"] = 12
				return p
			}(),
			want: 12,
		},
		{
			name: "the floor",
			properties: func() map[string]any {
				p := pipelineProperties()
				p["retain"] = 1
				return p
			}(),
			want: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorded := declaredPipeline(t, tt.properties)
			repository := findResource(t, recorded, artifactRepositoryToken)

			policies := repository.Inputs["cleanupPolicies"].ArrayValue()
			if len(policies) != 2 {
				t.Fatalf("the repository declares %d cleanup policies, want 2 (one keep, one delete)", len(policies))
			}

			var keep, remove map[string]any
			for _, policy := range policies {
				p := policy.ObjectValue().Mappable()
				switch p["action"] {
				case cleanupActionKeep:
					keep = p
				case cleanupActionDelete:
					remove = p
				}
			}
			if keep == nil || remove == nil {
				t.Fatalf("want one KEEP and one DELETE policy, got %v", policies)
			}

			// A KEEP policy alone keeps everything: Artifact Registry only
			// removes what a DELETE policy selects, and KEEP wins where the
			// two overlap.
			recent, ok := keep["mostRecentVersions"].(map[string]any)
			if !ok {
				t.Fatalf("the keep policy has no mostRecentVersions: %v", keep)
			}
			if got := recent["keepCount"]; got != float64(tt.want) && got != tt.want {
				t.Errorf("keepCount = %v, want %d", got, tt.want)
			}

			// "any" is what makes this count *versions*. Scoped to tagged
			// versions it would keep `retain` names and an unbounded number
			// of untagged manifests behind them, which is the storage bill
			// the property exists to bound.
			condition, ok := remove["condition"].(map[string]any)
			if !ok {
				t.Fatalf("the delete policy has no condition: %v", remove)
			}
			if got := condition["tagState"]; got != cleanupTagStateAny {
				t.Errorf("tagState = %v, want %q: the rule must count images, not tags", got, cleanupTagStateAny)
			}
		})
	}
}

// TestDeclareBuildPipelineTrigger covers the build itself: what it checks
// out, what it builds it with, and what it pushes.
func TestDeclareBuildPipelineTrigger(t *testing.T) {
	recorded := declaredPipeline(t, pipelineProperties())
	trigger := findResource(t, recorded, cloudBuildTriggerToken)
	build := trigger.Inputs["build"].ObjectValue()

	steps := build["steps"].ArrayValue()
	if len(steps) == 0 {
		t.Fatal("the build declares no steps")
	}

	// Everything the steps say, flattened, so the assertions below can ask
	// what the build does rather than which index does it.
	var flattened []string
	for _, step := range steps {
		s := step.ObjectValue()
		line := s["name"].StringValue()
		if args, ok := s["args"]; ok {
			for _, a := range args.ArrayValue() {
				line += " " + a.StringValue()
			}
		}
		flattened = append(flattened, line)
	}
	script := strings.Join(flattened, "\n")

	// The build checks out the commit the Engine resolved and showed, not
	// the branch that was written (RFC 018 §2.4.1). Cloud Build's own
	// source fetch cannot name a commit for an unconnected public
	// repository, which is exactly why the clone is a step.
	if !strings.Contains(script, testCommit) {
		t.Errorf("no step checks out the resolved commit %q:\n%s", testCommit, script)
	}
	if strings.Contains(script, " main") {
		t.Errorf("a step names the branch rather than the commit it resolved to:\n%s", script)
	}
	if !strings.Contains(script, "https://github.com/acme/api") {
		t.Errorf("no step clones the repository:\n%s", script)
	}

	// The generated Dockerfile travels base64-encoded, so no layer of
	// shell escaping can quietly turn it into a different one.
	want, err := pipeline.NewDockerfileGenerator().Generate(pipeline.BuildSpec{
		Stack: pipeline.Stack{Runtime: pipeline.RuntimeGo, Version: "1.22"},
		Ports: []int{8080},
	})
	if err != nil {
		t.Fatalf("Generate() = %v, want nil", err)
	}
	if !strings.Contains(script, base64.StdEncoding.EncodeToString([]byte(want))) {
		t.Error("the build does not carry the generated Dockerfile")
	}
	// It must not overwrite a Dockerfile the repository already has.
	if strings.Contains(script, "> Dockerfile\n") || strings.HasSuffix(script, "> Dockerfile") {
		t.Error("the build overwrites the repository's own Dockerfile")
	}

	// The image is pushed by Cloud Build itself, from `images`, using the
	// scoped identity — so there is no registry login step and no place for
	// a credential to appear.
	images := build["images"].ArrayValue()
	if len(images) != 1 {
		t.Fatalf("the build pushes %d images, want exactly 1", len(images))
	}
	image := images[0].StringValue()
	wantImage := artifactImageReference("europe-west1", testProjectID, "acme/api", testCommit)
	if image != wantImage {
		t.Errorf("pushed image = %q, want %q", image, wantImage)
	}
	if strings.HasSuffix(image, ":latest") {
		t.Error("the build pushes a `latest` tag")
	}
	if !strings.Contains(script, image) {
		t.Error("the build tags no image with what it pushes")
	}

	// Logs go to the pipeline's own bucket. Cloud Logging would need
	// roles/logging.logWriter on the *project*, which is the one thing the
	// GCP row of RFC 018 §2.8 promises the build identity does not get.
	options := build["options"].ObjectValue()
	if got := options["logging"].StringValue(); got != loggingGCSOnly {
		t.Errorf("logging = %q, want %q", got, loggingGCSOnly)
	}
	logs := findResource(t, recorded, storageBucketToken)
	if got := build["logsBucket"].StringValue(); !strings.Contains(got, logs.Inputs["name"].StringValue()) {
		t.Errorf("logsBucket = %q, want the pipeline's own bucket %q", got, logs.Inputs["name"].StringValue())
	}
	// The log bucket is CloudSDD's own output and it must not be reachable
	// from outside, nor stall a destroy.
	if got := logs.Inputs["publicAccessPrevention"].StringValue(); got != "enforced" {
		t.Errorf("log bucket publicAccessPrevention = %q, want %q", got, "enforced")
	}
	if !logs.Inputs["uniformBucketLevelAccess"].BoolValue() {
		t.Error("log bucket uniformBucketLevelAccess = false, want true")
	}
	if !logs.Inputs["forceDestroy"].BoolValue() {
		t.Error("log bucket forceDestroy = false; a destroy would stall on CloudSDD's own build logs")
	}
}

// TestDecodeBuildPipelineProperties covers what the provider refuses
// before anything is declared.
func TestDecodeBuildPipelineProperties(t *testing.T) {
	with := func(mutate func(map[string]any)) map[string]any {
		p := pipelineProperties()
		mutate(p)
		return p
	}

	tests := []struct {
		name    string
		props   map[string]any
		wantErr error
		wantMsg string
	}{
		{name: "the valid case", props: pipelineProperties()},
		{
			// Unlike CodeBuild, whose source types are named after specific
			// hosts, the clone here is a `git clone` in a build step. Any
			// host that serves a public repository over HTTPS works, and
			// refusing one would be an invented limit.
			name: "a self-hosted forge",
			props: with(func(p map[string]any) {
				p["source"] = map[string]any{"repository": "https://git.acme.example/acme/api", "revision": "v1.2.3"}
			}),
		},
		{
			name:    "a runtime with no pinned base image",
			props:   with(func(p map[string]any) { p["stack"] = map[string]any{"runtime": "go", "version": "1.19"} }),
			wantErr: pipeline.ErrUnsupportedVersion,
		},
		{
			name:    "a runtime outside the enum",
			props:   with(func(p map[string]any) { p["stack"] = map[string]any{"runtime": "rust", "version": "1.80"} }),
			wantMsg: "property validation failed",
		},
		{
			name: "an http repository",
			props: with(func(p map[string]any) {
				p["source"] = map[string]any{"repository": "http://github.com/acme/api", "revision": "main"}
			}),
			wantErr: pipeline.ErrRepositoryScheme,
		},
		{
			// The credential this schema has no property for, smuggled
			// through the one string that accepts arbitrary text.
			name: "a token in the repository URL",
			props: with(func(p map[string]any) {
				p["source"] = map[string]any{"repository": "https://x:ghp_secret@github.com/acme/api", "revision": "main"}
			}),
			wantErr: pipeline.ErrRepositoryCredential,
		},
		{
			name:    "an image name that is a full reference",
			props:   with(func(p map[string]any) { p["image_name"] = "ghcr.io/acme/api" }),
			wantErr: pipeline.ErrImageNameMalformed,
		},
		{
			name:    "retain above the ceiling",
			props:   with(func(p map[string]any) { p["retain"] = pipeline.MaxRetain + 1 }),
			wantMsg: "property validation failed",
		},
		{
			name:    "retain below the floor",
			props:   with(func(p map[string]any) { p["retain"] = 0 }),
			wantMsg: "property validation failed",
		},
		{
			// Mass Assignment: a property the provider does not understand
			// means the user asked for something they will not get.
			name:    "an unknown property",
			props:   with(func(p map[string]any) { p["dockerfile"] = "FROM scratch" }),
			wantMsg: "unknown or malformed property",
		},
		{
			// There is no property for a build argument, and a build
			// argument is where a secret gets baked into a layer.
			name:    "a build argument",
			props:   with(func(p map[string]any) { p["build_args"] = map[string]any{"VERSION": "1"} }),
			wantMsg: "unknown or malformed property",
		},
		{
			name:    "a credential-shaped property",
			props:   with(func(p map[string]any) { p["registry_password"] = "hunter2" }),
			wantMsg: "looks like a credential",
		},
		{
			name:    "a port outside the range",
			props:   with(func(p map[string]any) { p["ports"] = []any{70000} }),
			wantMsg: "property validation failed",
		},
		{
			name:    "duplicate ports",
			props:   with(func(p map[string]any) { p["ports"] = []any{8080, 8080} }),
			wantMsg: "property validation failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeBuildPipelineProperties(tt.props)

			if tt.wantErr == nil && tt.wantMsg == "" {
				if err != nil {
					t.Fatalf("decodeBuildPipelineProperties() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("decodeBuildPipelineProperties() = nil, want an error")
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

// TestPipelineImage covers the service side of RFC 018 §2.4.1: a
// `pipeline` reference becomes the image reference Cloud Run runs.
func TestPipelineImage(t *testing.T) {
	t.Run("a resolved reference becomes a pinned image", func(t *testing.T) {
		var got string
		runProgram(t, func(ctx *pulumi.Context) error {
			image, err := pipelineImage(ctx, spec.Resource{ID: "api", Resolved: testResolved}, "europe-west1")
			got = image
			return err
		})

		want := artifactImageReference("europe-west1", testProjectID, "acme/api", testCommit)
		if got != want {
			t.Errorf("pipelineImage() = %q, want %q", got, want)
		}
		// The registry is created with immutable tags, so a commit tag
		// names one artifact forever — and it is never `latest`.
		if strings.HasSuffix(got, ":latest") {
			t.Error("pipelineImage() produced a `latest` reference")
		}
		// The image the service runs must parse under RFC 017 §2.4's own
		// grammar, or the two halves disagree about what an image is.
		ref, err := container.ParseImage(got)
		if err != nil {
			t.Fatalf("the assembled reference does not parse: %v", err)
		}
		if ref.Tag != testCommit {
			t.Errorf("parsed tag = %q, want the commit %q", ref.Tag, testCommit)
		}
		if ref.Registry != "europe-west1-docker.pkg.dev" {
			t.Errorf("parsed registry = %q, want the regional Artifact Registry host", ref.Registry)
		}
	})

	t.Run("an unresolved reference is refused", func(t *testing.T) {
		unresolved := []struct {
			name     string
			resolved *spec.Resolved
		}{
			{name: "nothing at all", resolved: nil},
			{name: "no commit", resolved: &spec.Resolved{ImageName: "acme/api"}},
			{name: "no image name", resolved: &spec.Resolved{Commit: testCommit}},
		}

		for _, tt := range unresolved {
			t.Run(tt.name, func(t *testing.T) {
				var got error
				runProgram(t, func(ctx *pulumi.Context) error {
					_, err := pipelineImage(ctx, spec.Resource{ID: "api", Resolved: tt.resolved}, "europe-west1")
					got = err
					return nil
				})
				// Guessing would mean inventing a repository and a tag, and
				// the plausible inventions are respectively wrong and the
				// one thing RFC 017 §2.4 refuses outright.
				if !errors.Is(got, ErrPipelineNotResolved) {
					t.Fatalf("pipelineImage() = %v, want %v", got, ErrPipelineNotResolved)
				}
			})
		}
	})
}

// TestArtifactRepositoryID: an Artifact Registry repository id is not an
// OCI repository name — it admits no slashes and no dots — so the mapping
// has to be total, and both halves of §2.4.1 have to compute the same one.
func TestArtifactRepositoryID(t *testing.T) {
	tests := []struct {
		name      string
		imageName string
		want      string
	}{
		{name: "a plain name", imageName: "api", want: "api"},
		{name: "a namespaced name", imageName: "acme/api", want: "acme-api"},
		{name: "separators that are not legal here", imageName: "acme.corp/api_v2", want: "acme-corp-api-v2"},
		{name: "a leading digit", imageName: "2048/game", want: "r2048-game"},
		{
			name:      "a name longer than the limit",
			imageName: strings.Repeat("abcde", 20),
			want:      "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := artifactRepositoryID(tt.imageName)

			if tt.want != "" && got != tt.want {
				t.Errorf("artifactRepositoryID(%q) = %q, want %q", tt.imageName, got, tt.want)
			}
			if len(got) == 0 || len(got) > artifactRepositoryIDMaxLength {
				t.Errorf("artifactRepositoryID(%q) = %q, want 1..%d characters",
					tt.imageName, got, artifactRepositoryIDMaxLength)
			}
			if got[0] < 'a' || got[0] > 'z' {
				t.Errorf("artifactRepositoryID(%q) = %q, want it to start with a letter", tt.imageName, got)
			}
			for _, r := range got {
				if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
					t.Errorf("artifactRepositoryID(%q) = %q, which Artifact Registry would refuse", tt.imageName, got)
					break
				}
			}
		})
	}
}

// TestBuildLogsBucketName: a Cloud Storage bucket name is *globally*
// unique, unlike every other name this provider builds, so the project id
// is part of it and the length limit is a hard one.
func TestBuildLogsBucketName(t *testing.T) {
	tests := []struct {
		name    string
		project string
		id      string
	}{
		{name: "the ordinary case", project: testProjectID, id: "api-build"},
		{name: "an id needing sanitation", project: testProjectID, id: "API_Build/2"},
		{name: "a long project and a long id", project: strings.Repeat("p", 30), id: strings.Repeat("i", 60)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildLogsBucketName(tt.project, tt.id)

			if len(got) < 3 || len(got) > bucketNameMaxLength {
				t.Errorf("buildLogsBucketName() = %q (%d chars), want 3..%d",
					got, len(got), bucketNameMaxLength)
			}
			for _, r := range got {
				if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
					t.Errorf("buildLogsBucketName() = %q, which Cloud Storage would refuse", got)
					break
				}
			}
			// The project id is what makes the name unique in a global
			// namespace shared with every other Google Cloud customer.
			if !strings.HasPrefix(got, tt.project[:min(len(tt.project), 8)]) {
				t.Errorf("buildLogsBucketName() = %q, want it to carry the project id", got)
			}
		})
	}

	// Two pipelines in one project must not share a log bucket, or one
	// build's failure is written over another's.
	if buildLogsBucketName(testProjectID, "api-build") == buildLogsBucketName(testProjectID, "web-build") {
		t.Error("two pipelines in one project produced the same log bucket name")
	}
}

// TestBuildRevision: driven through the Engine a commit is always present,
// and driving the provider directly must still work.
func TestBuildRevision(t *testing.T) {
	props := pipeline.BuildPipelineProperties{
		Source: pipeline.Source{Repository: "https://github.com/acme/api", Revision: "main"},
	}

	tests := []struct {
		name     string
		resolved *spec.Resolved
		want     string
	}{
		{name: "the resolved commit wins", resolved: testResolved, want: testCommit},
		{name: "no resolution falls back to the revision", resolved: nil, want: "main"},
		{name: "an empty commit falls back", resolved: &spec.Resolved{ImageName: "acme/api"}, want: "main"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildRevision(props, tt.resolved); got != tt.want {
				t.Errorf("buildRevision() = %q, want %q", got, tt.want)
			}
		})
	}
}
