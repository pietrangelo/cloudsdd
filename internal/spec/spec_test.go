// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package spec

import "testing"

func TestScope_EffectiveSealed(t *testing.T) {
	sealed := true
	unsealed := false

	tests := []struct {
		name string
		s    Scope
		want bool
	}{
		{name: "absent defaults to sealed", s: Scope{}, want: true},
		{name: "explicit true", s: Scope{Sealed: &sealed}, want: true},
		{name: "explicit false", s: Scope{Sealed: &unsealed}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.s.EffectiveSealed(); got != tt.want {
				t.Errorf("EffectiveSealed() = %v, want %v", got, tt.want)
			}
		})
	}
}
