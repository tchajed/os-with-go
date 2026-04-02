# Module 10: File Systems, I/O, and the Network Poller

## Comprehension Check

### Question 1 (Code Reading)
Study this excerpt from `runtime/netpoll.go`:

```go
// Integrated network poller (platform-independent part).
// A particular implementation (epoll/kqueue/port/AIX/Windows)
// must define the following functions:
//
// func netpollinit()
//     Initialize the poller. Only called once.
//
// func netpollopen(fd uintptr, pd *pollDesc) int32
//     Arm edge-triggered notifications for fd.
//
// func netpollclose(fd uintptr) int32
//     Disable notifications for fd.
//
// func netpoll(delta int64) (gList, int32)
//     Poll the network. If delta < 0, block indefinitely.
//     If delta == 0, poll without blocking.
//     If delta > 0, block for up to delta nanoseconds.
//     Return a list of goroutines built by calling netpollready.
```

Why does the network poller use **edge-triggered** rather than **level-triggered** notifications? What would happen if it used level-triggered mode?

<details><summary>Answer</summary>

**Edge-triggered** means the poller notifies once when the state changes (e.g., data becomes available), not repeatedly while data remains available.

**Why edge-triggered:**

1. **Fewer wakeups:** With level-triggered mode, every call to `epoll_wait` would return the same file descriptors that still have data, even if the goroutine has not yet read it. In a system with thousands of connections, this causes `epoll_wait` to repeatedly return large batches of ready FDs, most of which are already being handled.

2. **Simpler goroutine model:** The network poller's design is "one goroutine per FD." When data arrives, the goroutine for that FD is woken exactly once. With level-triggered, the poller would need to suppress duplicate wakeups (since the FD remains "ready" until the goroutine reads all data), adding complexity.

3. **Efficient integration with scheduler:** The poller returns a `gList` of goroutines to make runnable. With edge-triggered, each FD produces at most one goroutine per state change, keeping the list small and predictable.

**If level-triggered were used:**
- The `netpoll` function would return the same goroutines repeatedly on every call until they fully drain their data.
- The scheduler would attempt to wake goroutines that are already running, requiring additional bookkeeping.
- On a busy server with 10,000 connections, each `netpoll` call might return thousands of "ready" FDs, most already being serviced, causing wasted work scanning the list.

The tradeoff is that edge-triggered requires the application to fully drain available data on each notification (otherwise data sits unprocessed until the next event). Go handles this by having goroutines read in a loop until `EAGAIN`.

</details>

---

### Question 2 (Short Answer)
Describe the path a `conn.Read()` call takes from user code down to the kernel. Include the role of `os.File`, `poll.FD`, `pollDesc`, and the network poller.

<details><summary>Answer</summary>

The path from user code to kernel and back:

1. **`conn.Read(buf)`** - User calls `Read` on a `net.Conn` (e.g., `*net.TCPConn`).

2. **`net.(*netFD).Read()`** - The `net.Conn` wraps a `netFD`, which delegates to `poll.FD`.

3. **`poll.(*FD).Read()`** - This is in `internal/poll`. It:
   - Calls the `read()` system call (non-blocking, since the FD is set to `O_NONBLOCK`).
   - If data is available, returns immediately (fast path).
   - If `EAGAIN` is returned (no data yet), calls `fd.pd.waitRead()`.

4. **`poll.(*pollDesc).waitRead()`** - Calls `runtime_pollWait(pd, 'r')`.

5. **`runtime.poll_runtime_pollWait()`** - In the runtime:
   - Checks if the FD is already ready (fast path).
   - If not, sets the `pollDesc.rg` field to the current goroutine pointer.
   - Calls `gopark()` to park the goroutine, removing it from the scheduler.

6. **The goroutine is now parked.** The network poller (`epoll_wait`/`kqueue` running in `sysmon` or `findRunnable`) will eventually detect that the FD has data.

7. **`netpoll()` returns** - When `epoll_wait` indicates readiness, `netpollready()` sets `pd.rg = pdReady` and adds the goroutine to the return list.

8. **Goroutine is made runnable** - The scheduler (via `findRunnable` or `sysmon`) injects the goroutine into a P's run queue.

9. **`poll.(*FD).Read()` resumes** - The goroutine wakes up and retries the `read()` system call, which now succeeds.

</details>

---

### Question 3 (True/False with Explanation)
**True or False:** File I/O (reading from disk) uses the same network poller mechanism as network I/O in Go.

<details><summary>Answer</summary>

**Mostly False.** Regular file I/O and network I/O take different paths:

**Network I/O** uses the network poller (`epoll`/`kqueue`). Sockets are set to non-blocking mode, and goroutines park/unpark efficiently through `pollDesc`.

**Regular file I/O** (reading/writing files on disk) does NOT use the network poller. This is because `epoll` and `kqueue` do not support regular files -- they always report regular files as "ready" since disk I/O is handled differently from socket I/O in the kernel.

Instead, regular file reads use **blocking system calls** (`pread`/`read`). When a goroutine does file I/O:
1. It calls `entersyscall()`, which detaches the P from the M.
2. The M blocks in the kernel waiting for disk I/O.
3. `sysmon` may retake the P and hand it to another M.
4. When the I/O completes, the M calls `exitsyscall()` and tries to reacquire a P.

**Exception:** On some platforms, certain special files (pipes, FIFOs, device files, eventfd) CAN be polled and do use the poller. Also, some platforms are adding io_uring support which could change this for regular files.

</details>

---

