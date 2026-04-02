# Module 6: Synchronization Primitives

## Learning Objectives

By the end of this module, students will understand:
- How hardware atomics (CAS, load-acquire, store-release) underpin all synchronization
- The futex abstraction on Linux and its use in the Go runtime
- How macOS uses semaphores instead of futexes for the same purpose
- The runtime mutex implementation (lock_spinbit.go)
- The `sema.go` semaphore that powers `sync.Mutex`, including its treap data structure
- How starvation is handled with handoff mode
- The reader-writer mutex and the `rwmutexMaxReaders` trick

---

## 1. The Need for Synchronization (5 min)

### Mutual Exclusion

When multiple goroutines (or threads) access shared data, we need to ensure that at most one is modifying the data at a time. Without this, we get data races: torn reads, lost updates, and corrupted state.

### Ordering

Beyond mutual exclusion, we often need to ensure that one operation *happens before* another. A goroutine writing to a channel must complete before the goroutine reading from that channel sees the value. This requires memory ordering guarantees that go beyond simple atomicity.

### The Synchronization Stack in Go

Go has a layered synchronization architecture:

```
User-visible:     sync.Mutex, sync.RWMutex, sync.WaitGroup, channels
                          │
                          ▼
Runtime:          sema.go (semacquire1 / semrelease1)
                          │
                          ▼
Runtime:          runtime mutex (lock_spinbit.go: lock2 / unlock2)
                          │
                          ▼
OS interface:     lock_futex.go (Linux) or lock_sema.go (macOS)
                  notesleep/notewakeup, semasleep/semawakeup
                          │
                          ▼
Hardware:         atomic CAS, load-acquire, store-release
```

Each layer builds on the one below. We'll work from the bottom up.

---

## 2. Hardware Atomics (5 min)

### Compare-and-Swap (CAS)

The fundamental building block. `CAS(addr, old, new)` atomically:
1. Reads `*addr`
2. If it equals `old`, writes `new` and returns true
3. Otherwise returns false

In Go's runtime: `atomic.Cas(addr, old, new)`, `atomic.Casuintptr(addr, old, new)`.

### Load-Acquire and Store-Release

These provide memory ordering guarantees:

- **Load-Acquire** (`atomic.LoadAcq`): No subsequent memory operations can be reordered before this load. Used by consumers to see all writes that happened before the corresponding store-release.

- **Store-Release** (`atomic.StoreRel`, `atomic.CasRel`): No preceding memory operations can be reordered after this store. Used by producers to publish data.

These appear throughout the run queue implementation:

```go
// src/runtime/proc.go, lines 7664-7665 (in runqgrab)
h := atomic.LoadAcq(&pp.runqhead) // load-acquire, synchronize with other consumers
t := atomic.LoadAcq(&pp.runqtail) // load-acquire, synchronize with the producer
```

```go
// src/runtime/proc.go, line 7721 (in runqgrab)
if atomic.CasRel(&pp.runqhead, h, h+n) { // cas-release, commits consume
```

### Atomic Exchange

`atomic.Xchg(addr, new)` atomically writes `new` and returns the old value. Used in the mutex fast path:

```go
// src/runtime/lock_spinbit.go, line 165
v8 := atomic.Xchg8(k8, mutexLocked)
if v8&mutexLocked == 0 {
    // We got the lock!
    return
}
```

---

## 3. Futex: The Fundamental Blocking Primitive (Linux) (10 min)

### What is a Futex?

A futex ("fast userspace mutex") is a Linux system call that lets a thread sleep until a memory location changes value. The key insight: the **fast path** (uncontended lock/unlock) never enters the kernel. Only when contention occurs does the thread make a syscall to sleep.

The Go runtime on Linux (`lock_futex.go`) uses futexes for two purposes:
1. **One-time notifications** (`notesleep`/`notewakeup`) -- used for thread synchronization in the runtime itself
2. **Thread parking** (`semasleep`/`semawakeup`) -- used to put Ms to sleep and wake them

### notesleep / notewakeup: One-Time Notifications

