package config

import (
	"encoding/json"
	"testing"
)

func TestStripJSONC_LineComment(t *testing.T) {
	in := "{\n  \"a\": 1 // trailing comment\n}\n"
	want := "{\n  \"a\": 1 \n}\n"
	if got := string(stripJSONC([]byte(in))); got != want {
		t.Errorf("stripJSONC(%q) = %q, want %q", in, got, want)
	}
}

func TestStripJSONC_BlockComment_SingleLine(t *testing.T) {
	in := `{"a": /* comment */ 1}`
	want := `{"a":   1}`
	if got := string(stripJSONC([]byte(in))); got != want {
		t.Errorf("stripJSONC(%q) = %q, want %q", in, got, want)
	}
}

func TestStripJSONC_BlockComment_MultiLine(t *testing.T) {
	in := "{\n  /* this is\n     a multi-line\n     comment */\n  \"a\": 1\n}"
	got := string(stripJSONC([]byte(in)))
	var out map[string]any
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatalf("stripped output did not parse as JSON: %v\ngot: %q", err, got)
	}
	if out["a"] != float64(1) {
		t.Errorf("a = %v, want 1", out["a"])
	}
}

func TestStripJSONC_DoesNotStripSlashesInsideStrings(t *testing.T) {
	in := `{"public_url": "https://emby.example.com"}`
	got := string(stripJSONC([]byte(in)))
	if got != in {
		t.Errorf("stripJSONC mutilated a URL string: got %q, want unchanged %q", got, in)
	}
}

func TestStripJSONC_DoesNotStripBlockCommentSyntaxInsideStrings(t *testing.T) {
	in := `{"note": "see /* not a comment */ here"}`
	got := string(stripJSONC([]byte(in)))
	if got != in {
		t.Errorf("stripJSONC mutilated a string containing /* */: got %q, want unchanged %q", got, in)
	}
}

func TestStripJSONC_EscapedQuoteInsideString(t *testing.T) {
	in := `{"note": "a \"quoted\" word // not a comment"}`
	got := string(stripJSONC([]byte(in)))
	if got != in {
		t.Errorf("stripJSONC mishandled an escaped quote: got %q, want unchanged %q", got, in)
	}
}

func TestStripJSONC_EscapedBackslashBeforeQuote(t *testing.T) {
	// The string is: a\  (a, backslash, backslash, closing quote) -- the
	// pair of backslashes is one escaped backslash, so the closing quote
	// really does end the string; a naive escape tracker would think the
	// quote is escaped and run past the end of the string.
	in := `{"note": "a\\", "after": "// still not a comment"}`
	got := string(stripJSONC([]byte(in)))
	if got != in {
		t.Errorf("stripJSONC mishandled an escaped backslash: got %q, want unchanged %q", got, in)
	}
}

func TestStripJSONC_BlockCommentGluingTokensAvoided(t *testing.T) {
	in := `[1/*c*/2]`
	got := string(stripJSONC([]byte(in)))
	if got != `[1 2]` {
		t.Errorf("stripJSONC(%q) = %q, want \"[1 2]\" (space, not glued digits)", in, got)
	}
}

func TestStripJSONC_EmptyFile(t *testing.T) {
	if got := string(stripJSONC([]byte(""))); got != "" {
		t.Errorf("stripJSONC(\"\") = %q, want empty", got)
	}
}

func TestStripJSONC_CommentsOnlyFile(t *testing.T) {
	in := "// just a comment\n/* and another */\n"
	got := stripJSONC([]byte(in))
	var out any
	if err := json.Unmarshal(got, &out); err == nil {
		t.Errorf("expected a comments-only file to still fail JSON parsing after stripping, got no error")
	}
}

func TestStripJSONC_TrailingCommaNotSupported(t *testing.T) {
	// Documents the deliberate non-feature: only comment syntax is
	// stripped, not trailing commas. A future config.jsonc writer must not
	// emit one.
	in := `{"a": 1,}`
	got := stripJSONC([]byte(in))
	var out map[string]any
	if err := json.Unmarshal(got, &out); err == nil {
		t.Errorf("expected a trailing comma to still be rejected by encoding/json after stripping, got no error")
	}
}

func TestStripJSONC_UnterminatedBlockComment_NoPanic(t *testing.T) {
	in := `{"a": /* never closed`
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("stripJSONC panicked on an unterminated block comment: %v", r)
		}
	}()
	stripJSONC([]byte(in))
}

func TestStripJSONC_TrailingSlashAtEOF_NoPanic(t *testing.T) {
	in := `{"a": 1}/`
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("stripJSONC panicked on a trailing lone slash: %v", r)
		}
	}()
	got := string(stripJSONC([]byte(in)))
	if got != in {
		t.Errorf("stripJSONC(%q) = %q, want unchanged (lone trailing slash isn't a comment start)", in, got)
	}
}
