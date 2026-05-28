package controlservice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/buildkite/cleanroom/internal/authz"
	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/cachestore"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/gen/cleanroom/v1/cleanroomv1connect"
	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/buildkite/cleanroom/internal/policy"
	"github.com/buildkite/cleanroom/internal/repositorychangeset"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
	"github.com/buildkite/cleanroom/internal/runtimeconfig"
	"github.com/buildkite/cleanroom/internal/volumestore"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"
)

const cachePeerZFSIncrementalExportPathPrefix = "/v1/cache/export/zfs-incremental/"

const (
	cachePeerLookupTimeout               = 5 * time.Second
	cachePeerExportDialTimeout           = 5 * time.Second
	cachePeerExportReadIdleTimeout       = 30 * time.Second
	cachePeerExportResponseHeaderTimeout = 10 * time.Second
)

var (
	cachePeerLookupHTTPClient = &http.Client{
		Timeout: cachePeerLookupTimeout,
	}
	cachePeerExportHTTPClient = &http.Client{
		Transport: newCachePeerExportTransport(),
	}
)

// Export streams can be much larger than lookup RPCs, so avoid a fixed total
// client timeout and bound setup plus idle reads instead.
func newCachePeerExportTransport() *http.Transport {
	dialer := &net.Dialer{
		Timeout:   cachePeerExportDialTimeout,
		KeepAlive: 30 * time.Second,
	}
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return &cachePeerTimeoutConn{
				Conn:            conn,
				readIdleTimeout: cachePeerExportReadIdleTimeout,
			}, nil
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		TLSHandshakeTimeout:   cachePeerExportDialTimeout,
		ResponseHeaderTimeout: cachePeerExportResponseHeaderTimeout,
		ExpectContinueTimeout: time.Second,
		IdleConnTimeout:       90 * time.Second,
	}
}

type cachePeerTimeoutConn struct {
	net.Conn
	readIdleTimeout time.Duration
}

func (c *cachePeerTimeoutConn) Read(p []byte) (int, error) {
	if c.readIdleTimeout > 0 {
		if err := c.Conn.SetReadDeadline(time.Now().Add(c.readIdleTimeout)); err != nil {
			return 0, err
		}
	}
	return c.Conn.Read(p)
}

type cachePeerImportResult struct {
	record   cachestore.Record
	imported bool
}

type cachePeerImportOptions struct {
	Stage           string
	CacheKey        string
	ParentStage     string
	ParentCacheKey  string
	Backend         string
	StorageDriver   string
	ProducerVersion string
	PolicyHash      string
	NewRecord       func(snapshotID string, snapshot volumestore.Snapshot, now time.Time) cachestore.Record
	ValidateRecord  func(context.Context) (cachestore.Record, bool, string, error)
}

type cachePeerCandidateMatch struct {
	peer      runtimeconfig.CachePeerConfig
	token     string
	candidate *cleanroomv1.CachePeerCandidate
}

