# Module 5: Work Stealing and Preemption
## Slides

---

## Slide 1: Module Overview

**Work Stealing and Preemption**

How does the Go runtime distribute goroutines across CPUs -- and take them back?

Topics:
- Why distributed scheduling?
- The work-stealing algorithm
- Spinning threads
- Global run queue and fairness
- Cooperative and asynchronous preemption
- The sysmon watchdog thread

---

## Slide 2: The Scalability Problem

**Why not a single run queue?**

```
    Single Global Run Queue
    ┌───────────────────────┐
    │  G1  G2  G3  G4  G5  │ ◄── mutex contention!
    └───┬───┬───┬───┬───┬──┘
        │   │   │   │   │
       M0  M1  M2  M3  M4
```

- Every enqueue/dequeue acquires a lock
- On 64 cores, the lock becomes the bottleneck
- Cache line bouncing destroys performance

---

## Slide 3: Three Rejected Approaches

From `src/runtime/proc.go`, lines 45-56:

1. **Centralize everything** -- "would inhibit scalability"
2. **Direct goroutine handoff** -- "thread state thrashing... destroy locality"
3. **Eager thread unparking** -- "excessive thread parking/unparking"

The Go team evaluated and rejected all three before settling on work stealing.

---

## Slide 4: The Solution: Per-P Run Queues + Work Stealing

```
    ┌─────────┐    ┌─────────┐    ┌─────────┐
    │  P0     │    │  P1     │    │  P2     │
    │ [G G G] │◄──steal──│ [G G G G]│    │ [    ] │──steal──►
    │ runnext │    │ runnext │    │ runnext │
    └────┬────┘    └────┬────┘    └────┬────┘
         │              │              │
        M0             M1             M2
```

- Each P has a local 256-entry ring buffer (lock-free)
- Empty Ps **steal** from busy Ps
- Global queue for overflow and fairness

---

## Slide 5: stealWork -- The Top-Level Stealer

```go
// src/runtime/proc.go, lines 3828-3835
func stealWork(now int64) (gp *g, ...) {
    pp := getg().m.p.ptr()
    const stealTries = 4
    for i := 0; i < stealTries; i++ {
        stealTimersOrRunNextG := i == stealTries-1
        for enum := stealOrder.start(cheaprand()); !enum.done(); enum.next() {
            p2 := allp[enum.position()]
            // ...try to steal from p2...
        }
    }
}
```

- **4 attempts** (stealing is racy)
- **Random victim order** (avoids hot spots)
- **Timer stealing only on last pass** (expensive)

---

## Slide 6: runqsteal -- Steal Half

```go
// src/runtime/proc.go, lines 7730-7747
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
    atomic.StoreRel(&pp.runqtail, t+n)
    return gp
}
```

Returns one G to run immediately; puts the rest on the thief's local queue.

---

## Slide 7: runqgrab -- Lock-Free Core

```go
// src/runtime/proc.go, lines 7662-7667
func runqgrab(pp *p, batch *[256]guintptr, batchHead uint32, stealRunNextG bool) uint32 {
    for {
        h := atomic.LoadAcq(&pp.runqhead)
        t := atomic.LoadAcq(&pp.runqtail)
        n := t - h
        n = n - n/2   // steal half!
```

- **No mutex** -- pure atomic CAS loop
- `n = n - n/2` steals the ceiling of half
- `atomic.CasRel` commits the steal or retries

