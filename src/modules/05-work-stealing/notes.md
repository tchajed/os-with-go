# Module 5: Work Stealing and Preemption

## Learning Objectives

By the end of this module, students will understand:
- Why distributed scheduling outperforms centralized run queues
- How Go's work-stealing scheduler balances load across processors
- The spinning thread concept and its role in avoiding unnecessary thread wake-ups
- How the global run queue provides fairness and overflow handling
- The dual preemption strategy: cooperative (stack checks) and asynchronous (signals)
- The sysmon background thread and its role in enforcing preemption and syscall hand-off

---

## 1. Why Distributed Scheduling? (10 min)

### The Problem with Centralized Run Queues

Traditional OS schedulers and early Go (before Go 1.1) used a single global run queue protected by a mutex. Every time a goroutine became runnable or a thread needed work, it had to acquire this lock. On a 64-core machine, this single lock becomes a devastating bottleneck.

The Go scheduler explicitly rejected centralization. From the top-of-file comment in `proc.go`:

```go
// src/runtime/proc.go, lines 36-56
//
// Worker thread parking/unparking.
// We need to balance between keeping enough running worker threads to utilize
// available hardware parallelism and parking excessive running worker threads
// to conserve CPU resources and power. This is not simple for two reasons:
// (1) scheduler state is intentionally distributed (in particular, per-P work
// queues), so it is not possible to compute global predicates on fast paths;
// (2) for optimal thread management we would need to know the future (don't park
// a worker thread when a new goroutine will be readied in near future).
//
// Three rejected approaches that would work badly:
// 1. Centralize all scheduler state (would inhibit scalability).
// 2. Direct goroutine handoff. That is, when we ready a new goroutine and there
//    is a spare P, unpark a thread and handoff it the thread and the goroutine.
//    This would lead to thread state thrashing, as the thread that readied the
//    goroutine can be out of work the very next moment, we will need to park it.
//    Also, it would destroy locality of computation as we want to preserve
//    dependent goroutines on the same thread; and introduce additional latency.
// 3. Unpark an additional thread whenever we ready a goroutine and there is an
//    idle P, but don't do handoff. This would lead to excessive thread parking/
//    unparking as the additional threads will instantly park without discovering
//    any work to do.
```

**Key insight**: The scheduler designers rejected three seemingly reasonable approaches. Centralization kills scalability. Direct handoff causes thrashing. Eager unparking wastes energy. The solution is work stealing with spinning threads.

### The GMP Model Recap

Recall from Module 4:
- **G** (goroutine): the unit of work
- **M** (machine): an OS thread
- **P** (processor): a scheduling context with a local run queue

Each P has a local run queue (a 256-element ring buffer). Work is distributed across these queues. When a P runs out of work, it *steals* from others.

---

## 2. The Work Stealing Algorithm (15 min)

### stealWork: The Top-Level Stealer

When `findRunnable()` can't find work locally, it calls `stealWork()`. This function makes up to 4 passes over all Ps in a random order, trying to steal goroutines:

```go
// src/runtime/proc.go, lines 3828-3895
func stealWork(now int64) (gp *g, inheritTime bool, rnow, pollUntil int64, newWork bool) {
    pp := getg().m.p.ptr()

    ranTimer := false

    const stealTries = 4
    for i := 0; i < stealTries; i++ {
        stealTimersOrRunNextG := i == stealTries-1

        for enum := stealOrder.start(cheaprand()); !enum.done(); enum.next() {
            if sched.gcwaiting.Load() {
                // GC work may be available.
                return nil, false, now, pollUntil, true
            }
            p2 := allp[enum.position()]
            if pp == p2 {
                continue
            }

            // ...timer stealing on last pass...

            // Don't bother to attempt to steal if p2 is idle.
            if !idlepMask.read(enum.position()) {
                if gp := runqsteal(pp, p2, stealTimersOrRunNextG); gp != nil {
                    return gp, false, now, pollUntil, ranTimer
                }
            }
        }
    }

    // No goroutines found to steal.
    return nil, false, now, pollUntil, ranTimer
}
```

