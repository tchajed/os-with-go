# Module 2: System Calls

**Duration:** 60 minutes

## 1. What Are System Calls? (10 min)

### The User/Kernel Boundary

A modern operating system divides execution into two privilege levels:

- **User mode**: where application code runs. It can execute normal instructions but cannot directly access hardware, other processes' memory, or privileged CPU features.
- **Kernel mode**: where the OS kernel runs. It has unrestricted access to hardware, memory, and privileged instructions.

A **system call** (syscall) is the mechanism by which user-mode code requests services from the kernel. On x86-64 Linux, the transition happens via the `SYSCALL` instruction, which:

1. Saves the user-mode instruction pointer (RIP) and flags (RFLAGS)
2. Loads the kernel-mode instruction pointer and stack pointer from MSRs (Model-Specific Registers)
3. Switches to ring 0 (kernel privilege level)
4. Jumps to the kernel's syscall entry point

The kernel looks at the value in `RAX` (the syscall number), dispatches to the appropriate handler, and returns to user mode via `SYSRET`.

### The x86-64 Linux Syscall Convention

| Register | Purpose |
|---|---|
| `RAX` | Syscall number (in) / return value (out) |
| `RDI` | Argument 1 |
| `RSI` | Argument 2 |
| `RDX` | Argument 3 |
| `R10` | Argument 4 |
| `R8`  | Argument 5 |
| `R9`  | Argument 6 |

On error, `RAX` contains a negative errno value (e.g., `-ENOENT` = -2).

## 2. How Go Makes System Calls on Linux (15 min)

The Go runtime implements system calls directly in assembly, bypassing libc entirely on Linux. The wrappers live in `src/runtime/sys_linux_amd64.s`.

### Syscall Number Definitions

The file begins by defining the syscall numbers as constants, matching the Linux kernel's numbering:

```asm
// src/runtime/sys_linux_amd64.s, lines 16-49

#define SYS_read                0
#define SYS_write               1
#define SYS_close               3
#define SYS_mmap                9
#define SYS_munmap              11
#define SYS_brk                 12
#define SYS_rt_sigaction        13
#define SYS_rt_sigprocmask      14
#define SYS_rt_sigreturn        15
#define SYS_sched_yield         24
#define SYS_madvise             28
#define SYS_nanosleep           35
#define SYS_getpid              39
#define SYS_clone               56
#define SYS_exit                60
#define SYS_kill                62
#define SYS_sigaltstack         131
#define SYS_futex               202
#define SYS_clock_gettime       228
#define SYS_exit_group          231
#define SYS_openat              257
#define SYS_pipe2               293
```

These numbers are stable ABI -- the Linux kernel guarantees they won't change. This is why Go can bypass libc on Linux: the syscall interface is the stable boundary, not the C library.

### exit: The Simplest Syscall

```asm
// src/runtime/sys_linux_amd64.s, lines 51-55

TEXT runtime·exit(SB),NOSPLIT,$0-4
    MOVL    code+0(FP), DI
    MOVL    $SYS_exit_group, AX
    SYSCALL
    RET
```

Line by line:

1. **`TEXT runtime·exit(SB),NOSPLIT,$0-4`** -- declares the function. `NOSPLIT` means it won't grow the stack (important because we're about to leave Go's world). `$0-4` means 0 bytes of local frame, 4 bytes of arguments.
2. **`MOVL code+0(FP), DI`** -- loads the Go function argument `code` into `DI` (first argument register per Linux ABI).
3. **`MOVL $SYS_exit_group, AX`** -- loads syscall number 231 (`exit_group`, which kills all threads in the process) into `AX`.
4. **`SYSCALL`** -- traps into the kernel.
5. **`RET`** -- never reached, since `exit_group` doesn't return.

Note: Go uses `exit_group` (not `exit`) because a Go process has multiple OS threads. `exit` would only terminate one thread; `exit_group` terminates the entire process.

### read, write: I/O Syscalls

```asm
// src/runtime/sys_linux_amd64.s, lines 93-109

TEXT runtime·write1(SB),NOSPLIT,$0-28
    MOVQ    fd+0(FP), DI
    MOVQ    p+8(FP), SI
    MOVL    n+16(FP), DX
    MOVL    $SYS_write, AX
    SYSCALL
    MOVL    AX, ret+24(FP)
    RET

TEXT runtime·read(SB),NOSPLIT,$0-28
    MOVL    fd+0(FP), DI
    MOVQ    p+8(FP), SI
    MOVL    n+16(FP), DX
    MOVL    $SYS_read, AX
    SYSCALL
    MOVL    AX, ret+24(FP)
    RET
```