func (s *Service) importDependencyStageCacheFromPeers(
	ctx context.Context,
	adapter backend.SnapshottingAdapter,
	backendName string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	repository *repositorycheckout.Checkout,
	changeset *repositorychangeset.Changeset,
	plan dependencyStagePlan,
) (cachestore.Record, bool, error) {
	if adapter == nil || compiled == nil || repository == nil || strings.TrimSpace(plan.CacheKey) == "" {
		return cachestore.Record{}, false, nil
	}
	result, err, _ := s.cachePeerImports.Do(cachePeerImportSingleflightKey(dependencyStageName, plan.CacheKey), func() (any, error) {
		return s.importCachePeerStage(ctx, adapter, firecrackerCfg, cachePeerImportOptions{
			Stage:           dependencyStageName,
			CacheKey:        plan.CacheKey,
			ParentStage:     workspaceStageName,
			ParentCacheKey:  plan.ParentWorkspaceCacheKey,
			Backend:         backendName,
			StorageDriver:   "zfs",
			ProducerVersion: dependencyStageProducerVersion,
			PolicyHash:      compiled.Hash,
			NewRecord: func(snapshotID string, snapshot volumestore.Snapshot, now time.Time) cachestore.Record {
				record := cachestore.Record{
					CacheKey:                 plan.CacheKey,
					Stage:                    dependencyStageName,
					ReuseMode:                dependencyStageReuseExact,
					State:                    cacheStateReady,
					BackingSnapshotID:        strings.TrimSpace(snapshotID),
					Backend:                  backendName,
					PolicyHash:               compiled.Hash,
					Policy:                   compiled.ToProto(),
					Repository:               cloneRepositoryCheckout(normalizeRepositoryCheckoutForComparison(repository)).ToProto(),
					RepositoryHasChangeset:   changeset != nil,
					RepositoryChangesetID:    repositoryChangesetID(repository, changeset),
					ParentCacheKey:           plan.ParentWorkspaceCacheKey,
					StorageDriver:            "zfs",
					StorageRef:               strings.TrimSpace(snapshot.StorageRef),
					DependencyKeyFilesDigest: plan.KeyFilesDigest,
					ImportedFromPeer:         true,
					CreatedAt:                now,
					LastUsedAt:               now,
					ProducerVersion:          dependencyStageProducerVersion,
				}
				populateStageCacheRecordMetadata(&record, plan.ParentRuntimeCacheKey, snapshotResultFromVolumeSnapshot(snapshot), now)
				return record
			},
			ValidateRecord: func(ctx context.Context) (cachestore.Record, bool, string, error) {
				return s.lookupDependencyStageCache(ctx, backendName, compiled, repository, changeset, plan)
			},
		})
	})
	if err != nil {
		return cachestore.Record{}, false, err
	}
	importResult, ok := result.(cachePeerImportResult)
	if !ok {
		return cachestore.Record{}, false, nil
	}
	if importResult.imported && strings.TrimSpace(plan.PortableCacheKey) != "" {
		if store, storeErr := s.cacheStoreOrErr(); storeErr == nil {
			portableRecord := portableDependencyStageRecordFromExactRecord(importResult.record, compiled, repository, changeset, plan)
			if err := store.Upsert(ctx, portableRecord); err != nil && s.Logger != nil {
				s.Logger.Warn("persist portable dependency stage cache after peer import", "error", err)
			}
		}
	}
	return importResult.record, importResult.imported, nil
}

func (s *Service) importServicesStageCacheFromPeers(
	ctx context.Context,
	adapter backend.SnapshottingAdapter,
	backendName string,
	compiled *policy.CompiledPolicy,
	firecrackerCfg backend.FirecrackerConfig,
	repository *repositorycheckout.Checkout,
	changeset *repositorychangeset.Changeset,
	plan servicesStagePlan,
) (cachestore.Record, bool, error) {
	if adapter == nil || compiled == nil || repository == nil || strings.TrimSpace(plan.CacheKey) == "" {
		return cachestore.Record{}, false, nil
	}
	parentStage := workspaceStageName
	if record, ok, err := s.cacheRecordByKey(ctx, dependencyStageName, plan.ParentStageCacheKey); err == nil && ok && strings.TrimSpace(record.ReuseMode) != dependencyStageReusePortable {
		parentStage = dependencyStageName
	}
	result, err, _ := s.cachePeerImports.Do(cachePeerImportSingleflightKey(servicesStageName, plan.CacheKey), func() (any, error) {
		return s.importCachePeerStage(ctx, adapter, firecrackerCfg, cachePeerImportOptions{
			Stage:           servicesStageName,
			CacheKey:        plan.CacheKey,
			ParentStage:     parentStage,
			ParentCacheKey:  plan.ParentStageCacheKey,
			Backend:         backendName,
			StorageDriver:   "zfs",
			ProducerVersion: servicesStageProducerVersion,
			PolicyHash:      compiled.Hash,
			NewRecord: func(snapshotID string, snapshot volumestore.Snapshot, now time.Time) cachestore.Record {
				record := cachestore.Record{
					CacheKey:               plan.CacheKey,
					Stage:                  servicesStageName,
					State:                  cacheStateReady,
					BackingSnapshotID:      strings.TrimSpace(snapshotID),
					Backend:                backendName,
					PolicyHash:             compiled.Hash,
					Policy:                 compiled.ToProto(),
					Repository:             cloneRepositoryCheckout(normalizeRepositoryCheckoutForComparison(repository)).ToProto(),
					RepositoryHasChangeset: changeset != nil,
					RepositoryChangesetID:  repositoryChangesetID(repository, changeset),
					ParentCacheKey:         plan.ParentStageCacheKey,
					StorageDriver:          "zfs",
					StorageRef:             strings.TrimSpace(snapshot.StorageRef),
					ImportedFromPeer:       true,
					CreatedAt:              now,
					LastUsedAt:             now,
					ProducerVersion:        servicesStageProducerVersion,
				}
				populateStageCacheRecordMetadata(&record, plan.RuntimeBaseKey, snapshotResultFromVolumeSnapshot(snapshot), now)
				return record
			},
			ValidateRecord: func(ctx context.Context) (cachestore.Record, bool, string, error) {
				return s.lookupServicesStageCache(ctx, backendName, compiled, repository, changeset, plan)
			},
		})
	})
	if err != nil {
		return cachestore.Record{}, false, err
	}
	importResult, ok := result.(cachePeerImportResult)
	if !ok {
		return cachestore.Record{}, false, nil
	}
	return importResult.record, importResult.imported, nil
}

