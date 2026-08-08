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
- [x] Create `internal/provider/aws/pipeline_test.go`: write the mock test `TestAWSCodeBuildGeneration`, validating the correct structure of the IAM Role and policy (scoped only to the ARN of the ECR repository that was created).
- [x] Create `internal/provider/aws/pipeline.go`: implement the creation of the ECR Repository and the CodeBuild Project. Wire the lifecycle to the `retain` property.
- [x] Wire `build_pipeline` into the AWS provider's `Validate` and `resourceProgram` dispatch.
### The digest hand-off — decided
The mechanism is the **commit SHA** (RFC 018 §2.4.1, added). The revision
resolves to a SHA at plan time, the build tags with it, and the service
asks for `<registry>/<image_name>:<sha>`. Immutable registry tags are what
make that as strong as a digest. No cross-stack reference is needed.

- [x] Amend `docs/rfc/018-build-pipeline.md` with §2.4.1 recording the mechanism.
- [x] Create `internal/provider/pipeline/revision.go`: resolve a branch/tag to a commit over Git's smart-HTTP ref advertisement, with no new dependency.
- [x] Create `internal/provider/pipeline/revision_test.go`: branch, tag, annotated tag, ambiguity, unreachable repositories, and a bounded response.
- [x] Add `Resolved` to `spec.Resource` (no JSON tag, so a Specification can never set it) carrying the pipeline `image_name` and the resolved commit.
- [x] Create `internal/engine/build.go`: resolve each build_pipeline's revision once, and populate `Resolved` on every service that references it.
- [x] Update `internal/provider/aws/pipeline.go`: build the resolved commit rather than the branch, and assemble `<ecr-host>/<image_name>:<sha>` for a service that references a pipeline.
- [x] Update `internal/provider/aws/pipeline_test.go`: assert the assembled reference parses under RFC 017 §2.4's own grammar and is never `latest`.

**AWS is now complete end to end**: a Specification naming a repository and
a runtime provisions a registry and a build, builds the commit the plan
showed, and runs the resulting image.

## GCP Provider
- [x] Create `internal/provider/gcp/pipeline_test.go`: write mock test `TestGCPCloudBuildGeneration` verifying the absence of *project-wide* permissions on the generated Service Account.
- [x] Create `internal/provider/gcp/pipeline.go`: implement Cloud Build and Artifact Registry. Apply the Cleanup Policy for retention. *(Also required teaching `internal/provider/gcp/declare_test.go`'s mock monitor the repository's `project` output and the `getRepository` invoke — the provider computes both, and the two halves of §2.4.1 read them.)*
- [x] Wire `build_pipeline` into the GCP provider's `Validate` and `resourceProgram` dispatch. *(Not in the original plan; the AWS section needed the same item and the GCP one omitted it. Without it the type decodes and declares but is unreachable.)*
- [x] Update `internal/provider/gcp/container.go` and its test: assemble the Artifact Registry reference from the resolved commit. *(`declareContainerService` now takes the `spec.Resource` — as the AWS twin already does — so it can read `Resolved` and refuse an unresolved reference before registering anything, rather than the dispatch filling `Image` in as on AWS.)*
- [x] Ratchet the `internal/provider/gcp` floor in `scripts/coverage-gate.sh` once the section is complete. *(Not in the original plan; the AWS section carried the same item. The package is at 73.2% against a floor of 69%, and the floors ratchet upward only.)*

**GCP is now complete end to end**, on the same terms as AWS: a
Specification naming a repository and a runtime provisions an Artifact
Registry repository and a Cloud Build trigger, builds the commit the plan
showed, and runs the resulting image on Cloud Run.

## Azure Provider
- [ ] Create `internal/provider/azure/pipeline_test.go`: write `TestAzureACRTasks`, verifying that the SKU in ARM switches to `Premium` when `retain` > 0.
- [ ] Create `internal/provider/azure/pipeline.go`: implement Azure Container Registry and ACR Tasks. Dynamically enable the Premium SKU when a retention spec is present.
- [ ] Update `internal/provider/azure/container.go` and its test: assemble the ACR reference from the resolved commit.

## Network File Sharing (Volumes)
- [ ] Update `internal/spec/spec.go`: add the `Volumes` array to the resource declaration for `container_service` and `build_pipeline`.
- [ ] Create `internal/provider/aws/filesystem_test.go`: test the EFS mount target setup.
- [ ] Create `internal/provider/aws/filesystem.go`: implement the logical provisioning of AWS EFS inside the VPC of the scope.
- [ ] Create `internal/provider/gcp/filesystem_test.go`: test the Filestore instance setup.
- [ ] Create `internal/provider/gcp/filesystem.go`: implement the logical provisioning of GCP Filestore NFS.
- [ ] Create `internal/provider/azure/filesystem_test.go`: test the Azure Files mount setup.
- [ ] Create `internal/provider/azure/filesystem.go`: implement the logical provisioning of Azure Files, attaching it to Azure Container Apps.

## Documentation (RFC 018 §6 step 7)
- [ ] Update `docs/architecture.md`: the new resource type, the dependency graph, and the fact that cross-resource references now exist.
- [ ] Update `docs/cli.md`: what a build_pipeline looks like in a Specification and what the plan shows.
- [ ] Update the translator system prompt: the type, the runtime enum, and the rule that a repository URL is carried through and never invented.
