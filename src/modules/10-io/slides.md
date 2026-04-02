# Module 10: File Systems, I/O, and the Network Poller

---

## Slide 1: The I/O Challenge

**Two bad options for concurrent I/O:**

1. **Blocking I/O**: simple code, but 1 OS thread per connection
   - 10,000 connections = 10,000 threads = 80 GB of stacks

2. **Non-blocking I/O + callbacks**: efficient, but complex code
   - Callback hell, state machines, fragmented logic

**Go's solution**: write blocking code, get non-blocking performance

---

## Slide 2: The Layering

```
User Code:  conn.Read(buf)
               │
               ▼
os.File     ← name, finalizer, concurrency safety
               │
               ▼
poll.FD     ← non-blocking mode, EAGAIN handling, poller integration
               │
               ▼
syscall     ← raw system call wrapper
               │
               ▼
kernel      ← actual I/O
```

---

## Slide 3: os.File Internals

```go
// src/os/file_unix.go, lines 59-66
type file struct {
    pfd         poll.FD
    name        string
    dirinfo     atomic.Pointer[dirInfo]
    nonblock    bool       // whether we set nonblocking mode
    stdoutOrErr bool       // whether this is stdout or stderr
    appendMode  bool       // whether file is opened for appending
}
```

The real file descriptor lives inside `pfd.Sysfd`

---

## Slide 4: The io/fs Abstraction

```go
// src/io/fs/fs.go, lines 40-52
type FS interface {
    Open(name string) (File, error)
}
```

```go
// src/io/fs/fs.go, lines 95-99
type File interface {
    Stat() (FileInfo, error)
    Read([]byte) (int, error)
    Close() error
}
```

Enables: `embed.FS`, in-memory filesystems, testing, `zip.Reader`

---

## Slide 5: The internal/poll.FD Struct

```go
// src/internal/poll/fd_unix.go, lines 19-48
type FD struct {
    fdmu fdMutex    // serialize Read/Write, manage close

    Sysfd int       // actual OS file descriptor

    SysFile          // platform-specific state

    pd pollDesc     // I/O poller integration

    csema uint32    // close semaphore

    isBlocking uint32 // bypass poller if set

    IsStream bool     // TCP vs UDP
    ZeroReadIsEOF bool
    isFile bool       // file vs network socket
}
```

---

## Slide 6: The Critical Read Loop

```go
// src/internal/poll/fd_unix.go, lines 160-173
for {
    n, err := ignoringEINTRIO(syscall.Read, fd.Sysfd, p)
    if err != nil {
        n = 0
        if err == syscall.EAGAIN && fd.pd.pollable() {
            if err = fd.pd.waitRead(fd.isFile); err == nil {
                continue  // ← retry after poller wakes us
            }
        }
    }
    err = fd.eofError(n, err)
    return n, err
}
```

**EAGAIN** = "no data yet" -> park goroutine on poller -> retry when ready

---

## Slide 7: The EAGAIN Flow

```
syscall.Read(fd, buf)
       │
       ├── n > 0  →  return data  (fast path)
       │
       └── EAGAIN (no data available)
               │
               ▼
       fd.pd.waitRead()
               │
               ▼
       gopark() — goroutine sleeps
       OS thread is FREE
               │
               ▼
       ... data arrives ...
               │
               ▼
       poller wakes goroutine
               │
               ▼
       retry syscall.Read → data available → return
```

---

## Slide 8: Non-Blocking Mode Setup

```go
// src/os/file_unix.go, lines 193-209
if pollable {
    if nonBlocking {
        if kind == kindSock {
            f.nonblock = true
        }
    } else if err := syscall.SetNonblock(fd, true); err == nil {
        f.nonblock = true
        clearNonBlock = true
    } else {
        pollable = false
    }
}
```

Go puts file descriptors into **non-blocking mode** so that reads return
`EAGAIN` instead of blocking the OS thread

---

## Slide 9: Platform Limitations

