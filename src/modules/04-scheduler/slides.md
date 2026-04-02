# Module 4: The Go Scheduler

---

## Slide 1: Module Overview

**How Go Schedules Goroutines**

- The GMP model: G, M, and P
- The P struct: local run queue and caches
- The scheduling loop: `schedule()` -> `findRunnable()` -> `execute()`
- Goroutine lifecycle: creation, blocking, waking, exit
- Run queue internals and the `runnext` optimization

---

## Slide 2: The GMP Model

```go
// src/runtime/proc.go, lines 24-34
// Goroutine scheduler
// The scheduler's job is to distribute ready-to-run goroutines
// over worker threads.
//
// The main concepts are:
// G - goroutine.
// M - worker thread, or machine.
// P - processor, a resource that is required to execute Go code.
//     M must have an associated P to execute Go code, however
//     it can be blocked or in a syscall w/o an associated P.
```

---

## Slide 3: Why P Exists

**Before Go 1.1:** Only G and M, with a global run queue.

Problem: global mutex on every schedule, create, complete operation.

**After Go 1.1:** Introduced P.

- Each P has a **local run queue** (no lock needed)
- Each P has **local caches** (mcache, defer pool, sudog pool)
- Number of Ps = `GOMAXPROCS` = bound on parallelism

---

## Slide 4: GMP Relationships

```
              ┌─────────┐
        ┌─────┤  P (id=0) ├─── runq: [G3, G4, G5]
        │     └─────────┘     runnext: G2
        │
   ┌────┴───┐
   │  M (OS  │  curg: G1 (running)
   │ thread) │  g0: scheduler stack
   └────┬───┘
        │
   [CPU core 0]
```

**Rule:** M must hold a P to execute Go code.
M may exist without a P (blocked in syscall, parked idle).

---

## Slide 5: The P Struct -- Key Fields

```go
// src/runtime/runtime2.go, lines 773-819
type p struct {
    id          int32
    status      uint32     // pidle/prunning/...
    m           muintptr   // back-link to associated m
    mcache      *mcache    // memory allocator cache

    // Queue of runnable goroutines. Lock-free.
    runqhead uint32
    runqtail uint32
    runq     [256]guintptr

    // Next G to run (high priority).
    runnext guintptr

    // Free G's for reuse
    gFree gList

    sudogcache []*sudog     // channel operation cache
    goidcache  uint64       // batch of goroutine IDs
}
```

---

## Slide 6: The Local Run Queue

```
runqhead                              runqtail
    |                                     |
    v                                     v
  [ G3 | G4 | G5 | G6 |  .  |  .  | ... |  ]
    0    1    2    3    4    5        255

  Fixed-size circular buffer of 256 entries
  Lock-free: atomic load-acquire / store-release / CAS
```

- Owner P pushes/pops without locks
- Other Ps can **steal** using atomic CAS
- If full (256): half moves to global queue

---

## Slide 7: The runnext Slot

```go
// src/runtime/runtime2.go, lines 807-818
// runnext, if non-nil, is a runnable G that was ready'd by
// the current G and should be run next instead of what's in
// runq if there's time remaining in the running G's time
// slice. It will inherit the time left in the current time
// slice. If a set of goroutines is locked in a
// communicate-and-wait pattern, this schedules that set as a
// unit and eliminates the (potentially large) scheduling
// latency that otherwise arises from adding the ready'd
// goroutines to the end of the run queue.
```

Producer sends on channel -> consumer goes into `runnext` -> runs immediately.

---

## Slide 8: Global Scheduler State (schedt)

```go
// src/runtime/runtime2.go, lines 931-970
type schedt struct {
    lock mutex

    midle   listHeadManual // idle m's waiting for work
    nmidle  int32
    mnext   int64          // next M ID

    pidle   puintptr       // idle p's
    npidle  atomic.Int32

    nmspinning atomic.Int32 // spinning M count

    // Global runnable queue.
    runq gQueue

    // Global cache of dead G's.
    gFree struct {
        lock    mutex
        stack   gList
        noStack gList
    }
}
```

---

## Slide 9: Local vs. Global State

| State | Location | Lock? |
|-------|----------|-------|
| Per-P run queue | `p.runq[256]` | Lock-free |
| `runnext` | `p.runnext` | Atomic CAS |
| Global run queue | `sched.runq` | `sched.lock` |
| Idle Ms | `sched.midle` | `sched.lock` |
| Idle Ps | `sched.pidle` | `sched.lock` |
| Dead G pool | `p.gFree` / `sched.gFree` | Local: none |

**Design principle:** fast paths use local state; slow paths use global + lock.

---

## Slide 10: The gobuf -- Minimal Context

