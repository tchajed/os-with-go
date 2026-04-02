# Further Reading

Recommended resources for deeper exploration of the topics covered in this course.

---

## General Operating Systems

- **Operating Systems: Three Easy Pieces** (Remzi & Andrea Arpaci-Dusseau)
  Free online textbook. Excellent coverage of virtualization, concurrency, and
  persistence. [ostep.org](https://pages.cs.wisc.edu/~remzi/OSTEP/)

- **Linux Kernel Development, 3rd Edition** (Robert Love)
  Practical guide to the Linux kernel internals. Good companion for understanding
  the kernel side of the system calls Go makes.

- **The Design and Implementation of the FreeBSD Operating System** (McKusick et al.)
  Comprehensive reference for BSD internals, relevant for understanding kqueue and
  the macOS kernel (XNU/Darwin).

## Go Runtime Internals

- **The Go runtime scheduler source code comments**
  `src/runtime/proc.go` lines 24-116 contain an excellent design document explaining
  the GMP model and the rationale behind spinning threads.

- **Go scheduler design document** (Dmitry Vyukov)
  The original proposal for the current GMP scheduler.
  [docs.google.com/document/d/1TTj4T2JO42uD5ID9e89oa0sLKhJYD0Y_kqxDv3I3XMw](https://docs.google.com/document/d/1TTj4T2JO42uD5ID9e89oa0sLKhJYD0Y_kqxDv3I3XMw)

- **Go memory allocator design** (based on TCMalloc)
  `src/runtime/malloc.go` lines 1-100 describe the allocator architecture. The design
  is based on TCMalloc (Thread-Caching Malloc) by Google.

- **Getting to Go: The Journey of Go's Garbage Collector** (Rick Hudson)
  Talk describing the evolution of Go's GC from STW to concurrent.
  [blog.golang.org/ismmkeynote](https://go.dev/blog/ismmkeynote)

- **Go GC design document**
  `src/runtime/mgc.go` lines 1-110 describe the four GC phases in detail.

- **A Guide to the Go Garbage Collector** (official Go documentation)
  [tip.golang.org/doc/gc-guide](https://tip.golang.org/doc/gc-guide)

## Scheduling

- **Scheduling: Introduction** (OSTEP Chapter 7)
  Covers basic scheduling algorithms: FIFO, SJF, STCF, Round Robin.

- **Multi-CPU Scheduling** (OSTEP Chapter 10)
  Covers work stealing, per-CPU run queues, and load balancing — directly relevant
  to the Go scheduler design.

- **Work-Stealing scheduling** (Blumofe & Leiserson, 1999)
  The foundational paper on work-stealing for parallel computation.
  "Scheduling Multithreaded Computations by Work Stealing", JACM 46(5).

- **Contention-Aware Scheduling** — Background on why the Go scheduler uses
  spinning threads and careful spinning→non-spinning transitions.

## Concurrency and Synchronization

- **Futexes Are Tricky** (Ulrich Drepper)
  Essential reading for understanding how futex-based mutexes work.
  Covers the subtleties of the futex API that Go's `lock_futex.go` navigates.

- **The Little Book of Semaphores** (Allen Downey)
  Free online book with dozens of synchronization problems and solutions.
  [greenteapress.com/semaphores](https://greenteapress.com/semaphores/)

- **Communicating Sequential Processes** (Tony Hoare, 1978)
  The theoretical foundation for Go's channel-based concurrency model.
  [usingcsp.com](http://www.usingcsp.com/)

- **Share Memory by Communicating** (Go blog)
  Explains Go's concurrency philosophy.
  [go.dev/blog/codelab-share](https://go.dev/blog/codelab-share)

## Memory Management

- **TCMalloc: Thread-Caching Malloc** (Google)
  The allocator design that inspired Go's mcache/mcentral/mheap hierarchy.
  [google.github.io/tcmalloc](https://google.github.io/tcmalloc/)

- **Virtual Memory** (OSTEP Chapters 13-23)
  Comprehensive coverage of address spaces, page tables, TLBs, and memory
  management policies.

- **Garbage Collection Handbook** (Jones, Hosking, Moss)
  Authoritative reference on GC algorithms. Covers tri-color marking, write
  barriers, and concurrent collection.

## I/O and File Systems

- **The epoll API** (Linux man pages)
  `man 7 epoll` — explains edge-triggered vs level-triggered semantics.

- **kqueue and kevent** (FreeBSD man pages)
  `man 2 kqueue` — BSD's event notification interface.

- **The C10K Problem** (Dan Kegel)
  Classic article on handling 10,000+ concurrent connections, motivating the
  design of epoll/kqueue and event-driven architectures.

- **I/O Multiplexing** (OSTEP Chapter 36)
  Covers the evolution from blocking I/O to select/poll/epoll.

## Go Source Code Exploration

The following files are the most rewarding to read in full:

| File | Lines | Topic |
|------|-------|-------|
| `runtime/proc.go` | 8125 | Scheduler (start with lines 24-116) |
| `runtime/chan.go` | 970 | Channel implementation |
| `runtime/select.go` | 806 | Select statement |
| `runtime/sema.go` | 542 | Semaphore with treap |
| `runtime/stack.go` | 1430 | Stack management |
| `runtime/netpoll.go` | 733 | Network poller interface |
| `runtime/netpoll_epoll.go` | 177 | epoll implementation |
| `runtime/netpoll_kqueue.go` | 184 | kqueue implementation |
| `runtime/malloc.go` | 2553 | Memory allocator |
| `runtime/mgc.go` | 2333 | Garbage collector |
| `runtime/runtime2.go` | 1520 | Core data structures |

## Tools for Exploration

- **`go tool trace`** — Visualize goroutine scheduling, GC events, and system calls.
  Generate a trace with `runtime/trace` package, then view in browser.

- **`GODEBUG=schedtrace=1000`** — Print scheduler state every second.
  Shows number of goroutines, threads, and per-P run queue lengths.

- **`GODEBUG=gctrace=1`** — Print GC statistics after each collection.
  Shows pause times, heap sizes, and CPU utilization.

- **`strace` / `dtruss`** — Trace system calls made by a Go program.
  Useful for seeing the raw syscalls behind Go's abstractions.

- **`perf` (Linux)** — Profile CPU usage, cache misses, and context switches.
  Can reveal scheduler overhead and synchronization contention.