A `note` is the simplest synchronization primitive: a one-shot event. One thread sleeps on it; another wakes it. It cannot be reused without calling `noteclear`.

```go
// src/runtime/lock_futex.go, lines 22-33
// One-time notifications.
func noteclear(n *note) {
    n.key = 0
}

func notewakeup(n *note) {
    old := atomic.Xchg(key32(&n.key), 1)
    if old != 0 {
        print("notewakeup - double wakeup (", old, ")\n")
        throw("notewakeup - double wakeup")
    }
    futexwakeup(key32(&n.key), 1)
}
```

The protocol:
1. `noteclear` sets `key = 0`
2. `notewakeup` atomically sets `key = 1`, then calls `futexwakeup`
3. Double wakeup is a fatal error (this is a one-shot primitive)

The sleeping side:

```go
// src/runtime/lock_futex.go, lines 35-53
func notesleep(n *note) {
    gp := getg()
    if gp != gp.m.g0 {
        throw("notesleep not on g0")
    }
    ns := int64(-1)
    if *cgo_yield != nil {
        // Sleep for an arbitrary-but-moderate interval to poll libc interceptors.
        ns = 10e6
    }
    for atomic.Load(key32(&n.key)) == 0 {
        gp.m.blocked = true
        futexsleep(key32(&n.key), 0, ns)
        if *cgo_yield != nil {
            asmcgocall(*cgo_yield, nil)
        }
        gp.m.blocked = false
    }
}
```

Key details:
- **Must run on g0** (the system stack). This is a low-level primitive for blocking OS threads, not goroutines.
- The `futexsleep(key32(&n.key), 0, ns)` call says: "sleep if `*key == 0`". The kernel checks the value atomically with going to sleep, avoiding the lost-wakeup race.
- The outer `for` loop handles spurious wakeups.
- With CGo, it polls periodically (every 10ms) for libc interceptors.

### semasleep / semawakeup: Thread Parking

These are used by the runtime mutex to park and wake OS threads:

```go
// src/runtime/lock_futex.go, lines 138-163
//go:nosplit
func semasleep(ns int64) int32 {
    mp := getg().m

    for v := atomic.Xadd(&mp.waitsema, -1); ; v = atomic.Load(&mp.waitsema) {
        if int32(v) >= 0 {
            return 0
        }
        futexsleep(&mp.waitsema, v, ns)
        if ns >= 0 {
            if int32(v) >= 0 {
                return 0
            } else {
                return -1
            }
        }
    }
}

//go:nosplit
func semawakeup(mp *m) {
    v := atomic.Xadd(&mp.waitsema, 1)
    if v == 0 {
        futexwakeup(&mp.waitsema, 1)
    }
}
```

The protocol uses `mp.waitsema` as a counter:
- `semasleep`: decrements the counter. If it goes negative, the thread sleeps via futex.
- `semawakeup`: increments the counter. If it was negative (meaning someone is sleeping), wakes them.

This is a counting semaphore: if `semawakeup` is called before `semasleep`, the subsequent `semasleep` returns immediately.

---

## 4. Semaphore-Based Blocking (macOS / Darwin) (5 min)

macOS doesn't have futexes. Instead, `lock_sema.go` uses OS-level semaphores (pthread mutexes and condition variables under the hood). The API is the same (`notesleep`/`notewakeup`, `semasleep`/`semawakeup`), but the implementation differs.

### notesleep / notewakeup on macOS

```go
// src/runtime/lock_sema.go, lines 23-44
func notewakeup(n *note) {
    var v uintptr
    for {
        v = atomic.Loaduintptr(&n.key)
        if atomic.Casuintptr(&n.key, v, locked) {
            break
        }
    }

    // Successfully set waitm to locked.
    // What was it before?
    switch {
    case v == 0:
        // Nothing was waiting. Done.
    case v == locked:
        // Two notewakeups! Not allowed.
        throw("notewakeup - double wakeup")
    default:
        // Must be the waiting m. Wake it up.
        semawakeup((*m)(unsafe.Pointer(v)))
    }
}
```

