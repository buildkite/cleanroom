package backend

import "context"

const (
	WarmupStatusConfigured = "configured"
	WarmupStatusCached     = "cached"
	WarmupStatusFetched    = "fetched"
	WarmupStatusPrepared   = "prepared"
	WarmupStatusReady      = "ready"
)

// WarmupAdapter prepares backend-managed assets that are normally created
// lazily on the first sandbox launch.
type WarmupAdapter interface {
	Adapter
	Warmup(context.Context, WarmupRequest) (*WarmupResult, error)
}

type WarmupRequest struct {
	FirecrackerConfig FirecrackerConfig
	ImageRef          string
}

type WarmupResult struct {
	Backend            string `json:"backend"`
	KernelPath         string `json:"kernel_path,omitempty"`
	KernelStatus       string `json:"kernel_status,omitempty"`
	RootFSPath         string `json:"rootfs_path,omitempty"`
	RootFSStatus       string `json:"rootfs_status,omitempty"`
	BaseRootFSRef      string `json:"base_rootfs_ref,omitempty"`
	BaseRootFSStatus   string `json:"base_rootfs_status,omitempty"`
	ImageRef           string `json:"image_ref,omitempty"`
	ImageDigest        string `json:"image_digest,omitempty"`
	MinimumRootFSBytes int64  `json:"minimum_rootfs_bytes,omitempty"`
}