```go
// src/os/file_unix.go, lines 164-181
if kind == kindOpenFile {
    switch runtime.GOOS {
    case "darwin", "ios", "dragonfly", "freebsd", "netbsd", "openbsd":
        // Don't try to use kqueue with regular files on *BSDs
        if err == nil && (typ == syscall.S_IFREG || typ == syscall.S_IFDIR) {
            pollable = false
        }
        // On Darwin, kqueue does not work properly with fifos
        if (runtime.GOOS == "darwin" || runtime.GOOS == "ios") && typ == syscall.S_IFIFO {
            pollable = false
        }
    }
}
```

Regular files on macOS: **blocking I/O** (ties up a thread, but disk I/O is fast)

---

## Slide 10: Network Poller Architecture

```go
// src/runtime/netpoll.go, lines 15-41
// Each platform must implement:
//
// func netpollinit()        // Initialize the poller
// func netpollopen(fd, pd)  // Register fd for notifications
// func netpollclose(fd)     // Unregister fd
// func netpoll(delta)       // Poll for ready fds
// func netpollBreak()       // Wake up blocked poller
```

**Platform backends:**
- Linux: `epoll`
- macOS/BSD: `kqueue`
- Windows: `IOCP`
- Solaris: `event ports`

---

## Slide 11: The pollDesc Struct

```go
// src/runtime/netpoll.go, lines 75-115
type pollDesc struct {
    link  *pollDesc        // free list link
    fd    uintptr          // file descriptor
    fdseq atomic.Uintptr   // stale event protection

    atomicInfo atomic.Uint32

    rg atomic.Uintptr  // reader: pdNil/pdReady/pdWait/G ptr
    wg atomic.Uintptr  // writer: pdNil/pdReady/pdWait/G ptr

    lock    mutex
    closing bool
    rd      int64      // read deadline
    wd      int64      // write deadline
    rt      timer      // read deadline timer
    wt      timer      // write deadline timer
}
```

One `pollDesc` per registered file descriptor

---

## Slide 12: Poll Semaphore States

```go
// src/runtime/netpoll.go, lines 64-68
const (
    pdNil   uintptr = 0  // idle
    pdReady uintptr = 1  // I/O notification pending
    pdWait  uintptr = 2  // goroutine preparing to park
)
// Also: G pointer — goroutine is blocked
```

**State machine for `rg` (read goroutine):**

```
pdNil → pdWait → G pointer → pdReady → pdNil
 idle    prep     blocked     ready     consumed
```

---

## Slide 13: netpollready -- Waking Goroutines

```go
// src/runtime/netpoll.go, lines 494-510
func netpollready(toRun *gList, pd *pollDesc, mode int32) int32 {
    delta := int32(0)
    var rg, wg *g
    if mode == 'r' || mode == 'r'+'w' {
        rg = netpollunblock(pd, 'r', true, &delta)
    }
    if mode == 'w' || mode == 'r'+'w' {
        wg = netpollunblock(pd, 'w', true, &delta)
    }
    if rg != nil { toRun.push(rg) }
    if wg != nil { toRun.push(wg) }
    return delta
}
```

Extracts parked goroutine from `pd.rg`/`pd.wg`, adds to runnable list

---

## Slide 14: epoll on Linux -- Initialization

```go
// src/runtime/netpoll_epoll.go, lines 21-43
func netpollinit() {
    epfd, errno = linux.EpollCreate1(linux.EPOLL_CLOEXEC)
    // ...
    efd, errno := linux.Eventfd(0, linux.EFD_CLOEXEC|linux.EFD_NONBLOCK)
    // ... register eventfd with epoll for wakeups ...
    netpollEventFd = uintptr(efd)
}
```

Two file descriptors:
- `epfd`: the epoll instance
- `netpollEventFd`: used by `netpollBreak()` to wake a blocked `epoll_wait`

---

## Slide 15: epoll -- Edge-Triggered Registration

