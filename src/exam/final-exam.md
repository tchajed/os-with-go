# Final Exam: Operating Systems Through the Go Runtime

**Duration:** 3 hours<br>
**Total Points:** 100<br>
**Open book: You may refer to the Go source code and course materials.**

---

## Part A: Concept Questions (40 points, 4 points each)

Answer each question in 3-5 sentences.

---

### A1 (Modules 1-2)
The Go runtime makes system calls directly via assembly (e.g., `sys_linux_amd64.s`) rather than calling through libc. Explain one situation where this causes a compatibility problem and one situation where it provides a significant advantage.

<details><summary>Answer</summary>

**Compatibility problem:** On some systems, DNS resolution depends on NSS (Name Service Switch) modules that are loaded as shared libraries by libc's `getaddrinfo()`. Since Go bypasses libc, it cannot use these modules and must implement its own DNS resolver (the "pure Go resolver"). This causes issues when systems use LDAP, mDNS, or custom NSS modules for name resolution, as Go's resolver does not support them.

**Advantage:** Direct syscalls eliminate the overhead of switching to a C calling convention, managing a separate C stack, and coordinating with libc's internal state (errno, signal masks, thread-local storage). This is critical for the Go scheduler, which makes frequent, performance-sensitive syscalls (e.g., `futex` for locks, `epoll_wait` for the network poller) and needs precise control over thread state during these calls. The overhead of a cgo call (~200ns) would be prohibitive for operations that occur millions of times per second.

</details>

---

### A2 (Modules 1-2)
When a goroutine makes a blocking system call, the runtime calls `entersyscall()` before the call and `exitsyscall()` after it returns. What would happen if these calls were omitted? Describe the impact on both the specific goroutine and the overall scheduler.

<details><summary>Answer</summary>

Without `entersyscall()`:
- The P remains attached to the M during the blocking syscall. Since the M is blocked in the kernel, the P is effectively frozen -- no other goroutines on that P's run queue can execute.
- `sysmon` would not know the M is in a syscall and would not retake the P.
- If all Ps end up blocked in syscalls, the entire program stops executing Go code until some syscall returns.

Without `exitsyscall()`:
- The goroutine's status would remain `_Gsyscall` after returning from the syscall. The scheduler would not transition it to `_Grunning`.
- If the P was retaken during the syscall, the goroutine would continue running without a P, violating the invariant that Go code requires a P. This could corrupt scheduler state.

In short, `entersyscall`/`exitsyscall` maintain the invariant that Ps are never wasted on blocked threads, and that the scheduler accurately tracks goroutine states.

</details>

---

### A3 (Modules 3-4)
Explain why the Go scheduler uses an M:N threading model rather than a 1:1 model (one goroutine per OS thread) or an N:1 model (all goroutines on one OS thread). What specific limitations of 1:1 and N:1 does M:N overcome?

<details><summary>Answer</summary>

**1:1 limitations:** OS threads are expensive -- each requires ~8KB kernel stack, a full `task_struct` in the kernel, and creation involves a `clone` syscall. Thread context switches require kernel transitions. A program with 100,000 goroutines would need 100,000 OS threads, consuming ~800MB of kernel stack alone and overwhelming the kernel scheduler. Go programs routinely create millions of goroutines, which is infeasible with 1:1.

**N:1 limitations:** All goroutines on a single OS thread cannot achieve parallelism on multi-core CPUs. Additionally, one blocking syscall blocks all goroutines, since there is only one thread to run them.

**M:N overcomes both:** Many goroutines (N) are multiplexed onto a smaller number of OS threads (M). This provides: true multi-core parallelism (multiple Ms running in parallel), lightweight concurrency (goroutines are cheap), and syscall resilience (blocking one M does not block other goroutines, because the P is handed to another M). The P abstraction limits parallelism to GOMAXPROCS, keeping OS thread count manageable.

</details>

---

### A4 (Modules 3-4)
The `g0` goroutine exists on every M and has a larger, fixed-size stack. Why can't the scheduler run on a regular goroutine's stack? Give two specific operations that require `g0`.

<details><summary>Answer</summary>

The scheduler cannot run on a regular goroutine's stack because:

1. **Stack growth:** If the scheduler tried to grow a goroutine's stack while running on that same stack, it would need to copy the memory it is currently executing on -- a self-referential operation that would corrupt the active stack frame. `g0` has a fixed, large stack that never needs growing.

