## Module 2: System Calls

How Go Crosses the User/Kernel Boundary

---

## What is a System Call?

- User-mode code cannot access hardware directly
- **System call** = request to the kernel for a privileged operation
- On x86-64 Linux: the `SYSCALL` instruction
  - Saves user RIP/RFLAGS
  - Switches to kernel mode (ring 0)
  - Dispatches based on syscall number in `RAX`
  - Returns via `SYSRET`

---

## x86-64 Linux Syscall Convention

| Register | Purpose |
|---|---|
| `RAX` | Syscall number (in) / return value (out) |
| `RDI` | Argument 1 |
| `RSI` | Argument 2 |
| `RDX` | Argument 3 |
| `R10` | Argument 4 |
| `R8`  | Argument 5 |
| `R9`  | Argument 6 |

On error: `RAX` contains negative errno (e.g., -2 = `ENOENT`)

---

## Go Bypasses libc on Linux

- Linux syscall numbers are **stable ABI**
- Go calls `SYSCALL` directly from assembly
- No dependency on glibc/musl
- This is why Go produces static binaries easily

All syscall wrappers live in `src/runtime/sys_linux_amd64.s`

---

## Syscall Numbers

```asm
// src/runtime/sys_linux_amd64.s, lines 16-49

#define SYS_read             0
#define SYS_write            1
#define SYS_close            3
#define SYS_mmap             9
#define SYS_munmap           11
#define SYS_brk              12
#define SYS_rt_sigaction     13
#define SYS_clone            56
#define SYS_exit             60
#define SYS_futex            202
#define SYS_clock_gettime    228
#define SYS_exit_group       231
#define SYS_openat           257
```

Same numbers as in the Linux kernel's syscall table.

---

## exit: The Simplest Syscall

```asm
// src/runtime/sys_linux_amd64.s, lines 51-55

TEXT runtime·exit(SB),NOSPLIT,$0-4
    MOVL    code+0(FP), DI      // arg 1: exit code
    MOVL    $SYS_exit_group, AX // syscall 231
    SYSCALL
    RET
```

- `NOSPLIT` = don't grow the stack (we're leaving Go's world)
- Uses `exit_group` (not `exit`) to kill all threads
- `RET` is never reached

---

## read and write

```asm
// src/runtime/sys_linux_amd64.s, lines 93-109

TEXT runtime·write1(SB),NOSPLIT,$0-28
    MOVQ    fd+0(FP), DI       // arg 1: fd
    MOVQ    p+8(FP), SI        // arg 2: buf
    MOVL    n+16(FP), DX       // arg 3: count
    MOVL    $SYS_write, AX     // syscall 1
    SYSCALL
    MOVL    AX, ret+24(FP)     // return value
    RET

TEXT runtime·read(SB),NOSPLIT,$0-28
    MOVL    fd+0(FP), DI
    MOVQ    p+8(FP), SI
    MOVL    n+16(FP), DX
    MOVL    $SYS_read, AX      // syscall 0
    SYSCALL
    MOVL    AX, ret+24(FP)
    RET
```

---

## The Pattern

Every Linux syscall wrapper follows the same template:

1. Load Go function args into Linux ABI registers (`DI`, `SI`, `DX`, `R10`, `R8`, `R9`)
2. Load syscall number into `AX`
3. Execute `SYSCALL`
4. Store return value from `AX` back to Go stack

That's it. No libc, no overhead, no dynamic linking.

---

## Error Handling in Assembly

```asm
// src/runtime/sys_linux_amd64.s, lines 69-81

TEXT runtime·open(SB),NOSPLIT,$0-20
    MOVL    $AT_FDCWD, DI
    MOVQ    name+0(FP), SI
    MOVL    mode+8(FP), DX
    MOVL    perm+12(FP), R10
    MOVL    $SYS_openat, AX
    SYSCALL
    CMPQ    AX, $0xfffffffffffff001  // -4095 unsigned
    JLS     2(PC)
    MOVL    $-1, AX
    MOVL    AX, ret+16(FP)
    RET
```

- Returns in range `[-4095, -1]` = error (negative errno)
- `0xfffffffffffff001` = `-4095` as unsigned 64-bit
- Uses `openat` + `AT_FDCWD` because Android blocked `open`

---

## macOS: A Different World

Apple does **not** guarantee a stable syscall ABI.

- Syscall numbers can change between macOS versions
- Go **must** call through libc
- The runtime uses "trampolines" that convert Go calling convention to C calling convention

---

## macOS Trampoline Pattern

