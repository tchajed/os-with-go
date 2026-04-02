# Module 3: Processes, Threads, and Goroutines

**Duration:** 60 minutes

---

## Background: From Processes to Lightweight Threads

The history of concurrent execution in operating systems is a story of
progressively reducing overhead. Early computers in the 1950s and 1960s ran one
program at a time in **batch processing** mode: a job was loaded, it ran to
completion, and the next job was loaded. The insight that CPUs spent most of
their time waiting for I/O led to **multiprogramming** in systems like IBM
OS/360 and Multics, where multiple programs could be resident in memory
simultaneously, and the OS would switch between them when one blocked on I/O.
Each program ran as a **process** with its own protected address space, file
descriptors, and kernel metadata. This isolation was essential for safety, but
it came at a cost: creating a process meant duplicating page tables, allocating
kernel data structures, and flushing the TLB. By the 1980s, researchers
recognized that many concurrent activities within the same application did not
need full isolation. This led to the development of **threads**---multiple
execution contexts sharing a single address space. The POSIX threads
(pthreads) standard, formalized in 1995 as IEEE 1003.1c, gave Unix systems a
portable API for kernel-managed threads, each with its own stack (typically 1--8
MB), register set, and scheduling state.

Linux took a distinctive approach to this problem. Rather than implementing
threads as a fundamentally different abstraction from processes, Linux unified
them under a single kernel data structure: `task_struct`. The `clone()` system
call, introduced in Linux 2.0 (1996), accepts a bitmask of flags that specify
exactly which resources the new task should share with its parent---address
space (`CLONE_VM`), file descriptors (`CLONE_FILES`), signal handlers
(`CLONE_SIGHAND`), and more. A "process" is simply a `clone()` call that
shares nothing; a "thread" is a `clone()` call that shares (almost)
everything. The kernel scheduler treats both identically. This design is
elegant and flexible, and it is why the Go runtime can call `clone()` directly
on Linux to create OS threads, bypassing the pthreads library entirely and
retaining full control over stack placement and signal masks.

Even with threads being cheaper than processes, OS threads remain expensive at
scale. Each thread requires a kernel scheduling entity, a fixed-size stack (8
MB of virtual address space on Linux by default, 512 KB on macOS), and context
switches that cost 1--10 microseconds due to kernel entry/exit, register
save/restore, and cache/TLB disruption. Dan Kegel's influential "C10K problem"
essay (1999) crystallized the challenge: how do you handle 10,000 simultaneous
network connections on a single server? A thread-per-connection model buckles
under the weight of memory consumption and scheduling overhead at that scale.
The problem has only intensified in the era of microservices and cloud
computing, where C10M (ten million connections) is the new frontier.

This tension between the *convenience* of threads (a sequential programming
model for each task) and their *cost* has driven a decades-long search for
lightweight alternatives. Solaris 2 introduced **green threads** in the early
1990s, multiplexing user-level threads onto a smaller number of kernel threads.
The GNU Portable Threads library (Pth) offered cooperative user-level threading
on any Unix system. Erlang, designed for telecom switches, built its entire
runtime around ultra-lightweight **processes** (not OS processes) that could
number in the millions. More recently, Java's **Project Loom** (finalized in
Java 21, 2023) introduced virtual threads that are scheduled by the JVM rather
than the OS. Rust's async/await model compiles asynchronous tasks into
state machines that are driven by a user-space executor. Each of these
approaches embodies the same core idea: decouple the unit of concurrent work
from the OS thread, so that the programmer can think in terms of tasks while
the runtime manages the mapping to hardware.

Go's answer to this problem is the **goroutine**: a user-level thread that
starts with a 2 KB stack, is scheduled cooperatively (with asynchronous
preemption added in Go 1.14), and can be created in about a microsecond. The
Go runtime multiplexes potentially millions of goroutines onto a small pool of
OS threads, handling all the complexity of stack growth, blocking system calls,
and work stealing behind the scenes. This module examines how that machinery
works, starting from the OS primitives (processes and threads) that form the
foundation, then diving into Go's runtime representations of OS threads (the
`m` struct) and goroutines (the `g` struct), and finally tracing the lifecycle
of a goroutine through its state transitions.

---

## Learning Objectives

By the end of this module, students will be able to:

1. Explain the difference between OS processes, OS threads, and goroutines
2. Describe the cost tradeoffs between OS threads and goroutines
3. Read and understand the `m` struct (OS thread) and `g` struct (goroutine) in the Go runtime
4. Trace how Go creates OS threads on Linux (clone) and Darwin (pthread_create)
5. Enumerate goroutine states and explain their transitions

---

