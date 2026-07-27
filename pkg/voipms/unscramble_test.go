package voipms

import (
	"strings"
	"testing"
)

// The real field case from 2026-07-26 (VoIP.ms rows 108872494 / 108875072 /
// 108877206 / 108878566): a 144-unit UCS-2 message split into 67+67+10
// segments, delivered in three different wrong orders and once correctly.
const leoFull = "Vous venez de mettre votre trajet Leo en pause. Si vous souhaitez terminer votre trajet, ouvrez l’app et appuyez sur “Terminer le trajet”. - Leo"

func leoSegments(t *testing.T) (a, b, c string) {
	t.Helper()
	runes := []rune(leoFull)
	if len(runes) != 144 {
		t.Fatalf("leoFull is %d runes, want 144", len(runes))
	}
	return string(runes[:67]), string(runes[67:134]), string(runes[134:])
}

func TestRepairScrambledBodyFieldCases(t *testing.T) {
	a, b, c := leoSegments(t)
	// Sanity: the segment cuts match what the scrambled rows showed.
	if !strings.HasSuffix(a, "souhaitez t") || !strings.HasSuffix(b, "le traj") || c != "et”. - Leo" {
		t.Fatalf("unexpected segment cuts: a=%q b=%q c=%q", a, b, c)
	}
	for _, scrambled := range []string{c + b + a, a + c + b, c + a + b} {
		got, ok := RepairScrambledBody(scrambled)
		if !ok || got != leoFull {
			t.Errorf("RepairScrambledBody(%q…) = (%q…, %v), want repaired", scrambled[:30], got[:30], ok)
		}
	}
}

func TestRepairScrambledBodyLeavesCorrectMessagesAlone(t *testing.T) {
	a, b, c := leoSegments(t)
	for _, body := range []string{
		leoFull,       // correct order: paired punctuation is fine as-is
		a + b + c,     // same, built from segments
		"short one 😉", // single segment
		"ok je passe. " + strings.Repeat("Merci pour hier, c’était vraiment une très belle soirée. ", 2), // lowercase start alone is not evidence
		strings.Repeat("Une phrase parfaitement banale sans ponctuation appariée qui dure. ", 3),         // long but structurally clean
	} {
		if got, ok := RepairScrambledBody(body); ok {
			t.Errorf("RepairScrambledBody(%q…) rewrote a clean body to %q…", body[:20], got[:20])
		}
	}
}

func TestRepairScrambledBodyASCII(t *testing.T) {
	// GSM-7 model: 153-char segments, ASCII only. The parenthesis pair spans
	// the segment boundary, so the swapped order shows a closer-first
	// violation.
	full := strings.Repeat("Lorem ipsum dolor sit amet, ", 5) + "(details follow soon) and more text here."
	runes := []rune(full)
	if len(runes) <= 160 {
		t.Fatalf("test message too short: %d", len(runes))
	}
	a, b := string(runes[:153]), string(runes[153:])
	got, ok := RepairScrambledBody(b + a)
	if !ok || got != full {
		t.Errorf("RepairScrambledBody(ascii swap) = (%q…, %v), want repaired", got[:30], ok)
	}
	if _, ok := RepairScrambledBody(full); ok {
		t.Error("RepairScrambledBody rewrote a clean ASCII body")
	}
}

func TestLooksScrambled(t *testing.T) {
	for body, want := range map[string]bool{
		"et”. - Leo suite du texte “ouvert":                         true, // closer before opener
		"début “jamais fermé et la suite":                           true, // opener never closed
		"”départ sur un fermant":                                    true, // starts with closing punct
		"Texte normal avec “guillemets” et (parenthèses) corrects.": false,
		"message tout simple":                                       false,
	} {
		if got := looksScrambled(body); got != want {
			t.Errorf("looksScrambled(%q) = %v, want %v", body, got, want)
		}
	}
}
