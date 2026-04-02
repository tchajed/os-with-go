# Module 6: Synchronization Primitives (futex, semaphores, rwmutex)

## Comprehension Check

### Question 1 (Code Reading)
Consider this implementation of `futexsleep` from `runtime/os_linux.go`:

```go
func futexsleep(addr *uint32, val uint32, ns int64) {
	if ns < 0 {
		futex(unsafe.Pointer(addr), _FUTEX_WAIT_PRIVATE, val, nil, nil, 0)
		return
	}

	var ts timespec
	ts.setNsec(ns)
	futex(unsafe.Pointer(addr), _FUTEX_WAIT_PRIVATE, val, &ts, nil, 0)
}
```

What does `_FUTEX_WAIT_PRIVATE` mean, and why does the runtime use the "PRIVATE" variant instead of plain `FUTEX_WAIT`?

<details><summary>Answer</summary>

`_FUTEX_WAIT_PRIVATE` is `FUTEX_WAIT | FUTEX_PRIVATE_FLAG`. The `FUTEX_PRIVATE_FLAG` tells the kernel that this futex is only shared within a single process (not shared between processes via shared memory).

The runtime uses the PRIVATE variant because:
1. **Performance:** Private futexes are faster in the kernel because they do not need to go through the full virtual-to-physical address translation to find the futex hash bucket. The kernel can use the virtual address directly, since it is unique within the process. This avoids a page table walk.
2. **Correctness:** The Go runtime's internal locks are never shared between processes. All Ms (threads) belong to the same process, so there is no need for cross-process futex semantics.
3. **Simpler kernel path:** The private path avoids potential lock contention on shared futex hash buckets that cross-process futexes require.

</details>

---

### Question 2 (Short Answer)
The Go runtime implements its own `mutex` type (in `lock_futex.go` or `lock_sema.go`) rather than using `pthread_mutex_t`. What are the key differences between the runtime's mutex and a pthread mutex? Why not use pthreads?

<details><summary>Answer</summary>

**Key differences:**

1. **Implementation:** The runtime's mutex on Linux is built directly on `futex()` system calls. On non-Linux platforms, it uses OS-specific semaphores. `pthread_mutex_t` wraps similar primitives but through the C library.

2. **Stack requirements:** The runtime's mutex can be used on the system stack (`g0`) and in `nosplit` contexts. `pthread_mutex_t` operations may involve C function calls that expect a C stack and calling convention.

3. **No error checking:** The runtime's mutex is minimal -- it does not track ownership, recursion, or error conditions like `EDEADLK`. It is a simple spin-then-sleep lock.

4. **Integration with scheduler:** The runtime's lock operations interact with the scheduler (e.g., `mp.locks++` prevents preemption while holding a lock). pthread mutexes have no knowledge of the Go scheduler.

**Why not pthreads:**
- Using pthreads would require cgo, which has significant overhead per call (~200ns) and stack switching requirements.
- The runtime mutex is used extremely early in process startup, before pthreads or the C runtime are initialized.
- The runtime needs to control the locking implementation for correctness: e.g., it must not call `gopark` while holding certain locks, and it must prevent stack splits during lock operations.

</details>

---

### Question 3 (True/False with Explanation)
**True or False:** Go's `sync.Mutex` is implemented directly using the runtime's internal `runtime.mutex` (the futex-based lock).

<details><summary>Answer</summary>

**False.** `sync.Mutex` and `runtime.mutex` are completely separate implementations.

- **`runtime.mutex`** is a low-level lock used internally by the runtime for protecting scheduler data structures, the heap, channel internals, etc. It is built on `futex` (Linux) or OS semaphores and operates at the M (OS thread) level. It blocks the entire OS thread.

- **`sync.Mutex`** is a user-facing lock that integrates with the goroutine scheduler. It uses the runtime's `semaphore` mechanism (`runtime_SemacquireMutex` / `runtime_Semrelease`), which parks and unparks goroutines (not OS threads). When a goroutine blocks on `sync.Mutex.Lock()`, only the goroutine is parked -- the M/P can continue scheduling other goroutines.

