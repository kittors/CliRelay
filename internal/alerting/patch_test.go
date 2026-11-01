package alerting

import (
	"encoding/json"
	"strings"
	"testing"
)

func strp(s string) *string { return &s }
func boolp(b bool) *bool    { return &b }
func intp(i int) *int       { return &i }

const (
	testFeishuURL    = "https://open.feishu.cn/open-apis/bot/v2/hook/0a1b2c3d-4e5f-6789-abcd-ef0123456789"
	testWebhookURL   = "https://hooks.example.com/services/T000/B000/XXXXXXXXXXXXXXXX"
	testTelegramTok  = "123456789:AAH-ExampleTokenExampleTokenExample_1"
	testSigningKey   = "feishu-signing-secret"
	testWebhookToken = "hmac-key-for-the-webhook"
)

func storedSettings() Settings {
	s := DefaultSettings()
	s.Enabled = true
	s.Channels = []Channel{
		{ID: "ch-webhook", Kind: KindWebhook, Name: "ops", Enabled: true, URL: testWebhookURL, Secret: testWebhookToken},
		{ID: "ch-feishu", Kind: KindFeishu, Name: "team", Enabled: true, URL: testFeishuURL, Secret: testSigningKey},
		{ID: "ch-telegram", Kind: KindTelegram, Name: "phone", Enabled: true, BotToken: testTelegramTok, ChatID: "-1001234567890"},
	}
	return s
}

func TestViewMasksEverySecret(t *testing.T) {
	view := storedSettings().View(true)
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, secret := range []string{
		testWebhookURL, "XXXXXXXXXXXXXXXX/", testWebhookToken, testFeishuURL, "0a1b2c3d", testSigningKey, testTelegramTok, "ExampleTokenExampleToken",
	} {
		if strings.Contains(body, secret) {
			t.Errorf("the view leaks %q: %s", secret, body)
		}
	}
	// What an operator needs to tell channels apart survives.
	for _, want := range []string{"https://hooks.example.com/", "https://open.feishu.cn/", "-1001234567890", "ops", "team", "phone"} {
		if !strings.Contains(body, want) {
			t.Errorf("the view lost %q: %s", want, body)
		}
	}
	if len(view.Channels) != 3 || view.Channels[0].Secret != maskedMarker || view.Channels[2].BotToken != MaskToken(testTelegramTok) {
		t.Errorf("unexpected masks: %+v", view.Channels)
	}
	if !view.Persistent || view.Limits.MaxChannels != MaxChannels {
		t.Errorf("view metadata: %+v", view)
	}
}

func TestMasks(t *testing.T) {
	if got := MaskURL(testWebhookURL); got != "https://hooks.example.com/****XXXX" {
		t.Errorf("MaskURL = %q", got)
	}
	if got := MaskURL("https://x.example/a"); got != "https://x.example/****" {
		t.Errorf("a short path must not leak its tail: %q", got)
	}
	if got := MaskURL("not a url"); got != maskedMarker {
		t.Errorf("MaskURL(garbage) = %q", got)
	}
	if MaskURL("") != "" || MaskSecret("") != "" || MaskToken("") != "" {
		t.Error("empty values mask to empty")
	}
	if got := MaskToken("short"); got != maskedMarker {
		t.Errorf("a short token shows nothing of itself: %q", got)
	}
	if got := MaskToken(testTelegramTok); got != maskedMarker+"le_1" {
		t.Errorf("MaskToken = %q", got)
	}
}

func TestPatchKeepsSecretsWhenTheMaskIsEchoed(t *testing.T) {
	stored := storedSettings()
	view := stored.View(true)
	// A client that read the settings and writes them back unchanged.
	var echoed []ChannelPatch
	for _, ch := range view.Channels {
		echoed = append(echoed, ChannelPatch{
			ID: ch.ID, Kind: ch.Kind, Name: strp(ch.Name), Enabled: boolp(ch.Enabled),
			URL: strp(ch.URL), Secret: strp(ch.Secret), BotToken: strp(ch.BotToken), ChatID: strp(ch.ChatID),
		})
	}
	next, err := stored.ApplyPatch(Patch{Channels: &echoed}, timeNow())
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Channels) != 3 {
		t.Fatalf("channels = %d", len(next.Channels))
	}
	for i, want := range stored.Channels {
		if next.Channels[i] != want {
			t.Errorf("channel %d changed by an echoed write:\n got %+v\nwant %+v", i, next.Channels[i], want)
		}
	}
	if verr := next.Validate(false); verr != nil {
		t.Errorf("an unchanged write must validate: %v", verr)
	}
}

