# Module 7: Channels and Select (60 min)

## Background: Message Passing and the CSP Tradition

Concurrent programs must coordinate access to shared state, and the history of
systems programming offers two fundamental paradigms for doing so. The first,
**shared memory with synchronization**, gives threads direct access to common
data structures and uses locks, condition variables, and atomic operations to
prevent races. This is the model of pthreads, Java's `synchronized` blocks, and
the mutexes we studied earlier in this course. The second paradigm, **message
passing**, structures concurrent programs as independent sequential processes
that communicate by sending and receiving messages over explicit channels. Rather
than protecting shared data with locks, message-passing programs avoid sharing
altogether: data moves between processes by copying (or transferring ownership),
and synchronization is implicit in the send/receive handshake. The distinction
is not absolute -- as we will see, Go's channel implementation itself uses a
mutex internally -- but it reflects a deep difference in how programmers reason
about concurrency. The shared-memory style asks "how do I protect this data?"
while the message-passing style asks "how do I route this data?"

The theoretical foundations of message passing trace to two landmark papers from
the 1970s. Carl Hewitt's **Actor model** (1973) proposed that computation
consists of autonomous actors that communicate by sending asynchronous messages
to named recipients; each actor has a mailbox, processes messages one at a time,
and can create new actors or send messages in response. Tony Hoare's
**Communicating Sequential Processes** (CSP, 1978) took a different approach:
processes communicate through unnamed, synchronous channels, and a send blocks
until a matching receive occurs (and vice versa). Where the Actor model
emphasizes identity (messages are sent *to* a specific actor), CSP emphasizes
the channel as a first-class connective tissue between anonymous processes. The
Actor model's asynchronous, identity-based messaging influenced Erlang, Akka,
and the "let it crash" school of fault-tolerant systems. CSP's synchronous,
channel-based model influenced occam, Limbo, and -- most prominently today --
Go. Both models avoid shared mutable state, but they differ in buffering
semantics, naming, and the role of the communication medium itself.

These ideas have been realized in strikingly different ways across programming
languages and systems. Erlang processes communicate through asynchronous
**mailboxes** with selective receive, giving each process an unbounded message
queue and pattern matching to pluck out messages of interest -- a direct
descendant of the Actor model. Rust's standard library provides `mpsc`
(multi-producer, single-consumer) channels that transfer ownership of values,
leveraging the type system to guarantee that sent data cannot be accessed by the
sender afterward; the `crossbeam` crate extends this with multi-consumer
channels and a `select!` macro. Kotlin offers channels within its coroutine
framework, with configurable buffering policies (rendezvous, buffered,
conflated, unlimited) that let programmers tune the coupling between producer
and consumer. Clojure's `core.async` brings CSP-style channels to the JVM with
`go` blocks that compile to state machines, much as Go's goroutines are
multiplexed onto OS threads. Even Unix pipes, dating to Doug McIlroy's 1964
proposal and Ken Thompson's 1973 implementation, embody the same principle:
composing programs as sequential stages connected by byte streams, with
back-pressure arising naturally from the pipe buffer's bounded capacity. Go's
channels sit squarely in this lineage, offering typed, optionally-buffered
channels as first-class values, with goroutines as lightweight processes.

A key operation in any channel-based system is **multiplexing**: waiting on
multiple channels simultaneously and proceeding with whichever is ready first.
This problem has deep roots in operating systems. Unix's `select()` system call
(4.2BSD, 1983) let a process block until any file descriptor in a set became
ready for I/O -- solving the problem of a server needing to handle multiple
clients without dedicating a thread to each one. `poll()` improved the interface,
and Linux's `epoll` (2002) scaled it to hundreds of thousands of descriptors.
Go's `select` statement is the channel-level analogue: it blocks until one of
several channel operations can proceed, then executes that case. But Go's
`select` adds two refinements absent from Unix I/O multiplexing. First,
**randomized fairness**: when multiple cases are ready simultaneously, `select`
chooses one uniformly at random, preventing starvation. Second, **deadlock-safe
locking**: the `select` implementation acquires locks on all involved channels
in address order to prevent lock-ordering deadlocks, a technique borrowed from
database concurrency control. The combination of randomization and careful lock
management makes `select` both fair and safe, properties that are surprisingly
difficult to achieve together.

