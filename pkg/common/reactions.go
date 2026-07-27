package common

import (
	"regexp"
	"strings"
	"unicode"
)

// ReactionFallback is a parsed reaction-fallback SMS. Phones that fail to
// deliver a reaction over a rich channel (iMessage tapbacks, RCS reactions)
// send a localized plain-text stand-in instead — "Reacted 😂 to \"msg\"",
// "Liked \"msg\"", « A réagi 😂 à « msg » » — which we convert back into a
// real Matrix reaction on the quoted message.
type ReactionFallback struct {
	// Emoji is the reaction emoji (tapback verbs are mapped to emoji).
	Emoji string
	// Snippet is the quoted excerpt of the target message, ellipsis stripped.
	Snippet string
	// Truncated is true when the quote ended with an ellipsis, i.e. Snippet
	// is only a prefix of the target message.
	Truncated bool
	// Remove is true for "Removed a like from …"-style fallbacks.
	Remove bool
}

// fq / fqs: French guillemet content, tolerating the NBSP/narrow-NBSP
// padding phones put inside « ».
const frOpen = `«[\s\x{00A0}\x{202F}]*`
const frClose = `[\s\x{00A0}\x{202F}]*»`

// enQuoted captures inside straight or curly double quotes.
const enOpen = `["\x{201C}]`
const enClose = `["\x{201D}]`

var emojiReactionPatterns = []struct {
	re     *regexp.Regexp
	remove bool
}{
	// Google Messages + iOS 17 custom emoji, English.
	{re: regexp.MustCompile(`^Reacted (\S+) to ` + enOpen + `(.+)` + enClose + `$`)},
	{re: regexp.MustCompile(`^Removed (\S+) from ` + enOpen + `(.+)` + enClose + `$`), remove: true},
	// French.
	{re: regexp.MustCompile(`^A réagi (\S+) à ` + frOpen + `(.+?)` + frClose + `$`)},
	{re: regexp.MustCompile(`^A retiré la réaction (\S+) (?:à|de) ` + frOpen + `(.+?)` + frClose + `$`), remove: true},
}

// Apple tapback verbs → emoji, English and French.
var tapbackVerbs = map[string]string{
	"Liked":        "👍",
	"Loved":        "❤️",
	"Disliked":     "👎",
	"Laughed at":   "😆",
	"Emphasized":   "‼️",
	"Questioned":   "❓",
	"A aimé":       "👍",
	"A adoré":      "❤️",
	"N’a pas aimé": "👎",
	"N'a pas aimé": "👎",
	"A ri de":      "😆",
	"A souligné":   "‼️",
}

// Apple tapback removal nouns → emoji (English).
var tapbackRemoveNouns = map[string]string{
	"like":          "👍",
	"heart":         "❤️",
	"dislike":       "👎",
	"laugh":         "😆",
	"exclamation":   "‼️",
	"question mark": "❓",
}

var (
	tapbackVerbRe   = regexp.MustCompile(`^(Liked|Loved|Disliked|Laughed at|Emphasized|Questioned) ` + enOpen + `(.+)` + enClose + `$`)
	tapbackVerbFrRe = regexp.MustCompile(`^(A aimé|A adoré|N[’']a pas aimé|A ri de|A souligné) ` + frOpen + `(.+?)` + frClose + `$`)
	tapbackRemoveRe = regexp.MustCompile(`^Removed an? (like|heart|dislike|laugh|exclamation|question mark) from ` + enOpen + `(.+)` + enClose + `$`)
	// « a attribué la mention « Adore » à « msg » » — alternate French tapback
	// wording (seen from Android in the field, sometimes with a lowercase
	// leading "a"). The mention slot is a named tapback or a raw emoji.
	mentionRe       = regexp.MustCompile(`^[Aa] attribué la mention ` + frOpen + `(.+?)` + frClose + ` à ` + frOpen + `(.+?)` + frClose + `$`)
	mentionRemoveRe = regexp.MustCompile(`^[Aa] retiré la mention ` + frOpen + `(.+?)` + frClose + ` (?:de|à) ` + frOpen + `(.+?)` + frClose + `$`)
)

// French tapback mention names → emoji, straight and curly apostrophes.
var mentionNames = map[string]string{
	"J'aime": "👍", "J’aime": "👍",
	"Adore": "❤️", "J'adore": "❤️", "J’adore": "❤️",
	"N'aime pas": "👎", "N’aime pas": "👎",
	"Je n'aime pas": "👎", "Je n’aime pas": "👎",
	"Rire": "😆", "Haha": "😆",
	"Exclamation": "‼️", "Point d'exclamation": "‼️", "Point d’exclamation": "‼️",
	"Question": "❓", "Point d'interrogation": "❓", "Point d’interrogation": "❓",
}

