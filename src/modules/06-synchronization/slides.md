# Module 6: Synchronization Primitives
## Slides

---

## Slide 1: Module Overview

**Synchronization Primitives in the Go Runtime**

From hardware atomics to `sync.Mutex` -- how does Go make goroutines wait?

```
sync.Mutex / sync.RWMutex
        │
   sema.go (semacquire1 / semrelease1)
        │
   lock_spinbit.go (lock2 / unlock2)
        │
   lock_futex.go (Linux) / lock_sema.go (macOS)
        │
   Hardware atomics (CAS, LoadAcq, StoreRel)
```

---

## Slide 2: Hardware Atomics

**Compare-and-Swap (CAS)**
```go
// Atomically: if *addr == old { *addr = new; return true }
//             else { return false }
atomic.Cas(addr, old, new)
```

**Load-Acquire / Store-Release**
```go
// No subsequent reads/writes reordered before this load
h := atomic.LoadAcq(&pp.runqhead)

// No preceding reads/writes reordered after this store
atomic.StoreRel(&pp.runqtail, t+n)
```

These provide **ordering guarantees** across threads, not just atomicity.

---

## Slide 3: Why Two Kinds of Blocking?

The Go runtime needs to block two different things:

| | OS Threads (M) | Goroutines (G) |
|---|---|---|
| Blocked by | `semasleep` / futex | `gopark` |
| Woken by | `semawakeup` / futex | `goready` |
| Used for | Scheduler locks, notes | `sync.Mutex`, channels |
| Cost | Expensive (syscall) | Cheap (context switch) |

The **runtime** blocks threads. **User code** blocks goroutines.

---

## Slide 4: Futex -- The Linux Foundation

**futex** = "fast userspace mutex"

```
futexsleep(addr, val, timeout):
    "Sleep if *addr == val"
    (atomic check-and-sleep in kernel)

futexwakeup(addr, count):
    "Wake up to count threads sleeping on addr"
```

The key property: **uncontended case never enters the kernel.**

---

## Slide 5: notesleep / notewakeup (Linux)

One-shot notification for OS threads:

```go
// src/runtime/lock_futex.go, lines 22-33
func noteclear(n *note) {
    n.key = 0
}

func notewakeup(n *note) {
    old := atomic.Xchg(key32(&n.key), 1)
    if old != 0 {
        throw("notewakeup - double wakeup")
    }
    futexwakeup(key32(&n.key), 1)
}
```

```go
// lines 35-52
func notesleep(n *note) {
    // ...
    for atomic.Load(key32(&n.key)) == 0 {
        gp.m.blocked = true
        futexsleep(key32(&n.key), 0, ns)
        gp.m.blocked = false
    }
}
```

- `key = 0`: not signaled. `key = 1`: signaled.
- Double wakeup is a fatal error.

---

## Slide 6: semasleep / semawakeup (Linux)

Thread parking for the runtime mutex:

```go
// src/runtime/lock_futex.go, lines 139-163
func semasleep(ns int64) int32 {
    mp := getg().m
    for v := atomic.Xadd(&mp.waitsema, -1); ; v = atomic.Load(&mp.waitsema) {
        if int32(v) >= 0 {
            return 0
        }
        futexsleep(&mp.waitsema, v, ns)
        // ...
    }
}

func semawakeup(mp *m) {
    v := atomic.Xadd(&mp.waitsema, 1)
    if v == 0 {
        futexwakeup(&mp.waitsema, 1)
    }
}
```

Counting semaphore using `mp.waitsema` as the counter.

---

## Slide 7: macOS: No Futexes

macOS uses OS semaphores instead. Same API, different implementation.

```go
// src/runtime/lock_sema.go, lines 23-44
func notewakeup(n *note) {
    for {
        v = atomic.Loaduintptr(&n.key)
        if atomic.Casuintptr(&n.key, v, locked) {
            break
        }
    }
    switch {
    case v == 0:        // No one waiting
    case v == locked:   // Double wakeup!
        throw("notewakeup - double wakeup")
    default:            // v is an M pointer
        semawakeup((*m)(unsafe.Pointer(v)))
    }
}
```

