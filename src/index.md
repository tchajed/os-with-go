# Operating Systems Through the Go Runtime

A mini course that teaches operating systems concepts through the
Go runtime.

---

## Why the Go Runtime?

The Go runtime is essentially a **user-space operating system**. It:

- **Schedules** thousands of goroutines onto a small number of OS threads
- **Manages memory** with its own allocator and garbage collector
- **Multiplexes I/O** using epoll/kqueue so blocking calls don't block threads
- **Handles signals** for preemption and OS integration
- **Manages stacks** that grow and shrink dynamically

All of this is written in readable Go (with some assembly for the lowest-level
operations), making it far more approachable than a kernel like Linux while
covering the same fundamental concepts.

---

## Course Structure

| Module | Topic | Duration |
|--------|-------|----------|
| 1 | [Introduction: The Runtime as an OS](modules/01-introduction/notes.md) | 45 min |
| 3 | [Processes, Threads, and Goroutines](modules/03-threads/notes.md) | 60 min |
| 4 | [The Go Scheduler](modules/04-scheduler/notes.md) | 75 min |
| 5 | [Work Stealing and Preemption](modules/05-work-stealing/notes.md) | 60 min |
| 6 | [Synchronization Primitives](modules/06-synchronization/notes.md) | 60 min |
| 7 | [Channels and Select](modules/07-channels/notes.md) | 60 min |
| 10 | [File Systems, I/O, and the Network Poller](modules/10-io/notes.md) | 60 min |

### Optional Modules

| Module | Topic | Duration |
|--------|-------|----------|
| 2 | [System Calls](modules/02-syscalls/notes.md) | 60 min |
| 8 | [Memory Management](modules/08-memory/notes.md) | 60 min |
| 9 | [Goroutine Stacks](modules/09-stacks/notes.md) | 45 min |

## Materials

Each module includes:

- **Lecture notes** — Detailed reading material with Go runtime code walkthroughs
- **Comprehension checks** — Short questions to verify understanding

Additional resources:

- [Programming Assignments](assignments/assignment1.md) — Three hands-on projects
- [Final Exam](exam/final-exam.md) — Comprehensive assessment
- [Glossary](extras/glossary.md) — Key terms defined
- [Code Walkthrough](extras/code-walkthrough.md) — Trace a goroutine's full lifecycle
- [Experiments](extras/experiments.md) — Hands-on runtime exploration
- [Further Reading](extras/further-reading.md) — Books, papers, and resources

## Getting Started

1. Read the [syllabus](syllabus.md) for the full course overview and learning outcomes
2. Clone the [Go source tree](https://go.googlesource.com/go) to follow along with code references
3. Start with [Module 1](modules/01-introduction/notes.md)

All code references use the format `file.go:line` and point to files under
`src/runtime/` in the Go source tree unless otherwise noted.