func (s *Service) importCachePeerStage(ctx context.Context, adapter backend.SnapshottingAdapter, firecrackerCfg backend.FirecrackerConfig, opts cachePeerImportOptions) (cachePeerImportResult, error) {
	if len(s.Config.Cache.Peers) == 0 || s.CachePeerTransferDriver == nil || opts.NewRecord == nil || opts.ValidateRecord == nil {
		return cachePeerImportResult{}, nil
	}
	if _, ok := authz.BoundPrincipalFromContext(ctx); ok {
		s.recordCachePeerImport(ctx, opts.Stage, observability.CacheResultFallback)
		trace.SpanFromContext(ctx).SetAttributes(attribute.String(observability.AttrCachePeerFallback, "cache peer import disabled for authenticated principal"))
		return cachePeerImportResult{}, nil
	}
	if opts.Backend != "firecracker" || opts.StorageDriver != "zfs" {
		return cachePeerImportResult{}, nil
	}
	if runtimeconfig.SnapshotDriverOrDefault(opts.Backend, firecrackerCfg.Snapshots.Driver) != "zfs" {
		return cachePeerImportResult{}, nil
	}

	store, err := s.cacheStoreOrErr()
	if err != nil {
		return cachePeerImportResult{}, nil
	}
	parent, ok, err := lookupReadyCacheRecord(ctx, store, opts.ParentStage, opts.ParentCacheKey)
	if err != nil {
		return cachePeerImportResult{}, err
	}
	if !ok {
		s.recordCachePeerImport(ctx, opts.Stage, observability.CacheResultFallback)
		trace.SpanFromContext(ctx).SetAttributes(attribute.String(observability.AttrCachePeerFallback, "parent record not found"))
		return cachePeerImportResult{}, nil
	}
	if strings.TrimSpace(parent.Backend) != opts.Backend || strings.TrimSpace(parent.StorageDriver) != opts.StorageDriver {
		s.recordCachePeerImport(ctx, opts.Stage, observability.CacheResultFallback)
		trace.SpanFromContext(ctx).SetAttributes(attribute.String(observability.AttrCachePeerFallback, "parent record incompatible"))
		return cachePeerImportResult{}, nil
	}
	parentDesc, err := s.CachePeerTransferDriver.DescribeSnapshot(ctx, volumestore.DescribeSnapshotRequest{
		SnapshotRef: strings.TrimSpace(parent.StorageRef),
	})
	if err != nil || strings.TrimSpace(parentDesc.SnapshotGUID) == "" {
		s.recordCachePeerImport(ctx, opts.Stage, observability.CacheResultFallback)
		trace.SpanFromContext(ctx).SetAttributes(attribute.String(observability.AttrCachePeerFallback, "parent snapshot metadata unavailable"))
		return cachePeerImportResult{}, err
	}

	lookupReq := &cleanroomv1.LookupCachePeerRequest{
		Stage:                 opts.Stage,
		CacheKey:              opts.CacheKey,
		Backend:               opts.Backend,
		StorageDriver:         opts.StorageDriver,
		Architecture:          runtime.GOARCH,
		ProducerVersion:       opts.ProducerVersion,
		PolicyHash:            opts.PolicyHash,
		ParentStage:           opts.ParentStage,
		ParentCacheKey:        opts.ParentCacheKey,
		ParentZfsSnapshotGuid: parentDesc.SnapshotGUID,
	}
	lookupCtx, cancelLookups := context.WithCancel(ctx)
	defer cancelLookups()
	candidateCount := 0
	for match := range s.lookupCachePeerImportCandidates(lookupCtx, lookupReq) {
		candidateCount++
		result, handled, err := s.importCachePeerCandidate(ctx, adapter, store, parent, parentDesc, firecrackerCfg, opts, match)
		if err != nil || handled {
			trace.SpanFromContext(ctx).SetAttributes(attribute.Int(observability.AttrCachePeerCandidates, candidateCount))
			cancelLookups()
			return result, err
		}
	}
	trace.SpanFromContext(ctx).SetAttributes(attribute.Int(observability.AttrCachePeerCandidates, candidateCount))
	if candidateCount == 0 {
		s.recordCachePeerImport(ctx, opts.Stage, observability.CacheResultFallback)
		trace.SpanFromContext(ctx).SetAttributes(attribute.String(observability.AttrCachePeerFallback, "no peer candidate"))
	}
	return cachePeerImportResult{}, nil
}

