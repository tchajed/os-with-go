## Module 1: The Runtime as an OS

Operating Systems Through the Go Runtime

---

## What Does an OS Do?

Three fundamental jobs:

- **Resource management** -- multiplex CPUs, memory, I/O across programs
- **Abstraction** -- hide hardware behind clean interfaces (files, processes, virtual memory)
- **Protection** -- isolate programs from each other

Every OS textbook starts here. But a *language runtime* solves the same problems.

---

## The Go Runtime: A User-Space OS

The Go runtime (~150k lines) ships in every Go binary.

It implements:
- **Scheduling** -- goroutines on OS threads
- **Memory management** -- garbage-collected heap
- **I/O multiplexing** -- integrated epoll/kqueue netpoller
- **Preemption** -- signal-based goroutine preemption
- **Stack management** -- growable, copyable stacks

---

## OS vs. Runtime: Side by Side

| OS Concept | Kernel | Go Runtime |
|---|---|---|
| Threads | `clone`/`pthread_create` | Goroutines |
| Scheduling | CFS, run queues | Work-stealing, per-P queues |
| Virtual memory | Page tables, `mmap` | GC heap, `mspan`, `mcache` |
| I/O multiplex | `epoll`, `kqueue` | Integrated netpoller |
| Preemption | Timer interrupts | `SIGURG` signals |
| Syscall mgmt | Trap table | `entersyscall`/`exitsyscall` |

---

## Why Study the Go Runtime?

- **Readable** -- mostly Go, targeted assembly only where needed
- **Self-contained** -- single `src/runtime/` directory (~250 files)
- **Runnable** -- modify, rebuild, test immediately
- **Production** -- runs at Google, Cloudflare, thousands more
- **Same problems** as a real kernel, approachable code

---

## The Runtime Source Tree

```
src/runtime/
├── proc.go           # Scheduler: schedule(), findRunnable(), sysmon
├── runtime2.go       # Core structs: g, m, p, schedt
├── malloc.go         # Memory allocator: mallocgc, size classes
├── mgc.go            # Garbage collector
├── stack.go          # Goroutine stack management
├── chan.go            # Channel implementation
├── netpoll.go         # I/O multiplexing interface
├── sys_linux_amd64.s  # Linux syscall wrappers (assembly)
├── sys_darwin_arm64.s # macOS syscall trampolines (assembly)
├── lock_futex.go      # Futex-based locks (Linux)
└── sema.go            # Semaphore implementation
```

---

## The Three Abstractions: G, M, P

From `src/runtime/proc.go` (lines 24-34):

```go
// The main concepts are:
// G - goroutine.
// M - worker thread, or machine.
// P - processor, a resource that is required to execute Go code.
//     M must have an associated P to execute Go code, however
//     it can be blocked or in a syscall w/o an associated P.
```

---

## G -- Goroutine

```go
// src/runtime/runtime2.go, line 473
type g struct {
    stack       stack   // [stack.lo, stack.hi)
    stackguard0 uintptr // for stack growth check
    _panic    *_panic
    _defer    *_defer
    m         *m        // current thread
    sched     gobuf     // saved PC, SP for context switch
    atomicstatus atomic.Uint32
    goid         uint64
    ...
}
```

A goroutine: lightweight (2-8 KB stack), user-space scheduled, growable stack.

---

## G Status: A Goroutine's Lifecycle

```
         newproc()
            │
            v
  ┌─> _Grunnable ──> _Grunning ──> _Gdead
  │        ^              │
  │        │              ├──> _Gsyscall ──┐
  │        │              │                │
  │        │              └──> _Gwaiting ──┘
  │        │                       │
  └────────┴───────────────────────┘
```

- `_Grunnable` (1) -- on a run queue, ready to run
- `_Grunning` (2) -- executing on an M with a P
- `_Gsyscall` (3) -- in a system call, M may lose its P
- `_Gwaiting` (4) -- blocked on channel, mutex, sleep

---

## M -- Machine (OS Thread)

```go
// src/runtime/runtime2.go, line 618
type m struct {
    g0       *g       // scheduling stack
    curg     *g       // current running goroutine
    p        puintptr // attached P (nil if not running Go code)
    oldp     puintptr // P before entering syscall
    spinning bool     // looking for work
    blocked  bool     // blocked on a note
    ...
}
```

- `g0`: special goroutine whose stack runs scheduler code
- Like a kernel switching to a kernel stack for interrupts

---

## P -- Processor (Logical CPU)

```go
// src/runtime/runtime2.go, line 773
type p struct {
    id          int32
    status      uint32
    m           muintptr   // back-link to associated M
    mcache      *mcache    // per-P memory cache
    // Local run queue (lock-free!)
    runqhead uint32
    runqtail uint32
    runq     [256]guintptr
    runnext  guintptr      // next G to run (cache-friendly)
    ...
}
```