The `note.key` encodes three states:
- `0` = clear, no waiter
- `locked` (1) = signaled
- Other = pointer to waiting M

---

## Slide 8: macOS notesleep

```go
// src/runtime/lock_sema.go, lines 46-72
func notesleep(n *note) {
    gp := getg()
    semacreate(gp.m)
    if !atomic.Casuintptr(&n.key, 0, uintptr(unsafe.Pointer(gp.m))) {
        // Must be locked (got wakeup already).
        if n.key != locked {
            throw("notesleep - waitm out of sync")
        }
        return
    }
    // Queued. Sleep.
    gp.m.blocked = true
    semasleep(-1)
    gp.m.blocked = false
}
```

The CAS atomically registers the waiter. If it fails, the note was already signaled.

---

## Slide 9: The Runtime Mutex -- State Layout

```go
// src/runtime/lock_spinbit.go, lines 53-71
const (
    mutexLocked      = 0x001
    mutexSleeping    = 0x002
    mutexSpinning    = 0x100
    mutexStackLocked = 0x200
)
```

```
┌──────────────────────────────┬────┬────┬───────┬─────────┬────────┐
│  M pointer (upper bits)      │ 9  │ 8  │ 7..2  │    1    │   0    │
│  (head of waiter stack)      │stkL│spin│unused │sleeping │locked  │
└──────────────────────────────┴────┴────┴───────┴─────────┴────────┘
```

Everything packed into a single `uintptr` -- flags + waiter stack pointer.

---

## Slide 10: Runtime Mutex Fast Path

```go
// src/runtime/lock_spinbit.go, lines 155-171
func lock2(l *mutex) {
    gp := getg()
    gp.m.locks++

    k8 := key8(&l.key)

    // Speculative grab for lock.
    v8 := atomic.Xchg8(k8, mutexLocked)
    if v8&mutexLocked == 0 {
        // Got it!
        return
    }
    // ...slow path...
}
```

**One atomic instruction** for the uncontended case.

8-bit swap: only touches the low byte, preserves waiter stack in upper bits.

---

## Slide 11: Runtime Mutex Slow Path

```go
// src/runtime/lock_spinbit.go, lines 182-257
    spin := 0
    if numCPUStartup > 1 {
        spin = mutexActiveSpinCount  // 4
    }

    for i := 0; ; i++ {
        // 1. Try to acquire (CAS or Xchg8)
        if v&mutexLocked == 0 { /* try acquire */ }

        // 2. Try to claim the spin bit
        if !weSpin && v&mutexSpinning == 0 {
            atomic.Casuintptr(&l.key, v, v|mutexSpinning)
        }

        // 3. Spin (only if we have the spin bit)
        if weSpin {
            procyield(30)  // ~30 PAUSE instructions
            osyield()      // then yield to OS
        }

        // 4. Sleep (push onto waiter stack)
        gp.m.mWaitList.next = mutexWaitListHead(v)
        semasleep(-1)
    }
```

Only **one** thread spins at a time (the spin bit). Others go to sleep.

---

## Slide 12: The Waiter Stack

```
l.key: ┌───────────────────┬──flags──┐
       │   ptr to M3       │ L S ... │
       └────────┬──────────┴─────────┘
                │
       M3.mWaitList.next ──► M2
       M2.mWaitList.next ──► M1
       M1.mWaitList.next ──► nil

       Stack: M3 -> M2 -> M1 (LIFO)
```

Waiters form a LIFO stack linked through `mWaitList.next`. The stack head pointer is packed into the upper bits of `l.key`.

---

## Slide 13: sema.go -- The Goroutine Semaphore

```go
// src/runtime/sema.go, lines 5-18
// Semaphore implementation exposed to Go.
// Intended use is provide a sleep and wakeup
// primitive that can be used in the contended case
// of other synchronization primitives.
//
// That is, don't think of these as semaphores.
// Think of them as a way to implement sleep and wakeup
// such that every sleep is paired with a single wakeup,
// even if, due to races, the wakeup happens before the sleep.
```

