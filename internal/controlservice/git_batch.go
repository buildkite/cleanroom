package controlservice

import (
	"context"

	"github.com/buildkite/cleanroom/internal/gitbatch"
)

func gitTreeEntriesForFiles(ctx context.Context, repoDir, commitSHA string, files []string) (map[string]gitTreeEntry, error) {
	result, err := gitbatch.TreeEntriesForFiles(ctx, repoDir, commitSHA, files)
	if err != nil {
		return nil, err
	}
	out := make(map[string]gitTreeEntry, len(result))
	for k, v := range result {
		out[k] = gitTreeEntry{Mode: v.Mode, Type: v.Type}
	}
	return out, nil
}

func gitFileDigestsAtCommit(ctx context.Context, repoDir, commitSHA string, files []string) (map[string]string, error) {
	return gitbatch.FileDigestsAtCommit(ctx, repoDir, commitSHA, files)
}

func gitTreeEntriesForFilesInWorktree(ctx context.Context, repoDir string, files []string) (map[string]gitTreeEntry, error) {
	result, err := gitbatch.TreeEntriesForFilesInWorktree(ctx, repoDir, files)
	if err != nil {
		return nil, err
	}
	out := make(map[string]gitTreeEntry, len(result))
	for k, v := range result {
		out[k] = gitTreeEntry{Mode: v.Mode, Type: v.Type}
	}
	return out, nil
}

func gitFileDigestsInWorktree(ctx context.Context, repoDir string, files []string) (map[string]string, error) {
	return gitbatch.FileDigestsInWorktree(ctx, repoDir, files)
}
