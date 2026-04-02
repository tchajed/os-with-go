# Module 8: Memory Management (mcache, mcentral, mheap, GC phases)

## Comprehension Check

### Question 1 (Short Answer)
Describe the three-level memory allocation hierarchy in Go: `mcache`, `mcentral`, and `mheap`. For each level, explain what it caches, who accesses it, and what synchronization is needed.

<details><summary>Answer</summary>

**`mcache` (per-P cache):**
- Each P has its own `mcache` (pointed to by `p.mcache`).
- Caches `mspan` objects for each size class (approximately 70 size classes from 8 bytes to 32KB).
- **No synchronization needed** -- only the P's current M accesses it, and only one goroutine runs on a P at a time. This makes small allocations lock-free.

**`mcentral` (per-size-class central cache):**
- One `mcentral` per size class, stored in `mheap`.
- When an `mcache` runs out of spans for a size class, it requests a new span from the corresponding `mcentral`.
- Returns a span with free objects to the `mcache`, and reclaims fully-swept empty spans.
- **Requires a lock** (`mcentral.lock`) since multiple Ps may request spans concurrently. However, contention is reduced because each size class has its own lock.

**`mheap` (global page heap):**
- The single global heap that manages large allocations and provides spans to `mcentral`.
- Manages pages (8KB each) obtained from the OS via `mmap`.
- Handles allocations larger than 32KB directly (large objects).
- Uses a page allocator (`pageAlloc`) with a radix tree for finding free page runs.
- **Requires `mheap.lock`** for most operations. This is the most contended lock in the allocator, which is why the mcache and mcentral layers exist to avoid it.

**Flow:** Goroutine allocates -> `mcache` (no lock) -> `mcentral` (size-class lock) -> `mheap` (global lock) -> OS (`mmap`).

</details>

---

### Question 2 (True/False with Explanation)
**True or False:** The Go garbage collector stops all goroutines for the entire duration of a collection cycle.

<details><summary>Answer</summary>

**False.** Go uses a **concurrent, tri-color mark-sweep** collector. It only stops the world (STW) briefly at two points:

1. **STW Phase 1 (Sweep Termination):** All goroutines are paused to finish any remaining sweeping from the previous cycle, enable the write barrier, and transition to the mark phase. This typically takes <1ms.

2. **Concurrent Mark:** Goroutines resume execution. GC mark workers run concurrently alongside application goroutines, tracing reachable objects. The write barrier ensures new pointer writes are tracked. This phase does most of the work.

3. **STW Phase 2 (Mark Termination):** All goroutines pause again briefly to finalize marking, disable the write barrier, and transition to the sweep phase. Also typically <1ms.

4. **Concurrent Sweep:** Spans are swept (unreachable objects freed) lazily as they are needed for allocation, or by background sweep goroutines.

The vast majority of GC work happens concurrently. STW pauses are kept short -- Go targets <1ms pauses, and in practice achieves sub-millisecond pauses for most workloads.

</details>

---

### Question 3 (Code Reading)
Consider the `mheap` struct:

```go
type mheap struct {
	_ sys.NotInHeap

	lock mutex

	pages pageAlloc // page allocation data structure

	sweepgen uint32

	allspans []*mspan
	...
}
```

Why does `mheap` have the `sys.NotInHeap` marker? What would go wrong if `mheap` were heap-allocated?

<details><summary>Answer</summary>

`sys.NotInHeap` is a compiler directive that prevents the type from being allocated on the Go heap. `mheap` must not be heap-allocated because:

1. **Circular dependency:** `mheap` IS the heap. If it were allocated on the heap, you would need the heap to allocate the heap -- a chicken-and-egg problem. `mheap` is instead a global variable (`mheap_`) in the BSS segment.

2. **GC interaction:** The GC uses `mheap` to track which objects are alive. If `mheap` itself were a GC-managed object, the GC would need to scan and potentially move the data structure it depends on for scanning. This would create impossible reentrancy and correctness issues.

3. **`mSpanList` fields:** `mheap` contains `mSpanList` (linked lists of spans) that use direct pointer manipulation. If the heap could move `mheap` (e.g., during a hypothetical compacting GC), all those internal pointers would become invalid.

4. **Lock ordering:** `mheap.lock` is acquired during allocation. If allocating `mheap` required acquiring `mheap.lock`, you would have a deadlock.

</details>

---

### Question 4 (What Would Happen If...)
What would happen if Go did not have the `mcache` (per-P cache) and all allocations went directly to `mcentral`?

<details><summary>Answer</summary>

Without `mcache`, every small allocation would need to:

1. **Acquire a lock:** Each allocation would contend on `mcentral.lock` for the relevant size class. In a program with high allocation rates across many goroutines (common in Go), this lock would become a severe bottleneck.

2. **Performance degradation:** A typical Go program allocates millions of small objects per second. Each allocation would go from ~25ns (cache hit in mcache) to potentially microseconds (lock contention under load). This could slow allocation-heavy programs by 10-100x.

