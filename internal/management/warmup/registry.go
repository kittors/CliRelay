package warmup

import (
	"sync"
)

// DriverRegistry manages all provider-specific warmup drivers.
type DriverRegistry struct {
	mu      sync.RWMutex
	drivers map[string]Driver
}

func NewDriverRegistry() *DriverRegistry {
	return &DriverRegistry{
		drivers: make(map[string]Driver),
	}
}

func (r *DriverRegistry) Register(driver Driver) {
	if driver == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.drivers[driver.Provider()] = driver
}

func (r *DriverRegistry) Get(provider string) (Driver, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.drivers[provider]
	return d, ok
}
