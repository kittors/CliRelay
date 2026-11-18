package config

import "sync/atomic"

var v8MigrationWarn atomic.Value

// SetV8MigrationWarnFunc installs a callback for compatibility warnings emitted
// while loading legacy configuration. A nil callback disables forwarding.
func SetV8MigrationWarnFunc(fn func(section, msg string)) {
	v8MigrationWarn.Store(fn)
}

func warnV8Migration(section, msg string) {
	if value := v8MigrationWarn.Load(); value != nil {
		if fn, ok := value.(func(string, string)); ok && fn != nil {
			fn(section, msg)
		}
	}
}
