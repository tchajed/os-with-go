# Module 9: Goroutine Stacks (45 min)

## Background: Stack Management and the Cost of Concurrency

The call stack is one of the oldest and most fundamental abstractions in computing.
Every time a function is called, a new *stack frame* is pushed onto the stack,
holding local variables, the return address, saved registers, and function
arguments. When the function returns, its frame is popped. This last-in, first-out
(LIFO) discipline is elegant and efficient: allocation is a single pointer
decrement (on architectures where the stack grows downward), and deallocation is
a pointer increment. There is no fragmentation, no free-list traversal, and no
garbage collection -- just a stack pointer moving up and down. This simplicity
is why nearly every compiled language uses a call stack, and why hardware provides
dedicated instructions (`CALL`, `RET`, `PUSH`, `POP` on x86) to manipulate it.

Operating systems allocate a fixed-size stack for each thread at creation time.
On Linux, the default is 8 MB (configurable via `ulimit -s` or
`pthread_attr_setstacksize`); macOS uses 512 KB for secondary threads; Windows
defaults to 1 MB. To detect overflow, the OS places a *guard page* at the bottom
of the stack -- a page mapped with no permissions (`PROT_NONE`). If the stack
pointer crosses into the guard page, the hardware triggers a fault, and the OS
delivers a signal (SIGSEGV on Unix) or structured exception (on Windows). This
mechanism is simple and adds zero runtime overhead to normal execution, since the
check is performed by the MMU in hardware. But the fixed-size design forces a
difficult trade-off: set the stack too small and deep recursion crashes; set it
too large and memory is wasted. Most programs use only a few kilobytes of their
multi-megabyte stack, yet the full allocation must be reserved up front. Some
systems also deploy *stack canaries* -- sentinel values placed between the return
address and local variables that are checked on function return to detect
buffer-overflow attacks -- but these serve security rather than growth management.

This fixed-size model becomes a serious bottleneck for highly concurrent systems.
A server handling 10,000 simultaneous connections with one thread per connection
commits 80 GB of virtual address space to stacks alone. At a million connections,
the numbers become absurd. Even though modern virtual memory systems use demand
paging (physical pages are not allocated until touched), the page table entries,
TLB pressure, and kernel bookkeeping for a million threads impose real costs.
This is the fundamental tension that drives language runtimes to seek alternatives:
how do you support millions of concurrent tasks when each one needs a stack?

Different runtimes have explored different answers. GCC's *split stacks* (used by
early Go through version 1.3) and similar *segmented stack* designs allocate small
initial stack segments and link new segments on demand. This avoids the upfront
memory commitment, but introduces the notorious "hot split" problem: if a function
near the segment boundary is called repeatedly in a tight loop, each call allocates
a new segment and each return frees it, causing severe thrashing. Erlang's BEAM VM
takes a different approach entirely, giving each process a tiny initial stack
(sometimes as small as 233 words) that grows incrementally, enabled by the fact
that Erlang's immutable data model means the stack rarely holds pointers that would
complicate relocation. Rust's async model sidesteps the problem altogether for
asynchronous code: `async fn` compiles to a state machine (a "stackless coroutine")
that captures only the variables live across yield points, requiring no contiguous
stack at all. This is memory-efficient but means async and synchronous Rust code
use fundamentally different execution models.

Go's solution, adopted in Go 1.4 and still in use today, is the *contiguous
growable stack*: goroutines start with a 2 KB stack, and when more space is needed,
the runtime allocates a new stack of double the size, copies the old stack contents
to the new location, adjusts every pointer that referenced the old stack, and frees
the old allocation. This approach eliminates the hot split problem (doubling
provides amortized O(1) cost per frame), preserves cache locality (the stack
remains a single contiguous region), and requires no hardware or OS support beyond
ordinary memory allocation. The price is that the compiler must generate precise
*stack maps* -- bitmaps that identify which words in every stack frame are pointers
-- so the runtime knows exactly what to adjust during a copy. This module examines
how Go implements this design, from the compiler-inserted prologue checks to the
stack copying algorithm and the per-P allocation pools that make it fast.

---

## 1. OS Thread Stacks: The Fixed-Size Problem (10 min)

### How OS Threads Allocate Stacks

When the OS creates a thread (via `pthread_create` on Unix or `CreateThread` on
Windows), it allocates a **fixed-size** stack. Typical defaults:

| OS            | Default Stack Size |
|---------------|-------------------|
| Linux         | 8 MB              |
| macOS         | 512 KB (secondary threads), 8 MB (main) |
| Windows       | 1 MB              |

The stack is allocated as a contiguous block of virtual memory. At the bottom sits
a **guard page** -- a page mapped with no permissions (`PROT_NONE`). If the stack
pointer reaches the guard page, the hardware triggers a segfault, which the OS
converts into a stack overflow error.

### Why Fixed Stacks Are Wasteful

Consider a web server handling 10,000 concurrent connections, one thread per
connection:

- At 8 MB per stack: **80 GB** of virtual address space committed to stacks
- Most of that stack space is unused -- typical functions use only a few KB
- Even with virtual memory (demand paging), the TLB and page table overhead is
  significant
- Thread creation is expensive: the kernel must set up the stack, allocate a
  task struct, and perform a system call

This is the fundamental reason Go uses goroutines instead of threads: **you cannot
have millions of OS threads, because the stack memory alone would be prohibitive.**

### The Goroutine Advantage

Go goroutines start with a **2 KB** stack. At that size:

- 1 million goroutines = ~2 GB of stack memory (vs. 8 TB for threads)
- Stacks grow on demand, so most goroutines never allocate more than they need
- Stack allocation is a userspace operation -- no system call required

---

## 2. Goroutine Stack Layout (10 min)

### The stack Struct

Every goroutine's stack bounds are tracked in the `g` struct. The stack itself is
described by a simple pair of pointers:

```go
// [src/runtime/runtime2.go, lines 462-465](https://cs.opensource.google/go/go/+/refs/tags/go1.26.1:src/runtime/runtime2.go;l=462)
type stack struct {
	lo uintptr
	hi uintptr
}
```

The stack occupies exactly the memory range `[lo, hi)`. The stack grows downward
(toward lower addresses), so `hi` is the "top" (where execution starts) and `lo`
is the "bottom" (the limit).

### Stack Fields in the G Struct

The first fields of the `g` struct are dedicated to stack management:

```go
// [src/runtime/runtime2.go, lines 473-483](https://cs.opensource.google/go/go/+/refs/tags/go1.26.1:src/runtime/runtime2.go;l=473)
type g struct {
	// Stack parameters.
	// stack describes the actual stack memory: [stack.lo, stack.hi).
	// stackguard0 is the stack pointer compared in the Go stack growth prologue.
	// It is stack.lo+StackGuard normally, but can be StackPreempt to trigger a preemption.
	// stackguard1 is the stack pointer compared in the //go:systemstack stack growth prologue.
	// It is stack.lo+StackGuard on g0 and gsignal stacks.
	// It is ~0 on other goroutine stacks, to trigger a call to morestackc (and crash).
	stack       stack   // offset known to runtime/cgo
	stackguard0 uintptr // offset known to liblink
	stackguard1 uintptr // offset known to liblink
	// ...
}
```

Key fields:
- **`stack`**: The `[lo, hi)` bounds of the current stack allocation.
- **`stackguard0`**: The threshold for the stack growth check. Normally set to
  `stack.lo + StackGuard`. The compiler-inserted prologue compares SP against
  this value. It can also be set to the special value `stackPreempt` to force a
  preemption check.
- **`stackguard1`**: Used for `//go:systemstack` functions. Set to `~0` on
  regular goroutine stacks so that any system stack growth attempt will crash
  (system stack code should not need to grow).

### Stack Guard and StackSmall

The stack layout includes a guard region at the bottom. From the comments in
`stack.go`:

```go
// [src/runtime/stack.go, lines 20-68](https://cs.opensource.google/go/go/+/refs/tags/go1.26.1:src/runtime/stack.go;l=20)
/*
Stack layout parameters.

The per-goroutine g->stackguard is set to point StackGuard bytes
above the bottom of the stack.  Each function compares its stack
pointer against g->stackguard to check for overflow.  To cut one
instruction from the check sequence for functions with tiny frames,
the stack is allowed to protrude StackSmall bytes below the stack
guard.  Functions with large frames don't bother with the check and
always call morestack.  The sequences are (for amd64, others are
similar):

	guard = g->stackguard
	frame = function's stack frame size
	argsize = size of function arguments (call + return)

	stack frame size <= StackSmall:
		CMPQ guard, SP
		JHI 3(PC)
		MOVQ m->morearg, $(argsize << 32)
		CALL morestack(SB)

	stack frame size > StackSmall but < StackBig
		LEAQ (frame-StackSmall)(SP), R0
		CMPQ guard, R0
		JHI 3(PC)
		MOVQ m->morearg, $(argsize << 32)
		CALL morestack(SB)

	stack frame size >= StackBig:
		MOVQ m->morearg, $((argsize << 32) | frame)
		CALL morestack(SB)
*/
```

