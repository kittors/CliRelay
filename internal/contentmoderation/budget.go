package contentmoderation

import (
	"sort"
	"strings"
	"unicode/utf8"

	log "github.com/sirupsen/logrus"
)

// Both moderation backends scan a bounded amount of text per request. When a
// request carries more, they keep the newest text and skip the oldest, and the
// newest user message is kept before anything else. Text is ordered oldest
// first (system prompt, then the conversation), and a coding agent's system
// prompt alone can exceed the budget; a window anchored at the start used to
// fill up before reaching what the caller is asking now.

// skippedChars counts, per class, the characters a backend did not scan.
type skippedChars [classCount]int

func (s skippedChars) total() int {
	total := 0
	for _, count := range s {
		total += count
	}
	return total
}

// logSkipped makes a budget cut visible: unscanned text is a coverage gap the
// operator must be able to see, and close by raising the limits where that is
// possible. Keyword checks are unaffected; they always read the whole request.
func logSkipped(profile Profile, skipped skippedChars, scannedChars int, message string, extra log.Fields) {
	if skipped.total() == 0 {
		return
	}
	fields := log.Fields{
		"profile_id":                profile.ID,
		"backend":                   profile.Backend,
		"scanned_chars":             scannedChars,
		"skipped_chars":             skipped.total(),
		"skipped_primary_chars":     skipped[ClassPrimary],
		"skipped_tool_output_chars": skipped[ClassToolOutput],
	}
	for key, value := range extra {
		fields[key] = value
	}
	log.WithFields(fields).Warn(message)
}

// piece is the kept part of one segment: all of it, or its tail.
type piece struct {
	index int
	text  string
}

// splitNewestUserMessage separates the primary segments of the newest user
// message from the rest. A request without any user turn falls back to its
// newest primary message.
func splitNewestUserMessage(segments []Segment) (newest, rest []int) {
	turn, found := 0, false
	for i := len(segments) - 1; i >= 0 && !found; i-- {
		if segments[i].Class == ClassPrimary && segments[i].User {
			turn, found = segments[i].Turn, true
		}
	}
	for i := len(segments) - 1; i >= 0 && !found; i-- {
		if segments[i].Class == ClassPrimary {
			turn, found = segments[i].Turn, true
		}
	}
	for i, segment := range segments {
		if found && segment.Class == ClassPrimary && segment.Turn == turn {
			newest = append(newest, i)
		} else {
			rest = append(rest, i)
		}
	}
	return newest, rest
}

// keepNewest keeps the newest text of the listed segments that fits in budget
// characters, counting one separator between kept pieces. The oldest piece it
// keeps may be cut down to its tail; older segments are skipped and counted.
// Pieces are returned oldest first.
func keepNewest(segments []Segment, indexes []int, budget int, skipped *skippedChars) []piece {
	kept := make([]piece, 0, len(indexes))
	i := len(indexes) - 1
	for ; i >= 0; i-- {
		if len(kept) > 0 {
			budget-- // the separator before the piece kept last
		}
		if budget <= 0 {
			break
		}
		segment := segments[indexes[i]]
		length := utf8.RuneCountInString(segment.Text)
		if length <= budget {
			kept = append(kept, piece{index: indexes[i], text: segment.Text})
			budget -= length
			continue
		}
		kept = append(kept, piece{index: indexes[i], text: lastRunes(segment.Text, budget)})
		skipped[segment.Class] += length - budget
		i--
		break
	}
	for ; i >= 0; i-- {
		segment := segments[indexes[i]]
		skipped[segment.Class] += utf8.RuneCountInString(segment.Text)
	}
	for left, right := 0, len(kept)-1; left < right; left, right = left+1, right-1 {
		kept[left], kept[right] = kept[right], kept[left]
	}
	return kept
}

func joinPieces(pieces []piece) string {
	texts := make([]string, len(pieces))
	for i, p := range pieces {
		texts[i] = p.text
	}
	return strings.Join(texts, " ")
}

// lastRunes returns the final n runes of s.
func lastRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	end := len(s)
	for ; n > 0 && end > 0; n-- {
		_, size := utf8.DecodeLastRuneInString(s[:end])
		end -= size
	}
	return s[end:]
}

// splitTail cuts text into chunks of at most limit runes, aligned to the end of
// the text, so the newest characters always share a chunk with as much of their
// context as fits. Chunks are returned oldest first.
func splitTail(text string, limit int) []string {
	runes := []rune(text)
	if len(runes) == 0 || limit <= 0 {
		return nil
	}
	chunks := make([]string, (len(runes)+limit-1)/limit)
	end := len(runes)
	for i := len(chunks) - 1; i >= 0; i-- {
		start := end - limit
		if start < 0 {
			start = 0
		}
		chunks[i] = string(runes[start:end])
		end = start
	}
	return chunks
}

// selectNewestText builds one moderation input of at most budget characters:
// the newest user message first, then the remaining text from newest to
// oldest, joined back in conversation order.
func selectNewestText(segments []Segment, budget int) (string, skippedChars) {
	var skipped skippedChars
	newest, rest := splitNewestUserMessage(segments)
	kept := keepNewest(segments, newest, budget, &skipped)
	remaining := budget - utf8.RuneCountInString(joinPieces(kept))
	if len(kept) > 0 {
		remaining-- // the separator between the two groups
	}
	kept = append(kept, keepNewest(segments, rest, remaining, &skipped)...)
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].index < kept[j].index })
	return joinPieces(kept), skipped
}

// guardChunks plans a Qwen3Guard scan of at most maxChunks chunks of limit
// characters, in the order they should be sent: newest first, so the budget
// and an early block are spent on what matters most.
//
// The newest user message is chunked on its own. Qwen3Guard classifies one
// prompt at a time, and a short final question sharing a chunk with the tail of
// a long system prompt is judged mostly on the system prompt. That can leave
// its chunk partly empty, which costs at most one chunk's worth of older text.
func guardChunks(segments []Segment, limit, maxChunks int) ([]string, skippedChars) {
	if limit <= 0 {
		limit = DefaultInputLimit
	}
	if maxChunks <= 0 {
		maxChunks = DefaultMaxChunks
	}
	var skipped skippedChars
	newest, rest := splitNewestUserMessage(segments)
	newestChunks := splitTail(joinPieces(keepNewest(segments, newest, limit*maxChunks, &skipped)), limit)
	restChunks := splitTail(joinPieces(keepNewest(segments, rest, limit*(maxChunks-len(newestChunks)), &skipped)), limit)
	chunks := make([]string, 0, len(newestChunks)+len(restChunks))
	for i := len(newestChunks) - 1; i >= 0; i-- {
		chunks = append(chunks, newestChunks[i])
	}
	for i := len(restChunks) - 1; i >= 0; i-- {
		chunks = append(chunks, restChunks[i])
	}
	return chunks, skipped
}