func TestPatchSecretContract(t *testing.T) {
	stored := storedSettings()
	apply := func(p ChannelPatch) (Channel, *FieldError) {
		patch := []ChannelPatch{p}
		next, err := stored.ApplyPatch(Patch{Channels: &patch}, timeNow())
		if err != nil {
			return Channel{}, err
		}
		return next.Channels[0], nil
	}

	t.Run("omitted keeps", func(t *testing.T) {
		ch, err := apply(ChannelPatch{ID: "ch-webhook"})
		if err != nil || ch.URL != testWebhookURL || ch.Secret != testWebhookToken {
			t.Fatalf("ch = %+v, err = %v", ch, err)
		}
	})
	t.Run("empty clears", func(t *testing.T) {
		ch, err := apply(ChannelPatch{ID: "ch-webhook", Secret: strp("")})
		if err != nil || ch.Secret != "" || ch.URL != testWebhookURL {
			t.Fatalf("ch = %+v, err = %v", ch, err)
		}
	})
	t.Run("new value replaces", func(t *testing.T) {
		ch, err := apply(ChannelPatch{ID: "ch-webhook", URL: strp("https://hooks.example.com/new")})
		if err != nil || ch.URL != "https://hooks.example.com/new" {
			t.Fatalf("ch = %+v, err = %v", ch, err)
		}
	})
	t.Run("a stale mask is refused, not stored", func(t *testing.T) {
		_, err := apply(ChannelPatch{ID: "ch-webhook", URL: strp("https://hooks.example.com/****ZZZZ")})
		if err == nil || err.Field != "channels[0].url" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a mask with no stored value behind it is refused", func(t *testing.T) {
		_, err := apply(ChannelPatch{Kind: KindWebhook, Name: strp("x"), URL: strp(MaskURL(testWebhookURL))})
		if err == nil {
			t.Fatal("a new channel must not be able to claim a mask")
		}
	})
	t.Run("a mask cannot move a secret to another channel", func(t *testing.T) {
		patch := []ChannelPatch{
			{ID: "ch-webhook", Kind: KindWebhook, Name: strp("ops")},
			{Kind: KindWebhook, Name: strp("copy"), URL: strp(MaskURL(testWebhookURL))},
		}
		if _, err := stored.ApplyPatch(Patch{Channels: &patch}, timeNow()); err == nil {
			t.Fatal("a mask resolved against a channel it does not belong to")
		}
	})
	t.Run("an id with another kind is a new channel", func(t *testing.T) {
		ch, err := apply(ChannelPatch{ID: "ch-webhook", Kind: KindFeishu, Name: strp("x"), URL: strp(testFeishuURL)})
		if err != nil {
			t.Fatal(err)
		}
		if ch.ID == "ch-webhook" || ch.Secret != "" {
			t.Fatalf("a channel changed kind and kept the old secret: %+v", ch)
		}
	})
	t.Run("a duplicate id is a new channel", func(t *testing.T) {
		patch := []ChannelPatch{{ID: "ch-webhook"}, {ID: "ch-webhook", Kind: KindWebhook, Name: strp("dup"), URL: strp("https://hooks.example.com/dup")}}
		next, err := stored.ApplyPatch(Patch{Channels: &patch}, timeNow())
		if err != nil {
			t.Fatal(err)
		}
		if next.Channels[0].ID != "ch-webhook" || next.Channels[1].ID == "ch-webhook" || next.Channels[1].Secret != "" {
			t.Fatalf("channels = %+v", next.Channels)
		}
	})
	t.Run("a channel left out of the list is removed", func(t *testing.T) {
		patch := []ChannelPatch{{ID: "ch-feishu"}}
		next, err := stored.ApplyPatch(Patch{Channels: &patch}, timeNow())
		if err != nil || len(next.Channels) != 1 || next.Channels[0].ID != "ch-feishu" {
			t.Fatalf("next = %+v, err = %v", next.Channels, err)
		}
	})
	t.Run("no channels field changes nothing", func(t *testing.T) {
		next, err := stored.ApplyPatch(Patch{Enabled: boolp(false)}, timeNow())
		if err != nil || len(next.Channels) != 3 || next.Enabled {
			t.Fatalf("next = %+v, err = %v", next, err)
		}
	})
}