This powers `sync.Mutex`, `sync.RWMutex`, `sync.WaitGroup`.

---

## Slide 14: The semTable Hash

```go
// src/runtime/sema.go, lines 48-58
const semTabSize = 251  // prime!

type semTable [semTabSize]struct {
    root semaRoot
    pad  [cpu.CacheLinePadSize - unsafe.Sizeof(semaRoot{})]byte
}

func (t *semTable) rootFor(addr *uint32) *semaRoot {
    return &t[(uintptr(unsafe.Pointer(addr))>>3)%semTabSize].root
}
```

- 251 buckets (prime avoids correlation)
- Cache-line padded (prevents false sharing)
- Hash by semaphore address

---

## Slide 15: The Treap Data Structure

```go
// src/runtime/sema.go, lines 30-44
type semaRoot struct {
    lock  mutex
    treap *sudog        // root of balanced tree of unique waiters.
    nwait atomic.Uint32 // Number of waiters. Read w/o the lock.
}
```

```
Treap: BST on address, min-heap on random priority

              addr=0x100 (ticket=3)
             /                      \
    addr=0x080 (ticket=7)    addr=0x200 (ticket=5)
         │
    waitlink: G1 -> G2 -> G3  (same address, linked list)
```

O(log n) lookup by address. O(1) operations on same-address waiters.

---

## Slide 16: semacquire1 -- Acquire

```go
// src/runtime/sema.go, lines 146-201
func semacquire1(addr *uint32, lifo bool, ...) {
    // Easy case: try atomic decrement
    if cansemacquire(addr) {
        return
    }

    // Hard case: enqueue and sleep
    s := acquireSudog()
    root := semtable.rootFor(addr)
    for {
        lockWithRank(&root.lock, lockRankRoot)
        root.nwait.Add(1)
        if cansemacquire(addr) {   // recheck!
            root.nwait.Add(-1)
            unlock(&root.lock)
            break
        }
        root.queue(addr, s, lifo)
        goparkunlock(&root.lock, reason, ...)
        if s.ticket != 0 || cansemacquire(addr) {
            break
        }
    }
    releaseSudog(s)
}
```

---

## Slide 17: cansemacquire -- The Fast Path

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

CAS loop: decrement if positive. No locks needed.

When `sync.Mutex` is uncontended, this is the only code that runs.

---

## Slide 18: semrelease1 -- Release

```go
// src/runtime/sema.go, lines 207-289
func semrelease1(addr *uint32, handoff bool, ...) {
    root := semtable.rootFor(addr)
    atomic.Xadd(addr, 1)

    if root.nwait.Load() == 0 {   // no waiters? done
        return
    }

    lockWithRank(&root.lock, lockRankRoot)
    s, t0, tailtime := root.dequeue(addr)
    if s != nil {
        root.nwait.Add(-1)
    }
    unlock(&root.lock)

    if s != nil {
        if handoff && cansemacquire(addr) {
            s.ticket = 1           // direct grant
        }
        readyWithTime(s, 5+skipframes)
        if s.ticket == 1 {
            goyield()              // switch to waiter now
        }
    }
}
```

---

## Slide 19: Starvation and Handoff Mode

**Problem**: Under high contention, a newly arriving goroutine can grab the mutex before a waiting goroutine wakes up.

**Solution**: `sync.Mutex` switches to starvation mode after 1ms of waiting, then uses `handoff=true`:

```go
// src/runtime/sema.go, lines 260-287
        if handoff && cansemacquire(addr) {
            s.ticket = 1
        }
        readyWithTime(s, 5+skipframes)
        if s.ticket == 1 && getg().m.locks == 0 {
            // Direct G handoff
            // readyWithTime has added the waiter G as runnext in the
            // current P; we now call the scheduler so that we start running
            // the waiter G immediately.
            goyield()
        }
```

1. Releaser grabs the semaphore for the waiter (`ticket = 1`)
2. Waiter goes into `runnext`
3. Releaser yields -- waiter runs immediately

---

## Slide 20: Treap Insertion