In this module, we open up Go's channel and select implementation to see how
these ideas are realized in practice. The runtime's `hchan` struct is a
mutex-protected circular buffer with embedded wait queues -- a bounded
producer-consumer buffer, much like the ones studied in OS textbooks, but with
several clever optimizations such as direct sender-to-receiver copying that
bypasses the buffer entirely. The `select` implementation is a carefully
orchestrated multi-pass algorithm that shuffles cases for fairness, sorts them
for deadlock freedom, and uses per-goroutine "pseudo-g" (`sudog`) structures to
allow a single goroutine to wait on many channels at once. Understanding this
machinery connects the high-level elegance of CSP to the low-level realities of
locks, memory layout, and scheduler integration.

## Overview

Channels are Go's primary communication primitive, implementing Hoare's
Communicating Sequential Processes (CSP) model. CSP is a formal model of
concurrent computation where independent processes communicate exclusively
through message passing, not shared memory. In CSP, sending and receiving are
synchronization points — a sender blocks until a receiver is ready (and vice
versa for unbuffered channels). Go's channels implement this model, though
buffered channels relax the strict synchronous coupling.

Under the hood, a channel is a mutex-protected circular buffer with embedded wait queues. The `select`
statement multiplexes across multiple channels using a carefully designed
algorithm that prevents deadlocks and ensures fairness.

This module examines the runtime implementation of channels and select in detail,
studying the data structures, synchronization invariants, and the multi-pass
select algorithm.

---

## 1. Channel Invariants

The channel implementation maintains a key structural invariant, documented at
the top of `chan.go`:

```go
// src/runtime/chan.go, lines 9-18

// Invariants:
//  At least one of c.sendq and c.recvq is empty,
//  except for the case of an unbuffered channel with a single goroutine
//  blocked on it for both sending and receiving using a select statement,
//  in which case the length of c.sendq and c.recvq is limited only by the
//  size of the select statement.
//
// For buffered channels, also:
//  c.qcount > 0 implies that c.recvq is empty.
//  c.qcount < c.dataqsiz implies that c.sendq is empty.
```

These invariants encode a fundamental property: if there is data in the buffer,
no goroutine should be blocked waiting to receive; if the buffer is not full, no
goroutine should be blocked waiting to send. The only exception is select
statements on unbuffered channels, where a single goroutine might appear on both
queues simultaneously.

**OS connection:** These invariants are analogous to the monitor invariants in
classical synchronization -- they define the legal states of the shared data
structure and constrain when threads (goroutines) may block.

---

## 2. The hchan Struct: Channel Internals

Every channel in Go is represented at runtime by an `hchan` struct:

```go
// src/runtime/chan.go, lines 34-55

type hchan struct {
    qcount   uint           // total data in the queue
    dataqsiz uint           // size of the circular queue
    buf      unsafe.Pointer // points to an array of dataqsiz elements
    elemsize uint16
    closed   uint32
    timer    *timer // timer feeding this chan
    elemtype *_type // element type
    sendx    uint   // send index
    recvx    uint   // receive index
    recvq    waitq  // list of recv waiters
    sendq    waitq  // list of send waiters
    bubble   *synctestBubble

    // lock protects all fields in hchan, as well as several
    // fields in sudogs blocked on this channel.
    //
    // Do not change another G's status while holding this lock
    // (in particular, do not ready a G), as this can deadlock
    // with stack shrinking.
    lock mutex
}
```

### Field-by-field analysis

| Field        | Purpose |
|-------------|---------|
| `qcount`    | Number of elements currently in the buffer. |
| `dataqsiz`  | Capacity of the buffer (0 for unbuffered channels). |
| `buf`       | Pointer to the circular buffer array. |
| `elemsize`  | Size of each element in bytes. |
| `closed`    | Non-zero if the channel has been closed. |
| `elemtype`  | Type descriptor for elements (used by GC and `typedmemmove`). |
| `sendx`     | Next index for writing into the buffer (producer cursor). |
| `recvx`     | Next index for reading from the buffer (consumer cursor). |
| `recvq`     | Doubly-linked list of goroutines waiting to receive. |
| `sendq`     | Doubly-linked list of goroutines waiting to send. |
| `lock`      | Mutex protecting all mutable fields. |

The `sendx` and `recvx` indices implement a standard circular buffer: they
advance through the buffer array and wrap to 0 when they reach `dataqsiz`. The
channel is full when `qcount == dataqsiz` and empty when `qcount == 0`.

