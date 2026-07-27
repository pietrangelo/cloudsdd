package aws

import (
	"errors"
	"testing"
)

func TestValidateRegionAllowed(t *testing.T) {
	tests := []struct {
		name    string
		region  string
		allowed []string
		wantErr bool
	}{
		{name: "no policy means no constraint", region: "eu-central-1", allowed: nil, wantErr: false},
		{name: "region within allowed list", region: "eu-central-1", allowed: []string{"eu-central-1", "eu-west-1"}, wantErr: false},
		{name: "region outside allowed list", region: "us-east-1", allowed: []string{"eu-central-1"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRegionAllowed(tt.region, tt.allowed)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateRegionAllowed() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(err, ErrRegionNotAllowed) {
				t.Fatalf("expected errors.Is ErrRegionNotAllowed, got %v", err)
			}
		})
	}
}
