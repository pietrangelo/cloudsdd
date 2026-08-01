// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package main

import (
	"github.com/spf13/cobra"

	"cloudsdd/internal/spec"
)

var destroyCmd = &cobra.Command{
	Use:   "destroy [natural language description]",
	Short: "Destroy cloud resources described in natural language",
	Long: `Destroy translates a natural language description into a strict CloudSDD
Specification, shows you what would be removed, and destroys it after
confirmation.

The translated specification must declare intent "destroy". If it does
not, the command refuses rather than coercing it: a request the
translator read as a deployment must never reach a destructive
operation.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runIntent(cmd, args, spec.IntentDestroy)
	},
}

func init() {
	destroyCmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "skip the interactive confirmation prompt")
	rootCmd.AddCommand(destroyCmd)
}
