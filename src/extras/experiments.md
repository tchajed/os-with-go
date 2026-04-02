# Runtime Experiments

Hands-on experiments to observe Go runtime behavior. These complement the lecture
materials by letting you see the runtime in action.

---

## Experiment 1: Observing the Scheduler

### 1.1: schedtrace

Run any Go program with scheduler tracing enabled:

```bash
GODEBUG=schedtrace=1000 go run myprogram.go
```

Output looks like:
```
SCHED 0ms: gomaxprocs=8 idleprocs=6 threads=4 spinningthreads=1
  idlethreads=0 runqueue=0 [0 0 0 0 0 0 0 0]
```

**What to observe:**
- `gomaxprocs`: number of Ps (= GOMAXPROCS)
- `idleprocs`: Ps with no work
- `threads`: total OS threads (Ms) created
- `spinningthreads`: Ms looking for work
- `runqueue`: global run queue length
- `[0 0 ...]`: per-P local run queue lengths

**Try:** Run a program that creates 1000 goroutines doing CPU work. Watch how
the run queues fill up and balance across Ps.

### 1.2: scheddetail

For more detail:

```bash
GODEBUG=schedtrace=1000,scheddetail=1 go run myprogram.go
```

This shows the state of every G, M, and P.

### 1.3: Execution Tracer

```go
package main

import (
    "os"
    "runtime/trace"
    "sync"
)

func main() {
    f, _ := os.Create("trace.out")
    trace.Start(f)
    defer trace.Stop()

    var wg sync.WaitGroup
    for i := 0; i < 10; i++ {
        wg.Add(1)
        go func(n int) {
            defer wg.Done()
            // do some work
            sum := 0
            for j := 0; j < 1000000; j++ {
                sum += j
            }
            _ = sum
        }(i)
    }
    wg.Wait()
}
```

View with:
```bash
go tool trace trace.out
```

**What to observe:**
- Goroutine creation and scheduling on Ps
- Work stealing events
- GC pauses
- System call blocking

---

## Experiment 2: System Call Overhead

### 2.1: Measuring syscall cost

```go
package main

import (
    "fmt"
    "os"
    "syscall"
    "time"
)

func main() {
    // Measure getpid (a trivial syscall)
    n := 1000000
    start := time.Now()
    for i := 0; i < n; i++ {
        syscall.Getpid()
    }
    elapsed := time.Since(start)
    fmt.Printf("syscall.Getpid: %v per call\n", elapsed/time.Duration(n))

    // Measure file read (1 byte from /dev/null)
    f, _ := os.Open("/dev/null")
    buf := make([]byte, 1)
    start = time.Now()
    for i := 0; i < n; i++ {
        f.Read(buf)
    }
    elapsed = time.Since(start)
    fmt.Printf("os.File.Read:   %v per call\n", elapsed/time.Duration(n))
    f.Close()
}
```

### 2.2: Tracing system calls

```bash
# Linux
strace -c go run myprogram.go

# macOS
sudo dtruss go run myprogram.go 2>&1 | tail -20
```

**What to observe:**
- Which system calls Go programs actually make
- The ratio of runtime syscalls vs application syscalls
- How many `futex`/`psynch` calls the scheduler makes

---

## Experiment 3: Goroutine Stack Growth

### 3.1: Observing stack growth

```go
package main

import (
    "fmt"
    "runtime"
)

func recurse(depth int) {
    var buf [64]byte // force some stack usage
    _ = buf
    if depth > 0 {
        recurse(depth - 1)
    }
    if depth == 0 {
        var m runtime.MemStats
        runtime.ReadMemStats(&m)
        fmt.Printf("Stack in use: %d bytes\n", m.StackInuse)
    }
}

func main() {
    fmt.Println("Initial stack size: 2KB (minimum)")
    recurse(10)
    fmt.Println("After shallow recursion:")
    recurse(10)

    fmt.Println("\nAfter deep recursion:")
    recurse(10000)
}
```

### 3.2: Stack size limits

```go
package main

import (
    "fmt"
    "runtime/debug"
)

func main() {
    // Default max stack size is 1GB
    // You can set it lower to observe stack overflow
    debug.SetMaxStack(1 << 20) // 1MB max

    var f func(int)
    f = func(n int) {
        if n > 0 {
            f(n - 1)
        }
    }
    f(1000000) // will hit the 1MB limit
    fmt.Println("done")
}
```

---

## Experiment 4: Channel Performance

### 4.1: Buffered vs unbuffered throughput

```go
package main

import (
    "fmt"
    "time"
)

func benchmark(name string, ch chan int) {
    n := 1000000
    start := time.Now()
    go func() {
        for i := 0; i < n; i++ {
            ch <- i
        }
    }()
    for i := 0; i < n; i++ {
        <-ch
    }
    elapsed := time.Since(start)
    fmt.Printf("%s: %v per op\n", name, elapsed/time.Duration(n))
}

func main() {
    benchmark("unbuffered", make(chan int))
    benchmark("buffered-1", make(chan int, 1))
    benchmark("buffered-100", make(chan int, 100))
    benchmark("buffered-10000", make(chan int, 10000))
}
```

**What to observe:**
- Unbuffered channels require goroutine synchronization on every send
- Buffered channels amortize the cost
- Very large buffers don't help much (cache effects)