## Part 1: OS Processes (10 min)

### What is a Process?

A **process** is the OS's unit of resource ownership and protection. Each process has:

- **Address space:** A virtual memory mapping (text, data, heap, stack segments) isolated from other processes via page tables.
- **File descriptor table:** An array of open file handles (files, sockets, pipes) private to the process.
- **Process table entry:** Kernel metadata including PID, parent PID, credentials (uid/gid), signal handlers, resource limits, and scheduling state.
- **One or more threads of execution.**

### Process Creation

On Unix, processes are created with `fork()`, which duplicates the entire address space (copy-on-write in practice), followed by `exec()` to load a new program image. This is expensive:

- Page table duplication
- File descriptor table duplication
- Kernel data structure allocation
- TLB flush on the new process

### Why Processes Are Too Heavy for Concurrency

If you want 10,000 concurrent activities, creating 10,000 processes is impractical:
- Each gets its own address space (page tables, memory mappings)
- Communication requires IPC (pipes, shared memory, sockets) -- all involve kernel transitions
- Context switching between processes requires TLB flushes

This motivates **threads**: multiple execution contexts sharing a single address space.

---

## Part 2: OS Threads (10 min)

### What is a Thread?

A **thread** (sometimes called a "kernel thread" or "OS thread") is an independent execution context within a process. Threads within the same process share:

- Address space (code, heap, global data)
- File descriptor table
- Signal handlers
- Process ID

Each thread has its own:

- **Stack** (typically 1-8 MB, allocated by the OS)
- **Register set** (saved/restored on context switch)
- **Thread ID**
- **Signal mask**
- **errno** (on Linux, thread-local storage)

### Why OS Threads Are Expensive

There are three major costs:

**1. Stack size (1-8 MB per thread)**

The OS must allocate a fixed-size stack for each thread at creation time. The default on Linux is typically 8 MB (though only pages actually touched are physically allocated). On macOS, the default pthread stack is 512 KB for secondary threads. Even with lazy allocation, the virtual address space is reserved upfront. With 10,000 threads, that is 10-80 GB of virtual address space for stacks alone.

**2. Context switch cost (~1-10 microseconds)**

Switching between OS threads requires:
- Saving all CPU registers to kernel memory
- Switching kernel stack pointers
- Potentially flushing CPU caches and TLB entries
- Restoring registers for the new thread
- Returning from kernel mode to user mode

This takes roughly 1-10 microseconds depending on hardware and how much cache state is disrupted.

**3. Kernel involvement in every operation**

Creating, destroying, blocking, and waking threads all require system calls. The kernel must maintain scheduling data structures, and the scheduler itself runs with O(1) or O(log n) complexity per operation, but the constant factors include kernel entry/exit overhead.

### Thread Creation on Linux and Darwin

Go creates OS threads through platform-specific mechanisms. On **Linux**, it uses the `clone()` system call directly. On **Darwin** (macOS), it uses `pthread_create()`.

**Linux: `newosproc` calls `clone()`**

From `src/runtime/os_linux.go`, lines 170-201:

```go
func newosproc(mp *m) {
    stk := unsafe.Pointer(mp.g0.stack.hi)
    // Disable signals during clone, so that the new thread starts
    // with signals disabled. It will enable them in minit.
    var oset sigset
    sigprocmask(_SIG_SETMASK, &sigset_all, &oset)
    ret := retryOnEAGAIN(func() int32 {
        r := clone(cloneFlags, stk, unsafe.Pointer(mp),
            unsafe.Pointer(mp.g0), unsafe.Pointer(abi.FuncPCABI0(mstart)))
        if r >= 0 {
            return 0
        }
        return -r
    })
    sigprocmask(_SIG_SETMASK, &oset, nil)
    // ...error handling...
}
```

The actual `clone` system call is issued from assembly in `src/runtime/sys_linux_amd64.s`, lines 574-623:

```asm
TEXT runtime·clone(SB),NOSPLIT|NOFRAME,$0
    MOVL    flags+0(FP), DI
    MOVQ    stk+8(FP), SI
    MOVQ    $0, DX
    MOVQ    $0, R10
    MOVQ    $0, R8
    // Copy mp, gp, fn off parent stack for use by child.
    MOVQ    mp+16(FP), R13
    MOVQ    gp+24(FP), R9
    MOVQ    fn+32(FP), R12
    // ...TLS setup...
    MOVL    $SYS_clone, AX
    SYSCALL
```

Key observation: the `clone()` call receives the flags, the new stack pointer, and pointers to the `m` and `g0` structs. The child thread starts executing at `mstart`, which enters the scheduler loop.

