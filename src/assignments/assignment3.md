# Assignment 3: Concurrent Channel Implementation

**Reinforces:** Module 6 (Synchronization Primitives), Module 7 (Channels and Select)

## Overview

In this assignment, you will implement a channel-like concurrent communication primitive from scratch, using only mutexes and condition variables (no Go channels allowed in the implementation). Your channel must support unbuffered and buffered modes, blocking send/receive, non-blocking try-send/try-receive, and a simplified `select`-like multiplexing operation.

This assignment will give you deep understanding of how channels work internally -- the waiting queues, the direct-send optimization, lock ordering, and the subtleties of blocking and waking goroutines.

## Learning Objectives

By completing this assignment, you will be able to:

1. Implement blocking synchronization using mutexes and condition variables.
2. Build a circular buffer with correct concurrent access.
3. Handle the direct-send/receive optimization for unbuffered channels.
4. Implement lock ordering to prevent deadlocks in multi-channel operations.
5. Reason about memory visibility, lost wakeups, and spurious wakeups.

## Specification

### Core Type

```go
package channel

import (
	"sync"
	"errors"
)

var (
	ErrClosed    = errors.New("send on closed channel")
	ErrEmpty     = errors.New("receive on empty closed channel")
)

// Chan is a typed channel supporting buffered and unbuffered operation.
// The type parameter T is the element type.
type Chan[T any] struct {
	// TODO: Add fields
	// Consider: mutex, circular buffer, send/recv wait queues,
	// closed flag, capacity, count, etc.
}

// New creates a new channel with the given buffer capacity.
// A capacity of 0 creates an unbuffered channel.
func New[T any](capacity int) *Chan[T]
```

### Required Operations

#### Part A: Buffered Channel (30 points)

```go
// Send sends a value on the channel. Blocks if the buffer is full.
// Panics if the channel is closed.
func (c *Chan[T]) Send(val T)

// Recv receives a value from the channel. Blocks if the buffer is empty.
// Returns the value and true if successful, or the zero value and false
// if the channel is closed and empty.
func (c *Chan[T]) Recv() (T, bool)

// Close closes the channel. After closing, Send will panic.
// Recv will return remaining buffered values, then (zero, false).
// Close may be called only once; subsequent calls panic.
func (c *Chan[T]) Close()

// Len returns the number of elements currently in the buffer.
func (c *Chan[T]) Len() int

// Cap returns the channel's buffer capacity.
func (c *Chan[T]) Cap() int
```

