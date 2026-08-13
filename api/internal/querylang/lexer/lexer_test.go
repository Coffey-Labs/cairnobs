package lexer

import "testing"

func collectKinds(input string) []Kind {
	l := New(input)
	var kinds []Kind
	for {
		tok := l.Next()
		kinds = append(kinds, tok.Kind)
		if tok.Kind == EOF {
			return kinds
		}
	}
}

func TestLexSimpleFilter(t *testing.T) {
	got := collectKinds(`service=api`)
	want := []Kind{Ident, Eq, Ident, EOF}
	assertKinds(t, got, want)
}

func TestLexPipeline(t *testing.T) {
	got := collectKinds(`service=api | where status>=500 | stats count(*) by host`)
	want := []Kind{
		Ident, Eq, Ident, Pipe,
		Ident, Ident, Gte, Number, Pipe,
		Ident, Ident, LParen, Star, RParen, Ident, Ident,
		EOF,
	}
	assertKinds(t, got, want)
}

func TestLexOperators(t *testing.T) {
	got := collectKinds(`= != > >= < <=`)
	want := []Kind{Eq, Neq, Gt, Gte, Lt, Lte, EOF}
	assertKinds(t, got, want)
}

func TestLexQuotedString(t *testing.T) {
	l := New(`"connection refused"`)
	tok := l.Next()
	if tok.Kind != String {
		t.Fatalf("Kind = %v, want String", tok.Kind)
	}
	if tok.Value != "connection refused" {
		t.Fatalf("Value = %q, want %q", tok.Value, "connection refused")
	}
}

func TestLexQuotedStringWithEscapes(t *testing.T) {
	l := New(`"has \"quotes\" and \\backslash"`)
	tok := l.Next()
	if tok.Kind != String {
		t.Fatalf("Kind = %v, want String", tok.Kind)
	}
	want := `has "quotes" and \backslash`
	if tok.Value != want {
		t.Fatalf("Value = %q, want %q", tok.Value, want)
	}
}

func TestLexUnterminatedStringIsIllegal(t *testing.T) {
	l := New(`"unterminated`)
	tok := l.Next()
	if tok.Kind != Illegal {
		t.Fatalf("Kind = %v, want Illegal", tok.Kind)
	}
}

func TestLexFieldWithDots(t *testing.T) {
	l := New(`winevt.event_id=4625`)
	tok := l.Next()
	if tok.Kind != Ident || tok.Value != "winevt.event_id" {
		t.Fatalf("got %v, want Ident(winevt.event_id)", tok)
	}
}

func TestLexNumber(t *testing.T) {
	cases := []string{"123", "1.5", "0"}
	for _, c := range cases {
		l := New(c)
		tok := l.Next()
		if tok.Kind != Number || tok.Value != c {
			t.Errorf("lexing %q: got %v, want Number(%s)", c, tok, c)
		}
	}
}

func TestLexNegativeTimeExpr(t *testing.T) {
	// "-1h" lexes as MINUS, NUMBER, IDENT -- the parser composes these,
	// not the lexer (see package doc comment).
	got := collectKinds(`-1h`)
	want := []Kind{Minus, Number, Ident, EOF}
	assertKinds(t, got, want)
}

func TestLexWhitespaceIsSkipped(t *testing.T) {
	got := collectKinds("  service  =  api  ")
	want := []Kind{Ident, Eq, Ident, EOF}
	assertKinds(t, got, want)
}

func TestLexEmptyInput(t *testing.T) {
	got := collectKinds("")
	want := []Kind{EOF}
	assertKinds(t, got, want)
}

func TestLexIllegalCharacter(t *testing.T) {
	l := New(`$`)
	tok := l.Next()
	if tok.Kind != Illegal {
		t.Fatalf("Kind = %v, want Illegal", tok.Kind)
	}
}

func TestTokenPositionsAreByteOffsets(t *testing.T) {
	l := New(`service=api`)
	first := l.Next()
	second := l.Next()
	if first.Pos != 0 {
		t.Errorf("first.Pos = %d, want 0", first.Pos)
	}
	if second.Pos != 7 {
		t.Errorf("second.Pos = %d, want 7", second.Pos)
	}
}

func assertKinds(t *testing.T, got, want []Kind) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d tokens %v, want %d tokens %v", len(got), got, len(want), want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("token %d: got %v, want %v (full: got=%v want=%v)", i, got[i], want[i], got, want)
		}
	}
}
