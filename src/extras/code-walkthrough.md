# Guided Code Walkthrough: Life of a Goroutine

This walkthrough traces the complete lifecycle of a goroutine through the Go runtime
source code, connecting every module in the course. Use this as a capstone exercise
or a guided self-study activity.

---

## Setup

Open the Go source tree to `src/runtime/`. You'll need these files:

- `proc.go` — scheduler
- `runtime2.go` — data structures
- `chan.go` — channels
- `stack.go` — stack management
- `malloc.go` — memory allocation

We'll trace what happens when you write:

```go
func main() {
    ch := make(chan int, 1)
    go func() {
        ch <- 42
    }()
    fmt.Println(<-ch)
}
```

---

## Phase 1: Goroutine Creation (`go func()`)

**Start at:** `proc.go` — search for `func newproc(`

When the compiler sees `go func() { ... }`, it emits a call to `newproc`.

### Step 1.1: newproc

```
proc.go:newproc()
```

- Gets the caller's PC (program counter)
- Calls `newproc1()` with the function value and caller info

### Step 1.2: newproc1

```
proc.go:newproc1()
```

- Tries to reuse a dead goroutine from the P's free list (`gfget`)
- If none available, allocates a new `g` struct with `malg(stackMin)`
  - This allocates a **2KB stack** (see Module 9)
  - `stackalloc()` in `stack.go` handles the allocation from per-P pools
- Initializes the goroutine:
  - Sets up the `gobuf` (saved registers) so that when the goroutine is scheduled,
    it starts executing the target function
  - Sets `gp.startpc` to the function's address
  - Assigns a new goroutine ID (`goidgen`)
  - Sets status to `_Grunnable`
- Puts the new goroutine on the current P's run queue via `runqput()`

### Step 1.3: runqput

```
proc.go:runqput()
```

- If `next=true`: stores the goroutine in `pp.runnext` (priority slot)
  - Any goroutine already in `runnext` gets pushed to the regular queue
- Otherwise: appends to the circular buffer `pp.runq[tail]`
- If the local queue is full (256 entries): calls `runqputslow()` to move
  half the queue to the global run queue

### Step 1.4: wakep

```
proc.go:wakep()
```

- Called after adding work to ensure an M is available to run it
- If there's an idle P and no spinning threads, starts or wakes an M

**Connection to Module 4:** This is the goroutine creation lifecycle.
**Connection to Module 5:** wakep triggers the spinning thread mechanism.

---

## Phase 2: Scheduling

**Start at:** `proc.go` — search for `func schedule(`

An idle M (or one that just finished running a goroutine) calls `schedule()`.

### Step 2.1: schedule

```
proc.go:schedule()
```

- Calls `findRunnable()` to find a goroutine to run
- This call **blocks** until work is available

### Step 2.2: findRunnable

```
proc.go:findRunnable()
```

The priority order for finding work:

1. Check for trace reader goroutines
2. Check for GC work
3. Every 61st tick: check the **global run queue** (fairness)
4. Check for finalizer goroutines
5. Check the **local run queue** via `runqget()`
6. Check the **global run queue** via `globrunqget()`
7. Check the **network poller** via `netpoll()`
8. **Steal from other Ps** via `stealWork()`
9. If nothing found: park the P and block the M

### Step 2.3: execute

```
proc.go:execute()
```

Once a runnable goroutine is found:

- Sets `mp.curg = gp` and `gp.m = mp` (link M and G)
- Transitions status from `_Grunnable` to `_Grunning`
- Calls `gogo(&gp.sched)` — an assembly function that restores the goroutine's
  saved registers and jumps to its code

**The goroutine is now running.** It executes on the M's OS thread, using the P's
resources (mcache for allocation, local run queue for spawning children).

**Connection to Module 5:** Work stealing in step 2.2.
**Connection to Module 10:** Network poller integration in step 2.2.

---

## Phase 3: Channel Send (Blocking)

**Start at:** `chan.go` — search for `func chansend(`

Our goroutine executes `ch <- 42`.

### Step 3.1: chansend

```
chan.go:chansend()
```

Since the channel is buffered (size 1) and empty:

- Acquires `c.lock`
- Checks if any receiver is waiting on `c.recvq` — none yet
- Checks if buffer has space (`c.qcount < c.dataqsiz`) — yes
- Copies the value into the circular buffer at `c.buf[c.sendx]`
- Increments `c.sendx` and `c.qcount`
- Releases `c.lock`
- Returns (goroutine is not blocked in this case)

**If the buffer were full**, the goroutine would:

- Create a `sudog` struct referencing itself
- Enqueue on `c.sendq`
- Call `gopark()` to block

