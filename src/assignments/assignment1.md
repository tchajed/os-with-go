# Assignment 1: System Call Tracer and Scheduler Event Analyzer

**Reinforces:** Module 2 (System Calls), Module 3 (Processes, Threads, Goroutines)

## Overview

In this assignment, you will build a tool that traces and analyzes the interaction between Go programs and the operating system. You will use Go's `runtime/trace` package to capture scheduler events and system call behavior, then build an analyzer that processes the trace data to produce human-readable reports about goroutine lifecycle, system call patterns, and scheduler decisions.

This assignment gives you hands-on experience with the boundary between the Go runtime and the OS kernel, and helps you understand how goroutines map to threads and how system calls affect scheduling.

## Learning Objectives

By completing this assignment, you will be able to:

1. Explain how the Go runtime interacts with the kernel via system calls.
2. Use `runtime/trace` to capture and interpret scheduler events.
3. Identify the relationship between goroutines (Gs), threads (Ms), and processors (Ps) in a running program.
4. Analyze how blocking system calls cause P handoffs and thread creation.
5. Measure and reason about the overhead of system calls on goroutine scheduling.

## Specification

### Part A: Trace Capture Wrapper (30 points)

Build a package `tracer` that wraps a workload function and captures a runtime trace.

```go
package tracer

import (
	"os"
	"runtime/trace"
)

// CaptureTrace runs the given function while recording a runtime trace
// to the specified output file. Returns an error if tracing cannot be started.
func CaptureTrace(outputFile string, workload func()) error {
	// TODO: Implement
	// 1. Create the output file
	// 2. Start tracing with trace.Start()
	// 3. Run the workload
	// 4. Stop tracing with trace.Stop()
	// 5. Close the file
	return nil
}
```

You must also implement three workload functions that exercise different runtime behaviors:

```go
// CPUBoundWorkload spawns N goroutines that each perform pure computation
// (no I/O, no syscalls) for the given duration.
func CPUBoundWorkload(numGoroutines int, duration time.Duration)

// IOBoundWorkload spawns N goroutines that each perform file I/O operations
// (creating temp files, writing, reading, deleting).
func IOBoundWorkload(numGoroutines int, numOps int)

// MixedWorkload spawns goroutines that communicate over channels,
// perform some computation, and do some file I/O.
func MixedWorkload(numProducers, numConsumers int, itemsPerProducer int)
```

### Part B: Trace Analyzer (50 points)

Build a `analyzer` package that reads a trace file (produced by `runtime/trace`) and extracts statistics. You will use the `internal/trace` or `golang.org/x/exp/trace` package to parse trace events.

Your analyzer must produce a report with the following information:

#### B.1: Goroutine Statistics
- Total number of goroutines created
- Maximum number of concurrently running goroutines
- Distribution of goroutine lifetimes (min, max, median, p99)
- Number of goroutines that blocked on syscalls vs. channels vs. mutexes

#### B.2: Thread (M) Statistics
- Maximum number of OS threads active
- Number of thread creations during the trace
- Time spent with threads parked (idle) vs. active

#### B.3: Syscall Analysis
- Count of distinct syscall-induced scheduling events (goroutine entering/exiting syscall state)
- Number of P handoffs caused by long syscalls
- Average and maximum syscall durations

#### B.4: Scheduler Event Timeline
- For each P, produce a timeline showing which goroutines ran on it and when context switches occurred
- Identify the longest uninterrupted goroutine execution (potential scheduling latency issue)

```go
package analyzer

// Report contains the analyzed trace statistics.
type Report struct {
	Goroutines   GoroutineStats
	Threads      ThreadStats
	Syscalls     SyscallStats
	Timeline     []PTimeline
}

// Analyze reads a trace file and produces a Report.
func Analyze(traceFile string) (*Report, error) {
	// TODO: Implement
	return nil, nil
}

// String returns a human-readable summary of the report.
func (r *Report) String() string {
	// TODO: Implement
	return ""
}
```

### Part C: Comparative Analysis Report (20 points)

Write a short report (1-2 pages) that:

1. Runs each of the three workloads from Part A with at least two different values of `GOMAXPROCS` (e.g., 1, 4, and the number of CPUs).
2. Analyzes the traces using your Part B analyzer.
3. Answers these questions with evidence from your traces:
   - How does the number of OS threads compare to GOMAXPROCS for each workload type? Why?
   - In the I/O-bound workload, how many P handoffs occurred? What triggered them?
   - In the CPU-bound workload, did any goroutine run for more than 10ms without being preempted? If so, explain why.
   - How does channel communication in the mixed workload affect goroutine scheduling (look at `runnext` usage)?

## Starter Code Structure

```
assignment1/
├── cmd/
│   └── tracer/
│       └── main.go          # CLI tool: capture trace and/or analyze
├── tracer/
│   ├── capture.go           # CaptureTrace function
│   └── workloads.go         # Three workload functions
├── analyzer/
│   ├── analyzer.go          # Trace parsing and analysis
│   ├── report.go            # Report struct and formatting
│   └── analyzer_test.go     # Tests with known trace files
├── go.mod
└── report/
    └── analysis.md          # Your Part C report
```

## Grading Rubric

| Component | Points | Criteria |
|-----------|--------|----------|
| Part A: CaptureTrace | 10 | Correctly captures traces; handles errors |
| Part A: Workloads | 20 | Three distinct workloads that exercise different runtime paths |
| Part B: Goroutine stats | 15 | Accurate goroutine count, lifetime distribution, blocking reasons |
| Part B: Thread stats | 10 | Accurate thread count and utilization |
| Part B: Syscall analysis | 15 | Correct identification of syscall events and P handoffs |
| Part B: Timeline | 10 | Per-P timeline with context switch identification |
| Part C: Analysis report | 20 | Correct interpretation with evidence from traces |
| **Total** | **100** | |

## Hints

1. Start with `go tool trace` to visually inspect your trace files before writing the analyzer. This gives you ground truth to validate against.

2. The `runtime/trace` package is straightforward for capture. The harder part is parsing. Look at `golang.org/x/exp/trace` for a stable parsing API, or study how `cmd/trace` parses events internally.

3. For the I/O workload, use `os.CreateTemp`, `os.WriteFile`, and `os.ReadFile`. These will trigger real blocking syscalls (unlike network I/O, which uses the poller).

4. To detect P handoffs in your analyzer, look for events where a goroutine's P assignment changes between entering and exiting a syscall.

5. For the mixed workload, use buffered and unbuffered channels to see different scheduling behaviors.

6. Run with `GODEBUG=schedtrace=1000` alongside your tracer to cross-reference scheduler state.

7. The `runtime.NumGoroutine()` and `runtime.GOMAXPROCS(0)` functions can help instrument your workloads.

8. Keep your workloads running for at least 1-2 seconds to get meaningful trace data, but not so long that trace files become unmanageably large.
