# CHANGELOG

## Unreleased

### Codex auth metadata and startup registration

- persisted Codex `plan_type` into runtime auth metadata during both CLI/device login and management OAuth login flows
- preserved Codex `account_id` explicitly in runtime auth metadata for downstream request handling
- backfilled Codex `plan_type` from legacy `id_token` metadata when older auth files do not store it explicitly
- registered loaded auths during service startup so executors and model visibility are available immediately after boot
- preserved Codex free-tier model routing without forcing tier-based model downgrades or synthetic excluded-model lists

### Verification and tests

- added auth metadata coverage test for Codex plan/account persistence
- added watcher synthesis coverage for Codex `plan_type` backfill and free-tier pass-through behavior
- added service startup registration coverage for loaded auth records

### Notes

- this change is aimed at Codex OAuth / ChatGPT-account free-tier behavior, not standard OpenAI API-key billing flows