The pattern is always the same:
1. Load Go function arguments into the Linux ABI registers (`DI`, `SI`, `DX`, `R10`, `R8`, `R9`)
2. Load the syscall number into `AX`
3. Execute `SYSCALL`
4. Store the return value from `AX` back into the Go return slot on the stack

### open: Error Handling Pattern

```asm
// src/runtime/sys_linux_amd64.s, lines 69-81

TEXT runtime·open(SB),NOSPLIT,$0-20
    // This uses openat instead of open, because Android O blocks open.
    MOVL    $AT_FDCWD, DI // AT_FDCWD, so this acts like open
    MOVQ    name+0(FP), SI
    MOVL    mode+8(FP), DX
    MOVL    perm+12(FP), R10
    MOVL    $SYS_openat, AX
    SYSCALL
    CMPQ    AX, $0xfffffffffffff001
    JLS     2(PC)
    MOVL    $-1, AX
    MOVL    AX, ret+16(FP)
    RET
```

The error-checking pattern at lines 77-79 is interesting:
- `CMPQ AX, $0xfffffffffffff001` checks if the return value is in the error range. On Linux, syscall returns in the range `[-4095, -1]` indicate errors (negative errno).
- `0xfffffffffffff001` is `-4095` in unsigned 64-bit representation.
- `JLS 2(PC)` -- if the result is below (unsigned) this threshold, it's a success; skip the error fixup.
- On error, the function returns `-1` rather than the raw negative errno.

Also note that Go uses `openat` with `AT_FDCWD` instead of `open`, because Android O blocked the `open` syscall in its seccomp filter.

## 3. macOS: libc Trampolines (10 min)

On macOS (Darwin), Go **cannot** make syscalls directly. Apple does not guarantee a stable syscall ABI -- the syscall numbers and conventions can change between macOS versions. Instead, Go must call through libc.

The trampoline pattern is visible in `src/runtime/sys_darwin_arm64.s`:

```asm
// src/runtime/sys_darwin_arm64.s, lines 21-29

TEXT runtime·open_trampoline(SB),NOSPLIT,$0
    SUB     $16, RSP
    MOVW    8(R0), R1       // arg 2 flags
    MOVW    12(R0), R2      // arg 3 mode
    MOVW    R2, (RSP)       // arg 3 is variadic, pass on stack
    MOVD    0(R0), R0       // arg 1 pathname
    BL      libc_open(SB)
    ADD     $16, RSP
    RET
```

Key differences from Linux:

1. **Indirection through libc**: `BL libc_open(SB)` calls the C library's `open()` function, which internally makes the actual syscall. The runtime never issues the `SVC` instruction directly.
2. **Argument marshalling**: Arguments arrive packed in a struct pointed to by `R0`. The trampoline unpacks them into the ARM64 calling convention registers (`R0`, `R1`, `R2`, etc.).
3. **Variadic argument handling**: `open()` in C is variadic (the `mode` argument is only present with `O_CREAT`). On ARM64, variadic arguments go on the stack, not in registers -- hence the `MOVW R2, (RSP)`.

### write and read trampolines with error handling

```asm
// src/runtime/sys_darwin_arm64.s, lines 36-62

TEXT runtime·write_trampoline(SB),NOSPLIT,$0
    MOVD    8(R0), R1       // arg 2 buf
    MOVW    16(R0), R2      // arg 3 count
    MOVW    0(R0), R0       // arg 1 fd
    BL      libc_write(SB)
    MOVD    $-1, R1
    CMP     R0, R1
    BNE     noerr
    BL      libc_error(SB)
    MOVW    (R0), R0
    NEG     R0, R0          // caller expects negative errno value
noerr:
    RET

TEXT runtime·read_trampoline(SB),NOSPLIT,$0
    MOVD    8(R0), R1       // arg 2 buf
    MOVW    16(R0), R2      // arg 3 count
    MOVW    0(R0), R0       // arg 1 fd
    BL      libc_read(SB)
    MOVD    $-1, R1
    CMP     R0, R1
    BNE     noerr
    BL      libc_error(SB)
    MOVW    (R0), R0
    NEG     R0, R0          // caller expects negative errno value
noerr:
    RET
```

