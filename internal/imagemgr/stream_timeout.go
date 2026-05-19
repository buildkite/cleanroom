package imagemgr

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

type rootFSStreamIdleTimeoutReadCloser struct {
	stream  io.ReadCloser
	timeout time.Duration

	closeOnce sync.Once
	closeErr  error
	timedOut  atomic.Bool
}

func (r *rootFSStreamIdleTimeoutReadCloser) Read(p []byte) (int, error) {
	if r == nil || r.stream == nil {
		return 0, io.ErrClosedPipe
	}
	if r.timeout <= 0 {
		return r.stream.Read(p)
	}

	timerDone := make(chan struct{})
	timer := time.AfterFunc(r.timeout, func() {
		r.timedOut.Store(true)
		_ = r.Close()
		close(timerDone)
	})
	n, err := r.stream.Read(p)
	if !timer.Stop() {
		<-timerDone
	}
	if r.timedOut.Load() {
		timeoutErr := fmt.Errorf("%w: no data received for %s", errRootFSStreamIdleTimeout, r.timeout)
		if err == nil {
			err = timeoutErr
		} else {
			err = errors.Join(timeoutErr, err)
		}
	}
	return n, err
}

func (r *rootFSStreamIdleTimeoutReadCloser) Close() error {
	if r == nil || r.stream == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		r.closeErr = r.stream.Close()
	})
	return r.closeErr
}
