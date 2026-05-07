package controlservice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os/exec"
	"path"
	"strconv"
	"strings"
)

func gitTreeEntriesForFiles(ctx context.Context, repoDir, commitSHA string, files []string) (map[string]gitTreeEntry, error) {
	args := make([]string, 0, 5+len(files))
	args = append(args, "-C", repoDir, "ls-tree", "-z", commitSHA, "--")
	for _, f := range files {
		args = append(args, ":(literal)"+f)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("%s", message)
	}

	result := make(map[string]gitTreeEntry, len(files))
	for _, raw := range bytes.Split(output, []byte{0}) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		metadata, rawPath, ok := bytes.Cut(raw, []byte{'\t'})
		if !ok {
			return nil, fmt.Errorf("parse git tree entry %q", string(raw))
		}
		normalizedPath := path.Clean(strings.ReplaceAll(string(rawPath), "\\", "/"))
		fields := strings.Fields(string(metadata))
		if len(fields) < 3 {
			return nil, fmt.Errorf("parse git tree entry %q", string(raw))
		}
		result[normalizedPath] = gitTreeEntry{Mode: fields[0], Type: fields[1]}
	}
	return result, nil
}

func gitFileDigestsAtCommit(ctx context.Context, repoDir, commitSHA string, files []string) (map[string]string, error) {
	if len(files) == 0 {
		return map[string]string{}, nil
	}

	cmd := exec.CommandContext(ctx, "git", "-C", repoDir, "cat-file", "--batch")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("create git cat-file stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("create git cat-file stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start git cat-file: %w", err)
	}

	type result struct {
		digests map[string]string
		err     error
	}

	writerDone := make(chan error, 1)
	readerDone := make(chan result, 1)

	go func() {
		var writeErr error
		for _, f := range files {
			spec := commitSHA + ":" + f + "\n"
			if _, err := io.WriteString(stdin, spec); err != nil {
				writeErr = fmt.Errorf("write git cat-file spec for %q: %w", f, err)
				break
			}
		}
		stdin.Close()
		writerDone <- writeErr
	}()

	go func() {
		sendErr := func(err error) {
			io.Copy(io.Discard, stdout)
			readerDone <- result{nil, err}
		}

		digests := make(map[string]string, len(files))
		buf := make([]byte, 32*1024)
		reader := &bufferedReader{r: stdout, buf: make([]byte, 0, 64*1024)}

		for _, f := range files {
			header, err := reader.readLine()
			if err != nil {
				sendErr(fmt.Errorf("read git cat-file header for %q: %w", f, err))
				return
			}
			headerStr := strings.TrimRight(string(header), "\n")
			if strings.HasSuffix(headerStr, " missing") {
				spec := commitSHA + ":" + f
				sendErr(fmt.Errorf("git cat-file reports %q is missing", spec))
				return
			}
			fields := strings.Fields(headerStr)
			if len(fields) < 3 {
				sendErr(fmt.Errorf("parse git cat-file header %q", headerStr))
				return
			}
			size, err := strconv.ParseInt(fields[2], 10, 64)
			if err != nil {
				sendErr(fmt.Errorf("parse git cat-file size %q: %w", fields[2], err))
				return
			}

			h := sha256.New()
			remaining := size
			for remaining > 0 {
				toRead := int64(len(buf))
				if remaining < toRead {
					toRead = remaining
				}
				n, err := reader.read(buf[:toRead])
				if err != nil && err != io.EOF {
					sendErr(fmt.Errorf("read git cat-file content for %q: %w", f, err))
					return
				}
				h.Write(buf[:n])
				remaining -= int64(n)
				if err == io.EOF && remaining > 0 {
					sendErr(fmt.Errorf("unexpected EOF reading git cat-file content for %q", f))
					return
				}
			}

			if _, err := reader.readByte(); err != nil {
				sendErr(fmt.Errorf("read git cat-file trailing newline for %q: %w", f, err))
				return
			}

			digests[f] = "sha256:" + hex.EncodeToString(h.Sum(nil))
		}
		readerDone <- result{digests, nil}
	}()

	writeErr := <-writerDone
	readResult := <-readerDone

	if waitErr := cmd.Wait(); waitErr != nil && writeErr == nil && readResult.err == nil {
		return nil, fmt.Errorf("git cat-file: %w", waitErr)
	}
	if writeErr != nil {
		return nil, writeErr
	}
	if readResult.err != nil {
		return nil, readResult.err
	}
	return readResult.digests, nil
}

type bufferedReader struct {
	r   io.Reader
	buf []byte
	pos int
	end int
}

func (b *bufferedReader) fill() error {
	if b.pos < b.end {
		return nil
	}
	b.buf = b.buf[:cap(b.buf)]
	n, err := b.r.Read(b.buf)
	b.buf = b.buf[:n]
	b.pos = 0
	b.end = n
	if n == 0 && err != nil {
		return err
	}
	return nil
}

func (b *bufferedReader) readByte() (byte, error) {
	for b.pos >= b.end {
		if err := b.fill(); err != nil {
			return 0, err
		}
	}
	ch := b.buf[b.pos]
	b.pos++
	return ch, nil
}

func (b *bufferedReader) readLine() ([]byte, error) {
	var line []byte
	for {
		ch, err := b.readByte()
		if err != nil {
			return nil, err
		}
		line = append(line, ch)
		if ch == '\n' {
			return line, nil
		}
	}
}

func (b *bufferedReader) read(p []byte) (int, error) {
	if b.pos >= b.end {
		if err := b.fill(); err != nil {
			return 0, err
		}
	}
	n := copy(p, b.buf[b.pos:b.end])
	b.pos += n
	return n, nil
}
