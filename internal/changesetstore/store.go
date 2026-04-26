// Package changesetstore persists explicit repository changesets separately
// from user snapshots and system stage caches.
package changesetstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	cleanroomv1 "github.com/buildkite/cleanroom/internal/gen/cleanroom/v1"
	"github.com/buildkite/cleanroom/internal/paths"
	"github.com/buildkite/cleanroom/internal/repositorychangeset"
	"github.com/buildkite/cleanroom/internal/repositorycheckout"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"
)

const (
	TransportFormatProtoV1 = "repository-changeset-proto-v1"
	idFormatVersion        = "v1"
)

type Record struct {
	ChangesetID        string
	CanonicalRemoteURL string
	BaseCommitSHA      string
	SubmoduleMode      string
	ChangesetDigest    string
	FinalTreeDigest    string
	TransportFormat    string
	TransportRef       string
	PayloadDigest      string
	CreatedAt          time.Time
	LastUsedAt         time.Time
}

type Options struct {
	MetadataDBPath string
	PayloadDir     string
}

type Store struct {
	metadataDBPath string
	payloadDir     string
}

func New(opts Options) (*Store, error) {
	metadataDBPath := strings.TrimSpace(opts.MetadataDBPath)
	payloadDir := strings.TrimSpace(opts.PayloadDir)
	if metadataDBPath == "" || payloadDir == "" {
		defaultMetadataDBPath, defaultPayloadDir, err := defaultPaths()
		if err != nil {
			return nil, fmt.Errorf("resolve changeset store paths: %w", err)
		}
		if metadataDBPath == "" {
			metadataDBPath = defaultMetadataDBPath
		}
		if payloadDir == "" {
			payloadDir = defaultPayloadDir
		}
	}
	if err := os.MkdirAll(filepath.Dir(metadataDBPath), 0o755); err != nil {
		return nil, fmt.Errorf("create changeset metadata directory for %q: %w", metadataDBPath, err)
	}
	if err := os.MkdirAll(payloadDir, 0o755); err != nil {
		return nil, fmt.Errorf("create changeset payload directory for %q: %w", payloadDir, err)
	}

	store := &Store{
		metadataDBPath: metadataDBPath,
		payloadDir:     payloadDir,
	}
	if err := store.initDB(context.Background()); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) Put(ctx context.Context, repository *repositorycheckout.Checkout, changeset *repositorychangeset.Changeset) (Record, error) {
	record, payload, err := newRecord(repository, changeset)
	if err != nil {
		return Record{}, err
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}
	if record.LastUsedAt.IsZero() {
		record.LastUsedAt = record.CreatedAt
	}
	if err := s.writePayload(record, payload); err != nil {
		return Record{}, err
	}

	db, err := s.open(ctx)
	if err != nil {
		return Record{}, err
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, `
		INSERT INTO changesets (
			changeset_id,
			canonical_remote_url,
			base_commit_sha,
			submodule_mode,
			changeset_digest,
			final_tree_digest,
			transport_format,
			transport_ref,
			payload_digest,
			created_at_unix_nano,
			last_used_at_unix_nano
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(changeset_id) DO UPDATE SET
			canonical_remote_url = excluded.canonical_remote_url,
			base_commit_sha = excluded.base_commit_sha,
			submodule_mode = excluded.submodule_mode,
			changeset_digest = excluded.changeset_digest,
			final_tree_digest = excluded.final_tree_digest,
			transport_format = excluded.transport_format,
			transport_ref = excluded.transport_ref,
			payload_digest = excluded.payload_digest,
			last_used_at_unix_nano = excluded.last_used_at_unix_nano
	`, record.ChangesetID,
		record.CanonicalRemoteURL,
		record.BaseCommitSHA,
		record.SubmoduleMode,
		record.ChangesetDigest,
		record.FinalTreeDigest,
		record.TransportFormat,
		record.TransportRef,
		record.PayloadDigest,
		record.CreatedAt.UTC().UnixNano(),
		record.LastUsedAt.UTC().UnixNano(),
	); err != nil {
		return Record{}, fmt.Errorf("persist changeset metadata %q: %w", record.ChangesetID, err)
	}
	stored, ok, err := s.getRecord(ctx, record.ChangesetID)
	if err != nil {
		return Record{}, err
	}
	if !ok {
		return Record{}, fmt.Errorf("changeset metadata %q was not visible after persist", record.ChangesetID)
	}
	return stored, nil
}

