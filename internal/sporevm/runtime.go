package sporevm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
)

var (
	ErrUnavailable = errors.New("cleanroom was built without libspore support")
	ErrClosed      = errors.New("spore client closed")
)

type Client interface {
	io.Closer
	NetworkCapabilities(context.Context) (NetworkCapabilities, error)
	InspectSpore(context.Context, InspectSporeOptions) (SporeInspectResult, error)
	CreateNamed(context.Context, CreateNamedOptions) (JSONResult, error)
	ExecNamed(context.Context, ExecNamedOptions) (JSONResult, error)
	ResumeNamed(context.Context, ResumeNamedOptions) (JSONResult, error)
	SnapshotNamed(context.Context, SnapshotNamedOptions) (JSONResult, error)
	RemoveNamed(context.Context, RemoveNamedOptions) (JSONResult, error)
}

type JSONResult struct {
	RawJSON json.RawMessage
}

var contextEnvNames = []string{
	"HOME",
	"PATH",
	"XDG_CACHE_HOME",
	"TMPDIR",
	"XDG_RUNTIME_DIR",
	"SPOREVM_KERNEL_CACHE_DIR",
	"SPOREVM_ROOTFS_CACHE_DIR",
	"SPOREVM_BUNDLE_CACHE_DIR",
	"SPOREVM_RUNTIME_DIR",
}

func contextEnvFromProcess() map[string]string {
	env := make(map[string]string)
	for _, name := range contextEnvNames {
		if value := os.Getenv(name); value != "" {
			env[name] = value
		}
	}
	return env
}

func defaultSporeExecutable() string {
	path, err := exec.LookPath("spore")
	if err != nil {
		return "spore"
	}
	return path
}

type InspectSporeOptions struct {
	SporeDir string
}

type SporeInspectResult struct {
	Annotations map[string]string
}

type CreateNamedOptions struct {
	Name           string
	Backend        string
	ImageRef       string
	MemoryBytes    uint64
	VCPUs          uint32
	TimeoutMS      uint64
	NetworkEnabled bool
	NetworkRules   []NetworkRule
	BoundServices  []BoundUnixService
	Annotations    map[string]string
}

type NetworkCapabilities struct {
	Supported     bool
	ExactHostPort bool
	BoundServices bool
}

type NetworkRule struct {
	Host  string
	Ports []uint16
}

type BoundUnixService struct {
	Name      string
	GuestHost string
	GuestPort uint16
	UnixPath  string
}

type BoundUnixServiceBinding struct {
	Name     string
	UnixPath string
}

type ExecNamedOptions struct {
	Name string
	Argv []string
}

type ResumeNamedOptions struct {
	SporeDir             string
	Name                 string
	BoundServiceBindings []BoundUnixServiceBinding
}

type SnapshotNamedOptions struct {
	Name        string
	OutDir      string
	Continue    bool
	Annotations map[string]string
}

type RemoveNamedOptions struct {
	Name string
}
