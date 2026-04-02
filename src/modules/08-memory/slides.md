# Module 8: Memory Management

## Slides

---

## Slide 1: Memory Management in Go

Three layers of memory management:

1. **Hardware**: virtual memory, page tables, TLB
2. **OS kernel**: `mmap`, page fault handler, physical frame allocator
3. **Go runtime**: allocator (tcmalloc-derived), garbage collector

This module: how Go's runtime sits on top of the OS to provide automatic,
efficient memory management.

---

## Slide 2: Virtual Memory Recap

```
Virtual Address Space          Physical Memory
+------------------+         +------------------+
| Stack            |         | Frame 0          |
+------------------+    +--->| Frame 1          |
| ...              |    |    +------------------+
+------------------+    |    | Frame 2          |<---+
| Heap (mmap)      |----+    +------------------+    |
+------------------+         | ...              |    |
| Data/BSS         |---------+------------------+    |
+------------------+         | Frame N          |    |
| Text             |-------->+------------------+    |
+------------------+                                  |
                                Page Table            |
                              maps virtual to ---------+
                              physical via TLB
```

- **Page**: 4 KB unit of virtual memory
- **Page table**: per-process, maps virtual pages to physical frames
- **TLB**: hardware cache of recent translations
- **mmap**: syscall to request virtual pages from the kernel

---

## Slide 3: Go Gets Memory from the OS

```
Go runtime                    Linux kernel
+------------+               +-------------+
| mheap      | -- mmap() --> | VMA         |
|            |               | (lazy:      |
|            | <-- pages --- |  zero-fill  |
|            |               |  on fault)  |
+------------+               +-------------+
```

- `mmap(nil, size, PROT_READ|PROT_WRITE, MAP_ANON|MAP_PRIVATE, ...)`
- Returns virtual address space; pages are zero-filled on first access
- Go allocates in **arena-sized** chunks: 64 MB on 64-bit systems
- At least 1 MB per OS request (amortizes syscall overhead)

---

## Slide 4: Allocator Architecture Overview

```go
// src/runtime/malloc.go, lines 17-25
// The allocator's data structures are:
//
//  fixalloc: a free-list allocator for fixed-size off-heap objects,
//      used to manage storage used by the allocator.
//  mheap: the malloc heap, managed at page (8192-byte) granularity.
//  mspan: a run of in-use pages managed by the mheap.
//  mcentral: collects all spans of a given size class.
//  mcache: a per-P cache of mspans with free space.
//  mstats: allocation statistics.
```

---

## Slide 5: The Allocation Hierarchy

```go
// src/runtime/malloc.go, lines 27-48
// Allocating a small object proceeds up a hierarchy of caches:
//
//  1. Round the size up to one of the small size classes
//     and look in the corresponding mspan in this P's mcache.
//     Scan the mspan's free bitmap to find a free slot.
//     This can all be done without acquiring a lock.
//
//  2. If the mspan has no free slots, obtain a new mspan
//     from the mcentral's list of mspans of the required size class.
//     Obtaining a whole span amortizes the cost of locking
//     the mcentral.
//
//  3. If the mcentral's mspan list is empty, obtain a run
//     of pages from the mheap to use for the mspan.
//
//  4. If the mheap is empty or has no page runs large enough,
//     allocate a new group of pages (at least 1MB) from the
//     operating system.
```

---

## Slide 6: The Three Allocation Paths

```
new(T) --> mallocgc(size, typ, needzero)
              |
              +-- size < 16B, no pointers?
              |   --> TINY allocator (bump pointer, per-P)
              |
              +-- size <= 32KB?
              |   --> SMALL: mcache -> mcentral -> mheap -> OS
              |
              +-- size > 32KB?
                  --> LARGE: mheap directly -> OS
```

**Key insight:** Most allocations (tiny + small) never touch a lock.

---

## Slide 7: The Tiny Allocator

```go
// src/runtime/malloc.go, lines 1270-1298
// Tiny allocator combines several tiny allocation requests
// into a single memory block. The resulting memory block
// is freed when all subobjects are unreachable. The subobjects
// must be noscan (don't have pointers), this ensures that
// the amount of potentially wasted memory is bounded.
//
// Current setting is 16 bytes, which relates to 2x worst case
// memory wastage.
//
// The main targets of tiny allocator are small strings and
// standalone escaping variables. On a json benchmark
// the allocator reduces number of allocations by ~12% and
// reduces heap size by ~20%.
```