func (s *Service) importCachePeerCandidate(
	ctx context.Context,
	adapter backend.SnapshottingAdapter,
	store cacheMetadataStore,
	parent cachestore.Record,
	parentDesc volumestore.SnapshotDescription,
	firecrackerCfg backend.FirecrackerConfig,
	opts cachePeerImportOptions,
	match cachePeerCandidateMatch,
) (cachePeerImportResult, bool, error) {
	exportResp, err := s.openCachePeerExport(ctx, match)
	if err != nil {
		s.recordCachePeerImport(ctx, opts.Stage, observability.CacheResultFallback)
		trace.SpanFromContext(ctx).SetAttributes(attribute.String(observability.AttrCachePeerFallback, "export failed"))
		s.logCachePeerImportFallback(ctx, opts.Stage, match.peer.URL, "export failed", err)
		return cachePeerImportResult{}, false, nil
	}
	defer exportResp.Body.Close()

	snapshotID := newSnapshotID()
	stream := &cachePeerCountingReadCloser{ReadCloser: exportResp.Body}
	transferStarted := s.clock().Now()
	imported, err := s.CachePeerTransferDriver.ImportIncrementalSnapshot(ctx, volumestore.IncrementalSnapshotImportRequest{
		SnapshotID:           snapshotID,
		ParentSnapshotRef:    strings.TrimSpace(parent.StorageRef),
		ParentSnapshotGUID:   parentDesc.SnapshotGUID,
		ExpectedSnapshotGUID: match.candidate.GetZfsSnapshotGuid(),
	}, stream)
	transferDuration := s.clock().Now().Sub(transferStarted)
	if err != nil {
		s.recordCachePeerTransfer(ctx, opts.Stage, observability.CachePeerDirectionImport, observability.CacheResultFailed, stream.bytes, transferDuration)
		s.recordCachePeerImport(ctx, opts.Stage, observability.CacheResultFallback)
		trace.SpanFromContext(ctx).SetAttributes(attribute.String(observability.AttrCachePeerFallback, "import failed"))
		s.logCachePeerImportFallback(ctx, opts.Stage, match.peer.URL, "import failed", err)
		return cachePeerImportResult{}, false, nil
	}
	if err := validateImportedCachePeerSnapshot(imported, parentDesc.SnapshotGUID, match.candidate.GetZfsSnapshotGuid()); err != nil {
		s.recordCachePeerTransfer(ctx, opts.Stage, observability.CachePeerDirectionImport, observability.CacheResultFailed, stream.bytes, transferDuration)
		s.recordCachePeerImport(ctx, opts.Stage, observability.CacheResultFallback)
		trace.SpanFromContext(ctx).SetAttributes(attribute.String(observability.AttrCachePeerFallback, "import validation failed"))
		s.logCachePeerImportFallback(ctx, opts.Stage, match.peer.URL, "import validation failed", err)
		_ = adapter.DeleteSnapshot(context.Background(), backend.DeleteSnapshotRequest{
			SnapshotID:        snapshotID,
			StorageRef:        imported.StorageRef,
			FirecrackerConfig: withSnapshotDriver(opts.Backend, firecrackerCfg, opts.StorageDriver),
		})
		return cachePeerImportResult{}, false, nil
	}

	record := opts.NewRecord(snapshotID, imported, s.clock().Now())
	stampCacheRecordOwner(ctx, &record)
	if err := store.Create(ctx, record); err != nil {
		existing, found, _, lookupErr := opts.ValidateRecord(ctx)
		_ = adapter.DeleteSnapshot(context.Background(), backend.DeleteSnapshotRequest{
			SnapshotID:        snapshotID,
			StorageRef:        imported.StorageRef,
			FirecrackerConfig: withSnapshotDriver(opts.Backend, firecrackerCfg, opts.StorageDriver),
		})
		if lookupErr != nil {
			s.recordCachePeerTransfer(ctx, opts.Stage, observability.CachePeerDirectionImport, observability.CacheResultFailed, stream.bytes, transferDuration)
			s.recordCachePeerImport(ctx, opts.Stage, observability.CacheResultFallback)
			trace.SpanFromContext(ctx).SetAttributes(attribute.String(observability.AttrCachePeerFallback, "metadata conflict lookup failed"))
			s.logCachePeerImportFallback(ctx, opts.Stage, match.peer.URL, "metadata conflict lookup failed", lookupErr)
			return cachePeerImportResult{}, true, fmt.Errorf("persist imported cache peer record: %w (lookup after conflict failed: %v)", err, lookupErr)
		}
		if found {
			s.recordCachePeerTransfer(ctx, opts.Stage, observability.CachePeerDirectionImport, observability.CacheResultImported, stream.bytes, transferDuration)
			s.recordCachePeerImport(ctx, opts.Stage, observability.CacheResultImported)
			s.logCachePeerImportCompleted(ctx, opts.Stage, match.peer.URL, stream.bytes, transferDuration)
			return cachePeerImportResult{record: existing, imported: true}, true, nil
		}
		s.recordCachePeerTransfer(ctx, opts.Stage, observability.CachePeerDirectionImport, observability.CacheResultFailed, stream.bytes, transferDuration)
		s.recordCachePeerImport(ctx, opts.Stage, observability.CacheResultFallback)
		trace.SpanFromContext(ctx).SetAttributes(attribute.String(observability.AttrCachePeerFallback, "metadata create failed"))
		s.logCachePeerImportFallback(ctx, opts.Stage, match.peer.URL, "metadata create failed", err)
		return cachePeerImportResult{}, true, fmt.Errorf("persist imported cache peer record: %w", err)
	}

	validated, found, reason, err := opts.ValidateRecord(ctx)
	if err != nil {
		_ = deleteCacheRecord(context.Background(), store, record)
		_ = adapter.DeleteSnapshot(context.Background(), backend.DeleteSnapshotRequest{
			SnapshotID:        snapshotID,
			StorageRef:        imported.StorageRef,
			FirecrackerConfig: withSnapshotDriver(opts.Backend, firecrackerCfg, opts.StorageDriver),
		})
		s.recordCachePeerTransfer(ctx, opts.Stage, observability.CachePeerDirectionImport, observability.CacheResultFailed, stream.bytes, transferDuration)
		s.recordCachePeerImport(ctx, opts.Stage, observability.CacheResultFallback)
		trace.SpanFromContext(ctx).SetAttributes(attribute.String(observability.AttrCachePeerFallback, "metadata validation failed"))
		s.logCachePeerImportFallback(ctx, opts.Stage, match.peer.URL, "metadata validation failed", err)
		return cachePeerImportResult{}, true, err
	}
	if !found {
		_ = deleteCacheRecord(context.Background(), store, record)
		_ = adapter.DeleteSnapshot(context.Background(), backend.DeleteSnapshotRequest{
			SnapshotID:        snapshotID,
			StorageRef:        imported.StorageRef,
			FirecrackerConfig: withSnapshotDriver(opts.Backend, firecrackerCfg, opts.StorageDriver),
		})
		if strings.TrimSpace(reason) == "" {
			reason = "unknown validation miss"
		}
		s.recordCachePeerTransfer(ctx, opts.Stage, observability.CachePeerDirectionImport, observability.CacheResultFailed, stream.bytes, transferDuration)
		s.recordCachePeerImport(ctx, opts.Stage, observability.CacheResultFallback)
		trace.SpanFromContext(ctx).SetAttributes(attribute.String(observability.AttrCachePeerFallback, reason))
		s.logCachePeerImportFallback(ctx, opts.Stage, match.peer.URL, reason, nil)
		return cachePeerImportResult{}, true, fmt.Errorf("imported cache peer record failed validation: %s", reason)
	}
	s.recordCachePeerTransfer(ctx, opts.Stage, observability.CachePeerDirectionImport, observability.CacheResultImported, stream.bytes, transferDuration)
	s.recordCachePeerImport(ctx, opts.Stage, observability.CacheResultImported)
	s.logCachePeerImportCompleted(ctx, opts.Stage, match.peer.URL, stream.bytes, transferDuration)
	return cachePeerImportResult{record: validated, imported: true}, true, nil
}