**Darwin: `newosproc` calls `pthread_create()`**

From `src/runtime/os_darwin.go`, lines 224-258:

```go
func newosproc(mp *m) {
    stk := unsafe.Pointer(mp.g0.stack.hi)
    // Initialize an attribute object.
    var attr pthreadattr
    var err int32
    err = pthread_attr_init(&attr)
    // ...
    // Find out OS stack size for our own stack guard.
    var stacksize uintptr
    if pthread_attr_getstacksize(&attr, &stacksize) != 0 { ... }
    mp.g0.stack.hi = stacksize // for mstart

    // Tell the pthread library we won't join with this thread.
    if pthread_attr_setdetachstate(&attr, _PTHREAD_CREATE_DETACHED) != 0 { ... }

    // Finally, create the thread.
    var oset sigset
    sigprocmask(_SIG_SETMASK, &sigset_all, &oset)
    err = retryOnEAGAIN(func() int32 {
        return pthread_create(&attr, abi.FuncPCABI0(mstart_stub), unsafe.Pointer(mp))
    })
    // ...
}
```

On Darwin, Go cannot use `clone()` (that is Linux-specific), so it goes through the POSIX threads API. The thread starts at `mstart_stub`, which does low-level setup and then calls `mstart`.

---

## Part 3: The M Struct -- Go's Representation of an OS Thread (10 min)

In Go's runtime, every OS thread is represented by an `m` struct (M for "machine"). This is defined in `src/runtime/runtime2.go`, lines 618-719.

### Key Fields

```go
// src/runtime/runtime2.go, lines 618-719
type m struct {
    g0      *g     // goroutine with scheduling stack
    morebuf gobuf  // gobuf arg to morestack

    procid       uint64            // for debuggers, but offset not hard-coded
    gsignal      *g                // signal-handling g
    sigmask      sigset            // storage for saved signal mask
    tls          [tlsSlots]uintptr // thread-local storage
    mstartfn     func()
    curg         *g       // current running goroutine

    p       puintptr       // currently attached P (nil if not executing Go code)
    nextp   puintptr       // the next P to install before executing
    oldp    puintptr       // the P that was attached before executing a syscall
    id      int64
    spinning bool          // m is out of work and is actively looking for work
    blocked  bool          // m is blocked on a note
    park     note
    alllink  *m            // on allm
    schedlink muintptr
    lockedg  guintptr
    // ...
}
```

### Important Concepts

**g0: The scheduling stack.** Every M has a special goroutine called `g0` that runs scheduler code. When the scheduler needs to make decisions (find a runnable goroutine, handle a syscall return, etc.), it switches to `g0`'s stack. This is necessary because the scheduler cannot run on a user goroutine's stack -- that stack might be tiny (2 KB) and might need to be moved.

**curg: The current goroutine.** This points to the user goroutine currently executing on this thread. When `curg` is nil, the M is running scheduler code on `g0`.

**p: The attached P.** An M must have an associated P (processor) to execute Go code. The M-P binding is how the runtime controls parallelism: GOMAXPROCS sets the number of Ps, which limits how many Ms can execute Go code simultaneously.

**spinning:** When an M has no work, it may enter a "spinning" state where it actively looks for work to steal from other Ps. This avoids the latency of parking and unparking threads.

**park:** A note (futex-based synchronization primitive) used to park the M when there is no work. The M sleeps on this note and is woken when new work appears.

---

## Part 4: Goroutines -- The G Struct (15 min)

A goroutine is Go's lightweight, user-level thread. Each goroutine is represented by a `g` struct, defined in `src/runtime/runtime2.go`, lines 473-579.

### Key Fields

```go
// src/runtime/runtime2.go, lines 473-569
type g struct {
    // Stack parameters.
    // stack describes the actual stack memory: [stack.lo, stack.hi).
    // stackguard0 is the stack pointer compared in the Go stack growth
    // prologue. It is stack.lo+StackGuard normally, but can be
    // StackPreempt to trigger a preemption.
    stack       stack   // offset known to runtime/cgo
    stackguard0 uintptr // offset known to liblink
    stackguard1 uintptr // offset known to liblink

    _panic    *_panic // innermost panic
    _defer    *_defer // innermost defer
    m         *m      // current m; offset known to arm liblink
    sched     gobuf   // saved context (sp, pc, etc.) for scheduling
    syscallsp uintptr // if status==Gsyscall, syscallsp = sched.sp
    syscallpc uintptr // if status==Gsyscall, syscallpc = sched.pc

    param        unsafe.Pointer
    atomicstatus atomic.Uint32
    goid         uint64
    schedlink    guintptr
    waitsince    int64      // approx time when the g became blocked
    waitreason   waitReason // if status==Gwaiting

    preempt       bool // preemption signal
    preemptStop   bool // transition to _Gpreempted on preemption

    lockedm   muintptr       // locked to this m
    parentGoid uint64         // goid of goroutine that created this goroutine
    gopc       uintptr        // pc of go statement that created this goroutine
    startpc    uintptr        // pc of goroutine function

    waiting    *sudog         // sudog structures this g is waiting on
    timer      *timer         // cached timer for time.Sleep
    // ...
}
```

