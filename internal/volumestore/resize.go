package volumestore

import (
	"context"

	"github.com/buildkite/cleanroom/internal/ext4image"
)

func EnsureWritableVolumeMinimumSize(ctx context.Context, volume WritableVolume, minimumBytes int64) error {
	return ext4image.EnsureMinimumSize(ctx, volume.AttachmentPath, minimumBytes)
}