The note's `key` field serves triple duty:
- `0` = not yet signaled, no one waiting
- `locked` (= 1) = signaled
- Any other value = pointer to the waiting M

```go
// src/runtime/lock_sema.go, lines 46-72
func notesleep(n *note) {
    gp := getg()
    if gp != gp.m.g0 {
        throw("notesleep not on g0")
    }
    semacreate(gp.m)
    if !atomic.Casuintptr(&n.key, 0, uintptr(unsafe.Pointer(gp.m))) {
        // Must be locked (got wakeup).
        if n.key != locked {
            throw("notesleep - waitm out of sync")
        }
        return
    }
    // Queued. Sleep.
    gp.m.blocked = true
    if *cgo_yield == nil {
        semasleep(-1)
    } else {
        const ns = 10e6
        for atomic.Loaduintptr(&n.key) == 0 {
            semasleep(ns)
            asmcgocall(*cgo_yield, nil)
        }
    }
    gp.m.blocked = false
}
```

The protocol:
1. Sleeper tries to CAS `key` from 0 to `&m` (its own M pointer)
2. If CAS fails, the note was already signaled -- return immediately
3. If CAS succeeds, call `semasleep(-1)` to block on the M's OS semaphore
4. Waker CASes `key` to `locked`. If the old value was an M pointer, calls `semawakeup(m)`

**Comparison with futex**: The futex version is simpler because the kernel handles the "check-and-sleep" atomicity. On macOS, the runtime must carefully encode the waiting state in the `key` field and use CAS to avoid races.

---

## 5. The Runtime Mutex (10 min)

### Architecture: lock_spinbit.go

The runtime mutex (`runtime.mutex`) is used internally throughout the Go runtime -- for the scheduler lock, timer heaps, and more. The current implementation (`lock_spinbit.go`, introduced in Go 1.24) uses an elegant bit-packing scheme.

```go
// src/runtime/lock_spinbit.go, lines 29-51
// The mutex state consists of four flags and a pointer. The flag at bit 0,
// mutexLocked, represents the lock itself. Bit 1, mutexSleeping, is a hint that
// the pointer is non-nil. The fast paths for locking and unlocking the mutex
// are based on atomic 8-bit swap operations on the low byte; bits 2 through 7
// are unused.
//
// Bit 8, mutexSpinning, is a try-lock that grants a waiting M permission to
// spin on the state word. Most other Ms must attempt to spend their time
// sleeping to reduce traffic on the cache line.
//
// Bit 9, mutexStackLocked, is a try-lock that grants an unlocking M permission
// to inspect the list of waiting Ms and to pop an M off of that stack.
//
// The upper bits hold a (partial) pointer to the M that most recently went to
// sleep. The sleeping Ms form a stack linked by their mWaitList.next fields.
```

The state word layout:

```
┌──────────────────────────────┬────┬────┬───────┬─────────┬────────┐
│  M pointer (upper bits)      │ 9  │ 8  │ 7..2  │    1    │   0    │
│  (head of waiter stack)      │stkL│spin│unused │sleeping │locked  │
└──────────────────────────────┴────┴────┴───────┴─────────┴────────┘
```

### The Fast Path: 8-bit Swap

```go
// src/runtime/lock_spinbit.go, lines 155-171
func lock2(l *mutex) {
    gp := getg()
    if gp.m.locks < 0 {
        throw("runtime·lock: lock count")
    }
    gp.m.locks++

    k8 := key8(&l.key)

    // Speculative grab for lock.
    v8 := atomic.Xchg8(k8, mutexLocked)
    if v8&mutexLocked == 0 {
        if v8&mutexSleeping != 0 {
            atomic.Or8(k8, mutexSleeping)
        }
        return
    }
    semacreate(gp.m)
    // ...slow path...
}
```

The uncontended case is a single `atomic.Xchg8` -- an 8-bit atomic swap of just the low byte. This is extremely fast because:
1. It's a single instruction on modern CPUs
2. It only touches the low byte, leaving the waiter stack pointer intact
3. The 8-bit operation avoids ABA problems with the pointer bits