2. **Context switching:** When switching from one goroutine to another, the old goroutine's state (including stack pointer) must be saved to its `gobuf`. This requires running on a different stack -- you cannot save and abandon a stack while still using it.

**Two operations requiring `g0`:**
- `copystack()`: Allocates a new stack and copies the goroutine's stack. Must run on `g0` because it is modifying the goroutine's stack.
- `schedule()` and `findRunnable()`: The scheduling loop runs on `g0` via `mcall()` or `systemstack()`, because it manipulates scheduler data structures that must not be interrupted by stack growth, and because it needs a reliable stack while goroutine stacks are being swapped.

</details>

---

### A5 (Modules 5-6)
The Go scheduler limits the number of "spinning" threads. Explain the tradeoff: what happens if no spinning threads are allowed? What happens if the limit is removed and all idle threads spin?

<details><summary>Answer</summary>

**No spinning threads:** When new work arrives (a goroutine becomes runnable), there would be no thread immediately available to pick it up. An idle thread would need to be unparked from sleep, which involves a `futex` wakeup syscall and OS scheduler intervention -- adding potentially 10-50 microseconds of latency before the goroutine starts running. For latency-sensitive workloads, this delay is significant.

**All idle threads spinning:** Every idle M would be in a busy loop consuming 100% of its CPU core, checking run queues, the global queue, and the network poller repeatedly. On a 64-core machine with only a few goroutines, 60+ cores would be spinning uselessly, wasting power and competing for memory bandwidth and cache. The system would also show 100% CPU utilization even when the Go program has little actual work.

**The balance:** Go allows one spinning thread (approximately). This ensures at least one thread is always ready to immediately pick up new work (low latency), while all other idle threads park (low power/CPU waste). When the single spinning thread finds work, it wakes another thread to spin in its place, maintaining the invariant.

</details>

---

### A6 (Modules 5-6)
Compare how Go's `sync.Mutex` and the runtime's internal `runtime.mutex` handle contention. What happens when a goroutine blocks on each? Why are two different mutex implementations necessary?

<details><summary>Answer</summary>

**`runtime.mutex` (internal):** When contended, blocks the entire OS thread using `futexsleep()`. The thread enters the kernel and sleeps until `futexwakeup()` is called by the unlocker. The Go scheduler cannot schedule other goroutines on this blocked M because the M itself is asleep in the kernel.

**`sync.Mutex` (user-facing):** When contended, parks the goroutine (not the thread) using `runtime_SemacquireMutex`, which calls `gopark()`. The M and P remain active and can schedule other goroutines. Only the specific goroutine is blocked. Additionally, `sync.Mutex` has two modes (normal and starvation) with spinning on the fast path before parking.

**Why two implementations:** `runtime.mutex` is used to protect scheduler internals (run queues, P state, etc.). Using `sync.Mutex` there would create a circular dependency -- `gopark` uses the scheduler, and the scheduler uses these locks. You cannot park a goroutine while modifying the data structures that manage goroutine parking. `runtime.mutex` operates at a lower level (OS thread) to break this circularity.

</details>

---

### A7 (Modules 7-8)
When sending a value on an unbuffered channel and a receiver is already waiting, Go copies the value directly from the sender's stack to the receiver's stack. Explain why this is safe even though writing to another goroutine's stack is normally forbidden.

<details><summary>Answer</summary>

This direct copy is safe because of several guarantees at the point it occurs:

1. **The receiver is in `_Gwaiting` state:** It is parked and will not be running or modifying its own stack until `goready` is called for it. Its stack is stable.

2. **The receiver's stack cannot be moved:** Stack growth only happens at function prologues when the goroutine is running. Since the receiver is parked, its stack cannot grow or be copied to a new location during the direct send.

3. **The channel lock is held:** The sender holds `c.lock`, ensuring no other goroutine can concurrently operate on this channel, and the receiver's `sudog` (which contains the pointer to the receiver's stack location, `sg.elem`) is stable.

4. **GC safety:** The GC will not scan or shrink the receiver's stack during this operation because it must acquire the channel lock (or observe the goroutine's status atomically) before manipulating it.

The key invariant is: the receiver goroutine is guaranteed to be completely stopped with a stable stack at the moment of the copy.

