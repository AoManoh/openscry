package mcpserver

import (
	"context"
	"strconv"
	"sync"
)

// CancellationRegistry tracks the cancel funcs for in-flight tools/call
// requests so a `notifications/cancelled` notification can interrupt the
// matching long-running worker. Ported from openPic-mcp.
//
// The engine populates the registry on tools/call entry (Register) and
// clears it after the handler returns (Done). The protocol handler queries
// it (Cancel) when a cancel notification arrives. All operations are safe
// for concurrent callers.
type CancellationRegistry struct {
	mu      sync.Mutex
	pending map[string]context.CancelFunc
}

// NewCancellationRegistry constructs an empty registry.
func NewCancellationRegistry() *CancellationRegistry {
	return &CancellationRegistry{pending: make(map[string]context.CancelFunc)}
}

// Register records cancel under the request ID. If the same ID is registered
// twice (a duplicate-id bug elsewhere) the previous cancel is invoked before
// being replaced so the old request still unwinds.
func (r *CancellationRegistry) Register(id any, cancel context.CancelFunc) {
	key, ok := normalizeID(id)
	if !ok || cancel == nil {
		return
	}
	r.mu.Lock()
	prev, exists := r.pending[key]
	r.pending[key] = cancel
	r.mu.Unlock()
	if exists {
		prev()
	}
}

// Cancel triggers the cancel func registered for id, if any, and removes it.
// It returns true when a matching pending request was found.
func (r *CancellationRegistry) Cancel(id any) bool {
	key, ok := normalizeID(id)
	if !ok {
		return false
	}
	r.mu.Lock()
	cancel, found := r.pending[key]
	if found {
		delete(r.pending, key)
	}
	r.mu.Unlock()
	if found {
		cancel()
	}
	return found
}

// Done removes id from the registry without invoking cancel. The engine
// calls Done in the worker's defer so successful completions do not leak
// entries.
func (r *CancellationRegistry) Done(id any) {
	key, ok := normalizeID(id)
	if !ok {
		return
	}
	r.mu.Lock()
	delete(r.pending, key)
	r.mu.Unlock()
}

// Len returns the number of currently registered cancellations. Intended
// for tests and observability.
func (r *CancellationRegistry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}

// normalizeID converts a JSON-RPC id (string or number) into a stable map
// key. Numbers decoded from JSON arrive as float64; the "s:"/"n:" prefixes
// keep the string id "1" distinct from the numeric id 1.
func normalizeID(id any) (string, bool) {
	switch v := id.(type) {
	case nil:
		return "", false
	case string:
		return "s:" + v, true
	case float64:
		return "n:" + strconv.FormatFloat(v, 'f', -1, 64), true
	case int:
		return "n:" + strconv.Itoa(v), true
	case int64:
		return "n:" + strconv.FormatInt(v, 10), true
	default:
		return "", false
	}
}
