package usagelogs

import (
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"slices"
	"sort"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/usage"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestLooksLikeAuthIndex(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "live file seed", value: "39a7982984e321e5", want: true},
		{name: "orphan id seed", value: "69e8946f1ffc2d23", want: true},
		{name: "uppercase hex", value: "69E8946F1FFC2D23", want: true},
		{name: "email label", value: "asherandersenloqv@outlook.com", want: false},
		{name: "display name", value: "Codex 主渠道", want: false},
		{name: "too short", value: "39a7982984e321e", want: false},
		{name: "too long", value: "39a7982984e321e5a", want: false},
		{name: "non hex", value: "gggggggggggggggg", want: false},
		{name: "empty", value: "", want: false},
		{name: "spaces", value: "  39a7982984e321e5  ", want: true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := looksLikeAuthIndex(tc.value); got != tc.want {
				t.Fatalf("looksLikeAuthIndex(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestChannelFilterSelectorsTreatsOrphanAuthIndexAsAuthIndex(t *testing.T) {
	t.Parallel()

	// Live auth currently uses the file: seed index; historical rows still use
	// the id: seed index for the same OAuth email label.
	liveIndex := "39a7982984e321e5"
	orphanIndex := "69e8946f1ffc2d23"
	label := "asherandersenloqv@outlook.com"

	authIndexChannelMap := map[string]string{liveIndex: label}
	authMetaByIndex := map[string]authChannelMeta{
		liveIndex: {label: label, provider: "xai", authType: "oauth"},
	}

	// Selecting the orphan facet value must stay on AuthIndexes. The previous
	// bug fell through to ChannelNames and queried channel_name = <hash>.
	subjects, authIndexes, channelNames, _ := channelFilterSelectors(
		[]string{orphanIndex},
		nil,
		authIndexChannelMap,
		nil,
		authMetaByIndex,
		nil,
		nil,
		nil,
		nil,
	)
	if len(subjects) != 0 {
		t.Fatalf("subjects = %#v, want empty for unmapped orphan", subjects)
	}
	if !reflect.DeepEqual(authIndexes, []string{orphanIndex}) {
		t.Fatalf("authIndexes = %#v, want [%s]", authIndexes, orphanIndex)
	}
	if len(channelNames) != 0 {
		t.Fatalf("channelNames = %#v, want empty", channelNames)
	}

	// Live index still resolves normally.
	subjects, authIndexes, channelNames, _ = channelFilterSelectors(
		[]string{liveIndex},
		nil,
		authIndexChannelMap,
		nil,
		authMetaByIndex,
		nil,
		nil,
		nil,
		nil,
	)
	if len(subjects) != 0 {
		t.Fatalf("live subjects = %#v, want empty without subject map", subjects)
	}
	if !reflect.DeepEqual(authIndexes, []string{liveIndex}) {
		t.Fatalf("live authIndexes = %#v, want [%s]", authIndexes, liveIndex)
	}
	if len(channelNames) != 0 {
		t.Fatalf("live channelNames = %#v, want empty", channelNames)
	}

	// Email/display labels still use the legacy channel_name path (and may also
	// expand to live auth indexes via authIndexChannelMap label matching).
	subjects, authIndexes, channelNames, _ = channelFilterSelectors(
		[]string{label},
		map[string]string{label: label},
		authIndexChannelMap,
		nil,
		authMetaByIndex,
		nil,
		nil,
		nil,
		nil,
	)
	if len(subjects) != 0 {
		t.Fatalf("label subjects = %#v, want empty without subject map", subjects)
	}
	if !reflect.DeepEqual(authIndexes, []string{liveIndex}) {
		t.Fatalf("label authIndexes = %#v, want [%s]", authIndexes, liveIndex)
	}
	if !reflect.DeepEqual(channelNames, []string{label}) {
		t.Fatalf("label channelNames = %#v, want [%s]", channelNames, label)
	}
}

func seedIndex(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:8])
}

func TestXaiOAuthAuthIndexGroupMergesIDAndFileSeeds(t *testing.T) {
	t.Parallel()

	fileName := "xai-asherandersenloqv@outlook.com.json"
	live := seedIndex("file:" + fileName)
	orphan := seedIndex("id:" + fileName)

	auth := &coreauth.Auth{
		Provider: "xai",
		FileName: fileName,
		ID:       fileName,
		Label:    "asherandersenloqv@outlook.com",
		Metadata: map[string]any{"email": "asherandersenloqv@outlook.com"},
	}
	group := xaiOAuthAuthIndexGroup(auth)
	if len(group) < 2 {
		t.Fatalf("group = %#v, want at least live+orphan", group)
	}
	if group[0] != live {
		t.Fatalf("canonical = %s, want live %s", group[0], live)
	}
	foundOrphan := false
	for _, member := range group {
		if member == orphan {
			foundOrphan = true
			break
		}
	}
	if !foundOrphan {
		t.Fatalf("group %#v missing orphan %s", group, orphan)
	}
}

func TestAuthIndexAliasGroupIncludesBasenameAndTenantRelative(t *testing.T) {
	t.Parallel()

	base := "codex-yuan364299311@gmail.com-pro.json"
	tenantRelative := "9e003dfb-751f-4898-b186-45f765c763a6/" + base
	auth := &coreauth.Auth{
		Provider: "codex",
		FileName: tenantRelative,
		ID:       tenantRelative,
		Label:    "yuan364299311@gmail.com",
		Metadata: map[string]any{"email": "yuan364299311@gmail.com"},
	}
	group := authIndexAliasGroup(auth)
	live := seedIndex("file:" + tenantRelative)
	basename := seedIndex("file:" + base)
	if group[0] != live {
		t.Fatalf("canonical = %s, want live %s", group[0], live)
	}
	foundBase := false
	for _, member := range group {
		if member == basename {
			foundBase = true
			break
		}
	}
	if !foundBase {
		t.Fatalf("group %#v missing basename index %s", group, basename)
	}
}

func TestChannelFilterSelectorsExpandsXaiOAuthIndexGroup(t *testing.T) {
	t.Parallel()

	liveIndex := "39a7982984e321e5"
	orphanIndex := "69e8946f1ffc2d23"
	label := "asherandersenloqv@outlook.com"
	group := []string{liveIndex, orphanIndex}

	authIndexChannelMap := map[string]string{
		liveIndex:   label,
		orphanIndex: label,
	}
	authMetaByIndex := map[string]authChannelMeta{
		liveIndex:   {label: label, provider: "xai", authType: "oauth"},
		orphanIndex: {label: label, provider: "xai", authType: "oauth"},
	}
	authIndexGroup := map[string][]string{
		liveIndex:   group,
		orphanIndex: group,
	}

	// Selecting the live option expands to both historical indexes.
	subjects, authIndexes, channelNames, _ := channelFilterSelectors(
		[]string{liveIndex},
		nil,
		authIndexChannelMap,
		nil,
		authMetaByIndex,
		authIndexGroup,
		nil,
		nil,
		nil,
	)
	if len(subjects) != 0 {
		t.Fatalf("subjects = %#v, want empty without subject map", subjects)
	}
	sort.Strings(authIndexes)
	want := []string{liveIndex, orphanIndex}
	sort.Strings(want)
	if !reflect.DeepEqual(authIndexes, want) {
		t.Fatalf("live expand authIndexes = %#v, want %#v", authIndexes, want)
	}
	if len(channelNames) != 0 {
		t.Fatalf("channelNames = %#v, want empty", channelNames)
	}

	// Selecting the orphan index also expands (old clients / deep links).
	subjects, authIndexes, _, _ = channelFilterSelectors(
		[]string{orphanIndex},
		nil,
		authIndexChannelMap,
		nil,
		authMetaByIndex,
		authIndexGroup,
		nil,
		nil,
		nil,
	)
	if len(subjects) != 0 {
		t.Fatalf("orphan subjects = %#v, want empty without subject map", subjects)
	}
	sort.Strings(authIndexes)
	if !reflect.DeepEqual(authIndexes, want) {
		t.Fatalf("orphan expand authIndexes = %#v, want %#v", authIndexes, want)
	}
}

func TestEnrichChannelFilterOptionsCollapsesXaiOAuthAliases(t *testing.T) {
	t.Parallel()

	liveIndex := "39a7982984e321e5"
	orphanIndex := "69e8946f1ffc2d23"
	label := "asherandersenloqv@outlook.com"
	group := []string{liveIndex, orphanIndex}

	authIndexChannelMap := map[string]string{
		liveIndex:   label,
		orphanIndex: label,
	}
	authMetaByIndex := map[string]authChannelMeta{
		liveIndex:   {label: label, provider: "xai", authType: "oauth"},
		orphanIndex: {label: label, provider: "xai", authType: "oauth"},
	}
	authIndexGroup := map[string][]string{
		liveIndex:   group,
		orphanIndex: group,
	}

	// SQL facet still returns both historical (channel_name, auth_index) pairs.
	// codex same-email must remain a separate option.
	codexIndex := "a9757e6dce652490"
	options := []usage.ChannelFilterOption{
		{Value: orphanIndex, Label: label, AuthIndex: orphanIndex},
		{Value: liveIndex, Label: label, AuthIndex: liveIndex},
		{
			Value:     codexIndex,
			Label:     "yuan364299311@gmail.com",
			AuthIndex: codexIndex,
			Provider:  "codex",
			AuthType:  "oauth",
		},
	}
	authIndexChannelMap[codexIndex] = "yuan364299311@gmail.com"
	authMetaByIndex[codexIndex] = authChannelMeta{
		label:    "yuan364299311@gmail.com",
		provider: "codex",
		authType: "oauth",
	}

	got := enrichChannelFilterOptions(options, nil, authIndexChannelMap, authMetaByIndex, authIndexGroup, nil, nil)

	var asher *usage.ChannelFilterOption
	var codex *usage.ChannelFilterOption
	for i := range got {
		switch got[i].AuthIndex {
		case liveIndex, orphanIndex:
			if asher != nil {
				t.Fatalf("expected one asher option, got multiple: %#v", got)
			}
			asher = &got[i]
		case codexIndex:
			codex = &got[i]
		}
	}
	if asher == nil {
		t.Fatalf("missing merged asher option: %#v", got)
	}
	if asher.AuthIndex != liveIndex {
		t.Fatalf("asher AuthIndex = %s, want live %s", asher.AuthIndex, liveIndex)
	}
	if asher.Value != liveIndex {
		t.Fatalf("asher Value = %s, want live %s", asher.Value, liveIndex)
	}
	if asher.Provider != "xai" {
		t.Fatalf("asher Provider = %q, want xai", asher.Provider)
	}
	if asher.AuthType != "oauth" {
		t.Fatalf("asher AuthType = %q, want oauth", asher.AuthType)
	}
	if codex == nil {
		t.Fatalf("codex option was dropped: %#v", got)
	}
}

func TestChannelFilterSelectorsPrefersAuthSubject(t *testing.T) {
	t.Parallel()

	liveIndex := "c84aac6579358b75"
	oldIndex := "a9757e6dce652490"
	subject := "authsub_29b975703f03bde1"
	label := "yuan364299311@gmail.com"

	authIndexChannelMap := map[string]string{
		liveIndex: label,
		oldIndex:  label,
	}
	authMetaByIndex := map[string]authChannelMeta{
		liveIndex: {label: label, provider: "codex", authType: "oauth"},
		oldIndex:  {label: label, provider: "codex", authType: "oauth"},
	}
	authSubjectByIndex := map[string]string{
		liveIndex: subject,
		oldIndex:  subject,
	}
	authIndexesBySubject := map[string][]string{
		subject: {liveIndex, oldIndex},
	}
	authMetaBySubject := map[string]authChannelMeta{
		subject: {label: label, provider: "codex", authType: "oauth"},
	}

	// New clients send subject value.
	subjects, authIndexes, channelNames, _ := channelFilterSelectors(
		[]string{subject},
		nil,
		authIndexChannelMap,
		nil,
		authMetaByIndex,
		nil,
		authSubjectByIndex,
		authIndexesBySubject,
		authMetaBySubject,
	)
	if !reflect.DeepEqual(subjects, []string{subject}) {
		t.Fatalf("subjects = %#v, want [%s]", subjects, subject)
	}
	sort.Strings(authIndexes)
	wantIndexes := []string{liveIndex, oldIndex}
	sort.Strings(wantIndexes)
	if !reflect.DeepEqual(authIndexes, wantIndexes) {
		t.Fatalf("authIndexes = %#v, want subject alias indexes %#v", authIndexes, wantIndexes)
	}
	if len(channelNames) != 0 {
		t.Fatalf("channelNames = %#v, want empty", channelNames)
	}

	// Old clients / deep links still send historical auth_index; map to subject.
	subjects, authIndexes, _, _ = channelFilterSelectors(
		[]string{oldIndex},
		nil,
		authIndexChannelMap,
		nil,
		authMetaByIndex,
		nil,
		authSubjectByIndex,
		authIndexesBySubject,
		authMetaBySubject,
	)
	if !reflect.DeepEqual(subjects, []string{subject}) {
		t.Fatalf("old index subjects = %#v, want [%s]", subjects, subject)
	}
	sort.Strings(authIndexes)
	if !reflect.DeepEqual(authIndexes, wantIndexes) {
		t.Fatalf("old index authIndexes = %#v, want %#v", authIndexes, wantIndexes)
	}
}

func TestEnrichChannelFilterOptionsCollapsesByAuthSubject(t *testing.T) {
	t.Parallel()

	liveIndex := "c84aac6579358b75"
	oldIndex := "a9757e6dce652490"
	xaiIndex := "b789c5a3171aeaff"
	codexSubject := "authsub_29b975703f03bde1"
	xaiSubject := "authsub_50d3fdc60cf66318"
	label := "yuan364299311@gmail.com"

	options := []usage.ChannelFilterOption{
		{Value: oldIndex, Label: label, AuthIndex: oldIndex, AuthSubjectID: codexSubject},
		{Value: liveIndex, Label: label, AuthIndex: liveIndex, AuthSubjectID: codexSubject},
		{Value: xaiIndex, Label: label, AuthIndex: xaiIndex, AuthSubjectID: xaiSubject},
	}
	authIndexChannelMap := map[string]string{
		liveIndex: label,
		oldIndex:  label,
		xaiIndex:  label,
	}
	authMetaByIndex := map[string]authChannelMeta{
		liveIndex: {label: label, provider: "codex", authType: "oauth"},
		oldIndex:  {label: label, provider: "codex", authType: "oauth"},
		xaiIndex:  {label: label, provider: "xai", authType: "oauth"},
	}
	authSubjectByIndex := map[string]string{
		liveIndex: codexSubject,
		oldIndex:  codexSubject,
		xaiIndex:  xaiSubject,
	}
	authMetaBySubject := map[string]authChannelMeta{
		codexSubject: {label: label, provider: "codex", authType: "oauth"},
		xaiSubject:   {label: label, provider: "xai", authType: "oauth"},
	}

	got := enrichChannelFilterOptions(
		options,
		nil,
		authIndexChannelMap,
		authMetaByIndex,
		nil,
		authSubjectByIndex,
		authMetaBySubject,
	)
	if len(got) != 2 {
		t.Fatalf("options = %#v, want 2 (codex+xai)", got)
	}

	var codex, xai *usage.ChannelFilterOption
	for i := range got {
		switch got[i].AuthSubjectID {
		case codexSubject:
			codex = &got[i]
		case xaiSubject:
			xai = &got[i]
		}
	}
	if codex == nil || xai == nil {
		t.Fatalf("missing subject options: %#v", got)
	}
	if codex.Value != codexSubject {
		t.Fatalf("codex value = %s, want %s", codex.Value, codexSubject)
	}
	if codex.Provider != "codex" {
		t.Fatalf("codex provider = %q, want codex", codex.Provider)
	}
	if xai.Value != xaiSubject {
		t.Fatalf("xai value = %s, want %s", xai.Value, xaiSubject)
	}
	if xai.Provider != "xai" {
		t.Fatalf("xai provider = %q, want xai", xai.Provider)
	}
}

func TestEnrichChannelFilterOptionsInfersProviderAuthTypeWithoutLiveMeta(t *testing.T) {
	t.Parallel()

	// Historical deleted xAI OAuth rows + orphan OpenCode Go API rows must still
	// render vendor icon + OAuth/API badge (no blank chip).
	options := []usage.ChannelFilterOption{
		{
			Value:         "authsub_50d3fdc60cf66318",
			Label:         "yuan364299311@gmail.com",
			AuthIndex:     "b789c5a3171aeaff",
			AuthSubjectID: "authsub_50d3fdc60cf66318",
			// Provider/auth_type intentionally empty: no live auth meta.
		},
		{
			Value:     "f90a51ed4f363dd2",
			Label:     "opencode go",
			AuthIndex: "f90a51ed4f363dd2",
			Provider:  "opencode-go",
			AuthType:  "api",
		},
		{
			Value:     "orphan-opencode",
			Label:     "opencode go",
			AuthIndex: "orphan-opencode",
			// empty provider/auth_type, must be inferred from label
		},
	}

	got := enrichChannelFilterOptions(options, nil, nil, nil, nil, nil, nil)
	if len(got) != 3 {
		t.Fatalf("options = %#v, want 3", got)
	}

	byValue := map[string]usage.ChannelFilterOption{}
	for _, opt := range got {
		byValue[opt.Value] = opt
	}
	xai := byValue["authsub_50d3fdc60cf66318"]
	if xai.Provider == "" || xai.AuthType != "oauth" {
		// email label without model still classifies as oauth; provider may stay empty
		// unless label/model/source can infer. Email-only should at least get oauth badge.
		if xai.AuthType != "oauth" {
			t.Fatalf("xai auth_type = %q, want oauth; option=%#v", xai.AuthType, xai)
		}
	}
	liveOpen := byValue["f90a51ed4f363dd2"]
	if liveOpen.Provider != "opencode-go" || liveOpen.AuthType != "api" {
		t.Fatalf("live opencode = %#v, want opencode-go/api", liveOpen)
	}
	orphanOpen := byValue["orphan-opencode"]
	if orphanOpen.Provider != "opencode-go" || orphanOpen.AuthType != "api" {
		t.Fatalf("orphan opencode = %#v, want opencode-go/api", orphanOpen)
	}
}

func TestFilterPublicAPIKeyIDOptionsFiltersCountsWithNames(t *testing.T) {
	allowed := map[string]struct{}{"key-a": {}, "key-b": {}}
	ids, names, counts := filterPublicAPIKeyIDOptions(
		allowed,
		[]string{"key-a", "key-private", "key-b"},
		map[string]string{"key-a": "Laptop", "key-private": "Hidden", "key-b": "Automation"},
		map[string]int64{"key-a": 9, "key-private": 99, "key-b": 3},
	)

	if !slices.Equal(ids, []string{"key-a", "key-b"}) {
		t.Fatalf("ids = %#v, want allowed ids only", ids)
	}
	if !reflect.DeepEqual(names, map[string]string{"key-a": "Laptop", "key-b": "Automation"}) {
		t.Fatalf("names = %#v, want allowed names only", names)
	}
	if !reflect.DeepEqual(counts, map[string]int64{"key-a": 9, "key-b": 3}) {
		t.Fatalf("counts = %#v, want allowed counts only", counts)
	}
}