type cachePeerLookupTarget struct {
	peer  runtimeconfig.CachePeerConfig
	token string
}

func (s *Service) lookupCachePeerImportCandidates(ctx context.Context, req *cleanroomv1.LookupCachePeerRequest) <-chan cachePeerCandidateMatch {
	targets := make([]cachePeerLookupTarget, 0, len(s.Config.Cache.Peers))
	for _, peer := range s.Config.Cache.Peers {
		token := cachePeerTokenForConfig(peer)
		if strings.TrimSpace(peer.URL) == "" || token == "" {
			continue
		}
		targets = append(targets, cachePeerLookupTarget{peer: peer, token: token})
	}
	matches := make(chan cachePeerCandidateMatch)
	if len(targets) == 0 {
		close(matches)
		return matches
	}
	var wg sync.WaitGroup
	wg.Add(len(targets))
	for _, target := range targets {
		target := target
		lookupReq := proto.Clone(req).(*cleanroomv1.LookupCachePeerRequest)
		go func() {
			defer wg.Done()
			match, ok := s.lookupCachePeerImportCandidate(ctx, lookupReq, target)
			if !ok {
				return
			}
			select {
			case matches <- match:
			case <-ctx.Done():
			}
		}()
	}
	go func() {
		wg.Wait()
		close(matches)
	}()
	return matches
}

