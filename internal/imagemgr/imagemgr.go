package imagemgr

import (
	"bufio"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/buildkite/cleanroom/internal/hosttools"
	"github.com/buildkite/cleanroom/internal/ociref"
	"github.com/buildkite/cleanroom/internal/paths"
	_ "modernc.org/sqlite"
)

const defaultMkfsBinary = "mkfs.ext4"

const (
	defaultResolveAttempts       = 2
	defaultRootFSStreamIdleLimit = 2 * time.Minute
	defaultResolveAttemptTimeout = 10 * time.Minute
)

var errRootFSStreamIdleTimeout = errors.New("image rootfs stream stalled")

type OCIConfig struct {
	Entrypoint   []string
	Cmd          []string
	Env          []string
	Workdir      string
	User         string
	OS           string
	Architecture string
	Variant      string
}

type Record struct {
	Digest     string
	Ref        string
	RootFSPath string
	SizeBytes  int64
	CreatedAt  time.Time
	LastUsedAt time.Time
	Source     string
	OCIConfig  OCIConfig
}

type EnsureResult struct {
	Record   Record
	CacheHit bool
}

type Options struct {
	CacheDir       string
	MetadataDBPath string
	MkfsBinary     string
	Now            func() time.Time

	PullImage               func(context.Context, string) (io.ReadCloser, OCIConfig, error)
	ResolveOCIConfig        func(context.Context, string) (OCIConfig, error)
	MaterializeRootFS       func(context.Context, io.Reader, string) (int64, error)
	ResolveAttempts         int
	ResolveAttemptTimeout   time.Duration
	RootFSStreamIdleTimeout time.Duration
}

type pullImageFunc func(resolveCtx, streamCtx context.Context, ref string) (io.ReadCloser, OCIConfig, error)

type Manager struct {
	cacheDir                string
	metadataDBPath          string
	mkfsBinary              string
	now                     func() time.Time
	pullImage               pullImageFunc
	resolveOCIConfig        func(context.Context, string) (OCIConfig, error)
	materialize             func(context.Context, io.Reader, string) (int64, error)
	resolveAttempts         int
	resolveAttemptTimeout   time.Duration
	rootFSStreamIdleTimeout time.Duration

	mu sync.Mutex
}

