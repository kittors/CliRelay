package enduser

import "testing"

func TestUsernameFromDisplay(t *testing.T) {
	if got := UsernameFromDisplay("Zhang San"); got != "zhang_san" {
		t.Fatalf("ascii name = %q", got)
	}
	if got := UsernameFromDisplay("陈龙"); got != "chenlong" {
		t.Fatalf("pinyin name = %q, want chenlong", got)
	}
	if got := UsernameFromDisplay("张军宝"); got != "zhangjunbao" {
		t.Fatalf("pinyin name = %q", got)
	}
}

func TestLockPenalty(t *testing.T) {
	stage, wait, permanent, apply := lockPenalty(5)
	if !apply || stage != 1 || wait.Minutes() != 1 || permanent {
		t.Fatalf("5 fails: stage=%d wait=%v permanent=%v apply=%v", stage, wait, permanent, apply)
	}
	_, _, _, apply = lockPenalty(6)
	if apply {
		t.Fatalf("6 fails should not re-apply cooldown")
	}
	stage, wait, permanent, apply = lockPenalty(10)
	if !apply || stage != 2 || wait.Minutes() != 5 || permanent {
		t.Fatalf("10 fails: stage=%d wait=%v permanent=%v apply=%v", stage, wait, permanent, apply)
	}
	stage, wait, permanent, apply = lockPenalty(15)
	if !apply || stage != 3 || wait.Minutes() != 10 || permanent {
		t.Fatalf("15 fails: stage=%d wait=%v permanent=%v apply=%v", stage, wait, permanent, apply)
	}
	stage, _, permanent, apply = lockPenalty(20)
	if !apply || stage != 4 || !permanent {
		t.Fatalf("20 fails: stage=%d permanent=%v apply=%v", stage, permanent, apply)
	}
}

func TestGenerateAPIKeyUniqueShape(t *testing.T) {
	k, err := GenerateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(k) < 10 || k[:3] != "sk-" {
		t.Fatalf("key shape %q", k)
	}
}

func hasPrefix(s, p string) bool {
	return len(s) >= len(p) && s[:len(p)] == p
}
