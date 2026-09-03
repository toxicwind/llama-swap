package astmatrix

import (
	"database/sql"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type HealthDB struct {
	mu        sync.RWMutex
	db        *sql.DB
	healthy   map[string]bool
	latencies map[string]time.Duration
	elos      map[string]int
	stickies  map[string]string
	lastProbe map[string]time.Time
}

func NewHealthDB(path string) *HealthDB {
	h := &HealthDB{
		healthy: make(map[string]bool), latencies: make(map[string]time.Duration),
		elos: make(map[string]int), stickies: make(map[string]string),
		lastProbe: make(map[string]time.Time),
	}
	if db, err := sql.Open("sqlite3", path); err == nil {
		h.db = db; h.initSchema()
	}
	return h
}

func (h *HealthDB) migrate() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS requests (
 		id INTEGER PRIMARY KEY AUTOINCREMENT,
 		ts REAL NOT NULL,
 		provider TEXT NOT NULL,
 		model TEXT NOT NULL,
 		status INTEGER NOT NULL,
 		latency_ms REAL NOT NULL,
 		strategy TEXT NOT NULL DEFAULT '',
 		winner INTEGER NOT NULL DEFAULT 0,
 		session_id TEXT NOT NULL DEFAULT ''
 	)`,
		`CREATE INDEX IF NOT EXISTS idx_req_prov_model ON requests(provider, model)`,
		`CREATE INDEX IF NOT EXISTS idx_req_ts ON requests(ts)`,

		`CREATE TABLE IF NOT EXISTS model_health (
 		provider TEXT NOT NULL,
 		model TEXT NOT NULL,
 		window_start REAL NOT NULL,
 		successes INTEGER NOT NULL DEFAULT 0,
 		failures INTEGER NOT NULL DEFAULT 0,
 		rate_limited INTEGER NOT NULL DEFAULT 0,
 		total_ms REAL NOT NULL DEFAULT 0,
 		min_ms REAL NOT NULL DEFAULT 999999,
 		max_ms REAL NOT NULL DEFAULT 0,
 		PRIMARY KEY (provider, model, window_start)
 	)`,

		`CREATE TABLE IF NOT EXISTS healing_events (
 		id INTEGER PRIMARY KEY AUTOINCREMENT,
 		ts REAL NOT NULL,
 		provider TEXT NOT NULL,
 		model TEXT NOT NULL,
 		event TEXT NOT NULL,
 		prev_status TEXT NOT NULL DEFAULT '',
 		new_status TEXT NOT NULL DEFAULT '',
 		details TEXT NOT NULL DEFAULT ''
 	)`,

		`CREATE TABLE IF NOT EXISTS rate_limit_events (
 		id INTEGER PRIMARY KEY AUTOINCREMENT,
 		ts REAL NOT NULL,
 		provider TEXT NOT NULL,
 		model TEXT NOT NULL,
 		status_code INTEGER NOT NULL,
 		retry_after REAL DEFAULT NULL
 	)`,

		`CREATE TABLE IF NOT EXISTS session_affinity (
 		session_id TEXT PRIMARY KEY,
 		provider TEXT NOT NULL,
 		model TEXT NOT NULL,
 		updated_at REAL NOT NULL
 	)`,
	}
	for _, stmt := range statements {
		if _, err := h.db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate exec: %w", err)
		}
	}
}

func (h *HealthDB) RecordLatency(provider string, latency time.Duration) {
	h.mu.Lock(); defer h.mu.Unlock()
	old := h.latencies[provider]
	if old == 0 { h.latencies[provider] = latency } else { h.latencies[provider] = old*7/10 + latency*3/10 }
	if h.db != nil {
		h.db.Exec(`INSERT INTO provider_health(provider,latency_ms,last_probe)
		VALUES(?,?,?) ON CONFLICT(provider) DO UPDATE SET
			latency_ms=excluded.latency_ms, last_probe=excluded.last_probe`,
			provider, float64(latency.Milliseconds()), time.Now().Format(time.RFC3339))
	}
}

func (h *HealthDB) GetLatency(provider string) time.Duration {
	h.mu.RLock(); defer h.mu.RUnlock()
	return h.latencies[provider]
}

func (h *HealthDB) GetELO(provider string) int {
	h.mu.RLock(); defer h.mu.RUnlock()
	if e, ok := h.elos[provider]; ok { return e }
	return 1500
}

func (h *HealthDB) SetELO(provider string, elo int) {
	h.mu.Lock(); defer h.mu.Unlock()
	h.elos[provider] = elo
}

func (h *HealthDB) IsHealthy(provider string) bool {
	h.mu.RLock(); defer h.mu.RUnlock()
	if _, ok := h.lastProbe[provider]; !ok { return true }
	return h.healthy[provider]
}

func (h *HealthDB) SetSticky(session, provider string, ttl time.Duration) {
	h.mu.Lock(); defer h.mu.Unlock()
	h.stickies[session] = provider
	if h.db != nil {
		h.db.Exec(`INSERT INTO sticky_sessions(session,provider,expires)
		VALUES(?,?,?) ON CONFLICT(session) DO UPDATE SET
			provider=excluded.provider, expires=excluded.expires`,
			session, provider, time.Now().Add(ttl).Format(time.RFC3339))
	}
}

func (h *HealthDB) GetSticky(session string) string {
	h.mu.RLock(); defer h.mu.RUnlock()
	return h.stickies[session]
}

func (h *HealthDB) Close() error {
	if h.db != nil { return h.db.Close() }
	return nil
}
