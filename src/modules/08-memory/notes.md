# Module 8: Memory Management (60 min)

## Background: Memory Management from Hardware to Runtime

Memory management is a problem that spans every layer of a computer system, from
the hardware up to application code. At the bottom, the CPU's memory management
unit (MMU) translates virtual addresses to physical addresses using page tables
-- a mechanism first pioneered by the Atlas computer at the University of
Manchester in 1962, which introduced the concept of "one-level storage" where
programs could address more memory than physically existed. That idea became
virtual memory, and by the 1970s it was universal. Modern systems use multi-level
page tables (four or five levels on x86-64) to map a 48- or 57-bit virtual
address space, with the TLB caching recent translations to avoid the cost of a
full page table walk on every memory access. The page -- typically 4 KB -- is the
fundamental unit of this translation, though huge pages (2 MB or 1 GB on x86)
reduce TLB pressure for large working sets.

The operating system kernel manages the page tables and physical memory.
Linux uses a **buddy allocator** to manage physical page frames: it maintains
free lists of power-of-two-sized page blocks and splits or coalesces them as
needed, providing O(log n) allocation with low external fragmentation. On top of
the buddy allocator sits the **SLUB allocator** (the successor to the original
slab allocator designed by Jeff Bonwick at Sun in 1994), which carves pages into
fixed-size object caches for frequently allocated kernel structures like inodes,
dentries, and socket buffers. SLUB maintains per-CPU freelists to minimize lock
contention -- a design principle we will see repeated at every level of the
stack. The kernel exposes memory to user space through system calls like `mmap`,
which creates virtual address mappings that are backed by physical pages lazily,
on first access (demand paging). Transparent Huge Pages (THP) allow the kernel
to automatically promote 4 KB pages to 2 MB huge pages, improving TLB
utilization for large allocations without requiring application changes.

