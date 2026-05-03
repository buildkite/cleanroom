package firecracker

import (
	"context"
	"io"
	"strings"

	"github.com/buildkite/cleanroom/internal/backend"
	"github.com/buildkite/cleanroom/internal/volumestore"
)

type ZFSImportDatasetStore struct {
	cfg backend.FirecrackerConfig
}

func NewZFSImportDatasetStore(cfg backend.FirecrackerConfig) *ZFSImportDatasetStore {
	if !firecrackerZFSDriverConfigured(cfg) {
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
	return openFirecrackerZFSDriver(s.cfg)
}

type ZFSIncrementalTransferDriver struct {
	cfg backend.FirecrackerConfig
}

func NewZFSIncrementalTransferDriver(cfg backend.FirecrackerConfig) *ZFSIncrementalTransferDriver {
	if !firecrackerZFSDriverConfigured(cfg) {
		return nil
	}
	return &ZFSIncrementalTransferDriver{cfg: cfg}
}

func (d *ZFSIncrementalTransferDriver) DescribeSnapshot(ctx context.Context, req volumestore.DescribeSnapshotRequest) (volumestore.SnapshotDescription, error) {
	driver, err := d.openDriver()
	if err != nil {
		return volumestore.SnapshotDescription{}, err
	}
	return driver.DescribeSnapshot(ctx, req)
}

func (d *ZFSIncrementalTransferDriver) PlanIncrementalSnapshotExport(ctx context.Context, req volumestore.IncrementalSnapshotExportRequest) (volumestore.IncrementalSnapshotExportPlan, error) {
	driver, err := d.openDriver()
	if err != nil {
		return volumestore.IncrementalSnapshotExportPlan{}, err
	}
	return driver.PlanIncrementalSnapshotExport(ctx, req)
}

func (d *ZFSIncrementalTransferDriver) ExportIncrementalSnapshot(ctx context.Context, plan volumestore.IncrementalSnapshotExportPlan, dst io.Writer) error {
	driver, err := d.openDriver()
	if err != nil {
		return err
	}
	return driver.ExportIncrementalSnapshot(ctx, plan, dst)
}

func (d *ZFSIncrementalTransferDriver) ImportIncrementalSnapshot(ctx context.Context, req volumestore.IncrementalSnapshotImportRequest, src io.Reader) (volumestore.Snapshot, error) {
	driver, err := d.openDriver()
	if err != nil {
		return volumestore.Snapshot{}, err
	}
	return driver.ImportIncrementalSnapshot(ctx, req, src)
}

func (d *ZFSIncrementalTransferDriver) openDriver() (*volumestore.ZFSDriver, error) {
	return openFirecrackerZFSDriver(d.cfg)
}

func openFirecrackerZFSDriver(cfg backend.FirecrackerConfig) (*volumestore.ZFSDriver, error) {
	return volumestore.NewZFSDriver(volumestore.ZFSDriverOptions{
		DatasetRoot: strings.TrimSpace(cfg.Snapshots.ZFSDataset),
		Runner:      hostRuntimeVolumeCommandRunner{runner: newPrivilegedCommandRunner(cfg)},
	})
}

func firecrackerZFSDriverConfigured(cfg backend.FirecrackerConfig) bool {
	return strings.EqualFold(strings.TrimSpace(cfg.Snapshots.Driver), "zfs") && strings.TrimSpace(cfg.Snapshots.ZFSDataset) != ""
}