```asm
// src/runtime/sys_darwin_arm64.s, lines 21-29

TEXT runtime·open_trampoline(SB),NOSPLIT,$0
    SUB     $16, RSP
    MOVW    8(R0), R1       // arg 2 flags
    MOVW    12(R0), R2      // arg 3 mode
    MOVW    R2, (RSP)       // variadic arg on stack
    MOVD    0(R0), R0       // arg 1 pathname
    BL      libc_open(SB)   // call libc, NOT direct syscall
    ADD     $16, RSP
    RET
```

Key: `BL libc_open(SB)` -- calls C library, which makes the real syscall.

---

## macOS Error Handling

```asm
// src/runtime/sys_darwin_arm64.s, lines 50-62

TEXT runtime·read_trampoline(SB),NOSPLIT,$0
    MOVD    8(R0), R1       // arg 2 buf
    MOVW    16(R0), R2      // arg 3 count
    MOVW    0(R0), R0       // arg 1 fd
    BL      libc_read(SB)
    MOVD    $-1, R1
    CMP     R0, R1
    BNE     noerr
    BL      libc_error(SB)  // get thread-local errno
    MOVW    (R0), R0
    NEG     R0, R0          // negate to match Linux convention
noerr:
    RET
```

- libc returns `-1` on error, sets `errno`
- Trampoline fetches `errno` and negates it

---

## Linux vs. macOS Summary

| | Linux | macOS |
|---|---|---|
| Method | Direct `SYSCALL` | libc trampolines |
| ABI stability | Syscall numbers guaranteed | Only libc API guaranteed |
| Error reporting | Negative return in RAX | Return -1, check errno |
| Implication | Static binaries easy | Must link libc |

---

## The syscall Package

User code uses the `syscall` package, not raw assembly.

Two flavors:

```go
// Notifies scheduler -- safe for blocking calls
func Syscall(trap, a1, a2, a3 uintptr) (r1, r2 uintptr, err Errno)

// Does NOT notify scheduler -- only for fast, non-blocking calls
func RawSyscall(trap, a1, a2, a3 uintptr) (r1, r2 uintptr, err Errno)
```

---

## Syscall vs. RawSyscall

```go
// src/syscall/syscall_linux.go, lines 73-90
func Syscall(trap, a1, a2, a3 uintptr) (r1, r2 uintptr, err Errno) {
    runtime_entersyscall()    // tell scheduler we're leaving
    r1, r2, err = RawSyscall6(trap, a1, a2, a3, 0, 0, 0)
    runtime_exitsyscall()     // tell scheduler we're back
    return
}

func RawSyscall(trap, a1, a2, a3 uintptr) (r1, r2 uintptr, err Errno) {
    return RawSyscall6(trap, a1, a2, a3, 0, 0, 0)
    // No scheduler notification!
}
```

**Rule**: Use `Syscall` for anything that might block. `RawSyscall` for guaranteed-fast calls only.

---

## Errno: Error Type

```go
// src/syscall/syscall_unix.go, lines 94-108

// An Errno is an unsigned number describing an error condition.
// It implements the error interface.
type Errno uintptr

func (e Errno) Is(target error) bool {
    switch target {
    case oserror.ErrPermission:
        return e == EACCES || e == EPERM
    case oserror.ErrNotExist:
        return e == ENOENT
    case errorspkg.ErrUnsupported:
        return e == ENOSYS || e == ENOTSUP || e == EOPNOTSUPP
    }
    return false
}
```

Maps kernel errno values to Go's `errors.Is()` interface.

---

## VDSO: Avoiding the Kernel

Some syscalls are called *constantly* (e.g., `clock_gettime` for scheduling).

**vDSO** (virtual Dynamic Shared Object):
- Kernel maps a shared library into every process
- Contains user-space implementations of time functions
- ~20ns function call vs. ~100ns kernel trap

---

## VDSO in the Runtime

```asm
// src/runtime/sys_linux_amd64.s, lines 222-298

TEXT runtime·nanotime1(SB),NOSPLIT,$16-8
    ...
    MOVL    $1, DI // CLOCK_MONOTONIC
    LEAQ    0(SP), SI
    MOVQ    runtime·vdsoClockgettimeSym(SB), AX
    CMPQ    AX, $0
    JEQ     fallback
    CALL    AX              // vDSO: user-space call, no trap!
    ...
    RET
fallback:
    MOVQ    $SYS_clock_gettime, AX
    SYSCALL                 // real syscall if vDSO unavailable
    JMP     ret
```

