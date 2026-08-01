// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
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

	specJSON, err := json.MarshalIndent(sddSpec, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to render specification: %w", err)
	}
	fmt.Fprintf(out, "\n--- Translated Specification (intent: %s) ---\n%s\n%s\n",
		intent, specJSON, strings.Repeat("-", 44))

	eng, err := buildEngine(sddSpec)
	if err != nil {
		return err
	}

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

	ok, err := confirm(cmd, intent)
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
	return nil
}

func printResults(out io.Writer, results []provider.Result) {
	for _, r := range results {
		fmt.Fprintf(out, "- %s (region: %s): %s\n", r.ResourceID, displayRegion(r.Region), r.Status)
	}
}

func displayRegion(region string) string {
	if region == "" {
		return "global"
	}
	return region
}

// buildEngine constructs only the providers the Specification actually
// references.
//
// Previously all three were built unconditionally, and each NewProvider
// hard-fails without CLOUDSDD_PULUMI_PASSPHRASE — so deploying to AWS
// alone still required GCP and Azure to be configured (RFC 011 §2.8).
func buildEngine(s spec.Specification) (engine.Engine, error) {
	needed := make(map[spec.Provider]struct{}, len(s.Resources))
	for _, r := range s.Resources {
		needed[r.Provider] = struct{}{}
	}

	providers := make(map[spec.Provider]provider.CloudProvider, len(needed))
	for name := range needed {
		factory, ok := providerFactories[name]
		if !ok {
			// spec.Validate already rejects unknown providers; the
			// remaining case is "agnostic", which the Engine reports
			// with its own error.
			continue
		}
		p, err := factory()
		if err != nil {
			return nil, fmt.Errorf("failed to initialize %s provider: %w", name, err)
		}
		providers[name] = p
	}

	return engine.New(providers), nil
}

// confirm gates every destructive operation. It reads a whole line rather
// than one whitespace-separated token, so "yes" is understood rather than
// silently treated as a decline, and defaults to "no" on EOF or any other
// input.
func confirm(cmd *cobra.Command, intent spec.Intent) (bool, error) {
	if assumeYes {
		return true, nil
	}

	verb := "apply these changes"
	if intent == spec.IntentDestroy {
		verb = "destroy these resources"
	}
	fmt.Fprintf(cmd.OutOrStdout(), "\nDo you want to %s? (y/N): ", verb)

	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
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
