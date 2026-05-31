package client

import cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"

type Sandbox = cleanroomv1.Sandbox

type SandboxStatus = cleanroomv1.SandboxStatus

const (
	SandboxStatus_SANDBOX_STATUS_UNSPECIFIED  = cleanroomv1.SandboxStatus_SANDBOX_STATUS_UNSPECIFIED
	SandboxStatus_SANDBOX_STATUS_PROVISIONING = cleanroomv1.SandboxStatus_SANDBOX_STATUS_PROVISIONING
	SandboxStatus_SANDBOX_STATUS_READY        = cleanroomv1.SandboxStatus_SANDBOX_STATUS_READY
	SandboxStatus_SANDBOX_STATUS_STOPPING     = cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPING
	SandboxStatus_SANDBOX_STATUS_STOPPED      = cleanroomv1.SandboxStatus_SANDBOX_STATUS_STOPPED
	SandboxStatus_SANDBOX_STATUS_FAILED       = cleanroomv1.SandboxStatus_SANDBOX_STATUS_FAILED
	SandboxStatus_SANDBOX_STATUS_SUSPENDING   = cleanroomv1.SandboxStatus_SANDBOX_STATUS_SUSPENDING
	SandboxStatus_SANDBOX_STATUS_SUSPENDED    = cleanroomv1.SandboxStatus_SANDBOX_STATUS_SUSPENDED
	SandboxStatus_SANDBOX_STATUS_WAKING       = cleanroomv1.SandboxStatus_SANDBOX_STATUS_WAKING
)

type PolicyAllowRule = cleanroomv1.PolicyAllowRule
type PolicyDocker = cleanroomv1.PolicyDocker
type PolicyBlockInputs = cleanroomv1.PolicyBlockInputs
type PolicyBlockOutputs = cleanroomv1.PolicyBlockOutputs
type PolicyBlock = cleanroomv1.PolicyBlock
type PolicyServices = cleanroomv1.PolicyServices
type PolicyDependencies = cleanroomv1.PolicyDependencies
type PolicyRun = cleanroomv1.PolicyRun
type PolicyNetwork = cleanroomv1.PolicyNetwork
type PolicyNetworkStages = cleanroomv1.PolicyNetworkStages
type PolicyResources = cleanroomv1.PolicyResources
type SandboxResources = cleanroomv1.SandboxResources
type Policy = cleanroomv1.Policy
type Snapshot = cleanroomv1.Snapshot
type SandboxOptions = cleanroomv1.SandboxOptions
type RepositoryCheckout = cleanroomv1.RepositoryCheckout
type RepositoryChangesetFile = cleanroomv1.RepositoryChangesetFile
type RepositoryChangeset = cleanroomv1.RepositoryChangeset
type RepositoryCommitBundle = cleanroomv1.RepositoryCommitBundle
type CreateSandboxRequest = cleanroomv1.CreateSandboxRequest
type CreateSandboxResponse = cleanroomv1.CreateSandboxResponse
type CreateSandboxPhase = cleanroomv1.CreateSandboxPhase
type CreateSandboxEvent = cleanroomv1.CreateSandboxEvent
type GetSandboxRequest = cleanroomv1.GetSandboxRequest
type GetSandboxResponse = cleanroomv1.GetSandboxResponse
type ListSandboxesRequest = cleanroomv1.ListSandboxesRequest
type ListSandboxesResponse = cleanroomv1.ListSandboxesResponse
type DownloadSandboxFileRequest = cleanroomv1.DownloadSandboxFileRequest
type DownloadSandboxFileResponse = cleanroomv1.DownloadSandboxFileResponse
type UploadSandboxFileRequest = cleanroomv1.UploadSandboxFileRequest
type UploadSandboxFileResponse = cleanroomv1.UploadSandboxFileResponse
type SandboxPathType = cleanroomv1.SandboxPathType

const (
	SandboxPathType_SANDBOX_PATH_TYPE_UNSPECIFIED = cleanroomv1.SandboxPathType_SANDBOX_PATH_TYPE_UNSPECIFIED
	SandboxPathType_SANDBOX_PATH_TYPE_FILE        = cleanroomv1.SandboxPathType_SANDBOX_PATH_TYPE_FILE
	SandboxPathType_SANDBOX_PATH_TYPE_DIRECTORY   = cleanroomv1.SandboxPathType_SANDBOX_PATH_TYPE_DIRECTORY
	SandboxPathType_SANDBOX_PATH_TYPE_SYMLINK     = cleanroomv1.SandboxPathType_SANDBOX_PATH_TYPE_SYMLINK
	SandboxPathType_SANDBOX_PATH_TYPE_OTHER       = cleanroomv1.SandboxPathType_SANDBOX_PATH_TYPE_OTHER
)

