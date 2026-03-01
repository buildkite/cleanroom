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
)

type PolicyAllowRule = cleanroomv1.PolicyAllowRule
type Policy = cleanroomv1.Policy
type SandboxOptions = cleanroomv1.SandboxOptions
type CreateSandboxRequest = cleanroomv1.CreateSandboxRequest
type CreateSandboxResponse = cleanroomv1.CreateSandboxResponse
type GetSandboxRequest = cleanroomv1.GetSandboxRequest
type GetSandboxResponse = cleanroomv1.GetSandboxResponse
type ListSandboxesRequest = cleanroomv1.ListSandboxesRequest
type ListSandboxesResponse = cleanroomv1.ListSandboxesResponse
type DownloadSandboxFileRequest = cleanroomv1.DownloadSandboxFileRequest
type DownloadSandboxFileResponse = cleanroomv1.DownloadSandboxFileResponse
type TerminateSandboxRequest = cleanroomv1.TerminateSandboxRequest
type TerminateSandboxResponse = cleanroomv1.TerminateSandboxResponse
type StreamSandboxEventsRequest = cleanroomv1.StreamSandboxEventsRequest
type SandboxEvent = cleanroomv1.SandboxEvent

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
type CreateExecutionRequest = cleanroomv1.CreateExecutionRequest
type CreateExecutionResponse = cleanroomv1.CreateExecutionResponse
type OpenInteractiveExecutionRequest = cleanroomv1.OpenInteractiveExecutionRequest
type OpenInteractiveExecutionResponse = cleanroomv1.OpenInteractiveExecutionResponse
type GetExecutionRequest = cleanroomv1.GetExecutionRequest
type GetExecutionResponse = cleanroomv1.GetExecutionResponse
type CancelExecutionRequest = cleanroomv1.CancelExecutionRequest
type CancelExecutionResponse = cleanroomv1.CancelExecutionResponse
type StreamExecutionRequest = cleanroomv1.StreamExecutionRequest
type ExecutionExit = cleanroomv1.ExecutionExit
type ExecutionStreamEvent = cleanroomv1.ExecutionStreamEvent
type ExecutionAttachOpen = cleanroomv1.ExecutionAttachOpen
type ExecutionResize = cleanroomv1.ExecutionResize
type ExecutionSignal = cleanroomv1.ExecutionSignal
type ExecutionHeartbeat = cleanroomv1.ExecutionHeartbeat
type ExecutionClose = cleanroomv1.ExecutionClose
type ExecutionAttachFrame = cleanroomv1.ExecutionAttachFrame
