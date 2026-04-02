# Glossary

Key terms used throughout this course, organized alphabetically.

---

**Arena** — A large contiguous block of virtual memory (64MB on 64-bit systems) that
the Go runtime reserves for heap allocations. The heap is composed of many arenas,
managed by the `mheap` struct. See Module 8.

**Atomic operation** — A CPU instruction that completes indivisibly, without the
possibility of interruption. Examples: compare-and-swap (CAS), atomic load/store.
The foundation for lock-free data structures and synchronization primitives.
See Module 6.

**Channel (chan)** — Go's primary communication primitive, implemented by the `hchan`
struct in `runtime/chan.go`. Channels provide synchronized message passing between
goroutines, with optional buffering. See Module 7.

**Compare-and-swap (CAS)** — An atomic instruction that updates a memory location only
if it currently holds an expected value. Returns whether the swap succeeded. Used
extensively in the scheduler's lock-free run queues.

**Context switch** — Saving the state of one execution context and restoring another.
Goroutine context switches happen in user space and are much cheaper than OS thread
context switches, which require entering the kernel.

**Edge-triggered** — A mode of I/O notification (used by epoll with `EPOLLET`) where
the kernel notifies only when the state *changes* (e.g., data arrives), not while
data *remains* available. Contrast with level-triggered. See Module 10.

**epoll** — Linux's scalable I/O multiplexing mechanism. The Go runtime uses epoll in
edge-triggered mode to monitor thousands of file descriptors efficiently.
See `runtime/netpoll_epoll.go` and Module 10.

**Futex** — "Fast userspace mutex." A Linux mechanism that combines an atomic integer
in user space with a kernel wait queue. Avoids system calls in the uncontended case.
See `runtime/lock_futex.go` and Module 6.

**G (goroutine)** — The Go runtime's representation of a goroutine. Defined as the `g`
struct in `runtime/runtime2.go`. Contains the goroutine's stack, saved registers
(`gobuf`), status, and scheduling metadata. See Modules 3-4.

**GMP model** — The Go scheduler's three-part design: **G**oroutines (units of work),
**M**achines (OS threads), and **P**rocessors (execution contexts with run queues).
See Module 4.

**gobuf** — The struct that saves a goroutine's register state (stack pointer, program
counter, etc.) when it is descheduled. Defined in `runtime/runtime2.go`. See Module 4.

**gopark** — The runtime function that blocks a goroutine by transitioning it from
`_Grunning` to `_Gwaiting` and calling `schedule()` to find other work. Used by
channels, locks, timers, and I/O. See Module 4.

**goready** — The runtime function that wakes a blocked goroutine by transitioning it
from `_Gwaiting` to `_Grunnable` and placing it on a run queue. See Module 4.

**Goroutine** — A lightweight concurrent execution context in Go, multiplexed onto
OS threads by the runtime scheduler. Initial stack size: 2KB. See Modules 3-4.

**Heap** — The region of memory used for dynamically allocated objects. Managed by
Go's multi-level allocator (mcache → mcentral → mheap). See Module 8.

**kqueue** — BSD/macOS's I/O event notification mechanism, analogous to Linux's epoll.
See `runtime/netpoll_kqueue.go` and Module 10.

**M (machine)** — The Go runtime's representation of an OS thread. Defined as the `m`
struct in `runtime/runtime2.go`. Each M has a system stack (`g0`) and may or may not
be associated with a P. See Module 3.

**mcache** — A per-P cache of memory spans, enabling lock-free allocation for small
objects. Defined in `runtime/mcache.go`. See Module 8.

**mcentral** — A shared pool of spans for a specific size class. When an mcache runs
out of a size class, it refills from the corresponding mcentral. See Module 8.

**mheap** — The central heap manager that allocates spans from the OS (via mmap) and
distributes them to mcentrals. Defined in `runtime/mheap.go`. See Module 8.

**mmap** — A system call that maps virtual memory pages into a process's address space.
The Go runtime uses mmap to request memory from the OS. See Modules 2 and 8.