### The Stack

```go
// src/runtime/runtime2.go, lines 460-465
type stack struct {
    lo uintptr
    hi uintptr
}
```

Goroutine stacks start small. From `src/runtime/stack.go`, line 78:

```go
// The minimum size of stack used by Go code
stackMin = 2048
```

That is **2 KB** -- compare this to the 1-8 MB default for OS thread stacks. Goroutine stacks grow dynamically: every function prologue checks if the stack needs to grow. If it does, the runtime allocates a new, larger stack (typically 2x), copies the old stack contents, and updates all pointers. This is called a **copyable stack** or **segmented stack** (Go used segmented stacks historically but switched to contiguous, copyable stacks in Go 1.4).

### The gobuf: Saved Execution Context

```go
// src/runtime/runtime2.go, lines 303-322
type gobuf struct {
    sp   uintptr
    pc   uintptr
    g    guintptr
    ctxt unsafe.Pointer
    lr   uintptr
    bp   uintptr // for framepointer-enabled architectures
}
```

When a goroutine is descheduled, its execution state (stack pointer, program counter, base pointer) is saved into `g.sched` (a `gobuf`). When it is scheduled again, these registers are restored. This is the core of user-level context switching -- no kernel involvement needed.

Contrast with an OS context switch, which must save/restore:
- All general-purpose registers (16 on amd64)
- Floating-point / SSE / AVX state (potentially hundreds of bytes)
- Kernel stack pointer
- Various control registers

The goroutine context switch saves only 6 values. The Go compiler ensures that no other registers hold live values at safe points where goroutines can be descheduled.

---

## Part 5: Goroutine States (10 min)

Goroutine states are defined as constants in `src/runtime/runtime2.go`, lines 17-119. The primary states are:

### State Definitions

```go
// src/runtime/runtime2.go, lines 17-99
const (
    // _Gidle means this goroutine was just allocated and has not
    // yet been initialized.
    _Gidle = iota // 0

    // _Grunnable means this goroutine is on a run queue. It is
    // not currently executing user code. The stack is not owned.
    _Grunnable // 1

    // _Grunning means this goroutine may execute user code. The
    // stack is owned by this goroutine. It is assigned an M and
    // usually has a P.
    _Grunning // 2

    // _Gsyscall means this goroutine is executing a system call.
    // It is not executing user code. The stack is owned by this
    // goroutine. It is assigned an M.
    _Gsyscall // 3

    // _Gwaiting means this goroutine is blocked in the runtime.
    // It is not executing user code. It is not on a run queue,
    // but should be recorded somewhere (e.g., a channel wait
    // queue) so it can be ready()d when necessary.
    _Gwaiting // 4

    // _Gdead means this goroutine is currently unused. It may be
    // just exited, on a free list, or just being initialized.
    _Gdead // 6

    // _Gcopystack means this goroutine's stack is being moved.
    _Gcopystack // 8

    // _Gpreempted means this goroutine stopped itself for a
    // suspendG preemption.
    _Gpreempted // 9
)
```

### State Transition Diagram

```
                    newproc1()
    _Gidle ──────────────────────► _Gdead ─────► _Grunnable
      │                              ▲               │
      │                              │               │
      │                         goexit0()            │ execute()
      │                              │               │
      │                              │               ▼
      │                         _Grunning ◄──── _Grunning
      │                              │               │
      │                              │               │
      │                         gopark()          entersyscall()
      │                              │               │
      │                              ▼               ▼
      │                         _Gwaiting        _Gsyscall
      │                              │               │
      │                              │               │
      │                         goready()        exitsyscall()
      │                              │               │
      │                              ▼               │
      │                         _Grunnable ◄─────────┘
      │
      └──────────────────────────────────────────────┘
```

### Key State Transitions

