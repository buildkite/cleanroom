package imagemgr

import (
	"bufio"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/buildkite/cleanroom/internal/ociref"
	"github.com/buildkite/cleanroom/internal/paths"
	_ "modernc.org/sqlite"
)

const defaultMkfsBinary = "mkfs.ext4"

type OCIConfig struct {
	Entrypoint []string
	Cmd        []string
	Env        []string
	Workdir    string
	User       string
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

	PullImage         func(context.Context, string) (io.ReadCloser, OCIConfig, error)
	MaterializeRootFS func(context.Context, io.Reader, string) (int64, error)
}

type Manager struct {
	cacheDir       string
	metadataDBPath string
	mkfsBinary     string
	now            func() time.Time
	pullImage      func(context.Context, string) (io.ReadCloser, OCIConfig, error)
	materialize    func(context.Context, io.Reader, string) (int64, error)

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
		mkfsBinary = defaultMkfsBinary
	}

	manager := &Manager{
		cacheDir:       cacheDir,
		metadataDBPath: metadataDBPath,
		mkfsBinary:     mkfsBinary,
		now:            now,
	}
	if opts.PullImage != nil {
		manager.pullImage = opts.PullImage
	} else {
		manager.pullImage = pullImageFromRegistry
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

	tarStream, config, err := m.pullImage(ctx, parsedRef.Original)
	if err != nil {
		return EnsureResult{}, err
	}
	defer tarStream.Close()

	record, err = m.persistFromTarStream(ctx, persistFromTarRequest{
		Ref:        parsedRef.Original,
		Digest:     parsedRef.Digest(),
		TarStream:  tarStream,
		OCIConfig:  config,
		Source:     "registry",
		CreatedAt:  now,
		LastUsedAt: now,
	})
	if err != nil {
		return EnsureResult{}, err
	}

	return EnsureResult{Record: record, CacheHit: false}, nil
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
			oci_user
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
		if _, err := db.ExecContext(ctx, `DELETE FROM images WHERE digest = ?`, record.Digest); err != nil {
			return nil, fmt.Errorf("delete cached image metadata for %s: %w", record.Digest, err)
		}
	}

	for _, record := range records {
		if err := os.Remove(record.RootFSPath); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("remove cached rootfs %q: %w", record.RootFSPath, err)
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

	outputPath := filepath.Join(m.cacheDir, strings.TrimPrefix(req.Digest, "sha256:")+".ext4")
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
			oci_user TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_images_ref ON images(ref);
	`)
	if err != nil {
		return fmt.Errorf("initialise image metadata schema: %w", err)
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
			oci_user
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
			oci_user
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
			oci_user = excluded.oci_user
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
			oci_user
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
			oci_user
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