```go
// src/runtime/netpoll_epoll.go, lines 49-55
func netpollopen(fd uintptr, pd *pollDesc) uintptr {
    var ev linux.EpollEvent
    ev.Events = linux.EPOLLIN | linux.EPOLLOUT |
                linux.EPOLLRDHUP | linux.EPOLLET
    tp := taggedPointerPack(unsafe.Pointer(pd), pd.fdseq.Load())
    *(*taggedPointer)(unsafe.Pointer(&ev.Data)) = tp
    return linux.EpollCtl(epfd, linux.EPOLL_CTL_ADD, int32(fd), &ev)
}
```

**`EPOLLET`** = edge-triggered: notify once per state change, not continuously

Tagged pointer stores both `pollDesc` address and sequence number

---

## Slide 16: epoll -- The Poll Loop

```go
// src/runtime/netpoll_epoll.go, lines 99-175 (simplified)
func netpoll(delay int64) (gList, int32) {
    var events [128]linux.EpollEvent
    n, errno := linux.EpollWait(epfd, events[:], 128, waitms)
    // ...
    for i := int32(0); i < n; i++ {
        ev := events[i]
        // determine mode ('r', 'w', or 'r'+'w')
        tp := *(*taggedPointer)(unsafe.Pointer(&ev.Data))
        pd := (*pollDesc)(tp.pointer())
        if pd.fdseq.Load() == tp.tag() {  // stale check
            delta += netpollready(&toRun, pd, mode)
        }
    }
    return toRun, delta
}
```

Returns a **list of goroutines** ready to run

---

## Slide 17: kqueue on macOS/BSD

```go
// src/runtime/netpoll_kqueue.go, lines 32-60
func netpollopen(fd uintptr, pd *pollDesc) int32 {
    // Arm EVFILT_READ and EVFILT_WRITE in edge-triggered mode
    var ev [2]keventt
    ev[0].filter = _EVFILT_READ
    ev[0].flags = _EV_ADD | _EV_CLEAR   // EV_CLEAR = edge-triggered
    ev[1] = ev[0]
    ev[1].filter = _EVFILT_WRITE
    n := kevent(kq, &ev[0], 2, nil, 0, nil)
    return 0
}
```

kqueue uses **two filters** (read + write) vs epoll's bitmask

`EV_CLEAR` = kqueue's equivalent of `EPOLLET`

---

## Slide 18: epoll vs kqueue Comparison

| Feature          | epoll (Linux)       | kqueue (macOS/BSD)    |
|------------------|---------------------|-----------------------|
| Creation         | `epoll_create1()`   | `kqueue()`            |
| Registration     | `epoll_ctl(ADD)`    | `kevent()` with `EV_ADD` |
| Polling          | `epoll_wait()`      | `kevent()`            |
| Edge-triggered   | `EPOLLET` flag      | `EV_CLEAR` flag       |
| Events per fd    | 1 (bitmask)         | 2 (READ + WRITE filters) |
| Wakeup mechanism | `eventfd`           | Platform-specific     |
| Close cleanup    | Explicit `EPOLL_CTL_DEL` | Automatic on close |

---

## Slide 19: Waking the Poller

```go
// src/runtime/netpoll_epoll.go, lines 67-89
func netpollBreak() {
    // CAS prevents duplicate wakeups
    if !netpollWakeSig.CompareAndSwap(0, 1) {
        return
    }
    var one uint64 = 1
    write(netpollEventFd, &one, 8)  // write to eventfd
}
```

Used when:
- New goroutine becomes runnable (need to redistribute work)
- Timer expires (poller may be blocked past the deadline)
- Runtime needs to shut down

---

## Slide 20: Scheduler Integration

```go
// src/runtime/proc.go, lines 3731-3754 (simplified)
// In findRunnable(), as a last resort:
if netpollinited() && netpollAnyWaiters() {
    list, delta := netpoll(delay) // block until I/O ready
    // ...
    // inject ready goroutines into run queues
}
```