For user-space programs, calling `mmap` for every allocation would be far too
expensive -- each call requires a transition to kernel mode and manipulation of
page table entries. User-space allocators like **dlmalloc** (Doug Lea's malloc,
the basis of glibc's allocator), **jemalloc** (developed for FreeBSD and later
adopted by Facebook), **tcmalloc** (Google's Thread-Caching Malloc), and
**mimalloc** (Microsoft Research's compact allocator) all solve this by
requesting large chunks of memory from the OS and then sub-dividing them
efficiently. These allocators face a common set of challenges: minimizing
fragmentation (both *internal* fragmentation from rounding up to size classes and
*external* fragmentation from non-contiguous free blocks), reducing lock
contention in multi-threaded programs (typically through per-thread or per-CPU
caches), and maintaining good cache locality. tcmalloc's key innovation was its
hierarchy of thread-local caches, central freelists, and a global page heap --
a design that directly inspired Go's memory allocator.

Where manual allocators require the programmer to pair every allocation with an
explicit free, **garbage collection** automates memory reclamation by
identifying objects that are no longer reachable. The idea dates back to John
McCarthy's Lisp in 1960, which used a simple stop-the-world mark-and-sweep
collector. Since then, GC research has produced a rich taxonomy of approaches:
**reference counting** (tracking how many pointers refer to each object, used by
CPython and Swift), **generational collection** (exploiting the observation that
most objects die young, pioneered by David Ungar in 1984), **copying/compacting
collectors** (which eliminate fragmentation by relocating live objects), and
**concurrent/incremental collectors** (which minimize pause times by doing GC
work alongside the application). Java's ecosystem showcases this diversity: its
**G1** collector partitions the heap into regions for incremental collection,
**ZGC** uses colored pointers and load barriers to achieve sub-millisecond
pauses on multi-terabyte heaps, and **Shenandoah** performs concurrent
compaction using Brooks forwarding pointers. On modern NUMA systems, allocators
must also be topology-aware -- allocating memory on the same NUMA node as the
thread that will use it, since cross-node accesses can be 2-3x slower.

Go's runtime sits at the intersection of these ideas. Its memory allocator
descends from tcmalloc but has diverged significantly, using a hierarchy of
per-P caches (`mcache`), per-size-class central lists (`mcentral`), and a global
heap (`mheap`) backed by `mmap`. Its garbage collector is a concurrent,
tri-color mark-and-sweep design -- non-generational and non-compacting, but with
sub-millisecond STW pauses achieved through concurrent marking with a hybrid
write barrier. In this module, we trace an allocation from `new(T)` all the way
down to `mmap`, then examine how the garbage collector reclaims memory
concurrently with the running program.

---

## 1. Virtual Memory Review

### Pages and page tables

Modern CPUs use virtual memory to provide each process with an isolated address
space. The hardware translates virtual addresses to physical addresses using
**page tables**, with a typical page size of 4 KB (though large/huge pages of
2 MB or 1 GB exist).

Key concepts:
- **Virtual address space**: each process sees a flat, contiguous address space.
- **Page table**: a per-process data structure (managed by the OS kernel) mapping
  virtual page numbers to physical frame numbers.
- **TLB (Translation Lookaside Buffer)**: a hardware cache of recent page table
  entries. TLB misses trigger a page table walk.
- **Page fault**: when a virtual page has no valid mapping, the CPU traps to the
  kernel, which may allocate a physical frame, read from disk, or deliver a
  segfault.

### mmap: Requesting memory from the OS

User-space programs request virtual memory from the kernel using `mmap`:

```c
void *mmap(void *addr, size_t length, int prot, int flags, int fd, off_t offset);
```

With `MAP_ANONYMOUS | MAP_PRIVATE`, `mmap` creates a new anonymous mapping --
virtual pages backed by physical memory (lazily, on first access). The Go
runtime uses this as its sole interface to the OS for heap memory.

---

## 2. Go's Memory Allocator Architecture

The allocator is documented at the top of `malloc.go`:

[`src/runtime/malloc.go` lines 5-25](https://cs.opensource.google/go/go/+/refs/tags/go1.26.1:src/runtime/malloc.go;l=5):

```go
// src/runtime/malloc.go, lines 5-25

// Memory allocator.
//
// This was originally based on tcmalloc, but has diverged quite a bit.
// http://goog-perftools.sourceforge.net/doc/tcmalloc.html

// The main allocator works in runs of pages.
// Small allocation sizes (up to and including 32 kB) are
// rounded to one of about 70 size classes, each of which
// has its own free set of objects of exactly that size.
// Any free page of memory can be split into a set of objects
// of one size class, which are then managed using a free bitmap.
//
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

### The allocation hierarchy

[`src/runtime/malloc.go` lines 27-63](https://cs.opensource.google/go/go/+/refs/tags/go1.26.1:src/runtime/malloc.go;l=27):

```
// src/runtime/malloc.go, lines 27-63

// Allocating a small object proceeds up a hierarchy of caches:
//
//  1. Round the size up to one of the small size classes
//     and look in the corresponding mspan in this P's mcache.
//     Scan the mspan's free bitmap to find a free slot.
//     If there is a free slot, allocate it.
//     This can all be done without acquiring a lock.
//
//  2. If the mspan has no free slots, obtain a new mspan
//     from the mcentral's list of mspans of the required size
//     class that have free space.
//     Obtaining a whole span amortizes the cost of locking
//     the mcentral.
//
//  3. If the mcentral's mspan list is empty, obtain a run
//     of pages from the mheap to use for the mspan.
//
//  4. If the mheap is empty or has no page runs large enough,
//     allocate a new group of pages (at least 1MB) from the
//     operating system. Allocating a large run of pages
//     amortizes the cost of talking to the operating system.
```

### Deallocation (sweeping) hierarchy

[`src/runtime/malloc.go` lines 49-63](https://cs.opensource.google/go/go/+/refs/tags/go1.26.1:src/runtime/malloc.go;l=49):

```
// src/runtime/malloc.go, lines 49-63

// Sweeping an mspan and freeing objects on it proceeds up a similar
// hierarchy:
//
//  1. If the mspan is being swept in response to allocation, it
//     is returned to the mcache to satisfy the allocation.
//
//  2. Otherwise, if the mspan still has allocated objects in it,
//     it is placed on the mcentral free list for the mspan's size
//     class.
//
//  3. Otherwise, if all objects in the mspan are free, the mspan's
//     pages are returned to the mheap and the mspan is now dead.
//
// Allocating and freeing a large object uses the mheap
// directly, bypassing the mcache and mcentral.
```

### Diagram: Allocation hierarchy

```
  Goroutine calls new(T)
        |
        v
  mallocgc(size, typ, needzero)
        |
        +------ size == 0? -----> return &zerobase
        |
        +------ size < 16 bytes, no pointers? -----> Tiny allocator
        |
        +------ size <= 32 KB? -----> Small object path:
        |           |
        |           v
        |       mcache (per-P, no lock)
        |           |  no free slot?
        |           v
        |       mcentral (per-size-class, locked)
        |           |  no spans available?
        |           v
        |       mheap (global, locked)
        |           |  no pages?
        |           v
        |       mmap(OS)
        |
        +------ size > 32 KB? -----> Large object path:
                    |
                    v
                mheap directly -> mmap if needed
```

---

## 3. Virtual Memory Layout: Arenas

Go organizes its heap into **arenas**:

[`src/runtime/malloc.go` lines 79-94](https://cs.opensource.google/go/go/+/refs/tags/go1.26.1:src/runtime/malloc.go;l=79):

```go
// src/runtime/malloc.go, lines 79-94

// The heap consists of a set of arenas, which are 64MB on 64-bit and
// 4MB on 32-bit (heapArenaBytes). Each arena's start address is also
// aligned to the arena size.
//
// Each arena has an associated heapArena object that stores the
// metadata for that arena: the heap bitmap for all words in the arena
// and the span map for all pages in the arena. heapArena objects are
// themselves allocated off-heap.
//
// Since arenas are aligned, the address space can be viewed as a
// series of arena frames. The arena map (mheap_.arenas) maps from
// arena frame number to *heapArena, or nil for parts of the address
// space not backed by the Go heap. The arena map is structured as a
// two-level array consisting of a "L1" arena map and many "L2" arena
// maps; however, since arenas are large, on many architectures, the
// arena map consists of a single, large L2 map.
```

The two-level arena map allows the runtime to cover the entire 48-bit virtual
address space without allocating metadata for unused regions:

[`src/runtime/mheap.go` line 150](https://cs.opensource.google/go/go/+/refs/tags/go1.26.1:src/runtime/mheap.go;l=150):

```go
// src/runtime/mheap.go, line 150
arenas [1 << arenaL1Bits]*[1 << arenaL2Bits]*heapArena
```

Each `heapArena` stores metadata for one 64 MB region:

[`src/runtime/mheap.go` lines 268-338](https://cs.opensource.google/go/go/+/refs/tags/go1.26.1:src/runtime/mheap.go;l=268):

```go
// src/runtime/mheap.go, lines 268-338

type heapArena struct {
    _ sys.NotInHeap

    // spans maps from virtual address page ID within this arena to *mspan.
    // For allocated spans, their pages map to the span itself.
    // For free spans, only the lowest and highest pages map to the span itself.
    spans [pagesPerArena]*mspan

    // pageInUse is a bitmap that indicates which spans are in
    // state mSpanInUse.
    pageInUse [pagesPerArena / 8]uint8

    // pageMarks is a bitmap that indicates which spans have any
    // marked objects on them.
    pageMarks [pagesPerArena / 8]uint8

    // zeroedBase marks the first byte of the first page in this
    // arena which hasn't been used yet and is therefore already
    // zero.
    zeroedBase uintptr
}
```

**OS connection:** The arena system is Go's user-space equivalent of a page
table. Just as the OS kernel uses a multi-level page table to map virtual
addresses to physical frames, Go uses a two-level arena map to map heap
addresses to span metadata.

---

## 4. The mheap Struct: The Central Heap

The `mheap` is the single global heap structure:

[`src/runtime/mheap.go` lines 64-264](https://cs.opensource.google/go/go/+/refs/tags/go1.26.1:src/runtime/mheap.go;l=64):

```go
// src/runtime/mheap.go, lines 64-264

type mheap struct {
    _ sys.NotInHeap

    // lock must only be acquired on the system stack, otherwise a g
    // could self-deadlock if its stack grows with the lock held.
    lock mutex

    pages pageAlloc // page allocation data structure

    sweepgen uint32 // sweep generation, see comment in mspan

    // allspans is a slice of all mspans ever created.
    allspans []*mspan // all spans out there

    // ...

    // arenas is the heap arena map.
    arenas [1 << arenaL1Bits]*[1 << arenaL2Bits]*heapArena

    // central free lists for small size classes.
    central [numSpanClasses]struct {
        mcentral mcentral
        pad      [...]byte  // cache line padding
    }

    spanalloc  fixalloc // allocator for span
    cachealloc fixalloc // allocator for mcache
    // ...
}

var mheap_ mheap
```

### Key fields

| Field | Purpose |
|-------|---------|
| `lock` | Global lock for heap operations. Must be acquired on system stack. |
| `pages` | Page allocator -- manages free/scavenged pages. |
| `sweepgen` | Sweep generation counter (incremented by 2 each GC cycle). |
| `allspans` | Master list of all `mspan` objects ever created. |
| `arenas` | Two-level map from address to `heapArena` metadata. |
| `central` | Array of `mcentral` structures, one per span class. Cache-line padded. |
| `spanalloc` | Fixed-size allocator for `mspan` structs (off-heap). |

**Design note:** The `central` array is padded to cache line boundaries to avoid
false sharing between different size classes being accessed by different
processors.

---

## 5. Size Classes and mspan

### Size classes

Go rounds small allocations up to one of approximately 70 **size classes**. For
example: 8, 16, 24, 32, 48, 64, 80, 96, ..., 32768 bytes. Each size class has
a corresponding **span class** (which also encodes whether the span contains
pointers or not -- "noscan" vs. "scan").

This eliminates external fragmentation for small objects: all objects within a
span are the same size, so any free slot can satisfy any request of that class.

### The mspan struct

An `mspan` represents a contiguous run of pages dedicated to one size class:

[`src/runtime/mheap.go` lines 422-516](https://cs.opensource.google/go/go/+/refs/tags/go1.26.1:src/runtime/mheap.go;l=422):

```go
// src/runtime/mheap.go, lines 422-516

type mspan struct {
    _    sys.NotInHeap
    next *mspan     // next span in list, or nil if none
    prev *mspan     // previous span in list, or nil if none
    list *mSpanList // For debugging.

    startAddr uintptr // address of first byte of span aka s.base()
    npages    uintptr // number of pages in span

    manualFreeList gclinkptr // list of free objects in mSpanManual spans

    // freeindex is the slot index between 0 and nelems at which to begin
    // scanning for the next free object in this span.
    // Each allocation scans allocBits starting at freeindex until it
    // encounters a 0 indicating a free object.
    freeindex uint16

    nelems uint16 // number of objects in the span

    freeIndexForScan uint16

    // Cache of the allocBits at freeindex. allocCache is shifted
    // such that the lowest bit corresponds to the bit freeindex.
    // allocCache holds the complement of allocBits, thus allowing
    // ctz (count trailing zero) to use it directly.
    allocCache uint64

    // allocBits and gcmarkBits hold pointers to a span's mark and
    // allocation bits.
    allocBits  *gcBits
    gcmarkBits *gcBits
    pinnerBits *gcBits // bitmap for pinned objects

    // sweep generation:
    // if sweepgen == h->sweepgen - 2, the span needs sweeping
    // if sweepgen == h->sweepgen - 1, the span is currently being swept
    // if sweepgen == h->sweepgen, the span is swept and ready to use
    // if sweepgen == h->sweepgen + 1, the span was cached before sweep
    //   began and is still cached, and needs sweeping
    // if sweepgen == h->sweepgen + 3, the span was swept and then cached
    //   and is still cached
    sweepgen              uint32
    divMul                uint32        // for divide by elemsize
    allocCount            uint16        // number of allocated objects
    spanclass             spanClass     // size class and noscan (uint8)
    state                 mSpanStateBox // mSpanInUse etc; accessed atomically
    needzero              uint8         // needs to be zeroed before allocation
    allocCountBeforeCache uint16
    elemsize              uintptr       // computed from sizeclass or from npages
    limit                 uintptr       // end of data in span
    speciallock           mutex         // guards specials list
    specials              *special      // linked list of special records
    largeType             *_type        // malloc header for large objects
}
```

### Allocation bitmaps

Each span has two key bitmaps:

- **`allocBits`**: A bit per object slot. 1 = allocated, 0 = free.
- **`gcmarkBits`**: A bit per object slot. 1 = marked (reachable), 0 = unmarked.

After GC, the sweep phase replaces `allocBits` with `gcmarkBits` (objects that
were not marked are now free), and `gcmarkBits` is reset to all zeros for the
next cycle. This is a very efficient way to free dead objects in bulk.

The `allocCache` field is an optimization: it caches 64 bits of the complement
of `allocBits` starting at `freeindex`, allowing the allocator to find a free
slot using a single `ctz` (count trailing zeros) CPU instruction.

### Span states

[`src/runtime/mheap.go` lines 386-390](https://cs.opensource.google/go/go/+/refs/tags/go1.26.1:src/runtime/mheap.go;l=386):

```go
// src/runtime/mheap.go, lines 386-390
const (
    mSpanDead   mSpanState = iota
    mSpanInUse             // allocated for garbage collected heap
    mSpanManual            // allocated for manual management (e.g., stack allocator)
)
```

Transitions between states are constrained by the GC phase to maintain
correctness of concurrent marking.

---

## 6. mcache: Per-P Lock-Free Allocation

The `mcache` is the first level of the allocation hierarchy. There is one per P
(logical processor), so allocations from it require **no locking**:

[`src/runtime/mcache.go` lines 14-66](https://cs.opensource.google/go/go/+/refs/tags/go1.26.1:src/runtime/mcache.go;l=14):

```go
// src/runtime/mcache.go, lines 14-66

// Per-thread (in Go, per-P) cache for small objects.
// This includes a small object cache and local allocation stats.
// No locking needed because it is per-thread (per-P).
//
// mcaches are allocated from non-GC'd memory, so any heap pointers
// must be specially handled.
type mcache struct {
    _ sys.NotInHeap

    // The following members are accessed on every malloc,
    // so they are grouped here for better caching.
    nextSample  int64   // trigger heap sample after allocating this many bytes
    memProfRate int     // cached mem profile rate
    scanAlloc   uintptr // bytes of scannable heap allocated

    // Allocator cache for tiny objects w/o pointers.
    // See "Tiny allocator" comment in malloc.go.
    tiny       uintptr
    tinyoffset uintptr
    tinyAllocs uintptr

    // The rest is not accessed on every malloc.

    // alloc contains spans to allocate from, indexed by spanClass.
    alloc [numSpanClasses]*mspan

    stackcache [_NumStackOrders]stackfreelist

    // flushGen indicates the sweepgen during which this mcache
    // was last flushed.
    flushGen atomic.Uint32
}
```

### How mcache allocation works

1. Look up `mcache.alloc[spanClass]` to get the current `mspan` for this size
   class.
2. Use `allocCache` + `ctz` to find a free slot in constant time.
3. If no free slots, call `mcache.nextFree()` which gets a new span from
   `mcentral`.

The `alloc` array has `numSpanClasses` entries (approximately 140: ~70 size
classes x 2 for scan/noscan). Each entry points to an `mspan` with free slots.

**OS connection:** This per-CPU caching strategy is the same principle used in
Linux's SLAB/SLUB allocator, which maintains per-CPU freelists to avoid lock
contention on the global heap.

---

## 7. The Tiny Allocator

For very small allocations (< 16 bytes) that do not contain pointers, Go uses a
special **tiny allocator** that packs multiple objects into a single 16-byte
block:

[`src/runtime/malloc.go` lines 1270-1298](https://cs.opensource.google/go/go/+/refs/tags/go1.26.1:src/runtime/malloc.go;l=1270):

```go
// src/runtime/malloc.go, lines 1270-1298

// Tiny allocator.
//
// Tiny allocator combines several tiny allocation requests
// into a single memory block. The resulting memory block
// is freed when all subobjects are unreachable. The subobjects
// must be noscan (don't have pointers), this ensures that
// the amount of potentially wasted memory is bounded.
//
// Size of the memory block used for combining (maxTinySize) is tunable.
// Current setting is 16 bytes, which relates to 2x worst case memory
// wastage (when all but one subobjects are unreachable).
// 8 bytes would result in no wastage at all, but provides less
// opportunities for combining.
// 32 bytes provides more opportunities for combining,
// but can lead to 4x worst case wastage.
// The best case winning is 8x regardless of block size.
//
// Objects obtained from tiny allocator must not be freed explicitly.
// So when an object will be freed explicitly, we ensure that
// its size >= maxTinySize.
//
// SetFinalizer has a special case for objects potentially coming
// from tiny allocator, it such case it allows to set finalizers
// for an inner byte of a memory block.
//
// The main targets of tiny allocator are small strings and
// standalone escaping variables. On a json benchmark
// the allocator reduces number of allocations by ~12% and
// reduces heap size by ~20%.
```

### Implementation

```go
// src/runtime/malloc.go, lines 1299-1324

c := getMCache(mp)
off := c.tinyoffset
// Align tiny pointer for required (conservative) alignment.
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
    mp.mallocing = 0
    releasem(mp)
    return x, 0
}
// Allocate a new maxTinySize block.
span := c.alloc[tinySpanClass]
v := nextFreeFast(span)
if v == 0 {
    v, span, checkGCTrigger = c.nextFree(tinySpanClass)
}
```

The tiny allocator maintains a current 16-byte block (`c.tiny`) and bumps a
pointer (`c.tinyoffset`) within it. When the block is full, a new 16-byte slot
is obtained from the regular span allocator.

**Trade-off:** The entire 16-byte block cannot be freed until *all* sub-objects
within it become unreachable. This trades a small amount of memory waste for a
large reduction in allocation overhead.

---

## 8. mallocgc: The Central Allocation Function

Every heap allocation in Go flows through `mallocgc`:

```go
// src/runtime/malloc.go, lines 1119-1128

func mallocgc(size uintptr, typ *_type, needzero bool) unsafe.Pointer {
    if doubleCheckMalloc {
        if gcphase == _GCmarktermination {
            throw("mallocgc called with gcphase == _GCmarktermination")
        }
    }

    // Short-circuit zero-sized allocation requests.
    if size == 0 {
        return unsafe.Pointer(&zerobase)
    }
```

### Allocation paths

```go
// src/runtime/malloc.go, lines 1186-1200

if size <= maxSmallSize-gc.MallocHeaderSize {
    if typ == nil || !typ.Pointers() {
        // tiny allocations might be kept alive by other co-located values.
        gp := getg()
        if size < maxTinySize && gp.secret == 0 {
            x, elemsize = mallocgcTiny(size, typ)
        } else {
            x, elemsize = mallocgcSmallNoscan(size, typ, needzero)
        }
    } else {
        if !needzero {
            throw("objects with pointers must be zeroed")
        }
        x, elemsize = mallocgcSmallScanHeader(size, typ)
    }
} else {
    x, elemsize = mallocgcLarge(size, typ, needzero)
}
```

Three paths:
1. **Tiny** (`size < 16`, no pointers): bump allocator within 16-byte blocks.
2. **Small** (`size <= 32 KB`): round to size class, allocate from mcache span.
3. **Large** (`size > 32 KB`): allocate directly from mheap.

### GC assist

```go
// src/runtime/malloc.go, lines 1162-1166

// Assist the GC if needed.
if gcBlackenEnabled != 0 {
    deductAssistCredit(size)
}
```

Before allocating, the goroutine may be required to do GC marking work
proportional to the amount it is allocating. This is the **GC assist**
mechanism: it ensures that goroutines that allocate heavily also contribute to
GC work, preventing the collector from falling behind.

**OS connection:** GC assist is analogous to proportional-share scheduling --
threads that consume more resources (memory) must contribute more work (marking)
to maintain system-wide progress.

---

## 9. Lazy Zeroing

```go
// src/runtime/malloc.go, lines 65-75

// If mspan.needzero is false, then free object slots in the mspan are
// already zeroed. Otherwise if needzero is true, objects are zeroed as
// they are allocated. There are various benefits to delaying zeroing
// this way:
//
//  1. Stack frame allocation can avoid zeroing altogether.
//
//  2. It exhibits better temporal locality, since the program is
//     probably about to write to the memory.
//
//  3. We don't zero pages that never get reused.
```

Rather than zeroing memory when it is freed, Go zeros memory when it is
allocated (if needed). Pages obtained fresh from the OS via `mmap` are
guaranteed to be zeroed by the kernel, so the runtime tracks whether a span's
pages have been written to (`needzero`). This avoids redundant zeroing and
improves cache behavior.

**OS connection:** This is a direct consequence of the OS's lazy page
allocation: `mmap` returns virtual pages that are mapped to the zero page until
first write (copy-on-write). The runtime cooperates with this by tracking which
pages are "still clean."

---

## 10. Garbage Collection Overview

The Go garbage collector is described at the top of `mgc.go`:

```go
// src/runtime/mgc.go, lines 5-10

// Garbage collector (GC).
//
// The GC runs concurrently with mutator threads, is type accurate (aka
// precise), allows multiple GC threads to run in parallel. It is a
// concurrent mark and sweep that uses a write barrier. It is
// non-generational and non-compacting. Allocation is done using size
// segregated per P allocation areas to minimize fragmentation while
// eliminating locks in the common case.
```

### The Four Phases

```
// src/runtime/mgc.go, lines 24-82

// 1. GC performs sweep termination.
//    a. Stop the world. This causes all Ps to reach a GC safe-point.
//    b. Sweep any unswept spans.

// 2. GC performs the mark phase.
//    a. Prepare for the mark phase by setting gcphase to _GCmark,
//       enabling the write barrier, enabling mutator assists, and
//       enqueueing root mark jobs. No objects may be scanned until
//       all Ps have enabled the write barrier, which is accomplished
//       using STW.
//    b. Start the world. From this point, GC work is done by mark
//       workers started by the scheduler and by assists performed as
//       part of allocation.
//    c. GC performs root marking jobs. This includes scanning all
//       stacks, shading all globals, and shading any heap pointers in
//       off-heap runtime data structures.
//    d. GC drains the work queue of grey objects, scanning each grey
//       object to black and shading all pointers found in the object.
//    e. Because GC work is spread across local caches, GC uses a
//       distributed termination algorithm to detect when there are no
//       more root marking jobs or grey objects.

// 3. GC performs mark termination.
//    a. Stop the world.
//    b. Set gcphase to _GCmarktermination, and disable workers and
//       assists.
//    c. Perform housekeeping like flushing mcaches.

// 4. GC performs the sweep phase.
//    a. Prepare for the sweep phase by setting gcphase to _GCoff,
//       setting up sweep state and disabling the write barrier.
//    b. Start the world. From this point on, newly allocated objects
//       are white, and allocating sweeps spans before use if necessary.
//    c. GC does concurrent sweeping in the background and in response
//       to allocation.
```

### Phase diagram

```
   [Normal execution -- gcphase = _GCoff, sweep concurrent]
        |
        v  (GC trigger: heap growth exceeds GOGC target)
   +---------+
   | Phase 1 |  Sweep Termination (STW)
   |  STW    |  - All Ps reach safe-point
   |         |  - Sweep remaining unswept spans
   +---------+
        |
        v
   +---------+
   | Phase 2 |  Mark (CONCURRENT)
   |         |  - Write barrier enabled
   |         |  - Mark workers scan roots and heap
   |         |  - Mutator assists: allocators do marking work
   |         |  - Tri-color invariant maintained
   +---------+
        |
        v
   +---------+
   | Phase 3 |  Mark Termination (STW)
   |  STW    |  - Disable workers
   |         |  - Flush mcaches
   +---------+
        |
        v
   +---------+
   | Phase 4 |  Sweep (CONCURRENT)
   |         |  - gcphase = _GCoff
   |         |  - Write barrier disabled
   |         |  - Spans swept lazily on allocation or by background sweeper
   +---------+
        |
        v
   [Normal execution continues until next GC trigger]
```

### Stop-the-world (STW) pauses

Only phases 1 and 3 require stopping the world. These are typically very short
(sub-millisecond on modern Go). The bulk of the work -- marking and sweeping --
happens concurrently with the application.

---

## 11. Tri-Color Marking

Go uses the **tri-color abstraction** for its concurrent mark phase:

- **White**: Not yet seen. Presumed garbage at end of marking.
- **Grey**: Seen but not yet scanned. In the work queue.
- **Black**: Scanned. All pointers from this object have been traced.

### The tri-color invariant

**Strong tri-color invariant:** No black object may point to a white object.

This invariant must be maintained even as the mutator (application code) runs
concurrently with the marker. Without it, the marker might miss reachable
objects.

### Write barriers

When the mutator writes a pointer during the mark phase, a **write barrier**
executes. Go uses a **hybrid write barrier** (Dijkstra + Yuasa) that shades
both the overwritten pointer and the new pointer value:

From `mgc.go`, line 42:
> The write barrier shades both the overwritten pointer and the new
> pointer value for any pointer writes.

This means:
- When `*p = q` is executed during GC, both the old value of `*p` and the new
  value `q` are marked grey.
- This prevents both the "lost update" problem (Dijkstra barrier) and the
  "premature free" problem (Yuasa barrier).

**OS connection:** Write barriers are conceptually similar to how OS page table
dirty bits track writes for copy-on-write or page-out decisions. Both intercept
writes to maintain metadata about memory state. In the GC case, the barrier is
implemented in software by the compiler inserting barrier code before pointer
writes.

---

## 12. Concurrent Sweep

```go
// src/runtime/mgc.go, lines 84-110

// Concurrent sweep.
//
// The sweep phase proceeds concurrently with normal program execution.
// The heap is swept span-by-span both lazily (when a goroutine needs
// another span) and concurrently in a background goroutine (this helps
// programs that are not CPU bound).
//
// To avoid requesting more OS memory while there are unswept spans,
// when a goroutine needs another span, it first attempts to reclaim
// that much memory by sweeping.
```

Sweeping is the process of examining each span's `gcmarkBits` and freeing
unmarked objects. After sweeping, the `allocBits` for the span are replaced with
the `gcmarkBits` (so marked objects become the new "allocated" set), and
`gcmarkBits` are zeroed for the next cycle.

Two mechanisms drive sweeping:
1. **Background sweeper**: A dedicated goroutine that sweeps spans one-by-one.
2. **Lazy sweeping**: When a goroutine needs a new span, it sweeps spans of the
   required size class first to try to reclaim memory before asking the heap.

The `sweepgen` field in `mspan` tracks the sweep state:

```go
// src/runtime/mheap.go, lines 494-500

// sweep generation:
// if sweepgen == h->sweepgen - 2, the span needs sweeping
// if sweepgen == h->sweepgen - 1, the span is currently being swept
// if sweepgen == h->sweepgen, the span is swept and ready to use
// if sweepgen == h->sweepgen + 1, the span was cached before sweep
//   began and is still cached, and needs sweeping
```

---

## 13. GC Triggers and Pacing

```go
// src/runtime/mgc.go, lines 112-118

// GC rate.
// Next GC is after we've allocated an extra amount of memory proportional to
// the amount already in use. The proportion is controlled by GOGC environment
// variable (100 by default). If GOGC=100 and we're using 4M, we'll GC again
// when we get to 8M (this mark is computed by the gcController.heapGoal
// method). This keeps the GC cost in linear proportion to the allocation
// cost. Adjusting GOGC just changes the linear constant
// (and also the amount of extra memory used).
```

The GC pacer maintains a **heap goal**: the target heap size at which the next
GC cycle should complete. With `GOGC=100` (default), the goal is 2x the live
heap from the previous cycle. The pacer adjusts the number of GC workers and
mutator assists to hit this target.

**GOMEMLIMIT** (added in Go 1.19) provides a soft memory limit. If set, the GC
will trigger more aggressively when approaching the limit, even if the
GOGC-based trigger hasn't fired.

**OS connection:** GC pacing is analogous to OS memory management policies like
the Linux OOM killer threshold or the page replacement daemon (kswapd) that
wakes up when free memory drops below a threshold. Both systems try to maintain
a target amount of free memory relative to total usage.

---

## 14. Summary: The Complete Picture

```
User code: x := new(MyStruct)
         |
         v
   Compiler: mallocgc(sizeof(MyStruct), type, true)
         |
         v
   +-- Tiny? (<16B, no ptrs) --> bump pointer in 16B block (per-P, no lock)
   |
   +-- Small? (<=32KB) --> mcache.alloc[sizeClass] (per-P, no lock)
   |       |  empty?
   |       v
   |   mcentral (per-size-class, locked)
   |       |  empty?
   |       v
   |   mheap (global, locked)
   |       |  no pages?
   |       v
   |   sysAlloc -> mmap(2) (kernel)
   |
   +-- Large? (>32KB) --> mheap directly
         |  no pages?
         v
     sysAlloc -> mmap(2) (kernel)

   [GC reclaims memory concurrently]
   mark phase:  trace reachable objects (grey -> black)
   sweep phase: free unreachable objects (swap allocBits/gcmarkBits)
```

---

## Discussion Questions

1. Why does Go use per-P allocation caches (`mcache`) instead of per-goroutine
   caches? Consider the number of goroutines vs. the number of Ps.

2. The tiny allocator packs multiple objects into one 16-byte block. What
   pathological case could this create? How does the "noscan" requirement
   mitigate the damage?

3. Why does Go's GC need *two* STW pauses (sweep termination and mark
   termination) instead of just one? What would go wrong with a single STW?

4. Compare Go's size-class allocator with the Linux kernel's SLAB/SLUB
   allocator. What structural similarities do you see? What are the key
   differences?

5. The `mspan.sweepgen` field uses a generation counter rather than a boolean.
   Why is a generation counter necessary for concurrent sweeping?

6. How does lazy zeroing (`needzero`) interact with `mmap`'s guarantee that new
   pages are zero-filled? What would happen if the runtime eagerly zeroed all
   freed memory?

---

## Further Reading

- Source files:
  - `/Users/tchajed/sw/go/src/runtime/malloc.go` -- allocator overview and `mallocgc`
  - `/Users/tchajed/sw/go/src/runtime/mheap.go` -- `mheap`, `mspan`, `heapArena` structs
  - `/Users/tchajed/sw/go/src/runtime/mcache.go` -- per-P cache
  - `/Users/tchajed/sw/go/src/runtime/mgc.go` -- GC algorithm documentation
  - `/Users/tchajed/sw/go/src/runtime/mcentral.go` -- central span lists
  - `/Users/tchajed/sw/go/src/runtime/sizeclasses.go` -- size class tables
- Bonwick, Jeff. "The Slab Allocator: An Object-Caching Kernel Memory Allocator." USENIX Summer 1994.
- TCMalloc: http://goog-perftools.sourceforge.net/doc/tcmalloc.html
- Dijkstra, E.W. et al. "On-the-fly garbage collection: an exercise in cooperation." CACM 1978.
