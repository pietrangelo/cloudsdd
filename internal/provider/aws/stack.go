package aws

import (
	"context"
	"fmt"

	pulumiaws "github.com/pulumi/pulumi-aws/sdk/v6/go/aws"
	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"cloudsdd/internal/provider"
)

// stackNameFor maps a Resource.ID onto the Pulumi stack that represents
// it: one stack per resource (RFC 002 §2.3), to isolate Plan/Destroy per
// individual resource.
func stackNameFor(resourceID string) string { return resourceID }

// upsertStack creates (or selects, if it already exists) the Pulumi stack
// for resourceID, with a local filesystem state backend and a
// passphrase-based secrets provider (RFC 002 §2.3). The passphrase is
// passed only as a Pulumi workspace environment variable, never written
// to disk or logged.
func (p *AWSProvider) upsertStack(ctx context.Context, resourceID string, program pulumi.RunFunc) (auto.Stack, error) {
	proj := workspace.Project{
		Name:    tokens.PackageName(pulumiProjectName),
		Runtime: workspace.NewProjectRuntimeInfo("go", nil),
		Backend: &workspace.ProjectBackend{URL: "file://" + p.stateDir},
	}

	return auto.UpsertStackInlineSource(ctx, stackNameFor(resourceID), pulumiProjectName, program,
		auto.Project(proj),
		auto.SecretsProvider("passphrase"),
		auto.EnvVars(map[string]string{"PULUMI_CONFIG_PASSPHRASE": p.passphrase}),
	)
}

// setRegionConfig sets aws:region on the stack when region is non-empty.
// Some ResourceTypes (e.g. cross_account_role, IAM is global on AWS, RFC
// 003 §2.3) do not need it.
func (p *AWSProvider) setRegionConfig(ctx context.Context, stack auto.Stack, region string) error {
	if region == "" {
		return nil
	}
	if err := stack.SetConfig(ctx, "aws:region", auto.ConfigValue{Value: region}); err != nil {
		return fmt.Errorf("aws: failed to set region config: %w", err)
	}
	return nil
}

// providerOpts builds the pulumi.ResourceOptions to forward to every
// resource declared in the program: empty when using the default
// credential chain (RFC 002 §2.4), or an explicit Pulumi provider with
// the assumed credentials when p.creds is set (RFC 004 §4, a resource
// applied against a DeploymentTarget).
func (p *AWSProvider) providerOpts(ctx *pulumi.Context, region string) ([]pulumi.ResourceOption, error) {
	if p.creds == nil {
		return nil, nil
	}

	args := &pulumiaws.ProviderArgs{
		AccessKey: pulumi.String(p.creds.AccessKeyID),
		SecretKey: pulumi.String(p.creds.SecretAccessKey),
		Token:     pulumi.String(p.creds.SessionToken),
	}
	if region != "" {
		args.Region = pulumi.String(region)
	}

	explicit, err := pulumiaws.NewProvider(ctx, "target", args)
	if err != nil {
		return nil, fmt.Errorf("aws: failed to build explicit provider for assumed credentials: %w", err)
	}
	return []pulumi.ResourceOption{pulumi.Provider(explicit)}, nil
}

// summarizeChangeSummary reduces the ChangeSummary of a Preview/Up
// (aggregated across all Pulumi resources declared for a single CloudSDD
// Resource, RFC 002 §2.7) to a single provider.Action.
func summarizeChangeSummary(cs map[apitype.OpType]int) provider.Action {
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

func changesFromSummary(cs map[apitype.OpType]int) map[string]any {
	if len(cs) == 0 {
		return nil
	}
	out := make(map[string]any, len(cs))
	for op, n := range cs {
		out[string(op)] = n
	}
	return out
}

// detailsFromOutputs converts Pulumi outputs into Result.Details. Outputs
// marked Secret are redacted: Result.Details can end up in logs/reports,
// which is not the right place for sensitive material even when Pulumi
// itself considers it a secret.
func detailsFromOutputs(outputs auto.OutputMap) map[string]any {
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
