# Module 7: Channels and Select (hchan, chansend/chanrecv, selectgo)

## Comprehension Check

### Question 1 (Code Reading)
Study the `hchan` struct from `runtime/chan.go`:

```go
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

	lock mutex
}
```

(a) How can you determine from these fields whether a channel is unbuffered?
(b) Why does the struct need both `sendx`/`recvx` and `qcount` if the buffer is a circular queue?

<details><summary>Answer</summary>

**(a)** An unbuffered channel has `dataqsiz == 0` (and `buf` may be nil or point to a zero-size allocation). An unbuffered `make(chan int)` sets `dataqsiz = 0`. You can also verify by checking `buf == nil` for channels with non-zero element size, though the canonical check is `dataqsiz`.

**(b)** `sendx` and `recvx` track the current write and read positions in the circular buffer, respectively. `qcount` tracks the actual number of elements in the buffer.

While in theory you could derive the count from `sendx` and `recvx` (using modular arithmetic: `(sendx - recvx) % dataqsiz`), maintaining a separate `qcount` provides:
- **Faster full/empty checks:** `qcount == 0` (empty) and `qcount == dataqsiz` (full) are O(1) comparisons without modular arithmetic.
- **Correct handling of the ambiguous case:** In a circular buffer, `sendx == recvx` is ambiguous -- it could mean the buffer is completely full or completely empty. A separate count resolves this ambiguity.
- **The `len()` builtin:** `len(ch)` returns `qcount` directly.

</details>

---

### Question 2 (Code Reading)
Consider this fast-path check in `chansend`:

```go
if !block && c.closed == 0 && full(c) {
	return false
}
```

The comment above this code explains that the reads of `c.closed` and `full(c)` are not atomic with respect to each other. Why is this safe for the non-blocking case? Under what condition would this be unsafe?

<details><summary>Answer</summary>

**Why it is safe for non-blocking sends:**

The function is checking whether a non-blocking send should immediately fail. The key insight from the comment is:

> "Because a closed channel cannot transition from 'ready for sending' to 'not ready for sending', even if the channel is closed between the two observations, they imply a moment between the two when the channel was both not yet closed and not ready for sending."

In other words, channel state transitions are monotonic in relevant ways:
- A closed channel stays closed (channels cannot be reopened).
- A full channel can only become "not full" (by someone receiving), never "more full."

So if we observe `closed == 0` then `full == true`, there existed a moment in time when both were true -- the channel was open and full, so the send legitimately cannot proceed. The non-blocking send returning `false` is a valid linearization.

**When it would be unsafe:** This reasoning does NOT apply to blocking sends. A blocking send must actually acquire the lock and re-check conditions because:
- The channel might become available (a receiver arrives) between the check and the park.
- A lost wakeup could occur if the sender parks without being properly registered in `sendq`.
- For blocking operations, we need the strong guarantee that the operation either succeeds or the goroutine is properly enqueued to be woken later.

</details>

---

### Question 3 (Short Answer)
Explain what happens during a channel send on an **unbuffered** channel when there is already a receiver waiting in `recvq`. Describe the data flow -- where is the value copied?

<details><summary>Answer</summary>

When a sender sends on an unbuffered channel and a receiver is waiting:

1. The sender acquires `c.lock`.
2. It dequeues a `sudog` from `c.recvq` (the waiting receiver).
3. It calls `send(c, sg, ep, ...)` which calls `sendDirect(t, sg, ep)`.
4. **The value is copied directly from the sender's stack to the receiver's stack** -- it bypasses the channel buffer entirely. The `sudog` contains a pointer to the receiver's memory location (`sg.elem`), and `sendDirect` uses `memmove` to copy the data.
5. The receiver goroutine is made runnable via `goready(gp)`, typically placing it in the waking P's `runnext`.
6. The sender releases `c.lock` and continues execution.

This **direct copy** is an important optimization: for unbuffered channels (and buffered channels with a waiting receiver), the data goes directly from sender to receiver without touching the circular buffer. This reduces the operation from two copies (sender->buffer, buffer->receiver) to one copy (sender->receiver).

