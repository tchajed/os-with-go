# Module 10: File Systems, I/O, and the Network Poller (60 min)

## Background: I/O Multiplexing and the Quest for Scalable Networking

I/O is the interface between a program and the outside world -- disks, networks,
terminals, pipes, and devices. The fundamental challenge of I/O is that it is
_slow_ relative to computation. A modern CPU can execute billions of instructions
per second, but a network round-trip across the same data center takes tens of
microseconds, and a spinning disk seek takes milliseconds -- an eternity in CPU
time. The simplest programming model, blocking I/O, has a thread call `read()`
and wait until data arrives. This works well for a single connection, but it
means one OS thread per concurrent I/O operation. Since OS threads are expensive
(each consumes kernel memory for its stack and task struct, and context-switching
between thousands of them bogs down the scheduler), blocking I/O hits a wall
when you need to handle many connections simultaneously. Unix's
"everything-is-a-file" abstraction gives us a uniform interface (file
descriptors, `read`, `write`, `close`), but it does not solve the problem of
_waiting_ on many file descriptors at once.

The history of I/O multiplexing on Unix and Linux is a story of successive
attempts to solve this scaling problem. The original `select()` system call,
introduced in 4.2BSD in 1983, let a process monitor multiple file descriptors
for readiness. But `select` has painful limitations: it uses a fixed-size
bitmask (typically limited to 1024 descriptors), and every call requires copying
the entire set of descriptors into the kernel and scanning all of them
regardless of how many are actually ready. `poll()` (SVR3, 1986) removed the
fixed-size limit by using an array of `pollfd` structs, but it still requires a
linear scan of all monitored descriptors on every call -- O(n) in the total
number of descriptors, not the number of _ready_ descriptors. For a server with
10,000 connections where only a handful have data at any moment, this is
wasteful. Linux's `epoll` (2002) and BSD's `kqueue` (2000, FreeBSD 4.1) finally
solved this with stateful, kernel-maintained interest sets: you register
descriptors once, and the kernel returns only the ready ones, giving O(1)
behavior with respect to the total number of monitored descriptors. More
recently, Linux's `io_uring` (2019) pushes the boundary further with a
shared-memory submission/completion ring that eliminates system call overhead
entirely for batched I/O, enabling truly asynchronous operations for both
network and disk I/O.

These kernel mechanisms were developed in response to a concrete engineering
challenge articulated by Dan Kegel in his influential 1999 essay, "The C10K
Problem": how do you build a server that handles 10,000 concurrent connections
on commodity hardware? At the time, this was considered very difficult. The
thread-per-connection model consumed too much memory and CPU on context
switches. Kegel surveyed the landscape of available approaches -- non-blocking
I/O with `select`/`poll`, signal-driven I/O (`SIGIO`), and the then-nascent
asynchronous I/O interfaces -- and argued that the OS needed better primitives.
The C10K essay directly influenced the development of `epoll` and `kqueue`, and
shaped the design of high-performance servers like Nginx (which replaced
Apache's thread-per-connection model with a single-threaded event loop). Today,
the frontier has moved to the "C10M problem" -- ten _million_ concurrent
connections -- driven by `epoll`, `io_uring`, and kernel-bypass techniques like
DPDK.

Different language runtimes have taken different approaches to giving
programmers a usable API on top of these kernel primitives. Event-driven
frameworks like libevent, libev, and libuv abstract over platform differences
(`epoll` on Linux, `kqueue` on macOS/BSD, IOCP on Windows) and expose a
callback-based API: register interest in an event, provide a function to call
when it fires. Node.js builds on libuv and embraces this model fully -- a
single-threaded event loop runs JavaScript callbacks when I/O completes, with a
thread pool for operations that lack async OS support (like file I/O on Linux).
Java took a different path: the NIO (New I/O) package introduced `Selector` and
non-blocking channels, but the callback-style programming was complex enough
that frameworks like Netty arose to manage it. Java 21's virtual threads
(Project Loom) revisit the problem with lightweight user-space threads, much
like goroutines. Rust's async ecosystem (Tokio runtime, mio for portable event
notification) uses `async`/`await` syntax compiled into state machines, offering
zero-cost abstractions but requiring the programmer to reason about pinning,
lifetimes, and `Send`/`Sync` bounds.

