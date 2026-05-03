package observability

import (
	"strings"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
)

const (
	ServiceNamespace = "cleanroom"

	SpanExec            = "cleanroom.exec"
	SpanConsole         = "cleanroom.console"
	SpanSandboxCreate   = "cleanroom.sandbox.create"
	SpanExecutionCreate = "cleanroom.execution.create"
	SpanExecutionRun    = "cleanroom.execution.run"

	MetricSandboxCreateDurationSeconds = "cleanroom_sandbox_create_duration_seconds"
	MetricExecutionTotal               = "cleanroom_execution_total"
	MetricExecutionDurationSeconds     = "cleanroom_execution_duration_seconds"
	MetricGatewayRequestsTotal         = "cleanroom_gateway_requests_total"
	MetricGatewayRequestDuration       = "cleanroom_gateway_request_duration_seconds"
	MetricLaunchPhaseDurationSeconds   = "cleanroom_launch_phase_duration_seconds"
	MetricCachePeerLookupTotal         = "cleanroom_cache_peer_lookup_total"
	MetricCachePeerTransferBytesTotal  = "cleanroom_cache_peer_transfer_bytes_total"
	MetricCachePeerTransferDuration    = "cleanroom_cache_peer_transfer_duration_seconds"
	MetricCachePeerImportTotal         = "cleanroom_cache_peer_import_total"

	MetricLabelBackend     = "backend"
	MetricLabelStage       = "stage"
	MetricLabelSource      = "source"
	MetricLabelOutcome     = "outcome"
	MetricLabelKind        = "kind"
	MetricLabelResult      = "result"
	MetricLabelDirection   = "direction"
	MetricLabelService     = "service"
	MetricLabelAction      = "action"
	MetricLabelReasonCode  = "reason_code"
	MetricLabelStatusClass = "status_class"
	MetricLabelPhase       = "phase"

	AttrBackend                = "cleanroom.backend"
	AttrBackendRequested       = "cleanroom.backend.requested"
	AttrSandboxID              = "cleanroom.sandbox.id"
	AttrSandboxFromSnapshot    = "cleanroom.sandbox.from_snapshot"
	AttrExecutionID            = "cleanroom.execution.id"
	AttrExecutionKind          = "cleanroom.execution.kind"
	AttrExecutionStatus        = "cleanroom.execution.status"
	AttrReasonCode             = "cleanroom.reason_code"
	AttrGatewayService         = "cleanroom.gateway.service"
	AttrGatewayAction          = "cleanroom.gateway.action"
	AttrGatewayTargetHost      = "cleanroom.gateway.target_host"
	AttrGatewayRepoPath        = "cleanroom.gateway.repo_path"
	AttrGatewayRequestType     = "cleanroom.gateway.request_type"
	AttrGatewayRegistryPrefix  = "cleanroom.gateway.registry_prefix"
	AttrGatewayUpstreamHost    = "cleanroom.gateway.upstream_host"
	AttrGatewayUpstreamPort    = "cleanroom.gateway.upstream_port"
	AttrGatewayUpstreamStatus  = "cleanroom.gateway.upstream_status_code"
	AttrCommandArgc            = "cleanroom.command.argc"
	AttrCommandName            = "cleanroom.command.name"
	AttrCommandSummary         = "cleanroom.command.summary"
	AttrKeepSandbox            = "cleanroom.keep_sandbox"
	AttrStdinDisabled          = "cleanroom.stdin.disabled"
	AttrRepositoryCheckout     = "cleanroom.repository.checkout"
	AttrRepositoryChangeset    = "cleanroom.repository.changeset"
	AttrRepositoryChangesetID  = "cleanroom.repository.changeset_id"
	AttrRepositoryCommitBundle = "cleanroom.repository.commit_bundle"
	AttrRepositoryCommitSHA    = "cleanroom.repository.commit_sha"
	AttrCacheStage             = "cleanroom.cache.stage"
	AttrCacheOperation         = "cleanroom.cache.operation"
	AttrCacheResult            = "cleanroom.cache.result"
	AttrCacheLookupReason      = "cleanroom.cache.lookup_reason"
	AttrCachePeerCandidates    = "cleanroom.cache.peer.candidates"
	AttrCachePeerDirection     = "cleanroom.cache.peer.direction"
	AttrCachePeerBytes         = "cleanroom.cache.peer.bytes"
	AttrCachePeerFallback      = "cleanroom.cache.peer.fallback_reason"
	AttrVMLaunched             = "cleanroom.vm.launched"
	AttrExitCode               = "cleanroom.exit_code"

	LogFieldTraceID     = "trace_id"
	LogFieldSpanID      = "span_id"
	LogFieldExecutionID = "execution_id"
	LogFieldSandboxID   = "sandbox_id"
	LogFieldBackend     = "backend"
	LogFieldReasonCode  = "reason_code"
	LogFieldComponent   = "component"
	LogFieldSubsystem   = "subsystem"

	OutcomeSucceeded = "succeeded"
	OutcomeFailed    = "failed"
	OutcomeCanceled  = "canceled"
	OutcomeTimedOut  = "timed_out"

	GatewayActionAllow = "allow"
	GatewayActionDeny  = "deny"

	ReasonHostNotAllowed        = "host_not_allowed"
	ReasonMethodNotAllowed      = "method_not_allowed"
	ReasonInvalidRequest        = "invalid_request"
	ReasonUpstreamError         = "upstream_error"
	ReasonUnknownRegistryPrefix = "unknown_registry_prefix"
	ReasonProxied               = "proxied"
	ReasonMirrored              = "mirrored"
	ReasonCached                = "cached"
	ReasonFallback              = "fallback"
	ReasonRubyGemsUnavailable   = "rubygems_unavailable"

	CacheStageRuntime    = "runtime"
	CacheStageWorkspace  = "workspace"
	CacheStageDependency = "dependency"
	CacheStageServices   = "services"

	CacheOperationLookup     = "lookup"
	CacheOperationRestore    = "restore"
	CacheOperationPublish    = "publish"
	CacheOperationInvalidate = "invalidate"

	CacheResultHit       = "hit"
	CacheResultMiss      = "miss"
	CacheResultRestored  = "restored"
	CacheResultPublished = "published"
	CacheResultFallback  = "fallback"
	CacheResultFailed    = "failed"
	CacheResultImported  = "imported"
	CacheResultExported  = "exported"

	CachePeerDirectionImport = "import"
	CachePeerDirectionExport = "export"

	CacheLookupReasonRecordNotFound         = "cache_record_not_found"
	CacheLookupReasonBackendMismatch        = "backend_mismatch"
	CacheLookupReasonPolicyHashMismatch     = "policy_hash_mismatch"
	CacheLookupReasonRepositoryChanged      = "repository_changed"
	CacheLookupReasonParentStageChanged     = "parent_stage_changed"
	CacheLookupReasonWorkspaceParentChanged = "workspace_parent_changed"
)

func GatewayRequestSpanName(service string) string {
	return "cleanroom.gateway." + service + ".request"
}

func ExecutionOutcome(status cleanroomv1.ExecutionStatus) string {
	switch status {
	case cleanroomv1.ExecutionStatus_EXECUTION_STATUS_SUCCEEDED:
		return OutcomeSucceeded
	case cleanroomv1.ExecutionStatus_EXECUTION_STATUS_FAILED:
		return OutcomeFailed
	case cleanroomv1.ExecutionStatus_EXECUTION_STATUS_CANCELED:
		return OutcomeCanceled
	case cleanroomv1.ExecutionStatus_EXECUTION_STATUS_TIMED_OUT:
		return OutcomeTimedOut
	default:
		return strings.ToLower(strings.TrimPrefix(status.String(), "EXECUTION_STATUS_"))
	}
}