The visual layout of a goroutine stack:

```
    stack.hi  ────────────────────── (top of stack, where execution begins)
              │                    │
              │  active frames     │  ← SP is somewhere in here
              │  (grows downward)  │
              │                    │
              ├────────────────────┤
              │                    │
              │  unused space      │
              │                    │
    guard0    ├────────────────────┤  ← stackguard0 = stack.lo + StackGuard
              │                    │
              │  guard area        │  ← room for nosplit functions + morestack
              │  (StackGuard bytes)│
              │                    │
    stack.lo  ────────────────────── (bottom of stack)
```

### Constants

```go
// [src/runtime/stack.go, lines 70-103](https://cs.opensource.google/go/go/+/refs/tags/go1.26.1:src/runtime/stack.go;l=70)
const (
	stackSystem = goos.IsWindows*4096 + goos.IsPlan9*512 + goos.IsIos*goarch.IsArm64*1024

	// The minimum size of stack used by Go code
	stackMin = 2048

	// stackNosplit is the maximum number of bytes that a chain of NOSPLIT
	// functions can use.
	stackNosplit = abi.StackNosplitBase * sys.StackGuardMultiplier

	// The stack guard leaves enough room for a stackNosplit chain of NOSPLIT calls
	// plus one stackSmall frame plus stackSystem bytes for the OS.
	stackGuard = stackNosplit + stackSystem + abi.StackSmall
)
```

The initial stack size is `stackMin = 2048` bytes (2 KB). The `stackSystem`
constant adds extra space on platforms that need it (4 KB on Windows for signal
handling on the goroutine stack, for example).

### Preemption via stackguard0

The `stackguard0` field serves double duty. Beyond stack overflow detection, it
is used for **cooperative preemption**:

```go
// [src/runtime/stack.go, lines 70-103](https://cs.opensource.google/go/go/+/refs/tags/go1.26.1:src/runtime/stack.go;l=70)
const (
	// Goroutine preemption request.
	// 0xfffffade in hex.
	stackPreempt = uintptrMask & -1314

	// Thread is forking. Causes a split stack check failure.
	// 0xfffffb2e in hex.
	stackFork = uintptrMask & -1234
)
```

When the scheduler wants to preempt a goroutine, it sets `stackguard0 = stackPreempt`.
This special sentinel value is larger than any real SP, so the next prologue check
will always fail, causing a call to `morestack`, which then checks for preemption
rather than actually growing the stack. This is how Go achieves cooperative
preemption at function call boundaries (complementing the asynchronous signal-based
preemption added in Go 1.14).

---

## 3. Stack Growth Mechanism (10 min)

### The Prologue Check

The Go compiler inserts a **stack growth prologue** at the beginning of every
function (except those marked `//go:nosplit`). For a small function on amd64,
the generated code is essentially:

```asm
CMPQ    g.stackguard0, SP
JHI     morestack_call
```

If SP has dropped below `stackguard0`, the function calls `morestack`, which is
an assembly trampoline that saves the current context and calls `newstack()`.

### newstack(): The Growth Entry Point

```go
// [src/runtime/stack.go, lines 1014-1026, 1026](https://cs.opensource.google/go/go/+/refs/tags/go1.26.1:src/runtime/stack.go;l=1014)
// Called from runtime·morestack when more stack is needed.
// Allocate larger stack and relocate to new stack.
// Stack growth is multiplicative, for constant amortized cost.
//
// ...
func newstack() {
```

The `newstack` function (line 1026) handles both preemption and actual stack growth.
After checking for preemption (lines 1093-1146), it doubles the stack size:

```go
// [src/runtime/stack.go, lines 1148-1151](https://cs.opensource.google/go/go/+/refs/tags/go1.26.1:src/runtime/stack.go;l=1148)
	// Allocate a bigger segment and move the stack.
	oldsize := gp.stack.hi - gp.stack.lo
	newsize := oldsize * 2
```

It then ensures the new size is sufficient for the pending frame:

