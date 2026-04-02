# Module 4: The Go Scheduler (GMP Model, schedule(), findRunnable())

## Comprehension Check

### Question 1 (Code Reading)
Study this excerpt from `schedule()` in `runtime/proc.go`:

```go
func schedule() {
	mp := getg().m

	if mp.locks != 0 {
		throw("schedule: holding locks")
	}

	if mp.lockedg != 0 {
		stoplockedm()
		execute(mp.lockedg.ptr(), false) // Never returns.
	}

top:
	pp := mp.p.ptr()
	pp.preempt = false

	if mp.spinning && (pp.runnext != 0 || pp.runqhead != pp.runqtail) {
		throw("schedule: spinning with local work")
	}

	gp, inheritTime, tryWakeP := findRunnable() // blocks until work is available
```

(a) Why does `schedule()` panic if `mp.locks != 0`?
(b) What does the `mp.lockedg` check accomplish?
(c) Why is it an error to be spinning with local work?

<details><summary>Answer</summary>

**(a)** If `mp.locks != 0`, the M is holding runtime locks. Calling `schedule()` could cause a deadlock because the scheduler might park the M or switch to a different goroutine that tries to acquire the same lock. The check is a safety invariant: you must release all locks before entering the scheduler.

**(b)** `mp.lockedg` is set when a goroutine has called `runtime.LockOSThread()`. If the M has a locked goroutine, it cannot schedule any other goroutine. `stoplockedm()` parks the M until its locked goroutine is runnable, then `execute()` runs it directly, bypassing the normal scheduling path. This ensures the goroutine always runs on its designated OS thread.

