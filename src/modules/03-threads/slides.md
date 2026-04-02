# Module 3: Processes, Threads, and Goroutines

---

## Slide 1: Module Overview

**From OS Processes to Goroutines**

- OS processes: isolation and resource ownership
- OS threads: shared address space, independent execution
- Why threads are expensive
- Go's answer: goroutines
- The M struct (OS thread) and G struct (goroutine)
- Goroutine states and transitions

---

## Slide 2: What is a Process?

The OS's unit of **resource ownership** and **protection**:

- **Address space** -- isolated virtual memory (text, data, heap, stack)
- **File descriptor table** -- open files, sockets, pipes
- **Process table entry** -- PID, credentials, signal handlers, limits
- **One or more threads** of execution

Creating a process: `fork()` + `exec()`
- Duplicates page tables (copy-on-write)
- Allocates kernel data structures
- Flushes TLB

---

## Slide 3: Why Not One Process per Task?

10,000 concurrent connections with 10,000 processes:

- Each process: own page tables, own kernel scheduling entity
- Communication requires IPC (pipes, shared memory) -- kernel transitions
- Context switch: full TLB flush, page table swap

**Too expensive for fine-grained concurrency.**

---

## Slide 4: What is an OS Thread?

An independent execution context **within** a process.

**Shared** with other threads:
- Address space (code, heap, globals)
- File descriptors
- Signal handlers, PID

**Private** to each thread:
- Stack (1-8 MB)
- Register set
- Thread ID
- Signal mask

---

## Slide 5: Cost of OS Threads -- Stack Size

Default thread stack sizes:
- Linux: **8 MB** (only touched pages physically allocated)
- macOS secondary threads: **512 KB**

10,000 threads on Linux:
- 10,000 x 8 MB = **80 GB virtual address space** just for stacks
- Even with lazy allocation, this is problematic

---

## Slide 6: Cost of OS Threads -- Context Switch

Switching between OS threads requires:

1. Save all CPU registers to kernel memory
2. Switch kernel stack pointer
3. Potentially flush CPU caches / TLB
4. Restore registers for new thread
5. Return from kernel to user mode

**Cost: ~1-10 microseconds** per switch

---

## Slide 7: Cost of OS Threads -- Kernel Involvement

Every thread operation is a **system call**:

- `clone()` / `pthread_create()` -- creation
- `futex()` -- blocking / waking
- `sched_yield()` -- voluntary yield
- Thread exit and cleanup

Each syscall: ~100ns+ kernel entry/exit overhead

---

## Slide 8: Thread Creation on Linux -- clone()

```go
// src/runtime/os_linux.go, lines 170-201
func newosproc(mp *m) {
    stk := unsafe.Pointer(mp.g0.stack.hi)
    var oset sigset
    sigprocmask(_SIG_SETMASK, &sigset_all, &oset)
    ret := retryOnEAGAIN(func() int32 {
        r := clone(cloneFlags, stk, unsafe.Pointer(mp),
            unsafe.Pointer(mp.g0),
            unsafe.Pointer(abi.FuncPCABI0(mstart)))
        // ...
    })
    sigprocmask(_SIG_SETMASK, &oset, nil)
}
```

Directly issues `SYS_clone` -- Go bypasses libc.

---

## Slide 9: Thread Creation on Darwin -- pthread_create()

```go
// src/runtime/os_darwin.go, lines 224-258
func newosproc(mp *m) {
    var attr pthreadattr
    pthread_attr_init(&attr)
    pthread_attr_getstacksize(&attr, &stacksize)
    pthread_attr_setdetachstate(&attr, _PTHREAD_CREATE_DETACHED)

    err = retryOnEAGAIN(func() int32 {
        return pthread_create(&attr,
            abi.FuncPCABI0(mstart_stub), unsafe.Pointer(mp))
    })
}
```

macOS requires POSIX threads API -- no raw `clone()`.

---

## Slide 10: The clone() System Call in Assembly