### The Slow Path: Spin then Sleep

```go
// src/runtime/lock_spinbit.go, lines 177-257
    var weSpin, atTail, haveTimers bool
    v := atomic.Loaduintptr(&l.key)
tryAcquire:
    for i := 0; ; i++ {
        if v&mutexLocked == 0 {
            // Try to acquire...
        }

        if !weSpin && v&mutexSpinning == 0 && atomic.Casuintptr(&l.key, v, v|mutexSpinning) {
            v |= mutexSpinning
            weSpin = true
        }

        if weSpin || atTail || mutexPreferLowLatency(l) {
            if i < spin {
                procyield(mutexActiveSpinSize)    // ~30 PAUSE instructions
                // ...
            } else if i < spin+mutexPassiveSpinCount {
                osyield()
                // ...
            }
        }

        // Go to sleep
        gp.m.mWaitList.next = mutexWaitListHead(v)
        next := (uintptr(unsafe.Pointer(gp.m)) &^ mutexMMask) | v&mutexMMask | mutexSleeping
        if atomic.Casuintptr(&l.key, v, next) {
            semasleep(-1)
            // ...
        }
    }
```

The slow path progression:
1. **Active spin** (`procyield`): Execute PAUSE instructions. Only for multi-CPU systems, only 4 iterations.
2. **Passive spin** (`osyield`): Yield the CPU to other threads. Only 1 iteration.
3. **Sleep** (`semasleep`): Push ourselves onto the waiter stack and call the OS to sleep.

**The spin bit**: Only one thread at a time gets the `mutexSpinning` bit. This prevents all waiting threads from spinning simultaneously (which would waste CPU). The other waiters go to sleep.

**Anti-starvation**: Threads at the tail of the waiter stack (`atTail = true`) get permission to spin, preventing indefinite starvation.

### The Waiter Stack

Waiting Ms are linked through their `mWaitList.next` fields, forming a LIFO stack. The head of the stack is packed into the upper bits of `l.key`:

```go
// src/runtime/lock_spinbit.go, lines 88-91
type mWaitList struct {
    next       muintptr // next m waiting for lock
    startTicks int64    // when this m started waiting
}
```

When a thread goes to sleep, it CASes its M pointer into the upper bits of `l.key`, linking to the previous head via `mWaitList.next`. When `unlock2` wakes a thread, it pops from this stack.

---

## 6. The sema.go Semaphore (15 min)

### Purpose

The `sema.go` semaphore is the mechanism that `sync.Mutex`, `sync.RWMutex`, and `sync.WaitGroup` use to block and wake goroutines (not OS threads). Unlike the runtime mutex which blocks Ms, this semaphore blocks Gs.

```go
// src/runtime/sema.go, lines 1-18
// Semaphore implementation exposed to Go.
// Intended use is provide a sleep and wakeup
// primitive that can be used in the contended case
// of other synchronization primitives.
// Thus it targets the same goal as Linux's futex,
// but it has much simpler semantics.
//
// That is, don't think of these as semaphores.
// Think of them as a way to implement sleep and wakeup
// such that every sleep is paired with a single wakeup,
// even if, due to races, the wakeup happens before the sleep.
```

### The Hash Table: semTable

To avoid a single global lock, the semaphore system uses a hash table indexed by the address of the semaphore word:

```go
// src/runtime/sema.go, lines 48-58
// Prime to not correlate with any user patterns.
const semTabSize = 251

type semTable [semTabSize]struct {
    root semaRoot
    pad  [cpu.CacheLinePadSize - unsafe.Sizeof(semaRoot{})]byte
}

func (t *semTable) rootFor(addr *uint32) *semaRoot {
    return &t[(uintptr(unsafe.Pointer(addr))>>3)%semTabSize].root
}
```

- 251 buckets (prime, to avoid correlation with allocation patterns)
- Each bucket padded to a cache line to prevent false sharing
- Address shifted right by 3 before hashing (alignment)