```go
// [src/runtime/stack.go, lines 1152-1162](https://cs.opensource.google/go/go/+/refs/tags/go1.26.1:src/runtime/stack.go;l=1152)
	// Make sure we grow at least as much as needed to fit the new frame.
	if f := findfunc(gp.sched.pc); f.valid() {
		max := uintptr(funcMaxSPDelta(f))
		needed := max + stackGuard
		used := gp.stack.hi - gp.sched.sp
		for newsize-used < needed {
			newsize *= 2
		}
	}
```

The goroutine transitions to `_Gcopystack` status (preventing the GC from scanning
it during the copy), and then calls `copystack`:

```go
// [src/runtime/stack.go, lines 1183-1192](https://cs.opensource.google/go/go/+/refs/tags/go1.26.1:src/runtime/stack.go;l=1183)
	casgstatus(gp, _Grunning, _Gcopystack)
	copystack(gp, newsize)
	casgstatus(gp, _Gcopystack, _Grunning)
	gogo(&gp.sched)
```

### copystack(): The Heart of Stack Growth

The `copystack` function (line 900) performs the actual stack relocation:

```go
// [src/runtime/stack.go, lines 898-904](https://cs.opensource.google/go/go/+/refs/tags/go1.26.1:src/runtime/stack.go;l=898)
// Copies gp's stack to a new stack of a different size.
// Caller must have changed gp status to Gcopystack.
func copystack(gp *g, newsize uintptr) {
	if gp.syscallsp != 0 {
		throw("stack growth not allowed in system call")
	}
	old := gp.stack
```

The algorithm:

1. **Allocate a new stack** of the requested size:
   ```go
   // line 916
   new := stackalloc(uint32(newsize))
   ```

2. **Compute the adjustment delta** (difference between new and old base addresses):
   ```go
   // lines 925-927
   var adjinfo adjustinfo
   adjinfo.old = old
   adjinfo.delta = new.hi - old.hi
   ```

3. **Adjust sudog pointers** -- goroutines blocked on channel operations may have
   pointers into the stack that need updating.

4. **Copy the used portion** of the stack to the new location:
   ```go
   // line 956
   memmove(unsafe.Pointer(new.hi-ncopy), unsafe.Pointer(old.hi-ncopy), ncopy)
   ```

5. **Walk all stack frames** and adjust every pointer that points into the old stack:
   ```go
   // lines 974-978
   var u unwinder
   for u.init(gp, 0); u.valid(); u.next() {
       adjustframe(&u.frame, &adjinfo)
   }
   ```

6. **Update the G's stack fields** and free the old stack:
   ```go
   // lines 968-971
   gp.stack = new
   gp.stackguard0 = new.lo + stackGuard
   gp.sched.sp = new.hi - used
   ```

### Pointer Adjustment in Detail

The `adjustpointer` function (line 610) checks whether a pointer falls within the
old stack bounds and, if so, adjusts it by the delta:

```go
// [src/runtime/stack.go, lines 610-615](https://cs.opensource.google/go/go/+/refs/tags/go1.26.1:src/runtime/stack.go;l=610)
func adjustpointer(adjinfo *adjustinfo, vpp unsafe.Pointer) {
	pp := (*uintptr)(vpp)
	p := *pp
	if adjinfo.old.lo <= p && p < adjinfo.old.hi {
		*pp = p + adjinfo.delta
	}
}
```

This works because the compiler generates **stack maps** (pointer bitmaps) for
every function at every safe point. These maps tell the runtime exactly which
words on the stack are pointers, enabling precise adjustment.

---

## 4. Stack Allocation Pools (7 min)

### Small Stacks: Per-P Free Lists

For small stacks (up to a few orders above the fixed stack size), the runtime
maintains **per-P caches** for lock-free allocation:

```go
// [src/runtime/stack.go, lines 147-168](https://cs.opensource.google/go/go/+/refs/tags/go1.26.1:src/runtime/stack.go;l=147)
// Global pool of spans that have free stacks.
// Stacks are assigned an order according to size.
//
//	order = log_2(size/FixedStack)
//
// There is a free list for each order.
var stackpool [_NumStackOrders]struct {
	item stackpoolItem
	_    [(cpu.CacheLinePadSize - unsafe.Sizeof(stackpoolItem{})%cpu.CacheLinePadSize) % cpu.CacheLinePadSize]byte
}

type stackpoolItem struct {
	_    sys.NotInHeap
	mu   mutex
	span mSpanList
}

// Global pool of large stack spans.
var stackLarge struct {
	lock mutex
	free [heapAddrBits - gc.PageShift]mSpanList
}
```

