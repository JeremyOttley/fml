// Package lexer implements a zero-allocation byte-slice state machine and
// F# off-side indentation tracker for FML.
//
// Jaime says: Lexing is just feeding bytes to a state machine. Stop allocating
// strings for every identifier. Take a []byte slice and return start/end
// offsets. The parser's job is to prove the grammar and lower it into the AST
// array as fast as possible.
package lexer

import (
	"bytes"

	"fml/internal/token"
)

// Lexer scans a raw []byte source buffer into a flat slice of token.Token.
//
// Jaime says: No regexes, no heap strings, no channels. Just an index walking
// forward through contiguous bytes and writing 16-byte structs into a slice.
type Lexer struct {
	src           []byte
	pos           int
	line          uint16
	col           uint16
	Tokens        []token.Token
	indentStack   [64]uint16
	indentDepth   int
	delimStack    [64]int
	delimDepth    int
	atLineStart   bool
	enableOffside bool
	lastKind      token.TokenKind
}

// New creates a Lexer over src using an optional pre-allocated token buffer.
func New(src []byte, buf []token.Token) *Lexer {
	if buf == nil {
		buf = make([]token.Token, 0, len(src)/4+16)
	} else {
		buf = buf[:0]
	}
	return &Lexer{
		src:           src,
		pos:           0,
		line:          1,
		col:           1,
		Tokens:        buf,
		atLineStart:   false,
		enableOffside: true,
		lastKind:      token.TokenEOF,
	}
}

// Reset re-initializes the Lexer on src, reusing the existing token slice capacity.
func (l *Lexer) Reset(src []byte, enableOffside bool) {
	l.src = src
	l.pos = 0
	l.line = 1
	l.col = 1
	l.Tokens = l.Tokens[:0]
	l.indentDepth = 0
	l.delimDepth = 0
	l.atLineStart = false
	l.enableOffside = enableOffside
	l.lastKind = token.TokenEOF
}

// ScanRaw tokenizes src without synthetic off-side indentation tokens.
func (l *Lexer) ScanRaw() []token.Token {
	l.enableOffside = false
	return l.scanLoop()
}

// ScanAll tokenizes src with F# off-side rule indentation tracking enabled.
func (l *Lexer) ScanAll() []token.Token {
	l.enableOffside = true
	return l.scanLoop()
}

func (l *Lexer) scanLoop() []token.Token {
	n := len(l.src)
	for l.pos < n {
		crossedNewline := l.skipWhitespaceAndComments()
		if l.pos >= n {
			break
		}
		if l.enableOffside && crossedNewline {
			l.handleOffsideNewline()
		}

		startPos := l.pos
		startLine := l.line
		startCol := l.col
		ch := l.src[l.pos]

		// Identifiers and keywords
		if isAlpha(ch) || ch == '_' {
			tok := l.scanIdentOrKeyword(startPos, startLine, startCol)
			l.emitToken(tok)
			continue
		}

		// Numbers (Int or Float)
		if isDigit(ch) {
			tok := l.scanNumber(startPos, startLine, startCol)
			l.emitToken(tok)
			continue
		}

		// String literal
		if ch == '"' {
			tok := l.scanString(startPos, startLine, startCol)
			l.emitToken(tok)
			continue
		}

		// Operators and delimiters
		tok := l.scanOperatorOrDelim(startPos, startLine, startCol)
		l.emitToken(tok)
	}

	if l.enableOffside {
		l.flushOffsideAtEOF()
	}

	l.Tokens = append(l.Tokens, token.Token{
		Kind:  token.TokenEOF,
		Line:  l.line,
		Col:   l.col,
		Start: uint32(l.pos),
		End:   uint32(l.pos),
	})
	return l.Tokens
}

func (l *Lexer) emitToken(tok token.Token) {
	if l.enableOffside {
		l.updateBracketDepth(tok.Kind)
	}
	l.Tokens = append(l.Tokens, tok)
	l.lastKind = tok.Kind
}

func (l *Lexer) skipWhitespaceAndComments() bool {
	crossedNewline := false
	n := len(l.src)
	for l.pos < n {
		ch := l.src[l.pos]
		if ch == ' ' || ch == '\t' || ch == '\r' {
			l.pos++
			if ch == '\t' {
				l.col += 4
			} else if ch == ' ' {
				l.col++
			}
			continue
		}
		if ch == '\n' {
			l.pos++
			l.line++
			l.col = 1
			crossedNewline = true
			continue
		}
		// Line comment //
		if ch == '/' && l.pos+1 < n && l.src[l.pos+1] == '/' {
			l.pos += 2
			l.col += 2
			for l.pos < n && l.src[l.pos] != '\n' {
				l.pos++
				l.col++
			}
			continue
		}
		// Block comment (* ... *)
		if ch == '(' && l.pos+1 < n && l.src[l.pos+1] == '*' {
			l.pos += 2
			l.col += 2
			depth := 1
			for l.pos < n && depth > 0 {
				if l.src[l.pos] == '(' && l.pos+1 < n && l.src[l.pos+1] == '*' {
					depth++
					l.pos += 2
					l.col += 2
					continue
				}
				if l.src[l.pos] == '*' && l.pos+1 < n && l.src[l.pos+1] == ')' {
					depth--
					l.pos += 2
					l.col += 2
					continue
				}
				if l.src[l.pos] == '\n' {
					l.line++
					l.col = 1
					crossedNewline = true
				} else {
					l.col++
				}
				l.pos++
			}
			continue
		}
		break
	}
	return crossedNewline
}

