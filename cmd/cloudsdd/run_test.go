// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"cloudsdd/internal/config"
	"cloudsdd/internal/nlp"
	"cloudsdd/internal/provider"
	"cloudsdd/internal/spec"
	"cloudsdd/internal/state"
)

// --- fakes -------------------------------------------------------------

type fakeTranslator struct {
	spec spec.Specification
	err  error

	gotPrompt  string
	gotContext string
}

func (f *fakeTranslator) Translate(ctx context.Context, prompt, contextJSON string) (spec.Specification, error) {
	f.gotPrompt = prompt
	f.gotContext = contextJSON
	return f.spec, f.err
}

type fakeProvider struct {
	name string

	planAction provider.Action
	applyErr   error
	destroyErr error

	applied   []string
	destroyed []string

	// networkScopes records EnsureNetwork calls (RFC 016 §2.2), and
	// destroyedNetworks the scopes reaped after a destroy (§2.6).
	networkScopes     []provider.NetworkScope
	destroyedNetworks []provider.NetworkScope
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) Validate(ctx context.Context, r spec.Resource, p spec.Policies) error {
	return nil
}

func (f *fakeProvider) Plan(ctx context.Context, r spec.Resource, p spec.Policies) (provider.Diff, error) {
	action := f.planAction
	if action == "" {
		action = provider.ActionCreate
	}
	return provider.Diff{ResourceID: r.ID, Region: r.Scope.Region, Action: action}, nil
}

func (f *fakeProvider) Apply(ctx context.Context, r spec.Resource, p spec.Policies) (provider.Result, error) {
	if f.applyErr != nil {
		return provider.Result{ResourceID: r.ID, Region: r.Scope.Region, Status: provider.StatusFailed}, f.applyErr
	}
	f.applied = append(f.applied, r.ID)
	return provider.Result{ResourceID: r.ID, Region: r.Scope.Region, Status: provider.StatusApplied}, nil
}

func (f *fakeProvider) Destroy(ctx context.Context, r spec.Resource, p spec.Policies) error {
	if f.destroyErr != nil {
		return f.destroyErr
	}
	f.destroyed = append(f.destroyed, r.ID)
	return nil
}

func (f *fakeProvider) EnsureNetwork(ctx context.Context, s provider.NetworkScope, c provider.ScopeContents, p spec.Policies) error {
	f.networkScopes = append(f.networkScopes, s)
	return nil
}

func (f *fakeProvider) DestroyNetwork(ctx context.Context, s provider.NetworkScope, c provider.ScopeContents, p spec.Policies) error {
	f.destroyedNetworks = append(f.destroyedNetworks, s)
	return nil
}

var _ provider.CloudProvider = (*fakeProvider)(nil)

// --- fixtures ----------------------------------------------------------

func testSpec(intent spec.Intent, providers ...spec.Provider) spec.Specification {
	if len(providers) == 0 {
		providers = []spec.Provider{spec.ProviderAWS}
	}
	s := spec.Specification{SDDVersion: "1.0", Intent: intent}
	for i, p := range providers {
		s.Resources = append(s.Resources, spec.Resource{
			ID:         "res-" + string(rune('a'+i)),
			Type:       spec.ResourceTypeObjectStorage,
			Provider:   p,
			Scope:      spec.Scope{Environment: "dev", Region: "eu-central-1"},
			Properties: map[string]any{"bucket_name": "res-" + string(rune('a'+i))},
		})
	}
	return s
}

// harness wires the package-level indirections to fakes and restores them
// afterwards.
type harness struct {
	translator *fakeTranslator
	providers  map[spec.Provider]*fakeProvider
	out        *bytes.Buffer
	cmd        *cobra.Command
}

func newHarness(t *testing.T, s spec.Specification, translateErr error, stdin string) *harness {
	t.Helper()

	t.Setenv(config.HomeEnv, t.TempDir())

	h := &harness{
		translator: &fakeTranslator{spec: s, err: translateErr},
		providers:  map[spec.Provider]*fakeProvider{},
		out:        &bytes.Buffer{},
	}

	origTranslator, origFactories, origYes := newTranslator, providerFactories, assumeYes
	t.Cleanup(func() {
		newTranslator, providerFactories, assumeYes = origTranslator, origFactories, origYes
	})

	newTranslator = func(providerName, model string) (nlp.Translator, error) {
		return h.translator, nil
	}

	providerFactories = map[spec.Provider]func() (provider.CloudProvider, error){}
	for _, name := range []spec.Provider{spec.ProviderAWS, spec.ProviderGCP, spec.ProviderAzure} {
		name := name
		fp := &fakeProvider{name: string(name)}
		h.providers[name] = fp
		providerFactories[name] = func() (provider.CloudProvider, error) { return fp, nil }
	}

	assumeYes = false

	h.cmd = &cobra.Command{}
	h.cmd.SetContext(context.Background())
	h.cmd.SetOut(h.out)
	h.cmd.SetIn(strings.NewReader(stdin))

	return h
}

