package controlservice

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/buildkite/cleanroom/internal/cachestore"
	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/observability"
	"github.com/buildkite/cleanroom/internal/volumestore"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var (
	ErrCachePeerUnauthorized        = errors.New("cache peer request unauthorized")
	ErrCachePeerExportTokenNotFound = errors.New("cache peer export token not found")
)

const (
	cachePeerMissUnsupportedStage      = "unsupported stage"
	cachePeerMissUnsupportedBackend    = "unsupported backend"
	cachePeerMissUnsupportedDriver     = "unsupported storage driver"
	cachePeerMissCacheStoreUnavailable = "cache metadata unavailable"
	cachePeerMissRecordNotFound        = "record not found"
	cachePeerMissParentRecordNotFound  = "parent record not found"
	cachePeerMissRecordMismatch        = "record metadata mismatch"
	cachePeerMissUnsupportedParent     = "unsupported parent stage"
	cachePeerMissParentGUIDMismatch    = "parent zfs snapshot guid mismatch"
	cachePeerMissDriverMetadataMissing = "zfs driver metadata missing"
	cachePeerMissTransferUnavailable   = "incremental transfer unavailable"
)

type cachePeerExport struct {
	Token                 string
	Stage                 string
	CacheKey              string
	ParentStage           string
	ParentCacheKey        string
	Backend               string
	StorageDriver         string
	Architecture          string
	ProducerVersion       string
	PolicyHash            string
	ParentZFSSnapshotGUID string
	ZFSSnapshotGUID       string
	ExpiresAt             time.Time
}

func (s *Service) AuthorizeCachePeerBearer(authorization string) error {
	scheme, token, ok := strings.Cut(strings.TrimSpace(authorization), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return ErrCachePeerUnauthorized
	}
	token = strings.TrimSpace(token)
	for _, configured := range s.cachePeerBearerTokens() {
		if subtle.ConstantTimeCompare([]byte(token), []byte(configured)) == 1 {
			return nil
		}
	}
	return ErrCachePeerUnauthorized
}

func (s *Service) cachePeerBearerTokens() []string {
	seen := map[string]struct{}{}
	var tokens []string
	for _, peer := range s.Config.Cache.Peers {
		tokenEnv := strings.TrimSpace(peer.TokenEnv)
		if tokenEnv == "" {
			continue
		}
		token := strings.TrimSpace(os.Getenv(tokenEnv))
		if token == "" {
			continue
		}
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		tokens = append(tokens, token)
	}
	return tokens
}

func (s *Service) LookupCachePeer(ctx context.Context, req *cleanroomv1.LookupCachePeerRequest) (*cleanroomv1.LookupCachePeerResponse, error) {
	if req == nil {
		return nil, errors.New("missing cache peer lookup request")
	}
	lookupStage := strings.TrimSpace(req.GetStage())
	lookupResult := observability.CacheResultFailed
	defer func() {
		s.recordCachePeerLookup(ctx, lookupStage, observability.CachePeerLookupDirectionInbound, lookupResult)
	}()
	if strings.TrimSpace(req.GetStage()) == "" {
		return nil, errors.New("missing cache peer lookup stage")
	}
	if strings.TrimSpace(req.GetCacheKey()) == "" {
		return nil, errors.New("missing cache peer lookup cache key")
	}

	match, missReason, err := s.planCachePeerExport(ctx, cachePeerLookupFromProto(req))
	if err != nil {
		return nil, err
	}
	if missReason != "" {
		lookupResult = observability.CacheResultMiss
		return cachePeerMiss(missReason), nil
	}

	token, err := newCachePeerExportToken()
	if err != nil {
		return nil, err
	}
	expiresAt := s.clock().Now().Add(s.cachePeerExportTokenTTL()).UTC()
	export := cachePeerExport{
		Token:                 token,
		Stage:                 match.Child.Stage,
		CacheKey:              match.Child.CacheKey,
		ParentStage:           match.Parent.Stage,
		ParentCacheKey:        match.Child.ParentCacheKey,
		Backend:               match.Child.Backend,
		StorageDriver:         match.Child.StorageDriver,
		Architecture:          match.Child.Architecture,
		ProducerVersion:       match.Child.ProducerVersion,
		PolicyHash:            match.Child.PolicyHash,
		ParentZFSSnapshotGUID: match.ParentMetadata.SnapshotGUID,
		ZFSSnapshotGUID:       match.ChildMetadata.SnapshotGUID,
		ExpiresAt:             expiresAt,
	}
	s.storeCachePeerExport(export)
	lookupResult = observability.CacheResultHit

	return &cleanroomv1.LookupCachePeerResponse{
		Candidate: &cleanroomv1.CachePeerCandidate{
			TransferToken:         token,
			Stage:                 match.Child.Stage,
			CacheKey:              match.Child.CacheKey,
			BackingSnapshotId:     match.Child.BackingSnapshotID,
			StorageRef:            match.Child.StorageRef,
			ParentStage:           match.Parent.Stage,
			ParentCacheKey:        match.Child.ParentCacheKey,
			ZfsSnapshotGuid:       match.ChildMetadata.SnapshotGUID,
			ZfsParentSnapshotGuid: match.ChildMetadata.ParentSnapshotGUID,
			ProducerVersion:       match.Child.ProducerVersion,
			Backend:               match.Child.Backend,
			StorageDriver:         match.Child.StorageDriver,
			Architecture:          match.Child.Architecture,
			PolicyHash:            match.Child.PolicyHash,
			EstimatedBytes:        match.Plan.EstimatedBytes,
			ExpiresAt:             timestamppb.New(expiresAt),
		},
	}, nil
}