Additionally, `sync.Mutex` implements a more sophisticated algorithm with two modes:
- **Normal mode:** New arrivals spin briefly then queue. Goroutines are woken in FIFO order.
- **Starvation mode:** If a waiter has been waiting >1ms, the mutex switches to starvation mode where the lock is directly handed to the first waiter, preventing tail latency.

</details>

---

### Question 4 (What Would Happen If...)
Go's `sync.RWMutex` allows multiple concurrent readers or a single writer. What would happen if `RLock()`/`RUnlock()` used an atomic counter but did not coordinate with writers at all (i.e., readers never blocked even when a writer is waiting)?

<details><summary>Answer</summary>

This would cause **writer starvation.** In a read-heavy workload:

1. Readers would continuously acquire the read lock via atomic increment.
2. A writer calling `Lock()` would wait for the reader count to reach zero.
3. But new readers would keep arriving and incrementing the count, never letting it reach zero.
4. The writer would wait indefinitely.

Go's actual `sync.RWMutex` prevents this by manipulating `readerCount` when a writer arrives:

When a writer calls `Lock()`, it subtracts `rwmutexMaxReaders` (a large constant, 1<<30) from `readerCount`, making it negative. New readers calling `RLock()` see the negative value and know a writer is pending, so they block (park). This ensures that once a writer is waiting, no new readers can proceed -- only existing readers drain, and then the writer gets the lock.

Without this mechanism, any workload with continuous read traffic would make writes effectively impossible.

</details>

---

### Question 5 (Short Answer)
Explain the difference between a **semaphore** and a **futex**. How does the Go runtime use each?

<details><summary>Answer</summary>

**Futex (Fast Userspace Mutex):**
- A Linux kernel primitive that provides atomic "compare-and-sleep" and "wake" operations on a 32-bit integer in user space.
- The fast path (uncontended lock/unlock) is entirely in user space -- no system call needed.
- The slow path (contended) involves a system call to put the thread to sleep or wake it.
- In the Go runtime: used to implement `runtime.mutex` (the internal lock) and `runtime.note` (one-shot notification).

**Semaphore (in Go's runtime):**
- Implemented in `runtime/sema.go` as a higher-level primitive.
- A goroutine-aware counting semaphore built on top of a treap (tree + heap) of waiters.
- Waiters are goroutines (not OS threads) and are parked/unparked via `gopark`/`goready`.
- Supports FIFO ordering and lifo wakeup for different use cases.
- In the Go runtime: used by `sync.Mutex`, `sync.RWMutex`, `sync.WaitGroup`, and `sync.Cond`.

**Key distinction:** Futexes operate at the OS thread level (block/wake threads), while Go's semaphores operate at the goroutine level (park/ready goroutines). This is crucial because blocking an OS thread is much more expensive than parking a goroutine.

</details>

---

### Question 6 (Code Reading)
Consider a simplified version of the semaphore acquire path:

```go
func semacquire1(addr *uint32, lifo bool, profile semaProfileFlags, skipframes int, reason waitReason) {
	// Fast path: *addr > 0, try to decrement
	if cansemacquire(addr) {
		return
	}

	// Slow path: increment waiter count, then try to acquire again
	s := acquireSudog()
	root := semtable[addr.hash()].root
	root.queue(addr, s, lifo)
	gopark_m(...)  // park the goroutine
	releaseSudog(s)
}
```

Why does the slow path increment the waiter count and try again before actually parking? What race condition does this double-check prevent?

<details><summary>Answer</summary>

This is a classic **double-checked locking / lost wakeup prevention** pattern:

1. The fast path checks if the semaphore is available (`*addr > 0`) and tries to decrement it atomically.
2. If it fails, the goroutine enters the slow path.
3. Between the fast path check and the slow path, another goroutine might have released the semaphore (called `semrelease`).
4. If the goroutine blindly parked without rechecking, the release would have incremented `*addr` and potentially called `goready` -- but since the goroutine was not yet parked, the wakeup would be lost. The goroutine would sleep forever.

By registering as a waiter first (adding to the queue) and then trying to acquire again:
- If the semaphore became available, the goroutine acquires it and dequeues itself.
- If it is still unavailable, the goroutine is already properly registered as a waiter, so any future `semrelease` will find and wake it.

This ensures no wakeup is ever lost, which is the same principle behind the atomic check in `futex(FUTEX_WAIT, addr, val)`.

</details>