### The Treap Data Structure

Each `semaRoot` contains a **treap** (tree + heap): a balanced binary search tree where:
- The **BST property** is on the semaphore address: left subtree has smaller addresses, right subtree has larger
- The **heap property** is on a random priority (`ticket`): parents have smaller tickets than children

```go
// src/runtime/sema.go, lines 30-44
// A semaRoot holds a balanced tree of sudog with distinct addresses (s.elem).
// Each of those sudog may in turn point (through s.waitlink) to a list
// of other sudogs waiting on the same address.
// The operations on the inner lists of sudogs with the same address
// are all O(1). The scanning of the top-level semaRoot list is O(log n),
// where n is the number of distinct addresses with goroutines blocked
// on them that hash to the given semaRoot.
type semaRoot struct {
    lock  mutex
    treap *sudog        // root of balanced tree of unique waiters.
    nwait atomic.Uint32 // Number of waiters. Read w/o the lock.
}
```

The two-level structure:
```
                treap (BST on address, heap on ticket)
                         ┌───────┐
                         │addr=A │
                         │ticket=3│
                         └───┬───┘
                        /         \
               ┌───────┐           ┌───────┐
               │addr=B │           │addr=C │
               │ticket=7│           │ticket=5│
               └───┬───┘           └───────┘
                   │
              waitlink list (same address B):
              sudog1 -> sudog2 -> sudog3
```

Within each treap node, goroutines waiting on the *same* address form a linked list via `waitlink`/`waittail`. Operations on this inner list are O(1). Finding the right address in the treap is O(log n).

### semacquire1: The Core Acquire

```go
// src/runtime/sema.go, lines 146-201
func semacquire1(addr *uint32, lifo bool, profile semaProfileFlags, skipframes int, reason waitReason) {
    gp := getg()
    if gp != gp.m.curg {
        throw("semacquire not on the G stack")
    }

    // Easy case.
    if cansemacquire(addr) {
        return
    }

    // Harder case:
    //  increment waiter count
    //  try cansemacquire one more time, return if succeeded
    //  enqueue itself as a waiter
    //  sleep
    //  (waiter descriptor is dequeued by signaler)
    s := acquireSudog()
    root := semtable.rootFor(addr)
    // ...
    for {
        lockWithRank(&root.lock, lockRankRoot)
        root.nwait.Add(1)
        // Check cansemacquire to avoid missed wakeup.
        if cansemacquire(addr) {
            root.nwait.Add(-1)
            unlock(&root.lock)
            break
        }
        // Any semrelease after the cansemacquire knows we're waiting
        // (we set nwait above), so go to sleep.
        root.queue(addr, s, lifo)
        goparkunlock(&root.lock, reason, traceBlockSync, 4+skipframes)
        if s.ticket != 0 || cansemacquire(addr) {
            break
        }
    }
    // ...
    releaseSudog(s)
}
```

The flow:
1. **Fast path**: Try `cansemacquire` (atomic decrement if > 0). If it succeeds, return immediately -- no locks needed.
2. **Slow path**: Acquire the semaRoot lock, increment `nwait`, try once more (to avoid missed wakeup), then enqueue and park via `goparkunlock`.
3. **On wakeup**: Check `s.ticket` (handoff mode) or retry `cansemacquire`.

### cansemacquire: The Atomic Fast Path

```go
// src/runtime/sema.go, lines 291-301
func cansemacquire(addr *uint32) bool {
    for {
        v := atomic.Load(addr)
        if v == 0 {
            return false
        }
        if atomic.Cas(addr, v, v-1) {
            return true
        }
    }
}
```

A simple CAS loop: atomically decrement the counter if it's positive. This is the uncontended fast path that avoids all locks.

### semrelease1: Release and Wake