### Step 3.2: gopark (when blocking occurs)

```
proc.go:gopark()
```

- Saves the unlock function and lock pointer to the M
- Calls `mcall(park_m)` to switch from user goroutine to g0

### Step 3.3: park_m

```
proc.go:park_m()
```

- Transitions goroutine from `_Grunning` to `_Gwaiting`
- Calls the unlock function (releases the channel lock)
- Calls `schedule()` to find other work

**Connection to Module 7:** Channel send/receive mechanics.
**Connection to Module 4:** gopark/schedule interaction.

---

## Phase 4: Channel Receive (Waking)

**Start at:** `chan.go` — search for `func chanrecv(`

The main goroutine executes `<-ch`.

### Step 4.1: chanrecv

```
chan.go:chanrecv()
```

- Acquires `c.lock`
- Checks if any sender is waiting on `c.sendq` — no (sender already completed)
- Checks if buffer has data (`c.qcount > 0`) — yes
- Copies value from `c.buf[c.recvx]` into the receiver's variable
- Decrements `c.qcount`
- Releases `c.lock`
- Returns the value

**If the buffer were empty and a sender were parked:**

- Dequeues the sender's `sudog` from `c.sendq`
- Copies the value directly from sender to receiver (bypassing buffer)
- Calls `goready()` on the sender's goroutine

### Step 4.2: goready (waking a parked goroutine)

```
proc.go:goready()
```

- Transitions goroutine from `_Gwaiting` to `_Grunnable`
- Puts it on the current P's local run queue via `runqput()`
- Calls `wakep()` to ensure an M picks it up

**Connection to Module 7:** Direct send optimization.
**Connection to Module 4:** goready/runqput interaction.

---

## Phase 5: Goroutine Exit

**Start at:** `proc.go` — search for `func goexit1(`

After `ch <- 42` completes, the anonymous function returns. The runtime has
arranged for the return address to be `goexit`, which calls `goexit1`.

### Step 5.1: goexit1

```
proc.go:goexit1()
```

- Calls `mcall(goexit0)` to switch to g0

### Step 5.2: goexit0

```
proc.go:goexit0()
```

- Transitions goroutine to `_Gdead`
- Clears the goroutine's fields (stack, M link, etc.)
- Calls `gdestroy()` to clean up
- Places the dead goroutine on the P's free list for reuse (`gfput`)
- Calls `schedule()` to find new work

**Connection to Module 3:** Goroutine lifecycle states.
**Connection to Module 9:** Stack is returned to the pool.

---

## Phase 6: Stack Growth (if it happens)

**Start at:** `stack.go` — search for `func newstack(`

If at any point during execution, a function prologue detects that the stack
pointer is below `stackguard0`:

### Step 6.1: morestack (assembly)

- Saves current state
- Switches to g0 stack
- Calls `newstack()`

### Step 6.2: newstack

```
stack.go:newstack()
```

- Checks if this is a preemption signal (`stackPreempt`) — if so, handle preemption
- Otherwise: allocates a new stack at **2x the current size**
- Calls `copystack()` to copy the old stack contents
- Adjusts all pointers that reference the old stack
- Resumes execution on the new stack

**Connection to Module 9:** Contiguous stack design.
**Connection to Module 5:** Preemption check piggybacks on stack check.

---

## Summary: The Complete Picture

```
newproc → newproc1 → runqput → wakep
                                 ↓
              schedule ← findRunnable (local/global/steal/netpoll)
                ↓
             execute → gogo → [user code runs]
                                 ↓
                          gopark ← channel/lock/IO blocked
                            ↓
                         park_m → schedule (find other work)
                                    ...
                          goready ← channel/lock/IO ready
                            ↓
                         runqput → schedule picks it up again
                                    ...
                          goexit1 → goexit0 → gfput → schedule
```

Every arrow in this diagram is a function call you can find in the source code.
The scheduler is not a separate thread — it runs on g0 whenever a goroutine
voluntarily yields (gopark), is preempted, or exits.

---

## Exercise

Trace the following program through the runtime, identifying which functions are
called and in what order. Note where goroutines block and wake up.

```go
func main() {
    ch := make(chan string)

    go func() {
        ch <- "hello"   // blocks: unbuffered channel, no receiver yet
    }()

    msg := <-ch          // wakes sender, receives directly
    fmt.Println(msg)
}
```

Hint: with an unbuffered channel, the send will block (gopark) because there's
no receiver. When the main goroutine does `<-ch`, it will find the sender in
`c.sendq`, copy the value directly (bypassing the buffer), and call goready
on the sender.
