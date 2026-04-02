# Module 9: Goroutine Stacks

---

## Slide 1: The Stack Problem

Every executing function needs stack space for:
- Local variables
- Function arguments and return values
- Return addresses and saved registers

**OS threads**: fixed-size stacks (1-8 MB), allocated at creation

**Problem**: 10,000 threads x 8 MB = 80 GB of stack memory

---

## Slide 2: Why Fixed Stacks Don't Scale

| Threads   | Stack Memory (8 MB each) |
|-----------|--------------------------|
| 1,000     | 8 GB                     |
| 10,000    | 80 GB                    |
| 100,000   | 800 GB                   |
| 1,000,000 | 8 TB                     |

Most of that memory is **never used** -- typical functions need only a few KB

---

## Slide 3: The Goroutine Advantage

Goroutines start with a **2 KB stack** that grows on demand

| Goroutines | Stack Memory (2 KB each) |
|------------|--------------------------|
| 1,000      | 2 MB                     |
| 10,000     | 20 MB                    |
| 100,000    | 200 MB                   |
| 1,000,000  | 2 GB                     |

**4000x** more efficient than OS thread stacks

---

## Slide 4: Stack Bounds in the G Struct

```go
// src/runtime/runtime2.go, lines 462-465
type stack struct {
    lo uintptr
    hi uintptr
}
```

```go
// src/runtime/runtime2.go, lines 473-483
type g struct {
    stack       stack   // [stack.lo, stack.hi)
    stackguard0 uintptr // stack growth check threshold
    stackguard1 uintptr // system stack check
    // ...
}
```

---

## Slide 5: Stack Layout Diagram

```
stack.hi  ──────────────────  (top)
          │ active frames  │
          │ (grows down)   │  ← SP
          │                │
          │ unused space   │
          │                │
guard0    ├────────────────┤  ← stack.lo + StackGuard
          │ guard area     │
          │ (nosplit room) │
stack.lo  ──────────────────  (bottom)
```

`stackguard0` is set to `stack.lo + StackGuard`

---

## Slide 6: The Initial Stack Size

```go
// src/runtime/stack.go, lines 77-78
const (
    // The minimum size of stack used by Go code
    stackMin = 2048
)
```

Just 2 KB -- enough for most leaf functions, grows as needed

---

## Slide 7: The Compiler Prologue

The compiler inserts a check at the start of **every function**:

```asm
; stack frame size <= StackSmall:
    CMPQ guard, SP       ; is SP below the guard?
    JHI  3(PC)           ; if so, jump to morestack
    CALL morestack(SB)
```

From `src/runtime/stack.go`, lines 37-41

Three variants depending on frame size (small, medium, large)

---

## Slide 8: StackSmall Optimization

```
stack frame size <= StackSmall:
    CMPQ guard, SP              ; 1 compare

stack frame size > StackSmall but < StackBig:
    LEAQ (frame-StackSmall)(SP), R0
    CMPQ guard, R0              ; subtract + compare

stack frame size >= StackBig:
    CALL morestack(SB)          ; always call
```

Small frames save one instruction by allowing SP to "protrude"
`StackSmall` bytes below the guard

---

## Slide 9: stackguard0 Double Duty

`stackguard0` serves **two purposes**:

**1. Stack overflow detection** (normal value):
```go
stackguard0 = stack.lo + StackGuard
```

**2. Cooperative preemption** (special sentinel):
```go
// src/runtime/stack.go, lines 132-133
stackPreempt = uintptrMask & -1314  // 0xfffffade
```

Sentinel is larger than any real SP, so the prologue check always fails,
routing through `morestack` -> `newstack` -> preemption handling

---

## Slide 10: newstack() -- The Growth Entry Point

```go
// src/runtime/stack.go, lines 1026, 1148-1151
func newstack() {
    // ... preemption checks ...

    // Allocate a bigger segment and move the stack.
    oldsize := gp.stack.hi - gp.stack.lo
    newsize := oldsize * 2  // always double
```

**Doubling** gives amortized O(1) cost per push (same analysis as dynamic arrays)

Growth sequence: 2K -> 4K -> 8K -> 16K -> 32K -> ...

---

## Slide 11: The Copy Process

`copystack()` at `src/runtime/stack.go`, line 900:

1. **Allocate** new stack: `stackalloc(newsize)`
2. **Compute delta**: `new.hi - old.hi`
3. **Adjust sudogs** (channel wait pointers)
4. **Copy** used portion: `memmove(new.hi-used, old.hi-used, used)`
5. **Walk all frames**, adjust every pointer into old stack
6. **Update G**: `gp.stack = new`, `gp.stackguard0 = new.lo + StackGuard`
7. **Free** old stack: `stackfree(old)`

---

## Slide 12: Pointer Adjustment

