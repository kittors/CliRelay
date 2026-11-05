package privacymask

import "regexp"

// extraRules are rules of this server's own, run after magpie's. magpie leaves
// these values to its local model, which this server does not run (doc.go),
// so without them they would go upstream as written. Each takes only what has
// the shape of a credential or of a mainland mobile number, so that ordinary
// sentences around the same words stay as they are:
//
//   - a password named in an English sentence: "my password is Tr0ub4dor&3xq",
//     "the staging db password is hunter2Xq9z". magpie's rules take a value
//     after = or : (a config) and after 是, 为 or ： (a Chinese message), not
//     after "is". The value has letters and digits both (secretValue), so "the
//     password is required" and "password is too short" are left;
//   - a key or a token named the same way: "the API key is …", "token was …";
//     a word ("the token is expired") is left (keyValue);
//   - a mainland mobile number written in groups, 139 1234 5678 or
//     139-1234-5678, as people write one in a message; magpie's PHONE rule
//     takes the eleven digits unbroken.
//
// Two more were this server's before magpie's rules came in, and stay so that
// what was masked is masked still: a bearer token anywhere in a line (curl -H
// 'Authorization: Bearer …' in a tool call; magpie's header rule takes the
// header only at a line's start), and Stripe's test keys (magpie takes the
// live ones).
var extraRules = []rule{
	{kind: "TOKEN", re: regexp.MustCompile(`(?i)\bbearer[ \t]+([A-Za-z0-9._~+/=-]{20,})`), fold: []string{"bearer"}, ok: notAVariable},
	{kind: "API_KEY", re: regexp.MustCompile(`(?:sk|rk)_test_[0-9A-Za-z]{20,}`), markers: []string{"_test_"}, bound: tokenCh},
	{kind: "PASSWORD", re: regexp.MustCompile(`(?i)\b(?:password|passwd|passphrase|passcode|pwd)[ \t]+(?:is|was)[ \t]+(?:\\?["'“])?(` + secretCh + `{5,63}[A-Za-z0-9_\-/+=~!@#%^&*])`),
		markers: fieldNames{{"pass", "pass"}, {"pwd", "pwd"}}.markers(), ok: secretValue, field: true},
	{kind: "SECRET", re: regexp.MustCompile(`(?i)\b(?:api[ _-]?key|secret[ _-]?key|access[ _-]?key|access[ _-]?token|auth[ _-]?token|token|secret)[ \t]+(?:is|was)[ \t]+(?:\\?["'“])?(` + secretCh + `{5,63}[A-Za-z0-9_\-/+=~!@#%^&*])`),
		markers: fieldNames{{"key", "key"}, {"token", "token"}, {"secret", "secret"}}.markers(), ok: keyValue, field: true},
	{kind: "PHONE", re: regexp.MustCompile(`1[3-9]\d[ -]\d{4}[ -]\d{4}`), bound: alnum + "-", at: phoneAt, cat: catPersonal, needs: digitCount(11)},
}
