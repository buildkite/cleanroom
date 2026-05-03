package controlservice

import "time"

type serviceClock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}

type realServiceClock struct{}

func (realServiceClock) Now() time.Time {
	return time.Now().UTC()
}

func (realServiceClock) After(d time.Duration) <-chan time.Time {
	return time.After(d)
}

type serviceIDSource interface {
	NewSandboxID() string
	NewExecutionID() string
	NewInteractiveSessionID() string
	NewSessionToken() (string, error)
}

type defaultServiceIDSource struct{}

func (defaultServiceIDSource) NewSandboxID() string            { return newSandboxID() }
func (defaultServiceIDSource) NewExecutionID() string          { return newExecutionID() }
func (defaultServiceIDSource) NewInteractiveSessionID() string { return newInteractiveSessionID() }
func (defaultServiceIDSource) NewSessionToken() (string, error) {
	return newSessionToken()
}

type retentionPolicy struct {
	maxRetainedStoppedSandboxes     int
	maxRetainedFinishedExecutions   int
	maxRetainedSandboxEvents        int
	maxRetainedExecutionEvents      int
	maxRetainedExecutionOutputBytes int
	retainedStateMaxAge             time.Duration
}

type serviceTimeouts struct {
	attachStdinRegistrationWait  time.Duration
	attachResizeRegistrationWait time.Duration
	attachPollInterval           time.Duration
	interactiveSessionTokenTTL   time.Duration
	bootstrapCleanupTimeout      time.Duration
	storageCleanupTimeout        time.Duration
}

type serviceRuntime struct {
	clock                           serviceClock
	ids                             serviceIDSource
	retention                       *retentionPolicy
	timeouts                        *serviceTimeouts
	defaultDownloadMaxBytes         int64
	terminatedSandboxStorageCleanup func(string)
	zfsImportDatasetStorageCleanup  func()
	storageCleanupQueueSize         int
	cachePeerExportTokenTTL         time.Duration
}

var defaultRetentionPolicy = retentionPolicy{
	maxRetainedStoppedSandboxes:     256,
	maxRetainedFinishedExecutions:   2048,
	maxRetainedSandboxEvents:        256,
	maxRetainedExecutionEvents:      2048,
	maxRetainedExecutionOutputBytes: 1 * 1024 * 1024,
	retainedStateMaxAge:             24 * time.Hour,
}

var defaultServiceTimeouts = serviceTimeouts{
	attachStdinRegistrationWait:  2 * time.Second,
	attachResizeRegistrationWait: 250 * time.Millisecond,
	attachPollInterval:           10 * time.Millisecond,
	interactiveSessionTokenTTL:   30 * time.Second,
	bootstrapCleanupTimeout:      10 * time.Second,
	storageCleanupTimeout:        10 * time.Second,
}

const defaultDownloadMaxBytes int64 = 10 * 1024 * 1024

const defaultStorageCleanupQueueSize = 128
const defaultCachePeerExportTokenTTL = 5 * time.Minute

func (s *Service) clock() serviceClock {
	if s.runtime.clock != nil {
		return s.runtime.clock
	}
	return realServiceClock{}
}

func (s *Service) ids() serviceIDSource {
	if s.runtime.ids != nil {
		return s.runtime.ids
	}
	return defaultServiceIDSource{}
}

func (s *Service) retention() retentionPolicy {
	if s.runtime.retention != nil {
		return *s.runtime.retention
	}
	return defaultRetentionPolicy
}

func (s *Service) timeouts() serviceTimeouts {
	if s.runtime.timeouts != nil {
		return *s.runtime.timeouts
	}
	return defaultServiceTimeouts
}

func (s *Service) downloadMaxBytesDefault() int64 {
	if s.runtime.defaultDownloadMaxBytes > 0 {
		return s.runtime.defaultDownloadMaxBytes
	}
	return defaultDownloadMaxBytes
}

func (s *Service) storageCleanupQueueSize() int {
	if s.runtime.storageCleanupQueueSize > 0 {
		return s.runtime.storageCleanupQueueSize
	}
	return defaultStorageCleanupQueueSize
}

func (s *Service) cachePeerExportTokenTTL() time.Duration {
	if s.runtime.cachePeerExportTokenTTL > 0 {
		return s.runtime.cachePeerExportTokenTTL
	}
	return defaultCachePeerExportTokenTTL
}