On macOS, libc functions return `-1` on error and set `errno` (a thread-local variable). The trampoline must:
1. Check if the return is `-1`
2. If so, call `libc_error(SB)` to get a pointer to the thread-local `errno`
3. Load and negate the errno value (the Go runtime expects negative errno, same as Linux kernel convention)

### exit trampoline

```asm
// src/runtime/sys_darwin_arm64.s, lines 72-77

TEXT runtime·exit_trampoline(SB),NOSPLIT|NOFRAME,$0
    MOVW    0(R0), R0
    BL      libc_exit(SB)
    MOVD    $1234, R0
    MOVD    $1002, R1
    MOVD    R0, (R1)        // fail hard
```

The crash at the end (writing to address 1002) is a safety net -- if `libc_exit` somehow returns, the process crashes immediately rather than continuing in an undefined state.

### Linux vs. macOS: Summary

| Aspect | Linux | macOS |
|---|---|---|
| Syscall method | Direct `SYSCALL` instruction | Call through libc (`BL libc_*`) |
| ABI stability | Syscall numbers are stable ABI | Only libc API is stable |
| Error reporting | Negative return in `RAX` | Return `-1`, check `errno` |
| Why? | Linux guarantees syscall ABI | Apple reserves right to change syscall numbers |

## 4. The syscall Package: Go Wrappers (10 min)

User code doesn't call the runtime's raw assembly directly. Instead, the `syscall` package provides Go-level wrappers.

### The Syscall/RawSyscall split

From `src/syscall/syscall_linux.go` (lines 55-90):

```go
// src/syscall/syscall_linux.go, lines 55-57
func RawSyscall(trap, a1, a2, a3 uintptr) (r1, r2 uintptr, err Errno) {
    return RawSyscall6(trap, a1, a2, a3, 0, 0, 0)
}
```

```go
// src/syscall/syscall_linux.go, lines 63-68
func RawSyscall6(trap, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err Errno) {
    var errno uintptr
    r1, r2, errno = linux.Syscall6(trap, a1, a2, a3, a4, a5, a6)
    err = Errno(errno)
    return
}
```

`RawSyscall` goes directly to the kernel without notifying the Go scheduler. This is dangerous -- if the syscall blocks, the scheduler doesn't know, and it can't reuse the goroutine's P.

The safe version notifies the scheduler:

```go
// src/syscall/syscall_linux.go, lines 73-90
func Syscall(trap, a1, a2, a3 uintptr) (r1, r2 uintptr, err Errno) {
    runtime_entersyscall()
    r1, r2, err = RawSyscall6(trap, a1, a2, a3, 0, 0, 0)
    runtime_exitsyscall()
    return
}
```

The critical difference: `Syscall` wraps the raw call with `runtime_entersyscall()` / `runtime_exitsyscall()`. These are linked to the runtime's `entersyscall` and `exitsyscall` functions:

```go
// src/syscall/syscall_linux.go, lines 29-33
//go:linkname runtime_entersyscall runtime.entersyscall
func runtime_entersyscall()

//go:linkname runtime_exitsyscall runtime.exitsyscall
func runtime_exitsyscall()
```

### When to use which?

- **`Syscall`**: For potentially blocking calls (`read`, `write`, `open`, `connect`). Notifies the scheduler so the P can be reassigned.
- **`RawSyscall`**: For calls guaranteed to be fast and non-blocking (`getpid`, `getuid`). Avoids the overhead of scheduler notification.

Using `RawSyscall` for a blocking call is a bug: it can cause the entire program to stall because the scheduler thinks the M's P is still in use.

### Errno: Error Handling

From `src/syscall/syscall_unix.go` (lines 94-108):

```go
// src/syscall/syscall_unix.go, lines 94-108

// An Errno is an unsigned number describing an error condition.
// It implements the error interface. The zero Errno is by convention
// a non-error, so code to convert from Errno to error should use:
//
//  err = nil
//  if errno != 0 {
//      err = errno
//  }
//
// Errno values can be tested against error values using [errors.Is].
// For example:
//
//  _, _, err := syscall.Syscall(...)
//  if errors.Is(err, fs.ErrNotExist) ...
type Errno uintptr
```

