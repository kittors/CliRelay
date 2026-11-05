// Package privacymask finds the values a request should not show its upstream
// (keys, passwords, tokens, personal data and a tenant's own words) and puts
// them back where a reply names their placeholders.
//
// The finders (rules.go, personal.go, phone.go, fieldvalue.go, ids.go,
// dateword.go, custom.go, memo.go and the rule-running parts of find.go) are
// ported from magpie's internal/redact (github.com/yetone/magpie, commit
// 4cb7159, MIT License, Copyright (c) 2026 yetone; the license is in
// MAGPIE_LICENSE beside this file). Their comments keep magpie's account of
// why each check is there: most were added after a user reported a value
// masked wrongly, or one that went out as written.
//
// What differs from magpie is everything that holds state. magpie is one
// user's gateway and keeps one process-wide table of every value it masked;
// here a value is only ever known to the scope (a tenant, and inside it a
// portal user or an API key) whose request brought it, so a reply can be made
// to restore only what its own scope sent (known.go, restore.go, masker.go).
// The local model, the made-up keys of the same form (magpie's shape.go) and
// the JSON walker are not ported: the request is read by contentmoderation's
// extractor, which already knows the four protocols this server speaks.
package privacymask
