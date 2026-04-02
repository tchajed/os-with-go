# Module 1: Introduction - The Runtime as an OS

**Duration:** 45 minutes

## Background: What Makes an Operating System?

At its core, an operating system is the layer of software that stands between application code and raw hardware, solving three fundamental problems. First, it *manages resources*: a machine has a finite number of CPU cores, a fixed amount of RAM, and a handful of I/O devices, yet dozens or hundreds of programs need to share them simultaneously. Second, it provides *abstraction*: rather than forcing every programmer to understand the registers of a particular NIC or the geometry of a disk, the OS exposes uniform interfaces -- files, sockets, virtual address spaces -- that hide hardware specifics. Third, it enforces *protection and isolation*: one process's bug should not be able to overwrite another's memory or starve the rest of the system of CPU time. These three pillars -- multiplexing, abstraction, and isolation -- appear in every OS textbook from Tanenbaum to OSTEP, and they have driven the design of operating systems for over half a century.

Linux is the most widely deployed kernel in the world, running on everything from Android phones to supercomputers. It is a monolithic kernel: the process scheduler, virtual memory subsystem, file system layer (VFS), device drivers, and networking stack all execute in a single, shared address space in kernel mode. The kernel has grown from roughly 10,000 lines of C in Linus Torvalds's 1991 original to well over 30 million lines of code today, with contributions from thousands of developers. Linux on x86-64 exposes over 450 system calls -- the narrow interface through which user-space programs request kernel services. When a process calls `read()`, the CPU executes a `SYSCALL` instruction that traps into kernel mode, where the kernel validates arguments, performs the I/O (perhaps scheduling the process to sleep while waiting for a disk), and eventually returns a result. Processes get the illusion of private address spaces through page tables managed by the memory subsystem. The Completely Fair Scheduler (CFS) distributes CPU time across runnable threads using a red-black tree keyed by virtual runtime. All of this machinery exists to solve the same three problems: sharing hardware, presenting clean interfaces, and keeping programs from interfering with each other.

What is less commonly appreciated is that language runtimes have been reinventing these same mechanisms in user space for decades. The Erlang BEAM virtual machine is perhaps the most striking example: it implements its own lightweight processes (a single BEAM instance can run millions of them), its own preemptive scheduler that interrupts a process after a fixed number of "reductions" (abstract units of work), per-process garbage-collected heaps, and its own I/O multiplexing layer. An Erlang application running on BEAM looks structurally more like a small operating system than a typical user-space program. The Java Virtual Machine (JVM) similarly manages its own heap with sophisticated garbage collectors (G1, ZGC, Shenandoah), maps application threads onto OS threads with its own scheduling hints, and provides a bytecode-level abstraction that decouples programs from the underlying hardware and OS. This pattern is not entirely new -- Smalltalk images in the 1970s managed their own object memory and green threads, and the Lisp machines of the 1980s ran language runtimes directly on custom hardware, blurring the line between OS and runtime entirely. Even in the world of OS research, projects like MIT's Exokernel (1995) argued that the kernel should do as little as possible, pushing resource management into application-level "library operating systems" -- a philosophy that rhymes with what modern language runtimes actually do, albeit above a conventional kernel rather than a minimal one.

The Go runtime sits squarely in this tradition. It is a substantial piece of systems software -- roughly 150,000 lines of Go and assembly -- that ships embedded in every compiled Go binary. It implements its own user-space thread scheduler (goroutines and the G-M-P model), its own memory allocator and garbage collector, its own I/O multiplexer (the "netpoller," built on top of `epoll` or `kqueue`), and its own mechanisms for preemption and synchronization. Unlike the BEAM or JVM, Go compiles to native machine code and interacts with the kernel through direct system calls rather than an interpreter or JIT, which makes its runtime a particularly transparent case study: you can trace the path from a goroutine sending on a channel all the way down to a `futex` system call, reading real production code at every layer. This module introduces the Go runtime through the lens of operating systems -- showing how the same problems that motivate kernel design (scheduling, memory management, I/O multiplexing, synchronization) reappear in the runtime, and how studying the runtime gives you a concrete, readable codebase in which to explore them.

## 1. What is an Operating System? (10 min)

An operating system performs three fundamental jobs:

1. **Resource management** -- multiplexing scarce hardware (CPUs, memory, disks, network) across many competing programs.
2. **Abstraction** -- hiding hardware details behind clean interfaces (files, processes, virtual memory) so programs don't need to know whether they're talking to an NVMe drive or a spinning disk.
3. **Protection** -- isolating programs from each other so one buggy process can't corrupt another's memory or monopolize the CPU.