Go's approach is distinctive: it hides all this complexity behind synchronous,
blocking-style I/O calls. When a goroutine calls `conn.Read()`, it looks like a
blocking call, and the goroutine does block -- but the OS thread does not. Under
the hood, the Go runtime sets file descriptors to non-blocking mode, attempts
the I/O, and if the operation would block (`EAGAIN`), it parks the goroutine on
the runtime's integrated network poller (`epoll` on Linux, `kqueue` on
macOS/BSD). The OS thread is freed to run other goroutines. When the kernel
signals that data is ready, the scheduler wakes the parked goroutine and it
retries the operation. The programmer writes simple sequential code; the runtime
provides the efficiency of an event-driven architecture. This module traces
exactly how this works, from the `os.File` type through the `internal/poll`
package down to the platform-specific poller implementations and their
integration with the scheduler.

## Overview

One of Go's most celebrated features is that goroutine I/O "just works" -- you
write blocking `Read` and `Write` calls, but under the hood the runtime uses
non-blocking I/O and an event-driven poller to avoid tying up OS threads. This
module traces the full path from `os.File.Read()` down through the internal
polling layer to the platform-specific network poller (`epoll` on Linux, `kqueue`
on macOS/BSD), showing how the runtime integrates I/O readiness with goroutine
scheduling.

---

## 1. The File Abstraction in Go (10 min)

### The os.File Type

The public-facing file type in Go is `os.File`. It is defined as a pointer to
a private `file` struct:

```go
// src/os/file_unix.go, lines 55-66
// file is the real representation of *File.
// The extra level of indirection ensures that no clients of os
// can overwrite this data, which could cause the finalizer
// to close the wrong file descriptor.
type file struct {
	pfd         poll.FD
	name        string
	dirinfo     atomic.Pointer[dirInfo] // nil unless directory being read
	nonblock    bool                    // whether we set nonblocking mode
	stdoutOrErr bool                    // whether this is stdout or stderr
	appendMode  bool                    // whether file is opened for appending
}
```

Key observations:
- The actual file descriptor is wrapped in a `poll.FD` (from `internal/poll`)
- The `nonblock` flag tracks whether Go put the descriptor into non-blocking mode
- There is no direct `int` file descriptor field -- it lives inside `pfd.Sysfd`

### The io/fs.FS Interface

Go 1.16 introduced a filesystem abstraction via the `io/fs` package:

```go
// src/io/fs/fs.go, lines 40-52
type FS interface {
	// Open opens the named file.
	Open(name string) (File, error)
}
```

And the minimal `File` interface:

```go
// src/io/fs/fs.go, lines 95-99
type File interface {
	Stat() (FileInfo, error)
	Read([]byte) (int, error)
	Close() error
}
```

This is a higher-level abstraction than `os.File`. The `os.File` type satisfies
this interface and much more (writing, seeking, etc.). The `fs.FS` interface
enables in-memory filesystems, embedded filesystems (`embed.FS`), and testing
without touching the real filesystem.

### The Layering

The I/O path through Go's runtime has multiple layers:

```
User Code
    │
    ▼
os.File.Read()           ← high-level, platform-independent
    │
    ▼
internal/poll.FD.Read()  ← non-blocking I/O with EAGAIN handling
    │
    ▼
syscall.Read()           ← raw system call wrapper
    │
    ▼
kernel                   ← actual I/O
```

Each layer adds important functionality:
- **os.File**: name tracking, finalizers, safe concurrent access
- **internal/poll.FD**: non-blocking mode, poller integration, EAGAIN retry
- **syscall**: thin wrapper around the kernel interface

---

## 2. The internal/poll Package (10 min)

### The FD Struct

The `internal/poll.FD` struct is the workhorse of Go's I/O system. It wraps a
raw file descriptor with synchronization and poller integration:

```go
// src/internal/poll/fd_unix.go, lines 17-48
// FD is a file descriptor. The net and os packages use this type as a
// field of a larger type representing a network connection or OS file.
type FD struct {
	// Lock sysfd and serialize access to Read and Write methods.
	fdmu fdMutex

	// System file descriptor. Immutable until Close.
	Sysfd int

	// Platform dependent state of the file descriptor.
	SysFile

	// I/O poller.
	pd pollDesc

	// Semaphore signaled when file is closed.
	csema uint32

	// Non-zero if this file has been set to blocking mode.
	isBlocking uint32

	// Whether this is a streaming descriptor, as opposed to a
	// packet-based descriptor like a UDP socket. Immutable.
	IsStream bool

	// Whether a zero byte read indicates EOF. This is false for a
	// message based socket connection.
	ZeroReadIsEOF bool

	// Whether this is a file rather than a network socket.
	isFile bool
}
```

Key fields:
- **`fdmu`**: A specialized `fdMutex` that serializes concurrent reads (and
  separately, concurrent writes) while also managing reference counting for
  safe close.
- **`Sysfd`**: The actual OS file descriptor number.
- **`pd`**: A `pollDesc` that hooks into the runtime's network poller.
- **`isBlocking`**: When set, the FD bypasses the poller and uses blocking I/O
  (tying up the OS thread).

### FD Initialization

When a file is opened, the `FD.Init` method registers it with the poller:

```go
// src/internal/poll/fd_unix.go, lines 55-73
func (fd *FD) Init(net string, pollable bool) error {
	fd.SysFile.init()

	if net == "file" {
		fd.isFile = true
	}
	if !pollable {
		fd.isBlocking = 1
		return nil
	}
	err := fd.pd.init(fd)
	if err != nil {
		// If we could not initialize the runtime poller,
		// assume we are using blocking mode.
		fd.isBlocking = 1
	}
	return err
}
```

The `pd.init(fd)` call ultimately invokes `runtime.poll_runtime_pollOpen`, which
registers the file descriptor with the platform's event notification mechanism
(epoll/kqueue).

### The Read Loop with EAGAIN Handling

The most important method is `FD.Read`. This is where non-blocking I/O meets
goroutine scheduling:

```go
// src/internal/poll/fd_unix.go, lines 140-173
// Read implements io.Reader.
func (fd *FD) Read(p []byte) (int, error) {
	if err := fd.readLock(); err != nil {
		return 0, err
	}
	defer fd.readUnlock()
	if len(p) == 0 {
		return 0, nil
	}
	if err := fd.pd.prepareRead(fd.isFile); err != nil {
		return 0, err
	}
	if fd.IsStream && len(p) > maxRW {
		p = p[:maxRW]
	}
	for {
		n, err := ignoringEINTRIO(syscall.Read, fd.Sysfd, p)
		if err != nil {
			n = 0
			if err == syscall.EAGAIN && fd.pd.pollable() {
				if err = fd.pd.waitRead(fd.isFile); err == nil {
					continue
				}
			}
		}
		err = fd.eofError(n, err)
		return n, err
	}
}
```

The critical loop:
1. **Try the read** via `syscall.Read` (non-blocking, since the fd is in non-blocking mode)
2. If the read returns **`EAGAIN`** (no data available):
   - Call `fd.pd.waitRead()` -- this **parks the goroutine** on the poller
   - When data arrives, the poller wakes the goroutine
   - The goroutine **retries** the read (`continue`)
3. If the read succeeds or fails with a real error, return

This is the fundamental trick: **blocking I/O semantics from the programmer's
perspective, non-blocking I/O under the hood.** The goroutine blocks, but the
OS thread does not.

---

## 3. Non-Blocking I/O Setup (5 min)

### How Files Enter Non-Blocking Mode

When Go opens a file or socket, it puts the descriptor into non-blocking mode.
This happens in the `newFile` function:

```go
// src/os/file_unix.go, lines 193-209
	clearNonBlock := false
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

Then the FD is registered with the poller:

```go
// src/os/file_unix.go, lines 218-222
	if pollErr := f.pfd.Init("file", pollable); pollErr != nil && clearNonBlock {
		if err := syscall.SetNonblock(fd, false); err == nil {
			f.nonblock = false
		}
	}