### 4.2: Select with many cases

```go
package main

import (
    "fmt"
    "time"
)

func main() {
    for nch := 2; nch <= 64; nch *= 2 {
        channels := make([]chan int, nch)
        for i := range channels {
            channels[i] = make(chan int, 1)
        }

        n := 100000
        start := time.Now()
        for i := 0; i < n; i++ {
            // Send on a random channel
            channels[i%nch] <- 1

            // Select across all channels (using reflect for dynamic select)
            // In practice, measure compile-time selects with fixed case counts
            <-channels[i%nch]
        }
        elapsed := time.Since(start)
        fmt.Printf("select with %d channels: %v per op\n", nch, elapsed/time.Duration(n))
    }
}
```

---

## Experiment 5: Observing GC

### 5.1: GC trace output

```bash
GODEBUG=gctrace=1 go run myprogram.go
```

Output looks like:
```
gc 1 @0.012s 2%: 0.11+1.2+0.063 ms clock, 0.89+0.45/1.0/0+0.50 ms cpu, 4->4->1 MB, 4 MB goal, 0 MB stacks, 0 MB globals, 8 P
```

**Fields:**
- `gc 1`: GC number
- `@0.012s`: time since program start
- `2%`: percentage of CPU used by GC
- `0.11+1.2+0.063 ms clock`: STW sweep term + concurrent mark + STW mark term
- `4->4->1 MB`: heap before → heap after → live data
- `8 P`: number of processors

### 5.2: Allocation pressure

```go
package main

import (
    "fmt"
    "runtime"
    "time"
)

func main() {
    var m runtime.MemStats

    // Allocate lots of small objects
    start := time.Now()
    for i := 0; i < 10000000; i++ {
        _ = make([]byte, 64)
    }
    elapsed := time.Since(start)

    runtime.ReadMemStats(&m)
    fmt.Printf("Time: %v\n", elapsed)
    fmt.Printf("Total allocs: %d\n", m.Mallocs)
    fmt.Printf("GC cycles: %d\n", m.NumGC)
    fmt.Printf("Total GC pause: %v\n", time.Duration(m.PauseTotalNs))
    fmt.Printf("Avg GC pause: %v\n", time.Duration(m.PauseTotalNs/uint64(m.NumGC)))
}
```

---

## Experiment 6: Preemption

### 6.1: Non-preemptible goroutine (pre-Go 1.14)

```go
package main

import (
    "fmt"
    "runtime"
    "time"
)

func main() {
    runtime.GOMAXPROCS(1)

    go func() {
        // Tight loop with no function calls
        // In Go 1.14+, this will be preempted via SIGURG
        // (asynchronous signal-based preemption)
        // In older Go, this would monopolize the P
        for {
            // Go 1.14+ uses signal-based async preemption (SIGURG),
            // so even tight loops without function calls are preemptible
        }
    }()

    time.Sleep(100 * time.Millisecond)
    fmt.Println("Main goroutine ran — preemption works!")
}
```

### 6.2: Observing sysmon

The sysmon thread runs independently and preempts goroutines that run for >10ms.
You can observe its effects with the execution tracer.

---

## Experiment 7: Network Poller Integration

### 7.1: Many concurrent connections

```go
package main

import (
    "fmt"
    "net"
    "runtime"
    "sync"
    "time"
)

func main() {
    // Start a simple echo server
    ln, _ := net.Listen("tcp", "127.0.0.1:0")
    addr := ln.Addr().String()

    go func() {
        for {
            conn, err := ln.Accept()
            if err != nil {
                return
            }
            go func(c net.Conn) {
                buf := make([]byte, 1024)
                for {
                    n, err := c.Read(buf)
                    if err != nil {
                        return
                    }
                    c.Write(buf[:n])
                }
            }(conn)
        }
    }()

    // Open many concurrent connections
    nConns := 1000
    var wg sync.WaitGroup
    start := time.Now()

    for i := 0; i < nConns; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            conn, _ := net.Dial("tcp", addr)
            defer conn.Close()
            msg := []byte("hello")
            conn.Write(msg)
            buf := make([]byte, len(msg))
            conn.Read(buf)
        }()
    }

    wg.Wait()
    elapsed := time.Since(start)
    ln.Close()

    fmt.Printf("%d connections in %v\n", nConns, elapsed)
    fmt.Printf("OS threads used: %d\n", runtime.GOMAXPROCS(0))
    fmt.Println("(Each connection used a goroutine, not an OS thread)")
}
```

**What to observe:**
- 1000 concurrent connections handled by GOMAXPROCS threads
- The network poller (epoll/kqueue) multiplexes all the I/O
- Goroutines park on I/O and wake when data arrives

---

## Tips for Exploration

1. **Read the source with an editor**, not a browser. IDE "go to definition"
   makes it easy to follow function calls through the runtime.

2. **Set breakpoints in the runtime** with Delve:
   ```bash
   dlv debug myprogram.go
   (dlv) break runtime.schedule
   (dlv) continue
   ```

3. **Use `go build -gcflags='-S'`** to see the compiler output, including
   stack growth checks and function prologues.

4. **Read the comments first.** The Go runtime has excellent documentation
   in source comments, especially at the top of `proc.go`, `malloc.go`,
   `mgc.go`, and `chan.go`.