```asm
;; src/runtime/sys_linux_amd64.s, lines 574-599
TEXT runtime·clone(SB),NOSPLIT|NOFRAME,$0
    MOVL    flags+0(FP), DI
    MOVQ    stk+8(FP), SI
    ;; Copy mp, gp, fn off parent stack for child
    MOVQ    mp+16(FP), R13
    MOVQ    gp+24(FP), R9
    MOVQ    fn+32(FP), R12
    ;; ...TLS setup...
    MOVL    $SYS_clone, AX
    SYSCALL
```

- Passes new stack pointer, M pointer, G pointer
- Child thread starts at `mstart` -> enters scheduler

---

## Slide 11: Go's Answer -- Goroutines

What if we could have threads that are:
- **Cheap to create** (~1 microsecond, not ~100)
- **Tiny stacks** (2 KB, not 1-8 MB)
- **Fast to switch** (~200 ns, not ~1-10 us)
- **Scheduled in user space** (no kernel involvement)

That is exactly what goroutines are.

---

## Slide 12: The M Struct -- OS Thread in Go

```go
// src/runtime/runtime2.go, lines 618-670
type m struct {
    g0       *g        // scheduling stack goroutine
    curg     *g        // current running goroutine
    p        puintptr  // attached P (for executing Go code)
    nextp    puintptr  // next P to attach
    id       int64
    spinning bool      // actively looking for work
    blocked  bool      // blocked on a note
    park     note      // for sleeping when idle
    alllink  *m        // linked list of all Ms
    lockedg  guintptr  // goroutine locked to this M
    // ...
}
```

---

## Slide 13: The g0 -- Scheduling Stack

Every M has a special goroutine `g0`:

- Runs **scheduler code** (finding work, context switches)
- Has a **large, fixed stack** (not a tiny 2 KB goroutine stack)
- When user code needs to call the scheduler, it uses `mcall()` or `systemstack()` to switch to g0

```
User goroutine (2KB stack)      g0 (system stack)
        |                            |
        |--- mcall(park_m) --------->|
        |                            |-- runs scheduler
        |                            |-- finds new G
        |<--- gogo(&gp.sched) ------|
        |                            |
```

---

## Slide 14: The G Struct -- Goroutine

```go
// src/runtime/runtime2.go, lines 473-509
type g struct {
    stack       stack   // [stack.lo, stack.hi)
    stackguard0 uintptr // for stack growth checks
    _panic      *_panic
    _defer      *_defer
    m           *m      // current M (nil if not running)
    sched       gobuf   // saved registers
    atomicstatus atomic.Uint32
    goid         uint64
    waitsince    int64
    waitreason   waitReason
    preempt      bool
    parentGoid   uint64  // who created us
    startpc      uintptr // function we're running
    // ...
}
```

---

## Slide 15: Goroutine Stack -- Starts at 2 KB

```go
// src/runtime/stack.go, line 78
stackMin = 2048
```

Goroutine stacks **grow dynamically**:

1. Every function prologue checks `stackguard0`
2. If stack space is insufficient, call `morestack`
3. Allocate new stack (2x size), copy old stack
4. Update all pointers into the stack
5. Continue execution on new stack

Can grow up to ~1 GB. Shrinks during GC if underutilized.

---

## Slide 16: The gobuf -- Saved Execution State

```go
// src/runtime/runtime2.go, lines 303-322
type gobuf struct {
    sp   uintptr        // stack pointer
    pc   uintptr        // program counter
    g    guintptr       // back-pointer to g
    ctxt unsafe.Pointer // for closures
    lr   uintptr        // link register (ARM)
    bp   uintptr        // base pointer (x86)
}
```

**Only 6 values** to save/restore for a goroutine switch.

OS thread switch must save: 16+ GPRs, FP/SSE/AVX state, kernel stack, control registers...

---

## Slide 17: Goroutine States Overview

```
_Gidle (0)      -- just allocated, not initialized
_Grunnable (1)  -- on a run queue, ready to execute
_Grunning (2)   -- executing on an M
_Gsyscall (3)   -- in a system call
_Gwaiting (4)   -- blocked (channel, mutex, sleep...)
_Gdead (6)      -- finished or on free list
_Gcopystack (8) -- stack being relocated
_Gpreempted (9) -- stopped for preemption
```

Source: `src/runtime/runtime2.go`, lines 17-99

---

## Slide 18: State Transitions -- The Happy Path

