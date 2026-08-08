package contentmoderation

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
)

// Qwen3Guard risk categories. The IDs are our canonical snake_case form; the
// guard model emits the human-readable labels documented in the Qwen3Guard
// model card ("Violent", "Non-violent Illegal Acts", ...), which
// NormalizeCategory maps back onto these IDs.
const (
	ScannerViolent                   = "violent"
	ScannerNonViolentIllegalActs     = "non_violent_illegal_acts"
	ScannerSexualContentOrSexualActs = "sexual_content_or_sexual_acts"
	ScannerPII                       = "pii"
	ScannerSuicideAndSelfHarm        = "suicide_and_self_harm"
	ScannerUnethicalActs             = "unethical_acts"
	ScannerPoliticallySensitive      = "politically_sensitive_topics"
	ScannerCopyrightViolation        = "copyright_violation"
	ScannerJailbreak                 = "jailbreak"
)

// AllScannerIDs is the display and serialization order for categories.
var AllScannerIDs = []string{
	ScannerViolent,
	ScannerNonViolentIllegalActs,
	ScannerSexualContentOrSexualActs,
	ScannerPII,
	ScannerSuicideAndSelfHarm,
	ScannerUnethicalActs,
	ScannerPoliticallySensitive,
	ScannerCopyrightViolation,
	ScannerJailbreak,
}

var scannerCatalog = map[string]struct{}{
	ScannerViolent:                   {},
	ScannerNonViolentIllegalActs:     {},
	ScannerSexualContentOrSexualActs: {},
	ScannerPII:                       {},
	ScannerSuicideAndSelfHarm:        {},
	ScannerUnethicalActs:             {},
	ScannerPoliticallySensitive:      {},
	ScannerCopyrightViolation:        {},
	ScannerJailbreak:                 {},
}

// categoryAliases maps normalized spellings the guard model (or an operator
// typing into the admin UI) may produce onto canonical IDs. Keys are already
// lowercase with separators collapsed to single spaces by NormalizeCategory.
var categoryAliases = map[string]string{
	"violent":                           ScannerViolent,
	"violence":                          ScannerViolent,
	"non violent illegal acts":          ScannerNonViolentIllegalActs,
	"nonviolent illegal acts":           ScannerNonViolentIllegalActs,
	"sexual content or sexual acts":     ScannerSexualContentOrSexualActs,
	"sexual content":                    ScannerSexualContentOrSexualActs,
	"sexual":                            ScannerSexualContentOrSexualActs,
	"pii":                               ScannerPII,
	"personal identifying information":  ScannerPII,
	"personal identifiable information": ScannerPII,
	"suicide and self harm":             ScannerSuicideAndSelfHarm,
	"suicide self harm":                 ScannerSuicideAndSelfHarm,
	"self harm":                         ScannerSuicideAndSelfHarm,
	"unethical acts":                    ScannerUnethicalActs,
	"unethical":                         ScannerUnethicalActs,
	"politically sensitive topics":      ScannerPoliticallySensitive,
	"political":                         ScannerPoliticallySensitive,
	"copyright violation":               ScannerCopyrightViolation,
	"copyright":                         ScannerCopyrightViolation,
	"jailbreak":                         ScannerJailbreak,
	"prompt injection":                  ScannerJailbreak,
}

// DefaultElevatedCategories are the categories that turn a "Controversial"
// verdict into a block under ControversialActionElevatedOnly. Mis-allowing them
// costs more than mis-blocking: jailbreak attacks this proxy itself, PII
// carries compliance exposure, and self-harm carries personal risk.
func DefaultElevatedCategories() []string {
	return []string{ScannerJailbreak, ScannerPII, ScannerSuicideAndSelfHarm}
}

// IsScannerID reports whether value is one of the nine known categories.
func IsScannerID(value string) bool {
	_, ok := scannerCatalog[strings.TrimSpace(value)]
	return ok
}

// NormalizeCategory folds separator and casing variants onto a canonical ID.
// Unknown values are returned in snake_case rather than dropped so callers can
// still surface them (see unknownCategoryID).
func NormalizeCategory(value string) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	normalized = strings.NewReplacer("_", " ", "&", " and ", "/", " ", "-", " ", "–", " ", "—", " ").Replace(normalized)
	normalized = strings.Join(strings.Fields(normalized), " ")
	if canonical, ok := categoryAliases[normalized]; ok {
		return canonical
	}
	return strings.ReplaceAll(normalized, " ", "_")
}

// unknownCategoryID hashes an out-of-catalog label instead of echoing it: the
// label is model output derived from user prompt text and must not reach logs
// or the admin API verbatim.
func unknownCategoryID(value string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(strings.ToLower(value))))
	return fmt.Sprintf("unknown:%x", digest[:8])
}

// normalizeScannerList canonicalizes, de-duplicates and orders a configured
// category list. Out-of-catalog values are kept rather than dropped so
// ValidateProfile can reject a typo instead of silently narrowing the policy
// the operator believes they saved.
func normalizeScannerList(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if category := NormalizeCategory(value); category != "" {
			seen[category] = struct{}{}
		}
	}
	return orderedScannerKeys(seen)
}

// orderedScannerKeys returns catalog members in AllScannerIDs order, followed
// by any remaining keys sorted alphabetically, so output is deterministic.
func orderedScannerKeys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	remaining := make(map[string]struct{}, len(values))
	for key := range values {
		remaining[key] = struct{}{}
	}
	for _, scannerID := range AllScannerIDs {
		if _, ok := remaining[scannerID]; ok {
			result = append(result, scannerID)
			delete(remaining, scannerID)
		}
	}
	return append(result, sortedKeys(remaining)...)
}

func sortedKeys(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}
