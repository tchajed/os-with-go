# Module 9: Goroutine Stacks (Stack Growth, Copying, Pools)

## Comprehension Check

### Question 1 (Code Reading)
Study this excerpt from the stack growth prologue description in `runtime/stack.go`:

```
guard = g->stackguard
frame = function's stack frame size
argsize = size of function arguments (call + return)

stack frame size <= StackSmall:
    CMPQ guard, SP
    JHI 3(PC)
    MOVQ m->morearg, $(argsize << 32)
    CALL morestack(SB)

stack frame size > StackSmall but < StackBig:
    LEAQ (frame-StackSmall)(SP), R0
    CMPQ guard, R0
    JHI 3(PC)
    MOVQ m->morearg, $(argsize << 32)
    CALL morestack(SB)
```

Why are there different code sequences for small vs. large stack frames? What optimization does the small-frame case provide?

<details><summary>Answer</summary>

**Small-frame case (frame <= StackSmall, typically 128 bytes):**
The prologue simply compares `SP` against `stackguard0` (`CMPQ guard, SP`). This works because the `stackguard` is set `StackGuard` bytes above the bottom of the stack, and `StackSmall` bytes of space below the guard are reserved as a safety margin. For small frames, the function's stack usage fits within this safety margin, so a simple comparison of SP against the guard is sufficient. **This is the common case and requires only one comparison instruction.**

**Larger-frame case (frame > StackSmall):**
The prologue subtracts the frame size (minus `StackSmall`) from SP and compares the result against the guard. This is necessary because the function will push more than `StackSmall` bytes onto the stack -- the safety margin is not sufficient, so we must account for the actual frame size in the comparison.

**Very large frames (frame >= StackBig):**
These skip the comparison entirely and always call `morestack`, because the frame might need more space than the remaining stack in all cases. The frame size is passed directly to `morestack` so it can allocate enough space.

The optimization is about instruction count on the hot path: most functions have small stack frames, so they get the cheapest possible check (one CMP + one conditional jump). The overhead of stack checking is kept to ~2 instructions for the common case.

</details>

---

### Question 2 (Code Reading)
Consider the `copystack` function:

```go
func copystack(gp *g, newsize uintptr) {
	old := gp.stack
	used := old.hi - gp.sched.sp

	// allocate new stack
	new := stackalloc(uint32(newsize))

	// Compute adjustment.
	var adjinfo adjustinfo
	adjinfo.old = old
	adjinfo.delta = new.hi - old.hi

	// Adjust sudogs, synchronizing with channel ops if necessary.
	ncopy := used
	...
	// Copy the stack to the new location
	memmove(unsafe.Pointer(new.hi-ncopy), unsafe.Pointer(old.hi-ncopy), ncopy)

	// Adjust pointers in the new stack.
	adjustctxt(gp, &adjinfo)
	adjustdefers(gp, &adjinfo)
	adjustpanics(gp, &adjinfo)
	adjustsudogs(gp, &adjinfo)
	...
	gp.stack = new
	gp.stackguard0 = new.lo + stackGuard
}
```

What does `adjinfo.delta` represent, and why must pointers within the stack be adjusted after copying?

<details><summary>Answer</summary>

**`adjinfo.delta`** is the difference between the new stack's high address and the old stack's high address: `new.hi - old.hi`. Since stacks grow downward, this represents how much every stack-relative pointer must be shifted.

**Why pointer adjustment is necessary:**

When the stack is copied to a new, larger memory region, its base address changes. Any pointer that refers to a location within the old stack now points to invalid (or freed) memory. These pointers must be adjusted by `delta` to point to the corresponding location in the new stack.

Pointers that need adjustment include:
- **Frame pointers and return addresses** on the stack itself (stack frames contain pointers to parent frames).
- **`defer` records** (`_defer` structs) that reference stack-allocated arguments.
- **`panic` records** that contain stack pointers.
- **`sudog` structs** that reference stack-allocated elements (e.g., the value being sent/received on a channel).
- **Context pointers** (`gp.sched.ctxt`, `gp.sched.sp`, `gp.sched.bp`).

