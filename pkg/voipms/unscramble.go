package voipms

import (
	"strings"
	"unicode"
	"unicode/utf16"
)

// VoIP.ms reassembles inbound multipart SMS server-side in segment-ARRIVAL
// order, not UDH sequence order, so a long message can arrive as a single
// getSMS row whose fixed-size segments are concatenated in the wrong order
// (verified in the field 2026-07-26, see CLAUDE.md). The true layout is
// known — N-1 full segments plus one short trailing segment — so the row is
// some permutation of blocks whose sizes we can enumerate. RepairScrambledBody
// tries every (short-block position × block order) hypothesis, scores each
// reassembly on structural plausibility, and rewrites the body only when the
// original shows hard evidence of scrambling AND exactly one candidate is
// decisively better.

const (
	// ucs2SegmentUnits is the payload of one segment of a concatenated
	// UCS-2 SMS, in UTF-16 code units.
	ucs2SegmentUnits = 67
	// gsm7SegmentChars approximates the payload of one segment of a
	// concatenated GSM-7 SMS (ignores 2-septet extended chars, so it is only
	// applied to plain-ASCII bodies).
	gsm7SegmentChars = 153
	// maxSegments bounds the permutation search ((N-1)! per layout).
	maxSegments = 5
	// repairMargin is how decisively the winning reassembly must beat the
	// original body's score before we dare rewrite message content.
	repairMargin = 4
)

// RepairScrambledBody attempts to restore the original segment order of a
// scrambled multipart SMS body. Returns (repaired, true) only on a confident
// repair; (body, false) otherwise.
func RepairScrambledBody(body string) (string, bool) {
	units := utf16.Encode([]rune(body))
	blockSize := ucs2SegmentUnits
	if isASCII(body) {
		blockSize = gsm7SegmentChars
	}

	n := (len(units) + blockSize - 1) / blockSize
	if n < 2 || n > maxSegments {
		return body, false
	}
	short := len(units) - blockSize*(n-1)
	if short <= 0 || short >= blockSize {
		// Exact multiple of the block size: the short block can't be located.
		return body, false
	}

	// Rewriting message content on a hunch is worse than the scramble: only
	// proceed when the body as-is has hard structural violations.
	if !looksScrambled(body) {
		return body, false
	}

	identity := scoreAssembly([]string{body})
	best, bestScore, ties := "", identity+repairMargin-1, 0
	for shortPos := 0; shortPos < n; shortPos++ {
		blocks, ok := splitBlocks(units, blockSize, shortPos, short)
		if !ok {
			continue
		}
		full := append([]string{}, blocks[:shortPos]...)
		full = append(full, blocks[shortPos+1:]...)
		// The short block is the true final segment; permute the full ones.
		permute(full, func(order []string) {
			parts := append(append([]string{}, order...), blocks[shortPos])
			cand := strings.Join(parts, "")
			if cand == body {
				return
			}
			switch score := scoreAssembly(parts); {
			case score > bestScore:
				best, bestScore, ties = cand, score, 1
			case score == bestScore && cand != best:
				ties++
			}
		})
	}
	if best == "" || ties != 1 {
		return body, false
	}
	return best, true
}

// splitBlocks cuts units into n blocks of blockSize with the short block at
// shortPos. Fails if a cut would split a UTF-16 surrogate pair (a real
// segment boundary never does).
func splitBlocks(units []uint16, blockSize, shortPos, short int) ([]string, bool) {
	var blocks []string
	for at, i := 0, 0; at < len(units); i++ {
		size := blockSize
		if i == shortPos {
			size = short
		}
		end := at + size
		if end < len(units) && units[end] >= 0xDC00 && units[end] < 0xE000 {
			return nil, false
		}
		blocks = append(blocks, string(utf16.Decode(units[at:end])))
		at = end
	}
	return blocks, true
}

// permute calls fn with every ordering of items (Heap's algorithm).
func permute(items []string, fn func([]string)) {
	var rec func(k int)
	rec = func(k int) {
		if k <= 1 {
			fn(items)
			return
		}
		for i := 0; i < k; i++ {
			rec(k - 1)
			if k%2 == 0 {
				items[i], items[k-1] = items[k-1], items[i]
			} else {
				items[0], items[k-1] = items[k-1], items[0]
			}
		}
	}
	rec(len(items))
}

var pairedPunct = [][2]rune{{'«', '»'}, {'“', '”'}, {'(', ')'}}

const closingPunct = "»”’)!?.,;:"

// looksScrambled reports hard structural evidence that a body is out of
// order: paired punctuation closing before it opens (or never closing), or a
// message starting with closing punctuation. A merely-lowercase first letter
// is NOT enough — plenty of real texts start lowercase.
func looksScrambled(body string) bool {
	runes := []rune(body)
	if len(runes) == 0 {
		return false
	}
	if strings.ContainsRune(closingPunct, runes[0]) {
		return true
	}
	for _, p := range pairedPunct {
		depth := 0
		for _, r := range runes {
			switch r {
			case p[0]:
				depth++
			case p[1]:
				if depth == 0 {
					return true // closer before its opener
				}
				depth--
			}
		}
		if depth > 0 {
			return true // opener never closed
		}
	}
	return false
}

// scoreAssembly rates the plausibility of a candidate reassembly: how the
// message starts, whether paired punctuation opens before it closes, and
// whether block seams glue a letter straight onto an uppercase letter (the
// signature of a message end jammed against a message start).
func scoreAssembly(parts []string) int {
	full := strings.Join(parts, "")
	runes := []rune(full)
	if len(runes) == 0 {
		return 0
	}
	score := 0

	switch first := runes[0]; {
	case unicode.IsUpper(first) || unicode.IsDigit(first):
		score += 2
	case unicode.IsLower(first):
		score -= 2
	case strings.ContainsRune(closingPunct, first):
		score -= 3
	}

	for _, p := range pairedPunct {
		depth, present := 0, false
		for _, r := range runes {
			switch r {
			case p[0]:
				depth++
				present = true
			case p[1]:
				present = true
				if depth == 0 {
					score -= 2
				} else {
					depth--
				}
			}
		}
		score -= depth
		if present && depth == 0 {
			score++
		}
	}

	at := 0
	for _, part := range parts[:len(parts)-1] {
		at += len([]rune(part))
		left, right := runes[at-1], runes[at]
		if unicode.IsLetter(left) && unicode.IsUpper(right) {
			score -= 2
		}
	}
	return score
}

func isASCII(s string) bool {
	for _, r := range s {
		if r > 127 {
			return false
		}
	}
	return true
}
