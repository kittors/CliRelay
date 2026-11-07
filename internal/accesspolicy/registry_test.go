package accesspolicy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/quota"
)

func TestPublishSkipsEmptyPoliciesAndLookupIsByKey(t *testing.T) {
	t.Cleanup(func() { Publish(nil) })
	withRoute := &Policy{Routes: []quota.ModelRoute{{ID: "r", From: []string{"a"}, To: "b"}}}
	// 只有固定列格子（美元四周期）的 Key 不需要新准入逻辑。
	columnOnly := &Policy{EndUserID: "u", KeyLimits: quota.MatrixFromPeriodLimits(quota.PeriodSpendingLimits{Day: 5})}
	tokens := quota.Matrix{}
	tokens.Set(quota.MetricTokens, quota.PeriodDay, 100)
	withTokens := &Policy{KeyLimits: tokens}
	Publish(map[string]*Policy{"sk-route": withRoute, "sk-column": columnOnly, "sk-tokens": withTokens, " ": withRoute})
	if Lookup("sk-route") != withRoute || Lookup(" sk-tokens ") != withTokens {
		t.Fatal("lookup by key failed")
	}
	if Lookup("sk-column") != nil {
		t.Fatal("a key with only column-backed cells must not be published")
	}
	if Lookup("sk-missing") != nil {
		t.Fatal("unknown key must return nil")
	}
}

func TestPolicyCellsFollowColumnLayouts(t *testing.T) {
	account := quota.Matrix{}
	account.Set(quota.MetricCost, quota.PeriodDay, 10)     // 账号列：旧逻辑检查
	account.Set(quota.MetricRequests, quota.PeriodWeek, 7) // 策略格：新逻辑检查
	key := quota.Matrix{}
	key.Set(quota.MetricCost, quota.PeriodWeek, 3)    // Key 列：旧逻辑检查
	key.Set(quota.MetricRequests, quota.PeriodDay, 4) // 账号下 Key 的策略格
	owned := &Policy{EndUserID: "u", AccountLimits: account, KeyLimits: key}
	if cells := owned.AccountPolicyCells(); len(cells) != 1 || cells[0].Metric != quota.MetricRequests || cells[0].Window != quota.PeriodWeek {
		t.Fatalf("account cells = %+v", cells)
	}
	if cells := owned.KeyPolicyCells(); len(cells) != 1 || cells[0].Window != quota.PeriodDay {
		t.Fatalf("owned key cells = %+v", cells)
	}
	// 独立 Key 的每日请求在 daily_limit 列上，由旧逻辑检查。
	standalone := &Policy{KeyLimits: key}
	if cells := standalone.KeyPolicyCells(); len(cells) != 0 {
		t.Fatalf("standalone key cells = %+v", cells)
	}
	if standalone.AccountPolicyCells() != nil {
		t.Fatal("standalone key has no account layer")
	}
}