---

## Slide 8: Tiny Allocator Implementation

```go
// src/runtime/malloc.go, lines 1299-1324
c := getMCache(mp)
off := c.tinyoffset
// Align tiny pointer for required alignment.
if size&7 == 0 {
    off = alignUp(off, 8)
} else if size&3 == 0 {
    off = alignUp(off, 4)
} else if size&1 == 0 {
    off = alignUp(off, 2)
}
if off+size <= maxTinySize && c.tiny != 0 {
    // The object fits into existing tiny block.
    x := unsafe.Pointer(c.tiny + off)
    c.tinyoffset = off + size
    c.tinyAllocs++
    return x, 0
}
// Allocate a new maxTinySize block.
span := c.alloc[tinySpanClass]
```

Bump-pointer allocation within 16-byte blocks. No lock needed.

---

## Slide 9: Size Classes

- ~70 size classes: 8, 16, 24, 32, 48, 64, ..., 32768 bytes
- Each size class has a **span class** (also encodes scan/noscan)
- Eliminates external fragmentation for small objects
- Internal fragmentation bounded by class spacing

```
Request    Size Class    Waste
  1 B   ->   8 B        7 B (87%)
 12 B   ->  16 B        4 B (25%)
 25 B   ->  32 B        7 B (22%)
 33 B   ->  48 B       15 B (31%)
```

OS parallel: Linux SLAB allocator uses the same approach.

---

## Slide 10: mspan -- A Run of Pages

```go
// src/runtime/mheap.go, lines 422-516
type mspan struct {
    startAddr uintptr // address of first byte
    npages    uintptr // number of pages in span

    freeindex  uint16   // where to start scanning for free
    nelems     uint16   // number of objects in the span
    allocCache uint64   // complement of allocBits (for ctz)

    allocBits  *gcBits  // 1 = allocated, 0 = free
    gcmarkBits *gcBits  // 1 = marked, 0 = unmarked

    sweepgen   uint32
    allocCount uint16   // number of allocated objects
    spanclass  spanClass
    state      mSpanStateBox
    elemsize   uintptr
}
```

---

## Slide 11: mspan Allocation Bitmaps

```
mspan for size class 32 (4 pages = 32KB = 1024 objects)

allocBits:  [1 1 0 1 1 0 0 1 0 0 ...]
              ^allocated    ^free

gcmarkBits: [1 1 0 1 0 0 0 1 0 0 ...]  (set during mark phase)

After sweep: allocBits := gcmarkBits
             gcmarkBits := zeros

New allocBits: [1 1 0 1 0 0 0 1 0 0 ...]
                          ^ newly freed (was allocated, not marked)
```

- `allocCache` = complement of 64 bits of `allocBits` at `freeindex`
- `ctz(allocCache)` finds first free slot in one instruction

---

## Slide 12: mcache -- Per-P, Lock-Free

```go
// src/runtime/mcache.go, lines 14-66
// Per-thread (in Go, per-P) cache for small objects.
// No locking needed because it is per-thread (per-P).
type mcache struct {
    // Tiny allocator state
    tiny       uintptr
    tinyoffset uintptr
    tinyAllocs uintptr

    // alloc contains spans to allocate from, indexed by spanClass.
    alloc [numSpanClasses]*mspan  // ~140 entries

    flushGen atomic.Uint32
}
```

- One `mcache` per P (logical processor)
- `alloc[spanClass]` -> current span for each size class
- Allocation = bitmap scan in cached span (no lock!)
- When span exhausted: get new span from `mcentral` (lock)

---

## Slide 13: mcentral and mheap

```
mcache (per-P, no lock)
    |  span exhausted
    v
mcentral (per-size-class, locked)
    |  no spans available
    v
mheap (global, locked)
    |  no pages
    v
OS mmap (syscall)
```

**mcentral**: one per span class. Maintains lists of spans with free objects.

**mheap**: single global heap. Manages page-level allocation. Contains the arena map.

```go
// src/runtime/mheap.go, lines 211-214
central [numSpanClasses]struct {
    mcentral mcentral
    pad      [...]byte  // cache line padding to avoid false sharing
}
```

---

## Slide 14: Arena Layout

