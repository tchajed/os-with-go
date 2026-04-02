# Module 1: Introduction (The Runtime as an Operating System)

## Comprehension Check

### Question 1 (Short Answer)
List at least four responsibilities that a traditional operating system kernel handles, and for each one, identify the corresponding component or subsystem in the Go runtime.

<details><summary>Answer</summary>

| OS Responsibility | Go Runtime Equivalent |
|---|---|
| Process/thread scheduling | Goroutine scheduler (`schedule()`, `findRunnable()` in `proc.go`) |
| Memory management / virtual memory | `mheap`, `mcache`, `mcentral`, garbage collector |
| Synchronization primitives (mutexes, semaphores) | `runtime.mutex` (futex-based), `sema.go`, `rwmutex` |
| I/O and file system access | Network poller (`netpoll.go`), `os.File`, `poll.FD` |
| Stack management | Growable stacks (`stack.go`, `newstack`, `copystack`) |
| Inter-process communication | Channels (`chan.go`, `hchan`) |

</details>

---

### Question 2 (True/False with Explanation)
**True or False:** The Go runtime runs entirely in user space and never makes system calls to the kernel.

<details><summary>Answer</summary>

**False.** The Go runtime runs in user space but relies heavily on the kernel for essential services. It makes system calls for:
- Creating OS threads (`clone` on Linux)
- Futex operations for low-level synchronization
- Memory mapping (`mmap`) to obtain memory from the OS
- I/O multiplexing (`epoll_create`, `epoll_ctl`, `epoll_wait` on Linux; `kqueue` on macOS/BSD)
- Signal handling

The runtime acts as a "mini OS" by providing higher-level abstractions (goroutines, channels, GC) on top of these kernel primitives, but it cannot function without the underlying kernel.

</details>

---

### Question 3 (Code Reading)
Consider this comment from the top of `runtime/proc.go`:

```go
// Goroutine scheduler
// The scheduler's job is to distribute ready-to-run goroutines over worker threads.
//
// The main concepts are:
// G - goroutine.
// M - worker thread, or machine.
// P - processor, a resource that is required to execute Go code.
//     M must have an associated P to execute Go code, however it can be
//     blocked or in a syscall w/o an associated P.
```

Why does the design separate M (machine/thread) from P (processor)? What problem would arise if every M always had exclusive access to a P?

<details><summary>Answer</summary>

The separation of M and P exists to decouple OS thread management from goroutine scheduling resources. Key reasons:

1. **Syscall handling:** When a goroutine makes a blocking system call, the M enters the kernel and blocks. If the M permanently owned the P, that P's entire run queue of goroutines would stall. By detaching the P from a blocked M, the runtime can hand the P (and its run queue) to another M that can continue executing goroutines.

2. **Limiting parallelism:** The number of Ps (set by `GOMAXPROCS`) controls the degree of true parallelism. There can be many more Ms than Ps (Ms are created for blocking syscalls, cgo calls, etc.). Without this separation, the runtime would either need to limit thread creation (causing syscall bottlenecks) or allow unbounded parallelism (causing excessive contention).

3. **Resource locality:** The P holds per-processor caches (`mcache`, run queue, `sudog` cache) that provide locality. When a P is handed off between Ms, these caches move with it.

</details>

---

### Question 4 (What Would Happen If...)
What would happen if the Go runtime used a single global run queue instead of per-P local run queues? Consider both correctness and performance.

<details><summary>Answer</summary>

**Correctness:** The scheduler would still be correct. A single global queue can distribute work to all available threads.

**Performance:** There would be significant degradation:
- **Lock contention:** Every goroutine creation (`go` statement), every scheduling decision, and every goroutine completion would require acquiring a global lock. On a machine with many cores, this lock would become a severe bottleneck, serializing all scheduling decisions.
- **Cache locality loss:** Goroutines that communicate frequently (e.g., a producer and consumer) would be randomly distributed across CPUs, losing cache locality. The per-P run queues and especially `runnext` allow communicating goroutines to run on the same P.
- **Scalability:** Performance would degrade roughly linearly with core count. The Go runtime design doc (go11sched) explicitly identifies the centralized run queue as the primary bottleneck in the old scheduler that the GMP model was designed to solve.

The runtime does still maintain a global run queue as a fallback, but it is checked only periodically (every 61 schedule ticks) or when local queues are empty.

</details>

---

### Question 5 (Short Answer)
Explain what `GOMAXPROCS` controls and why its default value equals the number of available CPU cores. What does it *not* control?

<details><summary>Answer</summary>

`GOMAXPROCS` sets the number of P (processor) structs, which determines the maximum number of goroutines that can execute Go code simultaneously on different OS threads.

**Default = number of CPUs:** This ensures the runtime can fully utilize all available hardware parallelism without oversubscription.

**What it does NOT control:**
- The number of goroutines that can exist (that is effectively unlimited)
- The number of OS threads (Ms) -- the runtime creates additional Ms as needed for blocking syscalls, cgo calls, etc. The thread count can far exceed GOMAXPROCS.
- The number of goroutines blocked in I/O or channel operations
- GC parallelism (though GC does use Ps)

</details>

---

### Question 6 (Short Answer)
In what sense is the Go runtime's relationship to goroutines analogous to a hypervisor's relationship to virtual machines? In what ways does the analogy break down?

<details><summary>Answer</summary>

**Similarities:**
- Both multiplex many virtual entities (goroutines / VMs) onto fewer physical resources (OS threads+CPUs / physical CPUs)
- Both provide an abstraction layer that hides the complexity of the underlying hardware/OS
- Both perform scheduling decisions transparent to the managed entities
- Both manage memory (GC / memory virtualization)

**Where the analogy breaks down:**
- Goroutines are cooperative/semi-cooperative (preemption via async signals is a recent addition), while VMs are fully preemptable via hardware timer interrupts
- Goroutines share a single address space and can communicate via shared memory and channels; VMs have isolated address spaces
- The Go runtime is a library linked into the application, not a separate privileged layer; a hypervisor runs at a higher privilege level
- Goroutines are extremely lightweight (2KB initial stack) compared to VMs (GBs of memory)
- There is no hardware support (e.g., VT-x) for goroutine isolation

</details>
