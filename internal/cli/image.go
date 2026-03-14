package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/buildkite/cleanroom/internal/imagemgr"
)

type ImageCommand struct {
	Pull    ImagePullCommand    `cmd:"" help:"Pull and cache a digest-pinned OCI image"`
	List    ImageListCommand    `name:"ls" aliases:"list" cmd:"" help:"List cached images"`
	Remove  ImageRemoveCommand  `name:"rm" aliases:"remove" cmd:"" help:"Remove a cached image by ref or digest"`
	Import  ImageImportCommand  `cmd:"" help:"Import a rootfs tar stream into the cache for a digest-pinned ref"`
	BumpRef ImageBumpRefCommand `name:"bump-ref" aliases:"set-ref" cmd:"" help:"Resolve an image tag to digest and update sandbox.image.ref in cleanroom policy"`
}

type ImagePullCommand struct {
	Ref string `arg:"" required:"" help:"Digest-pinned OCI reference (repo/image@sha256:...)"`
}

type ImageListCommand struct {
	JSON bool `help:"Print image records as JSON"`
}

type ImageRemoveCommand struct {
	Selector string `arg:"" required:"" help:"Image selector (ref, sha256:<digest>, or digest hex)"`
}

type ImageImportCommand struct {
	Ref     string `arg:"" required:"" help:"Digest-pinned OCI reference for this import"`
	TarPath string `arg:"" optional:"" help:"Tar/tar.gz path, or '-' for stdin (default: '-')"`
}

type imageManager interface {
	Pull(context.Context, string) (imagemgr.EnsureResult, error)
	List(context.Context) ([]imagemgr.Record, error)
	Remove(context.Context, string) ([]imagemgr.Record, error)
	Import(context.Context, string, string, io.Reader) (imagemgr.Record, error)
}

var newImageManager = func() (imageManager, error) {
	return imagemgr.New(imagemgr.Options{})
}

func (c *ImagePullCommand) Run(ctx *runtimeContext) error {
	mgr, err := newImageManager()
	if err != nil {
		return err
	}
	result, err := mgr.Pull(context.Background(), c.Ref)
	if err != nil {
		return err
	}

	status := "pulled"
	if result.CacheHit {
		status = "cached"
	}
	_, err = fmt.Fprintf(
		ctx.Stdout,
		"%s image\nref=%s\ndigest=%s\nrootfs=%s\nsize_bytes=%d\n",
		status,
		result.Record.Ref,
		result.Record.Digest,
		result.Record.RootFSPath,
		result.Record.SizeBytes,
	)
	return err
}

func (c *ImageListCommand) Run(ctx *runtimeContext) error {
	mgr, err := newImageManager()
	if err != nil {
		return err
	}
	items, err := mgr.List(context.Background())
	if err != nil {
		return err
	}

	if c.JSON {
		enc := json.NewEncoder(ctx.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(items)
	}

	if len(items) == 0 {
		_, err := fmt.Fprintln(ctx.Stdout, "no cached images")
		return err
	}

	tw := tabwriter.NewWriter(ctx.Stdout, 0, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "DIGEST\tREF\tSIZE\tLAST_USED\tROOTFS"); err != nil {
		return err
	}
	for _, item := range items {
		if _, err := fmt.Fprintf(
			tw,
			"%s\t%s\t%d\t%s\t%s\n",
			item.Digest,
			item.Ref,
			item.SizeBytes,
			item.LastUsedAt.Format(time.RFC3339),
			item.RootFSPath,
		); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func (c *ImageRemoveCommand) Run(ctx *runtimeContext) error {
	mgr, err := newImageManager()
	if err != nil {
		return err
	}
	removed, err := mgr.Remove(context.Background(), c.Selector)
	if err != nil {
		return err
	}
	if len(removed) == 0 {
		_, err := fmt.Fprintf(ctx.Stdout, "no cached images match %q\n", c.Selector)
		return err
	}
	for _, item := range removed {
		if _, err := fmt.Fprintf(ctx.Stdout, "removed %s (%s)\n", item.Digest, item.Ref); err != nil {
			return err
		}
	}
	return nil
}

func (c *ImageImportCommand) Run(ctx *runtimeContext) error {
	mgr, err := newImageManager()
	if err != nil {
		return err
	}
	record, err := mgr.Import(context.Background(), c.Ref, c.TarPath, os.Stdin)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(
		ctx.Stdout,
		"imported image\nref=%s\ndigest=%s\nrootfs=%s\nsize_bytes=%d\n",
		record.Ref,
		record.Digest,
		record.RootFSPath,
		record.SizeBytes,
	)
	return err
}