| Transition | Function | What Happens |
|---|---|---|
| `_Gidle` -> `_Gdead` | `newproc1` | Goroutine struct allocated, not yet initialized |
| `_Gdead` -> `_Grunnable` | `newproc1` | Stack set up, placed on run queue |
| `_Grunnable` -> `_Grunning` | `execute` | M picks up G, starts executing |
| `_Grunning` -> `_Gwaiting` | `gopark`/`park_m` | G blocks (channel, mutex, sleep, etc.) |
| `_Gwaiting` -> `_Grunnable` | `goready`/`ready` | G woken up, placed back on run queue |
| `_Grunning` -> `_Gsyscall` | `entersyscall` | G enters a system call |
| `_Gsyscall` -> `_Grunnable` | `exitsyscall` | G returns from system call |
| `_Grunning` -> `_Gdead` | `goexit0` | G's function returns, G is cleaned up |

### Stack Ownership

A critical concept: **the goroutine state controls who owns the stack**.

- `_Grunning`: The goroutine owns its stack. Only it can read/write the stack.
- `_Grunnable`: The stack is not owned. The scheduler may inspect it.
- `_Gwaiting`: The stack is not owned, *except* that a channel operation may read or write parts of it under the channel lock.
- `_Gsyscall`: The goroutine owns its stack, but the P may be detached.
- `_Gcopystack`: The stack is being moved by the goroutine that initiated the copy.

This ownership model is essential for garbage collection: the GC needs to scan goroutine stacks, but can only do so when it owns the stack (i.e., when the goroutine is not in `_Grunning`).

---

## Part 6: Cost Comparison -- Goroutines vs. OS Threads (5 min)

| Property | OS Thread | Goroutine |
|---|---|---|
| **Stack size** | 1-8 MB (fixed at creation) | 2 KB initial (grows dynamically to ~1 GB) |
| **Creation cost** | ~100 microseconds (syscall + kernel alloc) | ~1 microsecond (user-space allocation) |
| **Context switch** | ~1-10 microseconds (kernel transition) | ~100-200 nanoseconds (user-space register swap) |
| **Scheduling** | Kernel scheduler (preemptive, general-purpose) | Go runtime scheduler (cooperative + async preemption) |
| **Memory overhead per unit** | ~8-64 KB kernel structures + stack | ~400 bytes `g` struct + 2 KB stack |
| **Max practical count** | ~10,000 (limited by kernel resources) | ~1,000,000+ (limited by memory) |
| **Blocking** | Blocks entire OS thread | Only blocks goroutine; M can run other Gs |

### Why This Matters

A typical web server might have 10,000 concurrent connections. With the thread-per-connection model:
- 10,000 threads * 8 MB stack = 80 GB of virtual address space
- 10,000 kernel scheduling entities
- Context switches hit kernel on every transition

With goroutine-per-connection:
- 10,000 goroutines * 2 KB stack = 20 MB of memory
- ~4 kernel threads (GOMAXPROCS) for scheduling
- Context switches stay in user space

This is the fundamental insight behind Go's concurrency model: goroutines provide the programming convenience of threads at a fraction of the cost.

---

## Key Takeaways

1. **Processes** provide isolation but are too expensive for fine-grained concurrency.
2. **OS threads** share an address space but still carry significant kernel overhead (large stacks, expensive context switches).
3. **Goroutines** are user-level threads managed entirely by the Go runtime, with 2 KB stacks and sub-microsecond context switches.
4. The `m` struct represents an OS thread; the `g` struct represents a goroutine. The runtime multiplexes many Gs onto fewer Ms.
5. Goroutine states (`_Gidle`, `_Grunnable`, `_Grunning`, `_Gsyscall`, `_Gwaiting`, `_Gdead`) control stack ownership and are central to correctness of both the scheduler and the garbage collector.
6. On Linux, Go creates OS threads with the `clone()` system call; on Darwin, with `pthread_create()`. Both paths ultimately start the new thread at `mstart`, entering the scheduler loop.

---

## Discussion Questions

1. Why does Go use `clone()` directly on Linux instead of `pthread_create()`? What control does this give the runtime?
2. If goroutines are so cheap, why does Go still need OS threads at all? Why not implement everything in user space?
3. What would happen if goroutine stacks were fixed-size (say, 1 MB) instead of dynamically growing from 2 KB? How would this affect the design of Go programs?
4. The `g` struct stores `stackguard0`, which can be set to `StackPreempt` to trigger preemption. How is this different from how the OS preempts threads? (We will explore this more in Module 5.)

---

## Further Reading

- Go source: `src/runtime/runtime2.go` (struct definitions)
- Go source: `src/runtime/os_linux.go` and `src/runtime/os_darwin.go` (thread creation)
- Go source: `src/runtime/sys_linux_amd64.s` (clone syscall wrapper)
- Go source: `src/runtime/stack.go` (stack size constants)
- Scalable Go Scheduler Design Doc: https://golang.org/s/go11sched
