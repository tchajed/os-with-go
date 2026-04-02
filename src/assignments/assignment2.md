# Assignment 2: Simplified Goroutine Scheduler

**Reinforces:** Module 4 (The Go Scheduler), Module 5 (Work Stealing and Preemption)

## Overview

In this assignment, you will implement a simplified version of Go's GMP scheduler as a user-space library. You will use real OS threads as Ms, a struct with a run queue as P, and closures as Gs. Your scheduler must support local run queues, a global run queue, work stealing between Ps, and a basic preemption mechanism.

This hands-on implementation will solidify your understanding of why the GMP model exists, the tradeoffs in scheduling design, and the subtlety of concurrent data structures in the scheduler.

## Learning Objectives

By completing this assignment, you will be able to:

1. Implement the GMP scheduling model with correct concurrency control.
2. Build a lock-free (or low-lock) work queue with work stealing.
3. Implement cooperative and timer-based preemption of tasks.
4. Reason about thread parking, spinning, and wakeup strategies.
5. Measure and analyze the performance characteristics of your scheduler.

## Specification

### Core Types

```go
package scheduler

import (
	"sync"
	"sync/atomic"
	"time"
)

// Task represents a unit of work (analogous to a G).
type Task struct {
	id       int64
	fn       func()          // The function to execute
	state    int32           // TaskRunnable, TaskRunning, TaskDone
	// Add fields as needed for preemption, scheduling metadata, etc.
}

const (
	TaskRunnable = iota
	TaskRunning
	TaskDone
)

// Processor represents a scheduling context (analogous to a P).
type Processor struct {
	id       int
	runq     [256]*Task      // Local circular run queue
	runqHead uint32
	runqTail uint32
	runnext  atomic.Pointer[Task]  // Fast-path for producer-consumer patterns

	// Add fields as needed
}

// Machine represents a worker thread (analogous to an M).
// Each Machine runs on its own OS goroutine (acting as a thread).
type Machine struct {
	id       int
	proc     *Processor      // Currently attached P (may be nil)
	spinning bool

	// Add fields as needed
}

// Scheduler is the global scheduler state (analogous to the sched struct).
type Scheduler struct {
	processors []*Processor
	globalRunq []*Task        // Global run queue (protected by lock)
	globalLock sync.Mutex

	// Add fields for idle machines, spinning count, etc.
}
```

### Required Functions

#### Part A: Basic Scheduling (35 points)

Implement the core scheduling loop:

```go
// NewScheduler creates a scheduler with the given number of Ps.
// It starts one M (OS goroutine) per P.
func NewScheduler(numProcessors int) *Scheduler

// Spawn adds a new task to the scheduler. It should be placed on the
// current P's local run queue if called from a worker, or on the
// global run queue otherwise. If a local queue is full, shed half
// to the global queue (like runqputslow).
func (s *Scheduler) Spawn(fn func())

// schedule is the main scheduling loop for each M.
// It finds a runnable task and executes it, then repeats.
// It should check: (1) local run queue, (2) global run queue
// (every 61st tick), (3) try work stealing.
func (m *Machine) schedule()

// findRunnable finds the next task to run, blocking if necessary.
func (m *Machine) findRunnable() *Task

// Shutdown gracefully stops the scheduler after all tasks complete.
func (s *Scheduler) Shutdown()
```

Your `schedule()` function must:
1. Check `runnext` first (fast path for communicating tasks).
2. Check the local run queue.
3. Check the global run queue every 61st scheduling tick.
4. If no local work, attempt work stealing.
5. If no work anywhere, park the machine (block on a condition variable or channel).

#### Part B: Work Stealing (30 points)

Implement work stealing between processors:

```go
// runqSteal steals half the tasks from another processor's run queue
// and puts them in the caller's run queue. Returns the first stolen
// task (to execute immediately), or nil if nothing was stolen.
func (p *Processor) runqSteal(victim *Processor) *Task

// stealWork tries to steal from a random processor.
// Makes up to 4 attempts with randomized victim selection.
func (m *Machine) stealWork() *Task
```

Requirements for work stealing:
- Steal half the victim's run queue (not all of it).
- Use atomic operations for the lock-free run queue (or a mutex if you prefer, but document the tradeoff).
- Randomize the starting victim to avoid contention.
- On the last attempt, also try to steal from `runnext`.
- Track stealing statistics (number of steal attempts, successes, tasks stolen).

#### Part C: Preemption (20 points)

Implement a preemption mechanism:

```go
// Each task gets a time quantum. If a task runs longer than the quantum,
// it should be preempted and placed back on the run queue.
const TimeQuantum = 10 * time.Millisecond
```

Since you cannot preempt arbitrary Go code mid-execution (that would require signal handling), implement **cooperative preemption** with a check function:

