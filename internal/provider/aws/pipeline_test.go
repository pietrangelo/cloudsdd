// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package aws

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider/container"
	"cloudsdd/internal/provider/pipeline"
	"cloudsdd/internal/spec"
)

const (
	ecrRepositoryToken    = "aws:ecr/repository:Repository"
	ecrLifecycleToken     = "aws:ecr/lifecyclePolicy:LifecyclePolicy"
	codeBuildProjectToken = "aws:codebuild/project:Project"
	getEcrRepositoryToken = "aws:ecr/getRepository:getRepository"
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

// TestAWSCodeBuildGeneration asserts the shape of the build identity: RFC
// 018 §2.8 gives it write access to one repository and nothing else,
// because a build role that can push anywhere can replace any image in the
// account.
func TestAWSCodeBuildGeneration(t *testing.T) {
	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		_, err := declareBuildPipeline(ctx, "api-build", mustDecodePipeline(t, pipelineProperties()), testResolved)
		return err
	})

	role := findResource(t, recorded, iamRoleToken)
	var trust struct {
		Statement []struct {
			Effect    string            `json:"Effect"`
			Principal map[string]string `json:"Principal"`
			Action    string            `json:"Action"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(role.Inputs["assumeRolePolicy"].StringValue()), &trust); err != nil {
		t.Fatalf("the trust policy is not JSON: %v", err)
	}
	if len(trust.Statement) != 1 || trust.Statement[0].Principal["Service"] != "codebuild.amazonaws.com" {
		t.Errorf("the role trusts %v, want only codebuild.amazonaws.com", trust.Statement)
	}

	policy := findResource(t, recorded, iamRolePolicyToken)
	var document struct {
		Statement []struct {
			Sid      string          `json:"Sid"`
			Effect   string          `json:"Effect"`
			Action   json.RawMessage `json:"Action"`
			Resource json.RawMessage `json:"Resource"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal([]byte(policy.Inputs["policy"].StringValue()), &document); err != nil {
		t.Fatalf("the build policy is not JSON: %v", err)
	}

	wantRepositoryARN := `"arn:aws:ecr:eu-central-1:` + testAccountID + `:repository/acme/api"`
	byID := map[string]string{}
	for _, statement := range document.Statement {
		if statement.Effect != "Allow" {
			t.Errorf("statement %q is %q, want Allow or nothing", statement.Sid, statement.Effect)
		}
		byID[statement.Sid] = string(statement.Resource)
	}

	// Every image action is scoped to this pipeline's one repository.
	if got := byID["PushToOwnRepositoryOnly"]; got != wantRepositoryARN {
		t.Errorf("the push statement is scoped to %s, want %s", got, wantRepositoryARN)
	}
	// The one wildcard is the login call, which AWS refuses to let a
	// policy scope and which grants nothing on its own.
	if got := byID["RegistryLogin"]; got != `"*"` {
		t.Errorf("the login statement is scoped to %s, want *", got)
	}
	for _, statement := range document.Statement {
		if statement.Sid == "RegistryLogin" {
			continue
		}
		if strings.Contains(string(statement.Resource), `"*"`) {
			t.Errorf("statement %q is unscoped: %s", statement.Sid, statement.Resource)
		}
	}
	// And nothing else at all: three statements, no fourth.
	if len(document.Statement) != 3 {
		t.Errorf("the build role carries %d statements, want 3", len(document.Statement))
	}
	// Nothing may grant a permission on a resource other than ECR and this
	// project's own logs.
	for _, statement := range document.Statement {
		actions := string(statement.Action)
		if !strings.Contains(actions, "ecr:") && !strings.Contains(actions, "logs:") {
			t.Errorf("statement %q grants something other than ECR or logs: %s", statement.Sid, actions)
		}
	}
}

// TestDeclareBuildPipelineRepository covers the registry defaults that are
// not configurable because they are the state of the art rather than a
// preference.
func TestDeclareBuildPipelineRepository(t *testing.T) {
	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		_, err := declareBuildPipeline(ctx, "api-build", mustDecodePipeline(t, pipelineProperties()), testResolved)
		return err
	})

	repository := findResource(t, recorded, ecrRepositoryToken)

	if got := repository.Inputs["name"].StringValue(); got != "acme/api" {
		t.Errorf("repository name = %q, want %q", got, "acme/api")
	}
	// A tag that could be repointed would be a commit SHA that no longer
	// means what it said (RFC 018 §2.4).
	if got := repository.Inputs["imageTagMutability"].StringValue(); got != ecrTagImmutable {
		t.Errorf("imageTagMutability = %q, want %q", got, ecrTagImmutable)
	}
	if !repository.Inputs["imageScanningConfiguration"].ObjectValue()["scanOnPush"].BoolValue() {
		t.Error("scanOnPush is off; a vulnerable base image would be invisible")
	}
	encryption := repository.Inputs["encryptionConfigurations"].ArrayValue()
	if len(encryption) == 0 {
		t.Fatal("the repository declares no encryption configuration")
	}
	if got := encryption[0].ObjectValue()["encryptionType"].StringValue(); got != ecrEncryption {
		t.Errorf("encryptionType = %q, want %q", got, ecrEncryption)
	}
}

