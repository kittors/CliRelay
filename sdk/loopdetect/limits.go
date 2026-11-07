package loopdetect

import "fmt"

// MaxReplyDistinct is the most different units a window of that many units may
// be allowed to hold and still count as a loop: a quarter of the window, so that
// however the threshold is set, ordinary text never reaches it. A window of 0
// stands for the default.
func MaxReplyDistinct(window int) int {
	if window <= 0 {
		window = DefaultReplyWindow
	}
	return min(max(window, MinReplyWindow), MaxReplyWindow) / MaxDistinctShare
}

// Validate says why l is outside the bounds Normalized holds it to, so a setting
// a person typed is refused with the reason instead of quietly becoming another
// number. A zero field is not an error: it stands for the default.
func (l ReplyLimits) Validate() error {
	if l.Window != 0 && (l.Window < MinReplyWindow || l.Window > MaxReplyWindow) {
		return fmt.Errorf("window must be between %d and %d units", MinReplyWindow, MaxReplyWindow)
	}
	if l.Distinct != 0 {
		window := l.Window
		if window == 0 {
			window = DefaultReplyWindow
		}
		if upper := MaxReplyDistinct(window); l.Distinct < MinReplyDistinct || l.Distinct > upper {
			return fmt.Errorf("max_distinct must be between %d and %d for a window of %d units", MinReplyDistinct, upper, window)
		}
	}
	return nil
}

// Validate says why l is outside the bounds Normalized holds it to. A zero
// Repeats stands for the default.
func (l ToolLimits) Validate() error {
	if l.Repeats != 0 && (l.Repeats < MinToolRepeats || l.Repeats > MaxToolRepeats) {
		return fmt.Errorf("repeats must be between %d and %d turns", MinToolRepeats, MaxToolRepeats)
	}
	return nil
}