**Where `netpoll` is called:**
1. `findRunnable()` -- non-blocking check (delay=0) in fast path
2. `findRunnable()` -- blocking wait as last resort
3. `sysmon` -- periodic non-blocking poll to prevent starvation

---

## Slide 21: Complete I/O Lifecycle

```
 Goroutine                    Runtime                     Kernel
    │                            │                           │
    │ conn.Read(buf)             │                           │
    │──────────────────→         │                           │
    │                    syscall.Read()                      │
    │                            │─────────────────→         │
    │                            │        EAGAIN  ←─────────│
    │                            │                           │
    │                    pd.waitRead()                       │
    │                    gopark(goroutine)                   │
    │  [parked]                  │                           │
    │                            │                           │
    │                    ... other goroutines run ...        │
    │                            │                           │
    │                            │   data arrives            │
    │                            │         ←─────────────────│
    │                    epoll_wait/kevent returns           │
    │                    netpollready → G to run queue       │
    │                            │                           │
    │  [resumed]                 │                           │
    │                    syscall.Read()                      │
    │                            │─────────────────→         │
    │   ←────────────────────────│    data         ←────────│
    │   return n, nil            │                           │
```

---

## Slide 22: Why Edge-Triggered?

**Level-triggered**: "data is available" (fires repeatedly)
**Edge-triggered**: "data became available" (fires once)

Go uses edge-triggered because:
- Only **one goroutine** waits per fd per direction
- After wake, goroutine reads all available data in a loop
- Level-triggered would re-fire between gopark and the retry read
- Edge-triggered avoids thundering herd on shared fds

---

## Slide 23: The Three Worlds Compared

```
Traditional Unix (blocking):
  Thread 1: read(fd1, ...) ← blocked
  Thread 2: read(fd2, ...) ← blocked
  Thread 3: read(fd3, ...) ← blocked
  → 1 thread per connection

Node.js (callbacks):
  epoll_wait() → callback(fd1) → callback(fd2) → ...
  → 1 thread, complex code

Go (goroutines + poller):
  Goroutine 1: conn.Read() → park → wake → return
  Goroutine 2: conn.Read() → park → wake → return
  Goroutine 3: conn.Read() → park → wake → return
  → N goroutines, M threads, simple code
```

---

## Slide 24: Architecture Diagram

```
┌───────────────────────────────────────────┐
│            User Code                       │
│  conn.Read()    file.Write()              │
└──────────┬────────────────────────────────┘
           │
┌──────────▼────────────────────────────────┐
│         internal/poll.FD                   │
│  EAGAIN loop    fdMutex    pollDesc        │
└──────────┬────────────────┬───────────────┘
           │                │
   syscall.Read/Write    waitRead/waitWrite
           │                │
    ┌──────▼─────┐   ┌──────▼──────────────┐
    │   kernel   │   │   runtime netpoll    │
    │            │   │                      │
    └────────────┘   │  ┌───────┐ ┌──────┐ │
                     │  │ epoll │ │kqueue│ │
                     │  └───────┘ └──────┘ │
                     └──────────┬──────────┘
                                │
                     ┌──────────▼──────────┐
                     │    Scheduler         │
                     │  findRunnable()      │
                     │  → run queue         │
                     └─────────────────────┘
```

---

## Slide 25: Key Takeaways

1. **Non-blocking mode** + **EAGAIN retry** = blocking semantics without
   blocking threads

2. **pollDesc** semaphores (rg/wg) track one waiting goroutine per fd per
   direction

3. **Edge-triggered** epoll/kqueue avoids redundant notifications

4. **Scheduler integration**: `findRunnable()` calls `netpoll()` to discover
   I/O-ready goroutines

5. **The programmer's experience**: simple synchronous code that scales to
   millions of connections

6. **Platform abstraction**: one `netpoll.go` interface, multiple backends
   (epoll, kqueue, IOCP)

---
