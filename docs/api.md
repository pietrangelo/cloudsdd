# CloudSDD API

## Status: not implemented

No HTTP layer exists yet in the code (`internal/api` has not been
created). RFC 001 (`docs/rfc/001-core-architecture-and-json-schema.md`)
covers exclusively the engine core (`internal/spec`, `internal/provider`,
`internal/engine`): exposure via an HTTP API is explicitly out of scope
(RFC 001 §4) and will require a dedicated RFC, which must cover at least:

- routing and the choice between native `net/http` and Chi (CLAUDE.md,
  standard Go);
- authentication/identity propagated via `context.Context` (required for
  the BOLA prevention described in RFC 001 §3);
- rate limiting (`golang.org/x/time/rate`) at the middleware level;
- limits on payload size and maximum number of resources per request
  (Unrestricted Resource Consumption prevention);
- mapping between HTTP endpoints and the operations already defined in
  `engine.Engine` (`Validate`, `Plan`, `Apply`).

## Current programmatic usage (Go, no HTTP API)

Until the HTTP API is introduced, the engine can only be used as a Go
library internal to the module. Since RFC 002/003/004 a concrete AWS
provider exists (`internal/provider/aws`), which requires
`CLOUDSDD_PULUMI_PASSPHRASE` to be set in the environment (RFC 002 §2.3):

```go
import (
    "cloudsdd/internal/engine"
    "cloudsdd/internal/provider"
    awsprovider "cloudsdd/internal/provider/aws"
    "cloudsdd/internal/spec"
)

s, err := spec.ParseAndValidate(r) // r: io.Reader with the Specification JSON
if err != nil {
    // parsing or domain validation error
}

aws, err := awsprovider.NewProvider() // requires CLOUDSDD_PULUMI_PASSPHRASE
if err != nil {
    // fails explicitly if the passphrase is not set
}

e := engine.New(
    map[spec.Provider]provider.CloudProvider{
        spec.ProviderAWS: aws,
    },
    // Optional (RFC 004): DeploymentTarget to apply resources against
    // external AWS accounts via Resource.account.
    // engine.WithDeploymentTargets(targets),
    // engine.WithTargetProviderFactory(spec.ProviderAWS, awsprovider.NewTargetProviderFactory(aws)),
)

diffs, err := e.Plan(ctx, *s)
results, err := e.Apply(ctx, *s)
```

See `docs/openapi.yaml` for the `Specification` schema (including the
`S3ObjectStorageProperties` and `AWSCrossAccountRoleProperties`
components, reusable once the API is defined).
