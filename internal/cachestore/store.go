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
	CacheKey                 string
	Stage                    string
	ReuseMode                string
	State                    string
	BackingSnapshotID        string
	Backend                  string
	PolicyHash               string
	Policy                   *cleanroomv1.Policy
	Repository               *cleanroomv1.RepositoryCheckout
	RepositoryHasChangeset   bool
	ParentCacheKey           string
	StorageDriver            string
	StorageRef               string
	InputManifestDigest      string
	DependencyKeyFilesDigest string
	CheckoutRefreshRequired  bool
	CreatedAt                time.Time
	LastUsedAt               time.Time
	ProducerVersion          string
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
	return s.persist(ctx, record, false)
}

func (s *Store) Upsert(ctx context.Context, record Record) error {
	return s.persist(ctx, record, true)
}

func (s *Store) persist(ctx context.Context, record Record, replace bool) error {
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
	if strings.TrimSpace(record.BackingSnapshotID) == "" {
		return fmt.Errorf("cache record %q missing backing snapshot id", record.CacheKey)
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

	verb := "insert"
	statement := `
		INSERT INTO cache_entries (
			cache_key,
			stage,
			reuse_mode,
			state,
			backing_snapshot_id,
			backend,
			policy_hash,
			policy_proto,
			repository_proto,
			repository_has_changeset,
			parent_cache_key,
			storage_driver,
			storage_ref,
			input_manifest_digest,
			dependency_key_files_digest,
			checkout_refresh_required,
			created_at_unix_nano,
			last_used_at_unix_nano,
			producer_version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	if replace {
		verb = "upsert"
		statement = `
			INSERT INTO cache_entries (
				cache_key,
				stage,
				reuse_mode,
				state,
				backing_snapshot_id,
				backend,
				policy_hash,
				policy_proto,
				repository_proto,
				repository_has_changeset,
				parent_cache_key,
				storage_driver,
				storage_ref,
				input_manifest_digest,
				dependency_key_files_digest,
				checkout_refresh_required,
				created_at_unix_nano,
				last_used_at_unix_nano,
				producer_version
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(stage, cache_key) DO UPDATE SET
				reuse_mode = excluded.reuse_mode,
				state = excluded.state,
				backing_snapshot_id = excluded.backing_snapshot_id,
				backend = excluded.backend,
				policy_hash = excluded.policy_hash,
				policy_proto = excluded.policy_proto,
				repository_proto = excluded.repository_proto,
				repository_has_changeset = excluded.repository_has_changeset,
				parent_cache_key = excluded.parent_cache_key,
				storage_driver = excluded.storage_driver,
				storage_ref = excluded.storage_ref,
				input_manifest_digest = excluded.input_manifest_digest,
				dependency_key_files_digest = excluded.dependency_key_files_digest,
				checkout_refresh_required = excluded.checkout_refresh_required,
				created_at_unix_nano = excluded.created_at_unix_nano,
				last_used_at_unix_nano = excluded.last_used_at_unix_nano,
				producer_version = excluded.producer_version
		`
	}

	if _, err := db.ExecContext(ctx, statement,
		record.CacheKey,
		record.Stage,
		nullableString(record.ReuseMode),
		record.State,
		record.BackingSnapshotID,
		record.Backend,
		record.PolicyHash,
		policyBytes,
		repositoryBytes,
		boolToInt(record.RepositoryHasChangeset),
		nullableString(record.ParentCacheKey),
		record.StorageDriver,
		record.StorageRef,
		nullableString(record.InputManifestDigest),
		nullableString(record.DependencyKeyFilesDigest),
		boolToInt(record.CheckoutRefreshRequired),
		record.CreatedAt.UTC().UnixNano(),
		record.LastUsedAt.UTC().UnixNano(),
		record.ProducerVersion,
	); err != nil {
		return fmt.Errorf("%s cache metadata %q/%q: %w", verb, record.Stage, record.CacheKey, err)
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
			reuse_mode,
			state,
			backing_snapshot_id,
			backend,
			policy_hash,
			policy_proto,
			repository_proto,
			repository_has_changeset,
			parent_cache_key,
			storage_driver,
			storage_ref,
			input_manifest_digest,
			dependency_key_files_digest,
			checkout_refresh_required,
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
			reuse_mode,
			state,
			backing_snapshot_id,
			backend,
			policy_hash,
			policy_proto,
			repository_proto,
			repository_has_changeset,
			parent_cache_key,
			storage_driver,
			storage_ref,
			input_manifest_digest,
			dependency_key_files_digest,
			checkout_refresh_required,
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
			reuse_mode TEXT,
			state TEXT NOT NULL,
			backend TEXT NOT NULL,
			policy_hash TEXT NOT NULL,
			policy_proto BLOB NOT NULL,
			repository_proto BLOB,
			repository_has_changeset INTEGER NOT NULL DEFAULT 0,
			parent_cache_key TEXT,
			storage_driver TEXT NOT NULL DEFAULT 'file',
			storage_ref TEXT NOT NULL,
			input_manifest_digest TEXT,
			dependency_key_files_digest TEXT,
			checkout_refresh_required INTEGER NOT NULL DEFAULT 0,
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
	if _, err := db.ExecContext(ctx, `ALTER TABLE cache_entries ADD COLUMN backing_snapshot_id TEXT NOT NULL DEFAULT ''`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("ensure cache metadata backing_snapshot_id column: %w", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE cache_entries ADD COLUMN repository_has_changeset INTEGER NOT NULL DEFAULT 0`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("ensure cache metadata repository_has_changeset column: %w", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE cache_entries ADD COLUMN reuse_mode TEXT`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("ensure cache metadata reuse_mode column: %w", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE cache_entries ADD COLUMN dependency_key_files_digest TEXT`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("ensure cache metadata dependency_key_files_digest column: %w", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE cache_entries ADD COLUMN checkout_refresh_required INTEGER NOT NULL DEFAULT 0`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("ensure cache metadata checkout_refresh_required column: %w", err)
	}
	return nil
}

type recordScanner interface {
	Scan(dest ...any) error
}

func scanRecord(row recordScanner) (Record, error) {
	var (
		record                   Record
		policyBytes              []byte
		repositoryBytes          []byte
		repositoryHasChangeset   int
		checkoutRefreshRequired  int
		reuseMode                sql.NullString
		parentCacheKey           sql.NullString
		inputDigest              sql.NullString
		dependencyKeyFilesDigest sql.NullString
		createdAtNano            int64
		lastUsedAtNano           int64
	)
	if err := row.Scan(
		&record.CacheKey,
		&record.Stage,
		&reuseMode,
		&record.State,
		&record.BackingSnapshotID,
		&record.Backend,
		&record.PolicyHash,
		&policyBytes,
		&repositoryBytes,
		&repositoryHasChangeset,
		&parentCacheKey,
		&record.StorageDriver,
		&record.StorageRef,
		&inputDigest,
		&dependencyKeyFilesDigest,
		&checkoutRefreshRequired,
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
	record.RepositoryHasChangeset = repositoryHasChangeset != 0
	if parentCacheKey.Valid {
		record.ParentCacheKey = parentCacheKey.String
	}
	if reuseMode.Valid {
		record.ReuseMode = reuseMode.String
	}
	if inputDigest.Valid {
		record.InputManifestDigest = inputDigest.String
	}
	if dependencyKeyFilesDigest.Valid {
		record.DependencyKeyFilesDigest = dependencyKeyFilesDigest.String
	}
	record.CheckoutRefreshRequired = checkoutRefreshRequired != 0
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

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func defaultMetadataDBPath() (string, error) {
	base, err := paths.CacheBaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "stage-caches", "metadata.db"), nil
}