// TestRetentionPolicy covers RFC 018 §2.5: the policy counts images rather
// than tags, and keeps what `retain` asks for.
func TestRetentionPolicy(t *testing.T) {
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
			recorded := runProgram(t, func(ctx *pulumi.Context) error {
				_, err := declareBuildPipeline(ctx, "api-build", mustDecodePipeline(t, tt.properties), testResolved)
				return err
			})

			lifecycle := findResource(t, recorded, ecrLifecycleToken)
			var document struct {
				Rules []struct {
					Selection struct {
						TagStatus   string `json:"tagStatus"`
						CountType   string `json:"countType"`
						CountNumber int    `json:"countNumber"`
					} `json:"selection"`
					Action struct {
						Type string `json:"type"`
					} `json:"action"`
				} `json:"rules"`
			}
			if err := json.Unmarshal([]byte(lifecycle.Inputs["policy"].StringValue()), &document); err != nil {
				t.Fatalf("the lifecycle policy is not JSON: %v", err)
			}
			if len(document.Rules) != 1 {
				t.Fatalf("the policy has %d rules, want 1", len(document.Rules))
			}

			rule := document.Rules[0]
			if rule.Selection.CountNumber != tt.want {
				t.Errorf("countNumber = %d, want %d", rule.Selection.CountNumber, tt.want)
			}
			// "any" is what makes this count images. A rule scoped to
			// tagged images would keep `retain` names and an unbounded
			// number of untagged manifests behind them.
			if rule.Selection.TagStatus != "any" {
				t.Errorf("tagStatus = %q, want %q: the rule must count images, not tags",
					rule.Selection.TagStatus, "any")
			}
			if rule.Selection.CountType != "imageCountMoreThan" {
				t.Errorf("countType = %q, want imageCountMoreThan", rule.Selection.CountType)
			}
			if rule.Action.Type != "expire" {
				t.Errorf("action = %q, want expire", rule.Action.Type)
			}
		})
	}
}

// TestDeclareBuildPipelineProject covers the build project itself, and the
// buildspec that carries the generated Dockerfile into it.
func TestDeclareBuildPipelineProject(t *testing.T) {
	recorded := runProgram(t, func(ctx *pulumi.Context) error {
		_, err := declareBuildPipeline(ctx, "api-build", mustDecodePipeline(t, pipelineProperties()), testResolved)
		return err
	})

	project := findResource(t, recorded, codeBuildProjectToken)

	source := project.Inputs["source"].ObjectValue()
	if got := source["type"].StringValue(); got != sourceTypeGitHub {
		t.Errorf("source type = %q, want %q", got, sourceTypeGitHub)
	}
	if got := source["location"].StringValue(); got != "https://github.com/acme/api" {
		t.Errorf("source location = %q, want the repository URL unchanged", got)
	}
	// The build checks out the commit the Engine resolved and showed, not
	// the branch that was written (RFC 018 §2.4.1). Building the branch
	// would build whatever it points at when CodeBuild gets there, which
	// may not be what the plan displayed.
	if got := project.Inputs["sourceVersion"].StringValue(); got != testCommit {
		t.Errorf("sourceVersion = %q, want the resolved commit %q", got, testCommit)
	}
	// Nothing is written to S3: the artifact of this build is the image.
	if got := project.Inputs["artifacts"].ObjectValue()["type"].StringValue(); got != artifactsNone {
		t.Errorf("artifacts type = %q, want %q", got, artifactsNone)
	}

	spec := source["buildspec"].StringValue()

	// The generated Dockerfile travels base64-encoded, so no layer of YAML
	// or shell escaping can quietly turn it into a different one.
	want, err := pipeline.NewDockerfileGenerator().Generate(pipeline.BuildSpec{
		Stack: pipeline.Stack{Runtime: pipeline.RuntimeGo, Version: "1.22"},
		Ports: []int{8080},
	})
	if err != nil {
		t.Fatalf("Generate() = %v, want nil", err)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(want))
	if !strings.Contains(spec, encoded) {
		t.Error("the buildspec does not carry the generated Dockerfile")
	}

	// The tag is the commit CodeBuild resolved, never `latest`.
	if !strings.Contains(spec, "CODEBUILD_RESOLVED_SOURCE_VERSION") {
		t.Error("the buildspec does not tag the image with the resolved commit")
	}
	if strings.Contains(spec, ":latest") {
		t.Error("the buildspec names the `latest` tag")
	}
	// It must not overwrite a Dockerfile the repository already has.
	if strings.Contains(spec, "> Dockerfile\n") {
		t.Error("the buildspec overwrites the repository's own Dockerfile")
	}
}