Requirements:
- The buffer must be a circular queue (not a slice that grows/shrinks).
- `Send` must block (not spin) when the buffer is full.
- `Recv` must block (not spin) when the buffer is empty.
- After `Close()`, `Recv` must drain remaining buffer contents before returning `(zero, false)`.
- Calling `Send` on a closed channel must panic (matching Go's behavior).
- Must be safe for concurrent use by multiple goroutines.

#### Part B: Unbuffered Channel (25 points)

Your channel with `capacity == 0` must behave as an unbuffered (synchronous) channel:

- `Send` blocks until a corresponding `Recv` is ready.
- `Recv` blocks until a corresponding `Send` is ready.
- When both are ready, the value is transferred directly from sender to receiver (the **direct send** optimization -- no intermediate buffer copy).
- A `Send` and `Recv` that rendezvous must both unblock.

```go
// For unbuffered channels, implement a handshake mechanism:
// the sender waits until a receiver is available, then transfers
// the value directly and both proceed.
```

You must implement waiting queues for blocked senders and receivers (analogous to `sendq` and `recvq` in the Go runtime's `hchan`):

```go
// waiter represents a blocked goroutine waiting to send or receive.
type waiter[T any] struct {
	val    T            // For senders: the value to send.
	                    // For receivers: where to store the received value.
	done   chan struct{} // Signal that the operation completed.
	                    // (This is the ONE place you may use a Go channel --
	                    //  as a goroutine wake-up mechanism analogous to gopark/goready.
	                    //  Alternatively, use sync.Cond.)
}
```

#### Part C: Non-blocking Operations (15 points)

```go
// TrySend attempts to send without blocking.
// Returns true if the value was sent, false if the channel is full or closed.
func (c *Chan[T]) TrySend(val T) bool

// TryRecv attempts to receive without blocking.
// Returns the value, true, true if a value was received.
// Returns zero, true, false if the channel is closed and empty.
// Returns zero, false, false if the channel is empty but open (would block).
func (c *Chan[T]) TryRecv() (val T, ok bool, received bool)
```

These must be truly non-blocking: they acquire the lock, check the state, and return immediately.

#### Part D: Select (Multiplexing) (20 points)

Implement a simplified `Select` that waits on multiple channel operations:

```go
// SelectCase describes a single case in a Select operation.
type SelectCase[T any] struct {
	Chan      *Chan[T]
	Dir       SelectDir      // SelectSend or SelectRecv
	SendVal   T              // Value to send (for SelectSend)
}

type SelectDir int

const (
	SelectSend SelectDir = iota
	SelectRecv
)

// SelectResult contains the outcome of a Select.
type SelectResult[T any] struct {
	Index    int    // Which case was selected
	Value    T      // Received value (for SelectRecv cases)
	Ok       bool   // false if channel was closed (for SelectRecv)
}

// Select waits until one of the given cases can proceed.
// If multiple cases are ready, one is chosen at random.
// Select blocks until at least one case is ready (no default case).
func Select[T any](cases []SelectCase[T]) SelectResult[T]
```

Requirements:
- **Lock ordering:** Acquire channel locks in address order to prevent deadlocks (matching `selectgo`'s approach).
- **Random polling:** Check cases in a random order to prevent starvation.
- **Correct blocking:** If no case is ready, enqueue the goroutine on ALL channels' wait queues. When one operation completes, dequeue from all others.
- Handle the case where the same channel appears in multiple cases.

#### Part E: Testing and Verification (10 points)

Write comprehensive tests:

```go
// TestBufferedBasic - single producer, single consumer with buffered channel.
// TestUnbufferedRendezvous - verify sender and receiver synchronize.
// TestConcurrentSendRecv - many goroutines sending and receiving simultaneously.
// TestCloseSemantics - verify close behavior matches Go's channels.
// TestSelectBasic - select with multiple channels, verify fairness.
// TestSelectDeadlock - verify select with all-blocked channels does not spin.
// TestDirectSend - verify unbuffered channel copies directly (measure allocation?).
// TestStress - 1000 goroutines, mix of operations, run with -race.
```

## Constraints

- **No Go channels** in your implementation (except: you may use a `chan struct{}` inside the `waiter` struct as a goroutine wake-up mechanism, since you cannot call `gopark`/`goready` from user code. Alternatively, use `sync.Cond`).
- **No busy-waiting / spinning.** All blocking must use `sync.Cond.Wait()` or equivalent.
- **No `unsafe` package.** Use generics for type safety.
- **Must pass `go test -race`** with no data races.

## Starter Code Structure

```
assignment3/
├── channel/
│   ├── chan.go              # Chan struct, New, Send, Recv, Close
│   ├── unbuffered.go        # Unbuffered channel specialization
│   ├── nonblocking.go       # TrySend, TryRecv
│   ├── select.go            # Select implementation
│   ├── chan_test.go          # Correctness tests
│   └── chan_bench_test.go    # Performance benchmarks
├── cmd/
│   └── demo/
│       └── main.go          # Demo: producer-consumer, fan-in, pipeline
└── go.mod
```

## Grading Rubric

| Component | Points | Criteria |
|-----------|--------|----------|
| Part A: Buffered send/recv | 15 | Correct blocking, circular buffer, concurrent safety |
| Part A: Close semantics | 10 | Panic on send-after-close; drain buffer on recv |
| Part A: Edge cases | 5 | Zero-element types, single-capacity buffer, etc. |
| Part B: Unbuffered rendezvous | 15 | Correct synchronous handshake |
| Part B: Direct send | 10 | Value transferred without intermediate copy |
| Part C: TrySend/TryRecv | 15 | Truly non-blocking; correct return values |
| Part D: Select lock ordering | 8 | Channels locked in address order |
| Part D: Select blocking/wakeup | 12 | Correct multi-channel wait and dequeue |
| Part E: Test quality | 10 | Covers concurrency, edge cases, passes -race |
| **Total** | **100** | |

## Hints

1. **Start with the buffered channel.** Get `Send`/`Recv`/`Close` working for `capacity > 0` before tackling unbuffered. The buffered case is simpler because the buffer decouples senders from receivers.

2. **Use `sync.Cond` for blocking.** Create two condition variables on the same mutex: one for "buffer not full" (senders wait on this) and one for "buffer not empty" (receivers wait on this). Remember to use a `for` loop around `Cond.Wait()` to handle spurious wakeups.

3. **The unbuffered case is a rendezvous.** Think of it as: the sender adds itself to a "waiting senders" queue with its value, then waits. When a receiver arrives, it takes the value directly from the sender's waiter struct, signals the sender, and both proceed. Vice versa if the receiver arrives first.

4. **For direct send in unbuffered channels**, study how `runtime/chan.go`'s `send()` function copies directly to the receiver's memory via `sendDirect`. Your version will copy from the `waiter.val` field to the receiver's return value.

5. **Lock ordering for Select**: Sort the cases by `&c` (the channel pointer converted to `uintptr`) before acquiring locks. This is exactly what `selectgo` does with `lockorder`. After sorting, lock all channels, check each case in random order, and if none are ready, enqueue on all channels before releasing locks.

6. **The hardest bug will be lost wakeups.** When a receiver arrives and finds the buffer empty:
   - It must atomically (under the lock) check the buffer AND add itself to the wait queue.
   - If it checks, finds empty, releases the lock, then enqueues, a sender could deposit a value and signal between the check and the enqueue, causing a lost wakeup.

7. **Testing concurrent close**: `Close` must wake ALL blocked senders (they should panic) and ALL blocked receivers (they should return zero, false). Use `sync.Cond.Broadcast()` for this.

8. **Benchmarking**: Compare your channel against Go's built-in channels for single-producer-single-consumer and multi-producer-multi-consumer patterns. Expect your implementation to be 5-20x slower (Go's channels use runtime internals like `gopark` and direct stack manipulation that you cannot access). Analyze why in your benchmark comments.
