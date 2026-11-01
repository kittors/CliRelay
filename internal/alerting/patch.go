package alerting

import (
	"net/url"
	"strconv"
	"strings"
	"time"
)

// maskedMarker is what every masked value contains. A value that carries it
// but is not the mask of what is stored was not typed by a person: it is a
// stale mask echoed back by a client, and storing it would overwrite a working
// secret with garbage.
const maskedMarker = "****"

// Patch is a management write. Everything is optional: an omitted field keeps
// what is stored, so a client can change one threshold without sending the
// channels and an empty body changes nothing.
type Patch struct {
	Enabled         *bool           `json:"enabled"`
	Language        *string         `json:"language"`
	CooldownMinutes *int            `json:"cooldown_minutes"`
	Channels        *[]ChannelPatch `json:"channels"`
	Rules           *RulesPatch     `json:"rules"`
}

// ChannelPatch is one channel of a write, or the draft of one that is being
// tried before it is saved.
//
// The secret fields (URL, Secret, BotToken) follow the contract of the masked
// values the API hands out: the mask of the stored value keeps it, an empty
// string clears it, anything else replaces it, and an omitted field keeps it.
type ChannelPatch struct {
	ID       string  `json:"id"`
	Kind     string  `json:"kind"`
	Name     *string `json:"name"`
	Enabled  *bool   `json:"enabled"`
	URL      *string `json:"url"`
	Secret   *string `json:"secret"`
	BotToken *string `json:"bot_token"`
	ChatID   *string `json:"chat_id"`
}

// RulesPatch changes thresholds rule by rule; a rule left out is unchanged.
type RulesPatch struct {
	QuotaWindow    *QuotaWindowPatch    `json:"quota_window"`
	AccountFailure *AccountFailurePatch `json:"account_failure"`
	Spending       *SpendingPatch       `json:"spending"`
	UseItOrLoseIt  *UseItOrLoseItPatch  `json:"use_it_or_lose_it"`
}

type QuotaWindowPatch struct {
	Enabled          *bool `json:"enabled"`
	ThresholdPercent *int  `json:"threshold_percent"`
}

type AccountFailurePatch struct {
	Enabled    *bool `json:"enabled"`
	ForMinutes *int  `json:"for_minutes"`
}

type SpendingPatch struct {
	Enabled          *bool `json:"enabled"`
	ThresholdPercent *int  `json:"threshold_percent"`
}

type UseItOrLoseItPatch struct {
	Enabled             *bool `json:"enabled"`
	MinRemainingPercent *int  `json:"min_remaining_percent"`
	HoursBeforeReset    *int  `json:"hours_before_reset"`
}

// ApplyPatch returns the settings that result from applying a write to the
// stored ones. It does not validate: the caller runs Validate on the result,
// which is also what decides whether a generic webhook may point inside the
// network.
func (s Settings) ApplyPatch(p Patch, now time.Time) (Settings, *FieldError) {
	next := s.Normalized()
	if p.Enabled != nil {
		next.Enabled = *p.Enabled
	}
	if p.Language != nil {
		next.Language = strings.TrimSpace(*p.Language)
	}
	if p.CooldownMinutes != nil {
		next.CooldownMinutes = *p.CooldownMinutes
	}
	if p.Channels != nil {
		channels, err := mergeChannels(next.Channels, *p.Channels)
		if err != nil {
			return Settings{}, err
		}
		next.Channels = channels
	}
	if p.Rules != nil {
		next.Rules = p.Rules.apply(next.Rules)
	}
	next.UpdatedAt = now.UTC()
	return next, nil
}

func (p RulesPatch) apply(rules Rules) Rules {
	if q := p.QuotaWindow; q != nil {
		setBool(&rules.QuotaWindow.Enabled, q.Enabled)
		setInt(&rules.QuotaWindow.ThresholdPercent, q.ThresholdPercent)
	}
	if f := p.AccountFailure; f != nil {
		setBool(&rules.AccountFailure.Enabled, f.Enabled)
		setInt(&rules.AccountFailure.ForMinutes, f.ForMinutes)
	}
	if sp := p.Spending; sp != nil {
		setBool(&rules.Spending.Enabled, sp.Enabled)
		setInt(&rules.Spending.ThresholdPercent, sp.ThresholdPercent)
	}
	if u := p.UseItOrLoseIt; u != nil {
		setBool(&rules.UseItOrLoseIt.Enabled, u.Enabled)
		setInt(&rules.UseItOrLoseIt.MinRemainingPercent, u.MinRemainingPercent)
		setInt(&rules.UseItOrLoseIt.HoursBeforeReset, u.HoursBeforeReset)
	}
	return rules
}

