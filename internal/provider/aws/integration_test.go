// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Pietrangelo Masala

//go:build integration

// Integration test for RFC 002 §5: full cycle Plan → Apply → direct
// verification against the S3 API → Destroy → verify removal, against an
// ephemeral LocalStack (no call to real AWS).
//
// Requires Docker and the `pulumi` CLI installed on the machine running
// the tests (the Automation API shells out to the pulumi binary even for
// an inline Go program). Run with:
//
//	go test -tags=integration ./internal/provider/aws/... -run TestIntegration -v
//
// Not run by the default suite (`go test ./...`) precisely because they
// require these external dependencies.
package aws

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"cloudsdd/internal/spec"
)

func startLocalStack(t *testing.T) (endpoint string) {
	t.Helper()
	ctx := context.Background()

	req := testcontainers.ContainerRequest{
		Image:        "localstack/localstack:3.0",
		ExposedPorts: []string{"4566/tcp"},
		Env:          map[string]string{"SERVICES": "s3"},
		WaitingFor:   wait.ForLog("Ready.").WithStartupTimeout(2 * time.Minute),
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("failed to start localstack container: %v", err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Logf("failed to terminate localstack container: %v", err)
		}
	})

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("failed to get localstack host: %v", err)
	}
	port, err := container.MappedPort(ctx, "4566/tcp")
	if err != nil {
		t.Fatalf("failed to get localstack mapped port: %v", err)
	}
	return fmt.Sprintf("http://%s:%s", host, port.Port())
}

func s3ClientForLocalStack(t *testing.T, ctx context.Context, endpoint, region string) *s3.Client {
	t.Helper()
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	if err != nil {
		t.Fatalf("failed to load AWS config for localstack verification client: %v", err)
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
}

// TestIntegration_ObjectStorage_PlanApplyDestroy exercises AWSProvider
// against LocalStack: creates an S3 bucket, verifies it really exists via
// the SDK, destroys the resource, and verifies it was removed (RFC 002 §5).
func TestIntegration_ObjectStorage_PlanApplyDestroy(t *testing.T) {
	const region = "us-east-1"
	endpoint := startLocalStack(t)

	t.Setenv("CLOUDSDD_PULUMI_PASSPHRASE", "integration-test-passphrase")
	t.Setenv("CLOUDSDD_STATE_DIR", t.TempDir())
	// The Pulumi AWS provider resolves these variables with the same
	// mechanism as the standard AWS SDK: pointing them at LocalStack
	// avoids any call to real AWS during the test.
	t.Setenv("AWS_ENDPOINT_URL_S3", endpoint)
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", region)

	p, err := NewProvider()
	if err != nil {
		t.Fatalf("NewProvider() failed: %v", err)
	}

	resource := spec.Resource{
		ID:       "integration-test-bucket",
		Type:     spec.ResourceTypeObjectStorage,
		Provider: spec.ProviderAWS,
		Properties: map[string]any{
			"bucket_name": "cloudsdd-integration-test",
			"region":      region,
		},
	}
	policies := spec.Policies{AllowedRegions: []string{region}}

	ctx := context.Background()

	if err := p.Validate(ctx, resource, policies); err != nil {
		t.Fatalf("Validate() failed: %v", err)
	}

	diff, err := p.Plan(ctx, resource, policies)
	if err != nil {
		t.Fatalf("Plan() failed: %v", err)
	}
	t.Logf("plan: %+v", diff)

	result, err := p.Apply(ctx, resource, policies)
	if err != nil {
		t.Fatalf("Apply() failed: %v", err)
	}
	t.Logf("apply: %+v", result)

	verifyClient := s3ClientForLocalStack(t, ctx, endpoint, region)
	if _, err := verifyClient.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("cloudsdd-integration-test")}); err != nil {
		t.Fatalf("expected bucket to exist after Apply, HeadBucket failed: %v", err)
	}

	if err := p.Destroy(ctx, resource, policies); err != nil {
		t.Fatalf("Destroy() failed: %v", err)
	}

	if _, err := verifyClient.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("cloudsdd-integration-test")}); err == nil {
		t.Fatalf("expected bucket to be gone after Destroy, but HeadBucket succeeded")
	}
}
