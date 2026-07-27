package aws

import (
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func TestDecodeS3Properties(t *testing.T) {
	tests := []struct {
		name    string
		props   map[string]any
		wantErr bool
	}{
		{
			name:    "valid minimal properties",
			props:   map[string]any{"bucket_name": "app-data", "region": "eu-central-1"},
			wantErr: false,
		},
		{
			name:    "valid with explicit flags",
			props:   map[string]any{"bucket_name": "app-data", "region": "eu-central-1", "versioning": true, "encryption": false, "block_public_access": false},
			wantErr: false,
		},
		{
			name:    "missing bucket_name",
			props:   map[string]any{"region": "eu-central-1"},
			wantErr: true,
		},
		{
			name:    "missing region",
			props:   map[string]any{"bucket_name": "app-data"},
			wantErr: true,
		},
		{
			name:    "bucket_name too short",
			props:   map[string]any{"bucket_name": "ab", "region": "eu-central-1"},
			wantErr: true,
		},
		{
			name:    "bucket_name uppercase rejected",
			props:   map[string]any{"bucket_name": "App-Data", "region": "eu-central-1"},
			wantErr: true,
		},
		{
			name:    "bucket_name with dot rejected",
			props:   map[string]any{"bucket_name": "app.data", "region": "eu-central-1"},
			wantErr: true,
		},
		{
			name:    "malformed region rejected",
			props:   map[string]any{"bucket_name": "app-data", "region": "not-a-region"},
			wantErr: true,
		},
		{
			name:    "unknown field rejected",
			props:   map[string]any{"bucket_name": "app-data", "region": "eu-central-1", "acl": "public-read"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeS3Properties(tt.props)
			if (err != nil) != tt.wantErr {
				t.Fatalf("decodeS3Properties() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}

	t.Run("secure defaults when flags absent", func(t *testing.T) {
		p, err := decodeS3Properties(map[string]any{"bucket_name": "app-data", "region": "eu-central-1"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p.EffectiveVersioning() != false {
			t.Errorf("EffectiveVersioning() = true, want false (opt-in)")
		}
		if p.EffectiveEncryption() != true {
			t.Errorf("EffectiveEncryption() = false, want true (secure by default)")
		}
		if p.EffectiveBlockPublicAccess() != true {
			t.Errorf("EffectiveBlockPublicAccess() = false, want true (secure by default)")
		}
	})

	t.Run("explicit false overrides secure defaults", func(t *testing.T) {
		p, err := decodeS3Properties(map[string]any{
			"bucket_name": "app-data", "region": "eu-central-1",
			"encryption": false, "block_public_access": false,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if p.EffectiveEncryption() != false {
			t.Errorf("EffectiveEncryption() = true, want false (explicit override)")
		}
		if p.EffectiveBlockPublicAccess() != false {
			t.Errorf("EffectiveBlockPublicAccess() = true, want false (explicit override)")
		}
	})
}

// s3Mocks is a minimal MockResourceMonitor: it always returns an ID
// derived from the logical name and echoes back the Inputs as state,
// enough to exercise the Pulumi resource declaration logic without a real
// backend (no need for the pulumi CLI or Docker/LocalStack).
type s3Mocks struct{}

func (s3Mocks) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	return args.Name + "_id", args.Inputs, nil
}

func (s3Mocks) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	return resource.PropertyMap{}, nil
}

func TestDeclareS3Bucket(t *testing.T) {
	tests := []struct {
		name string
		p    S3Properties
	}{
		{
			name: "all secure defaults",
			p:    S3Properties{BucketName: "app-data", Region: "eu-central-1"},
		},
		{
			name: "versioning enabled",
			p: S3Properties{BucketName: "app-data", Region: "eu-central-1",
				Versioning: boolPtr(true)},
		},
		{
			name: "encryption and public access block disabled explicitly",
			p: S3Properties{BucketName: "app-data", Region: "eu-central-1",
				Encryption: boolPtr(false), BlockPublicAccess: boolPtr(false)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := pulumi.RunErr(func(ctx *pulumi.Context) error {
				return declareS3Bucket(ctx, "app-data-resource", tt.p)
			}, pulumi.WithMocks("cloudsdd-test", "test-stack", s3Mocks{}))
			if err != nil {
				t.Fatalf("declareS3Bucket() unexpected error: %v", err)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }
