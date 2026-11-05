package privacymask

import (
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// A masked word that is a date matches that calendar date however it is
// written (JackyCcao on X: with birthdays off, the user's own birthday as a
// masked word, 1990-05-12, let 1990/5/12, 1990年5月12日 and 19900512 go as
// written). The word is a date when it is all of one: year, month and day
// with - / . 、 or a space between (1990-05-12, 1990.5.12); 1990年5月12日 or
// 1990년 5월 12일; day and month before the year where only one reading is
// a date (13/05/1990, 05/13/1990, 05/05/1990, never 05/12/1990); or a month's
// English name (May 12, 1990; 12 May 1990). Eight digits alone are not
// taken for a date word: they are as likely an id.
//
// It then matches the same date, and only it, in each of those forms, with
// or without leading zeros, and as eight digits (19900512); day-first and
// month-first only where the day is over 12 or equals the month, so no
// form can read as another date. A match never has a digit beside it, nor
// a dot and a digit (1990.5.12.1 is a version, 11990-05-12 another number),
// whatever Whole words only says: a date is matched as a whole date.

// dateSep is what stands between a date's numbers.
const dateSep = `(?:[ \t]*[-/.、／－][ \t]*|[ \t])`

var (
	dateYMD   = regexp.MustCompile(`^(\d{4})` + dateSep + `(\d{1,2})` + dateSep + `(\d{1,2})\.?$`)
	dateCJK   = regexp.MustCompile(`^(\d{4})[ \t]*[年년][ \t]*(\d{1,2})[ \t]*[月월][ \t]*(\d{1,2})[ \t]*[日号號일]?$`)
	dateDMY   = regexp.MustCompile(`^(\d{1,2})` + dateSep + `(\d{1,2})` + dateSep + `(\d{4})$`)
	dateNameF = regexp.MustCompile(`^([A-Za-z]+)\.?[ \t]+(\d{1,2})(?:st|nd|rd|th)?,?[ \t]+(\d{4})$`)
	dateNameB = regexp.MustCompile(`^(\d{1,2})(?:st|nd|rd|th)?(?:[ \t]+|-)([A-Za-z]+)\.?,?(?:[ \t]+|-)(\d{4})$`)
)

var monthNames = []string{"January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"}

// monthOf is the month a name or its first three letters say, 0 for none.
func monthOf(name string) int {
	l := strings.ToLower(name)
	if l == "sept" {
		return 9
	}
	for i, m := range monthNames {
		ml := strings.ToLower(m)
		if l == ml || len(l) == 3 && strings.HasPrefix(ml, l) {
			return i + 1
		}
	}
	return 0
}

// realDate says y-m-d is a day of the calendar.
func realDate(y, m, d int) bool {
	if y < 1000 || y > 2999 || m < 1 || m > 12 || d < 1 {
		return false
	}
	t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
	return t.Day() == d && int(t.Month()) == m
}

// parseDateWord is the date the word w is, where it is one.
func parseDateWord(w string) (y, m, d int, ok bool) {
	n := func(s string) int { v, _ := strconv.Atoi(s); return v }
	if g := dateYMD.FindStringSubmatch(w); g != nil {
		y, m, d = n(g[1]), n(g[2]), n(g[3])
	} else if g := dateCJK.FindStringSubmatch(w); g != nil {
		y, m, d = n(g[1]), n(g[2]), n(g[3])
	} else if g := dateDMY.FindStringSubmatch(w); g != nil {
		p, q := n(g[1]), n(g[2])
		y = n(g[3])
		switch {
		case p == q:
			m, d = p, q
		case p > 12 && q <= 12:
			d, m = p, q
		case q > 12 && p <= 12:
			m, d = p, q
		default: // 05/12/1990: May 12th or December 5th
			return 0, 0, 0, false
		}
	} else if g := dateNameF.FindStringSubmatch(w); g != nil {
		m, d, y = monthOf(g[1]), n(g[2]), n(g[3])
	} else if g := dateNameB.FindStringSubmatch(w); g != nil {
		d, m, y = n(g[1]), monthOf(g[2]), n(g[3])
	} else {
		return 0, 0, 0, false
	}
	return y, m, d, realDate(y, m, d)
}

// dateForms is a pattern for the date y-m-d in each form a date word
// matches, and the word itself as written.
func dateForms(y, m, d int, w string) string {
	num := func(v int) string { // 5 or 05; 12
		if v < 10 {
			return "0?" + strconv.Itoa(v)
		}
		return strconv.Itoa(v)
	}
	Y, M, D := strconv.Itoa(y), num(m), num(d)
	full, abbr := monthNames[m-1], monthNames[m-1][:3]
	if m == 9 {
		abbr = "Sept|Sep"
	}
	name := `(?i:` + full + `|(?:` + abbr + `)\.?)`
	ord := `(?:st|nd|rd|th)?`
	forms := []string{
		Y + dateSep + M + dateSep + D,
		Y + `[ \t]*年[ \t]*` + M + `[ \t]*月[ \t]*` + D + `(?:[ \t]*[日号號])?`,
		Y + `[ \t]*년[ \t]*` + M + `[ \t]*월[ \t]*` + D + `(?:[ \t]*일)?`,
		Y + strconv.Itoa(100 + m)[1:] + strconv.Itoa(100 + d)[1:],
		name + `[ \t]+` + D + ord + `,?[ \t]+` + Y,
		D + ord + `(?:[ \t]+|-)` + name + `,?(?:[ \t]+|-)` + Y,
	}
	// day-first and month-first only where no other date reads the same
	if d > 12 || d == m {
		forms = append(forms, D+dateSep+M+dateSep+Y)
		if d != m {
			forms = append(forms, M+dateSep+D+dateSep+Y)
		}
	}
	return `(?:` + strings.Join(forms, "|") + `|` + regexp.QuoteMeta(w) + `)`
}

// dateWords holds each word's pattern, nil for one that is no date. Bounded
// (boundedCache): every tenant and portal user may have words of their own.
var dateWords = boundedCache{max: 4096}

// dateWord is the pattern for the date the masked word w is, nil where it
// is no date.
func dateWord(w string) *regexp.Regexp {
	return dateWords.get(w, func() *regexp.Regexp {
		if y, m, d, ok := parseDateWord(w); ok {
			return regexp.MustCompile(dateForms(y, m, d, w))
		}
		return nil
	})
}

// dateWhole says the date at s[a:b] is no part of a longer number: no
// digit beside it, no dot and a digit (a version's 1990.5.12.1, 3.1990.5.12),
// and no letter before a month's name (xMay), save in a script that puts
// no spaces between words (生日是May 12, 1990).
func dateWhole(s string, a, b int) bool {
	isDigit := func(c byte) bool { return c >= '0' && c <= '9' }
	if a > 0 {
		if isDigit(s[a-1]) || s[a-1] == '.' && a > 1 && isDigit(s[a-2]) {
			return false
		}
		if r, _ := utf8.DecodeLastRuneInString(s[:a]); unicode.IsLetter(rune(s[a])) && unicode.IsLetter(r) && !unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) {
			return false
		}
	}
	if b < len(s) {
		if isDigit(s[b]) || s[b] == '.' && b+1 < len(s) && isDigit(s[b+1]) {
			return false
		}
	}
	return true
}

// dateSpans are where s has the date the masked word w is, nil and false
// where w is no date.
func dateSpans(s, w string) ([]span, bool) {
	re := dateWord(w)
	if re == nil {
		return nil, false
	}
	var out []span
	for _, m := range re.FindAllStringIndex(s, -1) {
		if dateWhole(s, m[0], m[1]) {
			out = append(out, span{m[0], m[1], "TERM", ByWord, 0, false})
		}
	}
	return out, true
}
