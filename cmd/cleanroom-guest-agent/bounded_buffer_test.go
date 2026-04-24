package main

import "testing"

func TestBoundedOutputBufferRetainsTail(t *testing.T) {
	buf := newBoundedOutputBuffer(5)

	if n, err := buf.Write([]byte("hello")); err != nil || n != 5 {
		t.Fatalf("first write = %d, %v; want 5, nil", n, err)
	}
	if got, want := buf.String(), "hello"; got != want {
		t.Fatalf("buffer after first write = %q, want %q", got, want)
	}

	if n, err := buf.Write([]byte(" world")); err != nil || n != 6 {
		t.Fatalf("second write = %d, %v; want 6, nil", n, err)
	}
	if got, want := buf.String(), "world"; got != want {
		t.Fatalf("buffer after second write = %q, want %q", got, want)
	}
}