// --- tests -------------------------------------------------------------

func TestRunIntentHappyPath(t *testing.T) {
	h := newHarness(t, testSpec(spec.IntentDeploy), nil, "y\n")

	if err := runIntent(h.cmd, []string{"an", "encrypted", "bucket"}, spec.IntentDeploy); err != nil {
		t.Fatalf("runIntent() error: %v", err)
	}

	if h.translator.gotPrompt != "an encrypted bucket" {
		t.Errorf("translator prompt = %q, want the joined arguments", h.translator.gotPrompt)
	}
	if got := h.out.String(); !strings.Contains(got, "Apply completed successfully") {
		t.Errorf("output does not report success:\n%s", got)
	}
	if len(h.providers[spec.ProviderAWS].applied) != 1 {
		t.Error("the AWS provider was not applied")
	}
}

// TestRunIntentRejectsMismatchedIntent covers RFC 011 §2.4: the destroy
// command used to overwrite Intent after translation, silently masking a
// translator that misread the request.
func TestRunIntentRejectsMismatchedIntent(t *testing.T) {
	tests := []struct {
		name       string
		specIntent spec.Intent
		cmdIntent  spec.Intent
	}{
		{name: "destroy command, deploy spec", specIntent: spec.IntentDeploy, cmdIntent: spec.IntentDestroy},
		{name: "deploy command, destroy spec", specIntent: spec.IntentDestroy, cmdIntent: spec.IntentDeploy},
		{name: "deploy command, plan spec", specIntent: spec.IntentPlan, cmdIntent: spec.IntentDeploy},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, testSpec(tt.specIntent), nil, "y\n")

			err := runIntent(h.cmd, []string{"something"}, tt.cmdIntent)
			if err == nil {
				t.Fatal("runIntent() error = nil, want an intent mismatch refusal")
			}
			if !strings.Contains(err.Error(), "declares intent") {
				t.Errorf("error = %q, want it to explain the intent mismatch", err)
			}
			// Nothing may have been touched.
			if len(h.providers[spec.ProviderAWS].applied) != 0 {
				t.Error("resources were applied despite the intent mismatch")
			}
		})
	}
}

func TestRunIntentConfirmation(t *testing.T) {
	tests := []struct {
		name      string
		stdin     string
		assumeYes bool
		wantApply bool
	}{
		{name: "y proceeds", stdin: "y\n", wantApply: true},
		{name: "yes proceeds", stdin: "yes\n", wantApply: true},
		{name: "uppercase Y proceeds", stdin: "Y\n", wantApply: true},
		{name: "uppercase YES proceeds", stdin: "YES\n", wantApply: true},
		{name: "y with surrounding space proceeds", stdin: "  y  \n", wantApply: true},
		{name: "n declines", stdin: "n\n"},
		{name: "empty line declines", stdin: "\n"},
		{name: "arbitrary text declines", stdin: "maybe\n"},
		// The old fmt.Scanln read one token, so "yes" was silently
		// treated as a decline and EOF cancelled with no way to proceed.
		{name: "EOF declines", stdin: ""},
		{name: "--yes skips the prompt entirely", stdin: "", assumeYes: true, wantApply: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, testSpec(spec.IntentDeploy), nil, tt.stdin)
			assumeYes = tt.assumeYes

			err := runIntent(h.cmd, []string{"a bucket"}, spec.IntentDeploy)

			applied := len(h.providers[spec.ProviderAWS].applied) > 0
			if applied != tt.wantApply {
				t.Fatalf("applied = %v, want %v (err=%v)", applied, tt.wantApply, err)
			}
			if !tt.wantApply {
				if err == nil || !strings.Contains(err.Error(), "cancelled by user") {
					t.Errorf("error = %v, want a cancellation", err)
				}
			}
		})
	}
}

