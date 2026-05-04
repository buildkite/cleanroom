package overlaycapture

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

type EntryKind string

const (
	EntryKindWrite     EntryKind = "write"
	EntryKindDelete    EntryKind = "delete"
	EntryKindOpaqueDir EntryKind = "opaque_dir"
)

type Entry struct {
	Path string
	Kind EntryKind
	Mode fs.FileMode
}

type Options struct {
	BaselinePaths       []string
	DeclaredFileOutputs []string
	IgnoredPrefixes     []string
}

type Result struct {
	Entries       []Entry
	EscapedWrites []Entry
}

func Scan(upperDir string, opts Options) (Result, error) {
	upperDir = strings.TrimSpace(upperDir)
	if upperDir == "" {
		return Result{}, errors.New("missing overlay upperdir")
	}
	upperDir = filepath.Clean(upperDir)
	if !filepath.IsAbs(upperDir) {
		return Result{}, fmt.Errorf("overlay upperdir %q is not absolute", upperDir)
	}
	info, err := os.Stat(upperDir)
	if err != nil {
		return Result{}, fmt.Errorf("stat overlay upperdir %s: %w", upperDir, err)
	}
	if !info.IsDir() {
		return Result{}, fmt.Errorf("overlay upperdir %s is not a directory", upperDir)
	}

	filter, err := newFilter(opts)
	if err != nil {
		return Result{}, err
	}

	var entries []Entry
	if err := filepath.WalkDir(upperDir, func(current string, dirent os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("walk overlay upperdir path %s: %w", current, walkErr)
		}
		if current == upperDir {
			return nil
		}

		rel, err := filepath.Rel(upperDir, current)
		if err != nil {
			return fmt.Errorf("rel overlay upperdir path %s: %w", current, err)
		}
		if rel == "." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
			return fmt.Errorf("overlay upperdir path %s escapes %s", current, upperDir)
		}

		guestPath := upperdirRelGuestPath(rel)
		if filter.ignored(guestPath) && dirent.IsDir() {
			return filepath.SkipDir
		}

		info, err := dirent.Info()
		if err != nil {
			return fmt.Errorf("stat overlay upperdir path %s: %w", current, err)
		}
		entry := Entry{Path: guestPath, Kind: EntryKindWrite, Mode: info.Mode()}
		if overlayEntryIsWhiteout(current, info) {
			entry.Kind = EntryKindDelete
		} else if dirent.IsDir() && overlayDirIsOpaque(current) {
			entry.Kind = EntryKindOpaqueDir
		}
		entries = append(entries, entry)
		return nil
	}); err != nil {
		return Result{}, err
	}

	sortEntries(entries)
	escaped := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		if filter.allowed(entry) {
			continue
		}
		escaped = append(escaped, entry)
	}
	return Result{Entries: entries, EscapedWrites: escaped}, nil
}

type filter struct {
	baseline        map[string]struct{}
	fileOutputs     map[string]struct{}
	fileAncestors   map[string]struct{}
	ignoredPrefixes []string
}

func newFilter(opts Options) (filter, error) {
	out := filter{
		baseline:      make(map[string]struct{}, len(opts.BaselinePaths)),
		fileOutputs:   make(map[string]struct{}, len(opts.DeclaredFileOutputs)),
		fileAncestors: make(map[string]struct{}),
	}

	for _, value := range opts.BaselinePaths {
		cleaned, err := cleanGuestPath("baseline path", value)
		if err != nil {
			return filter{}, err
		}
		out.baseline[cleaned] = struct{}{}
	}
	for _, value := range opts.DeclaredFileOutputs {
		cleaned, err := cleanGuestPath("declared file output", value)
		if err != nil {
			return filter{}, err
		}
		if cleaned == "/" {
			return filter{}, errors.New("declared file output cannot be /")
		}
		out.fileOutputs[cleaned] = struct{}{}
		for dir := path.Dir(cleaned); dir != "." && dir != "/"; dir = path.Dir(dir) {
			out.fileAncestors[dir] = struct{}{}
		}
	}
	for _, value := range opts.IgnoredPrefixes {
		cleaned, err := cleanGuestPath("ignored prefix", value)
		if err != nil {
			return filter{}, err
		}
		if cleaned == "/" {
			return filter{}, errors.New("ignored prefix cannot be /")
		}
		out.ignoredPrefixes = append(out.ignoredPrefixes, cleaned)
	}
	sort.Strings(out.ignoredPrefixes)
	return out, nil
}

func (f filter) allowed(entry Entry) bool {
	if f.ignored(entry.Path) {
		return true
	}
	if _, ok := f.baseline[entry.Path]; ok && entry.Kind == EntryKindWrite && entry.Mode.IsDir() {
		return true
	}
	if _, ok := f.fileOutputs[entry.Path]; ok {
		return true
	}
	if _, ok := f.fileAncestors[entry.Path]; ok && entry.Kind == EntryKindWrite && entry.Mode.IsDir() {
		return true
	}
	return false
}

func (f filter) ignored(guestPath string) bool {
	for _, prefix := range f.ignoredPrefixes {
		if guestPath == prefix || strings.HasPrefix(guestPath, prefix+"/") {
			return true
		}
	}
	return false
}

func cleanGuestPath(name, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("missing %s", name)
	}
	if !strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("%s %q is not absolute", name, value)
	}
	return path.Clean(value), nil
}

func upperdirRelGuestPath(rel string) string {
	rel = filepath.ToSlash(filepath.Clean(rel))
	if rel == "." {
		return "/"
	}
	return path.Clean("/" + rel)
}

func sortEntries(entries []Entry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Path != entries[j].Path {
			return entries[i].Path < entries[j].Path
		}
		return entries[i].Kind < entries[j].Kind
	})
}