**mspan** — A contiguous run of memory pages dedicated to a single size class.
The fundamental unit of the Go memory allocator. Defined in `runtime/mheap.go`.
See Module 8.

**Mutex** — A mutual exclusion lock. The Go runtime has its own internal mutex
(`runtime.mutex`) distinct from `sync.Mutex`. The runtime mutex is implemented
using futex (Linux) or semaphores (macOS). See Module 6.

**Network poller (netpoller)** — The runtime subsystem that monitors file descriptors
for I/O readiness using epoll/kqueue. Integrates with the scheduler so goroutines
can block on I/O without blocking OS threads. See Module 10.

**P (processor)** — A logical execution context required to run Go code. Each P has
a local run queue of goroutines (capacity 256) and an mcache for memory allocation.
The number of Ps equals GOMAXPROCS. Defined in `runtime/runtime2.go`. See Module 4.

**Page** — The fundamental unit of virtual memory (typically 4KB or 8KB). The Go
runtime uses 8KB pages internally. See Module 8.

**Preemption** — Involuntarily stopping a running goroutine to let others run.
Go uses signal-based preemption (SIGURG) for goroutines in tight loops without
function calls. See Module 5.

**Run queue** — A queue of runnable goroutines waiting to be scheduled. Each P has
a local run queue (fixed-size circular buffer of 256 entries), and there is a global
run queue protected by a mutex. See Module 4.

**schedt** — The global scheduler state struct, containing idle M and P lists, the
global run queue, and scheduling statistics. Defined in `runtime/runtime2.go`.
See Module 4.

**Select** — Go's mechanism for waiting on multiple channel operations. Implemented
by `selectgo()` in `runtime/select.go` using a 3-pass algorithm. See Module 7.

**Semaphore** — A synchronization primitive with acquire (decrement, possibly block)
and release (increment, possibly wake) operations. The Go runtime implements
semaphores with a treap-based wait queue in `runtime/sema.go`. See Module 6.

**Size class** — One of ~70 predefined object sizes used by the Go allocator. Each
size class has its own pool of spans. Reduces fragmentation by rounding allocations
up to the nearest size class. See Module 8.

**Span** — See **mspan**.

**Spinning thread** — An M that is actively looking for work (checking run queues,
stealing from other Ps) but hasn't found any yet. Spinning threads consume CPU but
reduce latency for newly created goroutines. See Module 5.

**Stack copying** — Go's mechanism for growing goroutine stacks: allocate a new stack
at 2x the size, copy all contents, and update pointers. Replaced the earlier
"segmented stacks" approach. See Module 9.

**Stack guard** — A sentinel value stored in `g.stackguard0` that triggers stack
growth when the stack pointer gets too close to the stack boundary. The compiler
inserts checks against this value in function prologues. See Module 9.

**STW (stop-the-world)** — A phase where all goroutines are paused. The Go GC has
two brief STW phases: sweep termination and mark termination. See Module 8.

**sudog** — "Pseudo-goroutine." A struct that represents a goroutine waiting on a
synchronization event (channel send/receive, semaphore acquire). Defined in
`runtime/runtime2.go`. See Modules 6-7.

**System call (syscall)** — A request from user-space code to the kernel for a
privileged operation (file I/O, memory mapping, process creation, etc.). See Module 2.

**Treap** — A balanced binary search tree where nodes are ordered by key (BST
property) and by a random priority (heap property). Used in `runtime/sema.go` for
the semaphore wait queue. See Module 6.

**VDSO** — "Virtual Dynamic Shared Object." A mechanism where the kernel maps
frequently-used code (like `clock_gettime`) into user space, avoiding the overhead
of a real system call. Go uses VDSO for time operations on Linux. See Module 2.

**Work stealing** — A distributed scheduling strategy where idle processors steal
goroutines from busy processors' run queues. The Go scheduler steals half of the
victim's queue. See Module 5.

**Write barrier** — Code inserted by the compiler before pointer writes during GC
marking. Ensures the concurrent garbage collector doesn't miss reachable objects.
See Module 8.
