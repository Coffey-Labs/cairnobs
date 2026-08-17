// Package lexer tokenizes the pipe-syntax query language. Deliberately
// simple: keywords (where/stats/sort/and/etc.) aren't distinct token
// kinds -- they're just Ident tokens whose value the parser checks
// against a keyword set, so the lexer stays context-free and the parser
// owns all the grammar decisions. See /docs/query-language-design.md.
package lexer

import "fmt"

type Kind int

const (
	EOF Kind = iota
	Illegal
	Ident  // bare words: field names, keywords, unquoted values/free-text terms
	String // quoted string: "..."
	Number // 123, 1.5
	Pipe   // |
	Eq     // =
	Neq    // !=
	Gt     // >
	Gte    // >=
	Lt     // <
	Lte    // <=
	Colon  // :
	Comma  // ,
	LParen // (
	RParen // )
	Minus  // -
	Plus   // +
	Star   // * (only meaningful inside count(*), same as SQL)
)

type Token struct {
	Kind  Kind
	Value string
	Pos   int // byte offset into the original input, for error messages
}

func (t Token) String() string {
	return fmt.Sprintf("%s(%q)@%d", t.Kind, t.Value, t.Pos)
}

func (k Kind) String() string {
	switch k {
	case EOF:
		return "EOF"
	case Illegal:
		return "ILLEGAL"
	case Ident:
		return "IDENT"
	case String:
		return "STRING"
	case Number:
		return "NUMBER"
	case Pipe:
		return "PIPE"
	case Eq:
		return "EQ"
	case Neq:
		return "NEQ"
	case Gt:
		return "GT"
	case Gte:
		return "GTE"
	case Lt:
		return "LT"
	case Lte:
		return "LTE"
	case Colon:
		return "COLON"
	case Comma:
		return "COMMA"
	case LParen:
		return "LPAREN"
	case RParen:
		return "RPAREN"
	case Minus:
		return "MINUS"
	case Plus:
		return "PLUS"
	case Star:
		return "STAR"
	default:
		return "UNKNOWN"
	}
}

type Lexer struct {
	input []rune
	pos   int
}

func New(input string) *Lexer {
	return &Lexer{input: []rune(input)}
}

func (l *Lexer) Next() Token {
	l.skipWhitespace()
	if l.pos >= len(l.input) {
		return Token{Kind: EOF, Pos: l.pos}
	}

	start := l.pos
	c := l.input[l.pos]

	switch {
	case c == '|':
		l.pos++
		return Token{Kind: Pipe, Value: "|", Pos: start}
	case c == '=':
		l.pos++
		return Token{Kind: Eq, Value: "=", Pos: start}
	case c == '!' && l.peek(1) == '=':
		l.pos += 2
		return Token{Kind: Neq, Value: "!=", Pos: start}
	case c == '>' && l.peek(1) == '=':
		l.pos += 2
		return Token{Kind: Gte, Value: ">=", Pos: start}
	case c == '>':
		l.pos++
		return Token{Kind: Gt, Value: ">", Pos: start}
	case c == '<' && l.peek(1) == '=':
		l.pos += 2
		return Token{Kind: Lte, Value: "<=", Pos: start}
	case c == '<':
		l.pos++
		return Token{Kind: Lt, Value: "<", Pos: start}
	case c == ':':
		l.pos++
		return Token{Kind: Colon, Value: ":", Pos: start}
	case c == ',':
		l.pos++
		return Token{Kind: Comma, Value: ",", Pos: start}
	case c == '(':
		l.pos++
		return Token{Kind: LParen, Value: "(", Pos: start}
	case c == ')':
		l.pos++
		return Token{Kind: RParen, Value: ")", Pos: start}
	case c == '-':
		l.pos++
		return Token{Kind: Minus, Value: "-", Pos: start}
	case c == '+':
		l.pos++
		return Token{Kind: Plus, Value: "+", Pos: start}
	case c == '*':
		l.pos++
		return Token{Kind: Star, Value: "*", Pos: start}
	case c == '"':
		return l.lexString()
	case isDigit(c):
		return l.lexNumber()
	case isIdentStart(c):
		return l.lexIdent()
	default:
		l.pos++
		return Token{Kind: Illegal, Value: string(c), Pos: start}
	}
}

func (l *Lexer) peek(offset int) rune {
	p := l.pos + offset
	if p >= len(l.input) {
		return 0
	}
	return l.input[p]
}

func (l *Lexer) skipWhitespace() {
	for l.pos < len(l.input) {
		switch l.input[l.pos] {
		case ' ', '\t', '\n', '\r':
			l.pos++
		default:
			return
		}
	}
}

func (l *Lexer) lexString() Token {
	start := l.pos
	l.pos++ // consume opening quote
	var sb []rune
	for l.pos < len(l.input) {
		c := l.input[l.pos]
		if c == '"' {
			l.pos++
			return Token{Kind: String, Value: string(sb), Pos: start}
		}
		if c == '\\' && l.pos+1 < len(l.input) {
			l.pos++
			sb = append(sb, l.input[l.pos])
			l.pos++
			continue
		}
		sb = append(sb, c)
		l.pos++
	}
	// unterminated string -- return what we have as Illegal so the
	// parser can produce a clear "unterminated string" error rather than
	// the lexer silently accepting it.
	return Token{Kind: Illegal, Value: string(sb), Pos: start}
}

func (l *Lexer) lexNumber() Token {
	start := l.pos
	for l.pos < len(l.input) && isDigit(l.input[l.pos]) {
		l.pos++
	}
	if l.pos < len(l.input) && l.input[l.pos] == '.' && l.pos+1 < len(l.input) && isDigit(l.input[l.pos+1]) {
		l.pos++
		for l.pos < len(l.input) && isDigit(l.input[l.pos]) {
			l.pos++
		}
	}
	return Token{Kind: Number, Value: string(l.input[start:l.pos]), Pos: start}
}

func (l *Lexer) lexIdent() Token {
	start := l.pos
	for l.pos < len(l.input) && isIdentPart(l.input[l.pos]) {
		l.pos++
	}
	return Token{Kind: Ident, Value: string(l.input[start:l.pos]), Pos: start}
}

func isDigit(c rune) bool { return c >= '0' && c <= '9' }

func isIdentStart(c rune) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// Identifiers allow dots (e.g. winevt.event_id, a real attribute key
// shape from Phase 1), digits, and internal hyphens (e.g. host-03,
// api-service -- real, common bare-word values with no need for
// quoting) after the first character. A leading hyphen is deliberately
// NOT part of isIdentStart -- Minus has to stay its own token there so
// `earliest=-1h` and `sort -count`'s leading sign still lex correctly;
// this only affects a hyphen once a token has already started with a
// real identifier character.
func isIdentPart(c rune) bool {
	return isIdentStart(c) || isDigit(c) || c == '.' || c == '_' || c == '-'
}