func New(opts Options) (*Manager, error) {
	cacheDir := strings.TrimSpace(opts.CacheDir)
	if cacheDir == "" {
		var err error
		cacheDir, err = paths.ImageCacheDir()
		if err != nil {
			return nil, fmt.Errorf("resolve image cache directory: %w", err)
		}
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("create image cache directory %q: %w", cacheDir, err)
	}

	metadataDBPath := strings.TrimSpace(opts.MetadataDBPath)
	if metadataDBPath == "" {
		var err error
		metadataDBPath, err = paths.ImageMetadataDBPath()
		if err != nil {
			return nil, fmt.Errorf("resolve image metadata database path: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(metadataDBPath), 0o755); err != nil {
		return nil, fmt.Errorf("create image metadata directory for %q: %w", metadataDBPath, err)
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}

	mkfsBinary := strings.TrimSpace(opts.MkfsBinary)
	if mkfsBinary == "" {
		if resolvedMkfs, err := hosttools.ResolveE2FSProgsBinary(defaultMkfsBinary); err == nil {
			mkfsBinary = resolvedMkfs
		} else {
			mkfsBinary = defaultMkfsBinary
		}
	}

	manager := &Manager{
		cacheDir:                cacheDir,
		metadataDBPath:          metadataDBPath,
		mkfsBinary:              mkfsBinary,
		now:                     now,
		resolveAttempts:         defaultResolveAttempts,
		resolveAttemptTimeout:   defaultResolveAttemptTimeout,
		rootFSStreamIdleTimeout: defaultRootFSStreamIdleLimit,
	}
	if opts.ResolveAttempts > 0 {
		manager.resolveAttempts = opts.ResolveAttempts
	}
	if opts.ResolveAttemptTimeout > 0 {
		manager.resolveAttemptTimeout = opts.ResolveAttemptTimeout
	}
	if opts.RootFSStreamIdleTimeout > 0 {
		manager.rootFSStreamIdleTimeout = opts.RootFSStreamIdleTimeout
	}
	if opts.PullImage != nil {
		manager.pullImage = func(_, streamCtx context.Context, ref string) (io.ReadCloser, OCIConfig, error) {
			return opts.PullImage(streamCtx, ref)
		}
	} else {
		manager.pullImage = pullImageFromRegistry
	}
	if opts.ResolveOCIConfig != nil {
		manager.resolveOCIConfig = opts.ResolveOCIConfig
	} else if opts.PullImage != nil {
		manager.resolveOCIConfig = func(ctx context.Context, ref string) (OCIConfig, error) {
			stream, config, err := opts.PullImage(ctx, ref)
			if stream != nil {
				_ = stream.Close()
			}
			if err != nil {
				return OCIConfig{}, err
			}
			return config, nil
		}
	} else {
		manager.resolveOCIConfig = resolveOCIConfigFromRegistry
	}
	if opts.MaterializeRootFS != nil {
		manager.materialize = opts.MaterializeRootFS
	} else {
		manager.materialize = func(ctx context.Context, tarStream io.Reader, outputPath string) (int64, error) {
			return materializeExt4(ctx, manager.mkfsBinary, tarStream, outputPath)
		}
	}

	if err := manager.initDB(context.Background()); err != nil {
		return nil, err
	}
	return manager, nil
}

func (m *Manager) Ensure(ctx context.Context, ref string) (EnsureResult, error) {
	parsedRef, err := ociref.ParseDigestReference(ref)
	if err != nil {
		return EnsureResult{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now().UTC()
	record, found, err := m.lookupByDigest(ctx, parsedRef.Digest())
	if err != nil {
		return EnsureResult{}, err
	}
	if found {
		_, statErr := os.Stat(record.RootFSPath)
		if statErr == nil {
			if err := m.validateCachedRecordPlatform(ctx, parsedRef, &record); err != nil {
				return EnsureResult{}, err
			}
			record.Ref = parsedRef.Original
			record.LastUsedAt = now
			if err := m.upsertRecord(ctx, record); err != nil {
				return EnsureResult{}, err
			}
			return EnsureResult{Record: record, CacheHit: true}, nil
		}
		if !os.IsNotExist(statErr) {
			return EnsureResult{}, fmt.Errorf("stat cached rootfs %q: %w", record.RootFSPath, statErr)
		}
		if err := m.deleteByDigest(ctx, record.Digest); err != nil {
			return EnsureResult{}, err
		}
	}

	if record, found, err := m.recoverDigestCacheFile(ctx, parsedRef, now); err != nil {
		return EnsureResult{}, err
	} else if found {
		return EnsureResult{Record: record, CacheHit: true}, nil
	}

	var lastErr error
	for attempt := 1; attempt <= m.resolveAttempts; attempt++ {
		attemptCtx, cancelAttempt := m.resolveAttemptContext(ctx)
		tarStream, config, err := m.pullImage(attemptCtx, ctx, parsedRef.Original)
		if err != nil {
			cancelAttempt()
			lastErr = err
			if !m.shouldRetryResolve(ctx, err) {
				return EnsureResult{}, err
			}
			if attempt >= m.resolveAttempts {
				return EnsureResult{}, fmt.Errorf("resolve image %q after %d attempts: %w", parsedRef.Original, m.resolveAttempts, err)
			}
			continue
		}
		if tarStream == nil {
			cancelAttempt()
			return EnsureResult{}, fmt.Errorf("pull image %q returned nil rootfs stream", parsedRef.Original)
		}
		rootFSStream := m.withRootFSStreamIdleTimeout(tarStream)
		if err := ValidateImagePlatformForHost(config.OS, config.Architecture, runtime.GOARCH); err != nil {
			_ = rootFSStream.Close()
			cancelAttempt()
			return EnsureResult{}, fmt.Errorf("image %q platform is incompatible: %w", parsedRef.Original, err)
		}

		record, err = m.persistFromTarStream(ctx, persistFromTarRequest{
			Ref:        parsedRef.Original,
			Digest:     parsedRef.Digest(),
			TarStream:  rootFSStream,
			OCIConfig:  config,
			Source:     "registry",
			CreatedAt:  now,
			LastUsedAt: now,
		})
		_ = rootFSStream.Close()
		cancelAttempt()
		if err == nil {
			return EnsureResult{Record: record, CacheHit: false}, nil
		}
		lastErr = err
		if !m.shouldRetryResolve(ctx, err) {
			return EnsureResult{}, err
		}
		if attempt >= m.resolveAttempts {
			return EnsureResult{}, fmt.Errorf("resolve image %q after %d attempts: %w", parsedRef.Original, m.resolveAttempts, err)
		}
	}
	if lastErr != nil {
		return EnsureResult{}, fmt.Errorf("resolve image %q after %d attempts: %w", parsedRef.Original, m.resolveAttempts, lastErr)
	}

	return EnsureResult{Record: record, CacheHit: false}, nil
}

func (m *Manager) resolveAttemptContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if m.resolveAttemptTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, m.resolveAttemptTimeout)
}

func (m *Manager) recoverDigestCacheFile(ctx context.Context, parsedRef ociref.DigestReference, now time.Time) (Record, bool, error) {
	rootFSPath := m.cachedRootFSPath(parsedRef.Digest())
	info, err := os.Stat(rootFSPath)
	if err != nil {
		if os.IsNotExist(err) {
			return Record{}, false, nil
		}
		return Record{}, false, fmt.Errorf("stat cached rootfs %q: %w", rootFSPath, err)
	}
	if info.IsDir() {
		return Record{}, false, fmt.Errorf("cached rootfs %q is a directory", rootFSPath)
	}

	config, err := m.resolveOCIConfigWithRetries(ctx, parsedRef.Original)
	if err != nil {
		return Record{}, false, fmt.Errorf("resolve image metadata for cached rootfs %s: %w", parsedRef.Digest(), err)
	}
	if err := validateResolvedOCIConfigPlatform(parsedRef.Original, config); err != nil {
		return Record{}, false, err
	}

	record := Record{
		Digest:     parsedRef.Digest(),
		Ref:        parsedRef.Original,
		RootFSPath: rootFSPath,
		SizeBytes:  info.Size(),
		CreatedAt:  now,
		LastUsedAt: now,
		Source:     "cache-file",
		OCIConfig:  config,
	}
	if err := m.upsertRecord(ctx, record); err != nil {
		return Record{}, false, fmt.Errorf("recover cached image metadata for %s: %w", parsedRef.Digest(), err)
	}
	return record, true, nil
}

func (m *Manager) withRootFSStreamIdleTimeout(stream io.ReadCloser) io.ReadCloser {
	if stream == nil || m.rootFSStreamIdleTimeout <= 0 {
		return stream
	}
	return &rootFSStreamIdleTimeoutReadCloser{
		stream:  stream,
		timeout: m.rootFSStreamIdleTimeout,
	}
}

func (m *Manager) shouldRetryResolve(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	return errors.Is(err, errRootFSStreamIdleTimeout) || errors.Is(err, context.DeadlineExceeded)
}

func (m *Manager) validateCachedRecordPlatform(ctx context.Context, parsedRef ociref.DigestReference, record *Record) error {
	if record == nil {
		return nil
	}
	if needsPlatformMetadata(record.OCIConfig) && strings.EqualFold(strings.TrimSpace(record.Source), "registry") {
		resolvedConfig, err := m.resolveOCIConfigWithRetries(ctx, parsedRef.Original)
		if err == nil {
			record.OCIConfig = mergeOCIConfig(record.OCIConfig, resolvedConfig)
		}
	}
	if err := ValidateImagePlatformForHost(record.OCIConfig.OS, record.OCIConfig.Architecture, runtime.GOARCH); err != nil {
		return fmt.Errorf("image %q platform is incompatible: %w", parsedRef.Original, err)
	}
	return nil
}

func (m *Manager) resolveOCIConfigWithRetries(ctx context.Context, ref string) (OCIConfig, error) {
	if m == nil || m.resolveOCIConfig == nil {
		return OCIConfig{}, fmt.Errorf("image metadata resolver is not configured")
	}

	var lastErr error
	for attempt := 1; attempt <= m.resolveAttempts; attempt++ {
		attemptCtx, cancelAttempt := m.resolveAttemptContext(ctx)
		config, err := m.resolveOCIConfig(attemptCtx, ref)
		cancelAttempt()
		if err == nil {
			return config, nil
		}
		lastErr = err
		if !m.shouldRetryResolve(ctx, err) {
			return OCIConfig{}, err
		}
		if attempt >= m.resolveAttempts {
			return OCIConfig{}, fmt.Errorf("resolve image metadata %q after %d attempts: %w", ref, m.resolveAttempts, err)
		}
	}
	if lastErr != nil {
		return OCIConfig{}, fmt.Errorf("resolve image metadata %q after %d attempts: %w", ref, m.resolveAttempts, lastErr)
	}
	return OCIConfig{}, nil
}

func needsPlatformMetadata(cfg OCIConfig) bool {
	return strings.TrimSpace(cfg.OS) == "" || strings.TrimSpace(cfg.Architecture) == ""
}

func validateResolvedOCIConfigPlatform(ref string, cfg OCIConfig) error {
	if strings.TrimSpace(cfg.OS) == "" || strings.TrimSpace(cfg.Architecture) == "" {
		return fmt.Errorf("image %q metadata does not include a platform", ref)
	}
	if err := ValidateImagePlatformForHost(cfg.OS, cfg.Architecture, runtime.GOARCH); err != nil {
		return fmt.Errorf("image %q platform is incompatible: %w", ref, err)
	}
	return nil
}

func mergeOCIConfig(base, overlay OCIConfig) OCIConfig {
	if strings.TrimSpace(overlay.OS) != "" {
		base.OS = overlay.OS
	}
	if strings.TrimSpace(overlay.Architecture) != "" {
		base.Architecture = overlay.Architecture
	}
	if strings.TrimSpace(overlay.Variant) != "" {
		base.Variant = overlay.Variant
	}
	return base
}

func (m *Manager) Pull(ctx context.Context, ref string) (EnsureResult, error) {
	return m.Ensure(ctx, ref)
}

func (m *Manager) Import(ctx context.Context, ref, tarPath string, stdin io.Reader) (Record, error) {
	parsedRef, err := ociref.ParseDigestReference(ref)
	if err != nil {
		return Record{}, err
	}

	stream, closer, err := openImportStream(tarPath, stdin)
	if err != nil {
		return Record{}, err
	}
	defer closer()

	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now().UTC()
	return m.persistFromTarStream(ctx, persistFromTarRequest{
		Ref:        parsedRef.Original,
		Digest:     parsedRef.Digest(),
		TarStream:  stream,
		Source:     "import",
		CreatedAt:  now,
		LastUsedAt: now,
	})
}

func (m *Manager) List(ctx context.Context) ([]Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.initDB(ctx); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", m.metadataDBPath)
	if err != nil {
		return nil, fmt.Errorf("open image metadata database %q: %w", m.metadataDBPath, err)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, `
		SELECT
			digest,
			ref,
			rootfs_path,
			size_bytes,
			created_at_unix,
			last_used_at_unix,
			source,
			oci_entrypoint_json,
			oci_cmd_json,
			oci_env_json,
			oci_workdir,
			oci_user,
			oci_os,
			oci_architecture,
			oci_variant
		FROM images
		ORDER BY last_used_at_unix DESC, created_at_unix DESC, digest ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query cached images: %w", err)
	}
	defer rows.Close()

	items := make([]Record, 0)
	for rows.Next() {
		record, scanErr := scanRecord(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cached images: %w", err)
	}
	return items, nil
}

func (m *Manager) Remove(ctx context.Context, selector string) ([]Record, error) {
	sel := strings.TrimSpace(selector)
	if sel == "" {
		return nil, fmt.Errorf("image selector cannot be empty")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.initDB(ctx); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", m.metadataDBPath)
	if err != nil {
		return nil, fmt.Errorf("open image metadata database %q: %w", m.metadataDBPath, err)
	}
	defer db.Close()

	records, err := queryRecordsBySelector(ctx, db, sel)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, nil
	}

	for _, record := range records {
		if err := os.Remove(record.RootFSPath); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("remove cached rootfs %q: %w", record.RootFSPath, err)
		}
	}

	for _, record := range records {
		if _, err := db.ExecContext(ctx, `DELETE FROM images WHERE digest = ?`, record.Digest); err != nil {
			return nil, fmt.Errorf("delete cached image metadata for %s: %w", record.Digest, err)
		}
	}

	return records, nil
}

type persistFromTarRequest struct {
	Ref        string
	Digest     string
	TarStream  io.Reader
	OCIConfig  OCIConfig
	Source     string
	CreatedAt  time.Time
	LastUsedAt time.Time
}

func (m *Manager) persistFromTarStream(ctx context.Context, req persistFromTarRequest) (Record, error) {
	if req.CreatedAt.IsZero() {
		req.CreatedAt = m.now().UTC()
	}
	if req.LastUsedAt.IsZero() {
		req.LastUsedAt = req.CreatedAt
	}

	existing, found, err := m.lookupByDigest(ctx, req.Digest)
	if err != nil {
		return Record{}, err
	}
	if found {
		req.CreatedAt = existing.CreatedAt
	}

	outputPath := m.cachedRootFSPath(req.Digest)
	tmpFile, err := os.CreateTemp(m.cacheDir, strings.TrimPrefix(req.Digest, "sha256:")+".tmp-*.ext4")
	if err != nil {
		return Record{}, fmt.Errorf("create temporary image artifact for %q: %w", req.Digest, err)
	}
	tmpPath := tmpFile.Name()
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return Record{}, fmt.Errorf("close temporary image artifact file %q: %w", tmpPath, err)
	}
	defer os.Remove(tmpPath)

	sizeBytes, err := m.materialize(ctx, req.TarStream, tmpPath)
	if err != nil {
		return Record{}, err
	}
	if err := os.Rename(tmpPath, outputPath); err != nil {
		return Record{}, fmt.Errorf("move image artifact to cache %q: %w", outputPath, err)
	}

	record := Record{
		Digest:     req.Digest,
		Ref:        req.Ref,
		RootFSPath: outputPath,
		SizeBytes:  sizeBytes,
		CreatedAt:  req.CreatedAt,
		LastUsedAt: req.LastUsedAt,
		Source:     req.Source,
		OCIConfig:  req.OCIConfig,
	}
	if err := m.upsertRecord(ctx, record); err != nil {
		_ = os.Remove(outputPath)
		return Record{}, err
	}

	return record, nil
}

func (m *Manager) cachedRootFSPath(digest string) string {
	return filepath.Join(m.cacheDir, strings.TrimPrefix(digest, "sha256:")+".ext4")
}

func (m *Manager) initDB(ctx context.Context) error {
	db, err := sql.Open("sqlite", m.metadataDBPath)
	if err != nil {
		return fmt.Errorf("open image metadata database %q: %w", m.metadataDBPath, err)
	}
	defer db.Close()

	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS images (
			digest TEXT PRIMARY KEY,
			ref TEXT NOT NULL,
			rootfs_path TEXT NOT NULL,
			size_bytes INTEGER NOT NULL,
			created_at_unix INTEGER NOT NULL,
			last_used_at_unix INTEGER NOT NULL,
			source TEXT NOT NULL,
			oci_entrypoint_json TEXT NOT NULL,
			oci_cmd_json TEXT NOT NULL,
			oci_env_json TEXT NOT NULL,
			oci_workdir TEXT NOT NULL,
			oci_user TEXT NOT NULL,
			oci_os TEXT NOT NULL DEFAULT '',
			oci_architecture TEXT NOT NULL DEFAULT '',
			oci_variant TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_images_ref ON images(ref);
	`)
	if err != nil {
		return fmt.Errorf("initialise image metadata schema: %w", err)
	}
	for _, stmt := range []string{
		`ALTER TABLE images ADD COLUMN oci_os TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE images ADD COLUMN oci_architecture TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE images ADD COLUMN oci_variant TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			if !strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
				return fmt.Errorf("migrate image metadata schema: %w", err)
			}
		}
	}
	return nil
}

func (m *Manager) lookupByDigest(ctx context.Context, digest string) (Record, bool, error) {
	if err := m.initDB(ctx); err != nil {
		return Record{}, false, err
	}

	db, err := sql.Open("sqlite", m.metadataDBPath)
	if err != nil {
		return Record{}, false, fmt.Errorf("open image metadata database %q: %w", m.metadataDBPath, err)
	}
	defer db.Close()

	row := db.QueryRowContext(ctx, `
		SELECT
			digest,
			ref,
			rootfs_path,
			size_bytes,
			created_at_unix,
			last_used_at_unix,
			source,
			oci_entrypoint_json,
			oci_cmd_json,
			oci_env_json,
			oci_workdir,
			oci_user,
			oci_os,
			oci_architecture,
			oci_variant
		FROM images
		WHERE digest = ?
	`, digest)

	record, err := scanRecord(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return Record{}, false, nil
		}
		return Record{}, false, err
	}
	return record, true, nil
}

func (m *Manager) deleteByDigest(ctx context.Context, digest string) error {
	if err := m.initDB(ctx); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", m.metadataDBPath)
	if err != nil {
		return fmt.Errorf("open image metadata database %q: %w", m.metadataDBPath, err)
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, `DELETE FROM images WHERE digest = ?`, digest); err != nil {
		return fmt.Errorf("delete image metadata for digest %s: %w", digest, err)
	}
	return nil
}

func (m *Manager) upsertRecord(ctx context.Context, record Record) error {
	if err := m.initDB(ctx); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", m.metadataDBPath)
	if err != nil {
		return fmt.Errorf("open image metadata database %q: %w", m.metadataDBPath, err)
	}
	defer db.Close()

	entrypointJSON, err := marshalStringSlice(record.OCIConfig.Entrypoint)
	if err != nil {
		return err
	}
	cmdJSON, err := marshalStringSlice(record.OCIConfig.Cmd)
	if err != nil {
		return err
	}
	envJSON, err := marshalStringSlice(record.OCIConfig.Env)
	if err != nil {
		return err
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO images (
			digest,
			ref,
			rootfs_path,
			size_bytes,
			created_at_unix,
			last_used_at_unix,
			source,
			oci_entrypoint_json,
			oci_cmd_json,
			oci_env_json,
			oci_workdir,
			oci_user,
			oci_os,
			oci_architecture,
			oci_variant
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(digest) DO UPDATE SET
			ref = excluded.ref,
			rootfs_path = excluded.rootfs_path,
			size_bytes = excluded.size_bytes,
			created_at_unix = excluded.created_at_unix,
			last_used_at_unix = excluded.last_used_at_unix,
			source = excluded.source,
			oci_entrypoint_json = excluded.oci_entrypoint_json,
			oci_cmd_json = excluded.oci_cmd_json,
			oci_env_json = excluded.oci_env_json,
			oci_workdir = excluded.oci_workdir,
			oci_user = excluded.oci_user,
			oci_os = excluded.oci_os,
			oci_architecture = excluded.oci_architecture,
			oci_variant = excluded.oci_variant
	`,
		record.Digest,
		record.Ref,
		record.RootFSPath,
		record.SizeBytes,
		record.CreatedAt.Unix(),
		record.LastUsedAt.Unix(),
		record.Source,
		entrypointJSON,
		cmdJSON,
		envJSON,
		record.OCIConfig.Workdir,
		record.OCIConfig.User,
		record.OCIConfig.OS,
		record.OCIConfig.Architecture,
		record.OCIConfig.Variant,
	)
	if err != nil {
		return fmt.Errorf("upsert image metadata for %s: %w", record.Digest, err)
	}
	return nil
}

func queryRecordsBySelector(ctx context.Context, db *sql.DB, selector string) ([]Record, error) {
	if parsedRef, err := ociref.ParseDigestReference(selector); err == nil {
		record, found, lookupErr := queryRecordByDigest(ctx, db, parsedRef.Digest())
		if lookupErr != nil {
			return nil, lookupErr
		}
		if !found {
			return nil, nil
		}
		return []Record{record}, nil
	}

	if digest, ok := normalizeDigestSelector(selector); ok {
		record, found, lookupErr := queryRecordByDigest(ctx, db, digest)
		if lookupErr != nil {
			return nil, lookupErr
		}
		if !found {
			return nil, nil
		}
		return []Record{record}, nil
	}

	rows, err := db.QueryContext(ctx, `
		SELECT
			digest,
			ref,
			rootfs_path,
			size_bytes,
			created_at_unix,
			last_used_at_unix,
			source,
			oci_entrypoint_json,
			oci_cmd_json,
			oci_env_json,
			oci_workdir,
			oci_user,
			oci_os,
			oci_architecture,
			oci_variant
		FROM images
		WHERE ref = ?
	`, selector)
	if err != nil {
		return nil, fmt.Errorf("query images by ref %q: %w", selector, err)
	}
	defer rows.Close()

	out := make([]Record, 0)
	for rows.Next() {
		record, scanErr := scanRecord(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate images for ref %q: %w", selector, err)
	}
	return out, nil
}

func queryRecordByDigest(ctx context.Context, db *sql.DB, digest string) (Record, bool, error) {
	row := db.QueryRowContext(ctx, `
		SELECT
			digest,
			ref,
			rootfs_path,
			size_bytes,
			created_at_unix,
			last_used_at_unix,
			source,
			oci_entrypoint_json,
			oci_cmd_json,
			oci_env_json,
			oci_workdir,
			oci_user,
			oci_os,
			oci_architecture,
			oci_variant
		FROM images
		WHERE digest = ?
	`, digest)
	record, err := scanRecord(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return Record{}, false, nil
		}
		return Record{}, false, err
	}
	return record, true, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanRecord(s scanner) (Record, error) {
	var (
		record         Record
		createdAtUnix  int64
		lastUsedAtUnix int64
		entrypointJSON string
		cmdJSON        string
		envJSON        string
	)

	if err := s.Scan(
		&record.Digest,
		&record.Ref,
		&record.RootFSPath,
		&record.SizeBytes,
		&createdAtUnix,
		&lastUsedAtUnix,
		&record.Source,
		&entrypointJSON,
		&cmdJSON,
		&envJSON,
		&record.OCIConfig.Workdir,
		&record.OCIConfig.User,
		&record.OCIConfig.OS,
		&record.OCIConfig.Architecture,
		&record.OCIConfig.Variant,
	); err != nil {
		return Record{}, err
	}

	record.CreatedAt = time.Unix(createdAtUnix, 0).UTC()
	record.LastUsedAt = time.Unix(lastUsedAtUnix, 0).UTC()

	entrypoint, err := unmarshalStringSlice(entrypointJSON)
	if err != nil {
		return Record{}, err
	}
	cmd, err := unmarshalStringSlice(cmdJSON)
	if err != nil {
		return Record{}, err
	}
	env, err := unmarshalStringSlice(envJSON)
	if err != nil {
		return Record{}, err
	}

	record.OCIConfig.Entrypoint = entrypoint
	record.OCIConfig.Cmd = cmd
	record.OCIConfig.Env = env
	return record, nil
}

func marshalStringSlice(values []string) (string, error) {
	b, err := json.Marshal(slices.Clone(values))
	if err != nil {
		return "", fmt.Errorf("marshal OCI config string slice: %w", err)
	}
	return string(b), nil
}

func unmarshalStringSlice(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("parse OCI config string slice: %w", err)
	}
	return out, nil
}

func normalizeDigestSelector(selector string) (string, bool) {
	trimmed := strings.TrimSpace(strings.ToLower(selector))
	if trimmed == "" {
		return "", false
	}
	if strings.HasPrefix(trimmed, "sha256:") {
		digest := strings.TrimPrefix(trimmed, "sha256:")
		if len(digest) == 64 && isHexDigest(digest) {
			return "sha256:" + digest, true
		}
		return "", false
	}
	if len(trimmed) == 64 && isHexDigest(trimmed) {
		return "sha256:" + trimmed, true
	}
	return "", false
}

func isHexDigest(raw string) bool {
	for _, ch := range raw {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

func openImportStream(tarPath string, stdin io.Reader) (io.Reader, func(), error) {
	selectedPath := strings.TrimSpace(tarPath)
	if selectedPath == "" {
		selectedPath = "-"
	}

	var (
		baseReader io.Reader
		closeBase  = func() {}
	)
	if selectedPath == "-" {
		if stdin == nil {
			return nil, nil, fmt.Errorf("stdin import requested but stdin reader is nil")
		}
		baseReader = stdin
	} else {
		f, err := os.Open(selectedPath)
		if err != nil {
			return nil, nil, fmt.Errorf("open import tar stream %q: %w", selectedPath, err)
		}
		baseReader = f
		closeBase = func() {
			_ = f.Close()
		}
	}

	buffered := bufio.NewReader(baseReader)
	header, err := buffered.Peek(2)
	if err != nil && err != io.EOF {
		closeBase()
		return nil, nil, fmt.Errorf("peek import stream %q: %w", selectedPath, err)
	}

	if len(header) == 2 && header[0] == 0x1f && header[1] == 0x8b {
		gzReader, err := gzip.NewReader(buffered)
		if err != nil {
			closeBase()
			return nil, nil, fmt.Errorf("open gzip import stream %q: %w", selectedPath, err)
		}
		return gzReader, func() {
			_ = gzReader.Close()
			closeBase()
		}, nil
	}

	return buffered, closeBase, nil
}
