// Package tasks is a process-level async task store backing the
// submit/get/cancel/list MCP tools. It lets a client fire a long-running
// search in the background and poll for the result, instead of blocking a
// single tools/call for the whole upstream latency.
//
// This is a Go-native rewrite of GrokSearch's task_store.py: asyncio.Task
// becomes a goroutine, cancellation becomes context.CancelFunc, and the
// done-event becomes a closed channel. The store is kind-agnostic — it runs
// any Runner and stores a JSON-serializable result — so the search layer
// stays decoupled from task plumbing.
//
// Lifecycle: queued -> running -> (completed | failed | cancelled). Capacity
// is bounded (default 256) with terminal-first LRU eviction so in-flight
// tasks are never evicted out from under a poller.
package tasks

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// State is a task's lifecycle stage.
type State string

const (
	StateQueued    State = "queued"
	StateRunning   State = "running"
	StateCompleted State = "completed"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
)

func (s State) terminal() bool {
	return s == StateCompleted || s == StateFailed || s == StateCancelled
}

// defaultMaxTasks bounds the store; oldest terminal tasks are evicted first.
const defaultMaxTasks = 256

// Runner performs the actual work for a task. It must honor ctx cancellation
// so Cancel can interrupt an in-flight task. The returned value must be
// JSON-serializable; it is exposed verbatim as the task result.
type Runner func(ctx context.Context) (any, error)

// Task is a snapshot-friendly record of one submitted unit of work. The
// exported fields are safe to read after the task is terminal; while running,
// read snapshots via the Store methods (which lock) rather than touching
// fields directly.
type Task struct {
	ID          string
	Kind        string
	State       State
	Params      map[string]any
	SubmittedAt time.Time
	StartedAt   time.Time
	FinishedAt  time.Time
	Result      any
	Err         string
	CancelHint  string

	cancel context.CancelFunc
	done   chan struct{}
}

// Snapshot is an immutable copy of a task's observable state, returned to
// callers so they never race with the running goroutine.
type Snapshot struct {
	ID          string         `json:"task_id"`
	Kind        string         `json:"kind"`
	State       State          `json:"state"`
	Params      map[string]any `json:"params,omitempty"`
	SubmittedAt time.Time      `json:"submitted_at"`
	StartedAt   *time.Time     `json:"started_at,omitempty"`
	FinishedAt  *time.Time     `json:"finished_at,omitempty"`
	Result      any            `json:"result,omitempty"`
	Err         string         `json:"error,omitempty"`
	CancelHint  string         `json:"cancel_hint,omitempty"`
}

func (t *Task) snapshot() Snapshot {
	s := Snapshot{
		ID:          t.ID,
		Kind:        t.Kind,
		State:       t.State,
		Params:      t.Params,
		SubmittedAt: t.SubmittedAt,
		Result:      t.Result,
		Err:         t.Err,
		CancelHint:  t.CancelHint,
	}
	if !t.StartedAt.IsZero() {
		st := t.StartedAt
		s.StartedAt = &st
	}
	if !t.FinishedAt.IsZero() {
		ft := t.FinishedAt
		s.FinishedAt = &ft
	}
	return s
}

// Store is a concurrency-safe async task registry.
type Store struct {
	mu       sync.Mutex
	records  map[string]*Task
	order    []string // submission order, used for LRU eviction
	maxTasks int
	seq      uint64
	now      func() time.Time // injectable for tests
}

// Option configures a Store.
type Option func(*Store)

// WithMaxTasks overrides the capacity bound (values < 1 are ignored).
func WithMaxTasks(n int) Option {
	return func(s *Store) {
		if n >= 1 {
			s.maxTasks = n
		}
	}
}

// withClock injects a clock for deterministic tests.
func withClock(now func() time.Time) Option {
	return func(s *Store) { s.now = now }
}

