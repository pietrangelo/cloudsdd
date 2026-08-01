// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

// Package pulumiutil holds the Pulumi Automation API helpers shared by
// every provider that drives Pulumi (RFC 011 §2.2).
//
// SummarizeChangeSummary was private to internal/provider/aws. The GCP and
// Azure providers hand-copied a degraded version of it — the source even
// carried the comment "Convert changes manually for brevity since
// summarizeChangeSummary is private in aws pkg" — and both copies dropped
// the delete case and returned stringly-typed actions, so a Plan that would
// destroy infrastructure reported "no-op" (RFC 011 §1.1H2).
package pulumiutil

import (
	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"

	"cloudsdd/internal/provider"
)

// SummarizeChangeSummary reduces the ChangeSummary of a Preview/Up
// (aggregated across all Pulumi resources declared for a single CloudSDD
// Resource, RFC 002 §2.7) to a single provider.Action.
func SummarizeChangeSummary(cs map[apitype.OpType]int) provider.Action {
	if cs[apitype.OpCreate] > 0 || cs[apitype.OpCreateReplacement] > 0 {
		return provider.ActionCreate
	}
	if cs[apitype.OpUpdate] > 0 || cs[apitype.OpReplace] > 0 {
		return provider.ActionUpdate
	}
	if cs[apitype.OpDelete] > 0 || cs[apitype.OpDeleteReplaced] > 0 {
		return provider.ActionDestroy
	}
	return provider.ActionNoop
}

// ChangesFromSummary exposes the raw per-operation counts for Diff.Changes.
func ChangesFromSummary(cs map[apitype.OpType]int) map[string]any {
	if len(cs) == 0 {
		return nil
	}
	out := make(map[string]any, len(cs))
	for op, n := range cs {
		out[string(op)] = n
	}
	return out
}

// DetailsFromOutputs converts Pulumi outputs into Result.Details. Outputs
// marked Secret are redacted: Result.Details can end up in logs/reports,
// which is not the right place for sensitive material even when Pulumi
// itself considers it a secret.
func DetailsFromOutputs(outputs auto.OutputMap) map[string]any {
	if len(outputs) == 0 {
		return nil
	}
	out := make(map[string]any, len(outputs))
	for k, v := range outputs {
		if v.Secret {
			out[k] = "[redacted]"
			continue
		}
		out[k] = v.Value
	}
	return out
}
