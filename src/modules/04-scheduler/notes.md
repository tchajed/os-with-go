# Module 4: The Go Scheduler

**Duration:** 75 minutes

---

## Background: CPU Scheduling From Hardware to User Space

CPU scheduling is one of the oldest and most fundamental problems in operating systems. At its core, the question is deceptively simple: given more work than processors to run it on, which task should run next? But behind that simplicity lies a web of competing goals. We want *fairness*, so that no process starves while others monopolize the CPU. We want high *throughput*, so the system completes as much useful work as possible. And we want low *latency*, so interactive tasks feel responsive to users. These goals are often in tension -- optimizing for one can hurt another -- and the history of OS scheduling is largely the story of navigating those trade-offs with increasingly clever designs. Early systems used simple first-come, first-served (FCFS) scheduling, then introduced preemptive round-robin to bound response times, and eventually developed multilevel feedback queues (MLFQ) that adapt scheduling priority based on observed process behavior.

Linux's scheduling evolution illustrates how real systems have tackled these problems at scale. The 2.6 kernel introduced the O(1) scheduler, which used active and expired priority arrays to achieve constant-time scheduling decisions -- a big improvement over the earlier O(n) scheduler that scanned every process on each context switch. But the O(1) scheduler's heuristics for distinguishing interactive and batch processes were fragile and hard to tune. In 2007, Ingo Molnar replaced it with the Completely Fair Scheduler (CFS), which took a radically different approach: instead of priority arrays and heuristics, CFS tracks each task's *virtual runtime* -- a measure of how much CPU time it has received, weighted by its priority (nice value). Tasks are stored in a red-black tree ordered by virtual runtime, and the scheduler always picks the task with the smallest virtual runtime, achieving O(log n) scheduling with an elegant fairness guarantee. CFS served Linux well for over 15 years, but in 2023 (kernel 6.6), it was replaced by the EEVDF (Earliest Eligible Virtual Deadline First) scheduler. EEVDF builds on the same virtual-runtime foundation but adds a *virtual deadline* concept, allowing the scheduler to make better decisions about latency-sensitive tasks without the ad hoc "sleeper bonus" heuristics that CFS had accumulated over the years. The result is more predictable latency behavior with a cleaner theoretical basis.

Modern multicore hardware introduces another dimension to the scheduling problem: a single global run queue protected by one lock becomes a severe bottleneck when dozens or hundreds of cores contend for it. Linux solves this with *per-CPU run queues* -- each processor core maintains its own queue of runnable tasks, and scheduling decisions are made locally without cross-core locking in the common case. This dramatically reduces contention, but it creates a new challenge: load balancing. If one core's queue drains while another is overloaded, the system wastes hardware parallelism. Linux uses periodic load balancing and idle-core work stealing to redistribute tasks, with a topology-aware approach that accounts for the cache-sharing relationships between cores (preferring to migrate tasks within the same NUMA node or LLC domain before moving them farther away).

The kernel scheduler works at the level of OS threads, but many applications need concurrency at a much finer granularity. This is the domain of *user-level scheduling*, where a runtime manages its own lightweight tasks on top of OS threads. The M:N threading model -- multiplexing M user-level threads onto N kernel threads -- has a long and checkered history. Sun's Solaris operating system experimented with M:N threading in the 1990s, and the Linux community explored it with the NGPT (Next Generation POSIX Threads) project in the early 2000s. Both efforts were largely abandoned in favor of 1:1 threading (one user thread per kernel thread), which was simpler to implement correctly and avoided thorny problems like priority inversion between the user-level and kernel schedulers. But the 1:1 model has costs: OS threads are heavyweight (typically consuming 1-8 MB of stack), expensive to create, and context-switching between them requires entering the kernel. Runtimes like the Erlang BEAM VM sidestepped this by implementing their own preemptive schedulers for lightweight processes, and Java's ForkJoinPool uses work-stealing to efficiently distribute fine-grained tasks. These user-level approaches trade generality for performance within their specific domains.

Go revived the M:N model with a design that learns from earlier failures. Rather than trying to be a general-purpose threading library, Go's scheduler is tightly integrated with the language runtime, the compiler, and the garbage collector. It can insert preemption points at compile time, it knows when a goroutine is about to make a blocking system call, and it can grow goroutine stacks dynamically (starting at just a few kilobytes). The result is a system that supports millions of concurrent goroutines with efficient scheduling -- and at its heart is the GMP model we will study in this module. Understanding the design of Go's scheduler is not just about learning Go; it is a case study in how the classic ideas of per-CPU run queues, work stealing, and cooperative-preemptive hybrid scheduling come together in a modern, practical system.

---

## Learning Objectives

By the end of this module, students will be able to:

1. Explain the GMP model and why the P (processor) abstraction exists
2. Describe the P struct's role: local run queue, caching, logical processor
3. Read the scheduling loop (`schedule` -> `findRunnable` -> `execute`)
4. Trace a goroutine through its complete lifecycle: creation, execution, blocking, waking, exit
5. Explain the run queue data structures and the `runnext` optimization

---

## Part 1: The GMP Model (10 min)

### Overview

The Go scheduler is built around three core abstractions, described at the top of `src/runtime/proc.go`, lines 24-34:

```go
// src/runtime/proc.go, lines 24-34
// Goroutine scheduler
// The scheduler's job is to distribute ready-to-run goroutines over
// worker threads.
//
// The main concepts are:
// G - goroutine.
// M - worker thread, or machine.
// P - processor, a resource that is required to execute Go code.
//     M must have an associated P to execute Go code, however it can be
//     blocked or in a syscall w/o an associated P.
//
// Design doc at https://golang.org/s/go11sched.
```

### Why Three Entities?

The original Go scheduler (before Go 1.1) only had G and M. Every M had a global mutex-protected run queue. This created severe contention: every time any goroutine was created, scheduled, or completed, it had to acquire the global lock. With 8+ cores, the lock became a bottleneck.

The solution was to introduce **P** (processor) -- a logical processor that provides:

1. **A local run queue** per P (no lock needed for local operations)
2. **Local caches** (mcache, defer pool, sudog cache) to avoid global allocation locks
3. **Bounded parallelism** -- GOMAXPROCS controls the number of Ps, which bounds how many Ms can execute Go code simultaneously

The relationship:

```
G (goroutine)  -- the work to be done
M (machine)    -- the OS thread that does the work
P (processor)  -- the "ticket" that allows an M to do Go work

Rule: an M must hold a P to execute Go code.
      An M may exist without a P (blocked in syscall, idle).
      A P always has a local run queue of Gs.
```

### Worker Thread Parking/Unparking

The scheduler must balance two competing goals (from `src/runtime/proc.go`, lines 36-43):

1. Keep enough running worker threads to utilize available hardware parallelism
2. Park excessive running worker threads to conserve CPU resources and power

This is hard because scheduler state is distributed (per-P queues) and we cannot predict the future (a parked thread may be needed immediately).

---

## Part 2: The P Struct (10 min)

The P struct is defined in `src/runtime/runtime2.go`, lines 773-929.

### Key Fields

```go
// src/runtime/runtime2.go, lines 773-819
type p struct {
    id          int32
    status      uint32     // one of pidle/prunning/...
    link        puintptr
    schedtick   uint32     // incremented on every scheduler call
    syscalltick uint32     // incremented on every system call
    sysmontick  sysmontick // last tick observed by sysmon
    m           muintptr   // back-link to associated m (nil if idle)
    mcache      *mcache
    raceprocctx uintptr

    deferpool    []*_defer // pool of available defer structs
    deferpoolbuf [32]*_defer

    // Cache of goroutine ids, amortizes accesses to sched.goidgen.
    goidcache    uint64
    goidcacheend uint64

    // Queue of runnable goroutines. Accessed without lock.
    runqhead uint32
    runqtail uint32
    runq     [256]guintptr
    // runnext, if non-nil, is a runnable G that was ready'd by
    // the current G and should be run next instead of what's in
    // runq if there's time remaining in the running G's time
    // slice. It will inherit the time left in the current time
    // slice.
    runnext guintptr

    // Available G's (status == Gdead)
    gFree gList

    sudogcache []*sudog
    sudogbuf   [128]*sudog
    // ...
}
```

### The Local Run Queue

The most important part of the P is its **local run queue**:

```go
runqhead uint32
runqtail uint32
runq     [256]guintptr
```

This is a **lock-free, fixed-size circular buffer** of 256 goroutine pointers. Key properties:

- **No lock needed** for the owning P to push/pop
- **Lock-free** stealing by other Ps using atomic CAS operations
- **Fixed size of 256** -- if it overflows, half the queue is moved to the global run queue

### The runnext Slot

```go
// src/runtime/runtime2.go, lines 807-819
// runnext, if non-nil, is a runnable G that was ready'd by
// the current G and should be run next instead of what's in
// runq if there's time remaining in the running G's time
// slice. It will inherit the time left in the current time
// slice. If a set of goroutines is locked in a
// communicate-and-wait pattern, this schedules that set as a
// unit and eliminates the (potentially large) scheduling
// latency that otherwise arises from adding the ready'd
// goroutines to the end of the run queue.
//
// Note that while other P's may atomically CAS this to zero,
// only the owner P can CAS it to a valid G.
runnext guintptr
```