**Design decisions to note:**

1. **Random start order** (`stealOrder.start(cheaprand())`): Prevents all idle Ps from hammering the same victim. The enumeration visits all Ps in a pseudo-random permutation.

2. **4 attempts** (`stealTries = 4`): Stealing is racy -- a victim's queue might be momentarily empty even if it's about to receive work. Multiple passes increase the chance of finding work.

3. **Timer stealing only on the last pass** (`stealTimersOrRunNextG`): Timers are expensive to steal (requires locking another P's timer heap). The algorithm tries cheaper steals first.

4. **Skip idle Ps** (`!idlepMask.read(enum.position())`): No point stealing from Ps that have no work either.

### runqsteal and runqgrab: Steal Half

The actual stealing is a two-step process. `runqsteal` calls `runqgrab` to grab half the victim's goroutines, then returns one to run immediately:

```go
// src/runtime/proc.go, lines 7727-7747
// Steal half of elements from local runnable queue of p2
// and put onto local runnable queue of p.
// Returns one of the stolen elements (or nil if failed).
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
    h := atomic.LoadAcq(&pp.runqhead) // load-acquire, synchronize with consumers
    if t-h+n >= uint32(len(pp.runq)) {
        throw("runqsteal: runq overflow")
    }
    atomic.StoreRel(&pp.runqtail, t+n) // store-release, makes the item available for consumption
    return gp
}
```

The `runqgrab` function is the lock-free core. It steals `n - n/2` goroutines (i.e., half, rounding up for the thief):

```go
// src/runtime/proc.go, lines 7662-7725
func runqgrab(pp *p, batch *[256]guintptr, batchHead uint32, stealRunNextG bool) uint32 {
    for {
        h := atomic.LoadAcq(&pp.runqhead) // load-acquire, synchronize with other consumers
        t := atomic.LoadAcq(&pp.runqtail) // load-acquire, synchronize with the producer
        n := t - h
        n = n - n/2
        if n == 0 {
            if stealRunNextG {
                // Try to steal from pp.runnext.
                if next := pp.runnext; next != 0 {
                    // ...carefully steal runnext with back-off...
                    if !pp.runnext.cas(next, 0) {
                        continue
                    }
                    batch[batchHead%uint32(len(batch))] = next
                    return 1
                }
            }
            return 0
        }
        if n > uint32(len(pp.runq)/2) { // read inconsistent h and t
            continue
        }
        for i := uint32(0); i < n; i++ {
            g := pp.runq[(h+i)%uint32(len(pp.runq))]
            batch[(batchHead+i)%uint32(len(batch))] = g
        }
        if atomic.CasRel(&pp.runqhead, h, h+n) { // cas-release, commits consume
            return n
        }
    }
}
```

**Why steal half?** This is the classic work-stealing result from Blumofe and Leiserson (1999). If you steal only one goroutine, you might need to steal again immediately. If you steal all, you over-correct. Stealing half balances the queues in O(log n) steals.

**Lock-free protocol**: Note there are no mutex locks. The algorithm uses:
- `atomic.LoadAcq` for reading head/tail (load-acquire semantics)
- `atomic.CasRel` for committing the steal (compare-and-swap with release semantics)
- Retry loop if the CAS fails (another thread stole concurrently)

### The runnext Slot

Each P has a special `runnext` field -- a single goroutine slot for the next G to run. When a goroutine wakes another goroutine (e.g., via channel send), the awakened G goes into `runnext` rather than the run queue. This preserves locality: the producer and consumer likely share cache lines.

Stealing from `runnext` is the last resort (only on the final steal attempt). The code even includes a `usleep(3)` back-off to avoid thrashing when a goroutine has just placed something in `runnext` and is about to use it (lines 7694-7701).

---

## 3. Spinning Threads (10 min)

### The Concept

A "spinning" thread is an M that has no work but is actively looking for some. It has not yet gone to sleep. Spinning threads are a controlled form of busy-waiting that trades CPU cycles for reduced latency.

```go
// src/runtime/proc.go, lines 68-83
// A worker thread is considered spinning if it is out of local work and did
// not find work in the global run queue or netpoller; the spinning state is
// denoted in m.spinning and in sched.nmspinning. Threads unparked this way are
// also considered spinning; we don't do goroutine handoff so such threads are
// out of work initially. Spinning threads spin on looking for work in per-P
// run queues and timer heaps or from the GC before parking. If a spinning
// thread finds work it takes itself out of the spinning state and proceeds to
// execution. If it does not find work it takes itself out of the spinning
// state and then parks.
//
// If there is at least one spinning thread (sched.nmspinning>1), we don't
// unpark new threads when submitting work. To compensate for that, if the last
// spinning thread finds work and stops spinning, it must unpark a new spinning
// thread. This approach smooths out unjustified spikes of thread unparking,
// but at the same time guarantees eventual maximal CPU parallelism
// utilization.
```

**The invariant**: If there is at least one spinning thread, we don't wake new threads when work arrives. This prevents thundering herd -- imagine 1000 goroutines becoming runnable at once; without this, we'd wake 1000 threads.

### The Wake-up Policy: wakep()

The wake-up decision (in `wakep()`) is:
1. There is an idle P available, **AND**
2. There are no spinning worker threads (`sched.nmspinning == 0`)

If both conditions hold, wake one thread. That thread becomes spinning, searches for work, and if it finds some, it calls `resetspinning()` which calls `wakep()` again -- potentially waking another thread. This creates a cascade that ramps up parallelism smoothly.

```go
// src/runtime/proc.go, lines 4021-4035
func resetspinning() {
    gp := getg()
    if !gp.m.spinning {
        throw("resetspinning: not a spinning m")
    }
    gp.m.spinning = false
    nmspinning := sched.nmspinning.Add(-1)
    if nmspinning < 0 {
        throw("findRunnable: negative nmspinning")
    }
    // M wakeup policy is deliberately somewhat conservative, so check if we
    // need to wakeup another P here. See "Worker thread parking/unparking"
    // comment at the top of the file for details.
    wakep()
}
```

### The Delicate Dance: Spinning to Non-Spinning Transition

The transition from spinning to non-spinning is the trickiest part of the scheduler. There is a race between a thread submitting new work and a spinning thread deciding to park:

```go
// src/runtime/proc.go, lines 3622-3657
// Delicate dance: thread transitions from spinning to non-spinning
// state, potentially concurrently with submission of new work. We must
// drop nmspinning first and then check all sources again (with
// #StoreLoad memory barrier in between). If we do it the other way
// around, another thread can submit work after we've checked all
// sources but before we drop nmspinning; as a result nobody will
// unpark a thread to run the work.
//
// This applies to the following sources of work:
//
// * Goroutines added to the global or a per-P run queue.
// * New/modified-earlier timers on a per-P timer heap.
// * Idle-priority GC work (barring golang.org/issue/19112).
//
// If we discover new work below, we need to restore m.spinning as a
// signal for resetspinning to unpark a new worker thread (because
// there can be more than one starving goroutine).
```

**The pattern for work submission:**
1. Submit work to queue
2. StoreLoad memory barrier
3. Check `sched.nmspinning`

**The pattern for spinning->non-spinning:**
1. Decrement `nmspinning`
2. StoreLoad memory barrier
3. Re-check all work sources

This is a classic example of the "flag + data" synchronization pattern. The memory barriers ensure that if both sides race, at least one of them will see the other's update.

**Discussion question**: What happens if we reverse the order -- check for work first, then decrement nmspinning? (Answer: A submitter could add work, see nmspinning > 0, and not wake a thread. Then the spinner decrements nmspinning and parks. The work sits unprocessed.)

---

## 4. The Global Run Queue (5 min)

### Overflow: runqputslow

When a P's local run queue is full (256 entries), the next `runqput` call triggers `runqputslow`, which moves half the local queue to the global queue:

```go
// src/runtime/proc.go, lines 7524-7559
func runqputslow(pp *p, gp *g, h, t uint32) bool {
    var batch [len(pp.runq)/2 + 1]*g

    // First, grab a batch from local queue.
    n := t - h
    n = n / 2
    if n != uint32(len(pp.runq)/2) {
        throw("runqputslow: queue is not full")
    }
    for i := uint32(0); i < n; i++ {
        batch[i] = pp.runq[(h+i)%uint32(len(pp.runq))].ptr()
    }
    if !atomic.CasRel(&pp.runqhead, h, h+n) { // cas-release, commits consume
        return false
    }
    batch[n] = gp

    // ...optional randomization...

    // Link the goroutines.
    for i := uint32(0); i < n; i++ {
        batch[i].schedlink.set(batch[i+1])
    }

    q := gQueue{batch[0].guintptr(), batch[n].guintptr(), int32(n + 1)}

    // Now put the batch on global queue.
    lock(&sched.lock)
    globrunqputbatch(&q)
    unlock(&sched.lock)
    return true
}
```

The global queue requires `sched.lock`, which is why it's avoided on the fast path.

### Fairness: The schedtick % 61 Check

Without special handling, two goroutines on a local queue could starve goroutines on the global queue by constantly respawning each other. The scheduler prevents this:

```go
// src/runtime/proc.go, lines 3440-3450
    // Check the global runnable queue once in a while to ensure fairness.
    // Otherwise two goroutines can completely occupy the local runqueue
    // by constantly respawning each other.
    if pp.schedtick%61 == 0 && !sched.runq.empty() {
        lock(&sched.lock)
        gp := globrunqget()
        unlock(&sched.lock)
        if gp != nil {
            return gp, false, false
        }
    }
```

**Why 61?** It's a prime number. Using a prime avoids resonance with any regular patterns in the program. If we used 64, and a program happened to schedule exactly 64 goroutines between global queue checks, we'd check at the same point every time. A prime ensures the check point moves through the schedule cycle.

---

## 5. Cooperative Preemption (5 min)

### Function Prologue Stack Checks

Every non-leaf Go function begins with a *prologue* that checks if the stack needs to grow. The compiler inserts code equivalent to:

```
if SP < g.stackguard0 {
    call runtime.morestack
}
```

The scheduler exploits this for cooperative preemption. To preempt a goroutine, it sets `gp.stackguard0 = stackPreempt` (a value so large that *any* stack check will fail). The goroutine then calls `morestack`, which detects the preemption request and yields:

```go
// src/runtime/proc.go, lines 6880-6886
    gp.preempt = true

    // Every call in a goroutine checks for stack overflow by
    // comparing the current stack pointer to gp->stackguard0.
    // Setting gp->stackguard0 to StackPreempt folds
    // preemption into the normal stack overflow check.
    gp.stackguard0 = stackPreempt
```

From `preempt.go` (lines 27-33):
```go
// Synchronous safe-points are implemented by overloading the stack
// bound check in function prologues. To preempt a goroutine at the
// next synchronous safe-point, the runtime poisons the goroutine's
// stack bound to a value that will cause the next stack bound check
// to fail and enter the stack growth implementation, which will
// detect that it was actually a preemption and redirect to preemption
// handling.
```

**Limitation**: A goroutine in a tight loop with no function calls (e.g., `for { i++ }`) will never check its stack and never be preempted cooperatively. This was a real problem before Go 1.14.

---

## 6. Asynchronous (Signal-Based) Preemption (10 min)

### The SIGURG Choice

Go 1.14 introduced true preemptive scheduling via OS signals. The runtime sends a signal to the thread running a goroutine that needs preemption. But which signal?

```go
// src/runtime/signal_unix.go, lines 44-74
// sigPreempt is the signal used for non-cooperative preemption.
//
// There's no good way to choose this signal, but there are some
// heuristics:
//
// 1. It should be a signal that's passed-through by debuggers by
// default. On Linux, this is SIGALRM, SIGURG, SIGCHLD, SIGIO,
// SIGVTALRM, SIGPROF, and SIGWINCH, plus some glibc-internal signals.
//
// 2. It shouldn't be used internally by libc in mixed Go/C binaries
// because libc may assume it's the only thing that can handle these
// signals. For example SIGCANCEL or SIGSETXID.
//
// 3. It should be a signal that can happen spuriously without
// consequences. For example, SIGALRM is a bad choice because the
// signal handler can't tell if it was caused by the real process
// alarm or not (arguably this means the signal is broken, but I
// digress). SIGUSR1 and SIGUSR2 are also bad because those are often
// used in meaningful ways by applications.
//
// 4. We need to deal with platforms without real-time signals (like
// macOS), so those are out.
//
// We use SIGURG because it meets all of these criteria, is extremely
// unlikely to be used by an application for its "real" meaning (both
// because out-of-band data is basically unused and because SIGURG
// doesn't report which socket has the condition, making it pretty
// useless), and even if it is, the application has to be ready for
// spurious SIGURG.
const sigPreempt = _SIGURG
```

This is a great example of systems engineering trade-offs. The signal must be:
- Debugger-transparent (so GDB doesn't stop on it)
- Not used by libc (for CGo compatibility)
- Safe to receive spuriously (the handler must be idempotent)
- Available on all platforms (no real-time signals)

### How Async Preemption Works

From `preempt.go` (lines 35-43):
```go
// Preemption at asynchronous safe-points is implemented by suspending
// the thread using an OS mechanism (e.g., signals) and inspecting its
// state to determine if the goroutine was at an asynchronous
// safe-point. Since the thread suspension itself is generally
// asynchronous, it also checks if the running goroutine wants to be
// preempted, since this could have changed. If all conditions are
// satisfied, it adjusts the signal context to make it look like the
// signaled thread just called asyncPreempt and resumes the thread.
// asyncPreempt spills all registers and enters the scheduler.
```

The flow is:
1. `sysmon` or GC decides goroutine G needs preemption
2. Calls `preemptone(pp)`, which calls `preemptM(mp)`
3. `preemptM` sends `SIGURG` to the OS thread running G
4. Signal handler fires, checks if G is at an async safe-point
5. If safe, rewrites the signal return context to call `asyncPreempt`
6. `asyncPreempt` saves all registers and calls `schedule()`

**Async safe-points**: Not every instruction is safe for preemption. The runtime needs to be able to find all GC roots (stack pointers). The compiler emits metadata marking which instructions are safe.

### preemptone: The Combined Approach

```go
// src/runtime/proc.go, lines 6866-6895
func preemptone(pp *p) bool {
    mp := pp.m.ptr()
    if mp == nil || mp == getg().m {
        return false
    }
    gp := mp.curg
    if gp == nil || gp == mp.g0 {
        return false
    }

    gp.preempt = true

    // Every call in a goroutine checks for stack overflow by
    // comparing the current stack pointer to gp->stackguard0.
    // Setting gp->stackguard0 to StackPreempt folds
    // preemption into the normal stack overflow check.
    gp.stackguard0 = stackPreempt

    // Request an async preemption of this P.
    if preemptMSupported && debug.asyncpreemptoff == 0 {
        pp.preempt = true
        preemptM(mp)
    }

    return true
}
```

Notice the belt-and-suspenders approach: it sets BOTH the cooperative preemption flag (`stackguard0 = stackPreempt`) AND sends the async signal (`preemptM(mp)`). Whichever fires first wins.

---

## 7. The sysmon Thread (10 min)

### What is sysmon?

`sysmon` is a special background thread that runs without a P. It's the scheduler's watchdog, responsible for:
- Preempting long-running goroutines
- Retaking Ps from goroutines stuck in syscalls
- Polling the network (if no other thread is doing it)
- Triggering garbage collection

```go
// src/runtime/proc.go, lines 6486-6506
func sysmon() {
    lock(&sched.lock)
    sched.nmsys++
    checkdead()
    unlock(&sched.lock)

    lastgomaxprocs := int64(0)
    lasttrace := int64(0)
    idle := 0 // how many cycles in succession we had not wokeup somebody
    delay := uint32(0)

    for {
        if idle == 0 { // start with 20us sleep...
            delay = 20
        } else if idle > 50 { // start doubling the sleep after 1ms...
            delay *= 2
        }
        if delay > 10*1000 { // up to 10ms
            delay = 10 * 1000
        }
        usleep(delay)
        // ...
    }
}
```

**Adaptive sleep**: sysmon starts by polling every 20 microseconds. If it finds nothing to do for 50 iterations (~1ms), it starts doubling its sleep interval, up to a maximum of 10ms. When it finds work again, it resets. This balances responsiveness against CPU overhead.

### retake: Preempting and Reclaiming Ps

The `retake` function is called by `sysmon` to enforce time slices and reclaim Ps from syscalls:

```go
// src/runtime/proc.go, lines 6626-6628
// forcePreemptNS is the time slice given to a G before it is
// preempted.
const forcePreemptNS = 10 * 1000 * 1000 // 10ms
```

```go
// src/runtime/proc.go, lines 6656-6671
        // Preempt G if it's running on the same schedtick for
        // too long. This could be from a single long-running
        // goroutine or a sequence of goroutines run via
        // runnext, which share a single schedtick time slice.
        schedt := int64(pp.schedtick)
        if int64(pd.schedtick) != schedt {
            pd.schedtick = uint32(schedt)
            pd.schedwhen = now
        } else if pd.schedwhen+forcePreemptNS <= now {
            preemptone(pp)
            // If pp is in a syscall, preemptone doesn't work.
            sysretake = true
        }
```

The logic is:
1. If `pp.schedtick` changed since last check, the P is making progress -- update the timestamp
2. If `pp.schedtick` hasn't changed for 10ms, the same goroutine (or runnext chain) has been running too long -- preempt it

### Syscall Hand-off

When a goroutine enters a syscall, its M is blocked in the kernel. But the P is sitting idle. The `retake` function can steal this P and give it to another M:

```go
// src/runtime/proc.go, lines 6700-6705
        // On the one hand we don't want to retake Ps if there is no other work to do,
        // but on the other hand we want to retake them eventually
        // because they can prevent the sysmon thread from deep sleep.
        if runqempty(pp) && sched.nmspinning.Load()+sched.npidle.Load() > 0 && pd.syscallwhen+10*1000*1000 > now {
            thread.resume()
            goto done
```

The P is retaken from a syscalling M unless ALL of these conditions hold:
- The P's run queue is empty (no work waiting)
- There are spinning threads or idle Ps available (system is not starved)
- The syscall started less than 10ms ago

When the M returns from the syscall, it finds its P gone. It must acquire a new P to continue running Go code, or park itself and put the goroutine on the global queue.

### The schedule() Function: Putting It All Together

The main scheduler loop ties everything together:

```go
// src/runtime/proc.go, lines 4135-4164
func schedule() {
    mp := getg().m

    if mp.locks != 0 {
        throw("schedule: holding locks")
    }

    if mp.lockedg != 0 {
        stoplockedm()
        execute(mp.lockedg.ptr(), false) // Never returns.
    }

    // ...

top:
    pp := mp.p.ptr()
    pp.preempt = false

    // Safety check: if we are spinning, the run queue should be empty.
    if mp.spinning && (pp.runnext != 0 || pp.runqhead != pp.runqtail) {
        throw("schedule: spinning with local work")
    }

    gp, inheritTime, tryWakeP := findRunnable() // blocks until work is available

    // ...

    // This thread is going to run a goroutine and is not spinning anymore,
    // so if it was marked as spinning we need to reset it now and potentially
    // start a new spinning M.
    if mp.spinning {
        resetspinning()
    }

    // ...
}
```

The search order in `findRunnable()` is:
1. Check GC work
2. Every 61st tick, check the global run queue (fairness)
3. Check the local run queue
4. Check the global run queue (again, without the tick restriction)
5. Poll network
6. **Steal from other Ps** via `stealWork()`
7. If nothing found, park the thread

---

## 8. Summary: The Complete Picture

```
                    ┌─────────────────────────────────────┐
                    │           Global Run Queue           │
                    │  (overflow + fairness, locked)        │
                    └──────────┬────────────┬──────────────┘
                               │            │
                    schedtick%61        runqputslow
                               │            │
         ┌─────────────────────┼────────────┼─────────────────────┐
         │                     │            │                     │
    ┌────▼────┐          ┌─────▼────┐  ┌────▼─────┐         ┌─────────┐
    │  P0     │◄─steal───│  P1     │  │  P2      │──steal──►│  P3     │
    │ runq    │          │ runq    │  │ runq     │          │ runq    │
    │ runnext │          │ runnext │  │ runnext  │          │ runnext │
    └────┬────┘          └────┬────┘  └────┬─────┘         └────┬────┘
         │                    │            │                    │
    ┌────▼────┐          ┌────▼────┐  ┌────▼─────┐         ┌────▼────┐
    │   M0    │          │   M1    │  │   M2     │         │   M3    │
    │ (thread)│          │ (thread)│  │ (thread) │         │ (thread)│
    └─────────┘          └─────────┘  └──────────┘         └─────────┘

                    sysmon thread (no P):
                    - preempts goroutines after 10ms
                    - retakes Ps from syscalls
                    - polls network
```

### Key Takeaways

1. **Distributed state is fast but complex**: Per-P queues avoid lock contention but require careful synchronization with atomics and memory barriers.

2. **Work stealing balances load**: Steal half, random victim selection, multiple attempts.

3. **Spinning threads prevent thundering herd**: At most one spinning thread is sufficient to discover new work and cascade wake-ups.

4. **Two-tier preemption**: Cooperative (stack checks) handles the common case; async signals (SIGURG) handle tight loops.

5. **sysmon is the safety net**: It runs independently, enforcing time slices and reclaiming resources from blocked syscalls.

---

## Discussion Questions

1. Why does the scheduler steal half the victim's goroutines instead of just one? What would change if it stole all of them?

2. The spinning thread protocol uses `sched.nmspinning` as a shared counter. Why not use per-P spinning flags instead?

3. Consider a program that makes many blocking syscalls (e.g., a web server doing file I/O). How does the P hand-off mechanism keep the program responsive?

4. Why is 61 a better choice than 64 for the global queue fairness check interval?

5. What would happen in a pre-1.14 Go runtime if a goroutine entered `for { }` (an empty infinite loop)?

## Further Reading

- [Scalable Go Scheduler Design Doc](https://golang.org/s/go11sched) -- Dmitry Vyukov's original proposal
- Blumofe, R. and Leiserson, C. "Scheduling Multithreaded Computations by Work Stealing" (1999)
- [Go 1.14 Release Notes](https://golang.org/doc/go1.14) -- Asynchronous preemption
- [proposal: Non-cooperative goroutine preemption](https://github.com/golang/go/issues/24543)