// TestBuildEngineIsLazy covers RFC 011 §2.8: all three providers were
// constructed unconditionally, and each hard-fails without
// CLOUDSDD_PULUMI_PASSPHRASE — so an AWS-only deploy still demanded GCP
// and Azure configuration.
func TestBuildEngineIsLazy(t *testing.T) {
	tests := []struct {
		name      string
		providers []spec.Provider
		wantBuilt []spec.Provider
	}{
		{name: "aws only", providers: []spec.Provider{spec.ProviderAWS}, wantBuilt: []spec.Provider{spec.ProviderAWS}},
		{name: "gcp only", providers: []spec.Provider{spec.ProviderGCP}, wantBuilt: []spec.Provider{spec.ProviderGCP}},
		{
			name:      "aws and azure",
			providers: []spec.Provider{spec.ProviderAWS, spec.ProviderAzure},
			wantBuilt: []spec.Provider{spec.ProviderAWS, spec.ProviderAzure},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(config.HomeEnv, t.TempDir())

			built := map[spec.Provider]bool{}
			orig := providerFactories
			t.Cleanup(func() { providerFactories = orig })

			providerFactories = map[spec.Provider]func() (provider.CloudProvider, error){}
			for _, name := range []spec.Provider{spec.ProviderAWS, spec.ProviderGCP, spec.ProviderAzure} {
				name := name
				providerFactories[name] = func() (provider.CloudProvider, error) {
					built[name] = true
					return &fakeProvider{name: string(name)}, nil
				}
			}

			if _, _, err := buildEngine(testSpec(spec.IntentDeploy, tt.providers...), nil); err != nil {
				t.Fatalf("buildEngine() error: %v", err)
			}

			for _, want := range tt.wantBuilt {
				if !built[want] {
					t.Errorf("provider %q was not constructed but is referenced by the spec", want)
				}
			}
			for name := range built {
				found := false
				for _, want := range tt.wantBuilt {
					if name == want {
						found = true
					}
				}
				if !found {
					t.Errorf("provider %q was constructed but is not referenced by the spec", name)
				}
			}
		})
	}
}

func TestBuildEnginePropagatesFactoryErrors(t *testing.T) {
	t.Setenv(config.HomeEnv, t.TempDir())

	orig := providerFactories
	t.Cleanup(func() { providerFactories = orig })

	providerFactories = map[spec.Provider]func() (provider.CloudProvider, error){
		spec.ProviderAWS: func() (provider.CloudProvider, error) {
			return nil, errors.New("CLOUDSDD_PULUMI_PASSPHRASE must be set")
		},
	}

	_, _, err := buildEngine(testSpec(spec.IntentDeploy), nil)
	if err == nil {
		t.Fatal("buildEngine() error = nil, want the factory failure")
	}
	if !strings.Contains(err.Error(), "failed to initialize aws provider") {
		t.Errorf("error = %q, want it to name the failing provider", err)
	}
}

// TestLedgerRecordsPartialFailure covers RFC 011 §1.1G2: the CLI returned
// early on an apply error and recorded nothing, so resources that had
// actually been created went untracked.
func TestLedgerRecordsPartialFailure(t *testing.T) {
	h := newHarness(t, testSpec(spec.IntentDeploy, spec.ProviderAWS, spec.ProviderGCP), nil, "y\n")
	// AWS succeeds, GCP fails. The engine applies in declaration order.
	h.providers[spec.ProviderGCP].applyErr = errors.New("quota exceeded")

	err := runIntent(h.cmd, []string{"two buckets"}, spec.IntentDeploy)
	if err == nil {
		t.Fatal("runIntent() error = nil, want the apply failure")
	}

	l, readErr := state.ReadLedger()
	if readErr != nil {
		t.Fatalf("ReadLedger() error: %v", readErr)
	}
	if len(l.Resources) != 1 {
		t.Fatalf("ledger has %d resources, want the one that actually applied", len(l.Resources))
	}
	if l.Resources[0].ID != "res-a" {
		t.Errorf("ledger recorded %q, want the resource that succeeded", l.Resources[0].ID)
	}
	if got := h.out.String(); !strings.Contains(got, "Completed before the failure") {
		t.Errorf("output does not report what completed:\n%s", got)
	}
}

func TestRunIntentDestroyRemovesFromLedger(t *testing.T) {
	s := testSpec(spec.IntentDestroy)

	h := newHarness(t, s, nil, "y\n")

	// Seed the ledger as though the resource had been deployed.
	deployed := s.Resources[0]
	if err := state.RecordDeployment(
		[]spec.Resource{deployed},
		[]provider.Result{{ResourceID: deployed.ID, Region: "eu-central-1", Status: provider.StatusApplied}},
	); err != nil {
		t.Fatalf("seeding ledger: %v", err)
	}

	if err := runIntent(h.cmd, []string{"remove it"}, spec.IntentDestroy); err != nil {
		t.Fatalf("runIntent() error: %v", err)
	}

	l, err := state.ReadLedger()
	if err != nil {
		t.Fatalf("ReadLedger() error: %v", err)
	}
	if len(l.Resources) != 0 {
		t.Errorf("ledger has %d resources after destroy, want 0", len(l.Resources))
	}
	if got := h.out.String(); !strings.Contains(got, "Destroy completed successfully") {
		t.Errorf("output does not report success:\n%s", got)
	}
}