func setBool(dst *bool, src *bool) {
	if src != nil {
		*dst = *src
	}
}

func setInt(dst *int, src *int) {
	if src != nil {
		*dst = *src
	}
}

// mergeChannels builds the channel list of a write. A submitted channel is
// matched to a stored one by id, and only then inherits its secrets; a channel
// with no id, an unknown id, or an id already taken by an earlier entry is new.
// A client therefore cannot read a stored secret into another channel by
// guessing at a mask.
func mergeChannels(current []Channel, submitted []ChannelPatch) ([]Channel, *FieldError) {
	byID := make(map[string]Channel, len(current))
	for _, channel := range current {
		byID[channel.ID] = channel
	}
	used := make(map[string]struct{}, len(submitted))
	out := make([]Channel, 0, len(submitted))
	for i, in := range submitted {
		field := channelField(i)
		var stored *Channel
		if id := strings.TrimSpace(in.ID); id != "" {
			if channel, ok := byID[id]; ok {
				if _, taken := used[id]; !taken && (in.Kind == "" || strings.EqualFold(strings.TrimSpace(in.Kind), channel.Kind)) {
					stored = &channel
				}
			}
		}
		merged, err := mergeChannel(stored, in, field)
		if err != nil {
			return nil, err
		}
		if stored != nil {
			merged.ID = stored.ID
		} else {
			merged.ID = newChannelID()
		}
		used[merged.ID] = struct{}{}
		out = append(out, merged)
	}
	return out, nil
}

func channelField(i int) string {
	return "channels[" + strconv.Itoa(i) + "]"
}

// mergeChannel folds one submitted channel into the stored one it stands for
// (nil for a new channel).
func mergeChannel(stored *Channel, in ChannelPatch, field string) (Channel, *FieldError) {
	var out Channel
	if stored != nil {
		out = *stored
	}
	out.Kind = strings.ToLower(strings.TrimSpace(in.Kind))
	if out.Kind == "" && stored != nil {
		out.Kind = stored.Kind
	}
	if in.Name != nil {
		out.Name = strings.TrimSpace(*in.Name)
	}
	if in.Enabled != nil {
		out.Enabled = *in.Enabled
	} else if stored == nil {
		out.Enabled = true
	}
	if in.ChatID != nil {
		out.ChatID = strings.TrimSpace(*in.ChatID)
	}
	var err *FieldError
	if out.URL, err = mergeSecret(stored, in.URL, func(c Channel) string { return c.URL }, MaskURL, field+".url"); err != nil {
		return Channel{}, err
	}
	if out.Secret, err = mergeSecret(stored, in.Secret, func(c Channel) string { return c.Secret }, MaskSecret, field+".secret"); err != nil {
		return Channel{}, err
	}
	if out.BotToken, err = mergeSecret(stored, in.BotToken, func(c Channel) string { return c.BotToken }, MaskToken, field+".bot_token"); err != nil {
		return Channel{}, err
	}
	return out, nil
}

// mergeSecret resolves one secret field of a write; see ChannelPatch.
func mergeSecret(stored *Channel, submitted *string, get func(Channel) string, mask func(string) string, field string) (string, *FieldError) {
	var current string
	if stored != nil {
		current = get(*stored)
	}
	if submitted == nil {
		return current, nil
	}
	value := strings.TrimSpace(*submitted)
	switch {
	case value == "":
		return "", nil
	case current != "" && value == mask(current):
		return current, nil
	case strings.Contains(value, maskedMarker):
		return "", fieldErr(field, "looks like a masked value that does not match the saved one; send the real value or an empty string to clear it")
	default:
		return value, nil
	}
}

