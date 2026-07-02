# CHANGELOG

## v0.4.6 - OAuth Identity Fingerprints and Updater Diagnostics - 2026-06-25

### Highlights

- added account-level OAuth identity fingerprint learning and detail APIs for Claude, Codex, Gemini, and Kimi-compatible runtime flows
- added Codex OAuth client admission presets so OAuth auth files can restrict upstream access to recognized client identities
- added Claude OAuth health tracking and quota reconciliation helpers for clearer auth-file status reporting
- made updater sidecar token configuration explicit and returned health diagnostics instead of a generic unavailable state
- added quota status clear endpoints for auth-file quota and cooldown recovery workflows
- normalized OpenAI-compatible chat tool-call history and routed OpenCode Go tool-output images through vision fallback handling
- made issue triage automation command-driven for safer maintainer control

### Compatibility and upgrade notes

- direct Docker Compose deployments must provide a non-empty `CLIRELAY_UPDATER_TOKEN` shared by the API and updater sidecar
- existing identity-fingerprint configuration remains compatible; learned account records are added through runtime observation
- Codex OAuth admission uses fixed named presets rather than user-supplied matching rules
- the quota status clear endpoint is intended for management recovery flows and does not change normal quota accounting

### Verification

- `rtk go test ./...`
- `rtk git diff --check`
- PR checks for the merged `dev` pull requests