func (s *Service) ExportCachePeerZFSIncremental(ctx context.Context, token string, dst io.Writer) error {
	if dst == nil {
		return errors.New("missing cache peer export writer")
	}
	ctx, span := s.Observability.Tracer("github.com/buildkite/cleanroom/internal/controlservice").Start(ctx, "cleanroom.cache_peer.export")
	defer span.End()

	release, err := s.acquireCachePeerExportSlot(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	defer release()

	export, ok := s.consumeCachePeerExport(strings.TrimSpace(token), s.clock().Now())
	if !ok {
		span.SetAttributes(attribute.String(observability.AttrCacheResult, observability.CacheResultMiss))
		span.SetStatus(codes.Error, ErrCachePeerExportTokenNotFound.Error())
		return ErrCachePeerExportTokenNotFound
	}
	span.SetAttributes(
		attribute.String(observability.AttrCacheStage, strings.TrimSpace(export.Stage)),
		attribute.String(observability.AttrCachePeerDirection, observability.CachePeerDirectionExport),
	)
	match, missReason, err := s.planCachePeerExport(ctx, cachePeerLookup{
		Stage:                 export.Stage,
		CacheKey:              export.CacheKey,
		Backend:               export.Backend,
		StorageDriver:         export.StorageDriver,
		Architecture:          export.Architecture,
		ProducerVersion:       export.ProducerVersion,
		PolicyHash:            export.PolicyHash,
		ParentStage:           export.ParentStage,
		ParentCacheKey:        export.ParentCacheKey,
		ParentZFSSnapshotGUID: export.ParentZFSSnapshotGUID,
	})
	if err != nil {
		span.RecordError(err)
		span.SetAttributes(attribute.String(observability.AttrCacheResult, observability.CacheResultFailed))
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	if missReason != "" {
		err := fmt.Errorf("cache peer export candidate no longer valid: %s", missReason)
		span.SetAttributes(
			attribute.String(observability.AttrCacheResult, observability.CacheResultFailed),
			attribute.String(observability.AttrCachePeerFallback, missReason),
		)
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	if match.ChildMetadata.SnapshotGUID != export.ZFSSnapshotGUID {
		err := fmt.Errorf("cache peer export candidate no longer valid: zfs snapshot guid mismatch")
		span.SetAttributes(attribute.String(observability.AttrCacheResult, observability.CacheResultFailed))
		span.SetStatus(codes.Error, err.Error())
		return err
	}
	writer := &cachePeerCountingWriter{dst: dst}
	started := s.clock().Now()
	err = s.CachePeerTransferDriver.ExportIncrementalSnapshot(ctx, match.Plan, writer)
	duration := s.clock().Now().Sub(started)
	result := observability.CacheResultExported
	if err != nil {
		result = observability.CacheResultFailed
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	} else {
		span.SetStatus(codes.Ok, "")
		s.logCachePeerExportCompleted(ctx, export.Stage, writer.bytes, duration)
	}
	s.recordCachePeerTransfer(ctx, export.Stage, observability.CachePeerDirectionExport, result, writer.bytes, duration)
	span.SetAttributes(attribute.String(observability.AttrCacheResult, result))
	return err
}

func (s *Service) acquireCachePeerExportSlot(ctx context.Context) (func(), error) {
	s.cachePeerExportSlotsOnce.Do(func() {
		s.cachePeerExportSlots = make(chan struct{}, s.cachePeerExportConcurrency())
	})
	select {
	case s.cachePeerExportSlots <- struct{}{}:
		return func() { <-s.cachePeerExportSlots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type cachePeerLookup struct {
	Stage                 string
	CacheKey              string
	Backend               string
	StorageDriver         string
	Architecture          string
	ProducerVersion       string
	PolicyHash            string
	ParentStage           string
	ParentCacheKey        string
	ParentZFSSnapshotGUID string
}

type cachePeerExportMatch struct {
	Child          cachestore.Record
	Parent         cachestore.Record
	ChildMetadata  volumestore.ZFSDriverMetadata
	ParentMetadata volumestore.ZFSDriverMetadata
	Plan           volumestore.IncrementalSnapshotExportPlan
}

func cachePeerLookupFromProto(req *cleanroomv1.LookupCachePeerRequest) cachePeerLookup {
	return cachePeerLookup{
		Stage:                 strings.TrimSpace(req.GetStage()),
		CacheKey:              strings.TrimSpace(req.GetCacheKey()),
		Backend:               strings.TrimSpace(req.GetBackend()),
		StorageDriver:         strings.TrimSpace(req.GetStorageDriver()),
		Architecture:          strings.TrimSpace(req.GetArchitecture()),
		ProducerVersion:       strings.TrimSpace(req.GetProducerVersion()),
		PolicyHash:            strings.TrimSpace(req.GetPolicyHash()),
		ParentStage:           strings.TrimSpace(req.GetParentStage()),
		ParentCacheKey:        strings.TrimSpace(req.GetParentCacheKey()),
		ParentZFSSnapshotGUID: strings.TrimSpace(req.GetParentZfsSnapshotGuid()),
	}
}

func (s *Service) planCachePeerExport(ctx context.Context, lookup cachePeerLookup) (cachePeerExportMatch, string, error) {
	if lookup.Stage != dependencyStageName && lookup.Stage != servicesStageName {
		return cachePeerExportMatch{}, cachePeerMissUnsupportedStage, nil
	}
	if !cachePeerParentStageSupported(lookup.Stage, lookup.ParentStage) {
		return cachePeerExportMatch{}, cachePeerMissUnsupportedParent, nil
	}
	if lookup.Backend != "firecracker" {
		return cachePeerExportMatch{}, cachePeerMissUnsupportedBackend, nil
	}
	if lookup.StorageDriver != "zfs" {
		return cachePeerExportMatch{}, cachePeerMissUnsupportedDriver, nil
	}
	if lookup.Architecture == "" || lookup.ProducerVersion == "" || lookup.PolicyHash == "" || lookup.ParentCacheKey == "" || lookup.ParentZFSSnapshotGUID == "" {
		return cachePeerExportMatch{}, cachePeerMissRecordMismatch, nil
	}
	if s.CachePeerTransferDriver == nil {
		return cachePeerExportMatch{}, cachePeerMissTransferUnavailable, nil
	}

	store, err := s.cacheStoreOrErr()
	if err != nil {
		return cachePeerExportMatch{}, cachePeerMissCacheStoreUnavailable, nil
	}
	child, ok, err := store.GetReady(ctx, lookup.Stage, lookup.CacheKey)
	if err != nil {
		return cachePeerExportMatch{}, "", err
	}
	if !ok {
		return cachePeerExportMatch{}, cachePeerMissRecordNotFound, nil
	}
	if missReason := cachePeerChildRecordMissReason(child, lookup); missReason != "" {
		return cachePeerExportMatch{}, missReason, nil
	}

	parent, ok, err := store.GetReady(ctx, lookup.ParentStage, lookup.ParentCacheKey)
	if err != nil {
		return cachePeerExportMatch{}, "", err
	}
	if !ok {
		return cachePeerExportMatch{}, cachePeerMissParentRecordNotFound, nil
	}
	if strings.TrimSpace(parent.Backend) != lookup.Backend || strings.TrimSpace(parent.StorageDriver) != lookup.StorageDriver {
		return cachePeerExportMatch{}, cachePeerMissRecordMismatch, nil
	}

	childMetadata, ok := cachePeerRecordZFSMetadata(child)
	if !ok {
		return cachePeerExportMatch{}, cachePeerMissDriverMetadataMissing, nil
	}
	parentMetadata, ok := cachePeerRecordZFSMetadata(parent)
	if !ok {
		return cachePeerExportMatch{}, cachePeerMissDriverMetadataMissing, nil
	}
	if parentMetadata.SnapshotGUID != lookup.ParentZFSSnapshotGUID || childMetadata.ParentSnapshotGUID != lookup.ParentZFSSnapshotGUID {
		return cachePeerExportMatch{}, cachePeerMissParentGUIDMismatch, nil
	}

	plan, err := s.CachePeerTransferDriver.PlanIncrementalSnapshotExport(ctx, volumestore.IncrementalSnapshotExportRequest{
		FromSnapshotRef:  strings.TrimSpace(parent.StorageRef),
		FromSnapshotGUID: parentMetadata.SnapshotGUID,
		ToSnapshotRef:    strings.TrimSpace(child.StorageRef),
		ToSnapshotGUID:   childMetadata.SnapshotGUID,
	})
	if err != nil {
		return cachePeerExportMatch{}, cachePeerMissTransferUnavailable, nil
	}
	return cachePeerExportMatch{
		Child:          child,
		Parent:         parent,
		ChildMetadata:  childMetadata,
		ParentMetadata: parentMetadata,
		Plan:           plan,
	}, "", nil
}

func cachePeerChildRecordMissReason(record cachestore.Record, lookup cachePeerLookup) string {
	if strings.TrimSpace(record.Backend) != lookup.Backend {
		return cachePeerMissUnsupportedBackend
	}
	if strings.TrimSpace(record.StorageDriver) != lookup.StorageDriver {
		return cachePeerMissUnsupportedDriver
	}
	if strings.TrimSpace(record.Architecture) != lookup.Architecture {
		return cachePeerMissRecordMismatch
	}
	if strings.TrimSpace(record.ProducerVersion) != lookup.ProducerVersion {
		return cachePeerMissRecordMismatch
	}
	if strings.TrimSpace(record.PolicyHash) != lookup.PolicyHash {
		return cachePeerMissRecordMismatch
	}
	if strings.TrimSpace(record.ParentCacheKey) != lookup.ParentCacheKey {
		return cachePeerMissRecordMismatch
	}
	if lookup.Architecture != runtime.GOARCH {
		return cachePeerMissRecordMismatch
	}
	return ""
}

func cachePeerParentStageSupported(stage, parentStage string) bool {
	switch stage {
	case dependencyStageName:
		return parentStage == workspaceStageName
	case servicesStageName:
		return parentStage == workspaceStageName || parentStage == dependencyStageName
	default:
		return false
	}
}

func cachePeerRecordZFSMetadata(record cachestore.Record) (volumestore.ZFSDriverMetadata, bool) {
	metadata, err := volumestore.DecodeZFSDriverMetadata(record.DriverMetadata)
	if err != nil {
		return volumestore.ZFSDriverMetadata{}, false
	}
	if strings.TrimSpace(metadata.SnapshotGUID) == "" {
		return volumestore.ZFSDriverMetadata{}, false
	}
	return metadata, true
}

func cachePeerMiss(reason string) *cleanroomv1.LookupCachePeerResponse {
	return &cleanroomv1.LookupCachePeerResponse{MissReason: reason}
}

func newCachePeerExportToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate cache peer export token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func (s *Service) storeCachePeerExport(export cachePeerExport) {
	s.cachePeerExportsMu.Lock()
	defer s.cachePeerExportsMu.Unlock()
	if s.cachePeerExports == nil {
		s.cachePeerExports = map[string]cachePeerExport{}
	}
	now := s.clock().Now()
	for token, candidate := range s.cachePeerExports {
		if !candidate.ExpiresAt.After(now) {
			delete(s.cachePeerExports, token)
		}
	}
	s.cachePeerExports[export.Token] = export
}

func (s *Service) consumeCachePeerExport(token string, now time.Time) (cachePeerExport, bool) {
	if token == "" {
		return cachePeerExport{}, false
	}
	s.cachePeerExportsMu.Lock()
	defer s.cachePeerExportsMu.Unlock()
	if s.cachePeerExports == nil {
		return cachePeerExport{}, false
	}
	export, ok := s.cachePeerExports[token]
	if !ok {
		return cachePeerExport{}, false
	}
	delete(s.cachePeerExports, token)
	if !export.ExpiresAt.After(now) {
		return cachePeerExport{}, false
	}
	return export, true
}