3. **Cache inefficiency:** `mcache` keeps recently-used spans in CPU cache. Without it, each allocation would access a centralized data structure likely in a different cache line, causing frequent cache misses.

4. **Scaling collapse:** Performance would degrade with increasing GOMAXPROCS. On a 64-core machine, 64 goroutines simultaneously allocating 16-byte objects would all serialize on `mcentral` locks. With `mcache`, all 64 allocations proceed in parallel with zero contention.

The `mcache` is the key to Go's allocation performance: the fast path (allocating from a cached span with free slots) requires no locks, no atomics, and usually just a few instructions (check bitmap, bump pointer, return).

</details>

---

### Question 5 (Short Answer)
Explain the tri-color abstraction used by Go's garbage collector. What are white, grey, and black objects, and what invariant must the write barrier maintain?

<details><summary>Answer</summary>

**Tri-color abstraction:**

- **White objects:** Not yet visited by the GC. At the end of marking, white objects are garbage and can be freed.
- **Grey objects:** Discovered (reachable) but not yet fully scanned. Their outgoing pointers have not all been traced. Grey objects are in the mark work queue.
- **Black objects:** Fully scanned. The GC has traced all pointers emanating from them.

**Marking process:** Start with roots (stacks, globals) greyed. Repeatedly pick a grey object, scan its pointers (greying any white objects they point to), then blacken it. When no grey objects remain, all reachable objects are black and all white objects are garbage.

**Write barrier invariant (tri-color invariant):** A black object must never point directly to a white object without an intervening grey object. If a mutator (application goroutine) stores a pointer to a white object into a black object during concurrent marking, the GC might never discover the white object and incorrectly free it.

Go uses a **hybrid write barrier** (combining Dijkstra insertion barrier and Yuasa deletion barrier) that ensures:
- When a pointer is written to a black object, the pointed-to object is greyed (Dijkstra).
- When a pointer is overwritten (deleted), the old pointed-to object is greyed (Yuasa).

This combination allows stack rescanning to be eliminated (stacks are treated as always black after initial scanning), which was a major improvement for GC pause times.

</details>

---

### Question 6 (Short Answer)
What is a "size class" in Go's memory allocator, and why does Go use approximately 70 size classes rather than allocating exact-sized blocks?

<details><summary>Answer</summary>

A **size class** is a predetermined allocation size. When a program requests N bytes, the allocator rounds up to the next size class and allocates a block of that size. Size classes range from 8 bytes to 32,768 bytes (32KB), with approximately 70 classes.

**Why fixed size classes instead of exact sizes:**

1. **Reduced fragmentation management:** With exact sizes, the allocator would need to track millions of different free-block sizes, making coalescing and searching complex (as in traditional `malloc` implementations). Size classes group similar sizes, simplifying free-list management.

2. **Span efficiency:** Each `mspan` contains objects of a single size class. This means the span can use a simple bitmap to track which slots are free, rather than complex metadata. Allocation is just "find next free bit."

3. **Cache efficiency:** Objects of the same size class are densely packed in spans, improving spatial locality and reducing TLB misses.

4. **Fast allocation:** The allocator just needs to look up the size class from a table (indexed by allocation size), then check the corresponding mcache slot. No searching through free lists of varying sizes.

5. **Bounded internal waste:** The size classes are chosen so that the maximum wasted space (internal fragmentation) is ~12.5%. For example, a 17-byte allocation goes into size class 24 (wasting 7 bytes), but a 25-byte allocation goes into size class 32 (wasting 7 bytes). The classes are spaced to keep waste bounded.

Allocations >32KB are "large objects" handled directly by `mheap` at page granularity.

</details>

---

### Question 7 (True/False with Explanation)
**True or False:** Go's garbage collector can move objects to compact the heap, similar to Java's G1 or ZGC collectors.

<details><summary>Answer</summary>

**False.** Go's garbage collector is **non-moving** (non-compacting). Objects stay at their original memory address for their entire lifetime.

This design choice has significant implications:

**Advantages of non-moving GC:**
- Interior pointers are safe (you can point into the middle of an object).
- `unsafe.Pointer` and cgo interop are simpler -- C code can hold pointers to Go objects without pinning.
- No need for read/write barriers to handle pointer forwarding (moved objects).
- Simpler implementation with predictable performance.

**Disadvantages:**
- Fragmentation can occur over time (though size classes and spans mitigate this).
- Cannot achieve the high heap density that compacting collectors provide.
- Large objects that die leave "holes" that can only be filled by same-size-class allocations.

Go mitigates fragmentation through its size-class-based span allocator and by returning unused pages to the OS via `madvise(MADV_DONTNEED)` (scavenging). The one exception is goroutine stacks, which ARE moved (copied) when they grow, but this is handled by `copystack`, not by the GC.

</details>