```go
// src/runtime/sema.go, lines 207-289
func semrelease1(addr *uint32, handoff bool, skipframes int) {
    root := semtable.rootFor(addr)
    atomic.Xadd(addr, 1)

    // Easy case: no waiters?
    if root.nwait.Load() == 0 {
        return
    }

    // Harder case: search for a waiter and wake it.
    lockWithRank(&root.lock, lockRankRoot)
    if root.nwait.Load() == 0 {
        unlock(&root.lock)
        return
    }
    s, t0, tailtime := root.dequeue(addr)
    if s != nil {
        root.nwait.Add(-1)
    }
    unlock(&root.lock)
    if s != nil {
        // ...profiling...
        if handoff && cansemacquire(addr) {
            s.ticket = 1
        }
        readyWithTime(s, 5+skipframes)
        if s.ticket == 1 && getg().m.locks == 0 && getg() != getg().m.g0 {
            // Direct G handoff
            goyield()
        }
    }
}
```

Key steps:
1. Atomically increment the semaphore counter
2. Check `nwait` (without the lock) -- if zero, no waiters, return
3. Lock the root, dequeue a waiter from the treap, unlock
4. If `handoff` mode and we can re-acquire the semaphore, set `ticket = 1` (direct grant)
5. Mark the waiter as ready (`readyWithTime`)
6. In handoff mode, yield immediately via `goyield()` to let the waiter run

### Starvation Handling and Handoff Mode

The `handoff` parameter is critical for fairness. When `sync.Mutex` enters starvation mode (a goroutine has been waiting > 1ms), it calls `semrelease` with `handoff=true`.

```go
// src/runtime/sema.go, lines 260-287
        if handoff && cansemacquire(addr) {
            s.ticket = 1
        }
        readyWithTime(s, 5+skipframes)
        if s.ticket == 1 && getg().m.locks == 0 && getg() != getg().m.g0 {
            // Direct G handoff
            //
            // readyWithTime has added the waiter G as runnext in the
            // current P; we now call the scheduler so that we start running
            // the waiter G immediately.
            //
            // Note that waiter inherits our time slice: this is desirable
            // to avoid having a highly contended semaphore hog the P
            // indefinitely. goyield is like Gosched, but it emits a
            // "preempted" trace event instead and, more importantly, puts
            // the current G on the local runq instead of the global one.
            // We only do this in the starving regime (handoff=true), as in
            // the non-starving case it is possible for a different waiter
            // to acquire the semaphore while we are yielding/scheduling,
            // and this would be wasteful.
            goyield()
        }
```

In handoff mode:
1. The releaser atomically grabs the semaphore on behalf of the waiter (`s.ticket = 1`)
2. The waiter is placed in `runnext` of the current P
3. The releaser calls `goyield()` to immediately switch to the waiter
4. This prevents new goroutines from stealing the semaphore between release and acquire

### The queue Method: Treap Insertion

```go
// src/runtime/sema.go, lines 370-396
    // Add s as new leaf in tree of unique addrs.
    // The balanced tree is a treap using ticket as the random heap priority.
    // That is, it is a binary tree ordered according to the elem addresses,
    // but then among the space of possible binary trees respecting those
    // addresses, it is kept balanced on average by maintaining a heap ordering
    // on the ticket: s.ticket <= both s.prev.ticket and s.next.ticket.
    // https://en.wikipedia.org/wiki/Treap
    // https://faculty.washington.edu/aragon/pubs/rst89.pdf
    //
    // s.ticket compared with zero in couple of places, therefore set lowest bit.
    // It will not affect treap's quality noticeably.
    s.ticket = cheaprand() | 1
    s.parent = last
    *pt = s

    // Rotate up into tree according to ticket (priority).
    for s.parent != nil && s.parent.ticket > s.ticket {
        if s.parent.prev == s {
            root.rotateRight(s.parent)
        } else {
            if s.parent.next != s {
                panic("semaRoot queue")
            }
            root.rotateLeft(s.parent)
        }
    }
```

The treap maintains balance probabilistically:
1. Insert as a BST leaf (ordered by address)
2. Assign a random priority (`cheaprand() | 1`)
3. Rotate up while parent has higher priority
4. The `| 1` ensures ticket is never zero (zero has special meaning for handoff)