`Errno` is just a `uintptr` that implements the `error` interface. It maps to kernel errno values (`ENOENT` = 2, `EACCES` = 13, etc.).

The `Errno.Is()` method (lines 120-131) maps errno values to Go's standard error sentinels:

```go
// src/syscall/syscall_unix.go, lines 120-131
func (e Errno) Is(target error) bool {
    switch target {
    case oserror.ErrPermission:
        return e == EACCES || e == EPERM
    case oserror.ErrExist:
        return e == EEXIST || e == ENOTEMPTY
    case oserror.ErrNotExist:
        return e == ENOENT
    case errorspkg.ErrUnsupported:
        return e == ENOSYS || e == ENOTSUP || e == EOPNOTSUPP
    }
    return false
}
```

### Higher-level wrappers: Read and Write

The `syscall` package also provides higher-level Go functions. For example:

```go
// src/syscall/syscall_unix.go, lines 182-199
func Read(fd int, p []byte) (n int, err error) {
    n, err = read(fd, p)
    if race.Enabled {
        if n > 0 {
            race.WriteRange(unsafe.Pointer(&p[0]), n)
        }
        if err == nil {
            race.Acquire(unsafe.Pointer(&ioSync))
        }
    }
    if msan.Enabled && n > 0 {
        msan.Write(unsafe.Pointer(&p[0]), uintptr(n))
    }
    if asan.Enabled && n > 0 {
        asan.Write(unsafe.Pointer(&p[0]), uintptr(n))
    }
    return
}
```

Note how the wrapper integrates with Go's race detector, memory sanitizer (MSan), and address sanitizer (ASan). These instrumentation hooks are invisible to normal users but critical for development tooling.

## 5. VDSO: Avoiding the Kernel (5 min)

Some syscalls are so frequent that even the overhead of the `SYSCALL` instruction is too much. The most common example is `clock_gettime` -- called every time the runtime needs a timestamp (scheduling decisions, timer management, `time.Now()`).

Linux provides the **vDSO** (virtual Dynamic Shared Object): a small shared library that the kernel maps into every process's address space. It contains user-space implementations of certain syscalls that can execute without trapping into the kernel.

The Go runtime looks up the vDSO symbol at startup (`src/runtime/vdso_linux.go`) and uses it as a fast path:

```asm
// src/runtime/sys_linux_amd64.s, lines 222-298

// func nanotime1() int64
TEXT runtime·nanotime1(SB),NOSPLIT,$16-8
    // We don't know how much stack space the VDSO code will need,
    // so switch to g0.
    ...
    MOVQ    g_m(R14), BX

    // Set vdsoPC and vdsoSP for SIGPROF traceback.
    ...
    MOVL    $1, DI // CLOCK_MONOTONIC
    LEAQ    0(SP), SI
    MOVQ    runtime·vdsoClockgettimeSym(SB), AX
    CMPQ    AX, $0
    JEQ     fallback
    CALL    AX          // call vDSO function (user-space, no kernel trap!)
ret:
    MOVQ    0(SP), AX   // sec
    MOVQ    8(SP), DX   // nsec
    ...
    IMULQ   $1000000000, AX
    ADDQ    DX, AX
    MOVQ    AX, ret+0(FP)
    RET
fallback:
    MOVQ    $SYS_clock_gettime, AX
    SYSCALL             // fall back to real syscall if vDSO unavailable
    JMP     ret
```

The key optimization:

1. Check if `runtime.vdsoClockgettimeSym` is non-nil (the vDSO symbol was found at startup)
2. If yes, `CALL AX` -- this is a normal function call, **no kernel transition**
3. If no, fall back to `SYSCALL` with `SYS_clock_gettime`

The vDSO implementation works by having the kernel map a read-only page of time data into user space, updated by the kernel on every timer tick. The user-space code just reads from this page. This turns a ~100ns syscall into a ~20ns function call.

Note the comment: "We don't know how much stack space the VDSO code will need, so switch to g0." The runtime switches to the `g0` stack (the OS-thread-sized stack) before calling vDSO code, because the vDSO is essentially kernel-provided C code that assumes a normal-sized stack -- not the 2-8 KB goroutine stack.

## 6. entersyscall/exitsyscall: The Scheduler Handoff (10 min)

This is the most OS-like aspect of Go's syscall handling. When a goroutine enters a potentially blocking syscall, the runtime must decide what to do with the goroutine's P (logical processor).

