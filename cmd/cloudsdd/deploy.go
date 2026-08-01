// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package main

import (
	"github.com/spf13/cobra"

	"cloudsdd/internal/spec"
)

var deployCmd = &cobra.Command{
	Use:   "deploy [natural language description]",
	Short: "Deploy cloud resources described in natural language",
	Long: `Deploy translates a natural language description into a strict CloudSDD
Specification, shows you the resulting plan, and applies it after
confirmation.

State-of-the-art secure configurations (encryption at rest, private
networking, deletion protection) are applied automatically; you do not
need to ask for them.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runIntent(cmd, args, spec.IntentDeploy)
	},
}

func init() {
	deployCmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "skip the interactive confirmation prompt")
	rootCmd.AddCommand(deployCmd)
}