---

## 7. Reader-Writer Mutex (10 min)

### Overview

The runtime's `rwmutex` (in `rwmutex.go`) is used internally by the runtime (e.g., for protecting allocator state). It's a simplified version of `sync.RWMutex`.

```go
// src/runtime/rwmutex.go, lines 18-30
type rwmutex struct {
    rLock      mutex    // protects readers, readerPass, writer
    readers    muintptr // list of pending readers
    readerPass uint32   // number of pending readers to skip readers list

    wLock  mutex    // serializes writers
    writer muintptr // pending writer waiting for completing readers

    readerCount atomic.Int32 // number of pending readers
    readerWait  atomic.Int32 // number of departing readers

    readRank lockRank // semantic lock rank for read locking
}
```

### The rwmutexMaxReaders Trick

```go
// src/runtime/rwmutex.go, line 67
const rwmutexMaxReaders = 1 << 30
```

This constant is the key to the reader-writer protocol. `readerCount` serves dual purpose:
- When positive: number of active readers
- When negative: a writer is pending (readerCount was decremented by `rwmutexMaxReaders`)

### Read Lock (rlock)

```go
// src/runtime/rwmutex.go, lines 70-98
func (rw *rwmutex) rlock() {
    acquireLockRankAndM(rw.readRank)
    lockWithRankMayAcquire(&rw.rLock, getLockRank(&rw.rLock))

    if rw.readerCount.Add(1) < 0 {
        // A writer is pending. Park on the reader queue.
        systemstack(func() {
            lock(&rw.rLock)
            if rw.readerPass > 0 {
                // Writer finished.
                rw.readerPass -= 1
                unlock(&rw.rLock)
            } else {
                // Queue this reader to be woken by the writer.
                m := getg().m
                m.schedlink = rw.readers
                rw.readers.set(m)
                unlock(&rw.rLock)
                notesleep(&m.park)
                noteclear(&m.park)
            }
        })
    }
}
```

The flow:
1. Atomically increment `readerCount`
2. If result is positive: no writer pending, proceed (fast path)
3. If result is negative: a writer has subtracted `rwmutexMaxReaders`
   - Check `readerPass` -- if > 0, the writer already finished and we can proceed
   - Otherwise, add ourselves to the `readers` list and sleep via `notesleep`

### Write Lock (lock)

```go
// src/runtime/rwmutex.go, lines 121-140
func (rw *rwmutex) lock() {
    // Resolve competition with other writers and stick to our P.
    lock(&rw.wLock)
    m := getg().m
    // Announce that there is a pending writer.
    r := rw.readerCount.Add(-rwmutexMaxReaders) + rwmutexMaxReaders
    // Wait for any active readers to complete.
    lock(&rw.rLock)
    if r != 0 && rw.readerWait.Add(r) != 0 {
        // Wait for reader to wake us up.
        systemstack(func() {
            rw.writer.set(m)
            unlock(&rw.rLock)
            notesleep(&m.park)
            noteclear(&m.park)
        })
    } else {
        unlock(&rw.rLock)
    }
}
```

The protocol:
1. Acquire `wLock` to serialize writers
2. Subtract `rwmutexMaxReaders` from `readerCount` -- this makes it negative, signaling readers
3. The return value `r` tells us how many readers were active at that moment
4. Add `r` to `readerWait` -- as readers finish, they decrement `readerWait`
5. If `readerWait` becomes 0 (or was already 0), all readers are done
6. Otherwise, sleep until the last departing reader wakes us

### Read Unlock (runlock)

```go
// src/runtime/rwmutex.go, lines 101-118
func (rw *rwmutex) runlock() {
    if r := rw.readerCount.Add(-1); r < 0 {
        if r+1 == 0 || r+1 == -rwmutexMaxReaders {
            throw("runlock of unlocked rwmutex")
        }
        // A writer is pending.
        if rw.readerWait.Add(-1) == 0 {
            // The last reader unblocks the writer.
            lock(&rw.rLock)
            w := rw.writer.ptr()
            if w != nil {
                notewakeup(&w.park)
            }
            unlock(&rw.rLock)
        }
    }
    releaseLockRankAndM(rw.readRank)
}
```