func TestRunIntentPassesLedgerAsContext(t *testing.T) {
	h := newHarness(t, testSpec(spec.IntentDeploy), nil, "y\n")

	seeded := spec.Resource{
		ID:         "existing-db",
		Type:       spec.ResourceTypeObjectStorage,
		Provider:   spec.ProviderAWS,
		Scope:      spec.Scope{Region: "eu-central-1"},
		Properties: map[string]any{"bucket_name": "existing-db"},
	}
	if err := state.RecordDeployment(
		[]spec.Resource{seeded},
		[]provider.Result{{ResourceID: "existing-db", Region: "eu-central-1", Status: provider.StatusApplied}},
	); err != nil {
		t.Fatalf("seeding ledger: %v", err)
	}

	if err := runIntent(h.cmd, []string{"another bucket"}, spec.IntentDeploy); err != nil {
		t.Fatalf("runIntent() error: %v", err)
	}

	if !strings.Contains(h.translator.gotContext, "existing-db") {
		t.Errorf("translator context does not carry the ledger: %q", h.translator.gotContext)
	}
}

func TestRunIntentTranslationFailure(t *testing.T) {
	h := newHarness(t, spec.Specification{}, errors.New("api down"), "y\n")

	err := runIntent(h.cmd, []string{"a bucket"}, spec.IntentDeploy)
	if err == nil {
		t.Fatal("runIntent() error = nil, want the translation failure")
	}
	if !strings.Contains(err.Error(), "translation failed") {
		t.Errorf("error = %q, want it to report a translation failure", err)
	}
}

// TestRunIntentRejectsEmptySpec: a Specification with no resources fails
// spec.Validate (Resources is required), so a translator that produced
// nothing is surfaced as an error rather than a silent success.
func TestRunIntentRejectsEmptySpec(t *testing.T) {
	s := spec.Specification{SDDVersion: "1.0", Intent: spec.IntentDeploy}

	h := newHarness(t, s, nil, "y\n")

	err := runIntent(h.cmd, []string{"nothing"}, spec.IntentDeploy)
	if err == nil {
		t.Fatal("runIntent() error = nil, want the empty specification to be rejected")
	}
	if !strings.Contains(err.Error(), "plan failed") {
		t.Errorf("error = %q, want it to report the failed plan", err)
	}
	if len(h.providers[spec.ProviderAWS].applied) != 0 {
		t.Error("resources were applied from an invalid specification")
	}
}

func TestDisplayRegion(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{in: "eu-central-1", want: "eu-central-1"},
		{in: "", want: "global"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := displayRegion(tt.in); got != tt.want {
				t.Errorf("displayRegion(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestConfirm(t *testing.T) {
	tests := []struct {
		name      string
		stdin     string
		assumeYes bool
		want      bool
	}{
		{name: "y", stdin: "y\n", want: true},
		{name: "yes", stdin: "yes\n", want: true},
		{name: "no", stdin: "n\n"},
		{name: "empty", stdin: "\n"},
		{name: "eof", stdin: ""},
		{name: "assume yes", assumeYes: true, want: true},
		{name: "no trailing newline", stdin: "y", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orig := assumeYes
			t.Cleanup(func() { assumeYes = orig })
			assumeYes = tt.assumeYes

			cmd := &cobra.Command{}
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetIn(strings.NewReader(tt.stdin))

			got, err := confirm(cmd, bufio.NewReader(cmd.InOrStdin()), spec.IntentDeploy)
			if err != nil {
				t.Fatalf("confirm() error: %v", err)
			}
			if got != tt.want {
				t.Errorf("confirm() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCommandsAreRegistered(t *testing.T) {
	names := map[string]bool{}
	for _, c := range rootCmd.Commands() {
		names[c.Name()] = true
	}

	for _, want := range []string{"deploy", "destroy"} {
		if !names[want] {
			t.Errorf("command %q is not registered on the root command", want)
		}
	}
}

func TestCommandsExposeYesFlag(t *testing.T) {
	for _, cmd := range []*cobra.Command{deployCmd, destroyCmd} {
		t.Run(cmd.Name(), func(t *testing.T) {
			f := cmd.Flags().Lookup("yes")
			if f == nil {
				t.Fatal("--yes flag is not registered; the command is unusable from CI")
			}
			if f.Shorthand != "y" {
				t.Errorf("--yes shorthand = %q, want %q", f.Shorthand, "y")
			}
		})
	}
}
