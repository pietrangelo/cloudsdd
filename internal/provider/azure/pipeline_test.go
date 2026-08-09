// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package azure

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider/container"
	"cloudsdd/internal/provider/pipeline"
	"cloudsdd/internal/spec"
)

const (
	acrRegistryToken     = "azure:containerservice/registry:Registry"
	acrTaskToken         = "azure:containerservice/registryTask:RegistryTask"
	getAcrRegistryToken  = "azure:containerservice/getRegistry:getRegistry"
	getClientConfigToken = "azure:core/getClientConfig:getClientConfig"
)

// testSubscriptionID is what the ambient credentials resolve to. It is
// part of the registry's name because an ACR name is drawn from a *global*
// namespace, unlike an ECR repository (account-scoped) or an Artifact
// Registry repository id (project-scoped).
const testSubscriptionID = "3f2504e0-4f89-11d3-9a0c-0305e82c3301"

// testCommit is the commit the Engine would have resolved `main` to.
const testCommit = "1c9e0aa5a5e14b6d34a3f9c1d0d0b1b9f0a1c2d3"

// testResolved is what the Engine hands a provider (RFC 018 §2.4.1).
var testResolved = &spec.Resolved{ImageName: "acme/api", Commit: testCommit}

// testACRLoginServer is what ACR computes from the registry's name.
//
// Derived rather than written out, because the mock derives it the same
// way: a literal here would let the name and the login server drift apart
// and still pass, which is exactly the disagreement between the two halves
// of §2.4.1 that these tests exist to catch.
var testACRLoginServer = acrRegistryName(testSubscriptionID, "acme/api") + acrLoginServerSuffix

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
		_, err := declareBuildPipeline(ctx, "api-build", "westeurope", mustDecodePipeline(t, props), testResolved)
		return err
	})
}

// findNamed locates a resource by type *and* name. The pipeline declares
// two RegistryTasks — the build and the purge — so findResource, which
// returns the first of a type, would silently answer about whichever was
// registered first.
func findNamed(t *testing.T, recorded []recordedResource, typeToken, name string) recordedResource {
	t.Helper()
	for _, r := range recorded {
		if r.Type == typeToken && r.Name == name {
			return r
		}
	}
	t.Fatalf("no %q resource named %q was declared; got %v", typeToken, name, typeTokens(recorded))
	return recordedResource{}
}

// taskScript is the ACR Task template a task runs, as text.
func taskScript(t *testing.T, task recordedResource) string {
	t.Helper()
	step, ok := task.Inputs["encodedStep"]
	if !ok || !step.IsObject() {
		t.Fatalf("task %q declares no encodedStep: %v", task.Name, task.Inputs)
	}
	content, ok := step.ObjectValue()["taskContent"]
	if !ok {
		t.Fatalf("task %q declares no taskContent: %v", task.Name, step)
	}
	return content.StringValue()
}