Switches to `g0` stack first -- vDSO code expects a normal-sized stack.

---

## entersyscall: The Problem

Goroutine G7 runs on M3 with P2 and calls `read()`:

- `read()` might block for **seconds**
- P2 is stuck -- no other goroutine can use that CPU
- System loses 1/GOMAXPROCS of capacity

**Solution**: The scheduler can steal P2 from M3 during the syscall.

---

## entersyscall: How It Works

```go
// src/runtime/proc.go, lines 4627-4716
func reentersyscall(pc, sp, bp uintptr) {
    gp := getg()
    gp.m.locks++

    // Prevent stack growth during transition
    gp.stackguard0 = stackPreempt
    gp.throwsplit = true

    // Save tick so we can detect P theft later
    gp.m.syscalltick = gp.m.p.ptr().syscalltick

    // Save registers for GC and traceback
    save(pc, sp, bp)
    gp.syscallsp = sp
    gp.syscallpc = pc

    // Key transition: _Grunning → _Gsyscall
    gp.atomicstatus.CompareAndSwap(_Grunning, _Gsyscall)
}
```

P stays attached (optimistic). Status change signals "available for stealing."

---

## exitsyscall: Two Paths

```go
// src/runtime/proc.go, lines 4883-4962
func exitsyscall() {
    gp := getg()
    // Transition: _Gsyscall → _Grunning
    gp.atomicstatus.CompareAndSwap(_Gsyscall, _Grunning)

    pp := gp.m.p.ptr()
    if pp != nil {
        // FAST PATH: P still here. Continue running.
    } else {
        // SLOW PATH: P was stolen.
        // Try to get any idle P.
        // If none available: park G on global queue, park M.
    }
}
```

---

## sysmon retake: Stealing Ps

```go
// src/runtime/proc.go, lines 6630-6670
func retake(now int64) uint32 {
    for i := 0; i < len(allp); i++ {
        pp := allp[i]
        if pp == nil || atomic.Load(&pp.status) != _Prunning {
            continue
        }
        // Has this P been in a syscall too long?
        if pd.schedwhen+forcePreemptNS <= now {
            preemptone(pp)
            sysretake = true
        }
        ...
    }
}
```

`sysmon` runs every 20us-10ms, checks all Ps, steals from stuck syscalls.

---

## The Complete Syscall Lifecycle

```
 G7 calls read()
     │
     ▼
 entersyscall()
 G7: _Grunning → _Gsyscall
 P2 stays on M3 (optimistic)
     │
     ▼
 SYSCALL (into kernel)
     │
     ├── Fast return ──────────────────┐
     │                                 ▼
     │                          exitsyscall()
     │                          P2 still here!
     │                          G7: _Gsyscall → _Grunning
     │                          Continue running
     │
     ├── Slow... sysmon notices ──┐
     │                            ▼
     │                     retake() steals P2
     │                     P2 → another M
     │                            │
     └── Eventually returns ──────┘
                                  ▼
                           exitsyscall()
                           P2 gone! Try idle P.
                           No P? Park G7, park M3.
```

---

## Design Insight

The syscall mechanism reveals a core OS tradeoff:

- **Optimistic**: Don't release P immediately (fast syscalls are common)
- **Background monitor**: `sysmon` catches the slow cases
- **Separation of concerns**: The kernel handles the actual I/O; the runtime manages scheduling around it

This is exactly how an OS kernel manages I/O-bound vs. CPU-bound processes -- just implemented in user space.

---

## Key Takeaways

1. Go makes **direct syscalls** on Linux (stable ABI), uses **libc trampolines** on macOS
2. `Syscall` vs. `RawSyscall`: scheduler notification is the difference
3. **VDSO** eliminates kernel transitions for hot paths like `clock_gettime`
4. **entersyscall/exitsyscall** let the scheduler reclaim Ps from blocking syscalls
5. **sysmon** is the watchdog that steals Ps from stuck syscalls

---

## Exercises

1. Use `strace -f` to trace syscalls from a Go program
2. Compare `Syscall` vs. `RawSyscall` with GOMAXPROCS=1
3. Benchmark `time.Now()` (vDSO) vs. raw `clock_gettime` syscall
4. Read `reentersyscall` in `proc.go` line 4627 and draw the state diagram

---

## Next: Process Scheduling

How does the Go scheduler decide which goroutine to run?

- `schedule()` and `findRunnable()`
- Work stealing algorithm
- Global vs. local run queues
- The `runnext` optimization
