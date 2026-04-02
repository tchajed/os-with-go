# Module 2: System Calls (sys_linux_amd64.s, syscall package)

## Comprehension Check

### Question 1 (Code Reading)
Examine this snippet from `runtime/sys_linux_amd64.s`:

```asm
TEXT runtime·futex(SB),NOSPLIT,$0
	MOVQ	addr+0(FP), DI
	MOVL	op+8(FP), SI
	MOVL	val+12(FP), DX
	MOVQ	ts+16(FP), R10
	MOVQ	addr2+24(FP), R8
	MOVL	val3+32(FP), R9
	MOVL	$SYS_futex, AX
	SYSCALL
	MOVL	AX, ret+40(FP)
	RET
```

Explain what each `MOVQ`/`MOVL` instruction is doing and why the function uses `NOSPLIT`.

<details><summary>Answer</summary>

**The MOV instructions** are loading function arguments from the Go stack frame (accessed via `FP`, the frame pointer pseudo-register) into the specific CPU registers that the Linux system call ABI requires:
- `DI` = first argument (`addr`) -- the futex address
- `SI` = second argument (`op`) -- the futex operation
- `DX` = third argument (`val`) -- the expected value
- `R10` = fourth argument (`ts`) -- timeout (replaces `RCX` which `SYSCALL` clobbers)
- `R8` = fifth argument (`addr2`)
- `R9` = sixth argument (`val3`)
- `AX` = the system call number (`SYS_futex`)

After `SYSCALL`, the return value in `AX` is stored back to the stack frame.

**`NOSPLIT`** means this function must not trigger a stack growth check. This is essential because:
1. System call wrappers are called from contexts where stack growth would be unsafe (e.g., during scheduler operations on the system stack).
2. The function is written in assembly and does not follow Go's stack growth protocol.
3. It must execute on the current stack without any possibility of calling `morestack`.

</details>

---

### Question 2 (Short Answer)
The Go runtime makes system calls directly via assembly rather than using the C library (libc). What are two advantages and one disadvantage of this approach?

<details><summary>Answer</summary>

**Advantages:**
1. **No cgo overhead:** Calling into C requires saving/restoring different calling conventions, switching signal stacks, and potentially allocating a larger stack. Direct syscalls avoid all this overhead.
2. **Full control over thread state:** The runtime needs precise control over signal masks, thread-local storage, and stack pointers. Going through libc would mean coordinating with libc's internal state (e.g., libc's `errno`, signal handling, thread management via pthreads), which could conflict with the runtime's own management.

**Disadvantage:**
1. **Portability burden:** The runtime must maintain per-OS, per-architecture assembly files (`sys_linux_amd64.s`, `sys_darwin_arm64.s`, etc.). Any kernel ABI changes must be tracked manually. libc provides a stable ABI that abstracts over kernel changes.

Other valid disadvantages: no automatic DNS resolution via nsswitch/NSS, no automatic compatibility with OS security policies that hook libc functions, difficulty with some system calls that libc wraps with retry logic.

</details>

---

### Question 3 (True/False with Explanation)
**True or False:** When a goroutine makes a blocking system call (like reading from a file), the P (processor) remains attached to the M (OS thread) until the syscall returns.

<details><summary>Answer</summary>

**False.** When a goroutine enters a blocking syscall, the runtime calls `entersyscall()` which dissociates the P from the M. The `sysmon` thread (or other mechanisms) can then "hand off" the P to a different M so that other goroutines on that P's run queue can continue executing. When the syscall returns, the goroutine calls `exitsyscall()` and tries to re-acquire a P. If its old P is unavailable, it tries to get any idle P; if none are available, the goroutine is placed on the global run queue and the M parks itself.

This is one of the key reasons for the M/P separation in the GMP model.

</details>

---

