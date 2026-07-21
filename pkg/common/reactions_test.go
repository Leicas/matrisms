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