- Number of Ps = `GOMAXPROCS` (default: CPU count)
- Each P has a local run queue: **no lock needed** for scheduling

---

## Why the P Exists

Before Go 1.1: only G and M. Global run queue with a single mutex.

**Problem**: every `go` statement, every scheduling decision locks the global queue.

**Solution** (Dmitry Vyukov, 2012): add P as a scheduling context.

- Each P has its own run queue (256 slots, lock-free)
- Each P has its own memory cache (`mcache`)
- Work stealing balances load across Ps

---

## How G, M, P Connect

```
    ┌─────┐     ┌─────┐     ┌─────┐
    │ P0  │     │ P1  │     │ P2  │  GOMAXPROCS=3
    │runq │     │runq │     │(idle)│
    └──┬──┘     └──┬──┘     └─────┘
       │           │
    ┌──┴──┐     ┌──┴──┐     ┌─────┐
    │ M0  │     │ M1  │     │ M2  │  (in syscall, no P)
    └──┬──┘     └──┬──┘     └──┬──┘
       │           │           │
    ┌──┴──┐     ┌──┴──┐     ┌──┴──┐
    │ G5  │     │ G12 │     │ G7  │
    │(run)│     │(run)│     │(sys)│
    └─────┘     └─────┘     └─────┘
```

An M needs a P to run Go code. Syscall-blocked Ms release their P.

---

## The Scheduler Loop (Preview)

```go
// Simplified from proc.go
func schedule() {
    gp := findRunnable()  // blocks until work found
    execute(gp)           // context switch to gp
}

func findRunnable() *g {
    // 1. Check local run queue
    // 2. Check global run queue
    // 3. Poll network
    // 4. Steal from other Ps
}
```

We'll study this in detail in Module 3.

---

## sysmon: The Watchdog Thread

- Runs on its own M, **without a P**
- Polls every 20us-10ms
- Retakes Ps from goroutines stuck in syscalls
- Preempts goroutines running too long (>10ms)
- Triggers garbage collection
- Polls the network if no other P is doing it

Analogous to a kernel's timer interrupt handler.

---

## Course Roadmap

1. **Introduction** -- The runtime as a user-space OS (today)
2. **System Calls** -- Crossing the user/kernel boundary
3. **Scheduling** -- Work-stealing, `schedule()`, `findRunnable()`
4. **Preemption** -- Cooperative + async preemption via SIGURG
5. **Memory Management** -- Allocator hierarchy, virtual memory
6. **Garbage Collection** -- Concurrent mark-sweep, write barriers
7. **Stacks** -- Growable stacks, stack copying
8. **Synchronization** -- Futexes, channels, mutexes
9. **I/O Multiplexing** -- epoll/kqueue, netpoller
10. **Tracing and Profiling** -- Observing the runtime

---

## Scale Comparison

| | OS Thread | Goroutine |
|---|---|---|
| Stack size | ~1 MB (fixed) | 2-8 KB (growable) |
| Creation cost | ~1-10 us | ~0.3 us |
| Context switch | ~1-5 us (kernel) | ~0.1 us (user space) |
| Max practical count | ~10,000 | ~1,000,000 |
| Scheduling | Kernel (preemptive) | Runtime (cooperative + async) |

---

## Hands-On: Explore the Source

```bash
# Clone the Go source
git clone https://go.googlesource.com/go ~/go-src
cd ~/go-src/src/runtime

# Read the scheduler's opening comment
head -80 proc.go

# Find the g, m, p struct definitions
grep -n "^type [gmp] struct" runtime2.go

# Count runtime source files
ls *.go | wc -l    # ~130 Go files
ls *.s  | wc -l    # ~40 assembly files
```

---

## Exercise 1: Goroutine Scale

```go
package main

import (
    "fmt"
    "runtime"
    "sync"
    "time"
)

func main() {
    var wg sync.WaitGroup
    for i := 0; i < 100_000; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            time.Sleep(time.Second)
        }()
    }
    fmt.Println("goroutines:", runtime.NumGoroutine())
    wg.Wait()
}
```

How much memory per goroutine? (Hint: check with `runtime.MemStats`)

---

## Key Takeaways

1. The Go runtime solves the **same problems** as an OS kernel: scheduling, memory management, I/O multiplexing, preemption.

2. The **G-M-P model** decouples goroutines from OS threads, enabling millions of concurrent tasks.

3. The **P** is the key innovation -- it provides per-CPU local state (run queue, memory cache) without global locking.

4. Studying the runtime gives you real systems code that is **readable, runnable, and production-quality**.

---

## Next Module: System Calls

How does the Go runtime cross into the kernel?

- The `SYSCALL` instruction on Linux
- libc trampolines on macOS
- `entersyscall` / `exitsyscall` -- how the scheduler stays informed
- VDSO: avoiding the kernel for time queries

See you in Module 2!
