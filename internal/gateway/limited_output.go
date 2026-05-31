package gateway

import (
	"fmt"
	"strings"
)

const maxGitCommandErrorOutputBytes = 64 << 10

type limitedOutputBuffer struct {
	limit     int
	buf       []byte
	truncated int64
}

func newLimitedOutputBuffer(limit int) *limitedOutputBuffer {
	if limit < 0 {
		limit = 0
	}
	return &limitedOutputBuffer{limit: limit}
}

func (b *limitedOutputBuffer) Write(p []byte) (int, error) {
	originalLen := len(p)
	if b == nil {
		return originalLen, nil
	}
	if remaining := b.limit - len(b.buf); remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		b.buf = append(b.buf, p[:remaining]...)
		p = p[remaining:]
	}
	b.truncated += int64(len(p))
	return originalLen, nil
}

func (b *limitedOutputBuffer) String() string {
	if b == nil {
		return ""
	}
	out := strings.ToValidUTF8(string(b.buf), "\uFFFD")
	if b.truncated > 0 {
		if strings.TrimSpace(out) != "" {
			out += "\n"
		}
		out += fmt.Sprintf("[truncated %d bytes]", b.truncated)
	}
	return out
}
