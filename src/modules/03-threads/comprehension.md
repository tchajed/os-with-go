# Module 3: Processes, Threads, and Goroutines (G, M structs)

## Comprehension Check

### Question 1 (Code Reading)
Consider these fields from the `g` struct in `runtime/runtime2.go`:

```go
type g struct {
	stack       stack   // offset known to runtime/cgo
	stackguard0 uintptr // offset known to liblink
	stackguard1 uintptr // offset known to liblink

	_panic    *_panic // innermost panic - offset known to liblink
	_defer    *_defer // innermost defer
	m         *m      // current m; offset known to arm liblink
	sched     gobuf
	syscallsp uintptr // if status==Gsyscall, syscallsp = sched.sp to use during gc
	syscallpc uintptr // if status==Gsyscall, syscallpc = sched.pc to use during gc
	...
}
```

Why does the `g` struct store both `sched.sp`/`sched.pc` and separate `syscallsp`/`syscallpc` fields? Under what circumstances are the syscall variants used?

<details><summary>Answer</summary>

The `sched` field (a `gobuf`) stores the goroutine's saved registers (SP, PC, etc.) for normal context switches -- when the goroutine is descheduled by the Go scheduler (e.g., blocked on a channel, preempted).

The separate `syscallsp`/`syscallpc` fields are used specifically when the goroutine is in the `_Gsyscall` state (executing a blocking system call). During a syscall:
- The `sched` fields still point to the Go-level save point.
- `syscallsp`/`syscallpc` record the stack pointer and PC at the point of the syscall entry.

The GC needs `syscallsp`/`syscallpc` because:
1. When a goroutine is in a syscall, its Go stack is not actively changing, but the GC needs to scan it.
2. The GC uses `syscallsp` to know the boundary of the goroutine's active stack frame for accurate scanning.
3. The `sched` fields may be stale or point to a different context (the goroutine may have been through several scheduling rounds before entering the syscall).

This separation ensures the GC can correctly walk the stack of a goroutine regardless of whether it was descheduled normally or entered a syscall.

</details>

---

### Question 2 (Short Answer)
Compare and contrast goroutines with:
(a) OS threads (pthreads)
(b) Green threads (as in early Java or Erlang processes)

What specific design choices make goroutines different from both?

<details><summary>Answer</summary>

**vs. OS Threads (pthreads):**
- Goroutines have ~2KB initial stacks vs. ~2-8MB for OS threads
- Goroutines are scheduled by the Go runtime in user space; OS threads by the kernel
- Goroutine context switches do not require kernel transitions
- Goroutines use growable, copyable stacks; OS threads have fixed-size stacks
- Creating a goroutine is ~100x cheaper than creating an OS thread

**vs. Green threads:**
- Green threads (e.g., early Java) typically use N:1 scheduling (all green threads on one OS thread). Goroutines use M:N scheduling (many goroutines on many OS threads via the GMP model), enabling true parallelism.
- Green threads often cannot handle blocking syscalls without blocking all green threads. Go's runtime detaches the P from a blocked M, allowing other goroutines to continue.
- Goroutines have growable stacks via segmented/copying stacks; many green thread implementations use fixed-size stacks.
- Go's scheduler includes work stealing, which most green thread implementations lack.
- Go integrates non-blocking I/O (network poller) transparently, so goroutines that do I/O appear to block but actually yield to the scheduler.

**Unique design choices:**
- The M:N model with explicit P as a scheduling resource
- Integrated network poller making blocking-style I/O non-blocking under the hood
- Stack copying (not segmented stacks) allowing stacks to grow and shrink
- Asynchronous preemption via signals (SIGURG) for long-running goroutines without function call points

</details>

---

### Question 3 (True/False with Explanation)
**True or False:** A goroutine is always executed by the same OS thread throughout its lifetime.

<details><summary>Answer</summary>

**False.** A goroutine can migrate between OS threads. When a goroutine is descheduled (e.g., it blocks on a channel, is preempted, or its P is stolen), it goes back into a run queue. When it is rescheduled, it may be picked up by a different M (OS thread) that acquires the relevant P.

The exception is `runtime.LockOSThread()`, which locks a goroutine to its current OS thread. While locked, the goroutine will only execute on that specific M, and the M will only execute that goroutine. This is necessary for certain use cases like OpenGL (which requires thread-local state), or interacting with C libraries that use thread-local storage.

</details>

---

### Question 4 (Code Reading)
Consider these fields from the `m` struct:

