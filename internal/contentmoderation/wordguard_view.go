package contentmoderation

// WordSettings are the settings a policy resolves to once the defaults, and for
// a key policy the tenant policy beneath it, are applied. The panel shows them
// as what a setting left empty means.
type WordSettings struct {
	Action                 string `json:"action"`
	Replacement            string `json:"replacement"`
	ScanScope              string `json:"scan_scope"`
	BlockMessage           string `json:"block_message"`
	ReplyFilter            bool   `json:"reply_filter"`
	FoldTraditionalChinese bool   `json:"fold_traditional_chinese"`
}

func exportWordSettings(s wordSettings) WordSettings {
	return WordSettings{
		Action:                 s.action,
		Replacement:            s.replacement,
		ScanScope:              s.scanScope,
		BlockMessage:           s.blockMessage,
		ReplyFilter:            s.reply,
		FoldTraditionalChinese: s.fold,
	}
}

// WordDefaults are the settings of a policy that sets nothing.
func WordDefaults() WordSettings { return exportWordSettings(mergeWordSettings()) }

// ResolveWordSettings returns the settings policy is applied with. below is the
// tenant policy under a key policy and nil for any other; it counts only while
// it is enabled, as in a request (a key that keeps its own rules instead of the
// tenant's still takes the tenant's settings). The policy itself counts whether
// or not it is enabled: this says what its settings mean, not whether they are
// in force.
func ResolveWordSettings(below, policy *WordPolicy) WordSettings {
	if below != nil && !below.Enabled {
		below = nil
	}
	return exportWordSettings(mergeWordSettings(below, policy))
}

// WordPolicyLimits are the bounds a policy is saved within.
type WordPolicyLimits struct {
	MaxRules        int `json:"max_rules"`
	MaxRegex        int `json:"max_regex"`
	MaxKeywordChars int `json:"max_keyword_chars"`
	MaxRegexBytes   int `json:"max_regex_bytes"`
	// MaxRegexProgram and MaxRegexProgramTotal bound the compiled size of one
	// expression and of all of a policy's, in instructions.
	MaxRegexProgram      int `json:"max_regex_program"`
	MaxRegexProgramTotal int `json:"max_regex_program_total"`
	MaxNoteChars         int `json:"max_note_chars"`
	MaxReplacementChars  int `json:"max_replacement_chars"`
	MaxBlockMessageChars int `json:"max_block_message_chars"`
}

// WordLimits returns the bounds a policy is saved within.
func WordLimits() WordPolicyLimits {
	return WordPolicyLimits{
		MaxRules:             MaxWordRulesPerPolicy,
		MaxRegex:             MaxWordRegexPerPolicy,
		MaxKeywordChars:      MaxWordKeywordRunes,
		MaxRegexBytes:        MaxWordRegexBytes,
		MaxRegexProgram:      MaxWordRegexProgram,
		MaxRegexProgramTotal: MaxWordRegexProgramTotal,
		MaxNoteChars:         MaxWordNoteRunes,
		MaxReplacementChars:  MaxWordReplacementRunes,
		MaxBlockMessageChars: MaxWordBlockMessageRunes,
	}
}
