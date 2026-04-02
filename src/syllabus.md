# Operating Systems Through the Go Runtime

## Course Overview

This 10-hour mini course teaches core operating systems concepts using the Go runtime
as a living, readable case study. Rather than studying OS theory in isolation, students
will read and analyze production code that implements scheduling, concurrency,
memory management, and I/O — the same code that runs every Go program.

The Go runtime is an ideal subject because it is essentially a **user-space operating
system**: it multiplexes thousands of goroutines onto OS threads, manages its own memory
allocator and garbage collector, handles signals, and implements its own I/O scheduler.
All of this is written in readable Go (with some assembly), making it far more
approachable than a kernel like Linux.

**Prerequisites:** An introductory systems programming or computer organization course
(or equivalent). Students should be comfortable reading C-like code and have basic
familiarity with concepts like pointers, memory addresses, and CPU registers. Go syntax
is introduced as needed — prior Go experience is helpful but not required.

**Source code:** All code references point to the Go source tree. Students should have
a copy available (the course references files under `src/runtime/` and `src/os/`).

---

## Learning Outcomes

By the end of this course, students will be able to:

1. **Explain the role of an operating system** and identify which OS responsibilities
   the Go runtime handles in user space.
2. **Trace how a system call works** from a Go function call through the syscall
   package down to the hardware instruction, and explain the user/kernel boundary.
3. **Distinguish processes, OS threads, and goroutines**, and explain why user-level
   threading is useful.
4. **Describe the GMP scheduling model** (goroutines, machines, processors) and
   trace a goroutine through its lifecycle states.
5. **Explain work stealing** as a distributed scheduling strategy and analyze its
   tradeoffs versus centralized scheduling.
6. **Analyze low-level synchronization primitives** (futexes, semaphores, spin locks)
   and explain how they build on hardware atomics and OS support.
7. **Read and explain the channel implementation**, including the blocking/waking
   mechanism and the select algorithm.
8. **Describe a multi-level memory allocator** and explain how per-CPU caches,
   size classes, and spans reduce fragmentation and contention.
9. **Explain how goroutine stacks grow and shrink** dynamically, contrasting this
   with fixed-size OS thread stacks.
10. **Trace an I/O operation** from `os.File.Read()` through the internal poller
    to epoll/kqueue, and explain how the runtime makes blocking I/O non-blocking.

---

## Module Schedule

| # | Module | Duration | Key Runtime Files |
|---|--------|----------|-------------------|
| 1 | [Introduction: The Runtime as an OS](modules/01-introduction/notes.md) | 45 min | `runtime/` overview |
| 2 | [System Calls](modules/02-syscalls/notes.md) | 60 min | `sys_linux_amd64.s`, `syscall/` |
| 3 | [Processes, Threads, and Goroutines](modules/03-threads/notes.md) | 60 min | `runtime2.go`, `os_linux.go` |
| 4 | [The Go Scheduler](modules/04-scheduler/notes.md) | 75 min | `proc.go`, `runtime2.go` |
| 5 | [Work Stealing and Preemption](modules/05-work-stealing/notes.md) | 60 min | `proc.go`, `signal_unix.go` |
| 6 | [Synchronization Primitives](modules/06-synchronization/notes.md) | 60 min | `lock_futex.go`, `sema.go`, `rwmutex.go` |
| 7 | [Channels and Select](modules/07-channels/notes.md) | 60 min | `chan.go`, `select.go` |
| 8 | [Memory Management](modules/08-memory/notes.md) | 60 min | `malloc.go`, `mheap.go`, `mcache.go` |
| 9 | [Goroutine Stacks](modules/09-stacks/notes.md) | 45 min | `stack.go` |
| 10 | [File Systems, I/O, and the Network Poller](modules/10-io/notes.md) | 60 min | `netpoll.go`, `os/file.go`, `internal/poll/` |

**Total: 9 hours 45 minutes** (allows flexibility for questions and discussion)

---

## Assessments

| Assessment | Description | Weight |
|------------|-------------|--------|
| [Reading Checks](modules/01-introduction/comprehension.md) (per module) | Short comprehension questions after each module | 20% |
| [Assignment 1: System Call Tracer](assignments/assignment1.md) | Build a tool that traces Go program system calls | 20% |
| [Assignment 2: Goroutine Scheduler](assignments/assignment2.md) | Implement a simplified GMP scheduler | 30% |
| [Assignment 3: Concurrent Data Structure](assignments/assignment3.md) | Build a channel-like primitive from scratch | 15% |
| [Final Exam](exam/final-exam.md) | Comprehensive exam covering all modules | 15% |

---

## Narrative Arc

The course tells a coherent story, building from the bottom up:

1. **Modules 1-2** establish the foundation: what an OS does and how programs
   talk to the kernel via system calls.
2. **Modules 3-5** cover the scheduler — the heart of the runtime. Students learn
   why goroutines exist, how the GMP model works, and how work stealing keeps
   all CPUs busy.
3. **Modules 6-7** explore concurrency from the programmer's perspective:
   how locks and channels are implemented on top of the scheduler primitives
   (gopark/goready) introduced in Modules 4-5.
4. **Modules 8-9** cover memory: how the allocator and stack manager provide
   the illusion of unlimited memory to goroutines.
5. **Module 10** ties everything together by showing how I/O integrates with
   the scheduler, completing the picture of the runtime as a full OS.

---

## How to Use These Materials

- **Notes** provide detailed reading material with code walkthroughs. Read these
  before or after the corresponding lecture.
- **Slides** provide a structured outline for lectures and presentations.
- **Comprehension checks** are short questions to verify understanding of each module.
- **Assignments** are hands-on programming exercises that reinforce key concepts.
- The **exam** tests conceptual understanding across all modules.

All code references use the format `file.go:line` and point to the Go source tree
under `src/runtime/` unless otherwise noted. Students are encouraged to open the
actual source files and read the surrounding context.