The allocation fast path in `stackalloc` (line 344) checks the per-P mcache first:

```go
// [src/runtime/stack.go, lines 388-397](https://cs.opensource.google/go/go/+/refs/tags/go1.26.1:src/runtime/stack.go;l=388)
		} else {
			c := thisg.m.p.ptr().mcache
			x = c.stackcache[order].list
			if x.ptr() == nil {
				stackcacherefill(c, order)
				x = c.stackcache[order].list
			}
			c.stackcache[order].list = x.ptr().next
			c.stackcache[order].size -= uintptr(n)
		}
```

This is the same pattern used for heap allocation (module 8): keep a per-P cache
to avoid lock contention. When the local cache is empty, `stackcacherefill` grabs
a batch from the global pool (holding a lock briefly).

### Large Stacks: From the Heap

For large stacks (beyond the cached sizes), `stackalloc` allocates directly from
the memory heap:

```go
// [src/runtime/stack.go, lines 405-430](https://cs.opensource.google/go/go/+/refs/tags/go1.26.1:src/runtime/stack.go;l=405)
	} else {
		var s *mspan
		npage := uintptr(n) >> gc.PageShift
		log2npage := stacklog2(npage)

		// Try to get a stack from the large stack cache.
		lock(&stackLarge.lock)
		if !stackLarge.free[log2npage].isEmpty() {
			s = stackLarge.free[log2npage].first
			stackLarge.free[log2npage].remove(s)
		}
		unlock(&stackLarge.lock)

		if s == nil {
			// Allocate a new stack from the heap.
			s = mheap_.allocManual(npage, spanAllocStack)
			if s == nil {
				throw("out of memory")
			}
			osStackAlloc(s)
			s.elemsize = uintptr(n)
		}
		v = unsafe.Pointer(s.base())
	}
```

### stackfree(): Returning to Pools

The `stackfree` function (line 463) mirrors `stackalloc`. Small stacks are returned
to the per-P cache:

```go
// [src/runtime/stack.go, lines 501-532](https://cs.opensource.google/go/go/+/refs/tags/go1.26.1:src/runtime/stack.go;l=501)
	if n < fixedStack<<_NumStackOrders && n < _StackCacheSize {
		// ...
		if stackNoCache != 0 || gp.m.p == 0 || gp.m.preemptoff != "" {
			lock(&stackpool[order].item.mu)
			stackpoolfree(x, order)
			unlock(&stackpool[order].item.mu)
		} else {
			c := gp.m.p.ptr().mcache
			if c.stackcache[order].size >= _StackCacheSize {
				stackcacherelease(c, order)
			}
			x.ptr().next = c.stackcache[order].list
			c.stackcache[order].list = x
			c.stackcache[order].size += n
		}
```

Large stacks are returned differently depending on GC phase:

```go
// [src/runtime/stack.go, lines 533-555](https://cs.opensource.google/go/go/+/refs/tags/go1.26.1:src/runtime/stack.go;l=533)
	} else {
		s := spanOfUnchecked(uintptr(v))
		if gcphase == _GCoff {
			// Free the stack immediately if we're sweeping.
			osStackFree(s)
			mheap_.freeManual(s, spanAllocStack)
		} else {
			// If the GC is running, we can't return a stack span to the heap
			// because it could be reused as a heap span, and this state
			// change would race with GC. Add it to the large stack cache instead.
			log2npage := stacklog2(s.npages)
			lock(&stackLarge.lock)
			stackLarge.free[log2npage].insert(s)
			unlock(&stackLarge.lock)
		}
	}
```

---

## 5. Stack Shrinking (3 min)

Stacks only grow during execution, but can be **shrunk by the garbage collector**.
The `shrinkstack` function (line 1257) halves the stack if the goroutine is using
less than 1/4 of its allocated space:

```go
// [src/runtime/stack.go, lines 1253-1306](https://cs.opensource.google/go/go/+/refs/tags/go1.26.1:src/runtime/stack.go;l=1253)
// Maybe shrink the stack being used by gp.
func shrinkstack(gp *g) {
	// ...
	oldsize := gp.stack.hi - gp.stack.lo
	newsize := oldsize / 2
	// Don't shrink the allocation below the minimum-sized stack allocation.
	if newsize < fixedStack {
		return
	}
	// Compute how much of the stack is currently in use and only
	// shrink the stack if gp is using less than a quarter of its
	// current stack.
	avail := gp.stack.hi - gp.stack.lo
	if used := gp.stack.hi - gp.sched.sp + stackNosplit; used >= avail/4 {
		return
	}
	// ...
	copystack(gp, newsize)
}
```

