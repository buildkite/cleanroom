package snapshotstore

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
	SnapshotID             string
	SourceSandboxID        string
	Backend                string
	Name                   string
	PolicyHash             string
	Policy                 *cleanroomv1.Policy
	Repository             *cleanroomv1.RepositoryCheckout
	RepositoryHasChangeset bool
	StorageDriver          string
	StorageRef             string
	CreatedAt              time.Time
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
		metadataDBPath, err = paths.SnapshotMetadataDBPath()
		if err != nil {
			return nil, fmt.Errorf("resolve snapshot metadata database path: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(metadataDBPath), 0o755); err != nil {
		return nil, fmt.Errorf("create snapshot metadata directory for %q: %w", metadataDBPath, err)
	}

	store := &Store{metadataDBPath: metadataDBPath}
	if err := store.initDB(context.Background()); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) Create(ctx context.Context, record Record) error {
	if strings.TrimSpace(record.SnapshotID) == "" {
		return fmt.Errorf("snapshot record missing snapshot id")
	}
	if strings.TrimSpace(record.SourceSandboxID) == "" {
		return fmt.Errorf("snapshot record %q missing source sandbox id", record.SnapshotID)
	}
	if strings.TrimSpace(record.Backend) == "" {
		return fmt.Errorf("snapshot record %q missing backend", record.SnapshotID)
	}
	if strings.TrimSpace(record.PolicyHash) == "" {
		return fmt.Errorf("snapshot record %q missing policy hash", record.SnapshotID)
	}
	if record.Policy == nil {
		return fmt.Errorf("snapshot record %q missing policy", record.SnapshotID)
	}
	if strings.TrimSpace(record.StorageRef) == "" {
		return fmt.Errorf("snapshot record %q missing storage ref", record.SnapshotID)
	}
	if strings.TrimSpace(record.StorageDriver) == "" {
		return fmt.Errorf("snapshot record %q missing storage driver", record.SnapshotID)
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}

	policyBytes, err := proto.Marshal(record.Policy)
	if err != nil {
		return fmt.Errorf("marshal snapshot policy %q: %w", record.SnapshotID, err)
	}
	var repositoryBytes []byte
	if record.Repository != nil {
		repositoryBytes, err = proto.Marshal(record.Repository)
		if err != nil {
			return fmt.Errorf("marshal snapshot repository %q: %w", record.SnapshotID, err)
		}
	}

	db, err := s.open(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, `
		INSERT INTO snapshots (
			snapshot_id,
			source_sandbox_id,
			backend,
			name,
			policy_hash,
			policy_proto,
			repository_proto,
			repository_has_changeset,
			storage_driver,
			storage_ref,
			created_at_unix,
			created_at_unix_nano
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		record.SnapshotID,
		record.SourceSandboxID,
		record.Backend,
		record.Name,
		record.PolicyHash,
		policyBytes,
		repositoryBytes,
		boolToInt(record.RepositoryHasChangeset),
		record.StorageDriver,
		record.StorageRef,
		record.CreatedAt.UTC().Unix(),
		record.CreatedAt.UTC().UnixNano(),
	); err != nil {
		return fmt.Errorf("insert snapshot metadata %q: %w", record.SnapshotID, err)
	}
	return nil
}

func (s *Store) Get(ctx context.Context, snapshotID string) (Record, bool, error) {
	db, err := s.open(ctx)
	if err != nil {
		return Record{}, false, err
	}
	defer db.Close()

	row := db.QueryRowContext(ctx, `
		SELECT
			snapshot_id,
			source_sandbox_id,
			backend,
			name,
			policy_hash,
			policy_proto,
			repository_proto,
			repository_has_changeset,
			storage_driver,
			storage_ref,
			created_at_unix_nano
		FROM snapshots
		WHERE snapshot_id = ?
	`, strings.TrimSpace(snapshotID))

	record, err := scanRecord(row)
	if err != nil {
		if err == sql.ErrNoRows {
			return Record{}, false, nil
		}
		return Record{}, false, err
	}
	return record, true, nil
}

func (s *Store) List(ctx context.Context) ([]Record, error) {
	db, err := s.open(ctx)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, `
		SELECT
			snapshot_id,
			source_sandbox_id,
			backend,
			name,
			policy_hash,
			policy_proto,
			repository_proto,
			repository_has_changeset,
			storage_driver,
			storage_ref,
			created_at_unix_nano
		FROM snapshots
		ORDER BY created_at_unix_nano ASC, snapshot_id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query snapshots: %w", err)
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
		return nil, fmt.Errorf("iterate snapshots: %w", err)
	}
	return items, nil
}

