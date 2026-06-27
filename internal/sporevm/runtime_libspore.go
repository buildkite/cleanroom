//go:build libspore

package sporevm

/*
#cgo pkg-config: libspore
#include <stdlib.h>
#include <spore.h>
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"unsafe"
)

const minABIVersion uint32 = 7

var ErrClosed = errors.New("spore client closed")

type resultCode int

const (
	resultSuccess resultCode = C.SPORE_SUCCESS
)

type CallError struct {
	Code    resultCode
	Message string
}

func (e *CallError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("libspore error: %d", e.Code)
	}
	return fmt.Sprintf("libspore error: %s", e.Message)
}

type buildInfo struct {
	Version    string
	ABIVersion uint32
}

type client struct {
	ctx C.SporeContext
}

func New() (Client, error) {
	var ctx C.SporeContext
	if result := resultCode(C.spore_context_new(&ctx)); result != resultSuccess {
		return nil, &CallError{Code: result}
	}
	c := &client{ctx: ctx}
	info, err := readBuildInfo()
	if err != nil {
		c.Close()
		return nil, err
	}
	if info.ABIVersion < minABIVersion {
		c.Close()
		return nil, fmt.Errorf("libspore C ABI version %d is older than required %d", info.ABIVersion, minABIVersion)
	}
	return c, nil
}

func (c *client) Close() error {
	if c == nil || c.ctx == nil {
		return nil
	}
	C.spore_context_free(c.ctx)
	c.ctx = nil
	return nil
}

func (c *client) CreateNamed(ctx context.Context, options CreateNamedOptions) (JSONResult, error) {
	if err := c.ready(ctx); err != nil {
		return JSONResult{}, err
	}

	name, freeName := cString(options.Name)
	defer freeName()
	backend, freeBackend := cString(options.Backend)
	defer freeBackend()
	imageRef, freeImageRef := cString(options.ImageRef)
	defer freeImageRef()

	var opts C.SporeCreateNamedOptions
	C.spore_create_named_options_init(&opts)
	opts.name = name
	opts.backend = backend
	opts.image_ref = imageRef
	opts.memory_bytes = C.uint64_t(options.MemoryBytes)
	opts.vcpus = C.uint32_t(options.VCPUs)
	opts.timeout_ms = C.uint64_t(options.TimeoutMS)

	var out C.SporeOwnedString
	if result := resultCode(C.spore_create_named_json(c.ctx, &opts, &out)); result != resultSuccess {
		return JSONResult{}, c.callError(result)
	}
	return c.jsonResult(out), nil
}

func (c *client) ExecNamed(ctx context.Context, options ExecNamedOptions) (JSONResult, error) {
	if err := c.ready(ctx); err != nil {
		return JSONResult{}, err
	}

	name, freeName := cString(options.Name)
	defer freeName()
	argv, freeArgv := cStringArray(options.Argv)
	defer freeArgv()

	var opts C.SporeExecNamedOptions
	C.spore_exec_named_options_init(&opts)
	opts.name = name
	if len(argv) > 0 {
		opts.argv = &argv[0]
		opts.argc = C.size_t(len(argv))
	}

	var out C.SporeOwnedString
	if result := resultCode(C.spore_exec_named_json(c.ctx, &opts, &out)); result != resultSuccess {
		return JSONResult{}, c.callError(result)
	}
	return c.jsonResult(out), nil
}

func (c *client) ResumeNamed(ctx context.Context, options ResumeNamedOptions) (JSONResult, error) {
	if err := c.ready(ctx); err != nil {
		return JSONResult{}, err
	}

	sporeDir, freeSporeDir := cString(options.SporeDir)
	defer freeSporeDir()
	name, freeName := cString(options.Name)
	defer freeName()

	var opts C.SporeResumeNamedOptions
	C.spore_resume_named_options_init(&opts)
	opts.spore_dir = sporeDir
	opts.name = name

	var out C.SporeOwnedString
	if result := resultCode(C.spore_resume_named_json(c.ctx, &opts, &out)); result != resultSuccess {
		return JSONResult{}, c.callError(result)
	}
	return c.jsonResult(out), nil
}

func (c *client) SnapshotNamed(ctx context.Context, options SnapshotNamedOptions) (JSONResult, error) {
	if err := c.ready(ctx); err != nil {
		return JSONResult{}, err
	}

	name, freeName := cString(options.Name)
	defer freeName()
	outDir, freeOutDir := cString(options.OutDir)
	defer freeOutDir()

	var opts C.SporeSnapshotNamedOptions
	C.spore_snapshot_named_options_init(&opts)
	opts.name = name
	opts.out_dir = outDir
	if options.Continue {
		opts.continue_after = 1
	}

	var out C.SporeOwnedString
	if result := resultCode(C.spore_snapshot_named_json(c.ctx, &opts, &out)); result != resultSuccess {
		return JSONResult{}, c.callError(result)
	}
	return c.jsonResult(out), nil
}

func (c *client) RemoveNamed(ctx context.Context, options RemoveNamedOptions) (JSONResult, error) {
	if err := c.ready(ctx); err != nil {
		return JSONResult{}, err
	}

	name, freeName := cString(options.Name)
	defer freeName()

	var opts C.SporeRemoveNamedOptions
	C.spore_remove_named_options_init(&opts)
	opts.name = name

	var out C.SporeOwnedString
	if result := resultCode(C.spore_remove_named_json(c.ctx, &opts, &out)); result != resultSuccess {
		return JSONResult{}, c.callError(result)
	}
	return c.jsonResult(out), nil
}

func (c *client) ready(ctx context.Context) error {
	if c == nil || c.ctx == nil {
		return ErrClosed
	}
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func (c *client) callError(code resultCode) error {
	return &CallError{Code: code, Message: goString(C.spore_context_last_error(c.ctx))}
}

func (c *client) jsonResult(out C.SporeOwnedString) JSONResult {
	defer C.spore_free_string(c.ctx, out)
	if out.ptr == nil || out.len == 0 {
		return JSONResult{}
	}
	raw := C.GoBytes(unsafe.Pointer(out.ptr), C.int(out.len))
	return JSONResult{RawJSON: append(json.RawMessage(nil), raw...)}
}

func readBuildInfo() (buildInfo, error) {
	var version C.SporeString
	if result := resultCode(C.spore_build_info(C.SPORE_BUILD_INFO_VERSION_STRING, unsafe.Pointer(&version))); result != resultSuccess {
		return buildInfo{}, &CallError{Code: result}
	}
	var abi C.uint32_t
	if result := resultCode(C.spore_build_info(C.SPORE_BUILD_INFO_ABI_VERSION, unsafe.Pointer(&abi))); result != resultSuccess {
		return buildInfo{}, &CallError{Code: result}
	}
	return buildInfo{
		Version:    goString(version),
		ABIVersion: uint32(abi),
	}, nil
}

func goString(s C.SporeString) string {
	if s.ptr == nil || s.len == 0 {
		return ""
	}
	return C.GoStringN(s.ptr, C.int(s.len))
}

func cString(s string) (C.SporeString, func()) {
	if s == "" {
		return C.SporeString{}, func() {}
	}
	ptr := C.CString(s)
	return C.SporeString{ptr: ptr, len: C.size_t(len(s))}, func() {
		C.free(unsafe.Pointer(ptr))
	}
}

func cStringArray(values []string) ([]C.SporeString, func()) {
	if len(values) == 0 {
		return nil, func() {}
	}
	out := make([]C.SporeString, 0, len(values))
	freeFns := make([]func(), 0, len(values))
	for _, value := range values {
		s, free := cString(value)
		out = append(out, s)
		freeFns = append(freeFns, free)
	}
	return out, func() {
		for _, free := range freeFns {
			free()
		}
	}
}
