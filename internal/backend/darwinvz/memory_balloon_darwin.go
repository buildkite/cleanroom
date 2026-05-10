//go:build darwin

package darwinvz

import (
	"context"
	"fmt"
	"time"
)

var darwinVZMemoryBalloonGrowSettle = time.Second

func darwinVZInitialMemoryBalloonTargetMiB(memoryMiB int64) int64 {
	if memoryMiB > darwinVZAdaptiveMemoryStartMiB {
		return darwinVZAdaptiveMemoryStartMiB
	}
	return 0
}

func (a *Adapter) setDarwinVZMemoryBalloonTarget(ctx context.Context, helper *helperSession, vmID string, targetMiB int64) error {
	if targetMiB <= darwinVZAdaptiveMemoryStartMiB {
		return nil
	}
	helperRequest := a.helperRequestFn
	if helperRequest == nil {
		helperRequest = func(ctx context.Context, helper *helperSession, req helperControlRequest) (helperControlResponse, error) {
			return helper.request(ctx, req)
		}
	}
	if _, err := helperRequest(ctx, helper, helperControlRequest{
		Op:                     "SetMemoryBalloonTarget",
		VMID:                   vmID,
		MemoryBalloonTargetMiB: targetMiB,
	}); err != nil {
		return fmt.Errorf("set darwin-vz memory balloon target to %d MiB: %w", targetMiB, err)
	}
	return nil
}

func (a *Adapter) growDarwinVZMemoryBalloonTarget(ctx context.Context, helper *helperSession, vmID string, targetMiB int64) error {
	if targetMiB <= darwinVZAdaptiveMemoryStartMiB {
		return nil
	}
	if err := a.setDarwinVZMemoryBalloonTarget(ctx, helper, vmID, targetMiB); err != nil {
		return err
	}
	if darwinVZMemoryBalloonGrowSettle <= 0 {
		return nil
	}
	timer := time.NewTimer(darwinVZMemoryBalloonGrowSettle)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for darwin-vz memory balloon target settle: %w", ctx.Err())
	}
}

func (s *sandboxInstance) growDarwinVZMemoryBalloonTarget(ctx context.Context, adapter *Adapter) error {
	targetMiB := s.FirecrackerConfig.MemoryMiB
	if targetMiB <= darwinVZAdaptiveMemoryStartMiB {
		return nil
	}

	s.memoryBalloonMu.Lock()
	defer s.memoryBalloonMu.Unlock()
	if s.memoryBalloonTargetMiB >= targetMiB {
		return nil
	}
	if err := adapter.growDarwinVZMemoryBalloonTarget(ctx, s.Helper, s.VMID, targetMiB); err != nil {
		return err
	}
	s.memoryBalloonTargetMiB = targetMiB
	return nil
}
