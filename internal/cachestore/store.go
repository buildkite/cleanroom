package cachestore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/paths"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"
)

type Record struct {
	CacheKey            string
	Stage               string
	State               string
	Backend             string
	PolicyHash          string
	Policy              *cleanroomv1.Policy
	Repository          *cleanroomv1.RepositoryCheckout
	ParentCacheKey      string
	StorageDriver       string
	StorageRef          string
	InputManifestDigest string
	CreatedAt           time.Time
	LastUsedAt          time.Time
	ProducerVersion     string
}

type Options struct {
	MetadataDBPath string
}

type Store struct {
	metadataDBPath string
}

func New(opts Options) (*Store, error) {
	metadataDBPath := strings.TrimSpace(opts.MetadataDBPath)
	if metadataDBPath == "" {
		var err error
		metadataDBPath, err = defaultMetadataDBPath()
		if err != nil {
			return nil, fmt.Errorf("resolve cache metadata database path: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(metadataDBPath), 0o755); err != nil {
		return nil, fmt.Errorf("create cache metadata directory for %q: %w", metadataDBPath, err)
	}

	store := &Store{metadataDBPath: metadataDBPath}
	if err := store.initDB(context.Background()); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) Create(ctx context.Context, record Record) error {
	if strings.TrimSpace(record.CacheKey) == "" {
		return fmt.Errorf("cache record missing cache key")
	}
	if strings.TrimSpace(record.Stage) == "" {
		return fmt.Errorf("cache record %q missing stage", record.CacheKey)
	}
	if strings.TrimSpace(record.State) == "" {
		return fmt.Errorf("cache record %q missing state", record.CacheKey)
	}
	if strings.TrimSpace(record.Backend) == "" {
		return fmt.Errorf("cache record %q missing backend", record.CacheKey)
	}
	if strings.TrimSpace(record.PolicyHash) == "" {
		return fmt.Errorf("cache record %q missing policy hash", record.CacheKey)
	}
	if record.Policy == nil {
		return fmt.Errorf("cache record %q missing policy", record.CacheKey)
	}
	if strings.TrimSpace(record.StorageRef) == "" {
		return fmt.Errorf("cache record %q missing storage ref", record.CacheKey)
	}
	if strings.TrimSpace(record.StorageDriver) == "" {
		return fmt.Errorf("cache record %q missing storage driver", record.CacheKey)
	}
	if strings.TrimSpace(record.ProducerVersion) == "" {
		return fmt.Errorf("cache record %q missing producer version", record.CacheKey)
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}
	if record.LastUsedAt.IsZero() {
		record.LastUsedAt = record.CreatedAt
	}

	policyBytes, err := proto.Marshal(record.Policy)
	if err != nil {
		return fmt.Errorf("marshal cache policy %q: %w", record.CacheKey, err)
	}
	var repositoryBytes []byte
	if record.Repository != nil {
		repositoryBytes, err = proto.Marshal(record.Repository)
		if err != nil {
			return fmt.Errorf("marshal cache repository %q: %w", record.CacheKey, err)
		}
	}

	db, err := s.open(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, `
		INSERT INTO cache_entries (
			cache_key,
			stage,
			state,
			backend,
			policy_hash,
			policy_proto,
			repository_proto,
			parent_cache_key,
			storage_driver,
			storage_ref,
			input_manifest_digest,
			created_at_unix_nano,
			last_used_at_unix_nano,
			producer_version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		record.CacheKey,
		record.Stage,
		record.State,
		record.Backend,
		record.PolicyHash,
		policyBytes,
		repositoryBytes,
		nullableString(record.ParentCacheKey),
		record.StorageDriver,
		record.StorageRef,
		nullableString(record.InputManifestDigest),
		record.CreatedAt.UTC().UnixNano(),
		record.LastUsedAt.UTC().UnixNano(),
		record.ProducerVersion,
	); err != nil {
		return fmt.Errorf("insert cache metadata %q/%q: %w", record.Stage, record.CacheKey, err)
	}
	return nil
}

func (s *Store) GetReady(ctx context.Context, stage, cacheKey string) (Record, bool, error) {
	db, err := s.open(ctx)
	if err != nil {
		return Record{}, false, err
	}
	defer db.Close()

	row := db.QueryRowContext(ctx, `
		SELECT
			cache_key,
			stage,
			state,
			backend,
			policy_hash,
			policy_proto,
			repository_proto,
			parent_cache_key,
			storage_driver,
			storage_ref,
			input_manifest_digest,
			created_at_unix_nano,
			last_used_at_unix_nano,
			producer_version
		FROM cache_entries
		WHERE stage = ? AND cache_key = ? AND state = 'ready'
	`, strings.TrimSpace(stage), strings.TrimSpace(cacheKey))

	record, err := scanRecord(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return Record{}, false, nil
		}
		return Record{}, false, err
	}
	return record, true, nil
}

func (s *Store) UpdateLastUsedAt(ctx context.Context, stage, cacheKey string, lastUsedAt time.Time) error {
	if lastUsedAt.IsZero() {
		lastUsedAt = time.Now().UTC()
	}

	db, err := s.open(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	result, err := db.ExecContext(ctx, `
		UPDATE cache_entries
		SET last_used_at_unix_nano = ?
		WHERE stage = ? AND cache_key = ?
	`, lastUsedAt.UTC().UnixNano(), strings.TrimSpace(stage), strings.TrimSpace(cacheKey))
	if err != nil {
		return fmt.Errorf("update cache last used time %q/%q: %w", stage, cacheKey, err)
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return fmt.Errorf("cache metadata %q/%q not found", stage, cacheKey)
	}
	return nil
}

func (s *Store) Touch(ctx context.Context, stage, cacheKey string) error {
	return s.UpdateLastUsedAt(ctx, stage, cacheKey, time.Now().UTC())
}

func (s *Store) List(ctx context.Context) ([]Record, error) {
	db, err := s.open(ctx)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, `
		SELECT
			cache_key,
			stage,
			state,
			backend,
			policy_hash,
			policy_proto,
			repository_proto,
			parent_cache_key,
			storage_driver,
			storage_ref,
			input_manifest_digest,
			created_at_unix_nano,
			last_used_at_unix_nano,
			producer_version
		FROM cache_entries
		ORDER BY created_at_unix_nano ASC, cache_key ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query cache metadata: %w", err)
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
		return nil, fmt.Errorf("iterate cache metadata: %w", err)
	}
	return items, nil
}

func (s *Store) Delete(ctx context.Context, stage, cacheKey string) error {
	db, err := s.open(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, `
		DELETE FROM cache_entries
		WHERE stage = ? AND cache_key = ?
	`, strings.TrimSpace(stage), strings.TrimSpace(cacheKey)); err != nil {
		return fmt.Errorf("delete cache metadata %q/%q: %w", stage, cacheKey, err)
	}
	return nil
}

func (s *Store) open(ctx context.Context) (*sql.DB, error) {
	if err := s.initDB(ctx); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", s.metadataDBPath)
	if err != nil {
		return nil, fmt.Errorf("open cache metadata database %q: %w", s.metadataDBPath, err)
	}
	return db, nil
}

func (s *Store) initDB(ctx context.Context) error {
	db, err := sql.Open("sqlite", s.metadataDBPath)
	if err != nil {
		return fmt.Errorf("open cache metadata database %q: %w", s.metadataDBPath, err)
	}
	defer db.Close()

	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS cache_entries (
			cache_key TEXT NOT NULL,
			stage TEXT NOT NULL,
			state TEXT NOT NULL,
			backend TEXT NOT NULL,
			policy_hash TEXT NOT NULL,
			policy_proto BLOB NOT NULL,
			repository_proto BLOB,
			parent_cache_key TEXT,
			storage_driver TEXT NOT NULL DEFAULT 'file',
			storage_ref TEXT NOT NULL,
			input_manifest_digest TEXT,
			created_at_unix_nano INTEGER NOT NULL,
			last_used_at_unix_nano INTEGER NOT NULL,
			producer_version TEXT NOT NULL,
			PRIMARY KEY (stage, cache_key)
		);
		CREATE INDEX IF NOT EXISTS idx_cache_entries_state_created_at ON cache_entries(state, created_at_unix_nano);
		CREATE INDEX IF NOT EXISTS idx_cache_entries_last_used_at ON cache_entries(last_used_at_unix_nano);
	`)
	if err != nil {
		return fmt.Errorf("initialise cache metadata schema: %w", err)
	}
	return nil
}

type recordScanner interface {
	Scan(dest ...any) error
}

func scanRecord(row recordScanner) (Record, error) {
	var (
		record          Record
		policyBytes     []byte
		repositoryBytes []byte
		parentCacheKey  sql.NullString
		inputDigest     sql.NullString
		createdAtNano   int64
		lastUsedAtNano  int64
	)
	if err := row.Scan(
		&record.CacheKey,
		&record.Stage,
		&record.State,
		&record.Backend,
		&record.PolicyHash,
		&policyBytes,
		&repositoryBytes,
		&parentCacheKey,
		&record.StorageDriver,
		&record.StorageRef,
		&inputDigest,
		&createdAtNano,
		&lastUsedAtNano,
		&record.ProducerVersion,
	); err != nil {
		return Record{}, err
	}

	record.Policy = &cleanroomv1.Policy{}
	if err := proto.Unmarshal(policyBytes, record.Policy); err != nil {
		return Record{}, fmt.Errorf("decode cache policy %q/%q: %w", record.Stage, record.CacheKey, err)
	}
	if len(repositoryBytes) > 0 {
		record.Repository = &cleanroomv1.RepositoryCheckout{}
		if err := proto.Unmarshal(repositoryBytes, record.Repository); err != nil {
			return Record{}, fmt.Errorf("decode cache repository %q/%q: %w", record.Stage, record.CacheKey, err)
		}
	}
	if parentCacheKey.Valid {
		record.ParentCacheKey = parentCacheKey.String
	}
	if inputDigest.Valid {
		record.InputManifestDigest = inputDigest.String
	}
	record.CreatedAt = time.Unix(0, createdAtNano).UTC()
	record.LastUsedAt = time.Unix(0, lastUsedAtNano).UTC()
	return record, nil
}

func nullableString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func defaultMetadataDBPath() (string, error) {
	base, err := paths.CacheBaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "stage-caches", "metadata.db"), nil
}