// TestBuildPipelineRefusesUnsupportedSources: CodeBuild's source types are
// named after specific hosts, so a host it has no type for is refused
// rather than approximated to one that would fail at clone time with a
// message about credentials.
func TestBuildPipelineRefusesUnsupportedSources(t *testing.T) {
	tests := []struct {
		name       string
		repository string
		want       string
		wantErr    error
	}{
		{name: "github", repository: "https://github.com/acme/api", want: sourceTypeGitHub},
		{name: "gitlab", repository: "https://gitlab.com/acme/api", want: sourceTypeGitLab},
		{name: "bitbucket", repository: "https://bitbucket.org/acme/api", want: sourceTypeBitbucket},
		{name: "case and www", repository: "https://WWW.GitHub.com/acme/api", want: sourceTypeGitHub},
		{
			name:       "a self-hosted forge",
			repository: "https://git.acme.example/acme/api",
			wantErr:    ErrUnsupportedSourceHost,
		},
		{
			// A host that merely contains a supported one is not it.
			name:       "a lookalike host",
			repository: "https://github.com.acme.example/acme/api",
			wantErr:    ErrUnsupportedSourceHost,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := codeBuildSourceType(tt.repository)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("codeBuildSourceType() = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("codeBuildSourceType() = %v, want nil", err)
			}
			if got != tt.want {
				t.Errorf("codeBuildSourceType() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDecodeBuildPipelineProperties covers what the provider refuses before
// anything is declared.
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
			name: "a host CodeBuild cannot clone",
			props: with(func(p map[string]any) {
				p["source"] = map[string]any{"repository": "https://git.acme.example/acme/api", "revision": "main"}
			}),
			wantErr: ErrUnsupportedSourceHost,
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
// `pipeline` reference becomes the image reference the task definition
// runs.
func TestPipelineImage(t *testing.T) {
	wantHost := testAccountID + ".dkr.ecr.eu-central-1.amazonaws.com/acme/api"

	t.Run("a resolved reference becomes a pinned image", func(t *testing.T) {
		var got string
		runProgram(t, func(ctx *pulumi.Context) error {
			image, err := pipelineImage(ctx, spec.Resource{ID: "api", Resolved: testResolved})
			got = image
			return err
		})

		if want := wantHost + ":" + testCommit; got != want {
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
					_, err := pipelineImage(ctx, spec.Resource{ID: "api", Resolved: tt.resolved})
					got = err
					return nil
				})
				// Guessing would mean inventing a repository name and a
				// tag, and the plausible inventions are respectively wrong
				// and the one thing RFC 017 §2.4 refuses outright.
				if !errors.Is(got, pipeline.ErrNotResolved) {
					t.Fatalf("pipelineImage() = %v, want %v", got, pipeline.ErrNotResolved)
				}
			})
		}
	})
}

// The revision CodeBuild checks out — the resolved commit, falling back to
// the revision as written when a provider is driven outside the Engine —
// is now spec.Resolved.CommitOr, covered by TestResolved_CommitOr in
// internal/spec (RFC 019 §2.2). What stays asserted here is that the
// project carries it: see TestDeclareBuildPipelineProject's sourceVersion.
