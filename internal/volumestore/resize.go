package volumestore

import (
	"context"

	"github.com/buildkite/cleanroom/internal/ext4image"
)

type writableVolumeSizer interface {
	EnsureWritableVolumeMinimumSize(context.Context, WritableVolume, int64) error
}

var (
	ext4imageEnsureMinimumSize = ext4image.EnsureMinimumSize
	ext4imagePathSizeBytes     = ext4image.PathSizeBytes
	ext4imageAlignBytes        = ext4image.AlignBytes
)

func EnsureWritableVolumeMinimumSize(ctx context.Context, driver Driver, volume WritableVolume, minimumBytes int64) error {
	if sizer, ok := driver.(writableVolumeSizer); ok {
		return sizer.EnsureWritableVolumeMinimumSize(ctx, volume, minimumBytes)
	}
	return ext4imageEnsureMinimumSize(ctx, volume.AttachmentPath, minimumBytes)
}