```go
// src/runtime/runtime2.go, lines 303-322
type gobuf struct {
    sp   uintptr        // stack pointer
    pc   uintptr        // program counter
    g    guintptr       // owning goroutine
    ctxt unsafe.Pointer // closure context
    lr   uintptr        // link register (ARM)
    bp   uintptr        // base pointer (x86)
}
```

Only **6 values** saved per goroutine switch.

Why so few? The compiler ensures all live values are on the stack at safe points. No need to save general-purpose registers.

---

## Slide 11: The Scheduling Loop

```go
// src/runtime/proc.go, lines 4135-4231
func schedule() {
    mp := getg().m

    if mp.lockedg != 0 {
        stoplockedm()
        execute(mp.lockedg.ptr(), false)
    }

top:
    pp := mp.p.ptr()

    gp, inheritTime, tryWakeP := findRunnable()
    // blocks until work is available

    if mp.spinning {
        resetspinning()
    }

    execute(gp, inheritTime)
}
```

`schedule` -> `findRunnable` -> `execute` -> user code

**`execute` never returns.** Re-entry is via `mcall` from user code.

---

## Slide 12: execute() -- Start Running a Goroutine

```go
// src/runtime/proc.go, lines 3331-3383
func execute(gp *g, inheritTime bool) {
    mp := getg().m

    mp.curg = gp          // M -> G binding
    gp.m = mp             // G -> M binding
    casgstatus(gp, _Grunnable, _Grunning)
    gp.waitsince = 0
    gp.preempt = false
    gp.stackguard0 = gp.stack.lo + stackGuard
    if !inheritTime {
        mp.p.ptr().schedtick++
    }

    gogo(&gp.sched)       // restore context, jump to user code
}
```

`gogo` loads SP/PC/BP from `gp.sched` and jumps -- never returns.

---

## Slide 13: findRunnable() -- The Priority Order

```
Priority 0: GC stop-the-world          (stop if GC waiting)
Priority 1: Trace reader               (system goroutine)
Priority 2: GC mark worker             (assist GC)
Priority 3: Global queue (1/61)        (fairness check)
Priority 4: Finalizer/cleanup Gs       (wake if pending)
Priority 5: LOCAL RUN QUEUE            (fast path!)
Priority 6: Global queue (batch)       (take up to 128)
Priority 7: Network poller             (non-blocking)
Priority 8: WORK STEALING              (steal from other P)
Priority 9: Idle GC marking            (use idle time)
Priority 10: PARK THE M               (give up, sleep)
```

---

## Slide 14: findRunnable() -- Fairness Check

```go
// src/runtime/proc.go, line 3443
if pp.schedtick%61 == 0 && !sched.runq.empty() {
    lock(&sched.lock)
    gp := globrunqget()
    unlock(&sched.lock)
    if gp != nil {
        return gp, false, false
    }
}
```

Every **61st** scheduling decision, check global queue first.

Why 61? Prime number to avoid resonance. Prevents two goroutines on the same P from starving the global queue.

---

## Slide 15: findRunnable() -- Local Queue

```go
// src/runtime/proc.go, lines 3468-3471
// local runq
if gp, inheritTime := runqget(pp); gp != nil {
    return gp, inheritTime, false
}
```

**This is the fast path.** No locks, no contention. Most scheduling decisions end here.

---

## Slide 16: findRunnable() -- Global Queue Batch

```go
// src/runtime/proc.go, lines 3473-3484
// global runq
if !sched.runq.empty() {
    lock(&sched.lock)
    gp, q := globrunqgetbatch(int32(len(pp.runq)) / 2)
    unlock(&sched.lock)
    if gp != nil {
        runqputbatch(pp, &q)  // fill local queue
        return gp, false, false
    }
}
```

Takes a **batch** of goroutines (up to 128) from global to local. Amortizes the cost of acquiring `sched.lock`.

---

## Slide 17: findRunnable() -- Work Stealing

```go
// src/runtime/proc.go, lines 3517-3526
if mp.spinning || 2*sched.nmspinning.Load() <
        gomaxprocs-sched.npidle.Load() {
    if !mp.spinning {
        mp.becomeSpinning()
    }
    gp, inheritTime, tnow, w, newWork := stealWork(now)
    if gp != nil {
        return gp, inheritTime, false
    }
}
```

- Iterate over Ps in **random order**
- Steal **half** of another P's local queue
- Limit spinning Ms to half of busy Ps (avoid CPU waste)

---

## Slide 18: findRunnable() -- Parking

```go
// src/runtime/proc.go, lines 3616-3620
if releasep() != pp {
    throw("findRunnable: wrong p")
}
now = pidleput(pp, now)
unlock(&sched.lock)
```

If no work found anywhere:
1. Release the P (put it on idle list)
2. Park the M (sleep on `m.park` note)
3. Wait until woken by `wakep()` or `startm()`

---

## Slide 19: Goroutine Creation -- newproc

