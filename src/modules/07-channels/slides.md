# Module 7: Channels and Select

## Slides

---

## Slide 1: Channels -- Go's Communication Primitive

"Do not communicate by sharing memory; instead, share memory by communicating."

- Channels implement Hoare's CSP (Communicating Sequential Processes)
- A channel is: **mutex + circular buffer + two wait queues**
- Compile-time type safety; runtime synchronization
- Key operations: `make(chan T, n)`, `ch <- v`, `v := <-ch`, `close(ch)`

---

## Slide 2: Channel Invariants

```go
// src/runtime/chan.go, lines 9-18
// Invariants:
//  At least one of c.sendq and c.recvq is empty,
//  except for the case of an unbuffered channel with a single goroutine
//  blocked on it for both sending and receiving using a select statement.
//
// For buffered channels, also:
//  c.qcount > 0 implies that c.recvq is empty.
//  c.qcount < c.dataqsiz implies that c.sendq is empty.
```

- If data is buffered, no one should be waiting to receive
- If buffer has space, no one should be waiting to send
- OS parallel: monitor invariants in classical synchronization

---

## Slide 3: The hchan Struct

```go
// src/runtime/chan.go, lines 34-55
type hchan struct {
    qcount   uint           // total data in the queue
    dataqsiz uint           // size of the circular queue
    buf      unsafe.Pointer // points to an array of dataqsiz elements
    elemsize uint16
    closed   uint32
    elemtype *_type // element type
    sendx    uint   // send index
    recvx    uint   // receive index
    recvq    waitq  // list of recv waiters
    sendq    waitq  // list of send waiters
    lock     mutex
}
```

---

## Slide 4: hchan Memory Layout

```
+------------------+
| hchan struct     |
|  qcount          |
|  dataqsiz        |
|  buf        --------> [slot0][slot1][slot2]...[slotN-1]
|  elemsize        |         ^recvx       ^sendx
|  closed          |
|  sendx, recvx    |    Circular buffer:
|  recvq -----+    |    sendx advances on send, wraps at dataqsiz
|  sendq --+  |    |    recvx advances on recv, wraps at dataqsiz
|  lock    |  |    |
+---------+--+----+
          |  |
          v  v
       waitq: doubly-linked list of sudogs
```

---

## Slide 5: waitq and sudog

```go
// src/runtime/chan.go, lines 57-60
type waitq struct {
    first *sudog
    last  *sudog
}
```

```go
// src/runtime/runtime2.go, lines 406-448
type sudog struct {
    g    *g
    next *sudog
    prev *sudog
    elem maybeTraceablePtr  // data element (may point to stack)
    isSelect bool           // participating in select?
    success  bool           // woken by data or close?
    waitlink *sudog         // g.waiting list
    c        maybeTraceableChan
}
```

**Why sudog?** A goroutine in `select` waits on multiple channels -- needs one proxy per channel.

---

## Slide 6: makechan -- Three Allocation Strategies

```go
// src/runtime/chan.go, lines 96-111
switch {
case mem == 0:
    // Unbuffered or zero-size elements
    c = (*hchan)(mallocgc(hchanSize, nil, true))
case !elem.Pointers():
    // No pointers: single allocation for hchan + buffer
    c = (*hchan)(mallocgc(hchanSize+mem, nil, true))
    c.buf = add(unsafe.Pointer(c), hchanSize)
default:
    // Contains pointers: separate allocations
    c = new(hchan)
    c.buf = mallocgc(mem, elem, true)
}
```

GC-aware allocation: pointer-free buffers use a single allocation.

---

## Slide 7: chansend Overview

```
chansend(c, ep, block, callerpc)
    |
    v
nil channel? --> gopark forever
    |
    v
Fast path (no lock): !block && !closed && full? --> return false
    |
    v
lock(c.lock)
    |
    v
closed? --> panic
    |
    v
receiver waiting? --> direct send, bypass buffer
    |
    v
buffer space? --> circular buffer enqueue
    |
    v
block: acquireSudog, enqueue on sendq, gopark
```

---

## Slide 8: The Non-Blocking Fast Path

```go
// src/runtime/chan.go, lines 197-214
// Fast path: check for failed non-blocking operation
// without acquiring the lock.
if !block && c.closed == 0 && full(c) {
    return false
}
```

**Linearizability argument:**
- Read `c.closed` (== 0), then read `full(c)` (== true)
- Closed channel cannot transition from "ready" to "not ready"
- Therefore there exists a moment when channel was both open and full
- Safe to report "send cannot proceed"

**No lock needed!** Used by select with default case.

---

## Slide 9: Direct Send -- Bypassing the Buffer