### Question 4 (What Would Happen If...)
What would happen if the Go runtime used blocking `read()` system calls for network I/O instead of non-blocking I/O with `epoll`/`kqueue`? Consider the impact on a web server handling 10,000 concurrent connections.

<details><summary>Answer</summary>

Each blocked network read would tie up an OS thread (M). With 10,000 concurrent connections:

1. **Thread explosion:** The runtime would need ~10,000 OS threads. Each thread consumes kernel resources (kernel stack ~8KB, scheduling overhead, TLB entries, file descriptor table entries).
2. **Memory overhead:** Even with Go's small goroutine stacks, each M needs a system stack (typically 8MB of virtual address space reserved). 10,000 threads would reserve ~80GB of virtual address space.
3. **Scheduling overhead:** The OS scheduler would need to manage 10,000 threads, causing significant context-switch overhead and cache pollution.
4. **P starvation:** Each blocking syscall detaches the P from the M. With 10,000 concurrent syscalls and a default GOMAXPROCS of, say, 8, all 8 Ps would be continuously handed off. The overhead of thread parking/unparking and P handoffs would dominate.

By using non-blocking I/O with the network poller, the runtime parks goroutines in user space (essentially just saving a pointer in a `pollDesc` struct) and uses a single `epoll_wait` call to monitor all 10,000 file descriptors. Goroutines are woken only when their I/O is ready, requiring no additional OS threads.

</details>

---

### Question 5 (Short Answer)
Explain the difference between the `syscall` package and the `runtime` package's internal system call mechanisms. When would a Go developer use each?

<details><summary>Answer</summary>

**`runtime` internal syscalls:**
- Implemented in assembly (e.g., `sys_linux_amd64.s`)
- Called via `runtime·syscall` or direct assembly wrappers
- Used only by the runtime itself for fundamental operations (futex, mmap, clone, sigaction, etc.)
- Called from the system stack (`systemstack` or `go:nosplit` functions)
- Do not go through the `entersyscall`/`exitsyscall` protocol (the runtime manages this internally)

**`syscall` package:**
- A higher-level Go package that wraps system calls for user code
- Uses `runtime.entersyscall()` and `runtime.exitsyscall()` to properly notify the scheduler
- Provides a Go-friendly API with error returns as `error` values
- Generated partially from tables (e.g., `mksyscall.go`)
- User programs and the standard library use this for file I/O, networking, process management, etc.

A Go developer uses the `syscall` (or `golang.org/x/sys/unix`) package when they need to make system calls not exposed by higher-level packages. They would never directly call runtime internals.

</details>

---

### Question 6 (Code Reading)
Consider this function from `runtime/os_linux.go`:

```go
func futexsleep(addr *uint32, val uint32, ns int64) {
	if ns < 0 {
		futex(unsafe.Pointer(addr), _FUTEX_WAIT_PRIVATE, val, nil, nil, 0)
		return
	}

	var ts timespec
	ts.setNsec(ns)
	futex(unsafe.Pointer(addr), _FUTEX_WAIT_PRIVATE, val, &ts, nil, 0)
}
```

Why does the function check the value of `val` atomically against `*addr` inside the kernel? What race condition would exist without this check?

<details><summary>Answer</summary>

The `FUTEX_WAIT` operation atomically checks that `*addr == val` and, only if true, puts the calling thread to sleep. This atomicity is critical to avoid a **lost wakeup** race:

1. Thread A checks a condition variable and sees it should wait.
2. Between thread A's check and its call to sleep, thread B changes the value and calls `futexwake`.
3. If the sleep were not atomic with the value check, thread A would go to sleep and never be woken (the wakeup was already sent and lost).

By having the kernel atomically verify `*addr == val` at the moment of sleeping, thread A will not sleep if the value has already changed (the `futex` call returns immediately with `EAGAIN`). The caller can then re-check the condition.

This is the same fundamental problem that `pthread_cond_wait` solves by requiring the mutex to be held and atomically releasing it when sleeping.

</details>