### The Problem

Consider: goroutine G7 is running on M3 with P2. G7 calls `read()` on a network socket. This could block for milliseconds or even seconds. If P2 stays attached to M3 during the block, no other goroutine can use that logical processor -- the system loses 1/GOMAXPROCS of its scheduling capacity.

### entersyscall: Preparing for the Block

```go
// src/runtime/proc.go, lines 4627-4716
func reentersyscall(pc, sp, bp uintptr) {
    gp := getg()

    // Disable preemption because during this function g is in Gsyscall status,
    // but can have inconsistent g->sched, do not let GC observe it.
    gp.m.locks++

    // Entersyscall must not call any function that might split/grow the stack.
    // Catch calls that might, by replacing the stack guard with something that
    // will trip any stack check and leaving a flag to tell newstack to die.
    gp.stackguard0 = stackPreempt
    gp.throwsplit = true

    // Copy the syscalltick over so we can identify if the P got stolen later.
    gp.m.syscalltick = gp.m.p.ptr().syscalltick

    ...
    // Leave SP around for GC and traceback.
    save(pc, sp, bp)
    gp.syscallsp = sp
    gp.syscallpc = pc
    gp.syscallbp = bp

    ...
    // Transition to _Gsyscall.
    // As soon as we switch to _Gsyscall, we are in danger of losing our P.
    if gp.bubble != nil || !gp.atomicstatus.CompareAndSwap(_Grunning, _Gsyscall) {
        casgstatus(gp, _Grunning, _Gsyscall)
    }
    ...
}
```

Key steps:
1. **Save the goroutine's registers** (PC, SP, BP) so the GC and traceback can still walk the stack
2. **Record `syscalltick`** so the M can detect later if its P was stolen
3. **Transition G status** from `_Grunning` to `_Gsyscall` via atomic CAS
4. **Do NOT release the P** immediately -- the optimistic assumption is that the syscall will return quickly

The P remains attached to the M, but the status change to `_Gsyscall` signals to the rest of the runtime that this P is available for stealing if necessary.

### exitsyscall: Coming Back

```go
// src/runtime/proc.go, lines 4883-4962
func exitsyscall() {
    gp := getg()

    gp.m.locks++ // see comment in entersyscall
    if sys.GetCallerSP() > gp.syscallsp {
        throw("exitsyscall: syscall frame is no longer valid")
    }
    gp.waitsince = 0

    ...
    // Transition from _Gsyscall back to _Grunning.
    if gp.bubble != nil || !gp.atomicstatus.CompareAndSwap(_Gsyscall, _Grunning) {
        casgstatus(gp, _Gsyscall, _Grunning)
    }

    // Check if we still have our P.
    oldp := gp.m.oldp.ptr()
    gp.m.oldp.set(nil)

    pp := gp.m.p.ptr()
    if pp != nil {
        // Fast path: we still have our P. Just emit a trace event.
        ...
    } else {
        // Slow path: we lost our P. Try to get another one.
        systemstack(func() {
            if pp := exitsyscallTryGetP(oldp); pp != nil {
                // Got a P, install it and continue.
                ...
            } else {
                // No P available. Park this M and put G on global run queue.
                exitsyscallNoP(gp)
            }
        })
    }
    ...
}
```

Two paths:

1. **Fast path** (common case): The P is still attached. The syscall was fast enough that `sysmon` didn't steal the P. Transition back to `_Grunning` and continue executing.

