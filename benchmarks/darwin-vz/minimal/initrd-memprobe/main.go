//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type options struct {
	touchMiB   uint64
	preTouch   time.Duration
	hold       time.Duration
	postFree   time.Duration
	pageStride int
}

type event struct {
	Phase        string            `json:"phase"`
	ElapsedMS    int64             `json:"elapsed_ms"`
	AllocatedMiB uint64            `json:"allocated_mib,omitempty"`
	Meminfo      map[string]uint64 `json:"meminfo,omitempty"`
	Error        string            `json:"error,omitempty"`
}

func main() {
	opts, err := parseOptions(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "memprobe: %v\n", err)
		os.Exit(2)
	}

	start := time.Now()
	emit(start, event{Phase: "before"})
	time.Sleep(opts.preTouch)

	size := int(opts.touchMiB * 1024 * 1024)
	buf, err := unix.Mmap(-1, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANON)
	if err != nil {
		emit(start, event{Phase: "error", AllocatedMiB: opts.touchMiB, Error: err.Error()})
		os.Exit(1)
	}

	for i := 0; i < len(buf); i += opts.pageStride {
		buf[i] = 1
	}
	if len(buf) > 0 {
		buf[len(buf)-1] = 1
	}
	runtime.KeepAlive(buf)
	emit(start, event{Phase: "touched", AllocatedMiB: opts.touchMiB})

	time.Sleep(opts.hold)

	madviseErr := unix.Madvise(buf, unix.MADV_DONTNEED)
	munmapErr := unix.Munmap(buf)
	if madviseErr != nil || munmapErr != nil {
		msg := joinErrors(madviseErr, munmapErr)
		emit(start, event{Phase: "error", AllocatedMiB: opts.touchMiB, Error: msg})
		os.Exit(1)
	}
	emit(start, event{Phase: "freed", AllocatedMiB: opts.touchMiB})

	time.Sleep(opts.postFree)
	emit(start, event{Phase: "after_free", AllocatedMiB: opts.touchMiB})
}

func parseOptions(args []string) (options, error) {
	opts := options{
		touchMiB:   256,
		preTouch:   500 * time.Millisecond,
		hold:       time.Second,
		postFree:   3 * time.Second,
		pageStride: unix.Getpagesize(),
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		value := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("missing value for %s", arg)
			}
			i++
			return args[i], nil
		}
		switch arg {
		case "--touch-mib":
			raw, err := value()
			if err != nil {
				return opts, err
			}
			parsed, err := strconv.ParseUint(raw, 10, 64)
			if err != nil || parsed == 0 {
				return opts, fmt.Errorf("invalid --touch-mib")
			}
			opts.touchMiB = parsed
		case "--hold-ms":
			duration, err := parseMillis(value)
			if err != nil {
				return opts, fmt.Errorf("invalid --hold-ms: %w", err)
			}
			opts.hold = duration
		case "--pre-touch-ms":
			duration, err := parseMillis(value)
			if err != nil {
				return opts, fmt.Errorf("invalid --pre-touch-ms: %w", err)
			}
			opts.preTouch = duration
		case "--post-free-ms":
			duration, err := parseMillis(value)
			if err != nil {
				return opts, fmt.Errorf("invalid --post-free-ms: %w", err)
			}
			opts.postFree = duration
		default:
			return opts, fmt.Errorf("unknown argument: %s", arg)
		}
	}
	return opts, nil
}

func parseMillis(next func() (string, error)) (time.Duration, error) {
	raw, err := next()
	if err != nil {
		return 0, err
	}
	parsed, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(parsed) * time.Millisecond, nil
}

func emit(start time.Time, ev event) {
	ev.ElapsedMS = time.Since(start).Milliseconds()
	ev.Meminfo = readMeminfo()
	_ = json.NewEncoder(os.Stdout).Encode(ev)
}

func readMeminfo() map[string]uint64 {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return nil
	}
	defer file.Close()

	out := map[string]uint64{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		key := strings.TrimSuffix(fields[0], ":")
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		out[key] = value
	}
	return out
}

func joinErrors(first, second error) string {
	parts := []string{}
	if first != nil {
		parts = append(parts, "madvise: "+first.Error())
	}
	if second != nil {
		parts = append(parts, "munmap: "+second.Error())
	}
	return strings.Join(parts, "; ")
}