This is also why Go switched from **segmented stacks** to **copyable stacks** in Go 1.4. Segmented stacks avoided copying but caused "hot split" problems (rapidly growing/shrinking at a segment boundary). Copyable stacks are simpler and avoid that issue, at the cost of needing pointer adjustment.

Note: only pointers into the stack need adjustment. Pointers to heap objects are unchanged because the heap does not move.

</details>

---

### Question 3 (True/False with Explanation)
**True or False:** Goroutine stacks can only grow, never shrink.

<details><summary>Answer</summary>

**False.** Goroutine stacks can both grow and shrink.

**Growth:** When a function prologue detects insufficient stack space, it calls `morestack` -> `newstack`, which allocates a new stack (typically 2x the current size) and copies the old stack to the new one via `copystack`.

**Shrinking:** During garbage collection, the GC calls `shrinkstack` on goroutines whose stacks are less than 25% utilized. `shrinkstack` calls `copystack` with a smaller size (half the current size, but never below `stackMin` = 2048 bytes). This reclaims memory from goroutines that had deep call stacks at some point but are now at shallow call depth.

Stack shrinking is important for long-lived programs: a goroutine might briefly need a large stack (e.g., during initialization) but then spend most of its time in a shallow call chain. Without shrinking, that memory would be wasted for the goroutine's entire lifetime.

Shrinking only happens at GC safe points (when the goroutine is stopped) because it requires the same pointer adjustment as growing.

</details>

---

### Question 4 (What Would Happen If...)
Go starts goroutine stacks at 2KB (`stackMin = 2048`). What would happen if the initial stack size were instead set to 1MB (like a typical OS thread stack)?

<details><summary>Answer</summary>

With 1MB initial stacks:

1. **Memory explosion:** A program with 100,000 goroutines would require 100GB of memory just for stacks (vs. ~200MB with 2KB stacks). Most Go server programs create thousands to millions of goroutines, making this impractical.

2. **Virtual address space:** Even with lazy physical memory allocation (the OS does not commit all 1MB immediately if the pages are never touched), 1 million goroutines would need 1TB of virtual address space for stacks alone. On 64-bit systems this is technically possible but would strain the page table and TLB.

3. **Wasted memory:** Most goroutines use far less than 1MB of stack. A goroutine running a simple channel receive loop might use <4KB. The remaining 99.6% would be wasted.

4. **Loss of Go's concurrency advantage:** Go's ability to support millions of concurrent goroutines is one of its defining features, and it depends directly on cheap goroutine creation. 1MB stacks would make goroutines nearly as expensive as OS threads, eliminating this advantage.

5. **No stack overflow protection improvement:** Go's growable stacks already prevent stack overflows by growing as needed. The 1MB limit on OS threads is arbitrary and CAN overflow; Go's approach of growing to any size is strictly better.

The 2KB starting size is carefully chosen: it is large enough for most function calls without immediately triggering growth, yet small enough to allow millions of goroutines.

</details>

---

### Question 5 (Short Answer)
Explain how the stack pool works. What are stack pools, and why does the runtime cache freed stacks instead of always returning memory to the heap?

<details><summary>Answer</summary>

**Stack pools** are per-size free lists of previously allocated goroutine stacks. When a goroutine's stack is freed (the goroutine exits), the stack memory is returned to a pool keyed by its size. When a new goroutine needs a stack of that size, the pool is checked first before going to the allocator.

There are two pool structures:
- **`stackpool`**: For small stacks (2KB, 4KB, 8KB, 16KB). Each size class has a list of `mspan` objects with free stack slots. Accessed with a lock.
- **`stackLarge`**: For larger stacks (>= 32KB). Organized as an array of free lists indexed by `log2(size)`. Also accessed with a lock.

**Why cache freed stacks:**