```go
// src/runtime/malloc.go, lines 77-94
// The heap consists of a set of arenas, which are 64MB on 64-bit.
// Each arena has an associated heapArena object that stores the
// metadata for that arena: the heap bitmap for all words in the arena
// and the span map for all pages in the arena.
```

```
Virtual address space:
|--Arena 0 (64MB)--|--Arena 1 (64MB)--|--Arena 2 (64MB)--|...

mheap_.arenas: two-level map
arenas[L1][L2] -> *heapArena

heapArena:
  spans[pagesPerArena]  -> *mspan  (page -> span lookup)
  pageInUse[...]        -> bitmap (which pages have in-use spans)
  pageMarks[...]        -> bitmap (which spans have marked objects)
```

OS parallel: this is user-space page tables!

---

## Slide 15: Lazy Zeroing

```go
// src/runtime/malloc.go, lines 65-75
// If mspan.needzero is false, then free object slots in the mspan are
// already zeroed. Otherwise if needzero is true, objects are zeroed as
// they are allocated. There are various benefits to delaying zeroing:
//
//  1. Stack frame allocation can avoid zeroing altogether.
//  2. It exhibits better temporal locality, since the program is
//     probably about to write to the memory.
//  3. We don't zero pages that never get reused.
```

- Fresh mmap pages are kernel-zeroed (lazy, via zero page + COW)
- Runtime tracks `needzero` per span
- Zero on allocation, not on free
- Cooperates with OS page allocator

---

## Slide 16: Garbage Collection -- Overview

```go
// src/runtime/mgc.go, lines 5-10
// The GC runs concurrently with mutator threads, is type accurate,
// allows multiple GC threads to run in parallel. It is a concurrent
// mark and sweep that uses a write barrier. It is non-generational
// and non-compacting.
```

Properties:
- **Concurrent**: mark and sweep run alongside application
- **Precise**: knows exactly which words are pointers (type-accurate)
- **Parallel**: multiple GC worker goroutines
- **Non-generational**: no young/old distinction
- **Non-compacting**: objects never move (simplifies FFI, unsafe)

---

## Slide 17: The Four GC Phases

```
Phase 1: Sweep Termination (STW)
  - Stop all Ps at safe-points
  - Finish sweeping any remaining spans

Phase 2: Mark (CONCURRENT)
  - Enable write barrier
  - Mark workers scan roots (stacks, globals)
  - Drain grey object queue
  - Mutator assists: allocating goroutines help mark

Phase 3: Mark Termination (STW)
  - Stop the world
  - Disable workers and assists
  - Flush mcaches

Phase 4: Sweep (CONCURRENT)
  - Disable write barrier
  - Sweep spans lazily (on allocation) or by background sweeper
  - swap allocBits <-> gcmarkBits
```

STW pauses are typically < 1ms.

---

## Slide 18: Tri-Color Marking

```
WHITE = not visited (presumed garbage)
GREY  = visited, not yet scanned (in work queue)
BLACK = scanned (all pointers followed)

Start:  all objects WHITE, roots are GREY
        +---+    +---+    +---+
        | G |    | G |    | W |
        +---+    +---+    +---+
          |        |
          v        v
        +---+    +---+
        | W |    | W |
        +---+    +---+

End:    reachable = BLACK, unreachable = WHITE
        +---+    +---+    +---+
        | B |    | B |    | W | <-- garbage
        +---+    +---+    +---+
          |        |
          v        v
        +---+    +---+
        | B |    | B |
        +---+    +---+
```

**Invariant:** No BLACK object points to a WHITE object.

---

## Slide 19: Write Barriers

**Problem:** Mutator runs concurrently with marker. What if:
1. Marker scans object A (BLACK), seeing pointer to B
2. Mutator writes `A.ptr = C` (C is WHITE)
3. Mutator clears only reference to C from grey objects
4. Marker finishes -- C is WHITE, freed. **Dangling pointer!**

**Solution:** Write barrier intercepts pointer writes during mark phase.

```go
// From mgc.go, line 42:
// The write barrier shades both the overwritten pointer and the new
// pointer value for any pointer writes.
```

Go's hybrid barrier (Dijkstra + Yuasa):
- Shade the **old** pointer value (Yuasa: deletion barrier)
- Shade the **new** pointer value (Dijkstra: insertion barrier)
- Newly allocated objects are marked BLACK immediately

---

## Slide 20: GC Pacing and Triggers