```go
// src/runtime/chan.go, lines 229-233
if sg := c.recvq.dequeue(); sg != nil {
    // Found a waiting receiver. We pass the value we want to send
    // directly to the receiver, bypassing the channel buffer (if any).
    send(c, sg, ep, func() { unlock(&c.lock) }, 3)
    return true
}
```

- Even on buffered channels, data goes directly sender -> receiver
- One `memmove` instead of two (sender->buffer, buffer->receiver)
- The `send` function copies `ep` to `sg.elem` and calls `goready(sg.g)`

---

## Slide 10: Buffered Send -- Circular Buffer Enqueue

```go
// src/runtime/chan.go, lines 236-249
if c.qcount < c.dataqsiz {
    qp := chanbuf(c, c.sendx)
    typedmemmove(c.elemtype, qp, ep)
    c.sendx++
    if c.sendx == c.dataqsiz {
        c.sendx = 0  // wrap around
    }
    c.qcount++
    unlock(&c.lock)
    return true
}
```

Classic bounded buffer: copy data, advance index, wrap at capacity.

---

## Slide 11: Blocking Send -- gopark

```go
// src/runtime/chan.go, lines 257-283
gp := getg()
mysg := acquireSudog()
mysg.elem.set(ep)       // receiver will copy from here
mysg.g = gp
mysg.isSelect = false
mysg.c.set(c)
gp.waiting = mysg
c.sendq.enqueue(mysg)
gp.parkingOnChan.Store(true)
gopark(chanparkcommit, unsafe.Pointer(&c.lock), ...)
```

- `acquireSudog`: get a sudog from the per-P pool
- `mysg.elem` points to data on sender's stack
- `gopark` atomically releases `c.lock` and suspends goroutine
- Eventually a receiver calls `goready` to wake this goroutine

---

## Slide 12: chanrecv -- Symmetric to chansend

```go
// src/runtime/chan.go, lines 600-609
} else {
    // Just found waiting sender with not closed.
    if sg := c.sendq.dequeue(); sg != nil {
        // Found a waiting sender. If buffer is size 0, receive value
        // directly from sender. Otherwise, receive from head of queue
        // and add sender's value to the tail of the queue (both map to
        // the same buffer slot because the queue is full).
        recv(c, sg, ep, func() { unlock(&c.lock) }, 3)
        return true, true
    }
}
```

Same four phases: nil check, fast path, direct/buffered, block.

Key difference: receiving from a closed, empty channel returns `(true, false)` and the zero value.

---

## Slide 13: closechan -- Wake All Waiters

```go
// src/runtime/chan.go, lines 414-486
func closechan(c *hchan) {
    lock(&c.lock)
    c.closed = 1

    var glist gList
    // release all readers
    for {
        sg := c.recvq.dequeue()
        if sg == nil { break }
        sg.success = false
        glist.push(sg.g)
    }
    // release all writers (they will panic)
    for {
        sg := c.sendq.dequeue()
        if sg == nil { break }
        sg.success = false
        glist.push(sg.g)
    }
    unlock(&c.lock)
    // Ready all Gs AFTER dropping the lock
    for !glist.empty() {
        gp := glist.pop()
        goready(gp, 3)
    }
}
```

---

## Slide 14: closechan Design Points

- **Double close panics**: checked under lock
- **Receivers get zero values**: `typedmemclr` + `success = false`
- **Senders will panic**: `success = false` -> "send on closed channel"
- **Lock ordering**: `goready` called *after* releasing `c.lock`
  - Prevents deadlock with stack shrinking
- **OS parallel**: analogous to `pthread_cond_broadcast`

---

## Slide 15: The select Statement

```go
select {
case v := <-ch1:    // receive case
    use(v)
case ch2 <- x:     // send case
    sent()
default:            // non-blocking
    nothing()
}
```

Compiler optimizations before `selectgo` is reached:
- `select {}` -> `block()` (park forever)
- 1 case + default -> non-blocking send/recv
- 1 case, no default -> blocking send/recv
- Multi-case -> full `selectgo` algorithm

---

## Slide 16: The scase Struct

```go
// src/runtime/select.go, lines 20-23
type scase struct {
    c    *hchan         // chan
    elem unsafe.Pointer // data element
}
```

- Direction is implicit: sends come first (`nsends`), then receives (`nrecvs`)
- Cases and orders live on the goroutine's stack (no heap allocation)
- `order0` array is split into two halves: `pollorder` and `lockorder`

---

## Slide 17: selectgo Signature

```go
// src/runtime/select.go, lines 122
func selectgo(cas0 *scase, order0 *uint16, pc0 *uintptr,
              nsends, nrecvs int, block bool) (int, bool)
```

Returns:
- `int`: index of chosen case (-1 if default)
- `bool`: for receive cases, whether a value was received

The algorithm has **three passes** through the cases.

---

