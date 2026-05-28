package cachestore

import (
	"context"
	"database/sql"
	"encoding/json"
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
	OwnerPrincipalID         string
	OwnerScope               string
	ReuseMode                string
	State                    string
	BackingSnapshotID        string
	Backend                  string
	Architecture             string
	RuntimeBaseKey           string
	PolicyHash               string
	Policy                   *cleanroomv1.Policy
	Repository               *cleanroomv1.RepositoryCheckout
	RepositoryHasChangeset   bool
	RepositoryChangesetID    string
	ParentCacheKey           string
	StorageDriver            string
	StorageRef               string
	StorageSizeBytes         int64
	ExclusiveSizeBytes       int64
	DriverMetadata           string
	InputManifestDigest      string
	CommandDigest            string
	EnvDigest                string
	NormalizedOutputsDigest  string
	OutputManifestDigest     string
	DependencyKeyFilesDigest string
	OutputRecords            []OutputRecord
	CheckoutRefreshRequired  bool
	ImportedFromPeer         bool
	CreatedAt                time.Time
	LastUsedAt               time.Time
	LastValidatedAt          time.Time
	ProducerVersion          string
}

type OutputRecord struct {
	Kind           string `json:"kind"`
	Path           string `json:"path"`
	VolumeSubpath  string `json:"volume_subpath,omitempty"`
	StorageDriver  string `json:"storage_driver"`
	StorageRef     string `json:"storage_ref"`
	SnapshotRef    string `json:"snapshot_ref,omitempty"`
	ManifestDigest string `json:"manifest_digest,omitempty"`
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
	var outputRecordsBytes []byte
	if len(record.OutputRecords) > 0 {
		outputRecordsBytes, err = json.Marshal(record.OutputRecords)
		if err != nil {
			return fmt.Errorf("marshal cache output records %q: %w", record.CacheKey, err)
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
			owner_principal_id,
			owner_scope,
			reuse_mode,
			state,
			backing_snapshot_id,
			backend,
			architecture,
			runtime_base_key,
			policy_hash,
			policy_proto,
			repository_proto,
			repository_has_changeset,
			repository_changeset_id,
			parent_cache_key,
			storage_driver,
			storage_ref,
			storage_size_bytes,
			exclusive_size_bytes,
			driver_metadata,
			input_manifest_digest,
			command_digest,
			env_digest,
			normalized_outputs_digest,
			output_manifest_digest,
			dependency_key_files_digest,
			output_records_json,
			checkout_refresh_required,
			imported_from_peer,
			created_at_unix_nano,
			last_used_at_unix_nano,
			last_validated_at_unix_nano,
			producer_version
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	if replace {
		verb = "upsert"
		statement = `
			INSERT INTO cache_entries (
				cache_key,
				stage,
				owner_principal_id,
				owner_scope,
				reuse_mode,
				state,
				backing_snapshot_id,
				backend,
				architecture,
				runtime_base_key,
				policy_hash,
				policy_proto,
				repository_proto,
				repository_has_changeset,
				repository_changeset_id,
				parent_cache_key,
				storage_driver,
				storage_ref,
				storage_size_bytes,
				exclusive_size_bytes,
				driver_metadata,
				input_manifest_digest,
				command_digest,
				env_digest,
				normalized_outputs_digest,
				output_manifest_digest,
				dependency_key_files_digest,
				output_records_json,
				checkout_refresh_required,
				imported_from_peer,
				created_at_unix_nano,
				last_used_at_unix_nano,
				last_validated_at_unix_nano,
				producer_version
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(stage, cache_key, owner_principal_id) DO UPDATE SET
				owner_scope = excluded.owner_scope,
				reuse_mode = excluded.reuse_mode,
				state = excluded.state,
				backing_snapshot_id = excluded.backing_snapshot_id,
				backend = excluded.backend,
				architecture = excluded.architecture,
				runtime_base_key = excluded.runtime_base_key,
				policy_hash = excluded.policy_hash,
				policy_proto = excluded.policy_proto,
				repository_proto = excluded.repository_proto,
				repository_has_changeset = excluded.repository_has_changeset,
				repository_changeset_id = excluded.repository_changeset_id,
				parent_cache_key = excluded.parent_cache_key,
				storage_driver = excluded.storage_driver,
				storage_ref = excluded.storage_ref,
				storage_size_bytes = excluded.storage_size_bytes,
				exclusive_size_bytes = excluded.exclusive_size_bytes,
				driver_metadata = excluded.driver_metadata,
				input_manifest_digest = excluded.input_manifest_digest,
				command_digest = excluded.command_digest,
				env_digest = excluded.env_digest,
				normalized_outputs_digest = excluded.normalized_outputs_digest,
				output_manifest_digest = excluded.output_manifest_digest,
				dependency_key_files_digest = excluded.dependency_key_files_digest,
				output_records_json = excluded.output_records_json,
				checkout_refresh_required = excluded.checkout_refresh_required,
				imported_from_peer = excluded.imported_from_peer,
				created_at_unix_nano = excluded.created_at_unix_nano,
				last_used_at_unix_nano = excluded.last_used_at_unix_nano,
				last_validated_at_unix_nano = excluded.last_validated_at_unix_nano,
				producer_version = excluded.producer_version
		`
	}

	if _, err := db.ExecContext(ctx, statement,
		record.CacheKey,
		record.Stage,
		strings.TrimSpace(record.OwnerPrincipalID),
		nullableString(record.OwnerScope),
		nullableString(record.ReuseMode),
		record.State,
		record.BackingSnapshotID,
		record.Backend,
		nullableString(record.Architecture),
		nullableString(record.RuntimeBaseKey),
		record.PolicyHash,
		policyBytes,
		repositoryBytes,
		boolToInt(record.RepositoryHasChangeset),
		nullableString(record.RepositoryChangesetID),
		nullableString(record.ParentCacheKey),
		record.StorageDriver,
		record.StorageRef,
		record.StorageSizeBytes,
		record.ExclusiveSizeBytes,
		nullableString(record.DriverMetadata),
		nullableString(record.InputManifestDigest),
		nullableString(record.CommandDigest),
		nullableString(record.EnvDigest),
		nullableString(record.NormalizedOutputsDigest),
		nullableString(record.OutputManifestDigest),
		nullableString(record.DependencyKeyFilesDigest),
		nullableBytes(outputRecordsBytes),
		boolToInt(record.CheckoutRefreshRequired),
		boolToInt(record.ImportedFromPeer),
		record.CreatedAt.UTC().UnixNano(),
		record.LastUsedAt.UTC().UnixNano(),
		unixNanoOrZero(record.LastValidatedAt),
		record.ProducerVersion,
	); err != nil {
		return fmt.Errorf("%s cache metadata %q/%q: %w", verb, record.Stage, record.CacheKey, err)
	}
	return nil
}

func (s *Store) GetReady(ctx context.Context, stage, cacheKey string) (Record, bool, error) {
	return s.getReadyForOwner(ctx, stage, cacheKey, "")
}

func (s *Store) GetReadyForOwner(ctx context.Context, stage, cacheKey, ownerPrincipalID string) (Record, bool, error) {
	ownerPrincipalID = strings.TrimSpace(ownerPrincipalID)
	if ownerPrincipalID == "" {
		return Record{}, false, nil
	}
	return s.getReadyForOwner(ctx, stage, cacheKey, ownerPrincipalID)
}

func (s *Store) getReadyForOwner(ctx context.Context, stage, cacheKey, ownerPrincipalID string) (Record, bool, error) {
	db, err := s.open(ctx)
	if err != nil {
		return Record{}, false, err
	}
	defer db.Close()

	row := db.QueryRowContext(ctx, `
		SELECT
			cache_key,
			stage,
			owner_principal_id,
			owner_scope,
			reuse_mode,
			state,
			backing_snapshot_id,
			backend,
			architecture,
			runtime_base_key,
			policy_hash,
			policy_proto,
			repository_proto,
			repository_has_changeset,
			repository_changeset_id,
			parent_cache_key,
			storage_driver,
			storage_ref,
			storage_size_bytes,
			exclusive_size_bytes,
			driver_metadata,
			input_manifest_digest,
			command_digest,
			env_digest,
			normalized_outputs_digest,
			output_manifest_digest,
			dependency_key_files_digest,
			output_records_json,
			checkout_refresh_required,
			imported_from_peer,
			created_at_unix_nano,
			last_used_at_unix_nano,
			last_validated_at_unix_nano,
			producer_version
		FROM cache_entries
		WHERE stage = ? AND cache_key = ? AND owner_principal_id = ? AND state = 'ready'
	`, strings.TrimSpace(stage), strings.TrimSpace(cacheKey), strings.TrimSpace(ownerPrincipalID))

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
	return s.updateLastUsedAtForOwner(ctx, stage, cacheKey, "", lastUsedAt)
}

func (s *Store) UpdateLastUsedAtForOwner(ctx context.Context, stage, cacheKey, ownerPrincipalID string, lastUsedAt time.Time) error {
	ownerPrincipalID = strings.TrimSpace(ownerPrincipalID)
	if ownerPrincipalID == "" {
		return fmt.Errorf("cache metadata %q/%q missing owner principal", stage, cacheKey)
	}
	return s.updateLastUsedAtForOwner(ctx, stage, cacheKey, ownerPrincipalID, lastUsedAt)
}

func (s *Store) updateLastUsedAtForOwner(ctx context.Context, stage, cacheKey, ownerPrincipalID string, lastUsedAt time.Time) error {
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
		WHERE stage = ? AND cache_key = ? AND owner_principal_id = ?
	`, lastUsedAt.UTC().UnixNano(), strings.TrimSpace(stage), strings.TrimSpace(cacheKey), strings.TrimSpace(ownerPrincipalID))
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

func (s *Store) TouchForOwner(ctx context.Context, stage, cacheKey, ownerPrincipalID string) error {
	return s.UpdateLastUsedAtForOwner(ctx, stage, cacheKey, ownerPrincipalID, time.Now().UTC())
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
			owner_principal_id,
			owner_scope,
			reuse_mode,
			state,
			backing_snapshot_id,
			backend,
			architecture,
			runtime_base_key,
			policy_hash,
			policy_proto,
			repository_proto,
			repository_has_changeset,
			repository_changeset_id,
			parent_cache_key,
			storage_driver,
			storage_ref,
			storage_size_bytes,
			exclusive_size_bytes,
			driver_metadata,
			input_manifest_digest,
			command_digest,
			env_digest,
			normalized_outputs_digest,
			output_manifest_digest,
			dependency_key_files_digest,
			output_records_json,
			checkout_refresh_required,
			imported_from_peer,
			created_at_unix_nano,
			last_used_at_unix_nano,
			last_validated_at_unix_nano,
			producer_version
		FROM cache_entries
		ORDER BY created_at_unix_nano ASC, cache_key ASC, owner_principal_id ASC
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

func (s *Store) DeleteForOwner(ctx context.Context, stage, cacheKey, ownerPrincipalID string) error {
	db, err := s.open(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, `
		DELETE FROM cache_entries
		WHERE stage = ? AND cache_key = ? AND owner_principal_id = ?
	`, strings.TrimSpace(stage), strings.TrimSpace(cacheKey), strings.TrimSpace(ownerPrincipalID)); err != nil {
		return fmt.Errorf("delete cache metadata %q/%q for owner %q: %w", stage, cacheKey, ownerPrincipalID, err)
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
			owner_principal_id TEXT NOT NULL DEFAULT '',
			owner_scope TEXT,
			reuse_mode TEXT,
			state TEXT NOT NULL,
			backend TEXT NOT NULL,
			architecture TEXT,
			runtime_base_key TEXT,
			policy_hash TEXT NOT NULL,
			policy_proto BLOB NOT NULL,
			repository_proto BLOB,
			repository_has_changeset INTEGER NOT NULL DEFAULT 0,
			repository_changeset_id TEXT,
			parent_cache_key TEXT,
			storage_driver TEXT NOT NULL DEFAULT 'file',
			storage_ref TEXT NOT NULL,
			storage_size_bytes INTEGER NOT NULL DEFAULT 0,
			exclusive_size_bytes INTEGER NOT NULL DEFAULT 0,
			driver_metadata TEXT,
			input_manifest_digest TEXT,
			command_digest TEXT,
			env_digest TEXT,
			normalized_outputs_digest TEXT,
			output_manifest_digest TEXT,
			dependency_key_files_digest TEXT,
			output_records_json BLOB,
			checkout_refresh_required INTEGER NOT NULL DEFAULT 0,
			imported_from_peer INTEGER NOT NULL DEFAULT 0,
			created_at_unix_nano INTEGER NOT NULL,
			last_used_at_unix_nano INTEGER NOT NULL,
			last_validated_at_unix_nano INTEGER NOT NULL DEFAULT 0,
			producer_version TEXT NOT NULL,
			PRIMARY KEY (stage, cache_key, owner_principal_id)
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
	if _, err := db.ExecContext(ctx, `ALTER TABLE cache_entries ADD COLUMN architecture TEXT`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("ensure cache metadata architecture column: %w", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE cache_entries ADD COLUMN runtime_base_key TEXT`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("ensure cache metadata runtime_base_key column: %w", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE cache_entries ADD COLUMN repository_has_changeset INTEGER NOT NULL DEFAULT 0`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("ensure cache metadata repository_has_changeset column: %w", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE cache_entries ADD COLUMN repository_changeset_id TEXT`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("ensure cache metadata repository_changeset_id column: %w", err)
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
	if _, err := db.ExecContext(ctx, `ALTER TABLE cache_entries ADD COLUMN command_digest TEXT`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("ensure cache metadata command_digest column: %w", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE cache_entries ADD COLUMN env_digest TEXT`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("ensure cache metadata env_digest column: %w", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE cache_entries ADD COLUMN normalized_outputs_digest TEXT`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("ensure cache metadata normalized_outputs_digest column: %w", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE cache_entries ADD COLUMN output_manifest_digest TEXT`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("ensure cache metadata output_manifest_digest column: %w", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE cache_entries ADD COLUMN output_records_json BLOB`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("ensure cache metadata output_records_json column: %w", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE cache_entries ADD COLUMN storage_size_bytes INTEGER NOT NULL DEFAULT 0`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("ensure cache metadata storage_size_bytes column: %w", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE cache_entries ADD COLUMN exclusive_size_bytes INTEGER NOT NULL DEFAULT 0`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("ensure cache metadata exclusive_size_bytes column: %w", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE cache_entries ADD COLUMN driver_metadata TEXT`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("ensure cache metadata driver_metadata column: %w", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE cache_entries ADD COLUMN imported_from_peer INTEGER NOT NULL DEFAULT 0`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("ensure cache metadata imported_from_peer column: %w", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE cache_entries ADD COLUMN last_validated_at_unix_nano INTEGER NOT NULL DEFAULT 0`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("ensure cache metadata last_validated_at_unix_nano column: %w", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE cache_entries ADD COLUMN owner_principal_id TEXT NOT NULL DEFAULT ''`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("ensure cache metadata owner_principal_id column: %w", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE cache_entries ADD COLUMN owner_scope TEXT`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("ensure cache metadata owner_scope column: %w", err)
	}
	if err := ensureCacheEntriesOwnerPrimaryKey(ctx, db); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_cache_entries_state_created_at ON cache_entries(state, created_at_unix_nano);
		CREATE INDEX IF NOT EXISTS idx_cache_entries_last_used_at ON cache_entries(last_used_at_unix_nano);
	`); err != nil {
		return fmt.Errorf("ensure cache metadata indexes: %w", err)
	}
	return nil
}

func ensureCacheEntriesOwnerPrimaryKey(ctx context.Context, db *sql.DB) error {
	primaryKey, err := cacheEntriesPrimaryKeyColumns(ctx, db)
	if err != nil {
		return err
	}
	if stringSlicesEqual(primaryKey, []string{"stage", "cache_key", "owner_principal_id"}) {
		return nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin cache metadata owner primary key migration: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE cache_entries_owner_pk (
			cache_key TEXT NOT NULL,
			stage TEXT NOT NULL,
			owner_principal_id TEXT NOT NULL DEFAULT '',
			owner_scope TEXT,
			reuse_mode TEXT,
			state TEXT NOT NULL,
			backing_snapshot_id TEXT NOT NULL DEFAULT '',
			backend TEXT NOT NULL,
			architecture TEXT,
			runtime_base_key TEXT,
			policy_hash TEXT NOT NULL,
			policy_proto BLOB NOT NULL,
			repository_proto BLOB,
			repository_has_changeset INTEGER NOT NULL DEFAULT 0,
			repository_changeset_id TEXT,
			parent_cache_key TEXT,
			storage_driver TEXT NOT NULL DEFAULT 'file',
			storage_ref TEXT NOT NULL,
			storage_size_bytes INTEGER NOT NULL DEFAULT 0,
			exclusive_size_bytes INTEGER NOT NULL DEFAULT 0,
			driver_metadata TEXT,
			input_manifest_digest TEXT,
			command_digest TEXT,
			env_digest TEXT,
			normalized_outputs_digest TEXT,
			output_manifest_digest TEXT,
			dependency_key_files_digest TEXT,
			output_records_json BLOB,
			checkout_refresh_required INTEGER NOT NULL DEFAULT 0,
			imported_from_peer INTEGER NOT NULL DEFAULT 0,
			created_at_unix_nano INTEGER NOT NULL,
			last_used_at_unix_nano INTEGER NOT NULL,
			last_validated_at_unix_nano INTEGER NOT NULL DEFAULT 0,
			producer_version TEXT NOT NULL,
			PRIMARY KEY (stage, cache_key, owner_principal_id)
		)
	`); err != nil {
		return fmt.Errorf("create cache metadata owner primary key table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO cache_entries_owner_pk (
			cache_key,
			stage,
			owner_principal_id,
			owner_scope,
			reuse_mode,
			state,
			backing_snapshot_id,
			backend,
			architecture,
			runtime_base_key,
			policy_hash,
			policy_proto,
			repository_proto,
			repository_has_changeset,
			repository_changeset_id,
			parent_cache_key,
			storage_driver,
			storage_ref,
			storage_size_bytes,
			exclusive_size_bytes,
			driver_metadata,
			input_manifest_digest,
			command_digest,
			env_digest,
			normalized_outputs_digest,
			output_manifest_digest,
			dependency_key_files_digest,
			output_records_json,
			checkout_refresh_required,
			imported_from_peer,
			created_at_unix_nano,
			last_used_at_unix_nano,
			last_validated_at_unix_nano,
			producer_version
		)
		SELECT
			cache_key,
			stage,
			COALESCE(owner_principal_id, ''),
			owner_scope,
			reuse_mode,
			state,
			backing_snapshot_id,
			backend,
			architecture,
			runtime_base_key,
			policy_hash,
			policy_proto,
			repository_proto,
			repository_has_changeset,
			repository_changeset_id,
			parent_cache_key,
			storage_driver,
			storage_ref,
			storage_size_bytes,
			exclusive_size_bytes,
			driver_metadata,
			input_manifest_digest,
			command_digest,
			env_digest,
			normalized_outputs_digest,
			output_manifest_digest,
			dependency_key_files_digest,
			output_records_json,
			checkout_refresh_required,
			imported_from_peer,
			created_at_unix_nano,
			last_used_at_unix_nano,
			last_validated_at_unix_nano,
			producer_version
		FROM cache_entries
	`); err != nil {
		return fmt.Errorf("copy cache metadata into owner primary key table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE cache_entries`); err != nil {
		return fmt.Errorf("drop old cache metadata table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE cache_entries_owner_pk RENAME TO cache_entries`); err != nil {
		return fmt.Errorf("rename cache metadata owner primary key table: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit cache metadata owner primary key migration: %w", err)
	}
	return nil
}

func cacheEntriesPrimaryKeyColumns(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(cache_entries)`)
	if err != nil {
		return nil, fmt.Errorf("inspect cache metadata primary key: %w", err)
	}
	defer rows.Close()

	type primaryKeyColumn struct {
		name string
		pos  int
	}
	var cols []primaryKeyColumn
	for rows.Next() {
		var (
			cid       int
			name      string
			columnTyp string
			notNull   int
			defaultV  any
			pk        int
		)
		if err := rows.Scan(&cid, &name, &columnTyp, &notNull, &defaultV, &pk); err != nil {
			return nil, fmt.Errorf("scan cache metadata primary key: %w", err)
		}
		if pk > 0 {
			cols = append(cols, primaryKeyColumn{name: name, pos: pk})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cache metadata primary key: %w", err)
	}
	for i := 1; i < len(cols); i++ {
		for j := i; j > 0 && cols[j-1].pos > cols[j].pos; j-- {
			cols[j-1], cols[j] = cols[j], cols[j-1]
		}
	}
	names := make([]string, 0, len(cols))
	for _, col := range cols {
		names = append(names, col.name)
	}
	return names, nil
}

func stringSlicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
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
		importedFromPeer         int
		ownerScope               sql.NullString
		reuseMode                sql.NullString
		architecture             sql.NullString
		runtimeBaseKey           sql.NullString
		repositoryChangesetID    sql.NullString
		parentCacheKey           sql.NullString
		driverMetadata           sql.NullString
		inputDigest              sql.NullString
		commandDigest            sql.NullString
		envDigest                sql.NullString
		normalizedOutputsDigest  sql.NullString
		outputManifestDigest     sql.NullString
		dependencyKeyFilesDigest sql.NullString
		outputRecordsBytes       []byte
		createdAtNano            int64
		lastUsedAtNano           int64
		lastValidatedAtNano      int64
	)
	if err := row.Scan(
		&record.CacheKey,
		&record.Stage,
		&record.OwnerPrincipalID,
		&ownerScope,
		&reuseMode,
		&record.State,
		&record.BackingSnapshotID,
		&record.Backend,
		&architecture,
		&runtimeBaseKey,
		&record.PolicyHash,
		&policyBytes,
		&repositoryBytes,
		&repositoryHasChangeset,
		&repositoryChangesetID,
		&parentCacheKey,
		&record.StorageDriver,
		&record.StorageRef,
		&record.StorageSizeBytes,
		&record.ExclusiveSizeBytes,
		&driverMetadata,
		&inputDigest,
		&commandDigest,
		&envDigest,
		&normalizedOutputsDigest,
		&outputManifestDigest,
		&dependencyKeyFilesDigest,
		&outputRecordsBytes,
		&checkoutRefreshRequired,
		&importedFromPeer,
		&createdAtNano,
		&lastUsedAtNano,
		&lastValidatedAtNano,
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
	if ownerScope.Valid {
		record.OwnerScope = ownerScope.String
	}
	if repositoryChangesetID.Valid {
		record.RepositoryChangesetID = repositoryChangesetID.String
	}
	if architecture.Valid {
		record.Architecture = architecture.String
	}
	if runtimeBaseKey.Valid {
		record.RuntimeBaseKey = runtimeBaseKey.String
	}
	if parentCacheKey.Valid {
		record.ParentCacheKey = parentCacheKey.String
	}
	if reuseMode.Valid {
		record.ReuseMode = reuseMode.String
	}
	if driverMetadata.Valid {
		record.DriverMetadata = driverMetadata.String
	}
	if inputDigest.Valid {
		record.InputManifestDigest = inputDigest.String
	}
	if commandDigest.Valid {
		record.CommandDigest = commandDigest.String
	}
	if envDigest.Valid {
		record.EnvDigest = envDigest.String
	}
	if normalizedOutputsDigest.Valid {
		record.NormalizedOutputsDigest = normalizedOutputsDigest.String
	}
	if outputManifestDigest.Valid {
		record.OutputManifestDigest = outputManifestDigest.String
	}
	if dependencyKeyFilesDigest.Valid {
		record.DependencyKeyFilesDigest = dependencyKeyFilesDigest.String
	}
	if len(outputRecordsBytes) > 0 {
		if err := json.Unmarshal(outputRecordsBytes, &record.OutputRecords); err != nil {
			return Record{}, fmt.Errorf("decode cache output records %q/%q: %w", record.Stage, record.CacheKey, err)
		}
	}
	record.CheckoutRefreshRequired = checkoutRefreshRequired != 0
	record.ImportedFromPeer = importedFromPeer != 0
	record.CreatedAt = time.Unix(0, createdAtNano).UTC()
	record.LastUsedAt = time.Unix(0, lastUsedAtNano).UTC()
	if lastValidatedAtNano != 0 {
		record.LastValidatedAt = time.Unix(0, lastValidatedAtNano).UTC()
	}
	return record, nil
}

func nullableString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func nullableBytes(value []byte) any {
	if len(value) == 0 {
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

func unixNanoOrZero(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UTC().UnixNano()
}

func defaultMetadataDBPath() (string, error) {
	base, err := paths.CacheBaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "stage-caches", "metadata.db"), nil
}