```go
// src/runtime/stack.go, lines 610-631
func adjustpointer(adjinfo *adjustinfo, vpp unsafe.Pointer) {
    pp := (*uintptr)(vpp)
    p := *pp
    if adjinfo.old.lo <= p && p < adjinfo.old.hi {
        *pp = p + adjinfo.delta
    }
}
```

For every pointer-typed word on the stack:
- If it points into the old stack range -> add delta
- If it points elsewhere (heap, globals) -> leave unchanged

Requires **precise stack maps** from the compiler

---

## Slide 13: Visualizing Stack Copy

```
BEFORE:                        AFTER:
old stack                      new stack (2x size)
┌──────────┐ old.hi           ┌──────────┐ new.hi
│  frame C │                  │  frame C │ (copied, ptrs adjusted)
│  frame B │                  │  frame B │
│  frame A │                  │  frame A │
├──────────┤ SP               ├──────────┤ new SP
│  unused  │                  │          │
│          │                  │  unused  │
│          │                  │  (more   │
└──────────┘ old.lo           │   room)  │
     │                        │          │
     └── freed ──┘            └──────────┘ new.lo
```

---

## Slide 14: Stack Allocation Pools

**Small stacks** (2 KB - 16 KB): per-P caches (lock-free fast path)

```go
// src/runtime/stack.go, lines 388-396
c := thisg.m.p.ptr().mcache
x = c.stackcache[order].list
if x.ptr() == nil {
    stackcacherefill(c, order)
}
c.stackcache[order].list = x.ptr().next
```

**Large stacks**: global pool with lock, backed by `mheap`

Same pattern as the memory allocator: per-P cache -> global pool -> OS

---

## Slide 15: Stack Free Pools

```go
// src/runtime/stack.go, lines 518-531
// Small stack free (per-P cache):
c := gp.m.p.ptr().mcache
if c.stackcache[order].size >= _StackCacheSize {
    stackcacherelease(c, order)  // return excess to global
}
x.ptr().next = c.stackcache[order].list
c.stackcache[order].list = x
c.stackcache[order].size += n
```

Per-P caches have a **maximum size** (`_StackCacheSize`).
Excess is released to the global pool.

---

## Slide 16: Stack Shrinking

The GC can **shrink** stacks that are less than 1/4 utilized:

```go
// src/runtime/stack.go, lines 1284-1299
oldsize := gp.stack.hi - gp.stack.lo
newsize := oldsize / 2
if newsize < fixedStack {
    return  // don't go below minimum
}
avail := gp.stack.hi - gp.stack.lo
if used := gp.stack.hi - gp.sched.sp + stackNosplit; used >= avail/4 {
    return  // still using enough, don't shrink
}
copystack(gp, newsize)
```

Same `copystack` mechanism as growth -- just in reverse

---

## Slide 17: Segmented Stacks (Historical)

**Go 1.0-1.3**: segmented ("split") stacks

```
Segment 1         Segment 2         Segment 3
┌──────────┐      ┌──────────┐      ┌──────────┐
│  frames  │─────→│  frames  │─────→│  frames  │
└──────────┘      └──────────┘      └──────────┘
```

**The "hot split" problem**: function at stack boundary called in a loop

```
call f()  → allocate segment
return    → free segment
call f()  → allocate segment   ← thrashing!
return    → free segment
```

---

## Slide 18: Contiguous Stacks Win

**Go 1.4+**: contiguous (copying) stacks

| Aspect         | Segmented       | Contiguous      |
|----------------|-----------------|-----------------|
| Hot split      | Severe perf bug | Not possible    |
| Cache locality | Poor (segments scattered) | Excellent |
| Complexity     | Segment links   | Pointer adjustment |
| Requirement    | None extra      | Precise stack maps |

The key insight: **doubling + copying** gives amortized O(1) cost,
and the compiler already generates stack maps for GC

---

## Slide 19: The Full Picture

```
Function Entry
    │
    ▼
SP < stackguard0?  ──no──→  Execute function normally
    │
   yes
    │
    ▼
Call morestack (asm trampoline)
    │
    ▼
newstack()
    │
    ├── stackguard0 == stackPreempt?  → preempt goroutine
    │
    ├── Allocate 2x stack (stackalloc)
    │
    ├── Copy stack contents (copystack)
    │   ├── memmove used portion
    │   ├── Walk frames, adjust pointers
    │   └── Update g.stack, g.stackguard0
    │
    ├── Free old stack (stackfree)
    │
    └── Resume execution (gogo)
```

---

## Slide 20: Key Takeaways

1. **2 KB initial stacks** enable millions of goroutines

2. **Compiler prologues** transparently detect overflow at every function call

3. **Contiguous stack copying** with pointer adjustment replaces segmented stacks

4. **Per-P caching** makes stack alloc/free as fast as malloc

5. **stackguard0** unifies stack growth and cooperative preemption in one check

6. **GC-driven shrinking** reclaims memory from idle goroutines

---