func (s *Service) lookupCachePeerImportCandidate(ctx context.Context, req *cleanroomv1.LookupCachePeerRequest, target cachePeerLookupTarget) (cachePeerCandidateMatch, bool) {
	client := cleanroomv1connect.NewCachePeerServiceClient(cachePeerLookupHTTPClient, strings.TrimRight(strings.TrimSpace(target.peer.URL), "/"))
	connectReq := connect.NewRequest(req)
	connectReq.Header().Set("Authorization", "Bearer "+target.token)
	resp, err := client.LookupCachePeer(ctx, connectReq)
	if err != nil {
		if ctx.Err() != nil {
			return cachePeerCandidateMatch{}, false
		}
		s.recordCachePeerLookup(ctx, req.GetStage(), observability.CachePeerLookupDirectionOutbound, observability.CacheResultFailed)
		if s.Logger != nil {
			s.Logger.Debug("cache peer lookup failed", "peer", target.peer.URL, "error", err)
		}
		return cachePeerCandidateMatch{}, false
	}
	candidate := resp.Msg.GetCandidate()
	if candidate == nil {
		s.recordCachePeerLookup(ctx, req.GetStage(), observability.CachePeerLookupDirectionOutbound, observability.CacheResultMiss)
		return cachePeerCandidateMatch{}, false
	}
	if !cachePeerCandidateMatchesRequest(candidate, req, s.clock().Now()) {
		s.recordCachePeerLookup(ctx, req.GetStage(), observability.CachePeerLookupDirectionOutbound, observability.CacheResultMiss)
		if s.Logger != nil {
			s.Logger.Debug("cache peer candidate rejected", "peer", target.peer.URL)
		}
		return cachePeerCandidateMatch{}, false
	}
	s.recordCachePeerLookup(ctx, req.GetStage(), observability.CachePeerLookupDirectionOutbound, observability.CacheResultHit)
	return cachePeerCandidateMatch{peer: target.peer, token: target.token, candidate: candidate}, true
}