When a reader unlocks:
1. Decrement `readerCount`
2. If result is negative, a writer is waiting
3. Decrement `readerWait` -- if we're the last departing reader (readerWait reaches 0), wake the writer

### Write Unlock

```go
// src/runtime/rwmutex.go, lines 143-164
func (rw *rwmutex) unlock() {
    // Announce to readers that there is no active writer.
    r := rw.readerCount.Add(rwmutexMaxReaders)
    if r >= rwmutexMaxReaders {
        throw("unlock of unlocked rwmutex")
    }
    // Unblock blocked readers.
    lock(&rw.rLock)
    for rw.readers.ptr() != nil {
        reader := rw.readers.ptr()
        rw.readers = reader.schedlink
        reader.schedlink.set(nil)
        notewakeup(&reader.park)
        r -= 1
    }
    // If r > 0, there are pending readers that aren't on the
    // queue. Tell them to skip waiting.
    rw.readerPass += uint32(r)
    unlock(&rw.rLock)
    // Allow other writers to proceed.
    unlock(&rw.wLock)
}
```

The writer unlock:
1. Add `rwmutexMaxReaders` back to `readerCount` (makes it positive again)
2. Wake all readers on the `readers` list via `notewakeup`
3. If `r > 0`, some readers arrived after the writer and already incremented `readerCount` but haven't been queued yet. Set `readerPass` so they skip the queue.
4. Release `wLock` to allow other writers

### The Elegance of the Design

The `rwmutexMaxReaders` trick encodes the "writer pending" state directly in the reader counter. This means:
- Readers in the common (no-writer) case do a single atomic add and proceed
- The writer's presence is communicated through the sign of `readerCount`
- No separate "writer waiting" flag is needed
- The maximum number of concurrent readers is 2^30 (about 1 billion), which is more than sufficient

---

## 8. Summary: The Synchronization Hierarchy

| Layer | Primitive | Blocks | Platform |
|-------|-----------|--------|----------|
| Hardware | CAS, LoadAcq, StoreRel | N/A | All |
| OS (Linux) | futexsleep / futexwakeup | OS thread | Linux |
| OS (macOS) | semasleep / semawakeup | OS thread | Darwin |
| Runtime | notesleep / notewakeup | OS thread (one-shot) | All (via above) |
| Runtime | lock2 / unlock2 (lock_spinbit.go) | OS thread | All |
| Runtime | semacquire1 / semrelease1 (sema.go) | Goroutine | All |
| User | sync.Mutex, sync.RWMutex | Goroutine | All |

**Key insight**: The runtime has two parallel stacks of synchronization -- one for blocking OS threads (used by the scheduler itself) and one for blocking goroutines (used by Go code). They use the same OS primitives at the bottom but diverge at the level above.

---

## Discussion Questions

1. Why does the runtime need both `lock_futex.go` and `lock_sema.go`? Why not use one approach everywhere?

2. In the `sema.go` treap, why use a treap instead of a simpler structure like a hash table of linked lists?

3. The runtime mutex uses an 8-bit atomic swap for the fast path. Why is this faster than a full word-width CAS?

4. In the rwmutex, why does the writer subtract `rwmutexMaxReaders` from `readerCount` instead of using a separate "writer waiting" boolean?

5. Consider `semrelease1` with `handoff=true`. Why does it call `goyield()` instead of `Gosched()`? What's the difference?

## Further Reading

- Ulrich Drepper, "Futexes Are Tricky" (2011) -- the classic futex reference
- Mullender and Cox, "Semaphores in Plan 9" -- cited in `sema.go`'s header comment
- [sync.Mutex source](https://pkg.go.dev/sync#Mutex) -- see how it uses `runtime_SemacquireMutex`
- Aragon and Seidel, "Randomized Search Trees" (1989) -- the treap paper cited in `sema.go`
