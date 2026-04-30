# CHANGELOG

## Unreleased

### Codex free-tier support improvements

- persisted Codex `plan_type` into runtime auth metadata during both CLI/device login and management OAuth login flows
- preserved Codex `account_id` explicitly in runtime auth metadata for downstream request handling
- added free-tier-aware Codex auth synthesis so OAuth-backed Codex accounts with `plan_type=free` or `legacy` automatically exclude models currently blocked for free users:
  - `gpt-5.3-codex`
  - `gpt-5.3-codex-spark`
  - `gpt-5.4`
- added executor-side Codex model fallback for free-tier accounts so blocked requests are downgraded automatically:
  - `gpt-5.3-codex` / `gpt-5.3-codex-spark` → `gpt-5.2-codex`
  - `gpt-5.4` → `gpt-5.2`

### Verification and tests

- added auth metadata coverage test for Codex plan/account persistence
- added watcher synthesis test for Codex free-tier excluded model generation
- added executor test for Codex free-tier model fallback behavior

### Notes

- `/v1/models` is backed by the global model registry, so free-tier visibility is enforced at auth registration/synthesis time rather than in the HTTP handler itself
- this change is aimed at Codex OAuth / ChatGPT-account free-tier behavior, not standard OpenAI API-key billing flows
