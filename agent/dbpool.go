package agent

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"

	"github.com/nuzur/nuzur-cli/agent/connections"
)

// dbPool holds the daemon's *sqlx.DB handles keyed by local_agent_connection_uuid,
// plus a fallback handle backed by the legacy NUZUR_AGENT_DSN flow so self-test
// keeps working without registering a connection first.
//
// Handles are opened lazily on first use and cached for the lifetime of the
// daemon. They share sqlx's own internal pool, so concurrent RunQuery calls on
// the same connection_uuid are safe.
type dbPool struct {
	registry *connections.Registry
	fallback *sqlx.DB // optional; nil if NUZUR_AGENT_DSN was empty

	mu    sync.Mutex
	cache map[string]*sqlx.DB // local_agent_connection_uuid -> handle
}

func newDBPool(registry *connections.Registry, fallback *sqlx.DB) *dbPool {
	return &dbPool{
		registry: registry,
		fallback: fallback,
		cache:    make(map[string]*sqlx.DB),
	}
}

// Get returns a *sqlx.DB to use for a RunQuery request. Resolution order:
//   1. cached handle for connUUID
//   2. registered connection — open lazily, cache, return
//   3. fallback (env-var DSN) — kept for the no-registry case used by self-test
//
// Returns an error if nothing matches.
func (p *dbPool) Get(connUUID string) (*sqlx.DB, error) {
	p.mu.Lock()
	if h, ok := p.cache[connUUID]; ok {
		p.mu.Unlock()
		return h, nil
	}
	p.mu.Unlock()

	if entry, ok := p.registry.FindByUUID(connUUID); ok {
		h, err := openLocalDB(entry.Driver, entry.DSN)
		if err != nil {
			return nil, fmt.Errorf("open connection %q (%s): %w", entry.Name, entry.UUID, err)
		}
		if h == nil {
			return nil, fmt.Errorf("connection %q has an empty DSN", entry.Name)
		}
		p.mu.Lock()
		// Re-check under lock in case a concurrent Get raced us.
		if existing, ok := p.cache[connUUID]; ok {
			p.mu.Unlock()
			_ = h.Close()
			return existing, nil
		}
		p.cache[connUUID] = h
		p.mu.Unlock()
		log.Printf("opened local DB %q (uuid=%s, driver=%s)", entry.Name, entry.UUID, entry.Driver)
		return h, nil
	}

	if p.fallback != nil {
		return p.fallback, nil
	}

	return nil, fmt.Errorf("no local connection registered for uuid %s (run `nuzur agent connection add` or set NUZUR_AGENT_DSN)", connUUID)
}

// Close closes every cached handle, including the fallback. Idempotent.
func (p *dbPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, h := range p.cache {
		_ = h.Close()
	}
	p.cache = map[string]*sqlx.DB{}
	if p.fallback != nil {
		_ = p.fallback.Close()
		p.fallback = nil
	}
}

// openLocalDB lives here so both the pool and the legacy fallback path share
// the same pool-tuning. Returns (nil, nil) for empty DSN.
func openLocalDB(driver, dsn string) (*sqlx.DB, error) {
	if dsn == "" {
		return nil, nil
	}
	db, err := sqlx.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	// Conservative pool — single dev DB, low concurrency.
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(2 * time.Minute)
	return db, nil
}