Stack shrinking reuses the same `copystack` mechanism as growth -- allocate a
smaller stack, copy the contents, adjust pointers. Shrinking can only happen at
safe points (not during syscalls, not at asynchronous safe points, not while
parking on a channel).

The `preemptShrink` flag in the G struct allows the GC to request that a goroutine
shrink its stack at the next synchronous safe point:

```go
// [src/runtime/stack.go, lines 1130-1135](https://cs.opensource.google/go/go/+/refs/tags/go1.26.1:src/runtime/stack.go;l=1130)
		if gp.preemptShrink {
			// We're at a synchronous safe point now, so
			// do the pending stack shrink.
			gp.preemptShrink = false
			shrinkstack(gp)
		}
```

---

## 6. Segmented Stacks vs. Contiguous Stacks (5 min)

### The Segmented Stack Era (Go 1.0 - 1.3)

Early versions of Go used **segmented stacks** (also called "split stacks"). When a
goroutine needed more stack space:

1. A new segment was allocated (not necessarily adjacent to the old one)
2. A "stack link" connected the new segment to the old one
3. When the function returned, the segment was freed

**Problem: the "hot split"**. Consider a function right at the stack boundary that
is called in a tight loop. Each call allocates a new segment; each return frees it.
This thrashing destroyed performance for certain workloads.

### The Contiguous Stack Solution (Go 1.4+)

Go 1.4 switched to **contiguous stacks** (also called "stack copying"). Instead of
linking segments:

1. Allocate a new, larger, contiguous stack (2x the old size)
2. Copy the entire old stack to the new one
3. Walk all frames and adjust every pointer into the stack
4. Free the old stack

**Advantages:**
- No hot split problem -- growth is amortized (doubling means O(log n) copies)
- Better cache locality -- the entire stack is contiguous
- Simpler runtime -- no need to manage linked segments or handle cross-segment returns

**Requirement:**
- The runtime must have **precise pointer maps** for every stack frame, so it knows
  which words are pointers and need adjustment. This is why Go compiles with full
  stack map information -- it is not optional.

### Why Not Virtual Memory?

One might ask: why not just allocate a huge virtual address range and let the OS
page in memory on demand? This is what Go's `g0` (system) stacks and OS threads do.
For goroutines, this approach fails because:

- Each goroutine would need a reserved virtual address range (e.g., 1 MB)
- With 1 million goroutines, that is 1 TB of virtual address space
- Page table entries, TLB pressure, and `mmap` overhead make this impractical
- On 32-bit systems (historically relevant), address space is far too limited

---

## Summary

| Aspect              | OS Thread Stack       | Goroutine Stack        |
|---------------------|-----------------------|------------------------|
| Initial size        | 1-8 MB (fixed)        | 2 KB (growable)        |
| Growth mechanism    | Guard page + crash    | Prologue check + copy  |
| Maximum size        | Fixed at creation     | 1 GB (configurable)    |
| Allocation          | Kernel (mmap/VirtualAlloc) | Userspace pool    |
| Shrinking           | Never                 | GC can halve if < 1/4 used |
| Memory overhead     | ~8 MB per thread      | ~2 KB per goroutine    |
| Pointer adjustment  | N/A                   | Full stack walk + maps |

### Key Takeaways

1. **2 KB initial stacks** make goroutines cheap to create -- 4000x less memory
   than a typical OS thread stack.
2. **Compiler-inserted prologues** check SP against stackguard0 at every function
   entry, making stack growth transparent to user code.
3. **Contiguous stack copying** with pointer adjustment is the mechanism that makes
   growable stacks possible. It requires precise pointer maps from the compiler.
4. **Per-P stack caches** make allocation and deallocation fast, following the same
   pattern as the memory allocator (module 8).
5. **stackguard0 serves double duty**: stack overflow detection and cooperative
   preemption, unifying two mechanisms into one check.

### Discussion Questions

1. What happens if a goroutine calls a C function via cgo? Can the goroutine's
   stack be moved while C code holds pointers into it?
2. Why does `copystack` need to handle channel sudogs specially? What race
   condition could occur?
3. The maximum stack size defaults to 1 GB. What would happen if a goroutine
   legitimately needed more? Should the runtime support larger stacks?