func (s *Service) openCachePeerExport(ctx context.Context, match cachePeerCandidateMatch) (*http.Response, error) {
	exportURL, err := cachePeerExportURL(match.peer.URL, match.candidate.GetTransferToken())
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, exportURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+match.token)
	resp, err := cachePeerExportHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
		_ = resp.Body.Close()
		return nil, fmt.Errorf("cache peer export returned status %d", resp.StatusCode)
	}
	return resp, nil
}

func cachePeerCandidateMatchesRequest(candidate *cleanroomv1.CachePeerCandidate, req *cleanroomv1.LookupCachePeerRequest, now time.Time) bool {
	if strings.TrimSpace(candidate.GetTransferToken()) == "" || strings.TrimSpace(candidate.GetZfsSnapshotGuid()) == "" {
		return false
	}
	if expiresAt := candidate.GetExpiresAt(); expiresAt != nil {
		expiresAtTime := expiresAt.AsTime()
		if !expiresAtTime.IsZero() && !expiresAtTime.After(now) {
			return false
		}
	}
	return candidate.GetStage() == req.GetStage() &&
		candidate.GetCacheKey() == req.GetCacheKey() &&
		candidate.GetBackend() == req.GetBackend() &&
		candidate.GetStorageDriver() == req.GetStorageDriver() &&
		candidate.GetArchitecture() == req.GetArchitecture() &&
		candidate.GetProducerVersion() == req.GetProducerVersion() &&
		candidate.GetPolicyHash() == req.GetPolicyHash() &&
		candidate.GetParentStage() == req.GetParentStage() &&
		candidate.GetParentCacheKey() == req.GetParentCacheKey() &&
		candidate.GetZfsParentSnapshotGuid() == req.GetParentZfsSnapshotGuid()
}

func cachePeerTokenForConfig(peer runtimeconfig.CachePeerConfig) string {
	tokenEnv := strings.TrimSpace(peer.TokenEnv)
	if tokenEnv == "" {
		return ""
	}
	return strings.TrimSpace(os.Getenv(tokenEnv))
}

func cachePeerExportURL(baseURL, token string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", err
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + cachePeerZFSIncrementalExportPathPrefix + url.PathEscape(strings.TrimSpace(token))
	parsed.RawQuery = ""
	return parsed.String(), nil
}

func cachePeerImportSingleflightKey(stage, cacheKey string) string {
	return strings.TrimSpace(stage) + "\x00" + strings.TrimSpace(cacheKey)
}

func snapshotResultFromVolumeSnapshot(snapshot volumestore.Snapshot) *backend.SnapshotResult {
	return &backend.SnapshotResult{
		StorageRef:         strings.TrimSpace(snapshot.StorageRef),
		StorageSizeBytes:   snapshot.StorageSizeBytes,
		ExclusiveSizeBytes: snapshot.ExclusiveSizeBytes,
		DriverMetadata:     strings.TrimSpace(snapshot.DriverMetadata),
	}
}

func validateImportedCachePeerSnapshot(snapshot volumestore.Snapshot, parentGUID, expectedGUID string) error {
	metadata, err := volumestore.DecodeZFSDriverMetadata(snapshot.DriverMetadata)
	if err != nil {
		return err
	}
	if strings.TrimSpace(metadata.SnapshotGUID) != strings.TrimSpace(expectedGUID) {
		return fmt.Errorf("imported cache peer snapshot guid mismatch: got %q want %q", metadata.SnapshotGUID, expectedGUID)
	}
	if strings.TrimSpace(metadata.ParentSnapshotGUID) != strings.TrimSpace(parentGUID) {
		return fmt.Errorf("imported cache peer parent guid mismatch: got %q want %q", metadata.ParentSnapshotGUID, parentGUID)
	}
	if strings.TrimSpace(snapshot.StorageRef) == "" {
		return errors.New("imported cache peer snapshot missing storage ref")
	}
	return nil
}

func (s *Service) cacheRecordByKey(ctx context.Context, stage, cacheKey string) (cachestore.Record, bool, error) {
	store, err := s.cacheStoreOrErr()
	if err != nil {
		return cachestore.Record{}, false, nil
	}
	return lookupReadyCacheRecord(ctx, store, stage, cacheKey)
}