func (l *Lexer) scanIdentOrKeyword(startPos int, startLine, startCol uint16) token.Token {
	n := len(l.src)
	for l.pos < n && (isAlphaNum(l.src[l.pos]) || l.src[l.pos] == '_') {
		l.pos++
		l.col++
	}
	word := l.src[startPos:l.pos]

	// Check for monadic bang suffix (let!, do!, use!, return!)
	if l.pos < n && l.src[l.pos] == '!' {
		if bytes.Equal(word, kwLet) {
			l.pos++
			l.col++
			return makeTok(token.TokenLetBang, startLine, startCol, startPos, l.pos)
		}
		if bytes.Equal(word, kwDo) {
			l.pos++
			l.col++
			return makeTok(token.TokenDoBang, startLine, startCol, startPos, l.pos)
		}
		if bytes.Equal(word, kwUse) {
			l.pos++
			l.col++
			return makeTok(token.TokenUseBang, startLine, startCol, startPos, l.pos)
		}
		if bytes.Equal(word, kwReturn) {
			l.pos++
			l.col++
			return makeTok(token.TokenReturnBang, startLine, startCol, startPos, l.pos)
		}
	}

	kind := classifyKeyword(word)
	return makeTok(kind, startLine, startCol, startPos, l.pos)
}

func (l *Lexer) scanNumber(startPos int, startLine, startCol uint16) token.Token {
	n := len(l.src)
	kind := token.TokenInt
	for l.pos < n && isDigit(l.src[l.pos]) {
		l.pos++
		l.col++
	}
	// Check for fractional part (ensure it is not '..' range operator)
	if l.pos+1 < n && l.src[l.pos] == '.' && isDigit(l.src[l.pos+1]) {
		kind = token.TokenFloat
		l.pos++
		l.col++
		for l.pos < n && isDigit(l.src[l.pos]) {
			l.pos++
			l.col++
		}
		if l.pos < n && (l.src[l.pos] == 'e' || l.src[l.pos] == 'E') {
			l.pos++
			l.col++
			if l.pos < n && (l.src[l.pos] == '+' || l.src[l.pos] == '-') {
				l.pos++
				l.col++
			}
			for l.pos < n && isDigit(l.src[l.pos]) {
				l.pos++
				l.col++
			}
		}
	}
	return makeTok(kind, startLine, startCol, startPos, l.pos)
}

func (l *Lexer) scanString(startPos int, startLine, startCol uint16) token.Token {
	n := len(l.src)
	l.pos++ // skip opening '"'
	l.col++
	for l.pos < n {
		ch := l.src[l.pos]
		if ch == '\\' && l.pos+1 < n {
			l.pos += 2
			l.col += 2
			continue
		}
		if ch == '"' {
			l.pos++
			l.col++
			break
		}
		if ch == '\n' {
			l.line++
			l.col = 1
		} else {
			l.col++
		}
		l.pos++
	}
	return makeTok(token.TokenString, startLine, startCol, startPos, l.pos)
}