**Important design note:** The comment on `lock` warns against readying a
goroutine while holding the channel lock, because `goready` can trigger stack
shrinking, and stack shrinking also needs to acquire channel locks -- creating a
potential deadlock cycle. This is a concrete example of lock ordering constraints
in the runtime.

---

## 3. The waitq and sudog Structs

### waitq

The wait queue is a simple doubly-linked list:

```go
// src/runtime/chan.go, lines 57-60

type waitq struct {
    first *sudog
    last  *sudog
}
```

### sudog (pseudo-g)

A `sudog` represents a goroutine waiting on a synchronization object. The name
stands for "pseudo-g" -- it is a proxy for a `g` (goroutine) in a wait list:

```go
// src/runtime/runtime2.go, lines 396-448

// sudog (pseudo-g) represents a g in a wait list, such as for sending/receiving
// on a channel.
//
// sudog is necessary because the g <-> synchronization object relation
// is many-to-many. A g can be on many wait lists, so there may be
// many sudogs for one g; and many gs may be waiting on the same
// synchronization object, so there may be many sudogs for one object.
//
// sudogs are allocated from a special pool. Use acquireSudog and
// releaseSudog to allocate and free them.
type sudog struct {
    // The following fields are protected by the hchan.lock of the
    // channel this sudog is blocking on. shrinkstack depends on
    // this for sudogs involved in channel ops.

    g *g

    next *sudog
    prev *sudog

    elem maybeTraceablePtr // data element (may point to stack)

    // The following fields are never accessed concurrently.
    // For channels, waitlink is only accessed by g.
    // For semaphores, all fields (including the ones above)
    // are only accessed when holding a semaRoot lock.

    acquiretime int64
    releasetime int64
    ticket      uint32

    // isSelect indicates g is participating in a select, so
    // g.selectDone must be CAS'd to win the wake-up race.
    isSelect bool

    // success indicates whether communication over channel c
    // succeeded. It is true if the goroutine was awoken because a
    // value was delivered over channel c, and false if awoken
    // because c was closed.
    success bool

    waiters uint16

    parent   *sudog             // semaRoot binary tree
    waitlink *sudog             // g.waiting list or semaRoot
    waittail *sudog             // semaRoot
    c        maybeTraceableChan // channel
}
```

### Why sudogs exist

A goroutine blocked in a `select` statement may be waiting on *multiple*
channels simultaneously. A single `g` struct cannot appear in multiple wait
queues, so the runtime allocates a separate `sudog` for each channel case. This
is the "many-to-many" relationship the comment describes.

Key fields for channel operations:

- **`g`**: Points back to the goroutine this sudog represents.
- **`next`/`prev`**: Links for the channel's `waitq` linked list.
- **`elem`**: Pointer to the data element being sent or received. May point into the goroutine's stack.
- **`isSelect`**: If true, waking this sudog requires a CAS on `g.selectDone` to handle the race where multiple channels become ready simultaneously.
- **`success`**: Indicates whether the channel operation completed normally (`true`) or the channel was closed (`false`).
- **`waitlink`**: Used by `select` to chain all of a goroutine's sudogs together for cleanup.

**OS connection:** The sudog pool is analogous to how OS kernels pre-allocate
wait queue entries rather than embedding them in thread structures, allowing a
thread to wait on multiple events simultaneously (cf. Linux `poll`/`epoll`
implementation).

---

## 4. makechan: Channel Creation

The `makechan` function is called by the compiler when it encounters `make(chan T, size)`:

```go
// src/runtime/chan.go, lines 75-125

func makechan(t *chantype, size int) *hchan {
    elem := t.Elem

    // compiler checks this but be safe.
    if elem.Size_ >= 1<<16 {
        throw("makechan: invalid channel element type")
    }
    if hchanSize%maxAlign != 0 || elem.Align_ > maxAlign {
        throw("makechan: bad alignment")
    }

    mem, overflow := math.MulUintptr(elem.Size_, uintptr(size))
    if overflow || mem > maxAlloc-hchanSize || size < 0 {
        panic(plainError("makechan: size out of range"))
    }

    // Hchan does not contain pointers interesting for GC when elements
    // stored in buf do not contain pointers.
    // buf points into the same allocation, elemtype is persistent.
    // SudoG's are referenced from their owning thread so they can't be collected.
    var c *hchan
    switch {
    case mem == 0:
        // Queue or element size is zero.
        c = (*hchan)(mallocgc(hchanSize, nil, true))
        // Race detector uses this location for synchronization.
        c.buf = c.raceaddr()
    case !elem.Pointers():
        // Elements do not contain pointers.
        // Allocate hchan and buf in one call.
        c = (*hchan)(mallocgc(hchanSize+mem, nil, true))
        c.buf = add(unsafe.Pointer(c), hchanSize)
    default:
        // Elements contain pointers.
        c = new(hchan)
        c.buf = mallocgc(mem, elem, true)
    }

    c.elemsize = uint16(elem.Size_)
    c.elemtype = elem
    c.dataqsiz = uint(size)
    lockInit(&c.lock, lockRankHchan)

    if debugChan {
        print("makechan: chan=", c, "; elemsize=", elem.Size_, "; dataqsiz=", size, "\n")
    }
    return c
}
```

### Three allocation strategies

1. **Unbuffered or zero-size elements (`mem == 0`)**: Only the `hchan` struct
   itself is allocated. No buffer is needed.

2. **Pointer-free elements (`!elem.Pointers()`)**: The `hchan` struct and the
   buffer are allocated in a *single* `mallocgc` call. The buffer is placed
   immediately after the struct in memory (`c.buf = add(unsafe.Pointer(c),
   hchanSize)`). This is an optimization: the GC doesn't need to scan the
   buffer, so a single non-pointer allocation suffices.

3. **Pointer-containing elements (default)**: Two separate allocations are
   needed. The buffer must be allocated with type information (`mallocgc(mem,
   elem, true)`) so the GC can scan it for pointers.

**OS connection:** This is a nice example of how a runtime can cooperate with
its garbage collector by choosing different allocation strategies based on type
information -- something impossible with a C-style malloc that has no type
awareness.

---

## 5. chansend: Sending on a Channel

The `chansend` function is the heart of the send operation (`c <- x`):

```go
// src/runtime/chan.go, lines 176-183

func chansend(c *hchan, ep unsafe.Pointer, block bool, callerpc uintptr) bool {
    if c == nil {
        if !block {
            return false
        }
        gopark(nil, nil, waitReasonChanSendNilChan, traceBlockForever, 2)
        throw("unreachable")
    }
```

Sending on a nil channel parks the goroutine forever. This is by design: it
allows `select` statements to disable cases by setting channel variables to nil.

### Phase 1: Non-blocking fast path (no lock)

```go
// src/runtime/chan.go, lines 197-214

    // Fast path: check for failed non-blocking operation without acquiring the lock.
    //
    // After observing that the channel is not closed, we observe that the channel is
    // not ready for sending. Each of these observations is a single word-sized read
    // (first c.closed and second full()).
    // Because a closed channel cannot transition from 'ready for sending' to
    // 'not ready for sending', even if the channel is closed between the two
    // observations, they imply a moment between the two when the channel was both
    // not yet closed and not ready for sending. We behave as if we observed the
    // channel at that moment, and report that the send cannot proceed.
    if !block && c.closed == 0 && full(c) {
        return false
    }
```

This is a lock-free fast path for non-blocking sends (used by `select` with a
`default` case). The comment explains a subtle linearizability argument: even
though the two reads (`c.closed` and `full(c)`) are not atomic together, the
monotonicity of `closed` (once closed, always closed) guarantees correctness.

### Phase 2: Direct send to waiting receiver

```go
// src/runtime/chan.go, lines 222-233

    lock(&c.lock)

    if c.closed != 0 {
        unlock(&c.lock)
        panic(plainError("send on closed channel"))
    }

    if sg := c.recvq.dequeue(); sg != nil {
        // Found a waiting receiver. We pass the value we want to send
        // directly to the receiver, bypassing the channel buffer (if any).
        send(c, sg, ep, func() { unlock(&c.lock) }, 3)
        return true
    }
```

If a receiver is already waiting, the send copies the value *directly* to the
receiver's stack, bypassing the channel buffer entirely. This is an important
optimization: even on a buffered channel, if a receiver is blocked, the data
goes straight to the receiver without first going into the buffer and then back
out.

### Phase 3: Buffered send