```go
// src/runtime/mgc.go, lines 112-118
// Next GC is after we've allocated an extra amount of memory
// proportional to the amount already in use. The proportion is
// controlled by GOGC environment variable (100 by default).
// If GOGC=100 and we're using 4M, we'll GC again when we get to 8M.
// This keeps the GC cost in linear proportion to the allocation cost.
```

```
Live heap after GC: 4MB
GOGC=100 -> goal = 4MB * (1 + 100/100) = 8MB
GOGC=50  -> goal = 4MB * (1 + 50/100)  = 6MB
GOGC=200 -> goal = 4MB * (1 + 200/100) = 12MB
```

**GC assist:** goroutines that allocate heavily must help with marking.
Proportional to bytes allocated -- prevents mutator from outrunning GC.

**GOMEMLIMIT:** soft memory cap. GC triggers more aggressively near limit.

---

## Slide 21: Concurrent Sweep

```go
// src/runtime/mgc.go, lines 84-110
// The sweep phase proceeds concurrently with normal program execution.
// The heap is swept span-by-span both lazily (when a goroutine needs
// another span) and concurrently in a background goroutine.
```

```go
// src/runtime/mheap.go, lines 494-500
// sweep generation:
// if sweepgen == h->sweepgen - 2, the span needs sweeping
// if sweepgen == h->sweepgen - 1, the span is currently being swept
// if sweepgen == h->sweepgen, the span is swept and ready to use
```

- **Background sweeper**: goroutine sweeps spans one-by-one
- **Lazy sweep**: allocator sweeps before requesting more memory
- **sweepgen counter**: avoids boolean race conditions

---

## Slide 22: Sweep Mechanics

```
Before sweep:
  allocBits:    [1 1 0 1 1 0 0 1]  (current allocation state)
  gcmarkBits:   [1 0 0 1 0 0 0 1]  (marked during GC)

After sweep:
  allocBits  := gcmarkBits   -->  [1 0 0 1 0 0 0 1]
  gcmarkBits := zeros        -->  [0 0 0 0 0 0 0 0]

  Objects at indices 1 and 4 were allocated but not marked:
  they are now free.
```

- Bulk free: just swap bitmap pointers
- No per-object free() call needed
- `allocCount` updated from popcount of new `allocBits`

---

## Slide 23: The Complete Allocation Path

```
mallocgc(size, typ, needzero)
  |
  +-- size == 0 --> return &zerobase
  |
  +-- GC assist: deductAssistCredit(size)
  |
  +-- size < 16, no ptrs --> mallocgcTiny
  |     bump pointer in 16B block (mcache.tiny)
  |
  +-- size <= 32KB --> mallocgcSmall{Noscan,ScanHeader}
  |     1. mcache.alloc[sizeClass].nextFreeFast() -- ctz on allocCache
  |     2. mcache.nextFree() -- refill from mcentral
  |     3. mcentral grows from mheap
  |     4. mheap.sysAlloc -> mmap
  |
  +-- size > 32KB --> mallocgcLarge
        allocate pages directly from mheap
```

---

## Slide 24: OS Concepts Mapping

| Go Runtime | OS Kernel Equivalent |
|-----------|---------------------|
| mcache (per-P) | Per-CPU SLAB cache |
| mcentral | Global SLAB cache |
| mheap | Buddy allocator / page allocator |
| mmap | Physical frame allocator |
| Arena map | Page table (multi-level) |
| Size classes | SLAB object caches |
| GC mark | - (no equivalent; kernel doesn't GC) |
| Write barrier | Dirty bit / COW page fault |
| Lazy zeroing | Demand paging / zero page |
| sweepgen counter | Epoch-based reclamation |

---

## Slide 25: Key Takeaways

1. **Hierarchy minimizes contention**: per-P cache -> per-class central -> global heap -> OS

2. **Size classes eliminate fragmentation** for small objects (same trick as SLAB)

3. **Bitmaps enable bulk operations**: allocation via `ctz`, sweep via bitmap swap

4. **Concurrent GC** keeps STW pauses < 1ms using:
   - Write barriers for correctness
   - Tri-color invariant
   - GC assists for pacing

5. **Runtime cooperates with OS**: lazy zeroing, arena-aligned mmap, page-granularity management

6. **No compaction** means simpler FFI but potential fragmentation for large heaps