```go
// src/runtime/sema.go, lines 371-396
    // The balanced tree is a treap using ticket as the random heap priority.
    s.ticket = cheaprand() | 1
    s.parent = last
    *pt = s

    // Rotate up into tree according to ticket (priority).
    for s.parent != nil && s.parent.ticket > s.ticket {
        if s.parent.prev == s {
            root.rotateRight(s.parent)
        } else {
            root.rotateLeft(s.parent)
        }
    }
```

- BST order by address (binary search for the right semaphore)
- Heap order by random priority (keeps tree balanced on average)
- `| 1` ensures non-zero (zero = handoff ticket)

---

## Slide 21: Reader-Writer Mutex

```go
// src/runtime/rwmutex.go, lines 18-30
type rwmutex struct {
    rLock      mutex    // protects readers, readerPass, writer
    readers    muintptr // list of pending readers
    readerPass uint32   // number of pending readers to skip

    wLock  mutex    // serializes writers
    writer muintptr // pending writer

    readerCount atomic.Int32 // active readers (or negative if writer pending)
    readerWait  atomic.Int32 // readers the writer is waiting for
}
```

---

## Slide 22: The rwmutexMaxReaders Trick

```go
// src/runtime/rwmutex.go, line 67
const rwmutexMaxReaders = 1 << 30
```

`readerCount` encodes two things:
- **Positive**: number of active readers. Fast path -- just atomic add.
- **Negative**: writer subtracted `rwmutexMaxReaders`. Readers know to wait.

```
No writer:    readerCount = 3     (3 readers)
Writer arrives: readerCount = 3 - 2^30 = -1073741821
                                  (negative = writer pending)
Actual readers: readerCount + rwmutexMaxReaders = 3
```

---

## Slide 23: Read Lock Path

```go
// src/runtime/rwmutex.go, lines 70-98
func (rw *rwmutex) rlock() {
    if rw.readerCount.Add(1) < 0 {
        // A writer is pending. Park on the reader queue.
        systemstack(func() {
            lock(&rw.rLock)
            if rw.readerPass > 0 {
                rw.readerPass -= 1    // writer already done
                unlock(&rw.rLock)
            } else {
                m := getg().m
                m.schedlink = rw.readers
                rw.readers.set(m)
                unlock(&rw.rLock)
                notesleep(&m.park)    // sleep until writer unlocks
                noteclear(&m.park)
            }
        })
    }
}
```

**Fast path** (no writer): single `atomic.Add`, return. No locks.

---

## Slide 24: Write Lock Path

```go
// src/runtime/rwmutex.go, lines 121-140
func (rw *rwmutex) lock() {
    lock(&rw.wLock)                                    // serialize writers
    r := rw.readerCount.Add(-rwmutexMaxReaders) + rwmutexMaxReaders
    // r = number of active readers at this moment
    lock(&rw.rLock)
    if r != 0 && rw.readerWait.Add(r) != 0 {
        // Wait for active readers to finish
        rw.writer.set(m)
        unlock(&rw.rLock)
        notesleep(&m.park)
        noteclear(&m.park)
    } else {
        unlock(&rw.rLock)
    }
}
```

1. Lock `wLock` (one writer at a time)
2. Subtract `rwmutexMaxReaders` (signals readers)
3. Count active readers (`r`)
4. Wait for them to depart (last reader calls `notewakeup`)

---

## Slide 25: Key Takeaways

| Primitive | What it blocks | Key technique |
|-----------|---------------|---------------|
| futex (Linux) | OS thread | Kernel check-and-sleep |
| OS semaphore (macOS) | OS thread | CAS + semaphore |
| Runtime mutex | OS thread | 8-bit swap fast path + spin bit |
| sema.go | Goroutine | Treap + handoff mode |
| rwmutex | OS thread | `readerCount +/- rwmutexMaxReaders` |

**Design principles:**
- **Fast path must be fast**: single atomic op, no locks
- **Spin briefly before sleeping**: CPU cycles are cheaper than context switches
- **Prevent starvation**: handoff mode, tail-spinning, readerPass
- **Layer wisely**: each level adds capability without redundancy