// NewStore constructs an empty store.
func NewStore(opts ...Option) *Store {
	s := &Store{
		records:  make(map[string]*Task),
		maxTasks: defaultMaxTasks,
		now:      time.Now,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Submit registers a task and starts running it immediately in a goroutine.
// The runner receives a cancelable context derived from ctx; Cancel(id)
// triggers that cancellation. The returned Snapshot is in the queued/running
// state — poll Get for the result.
func (s *Store) Submit(ctx context.Context, kind string, params map[string]any, runner Runner) Snapshot {
	runCtx, cancel := context.WithCancel(ctx)
	t := &Task{
		ID:          s.newID(),
		Kind:        kind,
		State:       StateQueued,
		Params:      params,
		SubmittedAt: s.now(),
		cancel:      cancel,
		done:        make(chan struct{}),
	}

	s.mu.Lock()
	s.records[t.ID] = t
	s.order = append(s.order, t.ID)
	s.evictLocked()
	// Snapshot while holding the lock and before launching the goroutine, so
	// the returned value can never race with run()'s state transitions.
	snap := t.snapshot()
	s.mu.Unlock()

	go s.run(runCtx, t, runner)
	return snap
}

// run executes the runner and transitions the task to a terminal state. It is
// the only writer of a task's mutable fields after Submit; readers go through
// locked snapshot methods.
func (s *Store) run(ctx context.Context, t *Task, runner Runner) {
	s.mu.Lock()
	t.State = StateRunning
	t.StartedAt = s.now()
	s.mu.Unlock()

	result, err := runner(ctx)

	s.mu.Lock()
	t.FinishedAt = s.now()
	switch {
	case ctx.Err() == context.Canceled:
		// Cancellation wins even if the runner returned an error on the way
		// out: the operator asked to stop, so report cancelled.
		t.State = StateCancelled
		if t.CancelHint == "" {
			t.CancelHint = "cancelled"
		}
	case err != nil:
		t.State = StateFailed
		t.Err = err.Error()
	default:
		t.State = StateCompleted
		t.Result = result
	}
	s.mu.Unlock()
	t.cancel()    // release the context's resources
	close(t.done) // wake any long-poll waiters
}

// Get returns a snapshot of the task. When wait > 0 and the task is not yet
// terminal, it blocks up to wait for a terminal transition (long-poll). The
// bool is false when no task with that id exists.
func (s *Store) Get(ctx context.Context, id string, wait time.Duration) (Snapshot, bool) {
	t := s.peek(id)
	if t == nil {
		return Snapshot{}, false
	}
	if wait <= 0 {
		return s.lockedSnapshot(t), true
	}
	if snap, done := s.terminalSnapshot(t); done {
		return snap, true
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-t.done:
	case <-timer.C:
	case <-ctx.Done():
	}
	return s.lockedSnapshot(t), true
}

// Cancel requests cancellation of a non-terminal task. The hint is stored
// verbatim. Cancelling a terminal task is a no-op that returns its current
// snapshot. The bool is false when no task with that id exists.
func (s *Store) Cancel(id, hint string) (Snapshot, bool) {
	t := s.peek(id)
	if t == nil {
		return Snapshot{}, false
	}
	s.mu.Lock()
	if t.State.terminal() {
		snap := t.snapshot()
		s.mu.Unlock()
		return snap, true
	}
	if hint == "" {
		hint = "client"
	}
	t.CancelHint = hint
	cancel := t.cancel
	s.mu.Unlock()

	cancel() // run() observes ctx.Canceled and finalizes the state
	select {
	case <-t.done:
	case <-time.After(2 * time.Second):
		// run() has not observed cancellation yet (e.g. a runner ignoring
		// ctx); force a terminal snapshot so the caller gets feedback. The
		// goroutine will still finalize harmlessly when it returns.
		s.mu.Lock()
		if !t.State.terminal() {
			t.State = StateCancelled
			t.FinishedAt = s.now()
		}
		s.mu.Unlock()
	}
	return s.lockedSnapshot(t), true
}

// List returns snapshots sorted by submission time (oldest first), optionally
// filtered by state, kind, and a since cutoff (inclusive).
func (s *Store) List(states []State, kinds []string, since time.Time) []Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	stateSet := make(map[State]struct{}, len(states))
	for _, st := range states {
		if st != "" {
			stateSet[st] = struct{}{}
		}
	}
	kindSet := make(map[string]struct{}, len(kinds))
	for _, k := range kinds {
		if k != "" {
			kindSet[k] = struct{}{}
		}
	}

	var out []Snapshot
	for _, t := range s.records {
		if len(stateSet) > 0 {
			if _, ok := stateSet[t.State]; !ok {
				continue
			}
		}
		if len(kindSet) > 0 {
			if _, ok := kindSet[t.Kind]; !ok {
				continue
			}
		}
		if !since.IsZero() && t.SubmittedAt.Before(since) {
			continue
		}
		out = append(out, t.snapshot())
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].SubmittedAt.Before(out[j].SubmittedAt)
	})
	return out
}

func (s *Store) peek(id string) *Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.records[id]
}

func (s *Store) lockedSnapshot(t *Task) Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return t.snapshot()
}

// terminalSnapshot returns a snapshot and true only if the task is already
// terminal, avoiding the timer setup in Get's fast path.
func (s *Store) terminalSnapshot(t *Task) (Snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.State.terminal() {
		return t.snapshot(), true
	}
	return Snapshot{}, false
}

func (s *Store) newID() string {
	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.mu.Unlock()
	return fmt.Sprintf("task-%d-%d", time.Now().UnixNano(), seq)
}

// evictLocked drops oldest terminal tasks once capacity is exceeded; if the
// store is still over capacity (256+ concurrently in-flight) it falls back to
// dropping the oldest task regardless. Caller must hold s.mu.
func (s *Store) evictLocked() {
	if len(s.records) <= s.maxTasks {
		return
	}
	// First pass: evict oldest terminal tasks.
	kept := s.order[:0]
	for _, id := range s.order {
		t := s.records[id]
		if t == nil {
			continue
		}
		if len(s.records) > s.maxTasks && t.State.terminal() {
			delete(s.records, id)
			continue
		}
		kept = append(kept, id)
	}
	s.order = kept
	// Second pass: if still over capacity, drop oldest regardless.
	for len(s.records) > s.maxTasks && len(s.order) > 0 {
		id := s.order[0]
		s.order = s.order[1:]
		if t := s.records[id]; t != nil {
			t.cancel()
		}
		delete(s.records, id)
	}
}