func (s *Store) Get(ctx context.Context, changesetID string) (Record, *repositorychangeset.Changeset, bool, error) {
	record, ok, err := s.getRecord(ctx, changesetID)
	if err != nil || !ok {
		return record, nil, ok, err
	}
	changeset, err := s.readPayload(record)
	if err != nil {
		return Record{}, nil, false, err
	}
	return record, changeset, true, nil
}

func (s *Store) getRecord(ctx context.Context, changesetID string) (Record, bool, error) {
	db, err := s.open(ctx)
	if err != nil {
		return Record{}, false, err
	}
	defer db.Close()

	row := db.QueryRowContext(ctx, `
		SELECT
			changeset_id,
			canonical_remote_url,
			base_commit_sha,
			submodule_mode,
			changeset_digest,
			final_tree_digest,
			transport_format,
			transport_ref,
			payload_digest,
			created_at_unix_nano,
			last_used_at_unix_nano
		FROM changesets
		WHERE changeset_id = ?
	`, strings.TrimSpace(changesetID))

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
			changeset_id,
			canonical_remote_url,
			base_commit_sha,
			submodule_mode,
			changeset_digest,
			final_tree_digest,
			transport_format,
			transport_ref,
			payload_digest,
			created_at_unix_nano,
			last_used_at_unix_nano
		FROM changesets
		ORDER BY created_at_unix_nano ASC, changeset_id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query changesets: %w", err)
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
		return nil, fmt.Errorf("iterate changesets: %w", err)
	}
	return items, nil
}

func (s *Store) Delete(ctx context.Context, changesetID string) error {
	_, ok, err := s.getRecord(ctx, changesetID)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	db, err := s.open(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, `
		DELETE FROM changesets
		WHERE changeset_id = ?
	`, strings.TrimSpace(changesetID)); err != nil {
		return fmt.Errorf("delete changeset metadata %q: %w", changesetID, err)
	}
	return nil
}

func RecordID(repository *repositorycheckout.Checkout, changeset *repositorychangeset.Changeset) string {
	if repository == nil || changeset == nil {
		return ""
	}
	remoteURL := strings.TrimSpace(repository.RemoteURL)
	baseCommitSHA := strings.ToLower(strings.TrimSpace(changeset.BaseCommitSHA))
	changesetDigest := strings.TrimSpace(changeset.Digest)
	if remoteURL == "" || baseCommitSHA == "" || changesetDigest == "" {
		return ""
	}

	payload := strings.Join([]string{
		"cleanroom/changesetstore",
		idFormatVersion,
		remoteURL,
		baseCommitSHA,
		submoduleMode(repository),
		changesetDigest,
	}, "\x00")
	sum := sha256.Sum256([]byte(payload))
	return "changeset:" + idFormatVersion + ":" + hex.EncodeToString(sum[:])
}