```

### Platform Limitations

Not all file descriptors work with the poller. On macOS/BSD, regular files and
directories cannot be polled with kqueue (they always report as ready). The
`newFile` function checks for this:

```go
// src/os/file_unix.go, lines 164-191
	if kind == kindOpenFile {
		switch runtime.GOOS {
		case "darwin", "ios", "dragonfly", "freebsd", "netbsd", "openbsd":
			var st syscall.Stat_t
			err := ignoringEINTR(func() error {
				return syscall.Fstat(fd, &st)
			})
			typ := st.Mode & syscall.S_IFMT
			// Don't try to use kqueue with regular files on *BSDs.
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

When a descriptor cannot use the poller, it falls back to blocking I/O, which
will tie up an OS thread. This is acceptable for regular files (which complete
quickly) but would be problematic for network sockets (which is why network
sockets always use the poller).

---

## 4. The Network Poller: Core Design (10 min)

### Architecture

The network poller lives in `runtime/netpoll.go` and provides a platform-independent
interface. Each platform implements the actual polling mechanism:

```go
// src/runtime/netpoll.go, lines 15-41
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
//
// func netpollBreak()
//     Wake up the network poller, assumed to be blocked in netpoll.
```

### The pollDesc Struct

Each file descriptor registered with the poller gets a `pollDesc`:

```go
// src/runtime/netpoll.go, lines 75-115
type pollDesc struct {
	_     sys.NotInHeap
	link  *pollDesc      // in pollcache, protected by pollcache.lock
	fd    uintptr        // constant for pollDesc usage lifetime
	fdseq atomic.Uintptr // protects against stale pollDesc

	atomicInfo atomic.Uint32 // atomic pollInfo

	// rg, wg are accessed atomically and hold g pointers.
	rg atomic.Uintptr // pdReady, pdWait, G waiting for read or pdNil
	wg atomic.Uintptr // pdReady, pdWait, G waiting for write or pdNil

	lock    mutex // protects the following fields
	closing bool
	rrun    bool      // whether rt is running
	wrun    bool      // whether wt is running
	user    uint32    // user settable cookie
	rseq    uintptr   // protects from stale read timers
	rt      timer     // read deadline timer
	rd      int64     // read deadline
	wseq    uintptr   // protects from stale write timers
	wt      timer     // write deadline timer
	wd      int64     // write deadline
	self    *pollDesc // storage for indirect interface
}
```

The `pollDesc` contains **two binary semaphores** (`rg` and `wg`) for parking
reader and writer goroutines respectively.

### Poll States

The semaphores (`rg`, `wg`) can be in one of four states:

```go
// src/runtime/netpoll.go, lines 51-68
// pollDesc contains 2 binary semaphores, rg and wg, to park reader and writer
// goroutines respectively. The semaphore can be in the following states:
//
//	pdReady - io readiness notification is pending;
//	          a goroutine consumes the notification by changing the state to pdNil.
//	pdWait - a goroutine prepares to park on the semaphore, but not yet parked;
//	         the goroutine commits to park by changing the state to G pointer,
//	         or, alternatively, concurrent io notification changes the state to pdReady,
//	         or, alternatively, concurrent timeout/close changes the state to pdNil.
//	G pointer - the goroutine is blocked on the semaphore;
//	            io notification or timeout/close changes the state to pdReady or pdNil
//	            and unparks the goroutine.
//	pdNil - none of the above.
const (
	pdNil   uintptr = 0
	pdReady uintptr = 1
	pdWait  uintptr = 2
)
```

State transition diagram:

```
                    ┌─────────────┐
                    │   pdNil     │ (initial / idle)
                    └──────┬──────┘
                           │ goroutine wants to read
                           ▼
                    ┌─────────────┐
                    │   pdWait    │ (preparing to park)
                    └──────┬──────┘
                           │ goroutine commits to park
                           ▼
                    ┌─────────────┐
         I/O ready │  G pointer  │ goroutine is blocked
              ─────┤             ├───── timeout/close
              │    └─────────────┘      │
              ▼                         ▼
       ┌─────────────┐          ┌─────────────┐
       │   pdReady   │          │    pdNil     │
       └─────────────┘          └─────────────┘
              │ goroutine wakes and
              │ consumes notification
              ▼
       ┌─────────────┐
       │    pdNil     │
       └─────────────┘
```

### netpollready: Waking Goroutines

When the platform poller detects that a file descriptor is ready, it calls
`netpollready`:

```go
// src/runtime/netpoll.go, lines 483-510
// netpollready is called by the platform-specific netpoll function.
// It declares that the fd associated with pd is ready for I/O.
// The toRun argument is used to build a list of goroutines to return
// from netpoll. The mode argument is 'r', 'w', or 'r'+'w' to indicate
// whether the fd is ready for reading or writing or both.
func netpollready(toRun *gList, pd *pollDesc, mode int32) int32 {
	delta := int32(0)
	var rg, wg *g
	if mode == 'r' || mode == 'r'+'w' {
		rg = netpollunblock(pd, 'r', true, &delta)
	}
	if mode == 'w' || mode == 'r'+'w' {
		wg = netpollunblock(pd, 'w', true, &delta)
	}
	if rg != nil {
		toRun.push(rg)
	}
	if wg != nil {
		toRun.push(wg)
	}
	return delta
}
```

This function unblocks the goroutine(s) waiting on the given `pollDesc` and adds
them to the `toRun` list. The caller (`netpoll`) returns this list to the
scheduler, which puts these goroutines back on run queues.

---

## 5. Platform Implementations (10 min)

### epoll on Linux

The Linux implementation uses `epoll` in **edge-triggered** mode:

```go
// src/runtime/netpoll_epoll.go, lines 15-19
var (
	epfd           int32         = -1 // epoll descriptor
	netpollEventFd uintptr            // eventfd for netpollBreak
	netpollWakeSig atomic.Uint32      // used to avoid duplicate calls of netpollBreak
)
```

Initialization creates an epoll instance and an eventfd for waking the poller:

```go
// src/runtime/netpoll_epoll.go, lines 21-43
func netpollinit() {
	var errno uintptr
	epfd, errno = linux.EpollCreate1(linux.EPOLL_CLOEXEC)
	if errno != 0 {
		println("runtime: epollcreate failed with", errno)
		throw("runtime: netpollinit failed")
	}
	efd, errno := linux.Eventfd(0, linux.EFD_CLOEXEC|linux.EFD_NONBLOCK)
	// ... register eventfd with epoll for wakeups ...
	netpollEventFd = uintptr(efd)
}
```

Registering a file descriptor uses edge-triggered mode (`EPOLLET`):

```go
// src/runtime/netpoll_epoll.go, lines 49-55
func netpollopen(fd uintptr, pd *pollDesc) uintptr {
	var ev linux.EpollEvent
	ev.Events = linux.EPOLLIN | linux.EPOLLOUT | linux.EPOLLRDHUP | linux.EPOLLET
	tp := taggedPointerPack(unsafe.Pointer(pd), pd.fdseq.Load())
	*(*taggedPointer)(unsafe.Pointer(&ev.Data)) = tp
	return linux.EpollCtl(epfd, linux.EPOLL_CTL_ADD, int32(fd), &ev)
}
```

The flags `EPOLLIN | EPOLLOUT | EPOLLRDHUP | EPOLLET` mean:
- Monitor for both read and write readiness
- Detect remote hangup (`EPOLLRDHUP`)
- **Edge-triggered** (`EPOLLET`): only notify when readiness *changes* (e.g., new data arrives), not continuously while data remains available. This contrasts with **level-triggered** mode (the default) where the kernel repeatedly reports readiness as long as the condition holds. Edge-triggered is more efficient for Go's model because each fd has exactly one goroutine waiting, so there is no risk of missing events.

The main polling function calls `epoll_wait`:

```go
// src/runtime/netpoll_epoll.go, lines 99-175
func netpoll(delay int64) (gList, int32) {
	// ... convert delay to milliseconds ...
	var events [128]linux.EpollEvent
retry:
	n, errno := linux.EpollWait(epfd, events[:], int32(len(events)), waitms)
	// ... error handling ...
	var toRun gList
	delta := int32(0)
	for i := int32(0); i < n; i++ {
		ev := events[i]
		// ... skip eventfd wakeup events ...
		var mode int32
		if ev.Events&(linux.EPOLLIN|linux.EPOLLRDHUP|linux.EPOLLHUP|linux.EPOLLERR) != 0 {
			mode += 'r'
		}
		if ev.Events&(linux.EPOLLOUT|linux.EPOLLHUP|linux.EPOLLERR) != 0 {
			mode += 'w'
		}
		if mode != 0 {
			tp := *(*taggedPointer)(unsafe.Pointer(&ev.Data))
			pd := (*pollDesc)(tp.pointer())
			tag := tp.tag()
			if pd.fdseq.Load() == tag {
				pd.setEventErr(ev.Events == linux.EPOLLERR, tag)
				delta += netpollready(&toRun, pd, mode)
			}
		}
	}
	return toRun, delta
}
```

The tagged pointer stored in the epoll event data allows the runtime to map events
back to their `pollDesc`. The `fdseq` check guards against stale events for
file descriptors that have been closed and reused.

### kqueue on macOS/BSD

The macOS/BSD implementation uses `kqueue` with `EV_CLEAR` (the kqueue equivalent
of edge-triggered mode):

```go
// src/runtime/netpoll_kqueue.go, lines 32-60
func netpollopen(fd uintptr, pd *pollDesc) int32 {
	// Arm both EVFILT_READ and EVFILT_WRITE in edge-triggered mode (EV_CLEAR)
	var ev [2]keventt
	*(*uintptr)(unsafe.Pointer(&ev[0].ident)) = fd
	ev[0].filter = _EVFILT_READ
	ev[0].flags = _EV_ADD | _EV_CLEAR
	ev[0].fflags = 0
	ev[0].data = 0
	// ... store tagged pointer in ev[0].udata ...
	ev[1] = ev[0]
	ev[1].filter = _EVFILT_WRITE
	n := kevent(kq, &ev[0], 2, nil, 0, nil)
	if n < 0 {
		return -n
	}
	return 0
}
```

Key differences from epoll:
- kqueue uses two separate filters (`EVFILT_READ`, `EVFILT_WRITE`) instead of
  a bitmask
- `EV_CLEAR` is the kqueue equivalent of `EPOLLET` (edge-triggered)
- No need to explicitly unregister -- closing the fd removes all kevents
- kqueue uses `kevent()` for both registration and waiting

The polling function is structurally similar:

```go
// src/runtime/netpoll_kqueue.go, lines 90-183
func netpoll(delay int64) (gList, int32) {
	// ...
	var events [64]keventt
retry:
	n := kevent(kq, nil, 0, &events[0], int32(len(events)), tp)
	// ...
	for i := 0; i < int(n); i++ {
		ev := &events[i]
		// ... skip wakeup events ...
		var mode int32
		switch ev.filter {
		case _EVFILT_READ:
			mode += 'r'
			if ev.flags&_EV_EOF != 0 {
				mode += 'w'  // closed pipe: wake writers too
			}
		case _EVFILT_WRITE:
			mode += 'w'
		}
		if mode != 0 {
			// ... extract pollDesc from tagged pointer ...
			delta += netpollready(&toRun, pd, mode)
		}
	}
	return toRun, delta
}
```

### Waking the Poller: netpollBreak

Both implementations provide a way to wake the poller from another thread:

**Linux** uses an `eventfd`:
```go
// src/runtime/netpoll_epoll.go, lines 67-89
func netpollBreak() {
	if !netpollWakeSig.CompareAndSwap(0, 1) {
		return  // already being woken
	}
	var one uint64 = 1
	oneSize := int32(unsafe.Sizeof(one))
	for {
		n := write(netpollEventFd, noescape(unsafe.Pointer(&one)), oneSize)
		if n == oneSize {
			break
		}
		// ... handle EINTR, EAGAIN ...
	}
}
```

**macOS/BSD** uses a platform-specific wakeup mechanism (`wakeNetpoll`):
```go
// src/runtime/netpoll_kqueue.go, lines 73-80
func netpollBreak() {
	if !netpollWakeSig.CompareAndSwap(0, 1) {
		return
	}
	wakeNetpoll(kq)
}
```

The CAS on `netpollWakeSig` prevents redundant wakeup writes when multiple
goroutines try to break the poller simultaneously.

---

## 6. Integration with the Scheduler (10 min)

### findRunnable() and the Poller

The scheduler's `findRunnable()` function (in `proc.go`) checks the network
poller as part of its work-stealing loop. When there is no other work to do,
the scheduler thread blocks in the poller:

```go
// src/runtime/proc.go, lines 3731-3754
	// Poll network until next timer.
	if netpollinited() && (netpollAnyWaiters() || pollUntil != 0) && sched.lastpoll.Swap(0) != 0 {
		sched.pollUntil.Store(pollUntil)
		// ...
		delay := int64(-1)
		if pollUntil != 0 {
			if now == 0 {
				now = nanotime()
			}
			delay = pollUntil - now
			if delay < 0 {
				delay = 0
			}
		}
		// ...
		list, delta := netpoll(delay) // block until new work is available
```

The `netpoll(delay)` call blocks the OS thread in `epoll_wait` / `kevent` until
either:
- A file descriptor becomes ready (I/O event)
- The delay expires (timer event)
- `netpollBreak()` is called (new work available elsewhere)

The returned `gList` contains goroutines whose I/O is ready. The scheduler
injects them back into run queues.

### The Complete I/O Flow

Here is the full lifecycle of a goroutine performing a network read:

```
1. Goroutine calls conn.Read(buf)
       │
       ▼
2. os.File.Read → internal/poll.FD.Read
       │
       ▼
3. syscall.Read(fd, buf)
       │
       ├── Data available → return n, nil  (fast path)
       │
       └── EAGAIN (no data)
               │
               ▼
4. fd.pd.waitRead() → runtime.poll_runtime_pollWait
       │
       ▼
5. netpollblock():
   - Set rg = pdWait
   - CAS rg from pdWait to G pointer (commit to park)
   - gopark() — goroutine is descheduled
       │
       ▼
6. OS thread is FREE to run other goroutines
       │
       ▼
7. Eventually, data arrives on the socket
       │
       ▼
8. Another thread calls netpoll() (in findRunnable or sysmon)
   - epoll_wait/kevent returns the ready fd
   - netpollready() extracts the parked G from pd.rg
   - G is added to the run queue
       │
       ▼
9. Scheduler picks up the goroutine
       │
       ▼
10. Goroutine resumes in FD.Read loop, retries syscall.Read
    - This time data is available → returns to caller
```

### Why This Matters

This design achieves the best of both worlds:

| Approach            | Programmer Experience | OS Thread Usage |
|--------------------|-----------------------|-----------------|
| Blocking I/O       | Simple (synchronous)  | 1 thread per connection |
| Callbacks/async    | Complex (fragmented)  | Efficient       |
| **Go's approach**  | **Simple (synchronous)** | **Efficient** |

The programmer writes straightforward sequential code:
```go
for {
    n, err := conn.Read(buf)
    if err != nil {
        break
    }
    process(buf[:n])
}
```

But under the hood, the goroutine parks when no data is available, freeing
the OS thread to run other goroutines. When data arrives, the poller wakes
the goroutine and it seamlessly resumes.

---

## 7. The Big Picture: How All the Pieces Fit Together (5 min)

```
┌─────────────────────────────────────────────────────────────┐
│                     User Code                                │
│   conn.Read(buf)    file.Write(data)    http.Get(url)       │
└────────────┬────────────────┬───────────────────┬───────────┘
             │                │                   │
             ▼                ▼                   ▼
┌─────────────────────────────────────────────────────────────┐
│                    os / net packages                         │
│   os.File              net.Conn           http.Client       │
└────────────┬────────────────┬───────────────────┬───────────┘
             │                │                   │
             ▼                ▼                   ▼
┌─────────────────────────────────────────────────────────────┐
│                  internal/poll.FD                            │
│   Non-blocking I/O    EAGAIN retry loop    fdMutex          │
└────────────┬──────────────────────────────────┬─────────────┘
             │                                  │
    syscall.Read/Write                 fd.pd.waitRead/waitWrite
             │                                  │
             ▼                                  ▼
┌────────────────────┐          ┌──────────────────────────────┐
│   Linux kernel     │          │   runtime netpoll             │
│   (actual I/O)     │          │                               │
└────────────────────┘          │   pollDesc (per-fd state)     │
                                │   rg/wg semaphores            │
                                │                               │
                                │   ┌─────────┐  ┌──────────┐  │
                                │   │ epoll   │  │ kqueue   │  │
                                │   │ (Linux) │  │ (macOS)  │  │
                                │   └─────────┘  └──────────┘  │
                                └──────────────┬───────────────┘
                                               │
                                               ▼
                                ┌──────────────────────────────┐
                                │   Scheduler (findRunnable)    │
                                │                               │
                                │   netpoll() returns gList     │
                                │   → goroutines go to run queue│
                                │   → goroutines resume I/O     │
                                └──────────────────────────────┘
```

### Connections to Previous Modules

- **Module 4 (Scheduler)**: `findRunnable()` integrates poller checks into the
  scheduling loop. The poller is checked both in the fast path (non-blocking poll)
  and as a last resort (blocking poll when no other work exists).

- **Module 6 (Synchronization)**: The `fdMutex` in `poll.FD` is a specialized
  lock that supports concurrent readers/writers while managing close semantics.
  The `pollDesc.rg/wg` semaphores use atomic CAS operations.

- **Module 9 (Stacks)**: When a goroutine parks on I/O, its stack remains
  allocated but can be shrunk by the GC if it sits idle long enough.

---

## Summary

| Component | Role | File |
|-----------|------|------|
| `os.File` | User-facing file type | `os/file_unix.go` |
| `fs.FS` | Filesystem interface | `io/fs/fs.go` |
| `poll.FD` | Non-blocking I/O wrapper | `internal/poll/fd_unix.go` |
| `pollDesc` | Per-fd poller state | `runtime/netpoll.go` |
| `netpoll()` | Platform polling | `runtime/netpoll_epoll.go`, `runtime/netpoll_kqueue.go` |
| `findRunnable()` | Scheduler integration | `runtime/proc.go` |

### Key Takeaways

1. **Go files are opened in non-blocking mode** so that `EAGAIN` can be handled
   by parking the goroutine instead of blocking the OS thread.

2. **The EAGAIN retry loop** in `poll.FD.Read` is the core mechanism: try the
   syscall, if EAGAIN then park on the poller, wake up and retry.

3. **pollDesc semaphores** (rg/wg) track whether a goroutine is waiting for
   read/write readiness on each file descriptor.

4. **Edge-triggered polling** (EPOLLET / EV_CLEAR) avoids redundant wakeups --
   the poller only notifies on state transitions.

5. **The scheduler calls netpoll()** to discover goroutines whose I/O is ready,
   seamlessly integrating I/O events with goroutine scheduling.

6. **The result**: programmers write simple blocking I/O code, but the runtime
   multiplexes thousands of connections across a small number of OS threads.

### Discussion Questions

1. What happens when you pass an `os.File` to C code via cgo? Can the poller
   still manage it, or does the fd revert to blocking mode?

2. Regular files are always reported as ready by epoll on Linux (making epoll
   effectively useless for them), and regular files on macOS do not support
   kqueue at all. Given this, how does Go handle `os.File.Read` on a regular
   file? (Hint: check the `pollable` logic in `newFile`.)

3. Why does the poller use edge-triggered mode instead of level-triggered? What
   would go wrong with level-triggered notifications in Go's model?

4. The `netpollBreak` function uses a CAS to avoid duplicate wakeups. Why is this
   optimization important? What would happen without it?