// TestAzureACRTasks is RFC 018 §2.8's Azure row, and §2.8.1's answer to
// what §7.2 left open.
//
// Two things are at stake. The first is the SKU: the plan originally said
// Premium, on the belief that ACR's native retention policy could keep
// `retain` images. It cannot — it is age-based and reaps only untagged
// manifests — so Premium would have cost roughly five times Basic for a
// policy that deletes close to nothing. A regression to Premium here is
// not a broken test, it is a bill.
//
// The second is the build identity. An ACR Task belongs to its registry
// and authenticates to that registry alone, so the authority is bounded
// structurally rather than by a policy document — which means the failure
// to watch for is not a too-broad policy but an *added* identity or role
// assignment that widens what the task can reach. This package declares
// both subscription-scoped role definitions and assignments for the power
// scheduler, so a copy-paste that granted the build one would compile and
// pass every other test in this file.
func TestAzureACRTasks(t *testing.T) {
	recorded := declaredPipeline(t, pipelineProperties())

	registry := findResource(t, recorded, acrRegistryToken)

	// Basic, never Premium (RFC 018 §2.8.1).
	if got := registry.Inputs["sku"].StringValue(); got != acrSKUBasic {
		t.Errorf("registry sku = %q, want %q: Premium buys a retention policy that reaps only untagged manifests",
			got, acrSKUBasic)
	}

	// The admin account is a static username/password pair on the registry
	// itself. Enabling it is how a registry credential ends up somewhere it
	// can be read, and nothing here needs it: the task authenticates as the
	// registry's own task identity, and Container Apps pulls with its
	// managed identity.
	if admin, ok := registry.Inputs["adminEnabled"]; ok && admin.BoolValue() {
		t.Error("adminEnabled = true; the ACR admin account is a static credential pair")
	}
	if anon, ok := registry.Inputs["anonymousPullEnabled"]; ok && anon.BoolValue() {
		t.Error("anonymousPullEnabled = true; the registry would serve images to anyone")
	}

	// Nothing subscription-wide, and nothing scoped anywhere at all: the
	// task's reach is the registry it lives in, and any grant declared here
	// could only widen that.
	for _, token := range []string{roleDefinitionToken, assignmentToken} {
		if hasResource(recorded, token) {
			t.Errorf("%s was declared; an ACR Task already reaches its own registry and nothing else, "+
				"so a grant here can only widen it", token)
		}
	}

	// Both tasks must live in this pipeline's own registry rather than
	// naming one by string.
	for _, name := range []string{"api-build", "api-build-purge"} {
		task := findNamed(t, recorded, acrTaskToken, name)
		if _, ok := task.Inputs["containerRegistryId"]; !ok {
			t.Errorf("task %q names no registry", name)
		}
		if _, ok := task.Inputs["registryCredential"]; ok {
			t.Errorf("task %q carries a registryCredential; the task's own identity is what pushes", name)
		}
	}
}

// TestDeclarePipelineRegistryName covers the one thing ACR does that
// neither ECR nor Artifact Registry does: a registry name is drawn from a
// namespace shared with every other Azure customer, and it admits letters
// and digits only.
//
// Both halves of RFC 018 §2.4.1 have to compute the same one — the
// pipeline that creates it and the service that looks it up — which is why
// the derivation is a function rather than an inline expression.
func TestDeclarePipelineRegistryName(t *testing.T) {
	tests := []struct {
		name      string
		imageName string
	}{
		{name: "a plain name", imageName: "api"},
		{name: "a namespaced name", imageName: "acme/api"},
		{name: "separators ACR would refuse", imageName: "acme.corp/api_v2"},
		{name: "a leading digit", imageName: "2048/game"},
		{name: "a name longer than the limit", imageName: strings.Repeat("abcde", 40)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := acrRegistryName(testSubscriptionID, tt.imageName)

			if len(got) < 5 || len(got) > acrRegistryNameMaxLength {
				t.Errorf("acrRegistryName(%q) = %q (%d chars), want 5..%d",
					tt.imageName, got, len(got), acrRegistryNameMaxLength)
			}
			for _, r := range got {
				if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
					t.Errorf("acrRegistryName(%q) = %q, which ACR would refuse: "+
						"only letters and digits are legal, not even hyphens", tt.imageName, got)
					break
				}
			}
		})
	}

	// Two pipelines must not collide, and neither must two subscriptions —
	// the second is what the global namespace makes possible.
	if acrRegistryName(testSubscriptionID, "acme/api") == acrRegistryName(testSubscriptionID, "acme/web") {
		t.Error("two image names in one subscription produced the same registry name")
	}
	other := "8f14e45f-ceea-467a-9c1a-3f2504e04f89"
	if acrRegistryName(testSubscriptionID, "acme/api") == acrRegistryName(other, "acme/api") {
		t.Error("two subscriptions produced the same registry name; an ACR name is globally unique")
	}

	// And the declared registry must use it, or the service's lookup asks
	// for a registry that does not exist.
	recorded := declaredPipeline(t, pipelineProperties())
	registry := findResource(t, recorded, acrRegistryToken)
	if got, want := registry.Inputs["name"].StringValue(), acrRegistryName(testSubscriptionID, "acme/api"); got != want {
		t.Errorf("registry name = %q, want the derived %q", got, want)
	}

	rg := findResource(t, recorded, rgToken)
	if got, want := rg.Inputs["name"].StringValue(), acrResourceGroupName("acme/api"); got != want {
		t.Errorf("resource group name = %q, want the derived %q; the service side "+
			"knows the image name and not the pipeline's resource id", got, want)
	}
}

