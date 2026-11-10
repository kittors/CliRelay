package aiaccountstatus

import (
	"math"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
)

// windowSpendQuery reads what accounts spent inside their windows; tests
// replace it to see the requests a status list asks for.
type windowSpendQuery func([]usage.AIAccountWindowSpendRequest) (map[string]map[string]float64, error)

// withQuotaWindowSpend fills in each window's UsedCostUSD (CliRelay#1103): what
// the proxy spent inside the window, which next to the share used tells the
// window's size in dollars. It is best effort — the card is complete without
// it, so a failed read leaves the windows as they were.
func withQuotaWindowSpend(items []AccountStatusView) []AccountStatusView {
	return withQuotaWindowSpendAt(items, time.Now(), usage.QueryAIAccountWindowSpend)
}

func withQuotaWindowSpendAt(items []AccountStatusView, now time.Time, query windowSpendQuery) []AccountStatusView {
	requests := make([]usage.AIAccountWindowSpendRequest, 0, len(items)*2)
	for _, item := range items {
		for _, window := range item.Quotas {
			start, ok := quotaWindowStart(window.QuotaWindowDTO, now)
			if !ok {
				continue
			}
			covers, ok := windowSpendCovers(window.QuotaKey)
			if !ok {
				continue
			}
			requests = append(requests, usage.AIAccountWindowSpendRequest{
				SubjectID: item.AuthSubjectID, Window: window.QuotaKey, Start: start, Covers: covers,
			})
		}
	}
	if len(requests) == 0 {
		return items
	}
	spend, err := query(requests)
	if err != nil {
		log.Warnf("ai account status: quota window spend: %v", err)
		return items
	}
	for i := range items {
		bySubject := spend[items[i].AuthSubjectID]
		for j := range items[i].Quotas {
			value, ok := bySubject[items[i].Quotas[j].QuotaKey]
			if !ok {
				continue
			}
			// Sub-cent request prices add up with float noise; a millionth of a
			// dollar is far below anything the panel shows.
			value = math.Round(value*1e6) / 1e6
			items[i].Quotas[j].UsedCostUSD = &value
		}
	}
	return items
}

// quotaWindowStart is where the window a reading describes began: its renewal
// minus its length. There is none when either is missing, or once the renewal
// has passed: the window has started over and the reading describes the one
// before, so pairing it with spend since a guessed restart would mislead.
func quotaWindowStart(window usage.QuotaWindowDTO, now time.Time) (time.Time, bool) {
	if window.WindowSeconds <= 0 || window.ResetAt == nil || window.ResetAt.IsZero() {
		return time.Time{}, false
	}
	reset := window.ResetAt.UTC()
	if !reset.After(now) {
		return time.Time{}, false
	}
	return reset.Add(-time.Duration(window.WindowSeconds) * time.Second), true
}
