// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

package compute

import (
	"errors"
	"strings"
	"testing"

	"cloudsdd/internal/provider/decode"
)

func boolPtr(b bool) *bool { return &b }

func TestEffectiveDiskSizeGB(t *testing.T) {
	tests := []struct {
		name string
		in   Properties
		want int
	}{
		{name: "absent falls back to the default", in: Properties{}, want: defaultDiskSizeGB},
		{name: "explicit value is honored", in: Properties{DiskSizeGB: 100}, want: 100},
		{name: "minimum", in: Properties{DiskSizeGB: 8}, want: 8},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.EffectiveDiskSizeGB(); got != tt.want {
				t.Errorf("EffectiveDiskSizeGB() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestEffectivePublicIP(t *testing.T) {
	tests := []struct {
		name string
		in   Properties
		want bool
	}{
		// Private by default, like every other resource type: the absence
		// of the field must not read as "false is fine either way", it
		// must read as a deliberate secure default.
		{name: "absent is private", in: Properties{}, want: false},
		{name: "explicitly private", in: Properties{PublicIP: boolPtr(false)}, want: false},
		{name: "explicitly public", in: Properties{PublicIP: boolPtr(true)}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.EffectivePublicIP(); got != tt.want {
				t.Errorf("EffectivePublicIP() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestZone(t *testing.T) {
	tests := []struct {
		name    string
		zones   []string
		want    string
		wantErr bool
	}{
		{name: "none lets the cloud choose", zones: nil, want: ""},
		{name: "empty slice lets the cloud choose", zones: []string{}, want: ""},
		{name: "one zone pins the instance", zones: []string{"eu-central-1a"}, want: "eu-central-1a"},
		{
			// Taking the first entry would hand back infrastructure that
			// does not match what the user wrote.
			name:    "two zones are refused",
			zones:   []string{"eu-central-1a", "eu-central-1b"},
			wantErr: true,
		},
		{name: "three zones are refused", zones: []string{"a", "b", "c"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Zone(tt.zones)
			if tt.wantErr {
				if !errors.Is(err, ErrMultipleZones) {
					t.Fatalf("Zone() error = %v, want %v", err, ErrMultipleZones)
				}
				return
			}
			if err != nil {
				t.Fatalf("Zone() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("Zone() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestPropertiesDecoding drives the struct through the same strict decoder
// the providers use, so the validate tags are exercised rather than merely
// declared — the defect RFC 011 §1.1C found across four decoders.
func TestPropertiesDecoding(t *testing.T) {
	tests := []struct {
		name    string
		props   map[string]any
		wantErr string
	}{
		{
			name:  "minimal valid",
			props: map[string]any{"size": "small", "os": "ubuntu-22.04"},
		},
		{
			name: "fully specified",
			props: map[string]any{
				"size": "large", "os": "debian-12",
				"disk_size_gb": 100, "public_ip": true,
			},
		},
		{name: "missing size", props: map[string]any{"os": "ubuntu-22.04"}, wantErr: "validation failed"},
		{name: "missing os", props: map[string]any{"size": "small"}, wantErr: "validation failed"},
		{
			name:    "unknown size",
			props:   map[string]any{"size": "gigantic", "os": "ubuntu-22.04"},
			wantErr: "validation failed",
		},
		{
			// Excluded from the enum on purpose: an OS whose validity
			// depends on the provider is the coupling this schema avoids.
			name:    "provider-specific image is not portable",
			props:   map[string]any{"size": "small", "os": "amazon-linux-2023"},
			wantErr: "validation failed",
		},
		{
			name:    "disk below the minimum",
			props:   map[string]any{"size": "small", "os": "ubuntu-22.04", "disk_size_gb": 4},
			wantErr: "validation failed",
		},
		{
			name:    "disk above the maximum",
			props:   map[string]any{"size": "small", "os": "ubuntu-22.04", "disk_size_gb": 2048},
			wantErr: "validation failed",
		},
		{
			// Mass-assignment defense: a property the provider does not
			// understand means the user asked for something they are not
			// going to get.
			name:    "unknown property is rejected, not dropped",
			props:   map[string]any{"size": "small", "os": "ubuntu-22.04", "spot": true},
			wantErr: "unknown or malformed property",
		},
		{
			// No SSH key belongs in a Specification, and the denylist
			// catches the attempt independently of the schema.
			name:    "private key property is refused",
			props:   map[string]any{"size": "small", "os": "ubuntu-22.04", "private_key": "-----BEGIN"},
			wantErr: "looks like a credential",
		},
	}

	dec := decode.New("test")

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var p Properties
			err := dec.Properties(tt.props, &p)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Properties() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Properties() error = nil, want one containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Properties() error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// TestPropertiesDecodeDefaults pins that an omitted security-relevant
// property lands on the secure value rather than a zero value.
func TestPropertiesDecodeDefaults(t *testing.T) {
	var p Properties
	if err := decode.New("test").Properties(map[string]any{"size": "small", "os": "ubuntu-22.04"}, &p); err != nil {
		t.Fatalf("Properties() error = %v", err)
	}

	if p.EffectivePublicIP() {
		t.Error("an unspecified instance is public; it must be private by default")
	}
	if got := p.EffectiveDiskSizeGB(); got != defaultDiskSizeGB {
		t.Errorf("disk size = %d, want the %d default", got, defaultDiskSizeGB)
	}
}
