// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"cloudsdd/internal/config"
	"cloudsdd/internal/engine"
	"cloudsdd/internal/nlp"
	"cloudsdd/internal/provider"
	"cloudsdd/internal/provider/aws"
	"cloudsdd/internal/provider/azure"
	"cloudsdd/internal/provider/gcp"
	"cloudsdd/internal/spec"
	"cloudsdd/internal/state"
)

// assumeYes skips the interactive confirmation. Without it the CLI is
// unusable from CI: fmt.Scanln on a non-TTY returns an error, which the
// old code read as "not y" and cancelled, with no way to proceed
// (RFC 011 §2.8).
var assumeYes bool

// translatorFactory and providerFactories are indirections so the run
// flow can be exercised with fakes (RFC 011 §5). Production wiring is
// assigned below.
var (
	newTranslator = nlp.NewTranslator

	providerFactories = map[spec.Provider]func() (provider.CloudProvider, error){
		spec.ProviderAWS:   func() (provider.CloudProvider, error) { return aws.NewProvider() },
		spec.ProviderGCP:   func() (provider.CloudProvider, error) { return gcp.NewProvider() },
		spec.ProviderAzure: func() (provider.CloudProvider, error) { return azure.NewProvider() },
	}
)

// runIntent is the shared body of the deploy and destroy commands, which
// were ~90% duplicated before RFC 011 §2.8.
func runIntent(cmd *cobra.Command, args []string, intent spec.Intent) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	// One reader for the whole run. Two bufio.Readers over the same
	// stream would let the first swallow input meant for the second: the
	// schedule prompt would buffer the confirmation answer, and the
	// confirmation would then read EOF and cancel the operation.
	in := bufio.NewReader(cmd.InOrStdin())

	prompt := strings.Join(args, " ")

	fmt.Fprintf(out, "Translating prompt to SDD Specification...\n")

	cfg, err := config.LoadConfig()
	if err != nil {
		return fmt.Errorf("failed to load AI configuration: %w", err)
	}

	translator, err := newTranslator(cfg.AI.Provider, cfg.AI.Model)
	if err != nil {
		return fmt.Errorf("failed to initialize AI translator: %w", err)
	}

	// The ledger is best-effort context for the translator, but a read
	// failure is still reported rather than silently swallowed: a
	// corrupted ledger means the AI is working blind, which the user
	// should know before approving a plan.
	ledger, err := state.ReadLedger()
	if err != nil {
		fmt.Fprintf(out, "Warning: could not read deployment ledger, continuing without context: %v\n", err)
		ledger = &state.Ledger{}
	}
	ledgerJSON, err := json.Marshal(ledger)
	if err != nil {
		return fmt.Errorf("failed to serialize deployment ledger: %w", err)
	}

	sddSpec, err := translator.Translate(ctx, prompt, string(ledgerJSON))
	if err != nil {
		return fmt.Errorf("translation failed: %w", err)
	}

	// The declared intent must match the command the user actually ran.
	// The destroy command previously overwrote it after translation,
	// which silently masked a translator that misread the request
	// (RFC 011 §2.4).
	if sddSpec.Intent != intent {
		return fmt.Errorf(
			"refusing to %s: the translated specification declares intent %q.\n"+
				"Rephrase the request so the intent is unambiguous, or run the matching command",
			intent, sddSpec.Intent)
	}

	// Settle the power schedule before rendering the Specification, so
	// the times the user is about to approve are the ones that will be
	// provisioned rather than blanks filled in later (RFC 012 §5).
	if err := resolveSchedules(cmd, in, &sddSpec, intent); err != nil {
		return err
	}

	eng, notes, err := buildEngine(sddSpec, cfg)
	if err != nil {
		return err
	}
	// A provider that could not be constructed is excluded from agnostic
	// resolution. Saying so is the point: otherwise the same
	// Specification resolves differently on a colleague's machine with no
	// way to see why (RFC 014 §2.5).
	for _, note := range notes {
		fmt.Fprintf(out, "Note: %s\n", note)
	}

	// Bind any "agnostic" resource before rendering, so the user approves
	// a Specification that names a concrete cloud (RFC 014 §2.6).
	resolvedSpec, resolutions, err := eng.Resolve(ctx, sddSpec)
	if err != nil {
		return err
	}
	sddSpec = resolvedSpec

	specJSON, err := json.MarshalIndent(sddSpec, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to render specification: %w", err)
	}
	fmt.Fprintf(out, "\n--- Translated Specification (intent: %s) ---\n%s\n%s\n",
		intent, specJSON, strings.Repeat("-", 44))
	printResolutions(out, resolutions)

	fmt.Fprintln(out, "Planning infrastructure changes...")
	diffs, err := eng.Plan(ctx, sddSpec)
	if err != nil {
		return fmt.Errorf("plan failed: %w", err)
	}
	if len(diffs) == 0 {
		fmt.Fprintln(out, "No changes to apply.")
		return nil
	}
	for _, d := range diffs {
		fmt.Fprintf(out, "Resource: %s (region: %s) -> action: %s\n", d.ResourceID, displayRegion(d.Region), d.Action)
	}
	printSchedules(out, sddSpec)

	ok, err := confirm(cmd, in, intent)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("operation cancelled by user")
	}

	return execute(cmd, eng, sddSpec, intent)
}

