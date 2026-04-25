package main

const maxExecResponseOutputBytes = 1 << 20

type boundedOutputBuffer struct {
	limit int
	data  []byte
}

func newBoundedOutputBuffer(limit int) *boundedOutputBuffer {
	return &boundedOutputBuffer{limit: limit}
}

func (b *boundedOutputBuffer) Write(p []byte) (int, error) {
	written := len(p)
	if len(p) == 0 || b.limit <= 0 {
		return written, nil
	}
	if len(p) >= b.limit {
		b.data = append(b.data[:0], p[len(p)-b.limit:]...)
		return written, nil
	}
	b.data = append(b.data, p...)
	if len(b.data) > b.limit {
		copy(b.data, b.data[len(b.data)-b.limit:])
		b.data = b.data[:b.limit]
	}
	return written, nil
}

func (b *boundedOutputBuffer) String() string {
	return string(b.data)
}