func TestPatchRules(t *testing.T) {
	stored := storedSettings()
	next, err := stored.ApplyPatch(Patch{Rules: &RulesPatch{
		QuotaWindow:   &QuotaWindowPatch{ThresholdPercent: intp(75)},
		UseItOrLoseIt: &UseItOrLoseItPatch{Enabled: boolp(true), HoursBeforeReset: intp(12)},
	}}, timeNow())
	if err != nil {
		t.Fatal(err)
	}
	r := next.Rules
	if r.QuotaWindow.ThresholdPercent != 75 || !r.QuotaWindow.Enabled {
		t.Errorf("quota_window = %+v", r.QuotaWindow)
	}
	if !r.UseItOrLoseIt.Enabled || r.UseItOrLoseIt.HoursBeforeReset != 12 || r.UseItOrLoseIt.MinRemainingPercent != DefaultMinRemainingPercent {
		t.Errorf("use_it_or_lose_it = %+v", r.UseItOrLoseIt)
	}
	if r.Spending != stored.Rules.Spending || r.AccountFailure != stored.Rules.AccountFailure {
		t.Error("rules left out of the patch changed")
	}
}

func TestValidate(t *testing.T) {
	bad := []struct {
		name  string
		patch Patch
		field string
	}{
		{"language", Patch{Language: strp("fr")}, "language"},
		{"cooldown too small", Patch{CooldownMinutes: intp(1)}, "cooldown_minutes"},
		{"cooldown too large", Patch{CooldownMinutes: intp(MaxCooldownMinutes + 1)}, "cooldown_minutes"},
		{"threshold zero", Patch{Rules: &RulesPatch{QuotaWindow: &QuotaWindowPatch{ThresholdPercent: intp(0)}}}, "rules.quota_window.threshold_percent"},
		{"threshold over 100", Patch{Rules: &RulesPatch{Spending: &SpendingPatch{ThresholdPercent: intp(101)}}}, "rules.spending.threshold_percent"},
		{"negative failure minutes", Patch{Rules: &RulesPatch{AccountFailure: &AccountFailurePatch{ForMinutes: intp(-1)}}}, "rules.account_failure.for_minutes"},
		{"hours too large", Patch{Rules: &RulesPatch{UseItOrLoseIt: &UseItOrLoseItPatch{HoursBeforeReset: intp(MaxHoursBeforeReset + 1)}}}, "rules.use_it_or_lose_it.hours_before_reset"},
		{"missing name", Patch{Channels: &[]ChannelPatch{{Kind: KindWebhook, URL: strp("https://hooks.example.com/x")}}}, "channels[0].name"},
		{"unknown kind", Patch{Channels: &[]ChannelPatch{{Kind: "sms", Name: strp("x")}}}, "channels[0].kind"},
		{"private webhook", Patch{Channels: &[]ChannelPatch{{Kind: KindWebhook, Name: strp("x"), URL: strp("https://10.0.0.1/x")}}}, "channels[0].url"},
		{"feishu elsewhere", Patch{Channels: &[]ChannelPatch{{Kind: KindFeishu, Name: strp("x"), URL: strp("https://hooks.example.com/x")}}}, "channels[0].url"},
		{"telegram token", Patch{Channels: &[]ChannelPatch{{Kind: KindTelegram, Name: strp("x"), BotToken: strp("nope"), ChatID: strp("1")}}}, "channels[0].bot_token"},
		{"telegram chat", Patch{Channels: &[]ChannelPatch{{Kind: KindTelegram, Name: strp("x"), BotToken: strp(testTelegramTok), ChatID: strp("not a chat")}}}, "channels[0].chat_id"},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			next, err := DefaultSettings().ApplyPatch(tt.patch, timeNow())
			if err == nil {
				err = next.Validate(false)
			}
			if err == nil || err.Field != tt.field {
				t.Fatalf("err = %v, want field %q", err, tt.field)
			}
		})
	}

	tooMany := make([]ChannelPatch, MaxChannels+1)
	for i := range tooMany {
		tooMany[i] = ChannelPatch{Kind: KindWebhook, Name: strp("x"), URL: strp("https://hooks.example.com/x")}
	}
	next, err := DefaultSettings().ApplyPatch(Patch{Channels: &tooMany}, timeNow())
	if err == nil {
		err = next.Validate(false)
	}
	if err == nil || err.Field != "channels" {
		t.Errorf("too many channels: err = %v", err)
	}

	// The operator's own tenant may use the local network; nobody else may.
	local := []ChannelPatch{{Kind: KindWebhook, Name: strp("relay"), URL: strp("http://127.0.0.1:9000/hook")}}
	next, err = DefaultSettings().ApplyPatch(Patch{Channels: &local}, timeNow())
	if err != nil {
		t.Fatal(err)
	}
	if verr := next.Validate(true); verr != nil {
		t.Errorf("the operator's local webhook was refused: %v", verr)
	}
	if verr := next.Validate(false); verr == nil {
		t.Error("a tenant's local webhook was accepted")
	}
}

