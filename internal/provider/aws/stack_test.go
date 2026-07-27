package aws

import (
	"context"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider"
)

func TestStackNameFor(t *testing.T) {
	if got := stackNameFor("app-data"); got != "app-data" {
		t.Errorf("stackNameFor(%q) = %q, want %q", "app-data", got, "app-data")
	}
}

func TestSetRegionConfig_EmptyRegionIsNoop(t *testing.T) {
	p := &AWSProvider{}
	// A zero-value auto.Stack is not usable for real operations: this
	// verifies that, with an empty region, the function returns without
	// even trying to touch it (branch covered without needing the pulumi
	// CLI).
	if err := p.setRegionConfig(context.Background(), auto.Stack{}, ""); err != nil {
		t.Fatalf("setRegionConfig() with empty region should be a no-op, got error: %v", err)
	}
}

func TestSummarizeChangeSummary(t *testing.T) {
	tests := []struct {
		name string
		cs   map[apitype.OpType]int
		want provider.Action
	}{
		{name: "nil summary is noop", cs: nil, want: provider.ActionNoop},
		{name: "only same is noop", cs: map[apitype.OpType]int{apitype.OpSame: 3}, want: provider.ActionNoop},
		{name: "create takes priority", cs: map[apitype.OpType]int{apitype.OpCreate: 1, apitype.OpSame: 2}, want: provider.ActionCreate},
		{name: "create-replacement counts as create", cs: map[apitype.OpType]int{apitype.OpCreateReplacement: 1}, want: provider.ActionCreate},
		{name: "update", cs: map[apitype.OpType]int{apitype.OpUpdate: 1}, want: provider.ActionUpdate},
		{name: "replace counts as update", cs: map[apitype.OpType]int{apitype.OpReplace: 1}, want: provider.ActionUpdate},
		{name: "delete", cs: map[apitype.OpType]int{apitype.OpDelete: 1}, want: provider.ActionDestroy},
		{name: "delete-replaced counts as destroy", cs: map[apitype.OpType]int{apitype.OpDeleteReplaced: 1}, want: provider.ActionDestroy},
		{
			name: "create takes priority over update and delete",
			cs:   map[apitype.OpType]int{apitype.OpCreate: 1, apitype.OpUpdate: 1, apitype.OpDelete: 1},
			want: provider.ActionCreate,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := summarizeChangeSummary(tt.cs); got != tt.want {
				t.Errorf("summarizeChangeSummary() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestChangesFromSummary(t *testing.T) {
	if got := changesFromSummary(nil); got != nil {
		t.Errorf("changesFromSummary(nil) = %v, want nil", got)
	}
	if got := changesFromSummary(map[apitype.OpType]int{}); got != nil {
		t.Errorf("changesFromSummary(empty) = %v, want nil", got)
	}

	got := changesFromSummary(map[apitype.OpType]int{apitype.OpCreate: 2})
	if got["create"] != 2 {
		t.Errorf("changesFromSummary() = %v, want map with create=2", got)
	}
}

func TestDetailsFromOutputs(t *testing.T) {
	if got := detailsFromOutputs(nil); got != nil {
		t.Errorf("detailsFromOutputs(nil) = %v, want nil", got)
	}

	outputs := auto.OutputMap{
		"bucket_arn": auto.OutputValue{Value: "arn:aws:s3:::app-data", Secret: false},
		"role_arn":   auto.OutputValue{Value: "arn:aws:iam::123456789012:role/x", Secret: true},
	}
	got := detailsFromOutputs(outputs)
	if got["bucket_arn"] != "arn:aws:s3:::app-data" {
		t.Errorf("expected plaintext output preserved, got %v", got["bucket_arn"])
	}
	if got["role_arn"] != "[redacted]" {
		t.Errorf("expected secret output redacted, got %v", got["role_arn"])
	}
}

type providerOptsMocks struct{}

func (providerOptsMocks) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	return args.Name + "_id", args.Inputs, nil
}

func (providerOptsMocks) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	return resource.PropertyMap{}, nil
}

func TestProviderOpts(t *testing.T) {
	t.Run("nil creds means no explicit provider (default credential chain)", func(t *testing.T) {
		p := &AWSProvider{}
		err := pulumi.RunErr(func(ctx *pulumi.Context) error {
			opts, err := p.providerOpts(ctx, "eu-central-1")
			if err != nil {
				return err
			}
			if opts != nil {
				t.Errorf("expected nil opts for default credential chain, got %v", opts)
			}
			return nil
		}, pulumi.WithMocks("cloudsdd-test", "test-stack", providerOptsMocks{}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("assumed creds produce an explicit provider option", func(t *testing.T) {
		p := &AWSProvider{creds: &assumedCredentials{
			AccessKeyID:     "AKIAEXAMPLE",
			SecretAccessKey: "secret",
			SessionToken:    "token",
		}}
		err := pulumi.RunErr(func(ctx *pulumi.Context) error {
			opts, err := p.providerOpts(ctx, "eu-central-1")
			if err != nil {
				return err
			}
			if len(opts) != 1 {
				t.Errorf("expected exactly one ResourceOption for assumed credentials, got %d", len(opts))
			}
			return nil
		}, pulumi.WithMocks("cloudsdd-test", "test-stack", providerOptsMocks{}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}
