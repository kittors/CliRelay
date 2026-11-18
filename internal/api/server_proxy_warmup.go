package api

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	log "github.com/sirupsen/logrus"
)

func (s *Server) applyProxyWarmupConfig(cfg *config.Config) {
	if s == nil || cfg == nil {
		return
	}
	cfg.SanitizeProxyWarmup()
	proxyURL := resolveProxyWarmupURL(cfg)
	signature := proxyWarmupSignature(cfg, proxyURL)

	s.proxyWarmMu.Lock()
	defer s.proxyWarmMu.Unlock()

	if !cfg.ProxyWarmup.Enabled || proxyURL == "" {
		if cfg.ProxyWarmup.Enabled && proxyURL == "" {
			log.Warn("proxy_warmup: enabled but no proxy URL could be resolved")
		}
		s.replaceProxyWarmupLocked(nil, "")
		return
	}

	if s.proxyWarmManager != nil && s.proxyWarmSignature == signature {
		return
	}

	manager := executor.NewProxyWarmManager(cfg.ProxyWarmup, proxyURL, &cfg.SDKConfig)
	manager.Start(context.Background())
	s.replaceProxyWarmupLocked(manager, signature)
	log.Infof("proxy_warmup: enabled for %s", maskWarmProxyURL(proxyURL))
}

func (s *Server) stopProxyWarmup() {
	if s == nil {
		return
	}
	s.proxyWarmMu.Lock()
	defer s.proxyWarmMu.Unlock()
	s.replaceProxyWarmupLocked(nil, "")
}

func (s *Server) replaceProxyWarmupLocked(next interface{ Stop() }, signature string) {
	previous := s.proxyWarmManager
	s.proxyWarmManager = next
	s.proxyWarmSignature = signature
	if manager, ok := next.(*executor.ProxyWarmManager); ok {
		executor.SetGlobalWarmManager(manager)
	} else {
		executor.SetGlobalWarmManager(nil)
	}
	if previous != nil {
		previous.Stop()
	}
}

func resolveProxyWarmupURL(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	if proxyID := strings.TrimSpace(cfg.ProxyWarmup.ProxyID); proxyID != "" {
		return resolveProxyWarmupPoolURL(cfg, proxyID)
	}
	if proxyURL := validProxyWarmupURL(cfg.ProxyWarmup.ProxyURL); proxyURL != "" {
		return proxyURL
	}
	if proxyURL := validProxyWarmupURL(cfg.ProxyURL); proxyURL != "" {
		return proxyURL
	}

	var resolved string
	for _, entry := range cfg.ProxyPool {
		if !entry.Enabled || strings.TrimSpace(entry.URL) == "" {
			continue
		}
		proxyURL := validProxyWarmupURL(entry.URL)
		if proxyURL == "" {
			continue
		}
		if resolved != "" {
			return ""
		}
		resolved = proxyURL
	}
	return resolved
}

func resolveProxyWarmupPoolURL(cfg *config.Config, proxyID string) string {
	if cfg == nil {
		return ""
	}
	id := config.NormalizeProxyID(proxyID)
	if id == "" {
		return ""
	}
	for _, entry := range cfg.ProxyPool {
		if !entry.Enabled {
			continue
		}
		if config.NormalizeProxyID(entry.ID) == id {
			return validProxyWarmupURL(entry.URL)
		}
	}
	return ""
}

func validProxyWarmupURL(raw string) string {
	proxyURL := strings.TrimSpace(raw)
	if proxyURL == "" {
		return ""
	}
	if err := config.ValidateProxyURL(proxyURL); err != nil {
		log.Warnf("proxy_warmup: ignoring invalid proxy URL %s", maskWarmProxyURL(proxyURL))
		return ""
	}
	return proxyURL
}

func proxyWarmupSignature(cfg *config.Config, proxyURL string) string {
	if cfg == nil {
		return ""
	}
	payload := struct {
		Warm               config.ProxyWarmConfig `json:"warm"`
		ProxyURL           string                 `json:"proxy_url"`
		PreferIPv4         bool                   `json:"prefer_ipv4"`
		InsecureSkipVerify bool                   `json:"insecure_skip_verify"`
		CACert             string                 `json:"ca_cert"`
	}{
		Warm:               cfg.ProxyWarmup,
		ProxyURL:           strings.TrimSpace(proxyURL),
		PreferIPv4:         cfg.PreferIPv4,
		InsecureSkipVerify: cfg.InsecureSkipVerify,
		CACert:             strings.TrimSpace(cfg.CACert),
	}
	data, _ := json.Marshal(payload)
	return string(data)
}

func maskWarmProxyURL(raw string) string {
	masked := raw
	if at := strings.LastIndex(masked, "@"); at >= 0 {
		if scheme := strings.Index(masked, "://"); scheme >= 0 && scheme < at {
			masked = masked[:scheme+3] + "***@" + masked[at+1:]
		}
	}
	return masked
}