1. **Allocation speed:** Allocating a stack from a pool is much faster than going to `mheap` (which may need to search for free pages or call `mmap`). Goroutine creation/destruction is very frequent in Go programs.

2. **Reduced fragmentation:** Reusing stacks of the same size avoids fragmenting the heap with many small span allocations and deallocations.

3. **OS call avoidance:** Without pools, high goroutine churn would generate many `mmap`/`munmap` system calls, which are expensive (page table manipulation, TLB flushes).

4. **Working set reuse:** A reused stack page is likely still in the CPU cache or at least in physical memory. A freshly `mmap`-ed page requires a page fault.

The GC clears the stack pools periodically (every other GC cycle) to prevent unbounded memory accumulation when goroutine concurrency decreases.

</details>

---

### Question 6 (Short Answer)
What is the "hot split" problem that Go's original segmented stacks had, and how did the switch to contiguous (copyable) stacks solve it?

<details><summary>Answer</summary>

**The hot split problem:**

With segmented stacks (Go before 1.4), when a function needed more stack space, a new segment was allocated and chained to the current segment. When the function returned, the segment was freed.

The problem occurred when a function at the segment boundary was called repeatedly in a loop:

```go
func outer() {
    for {
        inner() // inner's frame crosses the segment boundary
    }
}
```

Each call to `inner()`:
1. Detects insufficient space on the current segment.
2. Allocates a new segment (involves `malloc`, linked list manipulation).
3. Executes `inner()`.
4. Returns and frees the segment (`free`, linked list manipulation).

This cycle repeats every iteration, turning a simple function call into an expensive allocation/deallocation pair. This is the "hot split" -- a frequently-called function repeatedly triggers segment creation and destruction.

**How contiguous stacks solve it:**

With contiguous (copyable) stacks, when growth is needed:
1. A new, larger stack is allocated (2x the current size).
2. The entire old stack is copied to the new one.
3. The old stack is freed.

Because the new stack is 2x larger, the growth is **amortized**: subsequent calls do NOT trigger growth until the new, larger stack is also exhausted. This makes the amortized cost of stack growth O(1) per function call, similar to how dynamic arrays (slices) achieve amortized O(1) append via doubling.

The hot split problem is completely eliminated because there is no "boundary" to repeatedly cross.

</details>

---

### Question 7 (Code Reading)
The constant `stackMin` is defined as:

```go
const (
	stackMin = 2048
)
```

And in `newstack()`, the decision to grow uses doubling:

```go
oldsize := gp.stack.hi - gp.stack.lo
newsize := oldsize * 2
```

Why doubling? What would be the time complexity of N nested function calls if the stack grew by a fixed additive amount (e.g., +4KB each time) instead of doubling?

<details><summary>Answer</summary>

**Why doubling:** Doubling achieves **amortized O(1) cost per function call** for stack growth, similar to the analysis for dynamic arrays:

- A goroutine starts with a 2KB stack.
- After k growths, the stack is 2KB * 2^k.
- The total copy work across all growths is: 2KB + 4KB + 8KB + ... + 2KB*2^k = 2KB * (2^(k+1) - 1) ≈ 2 * final_size.
- So the total copy work is proportional to the final stack size, meaning each frame push costs O(1) amortized.

**With additive growth (+4KB each time):**
- N nested calls on a 2KB stack would trigger growth approximately every time the stack is exhausted.
- After k growths, the stack is 2KB + 4KB*k.
- Each growth copies the entire current stack: 2KB, 6KB, 10KB, 14KB, ...
- Total copy work: sum of (2KB + 4KB*i) for i=0..k ≈ O(k^2).
- Since k is proportional to N (number of calls), the total cost is **O(N^2)** for N nested calls.

This means a deep recursion of depth 1000 with additive growth would do ~500x more copying work than with doubling. For recursive algorithms (tree traversal, parsing, etc.), this would be catastrophic.

Doubling is the standard technique for amortized-efficient resizable data structures, and its application to stacks is directly analogous to `append()` on slices.

</details>