```
         newproc1()           execute()
_Gdead ──────────► _Grunnable ──────────► _Grunning
   ▲                    ▲                      │
   │                    │                      │
   │                    │ goready()        gopark()
   │                    │                      │
   │                    └──── _Gwaiting ◄──────┘
   │                                           │
   └───────────── goexit0() ◄──────────────────┘
```

---

## Slide 19: _Grunnable -- "Ready to Run"

```go
// _Grunnable means this goroutine is on a run queue. It is
// not currently executing user code. The stack is not owned.
_Grunnable // 1
```

- Goroutine is on a **local or global run queue**
- Waiting to be picked up by a P/M pair
- Stack is **not owned** -- GC can scan it

---

## Slide 20: _Grunning -- "Executing"

```go
// _Grunning means this goroutine may execute user code.
// The stack is owned by this goroutine. It is assigned an M
// and it usually has a P.
_Grunning // 2
```

- Actively executing user code
- Has an M (`g.m != nil`) and usually a P (`g.m.p != nil`)
- **Owns its stack** -- GC cannot scan it directly

---

## Slide 21: _Gwaiting -- "Blocked"

```go
// _Gwaiting means this goroutine is blocked in the runtime.
// It is not executing user code. It is not on a run queue,
// but should be recorded somewhere (e.g., a channel wait
// queue) so it can be ready()d when necessary.
_Gwaiting // 4
```

Common wait reasons: channel send/recv, mutex, sleep, network I/O, select

---

## Slide 22: _Gsyscall -- "In a System Call"

```go
// _Gsyscall means this goroutine is executing a system call.
// It is not executing user code. The stack is owned by this
// goroutine. It is assigned an M.
_Gsyscall // 3
```

- Still bound to an M (the OS thread is blocked in kernel)
- The **P may be detached** and given to another M
- This is how Go avoids blocking all goroutines during syscalls

---

## Slide 23: Stack Ownership by State

| State | Stack Owner | Implication |
|-------|-------------|-------------|
| `_Grunning` | The goroutine itself | GC cannot scan |
| `_Grunnable` | Nobody | GC can scan |
| `_Gwaiting` | Nobody (mostly) | GC can scan; channel ops may touch stack under lock |
| `_Gsyscall` | The goroutine | GC needs cooperation |
| `_Gcopystack` | The copier | Stack being relocated |

Stack ownership is critical for **GC correctness**.

---

## Slide 24: Cost Comparison

| | OS Thread | Goroutine |
|---|---|---|
| **Initial stack** | 1-8 MB | 2 KB |
| **Creation** | ~100 us | ~1 us |
| **Context switch** | ~1-10 us | ~200 ns |
| **Scheduling** | Kernel | User-space |
| **Struct overhead** | ~8-64 KB | ~400 bytes |
| **Practical max** | ~10,000 | ~1,000,000+ |

---

## Slide 25: The Multiplexing Model

```
  Goroutines (G):    G1   G2   G3   G4   G5  ...  G10000
                      \    |    /     \    |         /
                       \   |   /       \   |        /
  OS Threads (M):       M1          M2          M3      M4
                         |           |           |       |
  Kernel:              [  OS Scheduler (preemptive)  ]
                         |           |           |       |
  CPUs:                CPU0        CPU1        CPU2    CPU3
```

- Many Gs multiplexed onto few Ms
- GOMAXPROCS controls how many Ms run Go code simultaneously
- Next module: the **P** that connects G to M

---

## Slide 26: Discussion

1. Why does Go use `clone()` on Linux instead of `pthread_create()`?
   - Direct control over TLS, stack, signal masks
   - Avoids libc overhead and assumptions

2. Why can't Go eliminate OS threads entirely?
   - System calls block the thread
   - cgo requires real threads
   - Signals are delivered to threads

3. What enables goroutines to have 2 KB stacks?
   - Compiler inserts stack growth checks
   - Runtime can copy and relocate stacks
   - `gobuf` saves minimal state

---

## Slide 27: What's Next -- Module 4

The **GMP scheduling model**:
- How does the runtime decide which goroutine runs next?
- What is the P struct and why does it exist?
- The scheduling loop: `schedule()` -> `findRunnable()` -> `execute()`
- Work stealing, run queues, and the `runnext` optimization