// ChannelFromPatch resolves the draft of a channel that is being tried before
// it is saved. A draft with an id inherits the saved channel's secrets the way
// a write does; one without cannot refer to a mask.
func (s Settings) ChannelFromPatch(in ChannelPatch) (Channel, *FieldError) {
	var stored *Channel
	if id := strings.TrimSpace(in.ID); id != "" {
		for _, channel := range s.Channels {
			if channel.ID == id && (in.Kind == "" || strings.EqualFold(strings.TrimSpace(in.Kind), channel.Kind)) {
				copied := channel
				stored = &copied
				break
			}
		}
	}
	merged, err := mergeChannel(stored, in, "channel")
	if err != nil {
		return Channel{}, err
	}
	if stored != nil {
		merged.ID = stored.ID
	}
	if merged.Name == "" {
		merged.Name = merged.Kind
	}
	return merged, nil
}

// MaskURL shows where a webhook points without showing what lets anyone post
// to it: the scheme and host, and the last four characters of the path.
func MaskURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return maskedMarker
	}
	return parsed.Scheme + "://" + parsed.Host + "/" + maskedMarker + lastFour(strings.TrimSuffix(parsed.EscapedPath(), "/")+rawQuerySuffix(parsed))
}

func rawQuerySuffix(parsed *url.URL) string {
	if parsed.RawQuery == "" {
		return ""
	}
	return "?" + parsed.RawQuery
}

// MaskSecret hides a signing secret completely: any character of it shown
// narrows a guess.
func MaskSecret(secret string) string {
	if strings.TrimSpace(secret) == "" {
		return ""
	}
	return maskedMarker
}

// MaskToken shows the last four characters of a token, enough to tell two
// bots apart.
func MaskToken(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	return maskedMarker + lastFour(token)
}

// lastFour returns the last four characters of value, or nothing for a value
// short enough that four characters would be most of it.
func lastFour(value string) string {
	runes := []rune(value)
	if len(runes) < 12 {
		return ""
	}
	return string(runes[len(runes)-4:])
}

// SettingsView is what the management API returns: the settings with every
// secret masked, plus what the panel needs to know about the backend.
type SettingsView struct {
	Enabled         bool          `json:"enabled"`
	Language        string        `json:"language"`
	CooldownMinutes int           `json:"cooldown_minutes"`
	Channels        []ChannelView `json:"channels"`
	Rules           Rules         `json:"rules"`
	UpdatedAt       *time.Time    `json:"updated_at,omitempty"`
	// Persistent is false when no settings store is attached; writes are then
	// refused instead of being lost on the next restart.
	Persistent bool       `json:"persistent"`
	Limits     ViewLimits `json:"limits"`
}

// ViewLimits are the bounds the server enforces, so the panel does not have to
// hard-code them.
type ViewLimits struct {
	MaxChannels int `json:"max_channels"`
}

// ChannelView is a channel with its secrets masked. Every field is always
// present so a client can read it without knowing the kind.
type ChannelView struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Enabled  bool   `json:"enabled"`
	URL      string `json:"url"`
	Secret   string `json:"secret"`
	BotToken string `json:"bot_token"`
	ChatID   string `json:"chat_id"`
}

// View renders the settings for the management API.
func (s Settings) View(persistent bool) SettingsView {
	s = s.Normalized()
	view := SettingsView{
		Enabled:         s.Enabled,
		Language:        s.Language,
		CooldownMinutes: s.CooldownMinutes,
		Channels:        make([]ChannelView, 0, len(s.Channels)),
		Rules:           s.Rules,
		Persistent:      persistent,
		Limits:          ViewLimits{MaxChannels: MaxChannels},
	}
	if !s.UpdatedAt.IsZero() {
		updated := s.UpdatedAt
		view.UpdatedAt = &updated
	}
	for _, channel := range s.Channels {
		view.Channels = append(view.Channels, ChannelView{
			ID: channel.ID, Kind: channel.Kind, Name: channel.Name, Enabled: channel.Enabled,
			URL: MaskURL(channel.URL), Secret: MaskSecret(channel.Secret),
			BotToken: MaskToken(channel.BotToken), ChatID: channel.ChatID,
		})
	}
	return view
}