Every OS textbook opens with these three pillars. What's less commonly taught is that a *language runtime* solves the same problems, just one level up the stack.

## 2. The Go Runtime as a User-Space OS (10 min)

The Go runtime is a substantial piece of systems software (~150k lines of Go and assembly) that ships inside every Go binary. It implements:

| OS Concept | Kernel Implementation | Go Runtime Implementation |
|---|---|---|
| Processes/threads | `fork`, `clone`, kernel scheduler | Goroutines (`G`), `runtime.newproc` |
| CPU scheduling | CFS (Completely Fair Scheduler), time slices, run queues | Work-stealing scheduler, per-P run queues |
| Virtual memory | Page tables, `mmap` | Garbage-collected heap, `mheap`, `mspan` |
| I/O multiplexing | `epoll`, `kqueue`, `io_uring` | Integrated netpoller (`netpoll_epoll.go`) |
| System calls | `SYSCALL` instruction, trap table | `entersyscall`/`exitsyscall` handoff |
| Preemption | Timer interrupts, signals | Cooperative + async preemption via `SIGURG` |

The analogy is not perfect -- the Go runtime runs in user space, relies on the real kernel for actual hardware access, and doesn't enforce hard protection boundaries. But the *design problems* it faces are remarkably similar to those of an OS kernel, and studying the runtime gives students a way to read real, production-quality systems code that solves the same problems covered in an OS course.

### Why study the Go runtime instead of (or alongside) a real kernel?

- **Readable**: The runtime is written mostly in Go, with targeted assembly only where necessary. Compare this to the Linux kernel's mix of C, macros, and architecture-specific assembly.
- **Self-contained**: A single `src/runtime/` directory (~250 files) contains the scheduler, memory allocator, garbage collector, and platform abstraction layer.
- **Runnable**: Students can instrument the runtime, rebuild it with `GOROOT`, and immediately run programs against their modified runtime.
- **Production-quality**: Unlike teaching kernels (xv6, PintOS), this is code that runs every Go program in production at Google, Cloudflare, and thousands of other companies.

## 3. Tour of the Runtime Source Tree (10 min)

The Go runtime lives at `src/runtime/` within the Go source tree. Here are the key files and their roles:

### Core Scheduling

| File | Role |
|---|---|
| `proc.go` | The heart of the scheduler. Contains `schedule()`, `findRunnable()`, goroutine creation (`newproc`), `entersyscall`/`exitsyscall`, `sysmon`, and the work-stealing loop. |
| `runtime2.go` | Data structure definitions for `g`, `m`, `p`, and the global `schedt` scheduler state. |
| `runtime1.go` | Runtime initialization, GOMAXPROCS, environment variable parsing. |

### Memory Management

| File | Role |
|---|---|
| `malloc.go` | The memory allocator: `mallocgc`, size classes, `mcache`/`mcentral`/`mheap` hierarchy. |
| `mheap.go` | Heap structure, page-level allocation, `mspan` management. |
| `mgc.go` | Garbage collector: concurrent mark-sweep, GC pacing, write barriers. |
| `stack.go` | Goroutine stack allocation, growth, and shrinking. |

### Platform Abstraction (System Calls)

| File | Role |
|---|---|
| `sys_linux_amd64.s` | Raw system call wrappers for Linux/amd64: `read`, `write`, `mmap`, `clone`, `futex`. |
| `sys_darwin_arm64.s` | System call trampolines for macOS/arm64 (calls through libc). |
| `os_linux.go` | Linux-specific OS interface: thread creation, signal setup. |
| `os_darwin.go` | macOS-specific OS interface. |

### I/O and Networking

| File | Role |
|---|---|
| `netpoll_epoll.go` | Linux I/O multiplexing via `epoll`. |
| `netpoll_kqueue.go` | macOS/BSD I/O multiplexing via `kqueue`. |
| `netpoll.go` | Platform-independent netpoller interface. |

### Synchronization

| File | Role |
|---|---|
| `chan.go` | Channel implementation: `chansend`, `chanrecv`, wait queues. |
| `sema.go` | Semaphore-based synchronization (used by `sync.Mutex`). |
| `lock_futex.go` | Lock implementation using Linux futexes. |
| `lock_sema.go` | Lock implementation using semaphores (macOS, Windows). |