2. **Slow path**: The P was stolen (by `sysmon`'s `retake()` function). The M must either acquire a different idle P or park the goroutine on the global run queue and park the M.

### sysmon retake: Stealing Ps from Slow Syscalls

The `sysmon` thread periodically checks for Ps that are stuck in syscalls:

```go
// src/runtime/proc.go, lines 6630-6670
func retake(now int64) uint32 {
    n := 0
    lock(&allpLock)
    for i := 0; i < len(allp); i++ {
        pp := allp[i]
        if pp == nil || atomic.Load(&pp.status) != _Prunning {
            continue
        }
        pd := &pp.sysmontick

        // Preempt G if it's running on the same schedtick for
        // too long. This could be from a single long-running
        // goroutine or a sequence of goroutines run via
        // runnext, which share a single schedtick time slice.
        schedt := int64(pp.schedtick)
        if int64(pd.schedtick) != schedt {
            pd.schedtick = uint32(schedt)
            pd.schedwhen = now
        } else if pd.schedwhen+forcePreemptNS <= now {
            preemptone(pp)
            // If pp is in a syscall, preemptone doesn't work.
            // The goroutine nor the thread can respond to a
            // preemption request because they're not in Go code,
            // so we need to take the P ourselves.
            sysretake = true
        }
        ...
    }
}
```

`sysmon` checks the `syscalltick` to see how long the P has been in a syscall. If it's been too long (~20 microseconds for the first check, increasing with backoff), `sysmon` steals the P and hands it to another M that has goroutines waiting to run.

### The Full Lifecycle

```
  Goroutine G7 on M3 with P2:

  1. G7 calls syscall.Read()
     → Syscall() calls runtime_entersyscall()
     → G7 status: _Grunning → _Gsyscall
     → P2 stays attached to M3 (optimistic)
     → Raw syscall to kernel

  2a. FAST: Syscall returns quickly
     → runtime_exitsyscall()
     → P2 still attached? Yes!
     → G7 status: _Gsyscall → _Grunning
     → Continue executing

  2b. SLOW: Syscall blocks for a while
     → sysmon's retake() notices P2 stuck in syscall
     → sysmon steals P2, hands it to idle M (or wakes one)
     → Other goroutines can now run on P2
     → Eventually, kernel returns from syscall
     → runtime_exitsyscall()
     → P2 gone! Try to get any idle P.
     → If no P available, park G7 on global run queue, park M3
```

This design is elegant: the common case (fast syscall) has minimal overhead (just a status CAS), while the slow case ensures the system doesn't lose scheduling capacity.

## Summary

| Topic | Key Insight |
|---|---|
| Syscall mechanism | `SYSCALL` instruction on Linux; libc calls on macOS |
| Direct vs. libc | Linux syscall ABI is stable; macOS's is not |
| Syscall vs. RawSyscall | `Syscall` notifies scheduler; `RawSyscall` does not |
| Errno | Just a `uintptr` implementing `error`; kernel returns negative values |
| VDSO | Kernel maps fast implementations into user space for time-related calls |
| entersyscall | Saves registers, transitions G to `_Gsyscall`, keeps P optimistically |
| exitsyscall | Fast path: P still there. Slow path: acquire new P or park. |
| sysmon retake | Steals Ps from goroutines stuck in long syscalls |

## Exercises

1. **Trace a syscall**: Write a Go program that reads from a file. Use `strace -f -e trace=read` (Linux) or `dtruss` (macOS) to observe the actual system calls. Compare the syscall arguments with what you see in the assembly.

2. **Syscall vs. RawSyscall**: Write a program that makes a slow syscall (e.g., `read` from a pipe that takes 1 second to produce data) using both `syscall.Syscall` and `syscall.RawSyscall`. With `GOMAXPROCS=1` and a second goroutine printing to stdout, observe the difference in behavior.

3. **VDSO experiment**: Write a tight loop calling `time.Now()` one million times and measure elapsed time. Compare this with a tight loop calling `syscall.Syscall(SYS_clock_gettime, ...)`. How much faster is the vDSO path?

4. **Read the entersyscall code**: Open `src/runtime/proc.go` and read `reentersyscall` (line 4627) through `exitsyscall` (line 4962). Draw a state diagram showing all the possible transitions for G status, P ownership, and M activity during a syscall.

## Further Reading

- Linux syscall table: `arch/x86/entry/syscalls/syscall_64.tbl` in the Linux kernel source
- [The Definitive Guide to Linux System Calls](https://blog.packagecloud.io/the-definitive-guide-to-linux-system-calls/) -- covers `SYSCALL`, `SYSENTER`, vDSO
- [Go source: `src/runtime/sys_linux_amd64.s`](https://cs.opensource.google/go/go/+/master:src/runtime/sys_linux_amd64.s) -- Linux syscall assembly
- [Go source: `src/runtime/sys_darwin_arm64.s`](https://cs.opensource.google/go/go/+/master:src/runtime/sys_darwin_arm64.s) -- macOS trampoline assembly
- [Go source: `src/runtime/proc.go`](https://cs.opensource.google/go/go/+/master:src/runtime/proc.go) -- `entersyscall`/`exitsyscall`