</details>

---

### A8 (Modules 7-8)
Explain why Go's garbage collector must use a write barrier during the concurrent mark phase. Give a concrete example of an object that would be incorrectly freed without it.

<details><summary>Answer</summary>

During concurrent marking, application goroutines (mutators) run simultaneously with GC mark workers. The write barrier is needed to maintain the tri-color invariant: no black object may point to a white object without an intervening grey object.

**Concrete example:**

1. Object A (black, already scanned) has a pointer to object B (grey).
2. Object B has a pointer to object C (white, not yet discovered).
3. The mutator executes: `A.ptr = C; B.ptr = nil` (moves C's only reference from B to A).
4. Without a write barrier: The GC scans B (greying it -> black), but B no longer points to C. A is already black and will not be rescanned. C remains white.
5. Marking finishes: C is white (unreachable by the GC's reckoning) and is freed.
6. **Bug:** A still points to C, which has been freed. A has a dangling pointer.

With the write barrier, step 3 would:
- Grey C when the pointer `A.ptr = C` is written (Dijkstra insertion barrier), ensuring C is discovered; AND/OR
- Grey C when `B.ptr = nil` deletes the reference (Yuasa deletion barrier), preserving the snapshot.

Go's hybrid barrier combines both, allowing stacks to be treated as always-black after initial scanning.

</details>

---

### A9 (Modules 9-10)
A goroutine's stack starts at 2KB and doubles when it needs to grow. Explain the amortized cost argument for why this is efficient. What is the total copy work done for a goroutine that ultimately needs a 64KB stack?

<details><summary>Answer</summary>

The doubling strategy ensures amortized O(1) cost per stack frame push, analogous to dynamic array resizing:

When a 2KB stack is full, a 4KB stack is allocated and 2KB is copied. When 4KB is full, 8KB is allocated and ~4KB is copied. And so on.

**For a goroutine needing 64KB:**

| Growth step | Old size | New size | Bytes copied |
|------------|----------|----------|--------------|
| 1 | 2KB | 4KB | 2KB |
| 2 | 4KB | 8KB | 4KB |
| 3 | 8KB | 16KB | 8KB |
| 4 | 16KB | 32KB | 16KB |
| 5 | 32KB | 64KB | 32KB |

Total bytes copied: 2 + 4 + 8 + 16 + 32 = 62KB. In general, total copy work is bounded by 2 * final_size = 2 * 64KB = 128KB, and indeed 62KB < 128KB.

In general, the total copy work is bounded by 2 * final_size. Since the goroutine uses final_size bytes of stack over its lifetime, the amortized overhead is at most 2x -- a constant factor. Each function call contributes O(1) amortized copying cost.

With additive growth (e.g., +4KB each time), the total copying would be O(n^2), making deep recursion catastrophically expensive.

</details>

---

### A10 (Modules 9-10)
Explain why regular file I/O in Go does not use the network poller (epoll/kqueue), while socket I/O does. What is the scheduling cost of this difference?

<details><summary>Answer</summary>

**Technical reason:** `epoll` and `kqueue` do not support regular files. These multiplexing interfaces are designed for file descriptors that have meaningful "readiness" states (data available, connection accepted, write buffer space). Regular files on local filesystems are always reported as "ready" by epoll -- the kernel always allows the read/write call, even though the actual I/O may block waiting for disk. There is no mechanism for the kernel to notify when a disk read completes asynchronously via epoll.

**Scheduling cost:** Network I/O parks the goroutine in user space (`gopark` via `pollDesc`) -- the M and P remain available for other goroutines. File I/O blocks the entire M in a kernel syscall. The runtime must:
1. Detach the P from the blocked M (via `entersyscall` / `sysmon` retake).
2. Potentially create a new M to run goroutines on the freed P.
3. When the I/O completes, the M must reacquire a P.

This means file I/O is more expensive from a scheduling perspective: it causes M creation (OS thread allocation), P handoffs, and context switches. A server doing 10,000 concurrent file reads might create 10,000 OS threads, whereas 10,000 concurrent socket reads need only GOMAXPROCS threads.

Modern solutions like `io_uring` on Linux could eventually allow file I/O to use a poller-like mechanism.

</details>

---

## Part B: Code Reading (36 points, 12 points each)

For each question, read the provided Go runtime code and answer the questions.

---

### B1: Channel Send with Waiting Receiver

The following is a simplified version of code from `runtime/chan.go` in the `chansend` function. This is the path taken when sending on a channel that has a goroutine waiting to receive.

```go
func chansend(c *hchan, ep unsafe.Pointer, block bool, callerpc uintptr) bool {
	// ... (nil check, fast-path checks omitted) ...

	lock(&c.lock)

	if c.closed != 0 {
		unlock(&c.lock)
		panic(plainError("send on closed channel"))
	}

	if sg := c.recvq.dequeue(); sg != nil {
		// Found a waiting receiver. We pass the value directly to
		// the receiver, bypassing the channel buffer (if any).
		send(c, sg, ep, func() { unlock(&c.lock) }, 3)
		return true
	}

	// ... (buffered path, blocking path omitted) ...
}

func send(c *hchan, sg *sudog, ep unsafe.Pointer, unlockf func(), skip int) {
	if sg.elem != nil {
		sendDirect(c.elemtype, sg, ep)
		sg.elem = nil
	}
	gp := sg.g
	unlockf()
	gp.param = unsafe.Pointer(sg)
	goready(gp, skip+1)
}
```

**(a)** (4 points) Why does `send()` call `sendDirect` before `unlockf()` but call `goready` after `unlockf()`? What would go wrong if these were reordered?

**(b)** (4 points) The function passes `unlockf` as a closure rather than calling `unlock(&c.lock)` directly inside `send()`. What design flexibility does this provide?

**(c)** (4 points) Explain what `gp.param = unsafe.Pointer(sg)` accomplishes. How does the receiver goroutine use this value when it wakes up?

<details><summary>Answer</summary>

**(a)** `sendDirect` copies data to the receiver's stack (`sg.elem`). This must happen while the lock is held because:
- The lock prevents the receiver from being woken by another send or a close operation.
- It ensures `sg.elem` is still valid (pointing to the receiver's stack location).
- If `unlockf()` were called first, another goroutine could modify the channel state or even wake the receiver before the data is copied, leading to a data race or the receiver seeing uninitialized data.

`goready` is called after `unlockf()` because:
- `goready` makes the goroutine runnable, which involves modifying scheduler state. The comment in `hchan` says "Do not change another G's status while holding this lock... as this can deadlock with stack shrinking."
- Holding `c.lock` while calling `goready` could cause lock ordering violations if the woken goroutine immediately tries to operate on the same channel.

**(b)** Passing `unlockf` as a closure allows `send()` to be called from different contexts with different cleanup actions. For example, `selectgo` may need to perform additional cleanup (dequeuing from multiple channels) in the unlock function. The closure pattern lets `send()` be agnostic about what cleanup is needed -- the caller specifies it. It also allows the unlock to happen at precisely the right point in the `send` sequence (after data copy, before `goready`).

**(c)** `gp.param = unsafe.Pointer(sg)` passes the `sudog` pointer to the receiver goroutine as a "return value" from `gopark`. When the receiver was parked in `chanrecv`, it called `gopark` and saved its `sudog`. When it wakes up, it reads `gp.param` to find the `sudog` that completed the operation. This tells the receiver:
- That the send completed successfully (vs. being woken by a channel close, where `sg.success` would be false).
- Which `sudog` was involved (important for `select` statements where a goroutine may have multiple pending `sudog`s).
The receiver checks `sg.success` to determine if it received a valid value or if the channel was closed.

</details>

---

### B2: Work Stealing

The following code is from `runtime/proc.go`:

```go
func stealWork(now int64) (gp *g, inheritTime bool, rnow, pollUntil int64, newWork bool) {
	pp := getg().m.p.ptr()

	ranTimer := false

	const stealTries = 4
	for i := 0; i < stealTries; i++ {
		stealTimersOrRunNextG := i == stealTries-1

		for enum := stealOrder.start(cheaprand()); !enum.done(); enum.next() {
			if sched.gcwaiting.Load() {
				return nil, false, now, pollUntil, true
			}
			p2 := allp[enum.position()]
			if pp == p2 {
				continue
			}

			// ... (timer stealing on last iteration) ...

			// Don't bother to attempt to steal if p2 is idle.
			if !idlepMask.read(enum.position()) {
				if gp := runqsteal(pp, p2, stealTimersOrRunNextG); gp != nil {
					return gp, false, now, pollUntil, newWork
				}
			}
		}
	}
	// ... (return nil if nothing found) ...
}
```

**(a)** (4 points) `stealOrder.start(cheaprand())` returns an enumerator that visits all Ps in a pseudo-random order. Why is this preferable to iterating `allp[0], allp[1], ..., allp[N-1]` in order?

**(b)** (4 points) The code checks `!idlepMask.read(enum.position())` before attempting to steal. Why is it safe to skip idle Ps? Could this check cause the stealer to miss work?

**(c)** (4 points) The function returns `inheritTime = false` for stolen goroutines, unlike goroutines from `runnext` which return `inheritTime = true`. Explain the scheduling consequence of this difference.

<details><summary>Answer</summary>

**(a)** If all stealers iterate in the same order, they all try to steal from P0 first, then P1, etc. This causes:
- **Contention:** Multiple stealers simultaneously CAS on P0's run queue, causing atomic operation retries.
- **Unfair draining:** P0 would lose goroutines disproportionately, while higher-numbered Ps retain them.
- **Correlated behavior:** Stealers would cluster on the same victims, reducing the effective parallelism of the stealing phase.

Random starting positions spread stealers across different victims, minimizing contention and distributing the load of being stolen from more evenly.

**(b)** If a P is idle (`idlepMask` bit is set), it has no M running goroutines on it. An idle P's run queue is expected to be empty (work would have been taken when the P went idle). Skipping it avoids a wasted `runqsteal` call (which involves atomic operations on the victim's queue).

This check COULD theoretically cause a miss: a goroutine could be added to an idle P's run queue (e.g., by `runqput` from another P via timer firing) between the idle check and the steal attempt. However, this is benign because:
- The P going from idle to having work will trigger `wakep()`, which starts a new M.
- The stealer will retry up to 4 times. The idle mask is re-read on each iteration.
- Even if missed, the goroutine will be found on the next scheduling round.

**(c)** `inheritTime = true` means the goroutine inherits the remaining time slice of the previously running goroutine. This is used for `runnext` because that goroutine was readied by the current goroutine (e.g., a channel send waking a receiver) and should run as part of the same "scheduling unit" to preserve locality.

`inheritTime = false` means the goroutine gets a fresh time slice. Stolen goroutines should not inherit time because:
- The stealer's P may have had its time slice mostly consumed by a different goroutine.
- Inheriting a nearly-expired time slice would cause the stolen goroutine to be immediately preempted.
- The stolen goroutine is running on a different P than it was created for, so time slice inheritance has no locality benefit.

A fresh time slice ensures stolen goroutines get fair CPU time on their new P.

</details>

---

### B3: Stack Growth

The following code is from `runtime/stack.go` (simplified):

```go
func newstack() {
	thisg := getg()

	gp := thisg.m.curg

	// ... (preemption check omitted) ...

	oldsize := gp.stack.hi - gp.stack.lo
	newsize := oldsize * 2

	// Make sure we grow at least as much as needed.
	if f := findfunc(gp.sched.pc); f.valid() {
		max := uintptr(funcMaxSPDelta(f))
		needed := max + stackGuard
		used := gp.stack.hi - gp.sched.sp
		for newsize-used < needed {
			newsize *= 2
		}
	}

	if newsize > maxstacksize {
		throw("stack overflow")
	}

	casgstatus(gp, _Grunning, _Gcopystack)
	copystack(gp, newsize)
	casgstatus(gp, _Gcopystack, _Grunning)
}
```

**(a)** (4 points) Why does the function change the goroutine status to `_Gcopystack` during the copy? What would go wrong if it stayed in `_Grunning`?

**(b)** (4 points) The code calculates `needed` based on `funcMaxSPDelta` and may grow by more than 2x. Under what circumstances would doubling not be sufficient?

**(c)** (4 points) After `copystack` returns, all pointers within the stack have been adjusted. But what about pointers from the heap into the stack (e.g., a heap object containing a pointer to a stack-allocated variable)? How does Go handle this?

<details><summary>Answer</summary>

**(a)** The `_Gcopystack` status serves as a synchronization signal to other parts of the runtime:

- **GC safety:** The garbage collector checks goroutine status before scanning stacks. If a goroutine is in `_Gcopystack`, the GC knows the stack is being moved and must not scan it (the pointers are being adjusted and would be inconsistent).
- **Stack shrinking:** The GC's `shrinkstack` also checks status and skips goroutines in `_Gcopystack`.
- **Channel operations:** `sendDirect`/`recvDirect` write to another goroutine's stack. If that goroutine's stack is being copied, the write would go to the old (soon-to-be-freed) memory. The `_Gcopystack` status prevents concurrent channel operations from writing to the stack.

If the goroutine stayed in `_Grunning`, the GC or channel operations could access the stack during the copy, reading inconsistent pointer values or writing to the wrong memory location.

**(b)** Doubling may not be sufficient when a function has a very large stack frame. `funcMaxSPDelta` returns the maximum stack pointer delta for the function at `gp.sched.pc` -- this is the stack space the function will need. If the function has a local variable like `var buf [65536]byte`, it needs 64KB of stack space in a single frame.

If the current stack is 4KB and the function needs 64KB, doubling to 8KB is insufficient. The `for` loop keeps doubling (`8KB -> 16KB -> 32KB -> 64KB -> 128KB`) until `newsize - used >= needed`. This ensures the new stack can accommodate the function that triggered the growth, even if it has an unusually large frame.

**(c)** Go does NOT allow pointers from the heap into the stack. This is a key invariant enforced by the compiler's escape analysis:

- If a variable's address escapes (is stored in a heap object, returned from a function, captured by a goroutine), the compiler allocates it on the heap instead of the stack.
- The escape analysis rule ensures that stack-allocated variables are only referenced by stack frames above them in the call chain (which are also on the same stack and will be adjusted by `copystack`).

This invariant is what makes stack copying feasible. If heap-to-stack pointers existed, `copystack` would need to find and update every heap object pointing into the stack, which would be prohibitively expensive (it would require scanning the entire heap).

The one exception is `sudog.elem` in channel operations, which is explicitly handled by `adjustsudogs` during stack copy.

</details>

---

## Part C: Design Questions (24 points, 12 points each)

Answer each question in approximately one page (300-500 words). Credit will be given for clarity of reasoning, identification of tradeoffs, and supporting evidence from the Go runtime.

---

### C1: Alternative Memory Allocator Design

Go's current memory allocator uses a three-level hierarchy: `mcache` (per-P, lock-free) -> `mcentral` (per-size-class, locked) -> `mheap` (global, locked). A colleague proposes replacing this with a simpler two-level design:

- **Level 1:** Per-P free lists for each size class (similar to `mcache` but with more capacity -- each P caches up to 1MB of free objects per size class).
- **Level 2:** A global allocator that uses `mmap` directly whenever a per-P cache is empty.

Analyze this proposal. Consider:
- Performance on allocation-heavy workloads with many goroutines.
- Memory overhead and waste when some Ps are busy with one size class and others are idle.
- Interaction with the garbage collector's sweep phase.
- What problems does `mcentral` solve that this design would reintroduce?

<details><summary>Answer</summary>

**Performance on allocation-heavy workloads:** The fast path (allocating from the per-P cache) would be similar in performance to the current design -- no lock contention. However, when a P's cache is empty, it must call `mmap`, which is a system call (thousands of nanoseconds) compared to `mcentral`'s lock acquisition (tens of nanoseconds in the uncontended case). Under allocation-heavy workloads, cache misses would cause frequent `mmap` calls, which are orders of magnitude slower than getting a span from `mcentral`. The current `mcentral` acts as a fast "refill" mechanism that avoids syscalls.

**Memory overhead and waste:** With 1MB per size class per P, and ~70 size classes, each P would cache up to 70MB. With `GOMAXPROCS=64`, that is 4.5GB of cached free objects. Most of this would be wasted because real programs use only a few size classes heavily. The current `mcache` keeps only one active span per size class (typically 8-64KB), totaling ~1-4MB per P. The `mcentral` acts as a shared pool that redistributes spans between Ps based on demand -- if P1 needs lots of 48-byte objects and P2 needs 128-byte objects, `mcentral` directs spans accordingly.

**GC interaction:** The sweep phase marks spans as free. Currently, swept spans return to `mcentral`, where any P can claim them. With the proposed design, swept memory would need to go directly to a specific P's cache or to the global allocator. If it goes to a specific P, that P may not need it (wasting memory). If it goes to the global allocator, subsequent allocations of that size class would require `mmap`-level operations to reclaim it.

**What `mcentral` solves:** It provides **cross-P memory redistribution** without syscalls. When P1 frees many 32-byte objects and P2 needs them, `mcentral` facilitates this transfer with just a lock acquisition. Without it, P1's freed memory would either sit in P1's oversized cache (waste) or be returned via `munmap` and re-obtained via `mmap` for P2 (expensive). `mcentral` also provides natural **flow control**: when a P's span is exhausted, it gets another span (not individual objects), amortizing the lock acquisition cost over hundreds of allocations.

The three-level design exists precisely because the extremes (fully local = memory waste, fully global = lock contention) are both bad. `mcentral` provides a balanced middle ground.

</details>

---

### C2: Channel vs. Shared Memory for Concurrency

Go's motto "Do not communicate by sharing memory; instead, share memory by communicating" promotes channels as the primary concurrency primitive. However, the standard library also provides `sync.Mutex`, `sync.RWMutex`, `sync.Map`, and `sync/atomic`.

Analyze the tradeoffs between channels and shared-memory primitives for the following scenarios, using your knowledge of their internal implementations:

1. **High-frequency counter** (millions of increments per second from many goroutines).
2. **Request routing** (distributing incoming requests to a pool of worker goroutines).
3. **Cache with read-heavy workload** (95% reads, 5% writes).

For each scenario, recommend the best primitive and justify your choice based on implementation-level costs (lock acquisition, goroutine parking, memory allocation, cache coherence).

<details><summary>Answer</summary>

**1. High-frequency counter:**

**Recommendation: `sync/atomic.AddInt64`**

Channels are the worst choice here. Each channel send involves: acquiring `c.lock`, checking for receivers, copying the value to the buffer or directly, potentially calling `goready`. This is ~100-200ns per operation. A mutex-protected counter requires `sync.Mutex.Lock()` (futex-based semaphore, ~50-100ns under contention), increment, unlock.

`atomic.AddInt64` is a single `LOCK XADD` instruction (~5-20ns), with no goroutine parking, no lock acquisition, and no memory allocation. For millions of increments per second, the 10-40x performance difference is decisive.

**2. Request routing to worker pool:**

**Recommendation: Buffered channel**

This is the ideal channel use case. A buffered `chan *Request` serves as a work queue. Implementation costs are well-amortized:
- Producers `Send` to the channel (~50-100ns with buffer space available: lock, enqueue, unlock).
- Workers `Recv` from the channel, parking when idle (no CPU consumption while waiting).
- The channel's `runnext` optimization ensures a worker that just finished a request (and is likely "warm" with request-handling code in cache) gets the next request quickly.

Alternatives like a `sync.Mutex`-protected queue require busy-polling or `sync.Cond` signaling, which channels already encapsulate more elegantly. The channel also provides natural backpressure (senders block when the buffer is full).

**3. Read-heavy cache:**

**Recommendation: `sync.RWMutex` protecting a `map`**

With 95% reads, `sync.RWMutex` excels: `RLock` allows concurrent readers, so read throughput scales linearly with cores. Each `RLock`/`RUnlock` is an atomic add/subtract on `readerCount` (~10-20ns), with no goroutine parking needed in the common case.

Using a channel for a cache would require a "cache server" goroutine pattern: all reads and writes are sent as messages to a single goroutine that owns the map. This serializes ALL operations (reads and writes) through one goroutine, destroying read parallelism. A read that would take 20ns with `RWMutex` would take 200+ns through a channel (send request + receive response, with goroutine scheduling overhead).

`sync.Map` is another option, optimized for the case where keys are stable (few writes). It uses a read-only map with atomic access (zero contention for reads) and a dirty map for writes. For truly read-heavy workloads with stable key sets, `sync.Map` can outperform `RWMutex` by avoiding even the atomic increment of `readerCount`. However, for workloads with non-trivial write rates, `sync.Map`'s promotion cost (copying dirty map to read-only map) can cause latency spikes.

**Summary:** The choice depends on the communication pattern. Channels excel when goroutines need to coordinate sequentially (pipelines, fan-out/fan-in). Shared memory primitives excel when the operation is simple (counter), read-dominated (cache), or when goroutine scheduling overhead would dominate the actual work.

</details>

---

*End of Exam*
