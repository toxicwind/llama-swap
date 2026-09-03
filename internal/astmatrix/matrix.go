package astmatrix

import (
	"sync"
)

// Matrix coordinates providers, circuit breakers, and health state.
// It is a thin wrapper around Router for compatibility with upstream naming.
type Matrix struct {
	mu               sync.RWMutex
	providers        map[string]*provider
	fail             map[string]int     // consecutive failures per provider
	lastFail         map[string]float64 // last failure timestamp
	elo              map[string]float64
	circuit          map[string]string // "closed" | "open" | "half"
	circuitOpenUntil map[string]float64
	fifoDepth        int
	health           *HealthDB
	rateLimiter      *PerProviderRateLimiter
	config           *AstMatrixConfig
}

// NewMatrix creates a Matrix from config, loading secrets from env.
func NewMatrix(cfg *AstMatrixConfig) (*Matrix, error) {
	cfg.Defaults()

	providers := defaultProviders()

	// Override provider configs from YAML if provided
	for name, pcfg := range cfg.Providers {
		if p, ok := providers[name]; ok {
			if pcfg.BaseURL != "" {
				p.base = pcfg.BaseURL
			}
			if pcfg.KeyEnv != "" {
				p.keyEnv = pcfg.KeyEnv
			}
			if pcfg.KeyEnvAlt != "" {
				p.keyEnvAlt = pcfg.KeyEnvAlt
			}
			if pcfg.NoAuth {
				p.noAuth = true
			}
		} else {
			providers[name] = &provider{
				base:      pcfg.BaseURL,
				keyEnv:    pcfg.KeyEnv,
				keyEnvAlt: pcfg.KeyEnvAlt,
				noAuth:    pcfg.NoAuth,
			}
		}
	}

	health, err := NewHealthDB(cfg.DbPath)
	if err != nil {
		return nil, err
	}

	elo := make(map[string]float64)
	circuit := make(map[string]string)
	for name := range providers {
		elo[name] = 1000
		circuit[name] = "closed"
	}

	m := &Matrix{
		rateLimiter:      NewRateLimiter(),
		providers:        providers,
		fail:             make(map[string]int),
		lastFail:         make(map[string]float64),
		elo:              elo,
		circuit:          circuit,
		circuitOpenUntil: make(map[string]float64),
		health:           health,
		config:           cfg,
	}
	return m, nil
}

func (m *Matrix) Providers() *ProviderRegistry { return m.registry }
func (m *Matrix) Health() *HealthDB          { return m.health }
func (m *Matrix) Limiter() *RateLimiter      { return m.limiter }
