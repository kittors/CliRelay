package auth

// maskSharedCredentials names the platform accounts shared with the request's
// tenant by their stand-ins (shared_name.go). It runs last, once every step
// that matches credentials and attempts by id is done.
func (m *Manager) maskSharedCredentials(sim *SelectionSimulation) {
	if m == nil || sim == nil {
		return
	}
	renamed := make(map[string]string)
	m.mu.RLock()
	for g := range sim.Groups {
		for c := range sim.Groups[g].Credentials {
			credential := &sim.Groups[g].Credentials[c]
			if !credential.Shared {
				continue
			}
			auth := m.auths[credential.AuthID]
			if auth == nil {
				continue
			}
			id := auth.SharedID()
			renamed[credential.AuthID] = id
			credential.AuthID, credential.Label = id, auth.SharedName()
		}
	}
	m.mu.RUnlock()
	for i := range sim.Attempts {
		if id, ok := renamed[sim.Attempts[i].AuthID]; ok {
			sim.Attempts[i].AuthID = id
		}
	}
}