### Question 4 (What Would Happen If...)
The `pollDesc` struct uses a state machine for its `rg` (read goroutine) field with states: `pdReady`, `pdWait`, `nil`, and a goroutine pointer. What would happen if the implementation skipped the `pdWait` intermediate state and went directly from `nil` to a goroutine pointer?

<details><summary>Answer</summary>

The `pdWait` state exists to handle a race condition between the goroutine preparing to park and the I/O becoming ready. The sequence is:

```
Normal flow:
1. goroutine sets rg = pdWait    (preparing to park)
2. goroutine sets rg = G pointer (committed to parking)
3. netpoll sets rg = pdReady     (I/O ready, wakes goroutine)
```

**Without `pdWait` (going directly nil -> G pointer):**

There is a window between when the goroutine decides to wait and when it actually parks. If I/O becomes ready during this window:

1. Goroutine calls `runtime_pollWait`, checks `rg == nil` (not ready).
2. **I/O completes.** `netpollready` tries to wake the goroutine but `rg == nil` -- no goroutine to wake. The wakeup is lost.
3. Goroutine sets `rg = G pointer` and parks.
4. **Goroutine sleeps forever** -- the I/O event was already delivered and will not be re-delivered (edge-triggered).

**With `pdWait`:**

1. Goroutine sets `rg = pdWait` (announces intent to sleep).
2. If I/O completes now, `netpollready` atomically CAS's `pdWait` -> `pdReady`. The goroutine sees `pdReady` and does not park.
3. If I/O has not completed, goroutine CAS's `pdWait` -> `G pointer` and parks.
4. When I/O completes, `netpollready` CAS's `G pointer` -> `pdReady` and wakes the goroutine.

The `pdWait` state makes the protocol robust against this race by allowing `netpollready` to signal readiness even before the goroutine has fully committed to sleeping.

</details>

---

### Question 5 (Short Answer)
Explain the role of `sysmon` in network polling. Why can't the scheduler rely solely on `findRunnable` calling `netpoll`?

<details><summary>Answer</summary>

`sysmon` periodically calls `netpoll(0)` (non-blocking poll) and injects any ready goroutines into the global run queue.

**Why `findRunnable` alone is insufficient:**

1. **All Ps busy:** `findRunnable` is only called when a P has no local work. If all Ps are busy executing compute-bound goroutines, `netpoll` is never called. Network-ready goroutines would be stuck until some P happens to enter `findRunnable`. `sysmon` runs on its own M (without a P) and can poll regardless of P utilization.

2. **All Ms in syscalls:** If all Ms are blocked in system calls and all Ps have been retaken but are idle (no runnable goroutines), nobody is calling `findRunnable`. `sysmon` can still poll and inject work, then wake an M to run it.

3. **Latency guarantees:** `sysmon` provides a guaranteed upper bound on network polling latency (~10ms). Without it, a burst of compute work could delay network polling indefinitely.

4. **Timer-based blocking:** `findRunnable` may call `netpoll(delta)` with a timeout to block until the next timer fires. But if `findRunnable` is not being called (all Ps busy), timers also need `sysmon` to fire them.

`sysmon` acts as a backstop that ensures the network poller is checked regularly regardless of scheduler activity.

</details>

---

### Question 6 (Code Reading)
Consider this comment from `runtime/netpoll.go` describing `pollDesc` states:

```go
// pollDesc contains 2 binary semaphores, rg and wg, to park reader and writer
// goroutines respectively. The semaphore can be in the following states:
//
//   pdReady - io readiness notification is pending;
//             a goroutine consumes the notification by changing the state to pdNil.
//   pdWait  - a goroutine prepares to park on the semaphore, but not yet parked;
//             the goroutine commits to park by changing the state to G pointer,
//             or, alternatively, concurrent io notification changes the state to pdReady,
//             or, alternatively, concurrent timeout/close changes the state to pdNil.
//   G pointer - the goroutine is blocked on the semaphore;
//               io notification or timeout/close changes the state to pdReady or pdNil
//               respectively and unparks the goroutine.
```

Draw the state transition diagram for `rg`. What transitions are valid, and who performs each transition?

<details><summary>Answer</summary>

State transitions for `rg`:

```
                    goroutine                    goroutine
    pdNil -----(prepare to wait)-----> pdWait ------(commit)-----> G pointer
      ^                                  |                            |
      |                                  |                            |
      |              netpollready        |  timeout/close             | netpollready
      |           (sets pdReady)         |  (sets pdNil)              | (sets pdReady,
      |                                  |                            |  unparks G)
      |                                  v                            v
      +---------(goroutine consumes)--- pdReady <-----(io ready)------+
```

**Transitions and who performs them:**

1. **pdNil -> pdWait**: The goroutine, in `runtime_pollWait`, sets this to announce intent to park.

2. **pdWait -> G pointer**: The goroutine, committing to park (CAS in `netpollblock`).

3. **pdWait -> pdReady**: The network poller (`netpollready`), when I/O becomes ready before the goroutine parked. Goroutine sees this and does not park.

4. **pdWait -> pdNil**: Timeout or FD close, aborting the wait before the goroutine parked.

5. **G pointer -> pdReady**: The network poller (`netpollready`), when I/O becomes ready. The goroutine is unparked.

6. **G pointer -> pdNil**: Timeout or FD close. The goroutine is unparked with an error.

7. **pdReady -> pdNil**: The goroutine, consuming the readiness notification before retrying the I/O operation.

All transitions on `rg` use atomic CAS operations to handle concurrent access between the goroutine and the network poller / timer / close paths.

</details>
