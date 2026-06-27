package sporevm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
)

var ErrUnavailable = errors.New("cleanroom was built without libspore support")

type Client interface {
	io.Closer
	CreateNamed(context.Context, CreateNamedOptions) (JSONResult, error)
	ExecNamed(context.Context, ExecNamedOptions) (JSONResult, error)
	ResumeNamed(context.Context, ResumeNamedOptions) (JSONResult, error)
	SnapshotNamed(context.Context, SnapshotNamedOptions) (JSONResult, error)
	RemoveNamed(context.Context, RemoveNamedOptions) (JSONResult, error)
}

type JSONResult struct {
	RawJSON json.RawMessage
}

type CreateNamedOptions struct {
	Name        string
	Backend     string
	ImageRef    string
	MemoryBytes uint64
	VCPUs       uint32
	TimeoutMS   uint64
}

type ExecNamedOptions struct {
	Name string
	Argv []string
}

type ResumeNamedOptions struct {
	SporeDir string
	Name     string
}

type SnapshotNamedOptions struct {
	Name     string
	OutDir   string
	Continue bool
}

type RemoveNamedOptions struct {
	Name string
}
