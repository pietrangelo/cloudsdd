// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package main

import (
	"context"
	"fmt"
	"io"
	"os"

	// The IANA timezone database is embedded rather than read from the
	// host. A power schedule resolves zone names such as "Europe/Rome"
	// (RFC 012 §2.2), and a stripped container or a Windows machine has
	// no zoneinfo to read — where the failure would be a schedule that
	// refuses to compile, or worse, one that compiles against the wrong
	// DST rules.
	_ "time/tzdata"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "cloudsdd",
	Short: "CloudSDD is a natural-language driven Cloud Engine",
	Long: `CloudSDD translates natural language prompts into strict Cloud Specifications
and applies them to your cloud provider using state-of-the-art IaC best practices.`,
	// A failed deploy already prints a specific error; dumping the full
	// usage text after it buries the actual cause. SilenceErrors stops
	// cobra printing the error too — run() below owns that, and without
	// this the user sees every failure twice.
	SilenceUsage:  true,
	SilenceErrors: true,
}

func main() {
	os.Exit(run(context.Background(), os.Stderr))
}

// run is main's testable body: it returns the exit code rather than
// calling os.Exit directly.
func run(ctx context.Context, stderr io.Writer) int {
	if err := rootCmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}
	return 0
}
