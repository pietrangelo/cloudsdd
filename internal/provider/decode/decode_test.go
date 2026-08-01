// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package decode

import (
	"errors"
	"strings"
	"testing"

	"github.com/go-playground/validator/v10"
)

type sampleProps struct {
	Name    string `json:"name" validate:"required,samplename"`
	Size    int    `json:"size,omitempty" validate:"omitempty,gt=0"`
	Enabled *bool  `json:"enabled,omitempty"`
}

func testDecoder(t *testing.T) *Decoder {
	t.Helper()
	d := New("test")
	d.MustRegister("samplename", func(fl validator.FieldLevel) bool {
		return !strings.ContainsAny(fl.Field().String(), " \t")
	})
	return d
}

func TestProperties(t *testing.T) {
	tests := []struct {
		name    string
		props   map[string]any
		wantErr string
		check   func(t *testing.T, p sampleProps)
	}{
		{
			name:  "minimal valid",
			props: map[string]any{"name": "bucket"},
			check: func(t *testing.T, p sampleProps) {
				if p.Name != "bucket" {
					t.Errorf("Name = %q, want %q", p.Name, "bucket")
				}
				if p.Enabled != nil {
					t.Errorf("Enabled = %v, want nil (absent)", *p.Enabled)
				}
			},
		},
		{
			name:  "all fields",
			props: map[string]any{"name": "bucket", "size": 10, "enabled": false},
			check: func(t *testing.T, p sampleProps) {
				if p.Size != 10 {
					t.Errorf("Size = %d, want 10", p.Size)
				}
				if p.Enabled == nil || *p.Enabled {
					t.Errorf("Enabled = %v, want explicit false", p.Enabled)
				}
			},
		},
		{
			// The whole point of RFC 011 §2.1: an unknown property means
			// the user asked for something they will not get.
			name:    "unknown property is rejected, not dropped",
			props:   map[string]any{"name": "bucket", "totally_unknown": true},
			wantErr: "unknown or malformed property",
		},
		{
			name:    "missing required field",
			props:   map[string]any{"size": 10},
			wantErr: "property validation failed",
		},
		{
			name:    "custom tag violation",
			props:   map[string]any{"name": "has a space"},
			wantErr: "property validation failed",
		},
		{
			name:    "builtin tag violation",
			props:   map[string]any{"name": "bucket", "size": -1},
			wantErr: "property validation failed",
		},
		{
			name:    "type mismatch",
			props:   map[string]any{"name": 42},
			wantErr: "unknown or malformed property",
		},
		{
			name:  "nil properties",
			props: nil,
			// A nil map marshals to "null", which decodes into the zero
			// struct and then fails the required check.
			wantErr: "property validation failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := testDecoder(t)

			var p sampleProps
			err := d.Properties(tt.props, &p)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Properties() error = nil, want error containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Properties() error = %q, want it to contain %q", err, tt.wantErr)
				}
				if !strings.HasPrefix(err.Error(), "test: ") {
					t.Errorf("Properties() error = %q, want it prefixed with the decoder's provider name", err)
				}
				return
			}

			if err != nil {
				t.Fatalf("Properties() unexpected error: %v", err)
			}
			if tt.check != nil {
				tt.check(t, p)
			}
		})
	}
}

// TestPropertiesRejectsCredentialLikeKeys covers the defense-in-depth
// check: these keys must be refused even though DisallowUnknownFields
// would already reject them, so the rule holds independently of any
// single resource's schema.
func TestPropertiesRejectsCredentialLikeKeys(t *testing.T) {
	keys := []string{
		"password",
		"secret",
		"secret_key",
		"access_key",
		"token",
		"session_token",
		"private_key",
		// Case and embedding must not evade the check.
		"PASSWORD",
		"db_password",
		"MySecretValue",
	}

	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			d := testDecoder(t)

			var p sampleProps
			err := d.Properties(map[string]any{"name": "bucket", key: "hunter2"}, &p)
			if err == nil {
				t.Fatalf("Properties() accepted credential-like key %q", key)
			}
			if !strings.Contains(err.Error(), "looks like a credential") {
				t.Errorf("Properties() error = %q, want the credential rejection message", err)
			}
			// The value must never appear in the error.
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("Properties() error leaked the credential value: %q", err)
			}
		})
	}
}

func TestMustRegisterPanicsOnInvalidTag(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("MustRegister() did not panic on an empty tag name")
		}
	}()

	d := New("test")
	d.MustRegister("", func(validator.FieldLevel) bool { return true })
}

func TestBoolOrDefault(t *testing.T) {
	tr, fa := true, false

	tests := []struct {
		name string
		in   *bool
		def  bool
		want bool
	}{
		{"absent takes the secure default", nil, true, true},
		{"absent takes the permissive default", nil, false, false},
		{"explicit true overrides", &tr, false, true},
		{"explicit false overrides a secure default", &fa, true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BoolOrDefault(tt.in, tt.def); got != tt.want {
				t.Errorf("BoolOrDefault() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestPropertiesRejectsNonStructTarget guards the programming-error path.
func TestPropertiesRejectsNonStructTarget(t *testing.T) {
	d := testDecoder(t)

	var notAStruct map[string]any
	err := d.Properties(map[string]any{"name": "bucket"}, &notAStruct)
	if err == nil {
		t.Fatal("Properties() accepted a non-struct target")
	}
	var invalid *validator.InvalidValidationError
	if !errors.As(err, &invalid) && !strings.Contains(err.Error(), "validation") {
		t.Errorf("Properties() error = %q, want a validation error", err)
	}
}

// FuzzProperties throws malformed JSON at the strict decoder. It must
// always either decode cleanly or return an error — never panic
// (CLAUDE.md: treat every input as hostile).
func FuzzProperties(f *testing.F) {
	f.Add(`{"name":"bucket"}`)
	f.Add(`{"name":"bucket","size":3}`)
	f.Add(`{"unknown":1}`)
	f.Add(`{"password":"x"}`)
	f.Add(`{"name":{"nested":true}}`)
	f.Add(`{}`)

	f.Fuzz(func(t *testing.T, raw string) {
		var props map[string]any
		if err := unmarshalLenient(raw, &props); err != nil {
			t.Skip() // not JSON object input; nothing to decode
		}

		d := testDecoder(t)
		var p sampleProps
		_ = d.Properties(props, &p) // must not panic
	})
}
