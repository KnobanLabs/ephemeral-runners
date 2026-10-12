package main

import (
	"context"
	"slices"
	"sync"
)

// maxSlots bounds concurrent VMs; each slot owns one fixed MAC address.
const maxSlots = 250

// Host hands out this Mac's capacity to VMs from every pool: shared memory
// and vCPU budgets, the macOS guest cap, and network slots.
//
// Memory is granted in arrival order so a large macOS request is not starved
// by a stream of small Linux ones. A request blocked only by the macOS cap
// does not hold up anyone else.
type Host struct {
	mu        sync.Mutex
	changed   chan struct{}
	memFree   int
	cpuFree   int
	macFree   int
	slots     [maxSlots]bool
	nextSeq   uint64
	waitQueue []*hostRequest
}

type hostRequest struct {
	seq   uint64
	mem   int
	cpu   int
	macOS bool
}

// Lease is capacity held by one VM. Release returns it.
type Lease struct {
	Slot  int
	mem   int
	cpu   int
	macOS bool
}

func NewHost(memoryMB, vcpus, maxMacOS int) *Host {
	return &Host{changed: make(chan struct{}), memFree: memoryMB, cpuFree: vcpus, macFree: maxMacOS}
}

// Acquire blocks until the request fits, or ctx is done.
func (h *Host) Acquire(ctx context.Context, memMB, cpu int, macOS bool) (*Lease, error) {
	h.mu.Lock()
	req := &hostRequest{seq: h.nextSeq, mem: memMB, cpu: cpu, macOS: macOS}
	h.nextSeq++
	h.waitQueue = append(h.waitQueue, req)
	h.mu.Unlock()

	for {
		h.mu.Lock()
		if lease, ok := h.tryTake(req); ok {
			h.mu.Unlock()
			return lease, nil
		}
		changed := h.changed
		h.mu.Unlock()

		select {
		case <-ctx.Done():
			h.mu.Lock()
			h.dequeue(req)
			h.mu.Unlock()
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (h *Host) tryTake(req *hostRequest) (*Lease, bool) {
	if req.macOS && h.macFree == 0 {
		return nil, false
	}
	for _, other := range h.waitQueue {
		if other.seq >= req.seq {
			break
		}
		if !(other.macOS && h.macFree == 0) {
			// An older request is waiting on memory or CPU; it goes first.
			return nil, false
		}
	}
	if req.mem > h.memFree || req.cpu > h.cpuFree {
		return nil, false
	}
	slot := slices.Index(h.slots[:], false)
	if slot < 0 {
		return nil, false
	}
	h.slots[slot] = true
	h.memFree -= req.mem
	h.cpuFree -= req.cpu
	if req.macOS {
		h.macFree--
	}
	h.dequeue(req)
	return &Lease{Slot: slot, mem: req.mem, cpu: req.cpu, macOS: req.macOS}, true
}

func (h *Host) dequeue(req *hostRequest) {
	h.waitQueue = slices.DeleteFunc(h.waitQueue, func(r *hostRequest) bool { return r == req })
	h.broadcast()
}

func (h *Host) Release(l *Lease) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.slots[l.Slot] = false
	h.memFree += l.mem
	h.cpuFree += l.cpu
	if l.macOS {
		h.macFree++
	}
	h.broadcast()
}

// broadcast wakes every waiter. Callers hold h.mu.
func (h *Host) broadcast() {
	close(h.changed)
	h.changed = make(chan struct{})
}