## Slide 18: Step 1 -- Randomize Poll Order

```go
// src/runtime/select.go, lines 167-195
norder := 0
for i := range scases {
    cas := &scases[i]
    if cas.c == nil {
        continue  // skip nil channels
    }
    j := cheaprandn(uint32(norder + 1))
    pollorder[norder] = pollorder[j]
    pollorder[j] = uint16(i)
    norder++
}
```

- Fisher-Yates shuffle of case indices
- **Fairness**: without this, earlier cases always have priority
- Nil channel cases are excluded (disabling a select case)

---

## Slide 19: Step 2 -- Sort Lock Order by Address

```go
// src/runtime/select.go, lines 206-240
// sort the cases by Hchan address to get the locking order.
// simple heap sort, to guarantee n log n time and constant stack footprint.
for i := range lockorder {
    j := i
    c := scases[pollorder[i]].c
    for j > 0 && scases[lockorder[(j-1)/2]].c.sortkey() < c.sortkey() {
        k := (j - 1) / 2
        lockorder[j] = lockorder[k]
        j = k
    }
    lockorder[j] = pollorder[i]
}
```

- Heap sort: O(n log n), constant extra stack
- `sortkey()` returns `uintptr(unsafe.Pointer(c))` -- the channel address
- **Deadlock prevention**: global ordering prevents ABBA deadlocks

---

## Slide 20: Pass 1 -- Try All Cases (Fast Path)

```go
// src/runtime/select.go, lines 264-301
sellock(scases, lockorder)        // lock ALL channels
for _, casei := range pollorder { // iterate in RANDOM order
    c = scases[casei].c
    if casei >= nsends {          // receive case
        if c.sendq has waiter  -> goto recv
        if c.qcount > 0       -> goto bufrecv
        if c.closed            -> goto rclose
    } else {                      // send case
        if c.closed            -> goto sclose (panic)
        if c.recvq has waiter  -> goto send
        if buffer has space    -> goto bufsend
    }
}
// No case ready
if !block { return -1 }          // default case
```

---

## Slide 21: Pass 2 -- Enqueue and Park

```go
// src/runtime/select.go, lines 309-351
nextp = &gp.waiting
for _, casei := range lockorder {
    sg := acquireSudog()
    sg.g = gp
    sg.isSelect = true
    sg.elem.set(cas.elem)
    sg.c.set(c)
    *nextp = sg
    nextp = &sg.waitlink
    if casei < nsends {
        c.sendq.enqueue(sg)
    } else {
        c.recvq.enqueue(sg)
    }
}
gopark(selparkcommit, nil, ...)
```

- One sudog per case, enqueued on each channel
- `isSelect = true`: waker must CAS `g.selectDone` to win
- Sudogs chained via `waitlink` for cleanup

---

## Slide 22: Pass 3 -- Dequeue Losers, Return Winner

```go
// src/runtime/select.go, lines 354-401
sellock(scases, lockorder)    // re-lock all channels
sg = (*sudog)(gp.param)      // the winning sudog

for _, casei := range lockorder {
    if sg == sglist {
        // This is the winner -- already dequeued by waker
        casi = int(casei)
        cas = &scases[casei]
    } else {
        // Loser: dequeue from channel's wait queue
        c.sendq.dequeueSudoG(sglist) // or recvq
    }
    releaseSudog(sglist)
    sglist = sglist.waitlink
}
```

---

## Slide 23: sellock -- Lock Ordering in Practice

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

- Locks channels in address-sorted order
- **Skips duplicates**: same channel in multiple cases -> lock only once
- `selunlock` iterates in reverse, same duplicate detection

---

## Slide 24: Select -- The Full Picture

```
selectgo(cases)
    |
    +-- 1. Shuffle pollorder (fairness)
    |       Sort lockorder by address (deadlock prevention)
    |
    +-- 2. Lock all channels (in lock order)
    |
    +-- 3. Pass 1: scan in poll order -> if ready, do it and return
    |
    +-- 4. Pass 2: enqueue sudog on every channel, gopark
    |       ... goroutine sleeps ...
    |       ... some channel operation wakes us ...
    |
    +-- 5. Pass 3: re-lock all, find winner, dequeue losers, return
```

---

## Slide 25: OS Concepts Recap

| OS Concept | Channel Implementation |
|-----------|----------------------|
| Bounded buffer | Circular buffer with `sendx`/`recvx` |
| Condition variable | `gopark`/`goready` on wait queues |
| Broadcast | `closechan` wakes all waiters |
| Monitor | `hchan.lock` protects all state |
| Lock ordering | select sorts by channel address |
| Fairness | select randomizes poll order |
| Linearizability | Non-blocking fast path argument |
| Wait queue | `waitq` linked list of `sudog` nodes |
