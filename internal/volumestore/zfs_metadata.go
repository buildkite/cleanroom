package volumestore

import (
	"encoding/json"
	"fmt"
	"strings"
)

const zfsDriverMetadataVersion = 1
const zfsReceiveStreamVersion = "zfs-send-v1"

// ZFSDriverMetadata is the stable cache metadata shape used to compare ZFS
// lineage across cleanroom peers.
type ZFSDriverMetadata struct {
	Version              int    `json:"version"`
	Dataset              string `json:"zfs_dataset,omitempty"`
	Snapshot             string `json:"zfs_snapshot,omitempty"`
	SnapshotGUID         string `json:"zfs_snapshot_guid,omitempty"`
	ParentSnapshotGUID   string `json:"zfs_parent_snapshot_guid,omitempty"`
	ReceiveStreamVersion string `json:"zfs_receive_stream_version,omitempty"`
}

// ZFSDriverMetadataFromDescription converts a driver-neutral description into
// the JSON shape persisted in cache metadata.
func ZFSDriverMetadataFromDescription(desc SnapshotDescription) ZFSDriverMetadata {
	dataset, snapshot, _ := strings.Cut(strings.TrimSpace(desc.SnapshotRef), "@")
	return ZFSDriverMetadata{
		Version:              zfsDriverMetadataVersion,
		Dataset:              strings.TrimSpace(dataset),
		Snapshot:             strings.TrimSpace(snapshot),
		SnapshotGUID:         strings.TrimSpace(desc.SnapshotGUID),
		ParentSnapshotGUID:   strings.TrimSpace(desc.ParentSnapshotGUID),
		ReceiveStreamVersion: zfsReceiveStreamVersion,
	}
}

// EncodeZFSDriverMetadata returns the stable JSON representation persisted in
// cache records and exchanged with peers.
func EncodeZFSDriverMetadata(metadata ZFSDriverMetadata) (string, error) {
	if metadata.Version == 0 {
		metadata.Version = zfsDriverMetadataVersion
	}
	out, err := json.Marshal(metadata)
	if err != nil {
		return "", fmt.Errorf("encode zfs driver metadata: %w", err)
	}
	return string(out), nil
}

// DecodeZFSDriverMetadata parses the stable JSON representation persisted in
// cache records and exchanged with peers.
func DecodeZFSDriverMetadata(raw string) (ZFSDriverMetadata, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ZFSDriverMetadata{}, fmt.Errorf("decode zfs driver metadata: empty metadata")
	}
	var metadata ZFSDriverMetadata
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		return ZFSDriverMetadata{}, fmt.Errorf("decode zfs driver metadata: %w", err)
	}
	return metadata, nil
}