func TestNormalizedRepairsWhatAnOldRowLacks(t *testing.T) {
	got := Settings{Enabled: true}.Normalized()
	if got.Rules != DefaultRules() || got.CooldownMinutes != DefaultCooldownMinutes || got.Language != LanguageEN {
		t.Errorf("Normalized = %+v", got)
	}
	got = Settings{CooldownMinutes: 100000, Rules: Rules{QuotaWindow: QuotaWindowRule{Enabled: true, ThresholdPercent: 5000}}}.Normalized()
	if got.CooldownMinutes != MaxCooldownMinutes || got.Rules.QuotaWindow.ThresholdPercent != 100 {
		t.Errorf("out-of-range values must clamp, not disable: %+v", got)
	}
	if (Settings{Enabled: true}).Active() {
		t.Error("alerting with no channel must not be active")
	}
	if s := storedSettings(); !s.Active() {
		t.Error("an enabled tenant with channels is active")
	}
	off := storedSettings()
	off.Enabled = false
	if off.Active() {
		t.Error("the master switch must win over the channels")
	}
}

func TestChannelFromPatchResolvesAgainstTheSavedChannel(t *testing.T) {
	stored := storedSettings()
	view := stored.View(true)
	// The panel's "test" button on an edited channel: the draft echoes the masks.
	ch, err := stored.ChannelFromPatch(ChannelPatch{
		ID: view.Channels[1].ID, Kind: KindFeishu, Name: strp("team"), URL: strp(view.Channels[1].URL), Secret: strp(view.Channels[1].Secret),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ch.URL != testFeishuURL || ch.Secret != testSigningKey {
		t.Errorf("the masks did not resolve: %+v", ch)
	}
	// Without an id a mask means nothing.
	if _, err := stored.ChannelFromPatch(ChannelPatch{Kind: KindFeishu, URL: strp(view.Channels[1].URL)}); err == nil {
		t.Error("a draft without an id resolved a mask")
	}
	draft, err := stored.ChannelFromPatch(ChannelPatch{Kind: KindWebhook, URL: strp("https://hooks.example.com/new")})
	if err != nil || draft.Name != KindWebhook || !draft.Enabled {
		t.Errorf("draft = %+v, err = %v", draft, err)
	}
}
