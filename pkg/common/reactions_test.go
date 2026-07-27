package common

import "testing"

func TestParseReactionFallback(t *testing.T) {
	cases := []struct {
		body      string
		emoji     string
		snippet   string
		truncated bool
		remove    bool
	}{
		// The exact format from the field (French Google Messages / iOS).
		{"A réagi 😂 à « Eh beh, le mardi c'est calme ici... »", "😂", "Eh beh, le mardi c'est calme ici", true, false},
		{"A réagi ❤️ à « ok »", "❤️", "ok", false, false},
		{"A retiré la réaction 😂 à « Eh beh, le mardi c'est calme ici... »", "😂", "Eh beh, le mardi c'est calme ici", true, true},
		{`Reacted 😂 to "hello there"`, "😂", "hello there", false, false},
		{"Reacted 👍🏽 to “curly quotes…”", "👍🏽", "curly quotes", true, false},
		{`Removed 😂 from "hello there"`, "😂", "hello there", false, true},
		{`Liked "nice one"`, "👍", "nice one", false, false},
		{`Laughed at "so funny…"`, "😆", "so funny", true, false},
		{`Removed a like from "nice one"`, "👍", "nice one", false, true},
		{`Removed a question mark from "what"`, "❓", "what", false, true},
		{"A adoré « superbe »", "❤️", "superbe", false, false},
		{"N’a pas aimé « bof »", "👎", "bof", false, false},
		// "attribué la mention" wording, exactly as seen in the field
		// (lowercase leading "a", named tapback in the mention slot).
		{"a attribué la mention « Adore » à « Avec enfant aujourd'hui, je quite le travail et la récupère à la guarderie. »", "❤️", "Avec enfant aujourd'hui, je quite le travail et la récupère à la guarderie.", false, false},
		{"A attribué la mention « J’aime » à « ok »", "👍", "ok", false, false},
		{"a attribué la mention « 😂 » à « lol »", "😂", "lol", false, false},
		{"A retiré la mention « Adore » de « ok »", "❤️", "ok", false, true},
	}
	for _, c := range cases {
		fb, ok := ParseReactionFallback(c.body)
		if !ok {
			t.Errorf("ParseReactionFallback(%q) did not match", c.body)
			continue
		}
		if fb.Emoji != c.emoji || fb.Snippet != c.snippet || fb.Truncated != c.truncated || fb.Remove != c.remove {
			t.Errorf("ParseReactionFallback(%q) = %+v, want emoji=%q snippet=%q trunc=%v remove=%v", c.body, fb, c.emoji, c.snippet, c.truncated, c.remove)
		}
	}
}

func TestParseReactionFallbackRejectsPlainText(t *testing.T) {
	for _, body := range []string{
		"hello there",
		`Reacted well to "your plan"`, // word in the emoji slot
		"Liked it a lot",
		"A réagi vite à « x »",
		"Assez calme au travail aussi pour surveiller tes apps? 😉",
		"a attribué la mention « Bravo » à « ok »", // unknown mention name
	} {
		if fb, ok := ParseReactionFallback(body); ok {
			t.Errorf("ParseReactionFallback(%q) matched %+v, want no match", body, fb)
		}
	}
}

func TestReactionFallbackMatchesTarget(t *testing.T) {
	fb := &ReactionFallback{Snippet: "Eh beh, le mardi c'est calme ici", Truncated: true}
	if !fb.MatchesTarget("Eh beh, le mardi c'est calme ici, non ?") {
		t.Error("truncated snippet should prefix-match")
	}
	if fb.MatchesTarget("something else") {
		t.Error("unrelated body should not match")
	}
	exact := &ReactionFallback{Snippet: "ok"}
	if !exact.MatchesTarget("ok") || exact.MatchesTarget("okay") {
		t.Error("exact snippet must match exactly")
	}
	// The quote lost a space in transit ("proposeren" vs "proposer en") —
	// exactly what happened in the field on 2026-07.
	mangled := &ReactionFallback{Snippet: "Peut être bien, j'ai pas trop autre chose à proposeren ce moment 😉"}
	if !mangled.MatchesTarget("Peut être bien, j'ai pas trop autre chose à proposer en ce moment 😉") {
		t.Error("whitespace-mangled quote should still match its target")
	}
	// Curly vs straight apostrophe (GSM-7 transcoding).
	curly := &ReactionFallback{Snippet: "j’arrive bientôt"}
	if !curly.MatchesTarget("j'arrive bientôt") {
		t.Error("curly/straight apostrophe difference should be ignored")
	}
}

func TestBuildReactionFallback(t *testing.T) {
	got := BuildReactionFallback(`Reacted {emoji} to "{message}"`, "😂", "short one")
	if got != `Reacted 😂 to "short one"` {
		t.Errorf("BuildReactionFallback = %q", got)
	}
	long := BuildReactionFallback(`Reacted {emoji} to "{message}"`, "😂", "this is a very long message that should definitely get truncated somewhere")
	if len([]rune(long)) > 70 {
		t.Errorf("long snippet not truncated: %q", long)
	}
}
