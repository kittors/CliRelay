package main

import "strings"

func (s *updaterServer) pullSkipFailure(runID uint64) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if runID != s.runID || !s.pullSkipped {
		return "", false
	}
	message := "docker compose pull skipped the target service; check pull policy and image refresh settings"
	if strings.TrimSpace(s.pullSkipLog) != "" {
		message += ": " + strings.TrimSpace(s.pullSkipLog)
	}
	return message, true
}