Note: `sendDirect` writes to another goroutine's stack, which is normally forbidden. It is safe here because the receiver is blocked (in `_Gwaiting`) and cannot be running or have its stack moved by the GC during this operation.

</details>

---

### Question 4 (What Would Happen If...)
What would happen if `selectgo` did not acquire channel locks in a consistent global order (the "lock order" phase)? Describe a specific deadlock scenario.

<details><summary>Answer</summary>

**Deadlock scenario with inconsistent lock ordering:**

Consider two goroutines executing select statements:

```go
// Goroutine 1
select {
case ch_A <- 1:
case <-ch_B:
}

// Goroutine 2
select {
case ch_B <- 2:
case <-ch_A:
}
```

Without consistent lock ordering:
1. Goroutine 1 acquires `ch_A.lock`, then tries to acquire `ch_B.lock`.
2. Goroutine 2 acquires `ch_B.lock`, then tries to acquire `ch_A.lock`.
3. **Deadlock:** Each goroutine holds one lock and waits for the other.

**How `selectgo` prevents this:** The function sorts channels by their memory address (pointer value) to establish a total order. Both goroutines will lock channels in the same order (say, ch_A then ch_B, if `&ch_A < &ch_B`). This makes circular wait impossible.

This is the classic **lock ordering** solution to deadlock prevention, applied to the dynamic set of channels in a select statement. The `lockorder` array in `selectgo` is sorted by `hchan` pointer address before any locks are acquired.

</details>

---

### Question 5 (Short Answer)
Describe the three passes that `selectgo` performs and explain why each is necessary.

<details><summary>Answer</summary>

`selectgo` performs three major phases:

**Pass 1 -- Poll (fast path):**
Iterate through all cases in a random order (`pollorder`) and check if any operation can proceed immediately without blocking:
- For sends: Is there a waiting receiver, or is the buffer not full?
- For receives: Is there a waiting sender, or is the buffer not empty?
- For default: Always ready.

If any case is ready, execute it and return. **This avoids the overhead of enqueueing/dequeueing the goroutine when an operation is immediately available.** Randomization ensures fairness -- no case is systematically favored.

**Pass 2 -- Enqueue:**
If no case was ready in Pass 1, the goroutine must block. It creates a `sudog` for each case and enqueues itself on the `sendq` or `recvq` of each channel. Then it calls `gopark` to sleep.

**This is necessary because the goroutine needs to be woken when ANY of the channels becomes ready.** The goroutine is simultaneously waiting on all channels.

**Pass 3 -- Dequeue (after wakeup):**
When the goroutine is woken (because one channel operation completed), it must remove its `sudog` from ALL other channels' wait queues. It re-acquires all channel locks (in lock order) and dequeues itself from every channel except the one that triggered the wakeup.

**This is necessary to prevent the goroutine from being woken multiple times.** If it stayed in multiple wait queues, a second channel becoming ready would try to wake an already-running goroutine, causing corruption.

</details>

---

### Question 6 (Short Answer)
Why does `selectgo` use a random permutation (`pollorder`) rather than checking cases in source-code order?

<details><summary>Answer</summary>

Random ordering prevents **starvation** and **priority inversion** among select cases.

If cases were always checked in source-code order, the first case would always be preferred when multiple cases are simultaneously ready. This would effectively create a priority system based on the order cases are written, leading to:

1. **Starvation:** Later cases would rarely be selected if earlier cases are frequently ready. For example, in a fan-in pattern merging two channels, the first channel would dominate.

2. **Subtle ordering dependencies:** Programmers would need to carefully order cases, and refactoring (reordering cases) would change program behavior.

3. **Non-determinism expectations:** Go's specification states that if multiple cases are ready, one is chosen "uniformly at random." Random pollorder implements this guarantee.

The `pollorder` is generated using a Fisher-Yates shuffle with `cheaprand()`, ensuring a uniform random permutation on each `selectgo` call.

</details>