```go
// src/runtime/proc.go, lines 5295-5308
func newproc(fn *funcval) {
    gp := getg()
    pc := sys.GetCallerPC()
    systemstack(func() {
        newg := newproc1(fn, gp, pc, false, waitReasonZero)

        pp := getg().m.p.ptr()
        runqput(pp, newg, true)  // next=true -> runnext

        if mainStarted {
            wakep()
        }
    })
}
```

`go f()` compiles to `newproc(&f)`.

New goroutine goes into **runnext** (will run next).

---

## Slide 20: newproc1 -- Goroutine Initialization

```go
// src/runtime/proc.go, lines 5313-5352
func newproc1(fn *funcval, ...) *g {
    mp := acquirem()
    pp := mp.p.ptr()
    newg := gfget(pp)        // reuse dead G?
    if newg == nil {
        newg = malg(stackMin) // allocate with 2KB stack
        casgstatus(newg, _Gidle, _Gdead)
        allgadd(newg)
    }

    memclrNoHeapPointers(&newg.sched, ...)
    newg.sched.sp = sp
    newg.sched.pc = abi.FuncPCABI0(goexit) + sys.PCQuantum
    newg.sched.g = guintptr(unsafe.Pointer(newg))
    gostartcallfn(&newg.sched, fn)
    newg.parentGoid = callergp.goid
    newg.startpc = fn.fn
    // ...assign goid, set _Grunnable...
}
```

Note: PC is set to `goexit` so when `fn` returns, cleanup runs automatically.

---

## Slide 21: Goroutine Blocking -- gopark

```go
// src/runtime/proc.go, lines 445-463
func gopark(unlockf func(*g, unsafe.Pointer) bool,
            lock unsafe.Pointer, reason waitReason, ...) {
    mp := acquirem()
    gp := mp.curg
    mp.waitlock = lock
    mp.waitunlockf = unlockf
    gp.waitreason = reason
    releasem(mp)
    mcall(park_m)  // switch to g0 stack
}
```

`mcall(park_m)`:
1. Save current G's registers into `g.sched`
2. Switch to g0 stack
3. Call `park_m`

---

## Slide 22: park_m -- The Parking Continuation

```go
// src/runtime/proc.go, lines 4253-4302
func park_m(gp *g) {
    mp := getg().m

    casgstatus(gp, _Grunning, _Gwaiting)

    dropg()   // gp.m = nil; mp.curg = nil

    if fn := mp.waitunlockf; fn != nil {
        ok := fn(gp, mp.waitlock)
        if !ok {
            casgstatus(gp, _Gwaiting, _Grunnable)
            execute(gp, true) // cancel park, resume
        }
    }

    schedule() // find next goroutine to run
}
```

**Critical:** The unlock function runs **after** the G is marked `_Gwaiting`. This prevents the race where a waker could call `ready()` before we finish parking.

---

## Slide 23: Goroutine Waking -- goready / ready

```go
// src/runtime/proc.go, lines 481-484
func goready(gp *g, traceskip int) {
    systemstack(func() {
        ready(gp, traceskip, true)
    })
}

// src/runtime/proc.go, lines 1120-1140
func ready(gp *g, traceskip int, next bool) {
    mp := acquirem()
    casgstatus(gp, _Gwaiting, _Grunnable)
    runqput(mp.p.ptr(), gp, next) // next=true -> runnext
    wakep()
    releasem(mp)
}
```

Woken goroutine goes to **runnext** -> runs next on this P.
`wakep()` wakes an idle M in case another P is available.

---

## Slide 24: Goroutine Exit -- goexit0 / gdestroy

```go
// src/runtime/proc.go, lines 4491-4501
func goexit0(gp *g) {
    gdestroy(gp)
    schedule()
}

// src/runtime/proc.go, lines 4503-4539
func gdestroy(gp *g) {
    casgstatus(gp, _Grunning, _Gdead)
    gp.m = nil
    gp._defer = nil
    gp._panic = nil
    gp.writebuf = nil
    gp.waitreason = waitReasonZero
    // ...clear all fields...
    dropg()
    // ...gfput(pp, gp) -- save for reuse...
}
```

G transitions to `_Gdead`, is cleaned up, placed on free list for reuse by `newproc1`.

---

## Slide 25: Complete Goroutine Lifecycle

```
  go f()
    │
    ▼
 newproc() ──► newproc1() ──► runqput(runnext)
                                    │
                                    ▼
                              _Grunnable
                                    │
                              execute()
                                    │
                                    ▼
                              _Grunning ◄──────┐
                               /    \          │
                         gopark()  return     ready()
                            │        │         │
                            ▼        ▼         │
                       _Gwaiting  goexit0()    │
                            │        │         │
                            │     _Gdead ──► free list
                            │
                            └──── (woken) ──► _Grunnable
```

---

## Slide 26: runqput -- Adding to Local Queue