// TestPipelineRetention covers RFC 018 §2.5 on ACR, which cannot express
// it natively: retention is a timer-triggered `acr purge --keep`, and the
// count it carries is the whole of the property's effect.
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
			purge := findNamed(t, recorded, acrTaskToken, "api-build-purge")
			script := taskScript(t, purge)

			if !strings.Contains(script, "--keep "+strconv.Itoa(tt.want)) {
				t.Errorf("the purge task does not keep %d images:\n%s", tt.want, script)
			}
			// Scoped to this pipeline's own repository. A filter across the
			// registry would be harmless today, when CloudSDD creates one
			// registry per pipeline, and would stop being harmless the moment
			// anything else is pushed there.
			if !strings.Contains(script, "acme/api") {
				t.Errorf("the purge task does not name this pipeline's repository:\n%s", script)
			}
			// Untagged manifests too: keeping `retain` tags while leaving the
			// manifests behind them is the storage bill the property exists
			// to bound (RFC 018 §2.5).
			if !strings.Contains(script, "--untagged") {
				t.Errorf("the purge task leaves untagged manifests behind:\n%s", script)
			}

			// A purge that never runs keeps nothing. The timer is what makes
			// this a retention policy rather than a command nobody invokes.
			triggers, ok := purge.Inputs["timerTriggers"]
			if !ok || len(triggers.ArrayValue()) == 0 {
				t.Fatal("the purge task has no timer trigger; it would never run")
			}
			if got := triggers.ArrayValue()[0].ObjectValue()["schedule"].StringValue(); got == "" {
				t.Error("the purge task's timer carries no schedule")
			}
		})
	}

	// The native retention policy is Premium-only and reaps only untagged
	// manifests, so declaring it would be paying for nothing (RFC 018
	// §2.8.1).
	recorded := declaredPipeline(t, pipelineProperties())
	registry := findResource(t, recorded, acrRegistryToken)
	if _, ok := registry.Inputs["retentionPolicy"]; ok {
		t.Error("a native retentionPolicy was declared; it is Premium-only and reaps only untagged manifests")
	}
}