```go
type m struct {
	g0      *g     // goroutine with scheduling stack
	...
	curg    *g     // current running goroutine
	p       puintptr // attached p for executing go code (nil if not executing go code)
	nextp   puintptr
	oldp    puintptr // the p that was attached before executing a syscall
	...
	spinning bool
}
```

What is `g0` and why does each M need a separate goroutine for scheduling? Why can't the scheduler code run on the current goroutine's stack?

<details><summary>Answer</summary>

`g0` is a special goroutine associated with each M that has a large, fixed-size stack (typically 8KB, allocated by the OS). It is used to run scheduling code, garbage collection, and stack management operations.

The scheduler cannot run on the current goroutine's stack for several reasons:

1. **Stack growth chicken-and-egg:** The scheduler may need to grow or shrink a goroutine's stack. If the scheduler were running on that same stack, it could not safely reallocate it.

2. **Stack size guarantees:** Goroutine stacks start small (2KB) and grow on demand. Scheduler and GC code needs a reliable, sufficiently large stack. The `g0` stack provides this guarantee.

3. **Safety during context switches:** When switching from one goroutine to another, there is a moment when the old goroutine's state is being saved and the new one is being loaded. Running on a separate stack ensures neither goroutine's stack is corrupted during this transition.

4. **Syscall handling:** When returning from a syscall or handling signals, the runtime needs a known-good stack to execute on before it can set up goroutine execution.

The pattern `systemstack(func() { ... })` switches to `g0`'s stack to execute sensitive runtime operations.

</details>

---

### Question 5 (What Would Happen If...)
What would happen if goroutine stacks were fixed at 2KB and could never grow? Consider a program with deep recursion or large local variables.

<details><summary>Answer</summary>

With fixed 2KB stacks:

1. **Stack overflows:** Any function call chain deeper than roughly 20-30 frames (depending on local variable sizes) would overflow the stack. This would crash the program or corrupt adjacent memory.

2. **No recursion:** Recursive algorithms (tree traversal, quicksort, etc.) would be essentially unusable for non-trivial input sizes.

3. **Large local variables:** A single function with a local `[1024]byte` array would consume half the stack immediately, leaving almost no room for any other calls.

4. **Trade-off with goroutine count:** To avoid overflows, users would need larger fixed stacks (e.g., 1MB). But 1MB x 1,000,000 goroutines = 1TB of memory (or at least virtual address space). The small initial stack is what makes millions of goroutines feasible.

5. **No `defer`/`panic` chains:** Complex defer chains and panic recovery would quickly exhaust a 2KB stack.

Go solves this with growable stacks: each function prologue checks if there is enough stack space and calls `morestack` -> `newstack` -> `copystack` if not, doubling the stack size and copying all contents to the new location. This gives goroutines effectively unlimited stack depth while keeping initial allocation tiny.

</details>

---

### Question 6 (Short Answer)
What are the possible states a goroutine (`g`) can be in? Describe the transitions between `_Grunnable`, `_Grunning`, `_Gwaiting`, and `_Gsyscall`.

<details><summary>Answer</summary>

Key goroutine states:

- **`_Grunnable`**: Ready to run but not yet assigned to an M/P. Sitting in a run queue (local or global).
- **`_Grunning`**: Currently executing on an M with a P. Only one G per M can be in this state.
- **`_Gwaiting`**: Blocked waiting for some event (channel op, mutex, I/O, sleep, select, etc.). Not on any run queue.
- **`_Gsyscall`**: Executing a system call. The M is in the kernel; the P may have been detached.

**Transitions:**
- `_Grunnable` -> `_Grunning`: The scheduler picks the G from a run queue and calls `execute()`.
- `_Grunning` -> `_Grunnable`: Preemption occurs, or the G yields. It goes back to a run queue.
- `_Grunning` -> `_Gwaiting`: The G blocks (e.g., `gopark()` called by channel ops, mutex locks, etc.).
- `_Gwaiting` -> `_Grunnable`: The event the G was waiting for occurs (e.g., `goready()` called when a channel op completes or a mutex is released).
- `_Grunning` -> `_Gsyscall`: The G calls `entersyscall()` before a blocking system call.
- `_Gsyscall` -> `_Grunning`: The syscall returns and `exitsyscall()` reacquires a P.
- `_Gsyscall` -> `_Grunnable`: The syscall returns but no P is immediately available; the G is placed on the global run queue.

Other states include `_Gidle` (just allocated), `_Gdead` (finished, available for reuse), `_Gcopystack` (stack is being copied), and `_Gpreempted` (stopped at an async preemption point).

</details>
