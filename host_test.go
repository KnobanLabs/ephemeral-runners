package main

import (
	"context"
	"testing"
	"time"
)

func acquireAsync(h *Host, mem int, macOS bool) <-chan *Lease {
	ch := make(chan *Lease, 1)
	go func() {
		l, _ := h.Acquire(context.Background(), mem, 1, macOS)
		ch <- l
	}()
	return ch
}

func waitQueued(t *testing.T, h *Host, n int) {
	t.Helper()
	for range 100 {
		h.mu.Lock()
		got := len(h.waitQueue)
		h.mu.Unlock()
		if got == n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("expected %d waiters", n)
}

func TestHostMacOSCap(t *testing.T) {
	h := NewHost(100, 100, 2)
	ctx := context.Background()
	a, _ := h.Acquire(ctx, 10, 1, true)
	if _, err := h.Acquire(ctx, 10, 1, true); err != nil {
		t.Fatal(err)
	}
	third := acquireAsync(h, 10, true)
	waitQueued(t, h, 1)

	// A Linux request isn't held up by a macOS one waiting on the cap.
	if _, err := h.Acquire(ctx, 10, 1, false); err != nil {
		t.Fatal(err)
	}
	h.Release(a)
	if l := <-third; l == nil {
		t.Fatal("third macOS VM never got capacity")
	}
}

func TestHostMemoryIsFirstComeFirstServed(t *testing.T) {
	h := NewHost(12, 100, 2)
	ctx := context.Background()
	l1, _ := h.Acquire(ctx, 4, 1, false)
	l2, _ := h.Acquire(ctx, 4, 1, false)
	_, _ = h.Acquire(ctx, 4, 1, false)

	mac := acquireAsync(h, 8, true)
	waitQueued(t, h, 1)
	linux := acquireAsync(h, 4, false)
	waitQueued(t, h, 2)

	// 4 MB frees up: enough for the Linux request, but the older macOS
	// request is first in line and must not be starved.
	h.Release(l1)
	select {
	case <-linux:
		t.Fatal("newer Linux request jumped ahead of the waiting macOS one")
	case <-time.After(50 * time.Millisecond):
	}
	h.Release(l2)
	if l := <-mac; l == nil || !l.macOS {
		t.Fatal("macOS request should get the freed memory")
	}
}

func TestHostAcquireCanceled(t *testing.T) {
	h := NewHost(4, 100, 2)
	_, _ = h.Acquire(context.Background(), 4, 1, false)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := h.Acquire(ctx, 4, 1, false); err == nil {
		t.Fatal("expected cancellation")
	}
	if len(h.waitQueue) != 0 {
		t.Fatal("canceled request left in queue")
	}
}

func TestHostCPUBudget(t *testing.T) {
	h := NewHost(100, 8, 2)
	ctx := context.Background()
	l, _ := h.Acquire(ctx, 4, 4, false)
	_, _ = h.Acquire(ctx, 4, 4, false)
	third := make(chan *Lease, 1)
	go func() {
		l, _ := h.Acquire(ctx, 4, 4, false)
		third <- l
	}()
	waitQueued(t, h, 1)
	h.Release(l)
	if l := <-third; l == nil {
		t.Fatal("third VM never got CPU")
	}
}

func TestWatchWriterSplitNeedle(t *testing.T) {
	found := make(chan struct{})
	w := &watchWriter{w: discard{}, needle: []byte("Listening for Jobs"), found: found}
	w.Write([]byte("√ Connected to GitHub\nListening f"))
	w.Write([]byte("or Jobs\n"))
	select {
	case <-found:
	default:
		t.Fatal("needle split across writes was missed")
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