func (l *Lexer) scanOperatorOrDelim(startPos int, startLine, startCol uint16) token.Token {
	n := len(l.src)
	ch := l.src[l.pos]
	var next byte
	if l.pos+1 < n {
		next = l.src[l.pos+1]
	}

	advance := func(count int, kind token.TokenKind) token.Token {
		l.pos += count
		l.col += uint16(count)
		return makeTok(kind, startLine, startCol, startPos, l.pos)
	}

	switch ch {
	case '(':
		if next == ')' {
			return advance(2, token.TokenUnit)
		}
		if next == '|' {
			return advance(2, token.TokenLActive)
		}
		return advance(1, token.TokenLParen)
	case ')':
		return advance(1, token.TokenRParen)
	case '[':
		if next == '|' {
			return advance(2, token.TokenLArray)
		}
		if next == '<' {
			return advance(2, token.TokenLAttr)
		}
		return advance(1, token.TokenLBrack)
	case ']':
		return advance(1, token.TokenRBrack)
	case '{':
		return advance(1, token.TokenLBrace)
	case '}':
		return advance(1, token.TokenRBrace)
	case '|':
		if next == '>' {
			return advance(2, token.TokenPipeRight)
		}
		if next == ']' {
			return advance(2, token.TokenRArray)
		}
		if next == ')' {
			return advance(2, token.TokenRActive)
		}
		if next == '|' {
			return advance(2, token.TokenOrOr)
		}
		return advance(1, token.TokenPipe)
	case '<':
		if next == '-' {
			return advance(2, token.TokenMutate)
		}
		if next == '|' {
			return advance(2, token.TokenPipeLeft)
		}
		if next == '<' {
			return advance(2, token.TokenComposeLeft)
		}
		if next == '@' {
			return advance(2, token.TokenLQuote)
		}
		if next == '=' {
			return advance(2, token.TokenLe)
		}
		if next == '>' {
			return advance(2, token.TokenNeq)
		}
		return advance(1, token.TokenLt)
	case '>':
		if next == '>' {
			return advance(2, token.TokenComposeRight)
		}
		if next == ']' {
			return advance(2, token.TokenRAttr)
		}
		if next == '=' {
			return advance(2, token.TokenGe)
		}
		return advance(1, token.TokenGt)
	case '-':
		if next == '>' {
			return advance(2, token.TokenArrow)
		}
		return advance(1, token.TokenMinus)
	case '=':
		if next == '=' {
			return advance(2, token.TokenEq)
		}
		return advance(1, token.TokenAssign)
	case '!':
		if next == '=' {
			return advance(2, token.TokenNeq)
		}
		return advance(1, token.TokenNot)
	case ':':
		if next == ':' {
			return advance(2, token.TokenCons)
		}
		if next == '?' {
			return advance(2, token.TokenColonQMark)
		}
		return advance(1, token.TokenColon)
	case '.':
		if next == '.' {
			return advance(2, token.TokenRange)
		}
		return advance(1, token.TokenDot)
	case '@':
		if next == '>' {
			return advance(2, token.TokenRQuote)
		}
		return advance(1, token.TokenAppend)
	case '&':
		if next == '&' {
			return advance(2, token.TokenAndAnd)
		}
		return advance(1, token.TokenAmp)
	case '+':
		return advance(1, token.TokenPlus)
	case '*':
		return advance(1, token.TokenStar)
	case '/':
		return advance(1, token.TokenSlash)
	case '%':
		return advance(1, token.TokenPercent)
	case '^':
		return advance(1, token.TokenCaret)
	case '?':
		return advance(1, token.TokenQMark)
	case ',':
		return advance(1, token.TokenComma)
	case ';':
		return advance(1, token.TokenSemicolon)
	case '\'':
		return advance(1, token.TokenApostrophe)
	default:
		return advance(1, token.TokenIllegal)
	}
}

func makeTok(kind token.TokenKind, line, col uint16, start, end int) token.Token {
	return token.Token{
		Kind:  kind,
		Line:  line,
		Col:   col,
		Start: uint32(start),
		End:   uint32(end),
	}
}

func isAlpha(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z')
}

func isDigit(ch byte) bool {
	return ch >= '0' && ch <= '9'
}

func isAlphaNum(ch byte) bool {
	return isAlpha(ch) || isDigit(ch)
}

var (
	kwUnderscore = []byte("_")
	kwLet        = []byte("let")
	kwMutable    = []byte("mutable")
	kwRec        = []byte("rec")
	kwUse        = []byte("use")
	kwFun        = []byte("fun")
	kwIn         = []byte("in")
	kwAnd        = []byte("and")
	kwReturn     = []byte("return")
	kwYield      = []byte("yield")
	kwSeq        = []byte("seq")
	kwAsync      = []byte("async")
	kwTask       = []byte("task")
	kwType       = []byte("type")
	kwOf         = []byte("of")
	kwSome       = []byte("Some")
	kwNone       = []byte("None")
	kwOk         = []byte("Ok")
	kwError      = []byte("Error")
	kwMeasure    = []byte("Measure")
	kwMatch      = []byte("match")
	kwWith       = []byte("with")
	kwWhen       = []byte("when")
	kwFunction   = []byte("function")
	kwIf         = []byte("if")
	kwThen       = []byte("then")
	kwElse       = []byte("else")
	kwElif       = []byte("elif")
	kwFor        = []byte("for")
	kwTo         = []byte("to")
	kwDownto     = []byte("downto")
	kwDo         = []byte("do")
	kwWhile      = []byte("while")
	kwTry        = []byte("try")
	kwFinally    = []byte("finally")
	kwRaise      = []byte("raise")
	kwException  = []byte("exception")
	kwModule     = []byte("module")
	kwNamespace  = []byte("namespace")
	kwOpen       = []byte("open")
	kwFunctor    = []byte("functor")
	kwStruct     = []byte("struct")
	kwSig        = []byte("sig")
	kwEnd        = []byte("end")
	kwMember     = []byte("member")
	kwAbstract   = []byte("abstract")
	kwNew        = []byte("new")
	kwInterface  = []byte("interface")
	kwInherit    = []byte("inherit")
	kwStatic     = []byte("static")
	kwVal        = []byte("val")
	kwTrue       = []byte("true")
	kwFalse      = []byte("false")
	kwNot        = []byte("not")
)