```go
// src/runtime/chan.go, lines 236-249

    if c.qcount < c.dataqsiz {
        // Space is available in the channel buffer. Enqueue the element to send.
        qp := chanbuf(c, c.sendx)
        typedmemmove(c.elemtype, qp, ep)
        c.sendx++
        if c.sendx == c.dataqsiz {
            c.sendx = 0
        }
        c.qcount++
        unlock(&c.lock)
        return true
    }
```

Standard circular buffer enqueue: copy the element to the `sendx` slot, advance
the index (wrapping at `dataqsiz`), increment the count.

### Phase 4: Blocking

```go
// src/runtime/chan.go, lines 257-283

    // Block on the channel. Some receiver will complete our operation for us.
    gp := getg()
    mysg := acquireSudog()
    mysg.releasetime = 0
    if t0 != 0 {
        mysg.releasetime = -1
    }
    // No stack splits between assigning elem and enqueuing mysg
    // on gp.waiting where copystack can find it.
    mysg.elem.set(ep)
    mysg.waitlink = nil
    mysg.g = gp
    mysg.isSelect = false
    mysg.c.set(c)
    gp.waiting = mysg
    gp.param = nil
    c.sendq.enqueue(mysg)
    // Signal to anyone trying to shrink our stack that we're about
    // to park on a channel. The window between when this G's status
    // changes and when we set gp.activeStackChans is not safe for
    // stack shrinking.
    gp.parkingOnChan.Store(true)
    reason := waitReasonChanSend
    if c.bubble != nil {
        reason = waitReasonSynctestChanSend
    }
    gopark(chanparkcommit, unsafe.Pointer(&c.lock), reason, traceBlockChanSend, 2)
```

When the buffer is full (or the channel is unbuffered and no receiver is
waiting), the goroutine parks itself:

1. Acquires a `sudog` from the pool.
2. Records the element pointer (`mysg.elem`) so the eventual receiver can copy the data.
3. Enqueues the `sudog` on `c.sendq`.
4. Calls `gopark` to suspend the goroutine, releasing the channel lock atomically.

When a receiver eventually arrives, it will dequeue this `sudog`, copy the data
from `mysg.elem`, and call `goready` to wake the sender.