func (s *Store) Delete(ctx context.Context, snapshotID string) error {
	db, err := s.open(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, `DELETE FROM snapshots WHERE snapshot_id = ?`, strings.TrimSpace(snapshotID)); err != nil {
		return fmt.Errorf("delete snapshot metadata %q: %w", snapshotID, err)
	}
	return nil
}

func (s *Store) open(ctx context.Context) (*sql.DB, error) {
	if err := s.initDB(ctx); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", s.metadataDBPath)
	if err != nil {
		return nil, fmt.Errorf("open snapshot metadata database %q: %w", s.metadataDBPath, err)
	}
	return db, nil
}

func (s *Store) initDB(ctx context.Context) error {
	db, err := sql.Open("sqlite", s.metadataDBPath)
	if err != nil {
		return fmt.Errorf("open snapshot metadata database %q: %w", s.metadataDBPath, err)
	}
	defer db.Close()

	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS snapshots (
			snapshot_id TEXT PRIMARY KEY,
			source_sandbox_id TEXT NOT NULL,
			backend TEXT NOT NULL,
			name TEXT NOT NULL,
			policy_hash TEXT NOT NULL,
			policy_proto BLOB NOT NULL,
			repository_proto BLOB,
			repository_has_changeset INTEGER NOT NULL DEFAULT 0,
			storage_driver TEXT NOT NULL DEFAULT 'file',
			storage_ref TEXT NOT NULL,
			created_at_unix INTEGER NOT NULL,
			created_at_unix_nano INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS idx_snapshots_created_at ON snapshots(created_at_unix);
	`)
	if err != nil {
		return fmt.Errorf("initialise snapshot metadata schema: %w", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE snapshots ADD COLUMN storage_driver TEXT NOT NULL DEFAULT 'file'`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("ensure snapshot metadata storage_driver column: %w", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE snapshots ADD COLUMN repository_proto BLOB`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("ensure snapshot metadata repository_proto column: %w", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE snapshots ADD COLUMN repository_has_changeset INTEGER NOT NULL DEFAULT 0`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("ensure snapshot metadata repository_has_changeset column: %w", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE snapshots ADD COLUMN created_at_unix_nano INTEGER NOT NULL DEFAULT 0`); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return fmt.Errorf("ensure snapshot metadata created_at_unix_nano column: %w", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE snapshots SET created_at_unix_nano = created_at_unix * 1000000000 WHERE created_at_unix_nano = 0`); err != nil {
		return fmt.Errorf("backfill snapshot metadata created_at_unix_nano column: %w", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_snapshots_created_at_nano ON snapshots(created_at_unix_nano)`); err != nil {
		return fmt.Errorf("ensure snapshot metadata created_at_unix_nano index: %w", err)
	}
	return nil
}

type recordScanner interface {
	Scan(dest ...any) error
}

func scanRecord(row recordScanner) (Record, error) {
	var (
		record                 Record
		policyBytes            []byte
		repositoryBytes        []byte
		repositoryHasChangeset int
		createdAtNano          int64
	)
	if err := row.Scan(
		&record.SnapshotID,
		&record.SourceSandboxID,
		&record.Backend,
		&record.Name,
		&record.PolicyHash,
		&policyBytes,
		&repositoryBytes,
		&repositoryHasChangeset,
		&record.StorageDriver,
		&record.StorageRef,
		&createdAtNano,
	); err != nil {
		return Record{}, err
	}

	record.Policy = &cleanroomv1.Policy{}
	if err := proto.Unmarshal(policyBytes, record.Policy); err != nil {
		return Record{}, fmt.Errorf("decode snapshot policy %q: %w", record.SnapshotID, err)
	}
	if len(repositoryBytes) > 0 {
		record.Repository = &cleanroomv1.RepositoryCheckout{}
		if err := proto.Unmarshal(repositoryBytes, record.Repository); err != nil {
			return Record{}, fmt.Errorf("decode snapshot repository %q: %w", record.SnapshotID, err)
		}
	}
	record.RepositoryHasChangeset = repositoryHasChangeset != 0
	record.CreatedAt = time.Unix(0, createdAtNano).UTC()
	return record, nil
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