## 4. The Key Abstractions: G, M, P (10 min)

The Go scheduler is built around three core data structures, defined in `src/runtime/runtime2.go`. The comment at the top of `src/runtime/proc.go` introduces them:

```go
// src/runtime/proc.go, lines 24-34

// Goroutine scheduler
// The scheduler's job is to distribute ready-to-run goroutines over worker threads.
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

### G -- Goroutine

A `g` represents a goroutine. It holds the goroutine's stack, saved registers (program counter, stack pointer), status, and a pointer to the `m` currently running it.

```go
// src/runtime/runtime2.go, lines 473-491

type g struct {
    // Stack parameters.
    // stack describes the actual stack memory: [stack.lo, stack.hi).
    // stackguard0 is the stack pointer compared in the Go stack growth prologue.
    // It is stack.lo+StackGuard normally, but can be StackPreempt to trigger a preemption.
    stack       stack   // offset known to runtime/cgo
    stackguard0 uintptr // offset known to liblink
    stackguard1 uintptr // offset known to liblink

    _panic    *_panic // innermost panic - offset known to liblink
    _defer    *_defer // innermost defer
    m         *m      // current m; offset known to arm liblink
    sched     gobuf
    syscallsp uintptr // if status==Gsyscall, syscallsp = sched.sp to use during gc
    syscallpc uintptr // if status==Gsyscall, syscallpc = sched.pc to use during gc
    syscallbp uintptr // if status==Gsyscall, syscallbp = sched.bp to use in fpTraceback
    ...
    atomicstatus atomic.Uint32
    goid         uint64
```

A goroutine moves through well-defined states (also in `runtime2.go`, lines 35-77):

- `_Gidle` (0) -- just allocated, not yet initialized
- `_Grunnable` (1) -- on a run queue, ready to execute
- `_Grunning` (2) -- currently executing on an M with a P
- `_Gsyscall` (3) -- executing a system call, not running Go code
- `_Gwaiting` (4) -- blocked (channel, mutex, sleep, etc.)
- `_Gdead` (6) -- finished execution or on a free list

### M -- Machine (OS Thread)

An `m` represents an OS thread. It points to the goroutine it's currently running (`curg`), its associated P, and a special `g0` goroutine used for scheduling stack frames.

```go
// src/runtime/runtime2.go, lines 618-644

type m struct {
    g0      *g     // goroutine with scheduling stack
    ...
    curg         *g       // current running goroutine
    ...
    p puintptr            // currently attached P
    nextp           puintptr
    oldp            puintptr // The P that was attached before executing a syscall.
    id              int64
    ...
    spinning        bool // m is out of work and is actively looking for work
    blocked         bool // m is blocked on a note
```

Key insight: `m.g0` is a special goroutine whose stack is used whenever the scheduler itself needs to run. Scheduling decisions happen on `g0`'s stack, not on any user goroutine's stack. This is analogous to how a kernel switches to a kernel stack to handle interrupts.

### P -- Processor (Logical CPU)

A `p` represents a "logical processor" -- a resource token that an M must hold to execute Go code. The number of Ps equals `GOMAXPROCS` (default: number of CPU cores).

```go
// src/runtime/runtime2.go, lines 773-819

type p struct {
    id          int32
    status      uint32 // one of pidle/prunning/...
    link        puintptr
    schedtick   uint32     // incremented on every scheduler call
    syscalltick uint32     // incremented on every system call
    sysmontick  sysmontick // last tick observed by sysmon
    m           muintptr   // back-link to associated m (nil if idle)
    mcache      *mcache
    ...
    // Queue of runnable goroutines. Accessed without lock.
    runqhead uint32
    runqtail uint32
    runq     [256]guintptr
    // runnext, if non-nil, is a runnable G that was ready'd by
    // the current G and should be run next instead of what's in
    // runq if there's time remaining in the running G's time slice.
    runnext guintptr
```

The P is the key innovation in Go's scheduler design (introduced in Go 1.1). Each P has:

- A **local run queue** (`runq` -- a circular buffer of 256 goroutines) that can be accessed without locks
- An **mcache** for fast, lock-free memory allocation
- A **runnext** slot for cache-friendly goroutine handoff

Without Ps, every goroutine operation would require locking a global run queue, creating a scalability bottleneck. The P design enables the **work-stealing** algorithm: when an M's P runs out of goroutines, it steals from other Ps' run queues.

### How G, M, P Relate

```
    ┌─────┐     ┌─────┐     ┌─────┐
    │  P  │     │  P  │     │  P  │   (GOMAXPROCS = 3)
    │runq │     │runq │     │runq │
    └──┬──┘     └──┬──┘     └─────┘
       │           │          (idle)
    ┌──┴──┐     ┌──┴──┐
    │  M  │     │  M  │     ┌─────┐
    │(thr)│     │(thr)│     │  M  │   (blocked in syscall,
    └──┬──┘     └──┬──┘     │     │    no P attached)
       │           │        └──┬──┘
    ┌──┴──┐     ┌──┴──┐     ┌──┴──┐
    │  G  │     │  G  │     │  G  │
    │(run)│     │(run)│     │(sys)│
    └─────┘     └─────┘     └─────┘
```

An M must acquire a P before it can run Go code. When a goroutine enters a blocking system call, the runtime can detach the P from that M and hand it to a different M, keeping the CPU utilized. This is covered in the [Processes, Threads, and Goroutines](../03-threads/notes.md) module.

## 5. Course Roadmap Preview (5 min)

This course explores OS concepts through the lens of the Go runtime. The seven core modules tell a bottom-up story:

1. **Introduction** (this module) -- The runtime as a user-space OS; G, M, P overview
3. **Processes, Threads, and Goroutines** -- OS threads vs. goroutines; the G and M structs; goroutine states
4. **The Go Scheduler** -- The scheduling loop; `schedule()`, `findRunnable()`, `execute()`; run queues
5. **Work Stealing and Preemption** -- Distributed scheduling; cooperative and asynchronous preemption via `SIGURG`
6. **Synchronization Primitives** -- Futexes, semaphores, spin locks; `sync.Mutex` internals
7. **Channels and Select** -- Channel implementation; `hchan`, `chansend`/`chanrecv`; the `selectgo` algorithm
10. **File Systems, I/O, and the Network Poller** -- `epoll`/`kqueue`; the integrated netpoller; non-blocking I/O

Three optional modules provide deeper dives into specific subsystems:

2. **System Calls** -- How Go crosses the user/kernel boundary; `SYSCALL` instruction; `entersyscall`/`exitsyscall`
8. **Memory Management** -- Virtual memory concepts; Go's allocator hierarchy; size classes; garbage collection
9. **Goroutine Stacks** -- Growable stacks; stack growth and copying; contrast with fixed OS thread stacks

### Suggested Exercises

1. **Explore the source**: Clone the Go repository. Open `src/runtime/proc.go` and read the top-level comment (lines 24-80). Identify where `schedule()` is defined.
2. **Count goroutines**: Write a Go program that spawns 100,000 goroutines, each sleeping for 1 second. Use `runtime.NumGoroutine()` to observe the count. How much memory does each goroutine consume?
3. **GOMAXPROCS experiment**: Run a CPU-bound benchmark with `GOMAXPROCS=1`, `2`, `4`, and your machine's core count. Observe the speedup and diminishing returns.
4. **Build the runtime**: Modify `src/runtime/proc.go` to add a `println("schedule called")` at the top of `schedule()`. Rebuild the toolchain with `./make.bash` and run a simple program. How often is `schedule()` called?

---

## Key Definitions

- **Goroutine (G)**: A lightweight, user-space thread of execution managed by the Go runtime. Initial stack size is 2 KB (vs. ~1 MB for OS threads).
- **Machine (M)**: An OS thread. The runtime creates Ms as needed and parks them when idle.
- **Processor (P)**: A logical scheduling context. An M needs a P to run Go code. The number of Ps is set by `GOMAXPROCS`.
- **Work stealing**: When a P's run queue is empty, its M steals goroutines from other Ps' run queues, balancing load without central coordination.
- **sysmon**: A special background thread (M without a P) that monitors for stuck goroutines, retakes Ps from long syscalls, and triggers GC.

## Further Reading

- [The Go scheduler design document](https://golang.org/s/go11sched) (Dmitry Vyukov, 2012)
- [Go source: `src/runtime/proc.go`](https://cs.opensource.google/go/go/+/master:src/runtime/proc.go) -- The scheduler
- [Go source: `src/runtime/runtime2.go`](https://cs.opensource.google/go/go/+/master:src/runtime/runtime2.go) -- Core data structures
- [GopherCon 2018: Kavya Joshi - The Scheduler Saga](https://www.youtube.com/watch?v=YHRO5WQGh0k)