// TestDeclareBuildPipelineTask covers the build itself: what it checks
// out, what it builds it with, and what it pushes.
func TestDeclareBuildPipelineTask(t *testing.T) {
	recorded := declaredPipeline(t, pipelineProperties())
	build := findNamed(t, recorded, acrTaskToken, "api-build")
	script := taskScript(t, build)

	// The build checks out the commit the Engine resolved and showed, not
	// the branch that was written (RFC 018 §2.4.1). A branch would build
	// whatever it points at when ACR gets there, which may not be what the
	// plan displayed.
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
	// escaping — YAML, then shell — can quietly turn it into a different
	// one, which is the single outcome RFC 018 §2.2 cannot tolerate.
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
	if !strings.Contains(script, pipeline.GeneratedDockerfileName) {
		t.Errorf("the build does not build the generated Dockerfile %q:\n%s", pipeline.GeneratedDockerfileName, script)
	}
	// It must not overwrite a Dockerfile the repository already has: the
	// difference between what the repository builds and what CloudSDD
	// builds has to stay visible.
	if strings.Contains(script, "> Dockerfile\n") || strings.HasSuffix(script, "> Dockerfile") {
		t.Error("the build overwrites the repository's own Dockerfile")
	}

	// The image is tagged and pushed with the commit, never `latest`.
	image := acrImageReference(testACRLoginServer, "acme/api", testCommit)
	if !strings.Contains(script, image) {
		t.Errorf("the build does not push %q:\n%s", image, script)
	}
	if strings.Contains(script, ":latest") {
		t.Errorf("the build pushes a `latest` tag:\n%s", script)
	}

	// No build arguments and no secret values. There is no property that
	// could carry one (RFC 018 §2.2), and a build argument is where a
	// secret gets baked into a layer — so the absence has to be a property
	// of the declaration rather than of the schema alone.
	step := build.Inputs["encodedStep"].ObjectValue()
	for _, field := range []string{"secretValues", "values", "valueContent"} {
		if v, ok := step[resource.PropertyKey(field)]; ok && !v.IsNull() {
			t.Errorf("the build declares %s; there is no property that could fill it", field)
		}
	}
	if _, ok := build.Inputs["dockerStep"]; ok {
		t.Error("the build uses a dockerStep, which can only build a Dockerfile the repository already has")
	}

	// A build with no bound is an unbounded invoice, and one that has not
	// finished by the timeout has failed in a way retrying will not fix.
	if timeout, ok := build.Inputs["timeoutInSeconds"]; !ok || timeout.NumberValue() <= 0 {
		t.Error("the build declares no timeout")
	}
	// Non-system tasks require a platform, and an unset one is rejected by
	// ARM after the user approved the plan.
	platform, ok := build.Inputs["platform"]
	if !ok {
		t.Fatal("the build declares no platform; ARM requires one for a non-system task")
	}
	if got := platform.ObjectValue()["os"].StringValue(); got != "Linux" {
		t.Errorf("platform.os = %q, want Linux", got)
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
			// As on GCP and unlike CodeBuild, the clone is a `git clone` in a
			// task step, so any host serving a public repository over HTTPS
			// works and refusing one would be an invented limit.
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
// `pipeline` reference becomes the image reference Container Apps runs.
func TestPipelineImage(t *testing.T) {
	t.Run("a resolved reference becomes a pinned image", func(t *testing.T) {
		var got string
		runProgram(t, func(ctx *pulumi.Context) error {
			image, err := pipelineImage(ctx, spec.Resource{ID: "api", Resolved: testResolved})
			got = image
			return err
		})

		want := acrImageReference(testACRLoginServer, "acme/api", testCommit)
		if got != want {
			t.Errorf("pipelineImage() = %q, want %q", got, want)
		}
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
		if ref.Registry != testACRLoginServer {
			t.Errorf("parsed registry = %q, want the ACR login server %q", ref.Registry, testACRLoginServer)
		}
	})

	t.Run("the lookup asks for the registry the pipeline created", func(t *testing.T) {
		recorded := runProgram(t, func(ctx *pulumi.Context) error {
			_, err := pipelineImage(ctx, spec.Resource{ID: "api", Resolved: testResolved})
			return err
		})

		// Both halves of §2.4.1 have to name the same registry, and the
		// service side knows only the image name — not the pipeline's
		// resource id — so both names must be derived from it.
		lookup := findResource(t, recorded, getAcrRegistryToken)
		if got, want := lookup.Inputs["name"].StringValue(), acrRegistryName(testSubscriptionID, "acme/api"); got != want {
			t.Errorf("looked up registry %q, want the derived %q", got, want)
		}
		if got, want := lookup.Inputs["resourceGroupName"].StringValue(), acrResourceGroupName("acme/api"); got != want {
			t.Errorf("looked up resource group %q, want the derived %q", got, want)
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
				// Guessing would mean inventing a registry and a tag, and the
				// plausible inventions are respectively wrong and the one
				// thing RFC 017 §2.4 refuses outright.
				if !errors.Is(got, pipeline.ErrNotResolved) {
					t.Fatalf("pipelineImage() = %v, want %v", got, pipeline.ErrNotResolved)
				}
			})
		}
	})
}

// The revision the ACR Task checks out — the resolved commit, falling back
// to the revision as written when a provider is driven outside the Engine —
// is now spec.Resolved.CommitOr, covered by TestResolved_CommitOr in
// internal/spec (RFC 019 §2.2). What stays asserted here is that the task
// carries it: see TestDeclareBuildPipelineTask, which requires the resolved
// commit in the script and refuses the branch that was written.