type SandboxPathInfo = cleanroomv1.SandboxPathInfo
type StatSandboxPathRequest = cleanroomv1.StatSandboxPathRequest
type StatSandboxPathResponse = cleanroomv1.StatSandboxPathResponse
type WalkSandboxTreeRequest = cleanroomv1.WalkSandboxTreeRequest
type WalkSandboxTreeResponse = cleanroomv1.WalkSandboxTreeResponse
type ReadSandboxFileRequest = cleanroomv1.ReadSandboxFileRequest
type ReadSandboxFileResponse = cleanroomv1.ReadSandboxFileResponse
type WriteSandboxFileInit = cleanroomv1.WriteSandboxFileInit
type WriteSandboxFileRequest = cleanroomv1.WriteSandboxFileRequest
type WriteSandboxFileResponse = cleanroomv1.WriteSandboxFileResponse
type RemoveSandboxPathRequest = cleanroomv1.RemoveSandboxPathRequest
type RemoveSandboxPathResponse = cleanroomv1.RemoveSandboxPathResponse
type ArchiveSandboxPathsRequest = cleanroomv1.ArchiveSandboxPathsRequest
type ArchiveSandboxPathsResponse = cleanroomv1.ArchiveSandboxPathsResponse
type ExtractSandboxArchiveInit = cleanroomv1.ExtractSandboxArchiveInit
type ExtractSandboxArchiveRequest = cleanroomv1.ExtractSandboxArchiveRequest
type ExtractSandboxArchiveResponse = cleanroomv1.ExtractSandboxArchiveResponse
type SuspendSandboxRequest = cleanroomv1.SuspendSandboxRequest
type SuspendSandboxResponse = cleanroomv1.SuspendSandboxResponse
type ResumeSandboxRequest = cleanroomv1.ResumeSandboxRequest
type ResumeSandboxResponse = cleanroomv1.ResumeSandboxResponse
type TerminateSandboxRequest = cleanroomv1.TerminateSandboxRequest
type TerminateSandboxResponse = cleanroomv1.TerminateSandboxResponse
type StreamSandboxEventsRequest = cleanroomv1.StreamSandboxEventsRequest
type SandboxEvent = cleanroomv1.SandboxEvent
type CreateSnapshotRequest = cleanroomv1.CreateSnapshotRequest
type CreateSnapshotResponse = cleanroomv1.CreateSnapshotResponse
type GetSnapshotRequest = cleanroomv1.GetSnapshotRequest
type GetSnapshotResponse = cleanroomv1.GetSnapshotResponse
type ListSnapshotsRequest = cleanroomv1.ListSnapshotsRequest
type ListSnapshotsResponse = cleanroomv1.ListSnapshotsResponse
type DeleteSnapshotRequest = cleanroomv1.DeleteSnapshotRequest
type DeleteSnapshotResponse = cleanroomv1.DeleteSnapshotResponse

type Execution = cleanroomv1.Execution

type ExecutionStatus = cleanroomv1.ExecutionStatus

const (
	ExecutionStatus_EXECUTION_STATUS_UNSPECIFIED = cleanroomv1.ExecutionStatus_EXECUTION_STATUS_UNSPECIFIED
	ExecutionStatus_EXECUTION_STATUS_QUEUED      = cleanroomv1.ExecutionStatus_EXECUTION_STATUS_QUEUED
	ExecutionStatus_EXECUTION_STATUS_RUNNING     = cleanroomv1.ExecutionStatus_EXECUTION_STATUS_RUNNING
	ExecutionStatus_EXECUTION_STATUS_SUCCEEDED   = cleanroomv1.ExecutionStatus_EXECUTION_STATUS_SUCCEEDED
	ExecutionStatus_EXECUTION_STATUS_FAILED      = cleanroomv1.ExecutionStatus_EXECUTION_STATUS_FAILED
	ExecutionStatus_EXECUTION_STATUS_CANCELED    = cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED
	ExecutionStatus_EXECUTION_STATUS_TIMED_OUT   = cleanroomv1.ExecutionStatus_EXECUTION_STATUS_TIMED_OUT
)

type ExecutionKind = cleanroomv1.ExecutionKind

const (
	ExecutionKind_EXECUTION_KIND_UNSPECIFIED = cleanroomv1.ExecutionKind_EXECUTION_KIND_UNSPECIFIED
	ExecutionKind_EXECUTION_KIND_BATCH       = cleanroomv1.ExecutionKind_EXECUTION_KIND_BATCH
	ExecutionKind_EXECUTION_KIND_INTERACTIVE = cleanroomv1.ExecutionKind_EXECUTION_KIND_INTERACTIVE
)

type ExecutionOptions = cleanroomv1.ExecutionOptions
type ListExecutionsRequest = cleanroomv1.ListExecutionsRequest
type ListExecutionsResponse = cleanroomv1.ListExecutionsResponse
type CreateExecutionRequest = cleanroomv1.CreateExecutionRequest
type CreateExecutionResponse = cleanroomv1.CreateExecutionResponse
type AttachExecutionRequest = cleanroomv1.AttachExecutionRequest
type AttachExecutionResponse = cleanroomv1.AttachExecutionResponse
type GetExecutionRequest = cleanroomv1.GetExecutionRequest
type GetExecutionResponse = cleanroomv1.GetExecutionResponse
type InspectExecutionRequest = cleanroomv1.InspectExecutionRequest
type InspectExecutionResponse = cleanroomv1.InspectExecutionResponse
type CancelExecutionRequest = cleanroomv1.CancelExecutionRequest
type CancelExecutionResponse = cleanroomv1.CancelExecutionResponse
type WriteExecutionStdinRequest = cleanroomv1.WriteExecutionStdinRequest
type WriteExecutionStdinResponse = cleanroomv1.WriteExecutionStdinResponse
type CloseExecutionStdinRequest = cleanroomv1.CloseExecutionStdinRequest
type CloseExecutionStdinResponse = cleanroomv1.CloseExecutionStdinResponse
type StreamExecutionRequest = cleanroomv1.StreamExecutionRequest
type ExecutionExit = cleanroomv1.ExecutionExit
type ExecutionStreamEvent = cleanroomv1.ExecutionStreamEvent
