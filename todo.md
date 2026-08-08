# CloudSDD Implementation Plan: RFC 018 (Build Pipeline)

## Data Model and Abstractions
- [x] Update `internal/spec/spec.go`: define the `ResourceTypeBuildPipeline = "build_pipeline"` constant.
- [x] Update `internal/provider/container/container.go`: extend the `container_service` struct to include the `Pipeline *string` pointer, and make the `Image` string field optional. *(The struct lives in `internal/provider/container`, not `internal/spec`, which keeps `Properties` a generic `map[string]any`.)*
- [x] Update `internal/provider/container/container_test.go`: implement `TestContainerServiceValidation` to verify that validation passes when ONLY `Image` or ONLY `Pipeline` is present. *(Same relocation as above; `internal/spec` cannot see container properties. `internal/spec/validate_test.go` covers the new resource type instead.)*
- [x] Update the three providers to call `ValidateImageSource`, so a pipeline-sourced service is exempt from the digest and allowlist rules (RFC 018 §2.4) and no other check is lost.
- [x] Create `internal/provider/pipeline/types.go`: define the `Runtime` enum and the `Source`, `Stack` and `BuildPipelineProperties` structs.
- [x] Update `docs/openapi.yaml`: add the `BuildPipelineProperties` JSON schema, in line with the specification.

## Dockerfile Generator
- [x] Create `internal/provider/pipeline/generator_test.go`: write `TestDockerfileGeneration` with the scaffolding to load *golden files* containing the expected Dockerfiles for Go, Node, Python and Java (non-root, distroless).
- [x] Create `internal/provider/pipeline/generator.go`: implement the `DockerfileGenerator` interface and its base constructor.
- [x] Update `internal/provider/pipeline/generator.go`: implement the `Generate` method containing the hardcoded Dockerfile logic (multi-stage, pinned digests, user isolation), satisfying the tests written in the previous step.
- [x] Pin real base image digests for every supported runtime version, resolved from the registries rather than placeheld.

## Core Engine: Dependency Graph
- [x] Create `internal/engine/graph_test.go`: implement `TestDependencyGraphValidation_Cycles` to catch circular dependencies in a fake Specification.
- [x] Create `internal/engine/graph_test.go`: implement `TestDependencyGraphValidation_PortAgreement` to ensure that `container.port` is included in `pipeline.ports`.
- [x] Create `internal/engine/graph_test.go`: implement `TestDependencyGraphValidation_SameProvider` to prevent an `agnostic` resolution from being split across two cloud providers.
- [x] Create `internal/engine/graph.go`: implement `DependencyGraph`, including the topological `Validate(ctx, spec)` function, satisfying all the tests written above.
- [x] Update `internal/engine/graph.go`: implement `Ordered` (topological traversal) and `ReverseOrdered`.
- [x] Update `internal/engine/default.go`: inject the call to `DependencyGraph.Validate` inside `Engine.Validate()`. *(`engine.go` holds only the interface; the implementation is in `default.go`.)*
- [x] Update `internal/engine/default_test.go`: verify the actual invocation order during Apply (using `Ordered`) and Destroy (using `ReverseOrdered`).
- [x] Update `internal/engine/default.go`: change `Apply()` and `Destroy()` so that they process the DAG, satisfying the tests.
- [x] Refuse duplicate resource ids in `DependencyGraph.Validate`. *(Not in the original plan: it was harmless until RFC 018 made resources referenceable by id, and a reference to a duplicated id names two resources.)*
- [x] Add a coverage floor for `internal/provider/pipeline` in `scripts/coverage-gate.sh`, and restore `internal/engine` above its own.

## AWS Provider
- [ ] Create `internal/provider/aws/pipeline_test.go`: write the mock test `TestAWSCodeBuildGeneration`, validating the correct structure of the IAM Role and policy (scoped only to the ARN of the ECR repository that was created).
- [ ] Create `internal/provider/aws/pipeline.go`: implement the creation of the ECR Repository and the CodeBuild Project. Wire the lifecycle to the `retain` property.
- [ ] Update `internal/provider/aws/container_test.go`: mock test validating the CodeBuild build-id query or the ECR tag resolution downstream of the run.
- [ ] Update `internal/provider/aws/container.go`: if `Pipeline` is referenced, dynamically retrieve the digest from ECR (via the AWS SDK) to feed the ECS container definition.

## GCP Provider
- [ ] Create `internal/provider/gcp/pipeline_test.go`: write the mock test `TestGCPCloudBuildGeneration`, verifying the absence of *project-wide* permissions on the generated Service Account.
- [ ] Create `internal/provider/gcp/pipeline.go`: implement Cloud Build and Artifact Registry. Apply the Cleanup Policy for retention.
- [ ] Update `internal/provider/gcp/container_test.go`: add the tests for wiring the Artifact Registry digest into Cloud Run.
- [ ] Update `internal/provider/gcp/container.go`: implement reading the post-build digest in order to instantiate the image in Google Cloud Run.

## Azure Provider
- [ ] Create `internal/provider/azure/pipeline_test.go`: write `TestAzureACRTasks`, verifying that the SKU in ARM switches to `Premium` when `retain` > 0.
- [ ] Create `internal/provider/azure/pipeline.go`: implement Azure Container Registry and ACR Tasks. Dynamically enable the Premium SKU when a retention spec is present.
- [ ] Update `internal/provider/azure/container_test.go`: add a mock test for ACR tag resolution.
- [ ] Update `internal/provider/azure/container.go`: handle reading the digest from the Container Registry to feed the Azure Container App.

## Network File Sharing (Volumes)
- [ ] Update `internal/spec/spec.go`: add the `Volumes` array to the resource declaration for `container_service` and `build_pipeline`.
- [ ] Create `internal/provider/aws/filesystem_test.go`: test the EFS mount target setup.
- [ ] Create `internal/provider/aws/filesystem.go`: implement the logical provisioning of AWS EFS inside the VPC of the scope.
- [ ] Create `internal/provider/gcp/filesystem_test.go`: test the Filestore instance setup.
- [ ] Create `internal/provider/gcp/filesystem.go`: implement the logical provisioning of GCP Filestore NFS.
- [ ] Create `internal/provider/azure/filesystem_test.go`: test the Azure Files mount setup.
- [ ] Create `internal/provider/azure/filesystem.go`: implement the logical provisioning of Azure Files, attaching it to Azure Container Apps.
