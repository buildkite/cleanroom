package firecracker

import (
	"context"
	"strings"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/volumestore"
)

type ZFSImportDatasetStore struct {
	cfg backend.FirecrackerConfig
}

func NewZFSImportDatasetStore(cfg backend.FirecrackerConfig) *ZFSImportDatasetStore {
	if !strings.EqualFold(strings.TrimSpace(cfg.Snapshots.Driver), "zfs") {
		return nil
	}
	if strings.TrimSpace(cfg.Snapshots.ZFSDataset) == "" {
		return nil
	}
	return &ZFSImportDatasetStore{cfg: cfg}
}

func (s *ZFSImportDatasetStore) ListZFSImportDatasets(ctx context.Context) ([]string, error) {
	driver, err := s.openDriver()
	if err != nil {
		return nil, err
	}
	return driver.ListZFSImportDatasets(ctx)
}

func (s *ZFSImportDatasetStore) DestroyZFSImportDataset(ctx context.Context, dataset string) error {
	driver, err := s.openDriver()
	if err != nil {
		return err
	}
	return driver.DestroyZFSImportDataset(ctx, dataset)
}

func (s *ZFSImportDatasetStore) openDriver() (*volumestore.ZFSDriver, error) {
	return volumestore.NewZFSDriver(volumestore.ZFSDriverOptions{
		DatasetRoot: strings.TrimSpace(s.cfg.Snapshots.ZFSDataset),
		Runner:      hostRuntimeVolumeCommandRunner{runner: newPrivilegedCommandRunner(s.cfg)},
	})
}
