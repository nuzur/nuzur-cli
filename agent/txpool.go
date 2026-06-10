package agent

import (
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofrs/uuid"
	"github.com/jmoiron/sqlx"
)

// txIdleTimeout matches the plan: a transaction held idle longer than this is
// rolled back so the underlying DB connection isn't pinned forever when the
// cloud caller bails (gRPC stream death, ctx cancel, etc.).
const txIdleTimeout = 60 * time.Second

type txEntry struct {
	tx       *sqlx.Tx
	lastUsed atomic.Int64 // unix nano; bumped on every use
}

func (e *txEntry) touch() { e.lastUsed.Store(time.Now().UnixNano()) }

// txPool tracks open transactions keyed by an agent-side tx_id (a v4 UUID).
// Goroutine-safe.
type txPool struct {
	mu  sync.Mutex
	txs map[string]*txEntry
}

func newTxPool() *txPool { return &txPool{txs: make(map[string]*txEntry)} }

// Add registers a freshly-opened *sqlx.Tx and returns its tx_id.
func (p *txPool) Add(tx *sqlx.Tx) string {
	id := uuid.Must(uuid.NewV4()).String()
	e := &txEntry{tx: tx}
	e.touch()
	p.mu.Lock()
	p.txs[id] = e
	p.mu.Unlock()
	return id
}

// Get returns the tx for an id and refreshes its last-used timestamp. Returns
// (nil, false) if the id is unknown — typically because the idle reaper killed
// it.
func (p *txPool) Get(id string) (*sqlx.Tx, bool) {
	p.mu.Lock()
	e, ok := p.txs[id]
	p.mu.Unlock()
	if !ok {
		return nil, false
	}
	e.touch()
	return e.tx, true
}

// Pop atomically removes and returns the tx. Used by Commit/Rollback so the
// underlying DB connection is released regardless of which lifecycle command
// the cloud sent.
func (p *txPool) Pop(id string) (*sqlx.Tx, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.txs[id]
	if !ok {
		return nil, false
	}
	delete(p.txs, id)
	return e.tx, true
}

// reapIdle rolls back and drops any tx idle longer than txIdleTimeout. Called
// periodically by a daemon goroutine.
func (p *txPool) reapIdle() {
	cutoff := time.Now().Add(-txIdleTimeout).UnixNano()
	p.mu.Lock()
	var toReap []string
	for id, e := range p.txs {
		if e.lastUsed.Load() < cutoff {
			toReap = append(toReap, id)
		}
	}
	for _, id := range toReap {
		e := p.txs[id]
		delete(p.txs, id)
		go func(tx *sqlx.Tx, id string) {
			if err := tx.Rollback(); err != nil {
				log.Printf("idle tx %s rollback error: %v", id, err)
			} else {
				log.Printf("rolled back idle tx %s", id)
			}
		}(e.tx, id)
	}
	p.mu.Unlock()
}

// CloseAll rolls back every tx in the pool. Called at daemon shutdown so
// underlying DB connections are returned to their pools cleanly.
func (p *txPool) CloseAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, e := range p.txs {
		_ = e.tx.Rollback()
		delete(p.txs, id)
	}
}