// mentionEmoji resolves the mention slot: a known French tapback name or a
// raw emoji. Unknown names are rejected so the text bridges as-is.
func mentionEmoji(name string) (string, bool) {
	if e, ok := mentionNames[name]; ok {
		return e, true
	}
	if looksLikeEmoji(name) {
		return name, true
	}
	return "", false
}

// looksLikeEmoji rejects plain words in the emoji slot ("Reacted well to…"):
// every rune must be outside the basic Latin range (emoji, variation
// selectors, ZWJ, skin tones all qualify).
func looksLikeEmoji(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < 0x2000 {
			return false
		}
	}
	return true
}

// stripEllipsis removes a trailing ellipsis and reports whether one was there.
func stripEllipsis(s string) (string, bool) {
	if cut, ok := strings.CutSuffix(s, "…"); ok {
		return strings.TrimRight(cut, " "), true
	}
	if cut, ok := strings.CutSuffix(s, "..."); ok {
		return strings.TrimRight(cut, " "), true
	}
	return s, false
}

// ParseReactionFallback recognizes reaction-fallback texts. Returns ok=false
// for anything that doesn't match a known format exactly.
func ParseReactionFallback(body string) (*ReactionFallback, bool) {
	body = strings.TrimSpace(body)

	for _, p := range emojiReactionPatterns {
		if m := p.re.FindStringSubmatch(body); m != nil && looksLikeEmoji(m[1]) {
			snippet, truncated := stripEllipsis(m[2])
			return &ReactionFallback{Emoji: m[1], Snippet: snippet, Truncated: truncated, Remove: p.remove}, true
		}
	}
	if m := tapbackVerbRe.FindStringSubmatch(body); m != nil {
		snippet, truncated := stripEllipsis(m[2])
		return &ReactionFallback{Emoji: tapbackVerbs[m[1]], Snippet: snippet, Truncated: truncated}, true
	}
	if m := tapbackVerbFrRe.FindStringSubmatch(body); m != nil {
		snippet, truncated := stripEllipsis(m[2])
		return &ReactionFallback{Emoji: tapbackVerbs[m[1]], Snippet: snippet, Truncated: truncated}, true
	}
	if m := tapbackRemoveRe.FindStringSubmatch(body); m != nil {
		snippet, truncated := stripEllipsis(m[2])
		return &ReactionFallback{Emoji: tapbackRemoveNouns[m[1]], Snippet: snippet, Truncated: truncated, Remove: true}, true
	}
	for _, p := range []struct {
		re     *regexp.Regexp
		remove bool
	}{{mentionRe, false}, {mentionRemoveRe, true}} {
		m := p.re.FindStringSubmatch(body)
		if m == nil {
			continue
		}
		if emoji, ok := mentionEmoji(strings.TrimSpace(m[1])); ok {
			snippet, truncated := stripEllipsis(m[2])
			return &ReactionFallback{Emoji: emoji, Snippet: snippet, Truncated: truncated, Remove: p.remove}, true
		}
	}
	return nil, false
}

// normalizeForMatch strips all whitespace and folds curly quotes/apostrophes
// to their straight forms. Reaction quotes get mutated in transit — carriers
// transcode punctuation between GSM-7 and UCS-2, and quoted spaces sometimes
// vanish ("proposer en" arriving as "proposeren") — so target matching must
// ignore both.
func normalizeForMatch(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return -1
		case r == '‘' || r == '’':
			return '\''
		case r == '“' || r == '”':
			return '"'
		}
		return r
	}, s)
}

// MatchesTarget reports whether a candidate message body is the target the
// fallback quotes: exact match, or prefix match when the quote was truncated.
// Whitespace and curly/straight punctuation differences are ignored.
func (f *ReactionFallback) MatchesTarget(body string) bool {
	body = normalizeForMatch(body)
	snippet := normalizeForMatch(f.Snippet)
	if f.Truncated {
		return snippet != "" && strings.HasPrefix(body, snippet)
	}
	return body == snippet
}

// BuildReactionFallback renders an outbound reaction as fallback text using
// a template with {emoji} and {message} placeholders. The message snippet is
// truncated to keep the SMS short.
func BuildReactionFallback(template, emoji, message string) string {
	const maxSnippet = 40
	message = strings.TrimSpace(message)
	if runes := []rune(message); len(runes) > maxSnippet {
		message = strings.TrimRight(string(runes[:maxSnippet]), " ") + "…"
	}
	out := strings.ReplaceAll(template, "{emoji}", emoji)
	return strings.ReplaceAll(out, "{message}", message)
}