This is an optimization for the common producer-consumer pattern: when goroutine A sends on a channel and wakes goroutine B, B is placed in `runnext` so it runs immediately (inheriting A's time slice). This avoids the latency of going to the back of the run queue.

### P Local Caches

The P also holds various caches to reduce contention on global data structures:

- `mcache` -- memory allocator cache (avoids locking the heap for small allocations)
- `deferpool` -- pool of `_defer` structs
- `sudogcache` -- pool of `sudog` structs (used for channel operations)
- `gFree` -- pool of dead goroutines for reuse
- `goidcache` -- batch of goroutine IDs (avoids atomically incrementing a global counter for every new goroutine)

---

## Part 3: The schedt Struct -- Global Scheduler State (5 min)

The global scheduler state is held in a single `schedt` struct (variable `sched`), defined in `src/runtime/runtime2.go`, lines 931-1049.

### Key Fields

```go
// src/runtime/runtime2.go, lines 931-1003
type schedt struct {
    goidgen    atomic.Uint64
    lastpoll   atomic.Int64 // time of last network poll
    pollUntil  atomic.Int64 // time to which current poll is sleeping

    lock mutex

    midle        listHeadManual // idle m's waiting for work
    nmidle       int32          // number of idle m's
    nmidlelocked int32          // number of locked m's waiting for work
    mnext        int64          // number of m's that have been created
    maxmcount    int32          // maximum number of m's allowed (or die)

    ngsys        atomic.Int32   // number of system goroutines

    pidle        puintptr       // idle p's
    npidle       atomic.Int32
    nmspinning   atomic.Int32   // spinning M count

    // Global runnable queue.
    runq gQueue

    // Global cache of dead G's.
    gFree struct {
        lock    mutex
        stack   gList // Gs with stacks
        noStack gList // Gs without stacks
    }

    // freem is the list of m's waiting to be freed
    freem *m

    gcwaiting  atomic.Bool // gc is waiting to run
    // ...
}
```

### Global vs. Local State

| State | Where | Lock? |
|---|---|---|
| Per-P run queue | `p.runq[256]` | Lock-free (atomics) |
| Global run queue | `sched.runq` | `sched.lock` |
| Idle Ms | `sched.midle` | `sched.lock` |
| Idle Ps | `sched.pidle` | `sched.lock` |
| Dead Gs (reuse pool) | `p.gFree` (local) + `sched.gFree` (global) | Local: none; Global: `sched.gFree.lock` |

The design principle: **fast paths use local, per-P state; slow paths fall back to global state under a lock.**

---

## Part 4: The gobuf Struct -- Saved Register State (5 min)

When a goroutine is descheduled, its register state is saved into a `gobuf`:

```go
// src/runtime/runtime2.go, lines 303-322
type gobuf struct {
    // The offsets of sp, pc, and g are known to (hard-coded in) libmach.
    sp   uintptr
    pc   uintptr
    g    guintptr
    ctxt unsafe.Pointer
    lr   uintptr
    bp   uintptr // for framepointer-enabled architectures
}
```

This is stored in `g.sched` for each goroutine. The key operations:

- **Save context:** When a goroutine yields (via `gopark`, `Gosched`, or preemption), its SP, PC, and BP are written to `g.sched`.
- **Restore context:** When the scheduler picks a goroutine to run, `gogo(&gp.sched)` loads SP, PC, and BP from the gobuf and jumps to the saved PC.

Why only 6 fields? The Go compiler cooperates with the scheduler to ensure that at any point where a goroutine can be descheduled (a "safe point"), all live values are on the stack, not in registers. This means the context switch only needs to save the stack pointer and program counter -- the values themselves are preserved on the goroutine's stack.

---

## Part 5: The Scheduling Loop (15 min)

### schedule()

The scheduling loop is the heart of the runtime. Every M that is ready to do work calls `schedule()`, which finds a runnable goroutine and starts executing it. From `src/runtime/proc.go`, lines 4135-4231:

```go
// src/runtime/proc.go, lines 4135-4231
func schedule() {
    mp := getg().m

    if mp.locks != 0 {
        throw("schedule: holding locks")
    }

    if mp.lockedg != 0 {
        stoplockedm()
        execute(mp.lockedg.ptr(), false) // Never returns.
    }

    // We should not schedule away from a g that is executing a cgo call,
    // since the cgo call is using the m's g0 stack.
    if mp.incgo {
        throw("schedule: in cgo")
    }

top:
    pp := mp.p.ptr()
    pp.preempt = false

    // Safety check: if we are spinning, the run queue should be empty.
    if mp.spinning && (pp.runnext != 0 || pp.runqhead != pp.runqtail) {
        throw("schedule: spinning with local work")
    }

    gp, inheritTime, tryWakeP := findRunnable() // blocks until work is available

    // This thread is going to run a goroutine and is not spinning anymore,
    // so if it was marked as spinning we need to reset it now and
    // potentially start a new spinning M.
    if mp.spinning {
        resetspinning()
    }

    // ...disabled scheduling, locked-g handling...

    execute(gp, inheritTime)
}
```

### The Flow

```
schedule()
    |
    +--> findRunnable()     // find a G to run (may block)
    |       |
    |       +--> returns (gp, inheritTime, tryWakeP)
    |
    +--> resetspinning()    // if we were spinning, stop
    |
    +--> execute(gp, inheritTime)  // start running gp
            |
            +--> gogo(&gp.sched)   // restore context, jump to user code
                                    // NEVER RETURNS to schedule()
```

Important: `execute()` never returns. When the goroutine eventually yields, the M will re-enter `schedule()` from a different path (via `mcall` which switches to g0 and calls the appropriate handler).

### execute()

From `src/runtime/proc.go`, lines 3331-3383:

```go
// src/runtime/proc.go, lines 3331-3383
func execute(gp *g, inheritTime bool) {
    mp := getg().m

    // Assign gp.m before entering _Grunning so running Gs have an M.
    mp.curg = gp
    gp.m = mp
    casgstatus(gp, _Grunnable, _Grunning)
    gp.waitsince = 0
    gp.preempt = false
    gp.stackguard0 = gp.stack.lo + stackGuard
    if !inheritTime {
        mp.p.ptr().schedtick++
    }

    // Check whether the profiler needs to be turned on or off.
    hz := sched.profilehz
    if mp.profilehz != hz {
        setThreadCPUProfiler(hz)
    }

    trace := traceAcquire()
    if trace.ok() {
        trace.GoStart()
        traceRelease(trace)
    }

    gogo(&gp.sched)
}
```

Key steps:
1. Bind M and G together (`mp.curg = gp; gp.m = mp`)
2. Transition status: `_Grunnable` -> `_Grunning`
3. Reset preemption state
4. Increment `schedtick` if this is a new time slice (not inherited)
5. Call `gogo()` to restore the goroutine's saved context and jump to user code

---

## Part 6: findRunnable() -- Finding Work (15 min)

`findRunnable()` is the most complex function in the scheduler. It implements a priority-ordered search for work. From `src/runtime/proc.go`, lines 3389-3658.

### The Priority Order

```go
// src/runtime/proc.go, lines 3389-3538
func findRunnable() (gp *g, inheritTime, tryWakeP bool) {
    mp := getg().m

top:
    pp := mp.p.ptr()
    if sched.gcwaiting.Load() {
        gcstopm()                           // 0. GC is running, stop
        goto top
    }

    // ...timer checks...

    // 1. Try to schedule the trace reader.
    if traceEnabled() || traceShuttingDown() {
        gp := traceReader()
        if gp != nil { return gp, false, true }
    }

    // 2. Try to schedule a GC worker.
    if gcBlackenEnabled != 0 {
        gp, tnow := gcController.findRunnableGCWorker(pp, now)
        if gp != nil { return gp, false, true }
    }

    // 3. Check global runnable queue once in a while (fairness).
    if pp.schedtick%61 == 0 && !sched.runq.empty() {
        lock(&sched.lock)
        gp := globrunqget()
        unlock(&sched.lock)
        if gp != nil { return gp, false, false }
    }

    // 4. Wake up finalizer/cleanup goroutines if needed.
    // ...

    // 5. Local run queue.
    if gp, inheritTime := runqget(pp); gp != nil {
        return gp, inheritTime, false
    }

    // 6. Global run queue (take a batch).
    if !sched.runq.empty() {
        lock(&sched.lock)
        gp, q := globrunqgetbatch(int32(len(pp.runq)) / 2)
        unlock(&sched.lock)
        if gp != nil {
            runqputbatch(pp, &q)
            return gp, false, false
        }
    }

    // 7. Poll network (non-blocking).
    if netpollinited() && netpollAnyWaiters() && ... {
        list, delta := netpoll(0)
        if !list.empty() {
            gp := list.pop()
            injectglist(&list)
            return gp, false, false
        }
    }

    // 8. Steal work from other Ps.
    if mp.spinning || 2*sched.nmspinning.Load() < gomaxprocs-sched.npidle.Load() {
        if !mp.spinning { mp.becomeSpinning() }
        gp, inheritTime, tnow, w, newWork := stealWork(now)
        if gp != nil { return gp, inheritTime, false }
    }

    // 9. Try idle-time GC marking.
    // ...

    // 10. Give up: release P, park the M.
    lock(&sched.lock)
    // ...re-check global queue, needspinning...
    releasep()
    pidleput(pp, now)
    unlock(&sched.lock)
    // ...park the M, wait for wakeup...
}
```

### The Priority Order Summarized

| Priority | Source | Lock Required? | Notes |
|---|---|---|---|
| 0 | GC stop-the-world | - | Stop if GC is waiting |
| 1 | Trace reader | - | Special system goroutine |
| 2 | GC mark worker | - | Assist the garbage collector |
| 3 | Global queue (1/61 chance) | `sched.lock` | Fairness: prevent local queue starvation |
| 4 | Finalizer/cleanup Gs | - | Wake if pending |
| 5 | **Local run queue** | None (atomics) | **Fast path -- most common** |
| 6 | Global run queue (batch) | `sched.lock` | Take up to len(runq)/2 goroutines |
| 7 | Network poller | None | Non-blocking poll for I/O completions |
| 8 | **Work stealing** | None (atomics) | Steal from another P's local queue |
| 9 | Idle GC marking | - | Use idle time for GC work |
| 10 | Park the M | `sched.lock` | Release P, sleep until woken |

### The 1/61 Fairness Check

```go
// src/runtime/proc.go, line 3443
if pp.schedtick%61 == 0 && !sched.runq.empty() {
```

Every 61st scheduling decision, the scheduler checks the global queue *before* the local queue. Without this, two goroutines on the same P that continuously spawn each other could starve goroutines on the global queue indefinitely. The number 61 is prime (to avoid resonance with other periodicities) and represents a balance between fairness and fast-path efficiency.

### Work Stealing

When the local queue is empty and the global queue is empty, the scheduler attempts to **steal** work from other Ps:

```go
// src/runtime/proc.go, line 3522
gp, inheritTime, tnow, w, newWork := stealWork(now)
```

`stealWork` iterates over all Ps in a random order and attempts to steal half of another P's local run queue. This is the key mechanism for load balancing across cores. The number of spinning Ms is limited to half the busy Ps to avoid excessive CPU consumption:

```go
// src/runtime/proc.go, line 3517
if mp.spinning || 2*sched.nmspinning.Load() < gomaxprocs-sched.npidle.Load() {
```

---

## Part 7: Goroutine Lifecycle (15 min)

### Creation: newproc / newproc1

When Go code executes `go f()`, the compiler emits a call to `newproc`:

```go
// src/runtime/proc.go, lines 5295-5308
func newproc(fn *funcval) {
    gp := getg()
    pc := sys.GetCallerPC()
    systemstack(func() {
        newg := newproc1(fn, gp, pc, false, waitReasonZero)

        pp := getg().m.p.ptr()
        runqput(pp, newg, true)

        if mainStarted {
            wakep()
        }
    })
}
```

Steps:
1. Switch to the system stack (g0)
2. Call `newproc1` to allocate and initialize a new G
3. Put the new G on the current P's local run queue via `runqput`, with `next=true` (so it goes into `runnext`)
4. If the runtime is past initialization, call `wakep()` to wake an idle M if one exists

`newproc1` does the detailed initialization, from `src/runtime/proc.go`, lines 5313-5394:

```go
// src/runtime/proc.go, lines 5313-5357
func newproc1(fn *funcval, callergp *g, callerpc uintptr,
              parked bool, waitreason waitReason) *g {
    if fn == nil {
        fatal("go of nil func value")
    }

    mp := acquirem() // disable preemption
    pp := mp.p.ptr()
    newg := gfget(pp)        // try to reuse a dead G from the free list
    if newg == nil {
        newg = malg(stackMin) // allocate new G with 2KB stack
        casgstatus(newg, _Gidle, _Gdead)
        allgadd(newg)
    }

    // Set up the stack frame
    memclrNoHeapPointers(unsafe.Pointer(&newg.sched), unsafe.Sizeof(newg.sched))
    newg.sched.sp = sp
    newg.stktopsp = sp
    newg.sched.pc = abi.FuncPCABI0(goexit) + sys.PCQuantum
    newg.sched.g = guintptr(unsafe.Pointer(newg))
    gostartcallfn(&newg.sched, fn)
    newg.parentGoid = callergp.goid
    newg.gopc = callerpc
    newg.startpc = fn.fn
    // ...assign goid, set status to _Grunnable...
}
```

Key detail: the new goroutine's PC is set up so that when `fn` returns, it returns into `goexit`, which performs cleanup. The `gostartcallfn` call arranges the stack frame so that `fn` appears to have been called by `goexit`.

Another key detail: `gfget(pp)` tries to **reuse** a dead goroutine from the P's local free list (`p.gFree`) before allocating a new one. This amortizes allocation cost for programs that create and destroy many goroutines.

### Execution: execute

Covered above in Part 5. The key transition: `_Grunnable` -> `_Grunning`, then `gogo(&gp.sched)` restores context and begins executing user code.

### Blocking: gopark / park_m

`gopark` and `goready` (below) are the runtime's universal blocking and waking primitives. Channels (Module 7), mutexes (Module 6), and I/O (Module 10) all ultimately use `gopark` to suspend a goroutine and `goready` to wake it. Understanding this pair is key to understanding all blocking operations in Go.

When a goroutine needs to block (channel operation, mutex, sleep, etc.), it calls `gopark`:

```go
// src/runtime/proc.go, lines 445-463
func gopark(unlockf func(*g, unsafe.Pointer) bool, lock unsafe.Pointer,
            reason waitReason, traceReason traceBlockReason, traceskip int) {
    if reason != waitReasonSleep {
        checkTimeouts()
    }
    mp := acquirem()
    gp := mp.curg
    status := readgstatus(gp)
    if status != _Grunning && status != _Gscanrunning {
        throw("gopark: bad g status")
    }
    mp.waitlock = lock
    mp.waitunlockf = unlockf
    gp.waitreason = reason
    mp.waitTraceBlockReason = traceReason
    mp.waitTraceSkip = traceskip
    releasem(mp)
    // can't do anything that might move the G between Ms here.
    mcall(park_m)
}
```

`gopark` stores the wait reason and unlock function, then calls `mcall(park_m)`. `mcall` switches from the user goroutine's stack to the M's g0 stack and calls `park_m`:

```go
// src/runtime/proc.go, lines 4253-4302
func park_m(gp *g) {
    mp := getg().m

    trace := traceAcquire()
    if trace.ok() {
        trace.GoPark(mp.waitTraceBlockReason, mp.waitTraceSkip)
    }
    casgstatus(gp, _Grunning, _Gwaiting)
    if trace.ok() {
        traceRelease(trace)
    }

    dropg()   // disconnect G from M

    if fn := mp.waitunlockf; fn != nil {
        ok := fn(gp, mp.waitlock)
        mp.waitunlockf = nil
        mp.waitlock = nil
        if !ok {
            // The unlock function said "don't park after all"
            casgstatus(gp, _Gwaiting, _Grunnable)
            execute(gp, true) // Schedule it back, never returns.
        }
    }

    // G is now parked. M calls schedule() to find new work.
    schedule()
}
```

Key steps:
1. Transition status: `_Grunning` -> `_Gwaiting`
2. `dropg()`: disconnect G from M (`m.curg = nil; g.m = nil`)
3. Call the unlock function (e.g., release a channel lock). If it returns `false`, the park is cancelled and the goroutine resumes.
4. Call `schedule()` to find the next goroutine to run

The unlock function pattern is important: it allows the runtime to atomically release a lock and park, avoiding a race where another goroutine could wake us before we are fully parked.

### Waking: goready / ready

When a blocked goroutine should be woken (e.g., a value arrives on a channel), `goready` is called:

```go
// src/runtime/proc.go, lines 481-484
func goready(gp *g, traceskip int) {
    systemstack(func() {
        ready(gp, traceskip, true)
    })
}
```

Which calls `ready`:

```go
// src/runtime/proc.go, lines 1120-1140
func ready(gp *g, traceskip int, next bool) {
    status := readgstatus(gp)

    // Mark runnable.
    mp := acquirem()
    if status&^_Gscan != _Gwaiting {
        dumpgstatus(gp)
        throw("bad g->status in ready")
    }

    // status is Gwaiting or Gscanwaiting, make Grunnable and put on runq
    trace := traceAcquire()
    casgstatus(gp, _Gwaiting, _Grunnable)
    if trace.ok() {
        trace.GoUnpark(gp, traceskip)
        traceRelease(trace)
    }
    runqput(mp.p.ptr(), gp, next)
    wakep()
    releasem(mp)
}
```

Key steps:
1. Transition status: `_Gwaiting` -> `_Grunnable`
2. Put the goroutine on the current P's local run queue via `runqput` with `next=true`, meaning it goes into `runnext` for immediate scheduling
3. Call `wakep()` to wake an idle M, since there is now new work available

### Exit: goexit0 / gdestroy

When a goroutine's function returns, it returns into `goexit` (which was set up during creation). The cleanup happens in `goexit0`:

```go
// src/runtime/proc.go, lines 4491-4501
func goexit0(gp *g) {
    if goexperiment.RuntimeSecret && gp.secret > 0 {
        memclrNoHeapPointers(unsafe.Pointer(gp.stack.lo),
            gp.stack.hi-gp.stack.lo)
    }
    gdestroy(gp)
    schedule()
}
```

And `gdestroy` does the actual cleanup, from `src/runtime/proc.go`, lines 4503-4539:

```go
// src/runtime/proc.go, lines 4503-4539
func gdestroy(gp *g) {
    mp := getg().m
    pp := mp.p.ptr()

    casgstatus(gp, _Grunning, _Gdead)
    gcController.addScannableStack(pp, -int64(gp.stack.hi-gp.stack.lo))
    if isSystemGoroutine(gp, false) {
        sched.ngsys.Add(-1)
    }
    gp.m = nil
    locked := gp.lockedm != 0
    gp.lockedm = 0
    mp.lockedg = 0
    gp.preemptStop = false
    gp.paniconfault = false
    gp._defer = nil
    gp._panic = nil
    gp.writebuf = nil
    gp.waitreason = waitReasonZero
    gp.param = nil
    gp.labels = nil
    gp.timer = nil

    dropg()
    // ...put gp on free list for reuse (gfput)...
}
```

Key steps:
1. Transition status: `_Grunning` -> `_Gdead`
2. Clear all goroutine-specific state (defers, panics, labels, etc.)
3. `dropg()`: disconnect G from M
4. Put the G on the P's free list (`gfput`) for reuse by `newproc1`
5. Call `schedule()` to find the next goroutine

### Complete Lifecycle Diagram

```
                    newproc()
                       |
                   newproc1() -- allocate G, set up stack
                       |
                  runqput(pp, newg, true) -- place on runnext
                       |
                       v
              ┌── _Grunnable ◄────────────────────────┐
              |        |                               |
              |   schedule() -> findRunnable()         |
              |        |                               |
              |   execute()                        ready()
              |        |                          [goready]
              |   casgstatus -> _Grunning              |
              |   gogo(&gp.sched)                      |
              |        |                               |
              |   ┌────┴────┐                          |
              |   |         |                          |
              | gopark   function                      |
              |   |     returns                        |
              |   v         |                          |
              | _Gwaiting   |                          |
              |   |         |              ────────────┘
              |   |         v
              |   |     goexit0()
              |   |         |
              |   |    casgstatus -> _Gdead
              |   |         |
              |   |    gfput() -- place on free list
              |   |         |
              |   |    schedule() -- find next G
              |   |
              |   └─── (woken by another G) ───► _Grunnable
              |
              └─────────── (stolen by another P)
```

---

## Part 8: Run Queue Operations (10 min)

### runqput -- Adding to the Local Queue

From `src/runtime/proc.go`, lines 7478-7520:

```go
// src/runtime/proc.go, lines 7478-7520
func runqput(pp *p, gp *g, next bool) {
    if !haveSysmon && next {
        // Without sysmon, avoid runnext to prevent starvation
        next = false
    }
    if randomizeScheduler && next && randn(2) == 0 {
        next = false
    }

    if next {
    retryNext:
        oldnext := pp.runnext
        if !pp.runnext.cas(oldnext, guintptr(unsafe.Pointer(gp))) {
            goto retryNext
        }
        if oldnext == 0 {
            return
        }
        // Kick the old runnext out to the regular run queue.
        gp = oldnext.ptr()
    }

retry:
    h := atomic.LoadAcq(&pp.runqhead) // load-acquire
    t := pp.runqtail
    if t-h < uint32(len(pp.runq)) {
        pp.runq[t%uint32(len(pp.runq))].set(gp)
        atomic.StoreRel(&pp.runqtail, t+1) // store-release
        return
    }
    if runqputslow(pp, gp, h, t) {
        return
    }
    goto retry
}
```

The logic:
1. If `next=true`, place the goroutine in `runnext` (CAS loop). If there was already something in `runnext`, kick it to the regular queue.
2. Try to add to the circular buffer (`runq[t % 256]`). Uses load-acquire on head and store-release on tail for lock-free producer-consumer.
3. If the buffer is full (256 entries), call `runqputslow` which moves half the queue to the global run queue.

### runqputslow -- Overflow to Global Queue

From `src/runtime/proc.go`, lines 7524-7559:

```go
// src/runtime/proc.go, lines 7524-7559
func runqputslow(pp *p, gp *g, h, t uint32) bool {
    var batch [len(pp.runq)/2 + 1]*g

    // Grab a batch from local queue.
    n := t - h
    n = n / 2
    for i := uint32(0); i < n; i++ {
        batch[i] = pp.runq[(h+i)%uint32(len(pp.runq))].ptr()
    }
    if !atomic.CasRel(&pp.runqhead, h, h+n) {
        return false
    }
    batch[n] = gp

    // Link the goroutines and put on global queue.
    for i := uint32(0); i < n; i++ {
        batch[i].schedlink.set(batch[i+1])
    }
    lock(&sched.lock)
    globrunqputbatch(&q)
    unlock(&sched.lock)
    return true
}
```

When the local queue is full, **half** of it (128 goroutines) plus the new goroutine are moved to the global queue. This serves dual purposes:
1. Makes room in the local queue
2. Makes work available for other Ps to find (load balancing)

### runqget -- Taking from the Local Queue

From `src/runtime/proc.go`, lines 7598-7619:

```go
// src/runtime/proc.go, lines 7598-7619
func runqget(pp *p) (gp *g, inheritTime bool) {
    // If there's a runnext, it's the next G to run.
    next := pp.runnext
    // If the runnext is non-0 and the CAS fails, it could only have
    // been stolen by another P, because other Ps can race to set
    // runnext to 0, but only the current P can set it to non-0.
    if next != 0 && pp.runnext.cas(next, 0) {
        return next.ptr(), true
    }

    for {
        h := atomic.LoadAcq(&pp.runqhead)
        t := pp.runqtail
        if t == h {
            return nil, false
        }
        gp := pp.runq[h%uint32(len(pp.runq))].ptr()
        if atomic.CasRel(&pp.runqhead, h, h+1) {
            return gp, false
        }
    }
}
```

Key details:
1. **Check `runnext` first.** If set, take it and return `inheritTime=true` (the goroutine inherits the current time slice).
2. **Then check the circular buffer.** FIFO order (take from head). Uses CAS on head to handle concurrent stealers.
3. The `inheritTime` flag is important: a goroutine from `runnext` shares the time slice with the goroutine that readied it, while a goroutine from the regular queue gets a fresh time slice.

---

## Part 9: The runnext Optimization (5 min)

The `runnext` optimization deserves special attention because it is central to Go's performance for channel-based patterns.

### The Problem

Consider a producer-consumer pattern:

```go
func producer(ch chan int) {
    for i := 0; i < 1000000; i++ {
        ch <- i   // wake consumer, then block
    }
}

func consumer(ch chan int) {
    for v := range ch {
        process(v) // process, then block waiting for next
    }
}
```

Without `runnext`, when the producer sends:
1. Consumer is woken, placed at the **back** of the run queue
2. Producer blocks
3. Other goroutines in the queue run first
4. Eventually consumer runs, processes value, blocks again
5. Repeat with high scheduling latency

### The Solution

With `runnext`:
1. Consumer is woken, placed in `runnext` (front of queue)
2. Producer blocks
3. Consumer runs **immediately** (inherits producer's time slice)
4. Consumer processes value, sends result/blocks
5. Very low scheduling latency

From the comment in `src/runtime/runtime2.go`, lines 807-818:

> If a set of goroutines is locked in a communicate-and-wait pattern, this schedules that set as a unit and eliminates the (potentially large) scheduling latency that otherwise arises from adding the ready'd goroutines to the end of the run queue.

### The Starvation Guard

There is a subtle starvation risk: if two goroutines keep waking each other via `runnext`, they could monopolize the P forever. The `sysmon` thread (system monitor) guards against this by preempting goroutines that run too long. From `runqput`:

```go
// src/runtime/proc.go, lines 7479-7488
if !haveSysmon && next {
    // A runnext goroutine shares the same time slice as the
    // current goroutine. To prevent a ping-pong pair from
    // starving all others, we depend on sysmon to preempt
    // "long-running goroutines".
    next = false
}
```

On platforms without `sysmon` (like Wasm), `runnext` is disabled entirely to prevent starvation.

---

## Key Takeaways

1. **The GMP model** separates concerns: G is the work, M is the thread, P is the scheduling context. P limits parallelism and provides per-core caches.

2. **The P struct** holds a local run queue (256-slot lock-free ring buffer), a `runnext` slot for producer-consumer optimization, and various caches to avoid global locks.

3. **The scheduling loop** (`schedule` -> `findRunnable` -> `execute`) searches for work in priority order: GC work > global queue (1/61 fairness) > local queue > global queue (batch) > network poll > work stealing > idle GC > park.

4. **Goroutine lifecycle:** `newproc1` creates and enqueues; `execute` transitions to `_Grunning`; `gopark`/`park_m` blocks with `_Gwaiting`; `goready`/`ready` wakes to `_Grunnable`; `goexit0`/`gdestroy` cleans up to `_Gdead` and recycles.

5. **The `runnext` optimization** enables efficient producer-consumer scheduling by giving the woken goroutine the current time slice.

6. **Load balancing** happens through work stealing (take half of another P's queue) and overflow (when local queue is full, half goes to global).

---

## Discussion Questions

1. Why is `findRunnable`'s priority order designed the way it is? Why does GC work come before user goroutines?

2. The global queue fairness check uses `schedtick%61`. What would happen if this were `schedtick%2` (too frequent) or `schedtick%10000` (too rare)?

3. When a goroutine makes a blocking system call, the P is detached from the M. How does the runtime know when the syscall completes? (Hint: look at `exitsyscall` and `sysmon`.)

4. Why does `runqputslow` move **half** the queue to the global queue instead of just the overflowing goroutine? What would happen with a different strategy?

5. The `runnext` optimization trades fairness for latency. In what scenarios might you want to disable it?

---

## Further Reading

- Go source: `src/runtime/proc.go` (scheduler implementation)
- Go source: `src/runtime/runtime2.go` (data structures)
- Design document: https://golang.org/s/go11sched (Scalable Go Scheduler)
- `src/runtime/proc.go` lines 36-97: detailed comment on worker thread parking/unparking strategy