**(c)** An M is "spinning" when it has no local work and is actively searching for work (checking other Ps' queues, the global queue, the network poller). If the M has local work (`runnext` is set or the local run queue is non-empty), it should not be marked as spinning -- it should be running that work. This invariant prevents the scheduler from wastefully searching for work while ignoring work it already has, and it ensures the spinning thread count (`sched.nmspinning`) accurately reflects how many threads are genuinely idle.

</details>

---

### Question 2 (Short Answer)
The `findRunnable()` function checks for work in a specific order. List at least five sources of work it checks, and explain why the ordering matters.

<details><summary>Answer</summary>

`findRunnable()` checks these sources (in order):

1. **GC work**: If GC is in a STW phase (`sched.gcwaiting`), stop the M.
2. **Trace reader**: If tracing is enabled, check for trace reader goroutines.
3. **GC marking work**: If GC is in the mark phase, run GC mark workers.
4. **Local run queue** (`pp.runq` and `pp.runnext`): Check the P's own run queue first.
5. **Global run queue** (`sched.runq`): Check every 61 scheduling ticks to prevent starvation.
6. **Network poller** (`netpoll`): Poll for ready network I/O goroutines.
7. **Work stealing** (`stealWork`): Try to steal from other Ps' run queues and timer heaps.

**Why ordering matters:**
- **GC first**: GC stop-the-world phases must be respected immediately for correctness.
- **Local queue before global**: Local queue access requires no lock and preserves cache locality. Checking it first is the fast path.
- **Global queue periodically**: The "every 61 ticks" check prevents goroutines on the global queue from being starved indefinitely by locally-produced work. The prime number 61 is chosen to avoid synchronization patterns.
- **Network poller before stealing**: Network-ready goroutines are "free" work that does not take from another P.
- **Stealing last**: Stealing is the most expensive option (requires accessing another P's queue with atomics) and is the fallback when no local work exists.

</details>

---

### Question 3 (True/False with Explanation)
**True or False:** The global run queue is checked on every call to `schedule()`.

<details><summary>Answer</summary>

**False.** The global run queue is checked approximately every 61st scheduling tick. In `findRunnable()`, the code checks:

```go
if pp.schedtick%61 == 0 && sched.runqsize > 0 {
    ...
}
```

This is a deliberate tradeoff: checking the global queue requires acquiring `sched.lock`, which is contended. By checking only periodically, the scheduler avoids lock contention on the fast path. The value 61 is prime to avoid patterns where multiple Ps check at the same time. This means goroutines on the global queue may wait up to 61 scheduling decisions before being picked up, but it prevents global queue starvation.

</details>

---

### Question 4 (What Would Happen If...)
The `p` struct has a `runnext` field described as:

```go
// runnext, if non-nil, is a runnable G that was ready'd by
// the current G and should be run next instead of what's in
// runq if there's time remaining in the running G's time
// slice. It will inherit the time left in the current time
// slice.
```

What would happen to the performance of a producer-consumer pattern (one goroutine sending values on a channel, another receiving) if `runnext` were removed?

<details><summary>Answer</summary>

Without `runnext`, a producer-consumer pair would experience significantly higher latency:

1. **Current behavior with `runnext`:** When the producer sends on a channel and wakes the consumer, the consumer is placed in `runnext`. On the next scheduling decision, the consumer runs immediately on the same P, inheriting the remaining time slice. This creates a tight ping-pong between producer and consumer on the same CPU, preserving cache locality.

2. **Without `runnext`:** The woken consumer would be appended to the end of the P's run queue. If there are N other runnable goroutines in the queue, the consumer would wait for all N to run before getting its turn. This adds O(N) scheduling latency to each communication round.

3. **Cache effects:** With `runnext`, the producer and consumer share CPU caches (the channel data, shared state). Without it, the consumer might be stolen by another P and run on a different CPU, causing cache misses.

4. **Throughput impact:** For tight communicate-and-wait patterns (which are extremely common in Go), throughput could drop by 2-10x depending on run queue depth and GOMAXPROCS.

`runnext` effectively implements a form of "affinity scheduling" for communicating goroutine pairs.

</details>

---

### Question 5 (Short Answer)
Explain the role of `sysmon` in the scheduler. What specific problems does it solve that the normal scheduling loop cannot?

<details><summary>Answer</summary>

`sysmon` is a special system monitoring goroutine that runs on a dedicated M without a P. It runs in an infinite loop, sleeping for increasing intervals (20us to 10ms). Its responsibilities include:

1. **Retaking Ps from syscalls:** If a goroutine has been in a syscall for >10ms, `sysmon` retakes its P via `retake()` and hands it to another M. Without this, Ps could be stuck on blocked syscalls indefinitely.

2. **Preempting long-running goroutines:** If a goroutine has been running for >10ms without yielding, `sysmon` sets its `stackguard0` to `stackPreempt`, forcing a preemption check at the next function call. (In newer Go, it also sends SIGURG for async preemption.)

3. **Network poller deadline injection:** `sysmon` calls `netpoll(0)` (non-blocking) to check for ready I/O, ensuring network-blocked goroutines are woken even if no other scheduling activity is happening.

4. **Forced GC:** If no GC has run for 2 minutes, `sysmon` triggers one.

5. **Scavenging memory:** It coordinates returning unused memory to the OS.

**Why the normal scheduling loop cannot do this:** The scheduling loop only runs when a P is making scheduling decisions. If all Ps are occupied (running compute-bound goroutines or stuck in syscalls), there is no scheduling activity to detect stalls. `sysmon` runs independently and can intervene from outside.

</details>

---

### Question 6 (Code Reading)
Consider the `execute()` function's signature and a key operation it performs:

```go
func execute(gp *g, inheritTime bool) {
	mp := getg().m
	...
	mp.curg = gp
	gp.m = mp
	casgstatus(gp, _Grunnable, _Grunning)
	gp.waitsince = 0
	gp.preempt = false
	gp.stackguard0 = gp.stack.lo + stackGuard
	...
	gogo(&gp.sched)
}
```

What is the purpose of the `inheritTime` parameter? Why does the function reset `stackguard0` to `gp.stack.lo + stackGuard`?

<details><summary>Answer</summary>

**`inheritTime`:** When `true`, the goroutine inherits the remaining time slice from the previously running goroutine (this happens when a goroutine comes from `runnext`). When `false`, the goroutine gets a fresh time slice. This is tracked to determine when a goroutine has exhausted its time quantum and should check `schedtick` for preemption. If every goroutine inherited time, tight `runnext` chains could starve other goroutines in the run queue; if none did, the scheduling overhead for communicating pairs would increase.

**Resetting `stackguard0`:** The `stackguard0` field is compared against the stack pointer in every function prologue to check for stack overflow. During preemption, `stackguard0` is set to the special value `stackPreempt` (a very large value that always triggers the check). When a goroutine is about to resume execution, `stackguard0` must be reset to its normal value (`stack.lo + stackGuard`) so that the stack check works correctly for detecting actual stack growth needs, rather than perpetually triggering preemption.

</details>

---

### Question 7 (Short Answer)
Why does the Go scheduler use a fixed-size local run queue (256 entries in `p.runq`) rather than a dynamically-sized slice?

<details><summary>Answer</summary>

The fixed-size array serves several purposes:

1. **Lock-free access:** The local run queue is implemented as a lock-free circular buffer using atomic operations on `runqhead` and `runqtail`. A fixed-size array makes this possible since there is no need for reallocation (which would require synchronization). A dynamically-sized slice would need a lock for resizing.

2. **Bounded memory:** Each P's memory footprint is predictable and bounded. With 256 entries of `guintptr` (8 bytes each), each run queue uses exactly 2KB.

3. **Overflow to global queue:** When the local queue is full, excess goroutines are moved to the global run queue (via `runqput` -> `runqputslow`, which moves half the local queue to the global queue). This provides natural load balancing and ensures no P hoards too many goroutines.

4. **Cache efficiency:** A fixed-size array has predictable memory layout and fits in a few cache lines, improving the performance of the scheduling hot path.

</details>
