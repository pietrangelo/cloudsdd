// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package pulumiutil

import (
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"

	"cloudsdd/internal/provider"
)

func TestSummarizeChangeSummary(t *testing.T) {
	tests := []struct {
		name    string
		summary map[apitype.OpType]int
		want    provider.Action
	}{
		{name: "empty is a no-op", summary: map[apitype.OpType]int{}, want: provider.ActionNoop},
		{name: "nil is a no-op", summary: nil, want: provider.ActionNoop},
		{name: "same count is a no-op", summary: map[apitype.OpType]int{apitype.OpSame: 3}, want: provider.ActionNoop},
		{name: "create", summary: map[apitype.OpType]int{apitype.OpCreate: 1}, want: provider.ActionCreate},
		{name: "create replacement", summary: map[apitype.OpType]int{apitype.OpCreateReplacement: 1}, want: provider.ActionCreate},
		{name: "update", summary: map[apitype.OpType]int{apitype.OpUpdate: 2}, want: provider.ActionUpdate},
		{name: "replace", summary: map[apitype.OpType]int{apitype.OpReplace: 1}, want: provider.ActionUpdate},
		{
			// RFC 011 §1.1H2: the GCP and Azure copies of this mapping
			// dropped the delete case entirely, so a plan that would
			// destroy infrastructure reported "no-op".
			name:    "delete is reported, not swallowed",
			summary: map[apitype.OpType]int{apitype.OpDelete: 1},
			want:    provider.ActionDestroy,
		},
		{
			name:    "delete replaced is reported",
			summary: map[apitype.OpType]int{apitype.OpDeleteReplaced: 1},
			want:    provider.ActionDestroy,
		},
		{
			name:    "create wins over update",
			summary: map[apitype.OpType]int{apitype.OpCreate: 1, apitype.OpUpdate: 1},
			want:    provider.ActionCreate,
		},
		{
			name:    "update wins over delete",
			summary: map[apitype.OpType]int{apitype.OpUpdate: 1, apitype.OpDelete: 1},
			want:    provider.ActionUpdate,
		},
		{
			name:    "zero counts are ignored",
			summary: map[apitype.OpType]int{apitype.OpCreate: 0, apitype.OpDelete: 2},
			want:    provider.ActionDestroy,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SummarizeChangeSummary(tt.summary); got != tt.want {
				t.Errorf("SummarizeChangeSummary(%v) = %q, want %q", tt.summary, got, tt.want)
			}
		})
	}
}

func TestChangesFromSummary(t *testing.T) {
	tests := []struct {
		name    string
		summary map[apitype.OpType]int
		wantNil bool
		wantLen int
	}{
		{name: "empty yields nil", summary: map[apitype.OpType]int{}, wantNil: true},
		{name: "nil yields nil", summary: nil, wantNil: true},
		{
			name:    "counts are preserved",
			summary: map[apitype.OpType]int{apitype.OpCreate: 2, apitype.OpSame: 1},
			wantLen: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ChangesFromSummary(tt.summary)

			if tt.wantNil {
				if got != nil {
					t.Fatalf("ChangesFromSummary() = %v, want nil", got)
				}
				return
			}
			if len(got) != tt.wantLen {
				t.Fatalf("ChangesFromSummary() has %d entries, want %d", len(got), tt.wantLen)
			}
			if got[string(apitype.OpCreate)] != 2 {
				t.Errorf("create count = %v, want 2", got[string(apitype.OpCreate)])
			}
		})
	}
}

// TestDetailsFromOutputsRedactsSecrets pins the rule that Result.Details
// can reach logs and reports, so Pulumi secrets must never appear there.
func TestDetailsFromOutputs(t *testing.T) {
	tests := []struct {
		name    string
		outputs auto.OutputMap
		want    map[string]any
	}{
		{name: "empty yields nil", outputs: auto.OutputMap{}, want: nil},
		{name: "nil yields nil", outputs: nil, want: nil},
		{
			name: "plain values pass through",
			outputs: auto.OutputMap{
				"bucketName": auto.OutputValue{Value: "my-bucket"},
			},
			want: map[string]any{"bucketName": "my-bucket"},
		},
		{
			name: "secrets are redacted",
			outputs: auto.OutputMap{
				"password": auto.OutputValue{Value: "hunter2", Secret: true},
			},
			want: map[string]any{"password": "[redacted]"},
		},
		{
			name: "mixed",
			outputs: auto.OutputMap{
				"endpoint": auto.OutputValue{Value: "db.internal"},
				"password": auto.OutputValue{Value: "hunter2", Secret: true},
			},
			want: map[string]any{"endpoint": "db.internal", "password": "[redacted]"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetailsFromOutputs(tt.outputs)

			if tt.want == nil {
				if got != nil {
					t.Fatalf("DetailsFromOutputs() = %v, want nil", got)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("DetailsFromOutputs() has %d entries, want %d", len(got), len(tt.want))
			}
			for k, want := range tt.want {
				if got[k] != want {
					t.Errorf("DetailsFromOutputs()[%q] = %v, want %v", k, got[k], want)
				}
			}
		})
	}
}

func TestDetailsFromOutputsNeverLeaksSecretValues(t *testing.T) {
	outputs := auto.OutputMap{
		"masterPassword": auto.OutputValue{Value: "super-secret-value", Secret: true},
	}

	for _, v := range DetailsFromOutputs(outputs) {
		if v == "super-secret-value" {
			t.Fatal("DetailsFromOutputs() leaked a secret value")
		}
	}
}
