# RFC 008: GCP and Azure Providers Implementation

## 1. Context and Problem Statement
The CloudSDD engine currently supports parsing cloud-agnostic schemas (like `object_storage` and `relational_database`) but only implements them for the `aws` provider. To fulfill the vision of a truly cloud-agnostic deployment engine, we must implement these resources for Google Cloud Platform (GCP) and Microsoft Azure.

## 2. Proposed Architecture

We will introduce two new Go packages:
- `internal/provider/gcp`
- `internal/provider/azure`

Both will implement the core `provider.CloudProvider` interface, allowing them to be registered seamlessly in the `DefaultEngine` within the CLI commands.

### 2.1 GCP Provider Mapping & Defaults
- **Object Storage** (`object_storage`): Maps to GCP Cloud Storage (`gcp.storage.Bucket`).
  - *State-of-the-art defaults*: `uniformBucketLevelAccess = true` (disables legacy ACLs), `publicAccessPrevention = "enforced"` (blocks public data), default CMEK or Google-managed encryption.
- **Relational Database** (`relational_database`): Maps to GCP Cloud SQL (`gcp.sql.DatabaseInstance`).
  - *State-of-the-art defaults*: Private IP only (no public IP), automated backups enabled, highly available regional deployment (if `high_availability` is true), storage auto-resize enabled.

### 2.2 Azure Provider Mapping & Defaults
- **Object Storage** (`object_storage`): Maps to Azure Blob Storage (`azure-native.storage.StorageAccount` and `BlobContainer`).
  - *State-of-the-art defaults*: `allowBlobPublicAccess = false`, `minimumTlsVersion = TLS1_2`, secure transfer required.
- **Relational Database** (`relational_database`): Maps to Azure Database Flexible Server (`azure-native.dbforpostgresql.FlexibleServer` / `dbformysql`).
  - *State-of-the-art defaults*: VNet integration (private access), encryption at rest, Geo-redundant backups, Zone-redundant HA (if `high_availability` is true).

### 2.3 Cross-Account Roles
The `cross_account_role` concept is highly AWS-specific in its current property mapping. To prevent scope creep, GCP and Azure providers will initially return `ErrUnsupportedResourceType` for `cross_account_role`, focusing strictly on the core data primitives (`object_storage` and `relational_database`). We can address cross-cloud identity federation in a future dedicated RFC.

## 3. Implementation Steps
1. **Initialize Pulumi SDKs**: Install the Pulumi GCP and Azure Native Go SDKs.
2. **Implement GCP**: Create `internal/provider/gcp/provider.go`, `storage.go`, and `database.go`.
3. **Implement Azure**: Create `internal/provider/azure/provider.go`, `storage.go`, and `database.go`.
4. **Wire into CLI**: Update `cmd/cloudsdd/deploy.go` and `destroy.go` to inject all three providers (`aws`, `gcp`, `azure`) into `engine.New()`.
5. **Update NLP Engine**: Ensure the Anthropic prompt context understands it can now safely output `provider: "gcp"` or `provider: "azure"`.

## 4. Security Considerations
- Each provider will adhere to the strict AppSec guidelines defined in our SDD paradigm: failing closed, relying strictly on the JSON Specification, and utilizing the cloud provider's highest tier of default security settings (e.g., blocking public access at the IAM/Resource level).