func newRecord(repository *repositorycheckout.Checkout, changeset *repositorychangeset.Changeset) (Record, []byte, error) {
	if repository == nil {
		return Record{}, nil, errors.New("changeset store requires a repository checkout")
	}
	if changeset == nil {
		return Record{}, nil, errors.New("changeset store requires a repository changeset")
	}
	if err := changeset.ValidateForCheckout(repository); err != nil {
		return Record{}, nil, err
	}
	if err := changeset.ValidateContent(); err != nil {
		return Record{}, nil, err
	}

	protoBytes, err := proto.Marshal(changeset.ToProto())
	if err != nil {
		return Record{}, nil, fmt.Errorf("marshal repository changeset payload: %w", err)
	}
	payloadDigest := sha256Bytes(protoBytes)
	transportRef, err := payloadRef(payloadDigest)
	if err != nil {
		return Record{}, nil, err
	}
	record := Record{
		ChangesetID:        RecordID(repository, changeset),
		CanonicalRemoteURL: strings.TrimSpace(repository.RemoteURL),
		BaseCommitSHA:      strings.ToLower(strings.TrimSpace(changeset.BaseCommitSHA)),
		SubmoduleMode:      submoduleMode(repository),
		ChangesetDigest:    strings.TrimSpace(changeset.Digest),
		FinalTreeDigest:    strings.TrimSpace(changeset.TreeDigest),
		TransportFormat:    TransportFormatProtoV1,
		TransportRef:       transportRef,
		PayloadDigest:      payloadDigest,
	}
	if record.ChangesetID == "" {
		return Record{}, nil, errors.New("changeset store could not derive changeset id")
	}
	return record, protoBytes, nil
}

