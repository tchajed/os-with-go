# Module 5: Work Stealing and Preemption

## Comprehension Check

### Question 1 (Code Reading)
Study the `stealWork` function from `runtime/proc.go`:

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
			...
```

(a) Why does the function try to steal 4 times rather than just once?
(b) Why is the iteration order randomized with `cheaprand()`?
(c) Why does it only steal timers and `runnext` on the last iteration (`stealTimersOrRunNextG`)?

<details><summary>Answer</summary>

**(a)** Multiple passes increase the probability of finding work. Other Ps are concurrently producing and consuming goroutines. A single pass might miss work due to race conditions: a goroutine might be added to a P's queue just after the stealer checked it. Four attempts give a reasonable probability of finding newly available work without being overly expensive.

**(b)** Randomization prevents systematic bias. If all idle Ps always started scanning from P0, they would all contend on the same victim's run queue. By randomizing the start position, stealers spread out across victims, reducing contention. `cheaprand()` is used rather than a full PRNG because it is faster and cryptographic quality is not needed.

**(c)** Stealing `runnext` and timers is more aggressive and potentially more disruptive:
- **`runnext`** represents a goroutine that the victim P's current goroutine just readied, likely for a tight communication pattern. Stealing it breaks locality and hurts the victim's performance. It should only be done as a last resort.
- **Timers** belong to a specific P and stealing them requires acquiring the timer lock on the remote P. This is expensive and only worthwhile if no regular work was found in earlier passes.

By deferring these to the last iteration, the scheduler prefers to steal regular run queue entries first.

</details>

---

### Question 2 (Short Answer)
Explain the "spinning thread" mechanism. What is a spinning thread, and why does the scheduler limit the number of spinning threads?

<details><summary>Answer</summary>

A **spinning thread** is an M that has no local work and is actively searching for work by checking other Ps' run queues, the global queue, and the network poller. Its `m.spinning` flag is set to `true`, and `sched.nmspinning` tracks the global count.

**Purpose:** Spinning threads are ready to immediately pick up new work without the latency of being unparked from sleep. They act as a buffer of ready workers.

**Why limit them:** The scheduler maintains the invariant that at most one thread should be spinning at a time (more precisely, it avoids unparking new threads when there is already a spinning thread). The rule in `wakep()` is:

> Unpark a new thread only if there is an idle P AND there are no spinning threads.

This prevents **thread thrashing**: if the scheduler unparked a thread for every new goroutine, threads would frequently be unparked only to find no work and immediately park again. This wastes CPU (the unpark/park cycle costs thousands of cycles) and power.

**Compensation mechanism:** When the last spinning thread finds work and stops spinning, it must call `wakep()` to potentially unpark a new spinning thread. This ensures at least one thread is always ready to find new work.

</details>

---

### Question 3 (True/False with Explanation)
**True or False:** The Go scheduler's cooperative preemption (checking `stackguard0` at function prologues) guarantees that a goroutine cannot monopolize a CPU for more than 10ms.

<details><summary>Answer</summary>

**False.** Cooperative preemption only triggers at function call sites. A goroutine running a tight loop without function calls (e.g., `for { i++ }`) will never hit a preemption check and can monopolize the CPU indefinitely.

This was a real problem in Go before version 1.14. The solution was **asynchronous preemption**: the `sysmon` thread sends a `SIGURG` signal to the M running the long-running goroutine. The signal handler injects a preemption point by modifying the goroutine's saved state, causing it to yield at a safe point.

However, even async preemption has limitations:
- The goroutine must be at a point where its stack can be safely scanned (an "async-safe point")
- Certain runtime-internal code paths disable preemption (`mp.locks > 0`)
- The signal delivery itself has some latency

So while async preemption greatly reduces the window, it does not provide hard real-time guarantees of exactly 10ms response time.

</details>

---

### Question 4 (What Would Happen If...)
What would happen if work stealing always stole the entire run queue of the victim P instead of stealing half?

<details><summary>Answer</summary>

Stealing the entire queue would cause several problems:

1. **Thrashing:** If P1 steals all of P2's work, P2 would immediately become idle and start stealing -- possibly from P1. This ping-pong effect would waste CPU on stealing overhead rather than executing goroutines.

2. **Loss of locality:** The goroutines in P2's queue likely have cache affinity for P2's CPU. Moving all of them at once causes more cache misses than moving half.

3. **Unfairness:** The victim P's current goroutine may be producing work for its local queue (e.g., spawning goroutines). Stealing everything leaves the victim unable to schedule any of its own recently-produced work.

4. **Contention:** Stealing the full queue takes longer (more atomic operations, more memory copies), increasing the window for contention with the victim.

**Stealing half** is a well-studied strategy from the work-stealing literature (Cilk, etc.). It provides a good balance: the thief gets enough work to stay busy for a while, and the victim retains enough work to continue without immediately needing to steal from others. The `runqgrab` function implements this by calculating `n = victim_queue_size / 2`.

</details>

---

### Question 5 (Code Reading)
Consider the `runqsteal` function:

```go
func runqsteal(pp, p2 *p, stealRunNextG bool) *g {
	t := pp.runqtail
	n := runqgrab(p2, &pp.runq, t, stealRunNextG)
	if n == 0 {
		return nil
	}
	n--
	gp := pp.runq[(t+n)%uint32(len(pp.runq))].ptr()
	if n == 0 {
		return gp
	}
	h := atomic.LoadAcq(&pp.runqhead)
	if t-h+n >= uint32(len(pp.runq)) {
		throw("runqsteal: runq overflow")
	}
	atomic.StoreRel(&pp.runqtail, t+n)
	return gp
}
```

Why does this function use `atomic.LoadAcq` and `atomic.StoreRel`? What memory ordering guarantee do these provide that a plain load/store would not?

<details><summary>Answer</summary>

The local run queue is a lock-free single-producer, multi-consumer circular buffer:
- The owning P writes to `runqtail` (producer)
- Other Ps read from `runqhead` (consumers, via stealing)

**`atomic.LoadAcq(&pp.runqhead)`** (load-acquire): Ensures that all memory writes visible to the thread that last modified `runqhead` are also visible to the current thread. This guarantees that after reading `runqhead`, we see the correct state of any goroutines that were consumed (removed from the queue). Without acquire semantics, the CPU might reorder the load before prior stores, leading to reading stale queue entries.

**`atomic.StoreRel(&pp.runqtail, t+n)`** (store-release): Ensures that all the writes to `pp.runq` elements (placing the stolen goroutines into the thief's queue) are visible to other threads before the updated `runqtail` becomes visible. Without release semantics, another thread might see the new tail but read uninitialized or stale goroutine pointers from the queue.

Together, acquire-release semantics provide the minimum synchronization needed for the lock-free queue to operate correctly without using a full memory barrier (which would be more expensive) or a mutex.

</details>

---

### Question 6 (Short Answer)
Explain how SIGURG-based asynchronous preemption works in Go. Why was SIGURG chosen as the signal?

<details><summary>Answer</summary>

**Mechanism:**
1. `sysmon` detects a goroutine running for >10ms on a P (via `retake()`).
2. It calls `preemptone(pp)`, which sets `gp.preempt = true` and `gp.stackguard0 = stackPreempt`.
3. If cooperative preemption is insufficient (no function calls), it calls `signalM(mp, sigPreempt)` which sends `SIGURG` to the M's OS thread.
4. The signal handler (`sighandler`) runs `doSigPreempt`, which:
   - Checks if the goroutine is at an async-safe point (a point where the stack map is available for GC)
   - If safe, injects a call to `asyncPreempt` by modifying the signal context's PC
5. `asyncPreempt` (an assembly function) saves all registers and calls `asyncPreempt2`, which calls `gopreempt_m`, which puts the goroutine back into the run queue.

**Why SIGURG:**
- It is one of the few signals not commonly used by applications or the system.
- Sending SIGURG to a process does not have visible side effects (unlike SIGSEGV, SIGSTOP, etc.).
- It does not terminate the process by default (its default disposition is "ignore").
- It can be sent to a specific thread via `tgkill` on Linux.
- Other candidates (SIGUSR1, SIGUSR2) are commonly used by application code and libraries, so using them would conflict. SIGURG is traditionally used only for TCP out-of-band data, which is extremely rare in practice.

</details>

---

### Question 7 (Short Answer)
What is the `retake()` function's role, and what are its two main responsibilities?

<details><summary>Answer</summary>

`retake()` is called by `sysmon` and iterates over all Ps to handle two situations:

1. **Preempting long-running goroutines (`_Prunning`):** If a P has been in the running state for more than one `sysmon` tick (~10ms), `retake()` sets `gp.stackguard0 = stackPreempt` on the running goroutine to trigger cooperative preemption, and sends SIGURG for async preemption.

2. **Retaking Ps from syscalls (`_Psyscall`):** If a P's M has been in a syscall for >10ms (and there is work to do or idle Ps available), `retake()` calls `handoffp(pp)` to take the P away from the syscall-blocked M and either:
   - Starts a new M to run goroutines from the P's queue
   - Puts the P on the idle list if its queue is empty

Without `retake()`, a single long-running goroutine or blocking syscall could monopolize a P indefinitely, starving other goroutines.

</details>
