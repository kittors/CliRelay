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