**Critical subtlety:** The comment about "No stack splits" is important. Between
setting `mysg.elem` (which points into the sender's stack) and enqueuing the
sudog where `copystack` can find it, a stack copy would invalidate the pointer.
The runtime prevents this by ensuring no function calls that could trigger stack
growth occur in this window.

---

## 6. chanrecv: Receiving from a Channel

The receive operation (`<-c`) is implemented by `chanrecv`, which is symmetric
to `chansend`:

```go
// src/runtime/chan.go, lines 518-524

// chanrecv receives on channel c and writes the received data to ep.
// ep may be nil, in which case received data is ignored.
// If block == false and no elements are available, returns (false, false).
// Otherwise, if c is closed, zeros *ep and returns (true, false).
// Otherwise, fills in *ep with an element and returns (true, true).
// A non-nil ep must point to the heap or the caller's stack.
func chanrecv(c *hchan, ep unsafe.Pointer, block bool) (selected, received bool) {
```

### Receive phases

The function follows the same structure as `chansend`:

1. **Nil channel check** (line 532-538): Receiving from a nil channel parks forever.

2. **Non-blocking fast path** (lines 548-579): Check `empty(c)` without the lock.
   If the channel is closed and empty, return the zero value.

3. **Direct receive from waiting sender** (lines 600-609):
   ```go
   if sg := c.sendq.dequeue(); sg != nil {
       // Found a waiting sender. If buffer is size 0, receive value
       // directly from sender. Otherwise, receive from head of queue
       // and add sender's value to the tail of the queue (both map to
       // the same buffer slot because the queue is full).
       recv(c, sg, ep, func() { unlock(&c.lock) }, 3)
       return true, true
   }
   ```
   For unbuffered channels, data is copied directly from sender to receiver.
   For buffered channels (which must be full if a sender is blocked), the
   receiver takes from the buffer head, and the sender's value fills the buffer
   tail.

4. **Buffered receive** (lines 612-629): Standard circular buffer dequeue.
   ```go
   if c.qcount > 0 {
       // Receive directly from queue
       qp := chanbuf(c, c.recvx)
       if ep != nil {
           typedmemmove(c.elemtype, ep, qp)
       }
       typedmemclr(c.elemtype, qp)
       c.recvx++
       if c.recvx == c.dataqsiz {
           c.recvx = 0
       }
       c.qcount--
       unlock(&c.lock)
       return true, true
   }
   ```

5. **Blocking** (lines 636-685): Acquire a sudog, enqueue on `c.recvq`, park.

---

## 7. closechan: Closing a Channel

Closing a channel wakes up *all* waiters:

```go
// src/runtime/chan.go, lines 414-486

func closechan(c *hchan) {
    if c == nil {
        panic(plainError("close of nil channel"))
    }

    lock(&c.lock)
    if c.closed != 0 {
        unlock(&c.lock)
        panic(plainError("close of closed channel"))
    }

    c.closed = 1

    var glist gList

    // release all readers
    for {
        sg := c.recvq.dequeue()
        if sg == nil {
            break
        }
        if sg.elem.get() != nil {
            typedmemclr(c.elemtype, sg.elem.get())
            sg.elem.set(nil)
        }
        gp := sg.g
        gp.param = unsafe.Pointer(sg)
        sg.success = false
        glist.push(gp)
    }

    // release all writers (they will panic)
    for {
        sg := c.sendq.dequeue()
        if sg == nil {
            break
        }
        sg.elem.set(nil)
        gp := sg.g
        gp.param = unsafe.Pointer(sg)
        sg.success = false
        glist.push(gp)
    }
    unlock(&c.lock)

    // Ready all Gs now that we've dropped the channel lock.
    for !glist.empty() {
        gp := glist.pop()
        gp.schedlink = 0
        goready(gp, 3)
    }
}
```

### Key points

1. **Double close panics**: The runtime checks `c.closed != 0` under the lock.

2. **Receivers get zero values**: The `typedmemclr` call zeros out the receive
   buffer. The `success = false` flag tells receivers they woke due to close,
   not data delivery.

3. **Senders will panic**: Woken senders see `success == false`, check
   `c.closed`, and panic with "send on closed channel."

4. **Lock ordering**: All goroutines are collected into `glist` *before*
   releasing the lock, then readied *after* the lock is released. This follows
   the constraint from the `hchan.lock` comment: "do not ready a G while holding
   this lock."

**OS connection:** This wake-all pattern is analogous to `pthread_cond_broadcast`
or the Linux `FUTEX_WAKE` with a large wake count. Closing a channel is
effectively a broadcast signal to all waiters.

---

## 8. The select Statement Implementation

### The scase struct

Each case in a `select` statement is described by an `scase`:

```go
// src/runtime/select.go, lines 20-23

type scase struct {
    c    *hchan         // chan
    elem unsafe.Pointer // data element
}
```

This is remarkably simple: just a channel pointer and a pointer to the data
element. The direction (send vs. receive) is implicit in the position: send
cases come first, followed by receive cases.

### Compiler optimizations

Before `selectgo` is called, the compiler has already optimized away simple
cases:
- `select {}` (no cases): compiled to a direct `block()` call (parks forever).
- `select` with one case + default: compiled to a non-blocking `chansend`/`chanrecv`.
- `select` with one case (no default): compiled to a blocking `chansend`/`chanrecv`.

Only multi-case selects (or degenerate cases where channels have been niled out)
reach `selectgo`.

### selectgo: The Multi-Pass Algorithm

```go
// src/runtime/select.go, lines 107-122

// selectgo implements the select statement.
//
// cas0 points to an array of type [ncases]scase, and order0 points to
// an array of type [2*ncases]uint16 where ncases must be <= 65536.
// Both reside on the goroutine's stack (regardless of any escaping in
// selectgo).
//
// selectgo returns the index of the chosen scase, which matches the
// ordinal position of its respective select{recv,send,default} call.
// Also, if the chosen scase was a receive operation, it reports whether
// a value was received.
func selectgo(cas0 *scase, order0 *uint16, pc0 *uintptr, nsends, nrecvs int,
              block bool) (int, bool) {
```

The function takes a flat array of cases and an array that is split into two
halves: `pollorder` (randomized iteration order) and `lockorder` (sorted by
channel address for lock acquisition).

#### Step 1: Generate randomized poll order

```go
// src/runtime/select.go, lines 167-197

    // generate permuted order
    norder := 0
    for i := range scases {
        cas := &scases[i]

        // Omit cases without channels from the poll and lock orders.
        if cas.c == nil {
            cas.elem = nil // allow GC
            continue
        }

        // ...

        j := cheaprandn(uint32(norder + 1))
        pollorder[norder] = pollorder[j]
        pollorder[j] = uint16(i)
        norder++
    }
```

Cases with nil channels are skipped. The remaining cases are shuffled using a
Fisher-Yates shuffle (`cheaprandn`). This randomization is critical for
**fairness**: without it, earlier cases in the source code would always have
priority, potentially starving later cases.

#### Step 2: Sort lock order by channel address

```go
// src/runtime/select.go, lines 206-240

    // sort the cases by Hchan address to get the locking order.
    // simple heap sort, to guarantee n log n time and constant stack footprint.
    for i := range lockorder {
        j := i
        // Start with the pollorder to permute cases on the same channel.
        c := scases[pollorder[i]].c
        for j > 0 && scases[lockorder[(j-1)/2]].c.sortkey() < c.sortkey() {
            k := (j - 1) / 2
            lockorder[j] = lockorder[k]
            j = k
        }
        lockorder[j] = pollorder[i]
    }
```

A heap sort orders cases by channel address. This establishes a **global lock
ordering** that prevents deadlocks when two goroutines running select statements
try to lock overlapping sets of channels.

**OS connection:** This is a textbook application of lock ordering to prevent
deadlock -- the same technique used in database systems (two-phase locking with
ordered resources) and OS kernels (e.g., Linux's lock ordering for inodes).

#### Pass 1: Try all cases without blocking (fast path)

```go
// src/runtime/select.go, lines 264-301

    // pass 1 - look for something already waiting
    var casi int
    var cas *scase
    var caseSuccess bool
    for _, casei := range pollorder {
        casi = int(casei)
        cas = &scases[casi]
        c = cas.c

        if casi >= nsends {
            sg = c.sendq.dequeue()
            if sg != nil {
                goto recv
            }
            if c.qcount > 0 {
                goto bufrecv
            }
            if c.closed != 0 {
                goto rclose
            }
        } else {
            if c.closed != 0 {
                goto sclose
            }
            sg = c.recvq.dequeue()
            if sg != nil {
                goto send
            }
            if c.qcount < c.dataqsiz {
                goto bufsend
            }
        }
    }

    if !block {
        selunlock(scases, lockorder)
        casi = -1
        goto retc
    }
```

After locking all channels (in lock order), the function iterates through cases
in the *randomized* poll order. For each case:

- **Receive cases** (`casi >= nsends`): Check for a waiting sender, buffered
  data, or a closed channel.
- **Send cases**: Check for close (panic), a waiting receiver, or buffer space.

If any case can proceed immediately, the function jumps to the appropriate
handler, which performs the operation, unlocks all channels, and returns.

If no case is ready and there's a default clause (`!block`), unlock and return -1.

#### Pass 2: Enqueue on all channels and park

```go
// src/runtime/select.go, lines 309-351

    // pass 2 - enqueue on all chans
    nextp = &gp.waiting
    for _, casei := range lockorder {
        casi = int(casei)
        cas = &scases[casi]
        c = cas.c
        sg := acquireSudog()
        sg.g = gp
        sg.isSelect = true
        // No stack splits between assigning elem and enqueuing
        // sg on gp.waiting where copystack can find it.
        sg.elem.set(cas.elem)
        sg.c.set(c)
        // Construct waiting list in lock order.
        *nextp = sg
        nextp = &sg.waitlink

        if casi < nsends {
            c.sendq.enqueue(sg)
        } else {
            c.recvq.enqueue(sg)
        }
    }

    // wait for someone to wake us up
    gp.param = nil
    gp.parkingOnChan.Store(true)
    gopark(selparkcommit, nil, waitReason, traceBlockSelect, 1)
```

If no case is immediately ready, the goroutine enqueues a sudog on *every*
channel and parks itself. Key details:

- Each sudog has `isSelect = true`, signaling that waking this goroutine requires
  a CAS on `g.selectDone`.
- The sudogs are chained together via `waitlink` in lock order, so pass 3 can
  efficiently clean up.

#### Pass 3: Dequeue from losers and return winner

```go
// src/runtime/select.go, lines 354-401

    sellock(scases, lockorder)

    gp.selectDone.Store(0)
    sg = (*sudog)(gp.param)
    gp.param = nil

    // pass 3 - dequeue from unsuccessful chans
    // otherwise they stack up on quiet channels
    // record the successful case, if any.
    casi = -1
    cas = nil
    caseSuccess = false
    sglist = gp.waiting
    // Clear all elem before unlinking from gp.waiting.
    for sg1 := gp.waiting; sg1 != nil; sg1 = sg1.waitlink {
        sg1.isSelect = false
        sg1.elem.set(nil)
        sg1.c.set(nil)
    }
    gp.waiting = nil

    for _, casei := range lockorder {
        k = &scases[casei]
        if sg == sglist {
            // sg has already been dequeued by the G that woke us up.
            casi = int(casei)
            cas = k
            caseSuccess = sglist.success
        } else {
            c = k.c
            if int(casei) < nsends {
                c.sendq.dequeueSudoG(sglist)
            } else {
                c.recvq.dequeueSudoG(sglist)
            }
        }
        sgnext = sglist.waitlink
        sglist.waitlink = nil
        releaseSudog(sglist)
        sglist = sgnext
    }
```

When the goroutine wakes up:

1. Re-lock all channels (in lock order).
2. Find which sudog caused the wakeup (`gp.param`).
3. Walk through all sudogs: the winning sudog has already been dequeued by the
   channel that woke us; dequeue the losers from their respective channels.
4. Release all sudogs back to the pool.

### sellock and selunlock: Careful lock management

```go
// src/runtime/select.go, lines 34-43

func sellock(scases []scase, lockorder []uint16) {
    var c *hchan
    for _, o := range lockorder {
        c0 := scases[o].c
        if c0 != c {
            c = c0
            lock(&c.lock)
        }
    }
}
```

The `sellock` function locks channels in sorted order, skipping duplicates (when
the same channel appears in multiple cases). `selunlock` unlocks in reverse
order, also skipping duplicates.

---

## 9. Summary: Channel Operation Decision Tree

### Send (`c <- x`)

```
c == nil?  --> park forever
           |
           v
[lock channel]
           |
c.closed?  --> panic("send on closed channel")
           |
           v
receiver waiting?  --> copy directly to receiver, wake receiver
           |
           v
buffer space?  --> enqueue in buffer
           |
           v
[block: park until receiver arrives]
```

### Receive (`<-c`)

```
c == nil?  --> park forever
           |
           v
[lock channel]
           |
c.closed && empty?  --> return zero value
           |
           v
sender waiting?  --> receive from sender/buffer, wake sender
           |
           v
buffer data?  --> dequeue from buffer
           |
           v
[block: park until sender arrives]
```

---

## 10. OS Concepts Illustrated

| OS Concept | Channel Implementation |
|-----------|----------------------|
| Bounded buffer / producer-consumer | `hchan` circular buffer with `sendx`/`recvx` |
| Condition variable wait/signal | `gopark`/`goready` on send/recv queues |
| Broadcast signal | `closechan` waking all waiters |
| Monitor | The channel lock protecting all shared state |
| Lock ordering (deadlock prevention) | `select` sorting channels by address |
| Fairness | `select` randomizing poll order |
| Wait-free fast path | Non-blocking check before acquiring lock |

---

## Discussion Questions

1. Why does `chansend` copy data directly to a waiting receiver instead of
   putting it in the buffer first? What performance benefit does this provide?

2. The non-blocking fast path in `chansend` reads `c.closed` and `full(c)`
   without holding the lock. Why is this safe? Under what conditions could this
   produce a "stale" answer, and why is a stale answer acceptable here?

3. In `closechan`, goroutines are collected into a list before being readied.
   Why can't we call `goready` while holding the channel lock?

4. Why does `select` use randomization for the poll order but a deterministic
   sort for the lock order? What would go wrong if we used the same order for
   both?

5. Consider a select statement with 100 cases, all on the same channel. How
   does the lock ordering logic handle this? (Hint: look at `sellock`'s
   duplicate detection.)

---

## Further Reading

- Source files:
  - `/Users/tchajed/sw/go/src/runtime/chan.go` -- channel implementation
  - `/Users/tchajed/sw/go/src/runtime/select.go` -- select implementation
  - `/Users/tchajed/sw/go/src/runtime/runtime2.go` -- sudog struct (line 406)
- Hoare, C.A.R. "Communicating Sequential Processes." *Communications of the ACM*, 1978.
- The Go Memory Model: https://go.dev/ref/mem