// execute runs the operation and records the outcome in the ledger.
//
// The ledger is updated even when the operation fails partway: Apply and
// Destroy both return the results accumulated before the error, and those
// resources really do exist (or really are gone). Returning early without
// recording them, as the old code did, silently desynchronized the ledger
// from reality (RFC 011 §1.1G2).
func execute(cmd *cobra.Command, eng engine.Engine, s spec.Specification, intent spec.Intent) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	var (
		results []provider.Result
		runErr  error
		record  func([]spec.Resource, []provider.Result) error
		done    string
	)

	switch intent {
	case spec.IntentDestroy:
		fmt.Fprintln(out, "Destroying resources...")
		results, runErr = eng.Destroy(ctx, s)
		record = state.RecordDestruction
		done = "Destroy"
	default:
		fmt.Fprintln(out, "Applying changes...")
		results, runErr = eng.Apply(ctx, s)
		record = state.RecordDeployment
		done = "Apply"
	}

	if err := record(s.Resources, results); err != nil {
		fmt.Fprintf(out, "Warning: failed to update deployment ledger: %v\n", err)
	}

	if runErr != nil {
		if len(results) > 0 {
			fmt.Fprintf(out, "\nCompleted before the failure (%d):\n", len(results))
			printResults(out, results)
		}
		return runErr
	}

	fmt.Fprintf(out, "%s completed successfully:\n", done)
	printResults(out, results)

	if intent == spec.IntentDestroy {
		reapNetworks(cmd, eng, s)
	}
	return nil
}

// reapNetworks removes the shared network of any scope this destroy left
// empty (RFC 016 §2.6).
//
// It runs after the ledger has been updated, because the ledger is what
// answers "is this scope empty now?". A failure here is reported and not
// returned: the resources the user asked to destroy are gone, so the
// operation succeeded, and what remains is an empty network that costs a
// little and can be removed on the next destroy. Failing the command
// would report a successful teardown as an error.
func reapNetworks(cmd *cobra.Command, eng engine.Engine, s spec.Specification) {
	out := cmd.OutOrStdout()

	reaped, err := eng.ReapNetworks(cmd.Context(), s)
	if err != nil {
		fmt.Fprintf(out, "Warning: failed to remove an empty environment network: %v\n", err)
	}
	for _, scope := range reaped {
		fmt.Fprintf(out, "- removed the %s network for %s (nothing left in it)\n",
			scope.Provider, displayScope(scope))
	}
}

// displayScope renders a scope for the teardown message.
func displayScope(s provider.NetworkScope) string {
	parts := make([]string, 0, 3)
	for _, p := range []string{s.Account, s.Environment, s.Region} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return "the default scope"
	}
	return strings.Join(parts, "/")
}

func printResults(out io.Writer, results []provider.Result) {
	for _, r := range results {
		fmt.Fprintf(out, "- %s (region: %s): %s\n", r.ResourceID, displayRegion(r.Region), r.Status)
	}
}

// printSchedules renders the effective power schedule of every affected
// resource, in prose, before the confirmation gate.
//
// A six-field cron expression does not let anyone tell at a glance that
// an environment is about to start powering down at 19:00, and the whole
// purpose of the gate is that somebody can (RFC 012 §5).
func printSchedules(out io.Writer, s spec.Specification) {
	statuses := engine.ScheduleStatuses(s)
	if len(statuses) == 0 {
		return
	}
	fmt.Fprintln(out, "\nPower schedule:")
	for _, st := range statuses {
		fmt.Fprintf(out, "- %s: %s\n", st.ResourceID, indentContinuation(st.Summary))
	}
}

