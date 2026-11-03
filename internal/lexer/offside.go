package lexer

import "fml/internal/token"

// handleOffsideNewline processes column alignment after a newline using a fixed
// 64-entry uint16 indentation stack.
//
// Jaime says: Whitespace sensitivity doesn't require an allocation-heavy
// post-processing pass. Track column numbers on a fixed [64]uint16 stack right
// inside the lexer. If someone indents past 64 levels, their code has bigger
// problems than the compiler.
func (l *Lexer) handleOffsideNewline() {
	col := l.col
	prev := l.lastKind
	if prev == token.TokenEOF {
		return
	}

	nextCh := byte(0)
	var next2 byte
	if l.pos < len(l.src) {
		nextCh = l.src[l.pos]
	}
	if l.pos+1 < len(l.src) {
		next2 = l.src[l.pos+1]
	}

	// If previous token opens an indented block and new column is deeper than current block
	currentIndent := uint16(1)
	if l.indentDepth > 0 {
		currentIndent = l.indentStack[l.indentDepth-1]
	}

	if isBlockOpener(prev) && col > currentIndent {
		if l.indentDepth < len(l.indentStack) {
			l.indentStack[l.indentDepth] = col
			l.indentDepth++
			l.Tokens = append(l.Tokens, token.Token{
				Kind:  token.TokenIndent,
				Line:  l.line,
				Col:   col,
				Start: uint32(l.pos),
				End:   uint32(l.pos),
			})
		}
		return
	}

	// Pop dedents when indentation retreats to an outer column
	for l.indentDepth > 0 && col < l.indentStack[l.indentDepth-1] {
		l.indentDepth--
		l.Tokens = append(l.Tokens, token.Token{
			Kind:  token.TokenDedent,
			Line:  l.line,
			Col:   col,
			Start: uint32(l.pos),
			End:   uint32(l.pos),
		})
	}

	// Emit synthetic TokenSemi when aligned at the active block column
	// (or top-level column 1) and neither previous nor next token is a continuation
	activeCol := uint16(1)
	if l.indentDepth > 0 {
		activeCol = l.indentStack[l.indentDepth-1]
	}
	if col == activeCol && !isContinuationPrev(prev) && !l.startsWithContinuation(nextCh, next2) {
		l.Tokens = append(l.Tokens, token.Token{
			Kind:  token.TokenSemi,
			Line:  l.line,
			Col:   col,
			Start: uint32(l.pos),
			End:   uint32(l.pos),
		})
	}
}

func (l *Lexer) flushOffsideAtEOF() {
	for l.indentDepth > 0 {
		l.indentDepth--
		l.Tokens = append(l.Tokens, token.Token{
			Kind:  token.TokenDedent,
			Line:  l.line,
			Col:   l.col,
			Start: uint32(l.pos),
			End:   uint32(l.pos),
		})
	}
}

func (l *Lexer) updateBracketDepth(k token.TokenKind) {
	switch k {
	case token.TokenLParen, token.TokenLBrack, token.TokenLArray, token.TokenLBrace, token.TokenLAttr, token.TokenLQuote, token.TokenLActive:
		if l.delimDepth < len(l.delimStack) {
			l.delimStack[l.delimDepth] = l.indentDepth
			l.delimDepth++
		}
	case token.TokenRParen, token.TokenRBrack, token.TokenRArray, token.TokenRBrace, token.TokenRAttr, token.TokenRQuote, token.TokenRActive:
		if l.delimDepth > 0 {
			l.delimDepth--
			targetDepth := l.delimStack[l.delimDepth]
			for l.indentDepth > targetDepth {
				l.indentDepth--
				l.Tokens = append(l.Tokens, token.Token{
					Kind:  token.TokenDedent,
					Line:  l.line,
					Col:   l.col,
					Start: uint32(l.pos),
					End:   uint32(l.pos),
				})
			}
		}
	}
}

func isBlockOpener(k token.TokenKind) bool {
	switch k {
	case token.TokenAssign, token.TokenArrow, token.TokenDo, token.TokenThen,
		token.TokenElse, token.TokenWith, token.TokenTry, token.TokenFinally,
		token.TokenLBrace, token.TokenModule, token.TokenStruct, token.TokenSig:
		return true
	default:
		return false
	}
}

func isContinuationPrev(k token.TokenKind) bool {
	switch k {
	case token.TokenAssign, token.TokenArrow, token.TokenPipeRight, token.TokenPipeLeft,
		token.TokenComposeRight, token.TokenComposeLeft, token.TokenPlus, token.TokenMinus,
		token.TokenStar, token.TokenSlash, token.TokenAndAnd, token.TokenOrOr,
		token.TokenComma, token.TokenSemicolon, token.TokenSemi, token.TokenIndent,
		token.TokenIn, token.TokenThen, token.TokenElse, token.TokenWith,
		token.TokenDo, token.TokenLet, token.TokenMatch, token.TokenIf, token.TokenLBrace:
		return true
	default:
		return false
	}
}

func (l *Lexer) startsWithContinuation(ch, next byte) bool {
	// Pipe '|', '|>', '<|', '>>', '<<', '}', ')', ']'
	if ch == '|' || ch == '}' || ch == ')' || ch == ']' {
		return true
	}
	if ch == '<' && (next == '|' || next == '<') {
		return true
	}
	if ch == '>' && next == '>' {
		return true
	}
	if ch == '-' && next == '>' {
		return true
	}
	// Check leading continuation keywords: in, then, else, elif, with, finally, end, and, do, to
	if isAlpha(ch) {
		end := l.pos
		for end < len(l.src) && (isAlphaNum(l.src[end]) || l.src[end] == '_') {
			end++
		}
		kw := classifyKeyword(l.src[l.pos:end])
		switch kw {
		case token.TokenIn, token.TokenThen, token.TokenElse, token.TokenElif,
			token.TokenWith, token.TokenFinally, token.TokenEnd, token.TokenAnd,
			token.TokenDo, token.TokenTo, token.TokenDownto:
			return true
		}
	}
	return false
}