func classifyKeyword(w []byte) token.TokenKind {
	switch len(w) {
	case 1:
		if w[0] == '_' {
			return token.TokenUnderscore
		}
	case 2:
		switch {
		case bytes.Equal(w, kwIn):
			return token.TokenIn
		case bytes.Equal(w, kwOf):
			return token.TokenOf
		case bytes.Equal(w, kwIf):
			return token.TokenIf
		case bytes.Equal(w, kwTo):
			return token.TokenTo
		case bytes.Equal(w, kwDo):
			return token.TokenDo
		case bytes.Equal(w, kwOk):
			return token.TokenOk
		}
	case 3:
		switch {
		case bytes.Equal(w, kwLet):
			return token.TokenLet
		case bytes.Equal(w, kwRec):
			return token.TokenRec
		case bytes.Equal(w, kwUse):
			return token.TokenUse
		case bytes.Equal(w, kwFun):
			return token.TokenFun
		case bytes.Equal(w, kwAnd):
			return token.TokenAnd
		case bytes.Equal(w, kwSeq):
			return token.TokenSeq
		case bytes.Equal(w, kwFor):
			return token.TokenFor
		case bytes.Equal(w, kwTry):
			return token.TokenTry
		case bytes.Equal(w, kwSig):
			return token.TokenSig
		case bytes.Equal(w, kwEnd):
			return token.TokenEnd
		case bytes.Equal(w, kwNew):
			return token.TokenNew
		case bytes.Equal(w, kwVal):
			return token.TokenVal
		case bytes.Equal(w, kwNot):
			return token.TokenNot
		}
	case 4:
		switch {
		case bytes.Equal(w, kwTask):
			return token.TokenTask
		case bytes.Equal(w, kwType):
			return token.TokenType
		case bytes.Equal(w, kwSome):
			return token.TokenSome
		case bytes.Equal(w, kwNone):
			return token.TokenNone
		case bytes.Equal(w, kwWith):
			return token.TokenWith
		case bytes.Equal(w, kwWhen):
			return token.TokenWhen
		case bytes.Equal(w, kwThen):
			return token.TokenThen
		case bytes.Equal(w, kwElse):
			return token.TokenElse
		case bytes.Equal(w, kwElif):
			return token.TokenElif
		case bytes.Equal(w, kwOpen):
			return token.TokenOpen
		case bytes.Equal(w, kwTrue):
			return token.TokenTrue
		}
	case 5:
		switch {
		case bytes.Equal(w, kwYield):
			return token.TokenYield
		case bytes.Equal(w, kwAsync):
			return token.TokenAsync
		case bytes.Equal(w, kwError):
			return token.TokenError
		case bytes.Equal(w, kwMatch):
			return token.TokenMatch
		case bytes.Equal(w, kwWhile):
			return token.TokenWhile
		case bytes.Equal(w, kwRaise):
			return token.TokenRaise
		case bytes.Equal(w, kwFalse):
			return token.TokenFalse
		}
	case 6:
		switch {
		case bytes.Equal(w, kwReturn):
			return token.TokenReturn
		case bytes.Equal(w, kwDownto):
			return token.TokenDownto
		case bytes.Equal(w, kwModule):
			return token.TokenModule
		case bytes.Equal(w, kwStruct):
			return token.TokenStruct
		case bytes.Equal(w, kwMember):
			return token.TokenMember
		case bytes.Equal(w, kwStatic):
			return token.TokenStatic
		}
	case 7:
		switch {
		case bytes.Equal(w, kwMutable):
			return token.TokenMutable
		case bytes.Equal(w, kwMeasure):
			return token.TokenMeasure
		case bytes.Equal(w, kwFinally):
			return token.TokenFinally
		case bytes.Equal(w, kwFunctor):
			return token.TokenFunctor
		case bytes.Equal(w, kwInherit):
			return token.TokenInherit
		}
	case 8:
		switch {
		case bytes.Equal(w, kwFunction):
			return token.TokenFunction
		case bytes.Equal(w, kwAbstract):
			return token.TokenAbstract
		}
	case 9:
		switch {
		case bytes.Equal(w, kwException):
			return token.TokenException
		case bytes.Equal(w, kwNamespace):
			return token.TokenNamespace
		case bytes.Equal(w, kwInterface):
			return token.TokenInterface
		}
	}
	return token.TokenIdent
}