// indentContinuation keeps a multi-line schedule summary aligned under
// its resource ID.
func indentContinuation(summary string) string {
	return strings.ReplaceAll(summary, "\n", "\n  ")
}

func displayRegion(region string) string {
	if region == "" {
		return "global"
	}
	return region
}

// printResolutions reports which cloud each agnostic resource was bound
// to, and why.
//
// Resolution is a decision made on the user's behalf, so it is shown
// before the confirmation gate rather than inferred from the rendered
// Specification (RFC 014 §2.1).
func printResolutions(out io.Writer, resolutions []engine.Resolution) {
	if len(resolutions) == 0 {
		return
	}
	fmt.Fprintln(out, "Resolved agnostic resources:")
	for _, r := range resolutions {
		fmt.Fprintf(out, "- %s -> %s (%s)\n", r.ResourceID, r.Provider, r.Reason)
	}
}

// buildEngine constructs the providers the Specification needs.
//
// For explicitly-named providers that is only the ones referenced, which
// is what RFC 011 §2.8 made lazy so an AWS-only deploy would not demand
// GCP and Azure credentials. A Specification containing an "agnostic"
// resource needs candidates, so every provider is attempted — and one
// that cannot be constructed is excluded and reported rather than
// silently dropped (RFC 014 §2.5).
func buildEngine(s spec.Specification, cfg *config.Config) (engine.Engine, []string, error) {
	explicit := make(map[spec.Provider]bool, len(s.Resources))
	agnostic := false
	for _, r := range s.Resources {
		if r.Provider == spec.ProviderAgnostic {
			agnostic = true
			continue
		}
		explicit[r.Provider] = true
	}

	wanted := make(map[spec.Provider]bool, len(providerFactories))
	for name := range explicit {
		wanted[name] = true
	}
	if agnostic {
		for name := range providerFactories {
			wanted[name] = true
		}
	}

	// Sorted, so the notes below appear in the same order every run.
	names := make([]spec.Provider, 0, len(wanted))
	for name := range wanted {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })

	var notes []string
	providers := make(map[spec.Provider]provider.CloudProvider, len(names))
	for _, name := range names {
		factory, ok := providerFactories[name]
		if !ok {
			// spec.Validate already rejects unknown providers.
			continue
		}
		p, err := factory()
		if err != nil {
			// A provider the user named by hand is a hard failure: they
			// asked for it specifically. One that was only a candidate
			// for resolution is merely unavailable.
			if explicit[name] {
				return nil, nil, fmt.Errorf("failed to initialize %s provider: %w", name, err)
			}
			notes = append(notes, fmt.Sprintf("%s is not available as a candidate (%v)", name, err))
			continue
		}
		providers[name] = p
	}

	opts := []engine.Option{
		// The ledger is what tells the Engine whether a scope still holds
		// anything, and so whether its shared network may be torn down
		// (RFC 016 §2.6). Supplied here rather than imported by the
		// Engine, so an Engine driven directly is not obliged to have a
		// ledger on disk — and, absent this, reaps nothing.
		engine.WithScopeOccupancy(func(_ context.Context, sc provider.NetworkScope) (int, error) {
			return state.CountInScope(sc.Account, sc.Environment, sc.Region)
		}),
	}
	if cfg != nil && cfg.Defaults.Provider != "" {
		opts = append(opts, engine.WithDefaultProvider(spec.Provider(cfg.Defaults.Provider)))
	}

	return engine.New(providers, opts...), notes, nil
}

// confirm gates every destructive operation. It reads a whole line rather
// than one whitespace-separated token, so "yes" is understood rather than
// silently treated as a decline, and defaults to "no" on EOF or any other
// input.
func confirm(cmd *cobra.Command, in *bufio.Reader, intent spec.Intent) (bool, error) {
	if assumeYes {
		return true, nil
	}

	verb := "apply these changes"
	if intent == spec.IntentDestroy {
		verb = "destroy these resources"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "\nDo you want to %s? (y/N): ", verb)

	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		// EOF with nothing typed: a non-interactive invocation without
		// --yes. Decline rather than guess.
		return false, nil
	}

	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}