```go
// src/runtime/proc.go, lines 7494-7519
if next {
    // CAS into runnext, kick old occupant to regular queue
    oldnext := pp.runnext
    pp.runnext.cas(oldnext, guintptr(unsafe.Pointer(gp)))
    if oldnext == 0 { return }
    gp = oldnext.ptr() // old runnext goes to regular queue
}

// Add to circular buffer
h := atomic.LoadAcq(&pp.runqhead)
t := pp.runqtail
if t-h < uint32(len(pp.runq)) {
    pp.runq[t%uint32(len(pp.runq))].set(gp)
    atomic.StoreRel(&pp.runqtail, t+1)
    return
}
runqputslow(pp, gp, h, t) // overflow: move half to global
```

---

## Slide 27: runqget -- Taking from Local Queue

```go
// src/runtime/proc.go, lines 7598-7619
func runqget(pp *p) (gp *g, inheritTime bool) {
    // 1. Check runnext first
    next := pp.runnext
    if next != 0 && pp.runnext.cas(next, 0) {
        return next.ptr(), true  // inheritTime = true!
    }

    // 2. Take from circular buffer (FIFO)
    for {
        h := atomic.LoadAcq(&pp.runqhead)
        t := pp.runqtail
        if t == h { return nil, false }
        gp := pp.runq[h%uint32(len(pp.runq))].ptr()
        if atomic.CasRel(&pp.runqhead, h, h+1) {
            return gp, false // inheritTime = false
        }
    }
}
```

`runnext` -> `inheritTime=true` (shares time slice)
Regular queue -> `inheritTime=false` (new time slice)

---

## Slide 28: The runnext Optimization -- Why It Matters

**Producer-consumer without runnext:**
```
Producer sends -> Consumer added to back of queue
                  [G7, G8, G9, ..., Consumer]
                  Consumer waits behind many Gs
                  High latency
```

**Producer-consumer with runnext:**
```
Producer sends -> Consumer placed in runnext
                  Consumer runs NEXT (inherits time slice)
                  ~0 scheduling latency
```

Starvation guard: `sysmon` preempts long-running goroutines. On platforms without `sysmon`, `runnext` is disabled.

---

## Slide 29: Overflow and Load Balancing

When local queue is full (256 entries):

```go
// src/runtime/proc.go, lines 7524-7559
func runqputslow(pp *p, gp *g, h, t uint32) bool {
    // Grab HALF the local queue
    n := (t - h) / 2  // 128 goroutines
    for i := uint32(0); i < n; i++ {
        batch[i] = pp.runq[(h+i)%uint32(len(pp.runq))].ptr()
    }
    batch[n] = gp  // plus the new one

    // Move them all to the global queue
    lock(&sched.lock)
    globrunqputbatch(&q)
    unlock(&sched.lock)
}
```

Moving half (not just 1) serves dual purpose:
1. Makes room in local queue
2. Makes work available for other Ps to find

---

## Slide 30: Putting It All Together

```
     ┌──────────────────────────────────────────────┐
     │              schedule()                       │
     │                  │                            │
     │            findRunnable()                     │
     │    ┌─────────────┼──────────────┐             │
     │    │             │              │             │
     │  local q     global q      steal from        │
     │  (runqget)   (globrunqget)  other P           │
     │    │             │              │             │
     │    └─────────────┼──────────────┘             │
     │                  │                            │
     │            execute(gp)                        │
     │                  │                            │
     │            gogo(&gp.sched) ──► user code      │
     │                                    │          │
     │              ┌─────────┬───────────┤          │
     │              │         │           │          │
     │          gopark()   Gosched()   return        │
     │              │         │           │          │
     │          park_m()  goschedImpl  goexit0()     │
     │              │         │           │          │
     │              └─────────┴───────────┘          │
     │                        │                      │
     │                   schedule() ◄────────────────┘
     └──────────────────────────────────────────────┘
```

The scheduler is a **loop**, not a separate thread.
Each M runs the loop on its g0 stack between goroutine executions.

---

## Slide 31: Discussion

1. Why does GC work get higher priority than user goroutines in `findRunnable`?
   - GC progress prevents heap exhaustion; starving GC risks OOM

2. What would happen if `schedtick%61` were `schedtick%2`?
   - Too much global queue checking; lock contention on `sched.lock`

3. Why steal **half** of another P's queue?
   - Stealing just 1 would require stealing again immediately
   - Stealing all would starve the victim

4. Why does `runnext` use atomic CAS?
   - Other Ps can steal from `runnext` (CAS to zero)
   - Only the owner P can set it to a non-zero value

---

## Slide 32: What's Next -- Module 5

**Work Stealing in Depth**

- The `stealWork` function
- Randomized iteration order
- Stealing timers as well as goroutines
- The spinning/non-spinning M protocol
- Interaction with the system monitor (`sysmon`)
