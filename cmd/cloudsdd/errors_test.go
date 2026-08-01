// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"cloudsdd/internal/config"
	"cloudsdd/internal/nlp"
	"cloudsdd/internal/provider"
	"cloudsdd/internal/spec"
)

func testCommand(t *testing.T, out *bytes.Buffer) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(out)
	cmd.SetIn(strings.NewReader("y\n"))
	return cmd
}

// TestRunIntentReportsConfigFailure covers RFC 011 §1.1F3: the config
// write errors were discarded, so a home directory that could not be
// written failed silently.
func TestRunIntentReportsConfigFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are not enforced")
	}

	home := filepath.Join(t.TempDir(), "cloudsdd")
	if err := os.Mkdir(home, 0o500); err != nil { // no write permission
		t.Fatalf("setup: %v", err)
	}
	t.Setenv(config.HomeEnv, home)

	out := &bytes.Buffer{}
	err := runIntent(testCommand(t, out), []string{"a bucket"}, spec.IntentDeploy)
	if err == nil {
		t.Fatal("runIntent() error = nil, want the config failure to be reported")
	}
	if !strings.Contains(err.Error(), "AI configuration") {
		t.Errorf("error = %q, want it to name the configuration step", err)
	}
}

func TestRunIntentReportsTranslatorFailure(t *testing.T) {
	t.Setenv(config.HomeEnv, t.TempDir())

	orig := newTranslator
	t.Cleanup(func() { newTranslator = orig })
	newTranslator = func(string, string) (nlp.Translator, error) {
		return nil, errors.New("ANTHROPIC_API_KEY environment variable is required")
	}

	out := &bytes.Buffer{}
	err := runIntent(testCommand(t, out), []string{"a bucket"}, spec.IntentDeploy)
	if err == nil {
		t.Fatal("runIntent() error = nil, want the translator failure")
	}
	if !strings.Contains(err.Error(), "AI translator") {
		t.Errorf("error = %q, want it to name the translator step", err)
	}
}

// TestRunIntentContinuesOnCorruptLedger: a corrupted ledger must warn and
// proceed without context rather than block the user entirely — but the
// warning has to be visible, because the AI is then working blind.
func TestRunIntentContinuesOnCorruptLedger(t *testing.T) {
	h := newHarness(t, testSpec(spec.IntentDeploy), nil, "y\n")

	home := os.Getenv(config.HomeEnv)
	if err := os.WriteFile(filepath.Join(home, "ledger.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err := runIntent(h.cmd, []string{"a bucket"}, spec.IntentDeploy); err != nil {
		t.Fatalf("runIntent() error: %v", err)
	}

	got := h.out.String()
	if !strings.Contains(got, "could not read deployment ledger") {
		t.Errorf("output does not warn about the unreadable ledger:\n%s", got)
	}
	if !strings.Contains(got, "Apply completed successfully") {
		t.Errorf("run did not proceed despite the corrupt ledger:\n%s", got)
	}
}

func TestRunIntentReportsProviderFailure(t *testing.T) {
	h := newHarness(t, testSpec(spec.IntentDeploy), nil, "y\n")

	// newHarness installed working fakes; swap AWS for a failing factory.
	providerFactories[spec.ProviderAWS] = func() (provider.CloudProvider, error) {
		return nil, errors.New("CLOUDSDD_PULUMI_PASSPHRASE must be set")
	}

	err := runIntent(h.cmd, []string{"a bucket"}, spec.IntentDeploy)
	if err == nil {
		t.Fatal("runIntent() error = nil, want the provider construction failure")
	}
	if !strings.Contains(err.Error(), "failed to initialize aws provider") {
		t.Errorf("error = %q, want it to name the failing provider", err)
	}
}

func TestRunExitCodes(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{name: "help succeeds", args: []string{"--help"}, want: 0},
		{name: "unknown command fails", args: []string{"nonsense-command"}, want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			origArgs := os.Args
			t.Cleanup(func() {
				os.Args = origArgs
				rootCmd.SetArgs(nil)
				rootCmd.SetOut(nil)
				rootCmd.SetErr(nil)
			})

			rootCmd.SetArgs(tt.args)
			rootCmd.SetOut(&bytes.Buffer{})
			rootCmd.SetErr(&bytes.Buffer{})

			stderr := &bytes.Buffer{}
			if got := run(context.Background(), stderr); got != tt.want {
				t.Errorf("run() = %d, want %d (stderr: %s)", got, tt.want, stderr)
			}
		})
	}
}