**Why half?** Balances queues in O(log n) steals (Blumofe-Leiserson '99).

---

## Slide 8: The runnext Slot

Each P has a special `runnext` field:
- Holds exactly one goroutine
- Set when a goroutine wakes another (channel send, etc.)
- **Preserves producer-consumer locality**
- Stolen only as a last resort (final steal attempt)

```go
// Stealing runnext includes a 3us back-off!
// src/runtime/proc.go, lines 7683-7684
// A sync chan send/recv takes ~50ns as of time of
// writing, so 3us gives ~50x overshoot.
```

---

## Slide 9: Spinning Threads -- The Concept

```go
// src/runtime/proc.go, lines 68-76
// A worker thread is considered spinning if it is out of local work and did
// not find work in the global run queue or netpoller; the spinning state is
// denoted in m.spinning and in sched.nmspinning. Threads unparked this way are
// also considered spinning; we don't do goroutine handoff so such threads are
// out of work initially. Spinning threads spin on looking for work in per-P
// run queues and timer heaps or from the GC before parking. If a spinning
// thread finds work it takes itself out of the spinning state and proceeds to
// execution. If it does not find work it takes itself out of the spinning
// state and then parks.
```

A **spinning** thread: has no work, but is actively searching.

---

## Slide 10: The Wake-up Policy

```go
// src/runtime/proc.go, lines 78-83
// If there is at least one spinning thread (sched.nmspinning>1), we don't
// unpark new threads when submitting work. To compensate for that, if the last
// spinning thread finds work and stops spinning, it must unpark a new spinning
// thread.
```

**Rule**: Wake a new thread only if:
1. There is an idle P, **AND**
2. `sched.nmspinning == 0`

> **Note:** The source comment says `nmspinning>1` but means `>0`; the code checks `== 0`. This is a known comment inconsistency.

This prevents **thundering herd** -- 1000 new goroutines don't wake 1000 threads.

---

## Slide 11: Spinning Thread Cascade

```
Time ──►

M1 spinning ──► finds work ──► resetspinning() ──► wakep()
                                                       │
                                                       ▼
                                               M2 now spinning ──► finds work ──► wakep()
                                                                                      │
                                                                                      ▼
                                                                              M3 now spinning...
```

Smooth ramp-up: each spinner that finds work wakes one more.

---

## Slide 12: The Delicate Dance

```go
// src/runtime/proc.go, lines 3622-3628
// Delicate dance: thread transitions from spinning to non-spinning
// state, potentially concurrently with submission of new work. We must
// drop nmspinning first and then check all sources again (with
// #StoreLoad memory barrier in between). If we do it the other way
// around, another thread can submit work after we've checked all
// sources but before we drop nmspinning; as a result nobody will
// unpark a thread to run the work.
```

---

## Slide 13: The Synchronization Pattern

**Work submitter:**
1. Put G on run queue
2. Memory barrier
3. Check `nmspinning` -- if 0, call `wakep()`

**Spinning thread parking:**
1. Decrement `nmspinning`
2. Memory barrier
3. Re-check all queues -- if work found, grab it

If either side sees the other's update, we're safe. The memory barriers guarantee at least one side does.

---

## Slide 14: Global Run Queue -- Overflow

```go
// src/runtime/proc.go, lines 7524-7539
func runqputslow(pp *p, gp *g, h, t uint32) bool {
    var batch [len(pp.runq)/2 + 1]*g
    // Grab half from local queue
    n := t - h
    n = n / 2
    for i := uint32(0); i < n; i++ {
        batch[i] = pp.runq[(h+i)%uint32(len(pp.runq))].ptr()
    }
    // ...
    // Now put the batch on global queue.
    lock(&sched.lock)
    globrunqputbatch(&q)
    unlock(&sched.lock)
}
```

When local queue is full (256 entries): move half + the new G to global queue.

---

## Slide 15: Global Run Queue -- Fairness

```go
// src/runtime/proc.go, lines 3440-3450
    // Check the global runnable queue once in a while to ensure fairness.
    // Otherwise two goroutines can completely occupy the local runqueue
    // by constantly respawning each other.
    if pp.schedtick%61 == 0 && !sched.runq.empty() {
        lock(&sched.lock)
        gp := globrunqget()
        // ...
    }
```

**Why 61?** It's prime. Avoids resonance with regular scheduling patterns.

Every 61st scheduling decision checks the global queue first.

---

## Slide 16: Cooperative Preemption

Every Go function prologue checks the stack:
```
if SP < g.stackguard0 {
    call runtime.morestack  // "I need more stack"
}
```

To preempt: set `stackguard0 = stackPreempt` (a poison value).

```go
// src/runtime/proc.go, lines 6882-6886
    // Every call in a goroutine checks for stack overflow by
    // comparing the current stack pointer to gp->stackguard0.
    // Setting gp->stackguard0 to StackPreempt folds
    // preemption into the normal stack overflow check.
    gp.stackguard0 = stackPreempt
```

Next function call triggers `morestack`, which detects preemption.

---

## Slide 17: The Tight Loop Problem

```go
func cpuHog() {
    for {
        // no function calls = no stack checks = no preemption!
    }
}
```

Before Go 1.14, this goroutine could monopolize its thread forever.

**Solution**: Asynchronous preemption via signals.

---

## Slide 18: Why SIGURG?

```go
// src/runtime/signal_unix.go, lines 44-74
// 1. Passed-through by debuggers by default
// 2. Not used internally by libc
// 3. Can happen spuriously without consequences
// 4. Available on all platforms (no real-time signals)
//
// We use SIGURG because it meets all of these criteria, is extremely
// unlikely to be used by an application for its "real" meaning (both
// because out-of-band data is basically unused and because SIGURG
// doesn't report which socket has the condition, making it pretty
// useless)
const sigPreempt = _SIGURG
```

---

## Slide 19: Async Preemption Flow

```
sysmon detects G running > 10ms
    │
    ▼
preemptone(pp)
    │
    ├── gp.stackguard0 = stackPreempt  (cooperative)
    │
    └── preemptM(mp)                    (async)
            │
            ▼
        Send SIGURG to thread
            │
            ▼
        Signal handler checks: at async safe-point?
            │
            ├── Yes: rewrite return to asyncPreempt()
            │         asyncPreempt spills regs, calls schedule()
            │
            └── No: signal ignored, retry later
```

---

## Slide 20: preemptone -- Belt and Suspenders

```go
// src/runtime/proc.go, lines 6866-6895
func preemptone(pp *p) bool {
    mp := pp.m.ptr()
    gp := mp.curg

    gp.preempt = true
    gp.stackguard0 = stackPreempt        // cooperative

    if preemptMSupported && debug.asyncpreemptoff == 0 {
        pp.preempt = true
        preemptM(mp)                      // async (SIGURG)
    }
    return true
}
```

Both mechanisms fire. Whichever succeeds first preempts the goroutine.

---

## Slide 21: The sysmon Thread

```go
// src/runtime/proc.go, lines 6486-6506
func sysmon() {
    // ...
    for {
        if idle == 0 {
            delay = 20          // start: 20us
        } else if idle > 50 {
            delay *= 2          // after 1ms: double
        }
        if delay > 10*1000 {
            delay = 10 * 1000   // max: 10ms
        }
        usleep(delay)
        // ...check for preemption, syscall retake, network poll...
    }
}
```

Responsibilities:
- Preempt goroutines running > 10ms
- Retake Ps from blocked syscalls
- Poll the network
- Trigger GC

---

## Slide 22: The 10ms Time Slice

```go
// src/runtime/proc.go, lines 6626-6628
const forcePreemptNS = 10 * 1000 * 1000 // 10ms
```

```go
// lines 6660-6665
        schedt := int64(pp.schedtick)
        if int64(pd.schedtick) != schedt {
            pd.schedtick = uint32(schedt)
            pd.schedwhen = now
        } else if pd.schedwhen+forcePreemptNS <= now {
            preemptone(pp)
```

If `schedtick` hasn't advanced in 10ms, the goroutine is preempted.

---

## Slide 23: Syscall Hand-off

```
Before syscall:          During syscall:         After retake:
┌─────┐                  ┌─────┐                ┌─────┐
│  P  │──────M           │  P  │  M (blocked)   │  P  │──────M2
│[G G]│                  │[G G]│  in kernel     │[G G]│
└─────┘                  └─────┘                └─────┘
                              ▲
                              │
                         sysmon retakes P
```

- P is too valuable to sit idle while M is in a syscall
- sysmon gives the P to another M
- When M returns from syscall, it must acquire a new P (or park)

---

## Slide 24: findRunnable Search Order

```go
// src/runtime/proc.go, line 3389
func findRunnable() (gp *g, inheritTime, tryWakeP bool) {
```

Priority:
1. GC work
2. Every 61st tick: global run queue (fairness)
3. Local run queue
4. Global run queue
5. Network poll
6. **Work stealing** (`stealWork`)
7. Park the thread

If nothing found after all steps, M releases its P and sleeps.

---

## Slide 25: Key Takeaways

| Mechanism | Purpose | Cost |
|-----------|---------|------|
| Per-P run queues | Avoid lock contention | Complexity of work stealing |
| Work stealing (half) | Load balancing | Lock-free atomic ops |
| Spinning threads | Low-latency discovery | Controlled CPU burn |
| Global queue + %61 | Fairness | Occasional lock acquisition |
| Cooperative preemption | Safe preemption at function calls | Requires function calls |
| SIGURG async preemption | Preempt tight loops | Signal handler overhead |
| sysmon | Enforce time slices, retake Ps | Background thread |

**The Go scheduler is a masterclass in distributed systems design applied to a single process.**