```go
// ShouldYield returns true if the current task has exceeded its time quantum.
// Long-running tasks should call this periodically.
func (s *Scheduler) ShouldYield() bool

// Yield voluntarily yields the current task's execution, placing it back
// on the run queue. Returns when the task is rescheduled.
func (s *Scheduler) Yield()
```

Also implement a **monitor goroutine** (analogous to `sysmon`) that:
- Runs on its own goroutine (not tied to a P).
- Periodically checks for tasks running longer than `TimeQuantum`.
- Sets a preemption flag on long-running tasks.
- Detects and reports if any processor has been idle for too long while work exists elsewhere.

#### Part D: Performance Evaluation (15 points)

Write benchmarks that compare your scheduler against Go's native goroutine scheduler:

```go
// BenchmarkFanOut creates N tasks that each spawn M subtasks.
// Compare: your scheduler vs. native goroutines.
func BenchmarkFanOut(b *testing.B)

// BenchmarkPingPong creates pairs of tasks that alternate execution
// (simulating channel-like communication).
func BenchmarkPingPong(b *testing.B)

// BenchmarkWorkStealingEfficiency creates an imbalanced workload
// (all tasks initially on one P) and measures how quickly
// work is distributed.
func BenchmarkWorkStealingEfficiency(b *testing.B)
```

Write a short analysis (in comments or a separate file) discussing:
- How does your scheduler's throughput compare to Go's? Why?
- What is the overhead of your work stealing vs. Go's lock-free implementation?
- How effective is your preemption mechanism?

## Starter Code Structure

```
assignment2/
├── scheduler/
│   ├── scheduler.go         # Scheduler, NewScheduler, Spawn, Shutdown
│   ├── processor.go         # Processor, run queue operations
│   ├── machine.go           # Machine, schedule(), findRunnable()
│   ├── stealing.go          # Work stealing implementation
│   ├── preemption.go        # Preemption and sysmon-like monitor
│   ├── scheduler_test.go    # Correctness tests
│   └── scheduler_bench_test.go  # Performance benchmarks
├── cmd/
│   └── demo/
│       └── main.go          # Demo program showing the scheduler in action
└── go.mod
```

## Grading Rubric

| Component | Points | Criteria |
|-----------|--------|----------|
| Part A: Basic scheduling loop | 15 | Correct schedule/findRunnable with local+global queues |
| Part A: Spawn and run queue | 10 | Correct enqueue with overflow to global queue |
| Part A: Machine parking/wakeup | 10 | Machines park when idle and wake when work arrives |
| Part B: runqSteal | 15 | Correctly steals half; handles edge cases (empty queue, single item) |
| Part B: stealWork | 10 | Randomized victim selection; 4 attempts; runnext on last try |
| Part B: Statistics | 5 | Tracks steal attempts, successes, total tasks stolen |
| Part C: Cooperative preemption | 10 | ShouldYield/Yield work correctly |
| Part C: Monitor goroutine | 10 | Detects long-running tasks; sets preempt flag |
| Part D: Benchmarks | 10 | Three benchmarks with meaningful analysis |
| Part D: Analysis | 5 | Thoughtful comparison with Go's scheduler |
| **Total** | **100** | |

## Hints

1. **Start simple.** Get Part A working with mutexes everywhere before optimizing. A correct mutex-based scheduler is worth more than a buggy lock-free one.

2. **Use Go goroutines as your "OS threads."** Each Machine's `schedule()` loop runs in its own goroutine, which Go maps to an OS thread. This is a legitimate simplification -- you are building a scheduler on top of Go's scheduler, focusing on the algorithm rather than the OS interface.

3. **For task preemption**, since you cannot interrupt a running closure, have your long-running tasks call `s.ShouldYield()` in their inner loops. The monitor goroutine sets a flag; `ShouldYield()` checks it. This mirrors Go's cooperative preemption at function prologues.

4. **Machine parking**: Use `sync.Cond` or a channel to park idle machines. When `Spawn()` adds work, signal a parked machine to wake up. Be careful about the "lost wakeup" problem (the same issue `futexsleep` solves with its atomic check).

5. **For the lock-free run queue**, study how `runtime/proc.go` implements `runqput`, `runqget`, `runqgrab` using `atomic.LoadAcq` and `atomic.StoreRel`. The key insight is that the queue is single-producer (owning P) and multi-consumer (stealing Ps), which simplifies the protocol.

6. **The 61-tick global queue check**: Use an atomic counter on each Processor. Every 61st call to `findRunnable`, check the global queue first. This prevents starvation of globally-queued tasks.

7. **Testing concurrency**: Use `go test -race` religiously. Your scheduler is a concurrent data structure and race conditions will be subtle. Write tests that spawn thousands of tasks to surface races.

8. **Spinning threads**: Start without the spinning optimization. Once basic stealing works, add it: a machine that has no local work and is searching for work is "spinning." Limit the number of spinning machines to avoid wasting CPU. When you `Spawn()`, only wake a parked machine if there are no spinning machines.