func (s *Store) writePayload(record Record, payload []byte) error {
	if strings.TrimSpace(record.PayloadDigest) == "" {
		return fmt.Errorf("changeset %q missing payload digest", record.ChangesetID)
	}
	if got := sha256Bytes(payload); got != strings.TrimSpace(record.PayloadDigest) {
		return fmt.Errorf("changeset %q payload digest mismatch: got %s want %s", record.ChangesetID, got, record.PayloadDigest)
	}

	target, err := s.payloadPath(record)
	if err != nil {
		return err
	}
	if existing, err := os.ReadFile(target); err == nil {
		if got := sha256Bytes(existing); got != record.PayloadDigest {
			return fmt.Errorf("changeset payload %q digest mismatch: got %s want %s", target, got, record.PayloadDigest)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read changeset payload %q: %w", target, err)
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create changeset payload directory for %q: %w", target, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".tmp-changeset-*")
	if err != nil {
		return fmt.Errorf("create temporary changeset payload near %q: %w", target, err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary changeset payload %q: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temporary changeset payload %q: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary changeset payload %q: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return fmt.Errorf("publish changeset payload %q: %w", target, err)
	}
	cleanup = false
	return nil
}

func (s *Store) readPayload(record Record) (*repositorychangeset.Changeset, error) {
	payloadPath, err := s.payloadPath(record)
	if err != nil {
		return nil, err
	}
	payload, err := os.ReadFile(payloadPath)
	if err != nil {
		return nil, fmt.Errorf("read changeset payload %q: %w", record.ChangesetID, err)
	}
	if got := sha256Bytes(payload); got != strings.TrimSpace(record.PayloadDigest) {
		return nil, fmt.Errorf("changeset payload %q digest mismatch: got %s want %s", record.ChangesetID, got, record.PayloadDigest)
	}

	var protoChangeset cleanroomv1.RepositoryChangeset
	if err := proto.Unmarshal(payload, &protoChangeset); err != nil {
		return nil, fmt.Errorf("decode changeset payload %q: %w", record.ChangesetID, err)
	}
	changeset := repositorychangeset.FromProto(&protoChangeset)
	if err := changeset.ValidateContent(); err != nil {
		return nil, fmt.Errorf("validate changeset payload %q: %w", record.ChangesetID, err)
	}
	if strings.TrimSpace(changeset.Digest) != strings.TrimSpace(record.ChangesetDigest) {
		return nil, fmt.Errorf("changeset payload %q digest %q does not match metadata %q", record.ChangesetID, changeset.Digest, record.ChangesetDigest)
	}
	if strings.TrimSpace(changeset.TreeDigest) != strings.TrimSpace(record.FinalTreeDigest) {
		return nil, fmt.Errorf("changeset payload %q tree digest %q does not match metadata %q", record.ChangesetID, changeset.TreeDigest, record.FinalTreeDigest)
	}
	return changeset, nil
}

func (s *Store) payloadPath(record Record) (string, error) {
	transportRef := strings.TrimSpace(record.TransportRef)
	if transportRef == "" {
		return "", fmt.Errorf("changeset %q missing transport ref", record.ChangesetID)
	}
	rel := filepath.Clean(filepath.FromSlash(transportRef))
	if rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("changeset %q has invalid transport ref %q", record.ChangesetID, record.TransportRef)
	}
	return filepath.Join(s.payloadDir, rel), nil
}

func (s *Store) open(ctx context.Context) (*sql.DB, error) {
	if err := s.initDB(ctx); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", s.metadataDBPath)
	if err != nil {
		return nil, fmt.Errorf("open changeset metadata database %q: %w", s.metadataDBPath, err)
	}
	return db, nil
}

func (s *Store) initDB(ctx context.Context) error {
	db, err := sql.Open("sqlite", s.metadataDBPath)
	if err != nil {
		return fmt.Errorf("open changeset metadata database %q: %w", s.metadataDBPath, err)
	}
	defer db.Close()

	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS changesets (
			changeset_id TEXT PRIMARY KEY,
			canonical_remote_url TEXT NOT NULL,
			base_commit_sha TEXT NOT NULL,
			submodule_mode TEXT NOT NULL,
			changeset_digest TEXT NOT NULL,
			final_tree_digest TEXT NOT NULL,
			transport_format TEXT NOT NULL,
			transport_ref TEXT NOT NULL,
			payload_digest TEXT NOT NULL,
			created_at_unix_nano INTEGER NOT NULL,
			last_used_at_unix_nano INTEGER NOT NULL
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_changesets_identity ON changesets(canonical_remote_url, base_commit_sha, submodule_mode, changeset_digest);
		CREATE INDEX IF NOT EXISTS idx_changesets_created_at ON changesets(created_at_unix_nano);
		CREATE INDEX IF NOT EXISTS idx_changesets_last_used_at ON changesets(last_used_at_unix_nano);
	`)
	if err != nil {
		return fmt.Errorf("initialise changeset metadata schema: %w", err)
	}
	return nil
}

type recordScanner interface {
	Scan(dest ...any) error
}

func scanRecord(row recordScanner) (Record, error) {
	var (
		record         Record
		createdAtNano  int64
		lastUsedAtNano int64
	)
	if err := row.Scan(
		&record.ChangesetID,
		&record.CanonicalRemoteURL,
		&record.BaseCommitSHA,
		&record.SubmoduleMode,
		&record.ChangesetDigest,
		&record.FinalTreeDigest,
		&record.TransportFormat,
		&record.TransportRef,
		&record.PayloadDigest,
		&createdAtNano,
		&lastUsedAtNano,
	); err != nil {
		return Record{}, err
	}
	record.CreatedAt = time.Unix(0, createdAtNano).UTC()
	record.LastUsedAt = time.Unix(0, lastUsedAtNano).UTC()
	return record, nil
}

func payloadRef(digest string) (string, error) {
	hexDigest, ok := strings.CutPrefix(strings.TrimSpace(digest), "sha256:")
	if !ok {
		return "", fmt.Errorf("changeset payload digest %q is not a sha256 digest", digest)
	}
	if len(hexDigest) != sha256.Size*2 {
		return "", fmt.Errorf("changeset payload digest %q has invalid length", digest)
	}
	if _, err := hex.DecodeString(hexDigest); err != nil {
		return "", fmt.Errorf("changeset payload digest %q is invalid: %w", digest, err)
	}
	return "sha256/" + hexDigest[:2] + "/" + hexDigest + ".pb", nil
}

func sha256Bytes(payload []byte) string {
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func submoduleMode(repository *repositorycheckout.Checkout) string {
	if repository != nil && repository.Submodules {
		return "enabled"
	}
	return "disabled"
}

func defaultPaths() (metadataDBPath, payloadDir string, err error) {
	base, err := paths.StateBaseDir()
	if err != nil {
		return "", "", err
	}
	return filepath.Join(base, "changesets", "metadata.db"), filepath.Join(base, "changesets", "payloads"), nil
}
