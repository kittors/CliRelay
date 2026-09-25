package middleware

import (
	"strings"
	"sync"
)

// admissionSubjects maps an API key to the limiter subject it was last
// admitted under, when that is not the key itself: keys owned by an account
// share the account's pool. TPM for a usage record written without a database
// round trip (spooled during an outage) uses it to find the pool the
// admission check reads.
var admissionSubjects sync.Map

// rememberAdmissionSubject records the limiter subject an API key is admitted
// under when it differs from the key.
func rememberAdmissionSubject(apiKey, subject string) {
	if apiKey == "" {
		return
	}
	if subject == "" || subject == apiKey {
		if _, ok := admissionSubjects.Load(apiKey); ok {
			admissionSubjects.Delete(apiKey)
		}
		return
	}
	if current, ok := admissionSubjects.Load(apiKey); !ok || current.(string) != subject {
		admissionSubjects.Store(apiKey, subject)
	}
}

// admissionSubjectFor returns the subject apiKey was last admitted under.
func admissionSubjectFor(apiKey string) (string, bool) {
	if value, ok := admissionSubjects.Load(strings.TrimSpace(apiKey)); ok {
		return value.(string), true
	}
	return "", false
}
