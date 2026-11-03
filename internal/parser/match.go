package parser

import (
	"math"

	"fml/internal/ast"
	"fml/internal/token"
)

// parseMatch parses `match expr with | pat [when guard] -> body`.
func (p *Parser) parseMatch() ast.NodeID {
	tokIdx := uint32(p.pos)
	p.expect(token.TokenMatch)
	scrutinee := p.parseExpr(0)
	p.expect(token.TokenWith)
	armsBlock := p.parseMatchArms()
	return p.Arena.Add(ast.Node{
		Kind:     ast.KindMatch,
		TokenIdx: tokIdx,
		Left:     uint32(scrutinee),
		Right:    uint32(armsBlock),
	})
}

// parseFunctionMatchLambda parses `function | pat -> body` as a lambda over a match.
func (p *Parser) parseFunctionMatchLambda() ast.NodeID {
	tokIdx := uint32(p.pos)
	p.expect(token.TokenFunction)
	argID := p.Arena.Strings.InternString("__match_arg")
	paramNode := p.Arena.Add(ast.Node{
		Kind: ast.KindIdent,
		Data: uint64(argID),
	})
	scrutinee := p.Arena.Add(ast.Node{
		Kind: ast.KindIdent,
		Data: uint64(argID),
	})
	armsBlock := p.parseMatchArms()
	matchNode := p.Arena.Add(ast.Node{
		Kind:     ast.KindMatch,
		TokenIdx: tokIdx,
		Left:     uint32(scrutinee),
		Right:    uint32(armsBlock),
	})
	return p.Arena.Add(ast.Node{
		Kind:     ast.KindLambda,
		TokenIdx: tokIdx,
		Left:     uint32(paramNode),
		Right:    uint32(matchNode),
		Data:     uint64(argID),
	})
}

func (p *Parser) parseMatchArms() ast.NodeID {
	indented := p.match(token.TokenIndent)
	p.skipSeparators()
	var arms []ast.NodeID

	for !p.isEOF() {
		p.skipSeparators()
		if indented && p.peek().Kind == token.TokenDedent {
			break
		}
		if p.peek().Kind != token.TokenPipe && len(arms) > 0 {
			break
		}
		p.match(token.TokenPipe)

		pats := []ast.NodeID{p.parsePattern()}
		for p.match(token.TokenPipe) {
			pats = append(pats, p.parsePattern())
		}
		var guard ast.NodeID
		if p.match(token.TokenWhen) {
			guardExpr := p.parseExpr(0)
			guard = p.Arena.Add(ast.Node{
				Kind: ast.KindGuard,
				Left: uint32(guardExpr),
			})
		}
		p.expect(token.TokenArrow)
		body := p.parseBlockBody()
		for _, pat := range pats {
			arms = append(arms, p.Arena.Add(ast.Node{
				Kind:  ast.KindMatchArm,
				Left:  uint32(pat),
				Right: uint32(body),
				Data:  uint64(guard),
			}))
		}
		p.skipSeparators()
	}

	if indented {
		p.match(token.TokenDedent)
	}
	start, count := p.Arena.AddExtra(arms)
	return p.Arena.Add(ast.Node{
		Kind:  ast.KindBlock,
		Left:  start,
		Right: count,
	})
}

// parsePattern parses structural patterns: wildcards, variables, constants,
// DU/Option constructors, tuples, lists, cons `h :: t`, records, and `: ?` type tests.
func (p *Parser) parsePattern() ast.NodeID {
	lhs := p.parseAtomicPattern()
	if p.match(token.TokenCons) {
		rhs := p.parsePattern()
		return p.Arena.Add(ast.Node{
			Kind:  ast.KindPatCons,
			Left:  uint32(lhs),
			Right: uint32(rhs),
		})
	}
	return lhs
}

func (p *Parser) parseAtomicPattern() ast.NodeID {
	tk := p.peek()
	tokIdx := uint32(p.pos)

	switch tk.Kind {
	case token.TokenUnderscore:
		p.advance()
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindPatWildcard,
			TokenIdx: tokIdx,
		})
	case token.TokenInt:
		p.advance()
		v := p.parseIntTok(tk)
		c := p.Arena.Add(ast.Node{Kind: ast.KindInt, Data: uint64(v)})
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindPatConst,
			TokenIdx: tokIdx,
			Left:     uint32(c),
			Data:     uint64(v),
		})
	case token.TokenFloat:
		p.advance()
		v := p.parseFloatTok(tk)
		c := p.Arena.Add(ast.Node{Kind: ast.KindFloat, Data: math.Float64bits(v)})
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindPatConst,
			TokenIdx: tokIdx,
			Left:     uint32(c),
			Data:     math.Float64bits(v),
		})
	case token.TokenString:
		p.advance()
		raw := tk.Slice(p.src)
		if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
			raw = raw[1 : len(raw)-1]
		}
		sid := p.Arena.Strings.Intern(raw)
		c := p.Arena.Add(ast.Node{Kind: ast.KindString, Data: uint64(sid)})
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindPatConst,
			TokenIdx: tokIdx,
			Left:     uint32(c),
			Data:     uint64(sid),
		})
	case token.TokenTrue, token.TokenFalse:
		p.advance()
		var b uint64
		if tk.Kind == token.TokenTrue {
			b = 1
		}
		c := p.Arena.Add(ast.Node{Kind: ast.KindBool, Data: b})
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindPatConst,
			TokenIdx: tokIdx,
			Left:     uint32(c),
			Data:     b,
		})
	case token.TokenUnit:
		p.advance()
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindPatConst,
			TokenIdx: tokIdx,
		})
	case token.TokenNone:
		p.advance()
		sid := p.Arena.Strings.InternString("None")
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindPatDU,
			TokenIdx: tokIdx,
			Data:     uint64(sid),
		})
	case token.TokenSome, token.TokenOk, token.TokenError:
		p.advance()
		sid := p.internTok(tk)
		var subPat ast.NodeID
		if p.canStartSubPattern(p.peek().Kind) {
			subPat = p.parseAtomicPattern()
		}
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindPatDU,
			TokenIdx: tokIdx,
			Left:     uint32(subPat),
			Data:     uint64(sid),
		})
	case token.TokenColonQMark:
		p.advance()
		tyTok := p.expect(token.TokenIdent)
		fullName := string(tyTok.Slice(p.src))
		for p.match(token.TokenDot) {
			part := p.expect(token.TokenIdent)
			fullName += "." + string(part.Slice(p.src))
		}
		sid := p.Arena.Strings.InternString(fullName)
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindPatTypeTest,
			TokenIdx: tokIdx,
			Data:     uint64(sid),
		})
	case token.TokenLParen:
		p.advance()
		if p.match(token.TokenRParen) {
			return p.Arena.Add(ast.Node{Kind: ast.KindPatConst, TokenIdx: tokIdx})
		}
		first := p.parsePattern()
		if p.match(token.TokenComma) {
			pats := []ast.NodeID{first}
			for !p.isEOF() && p.peek().Kind != token.TokenRParen {
				pats = append(pats, p.parsePattern())
				if !p.match(token.TokenComma) {
					break
				}
			}
			p.expect(token.TokenRParen)
			start, count := p.Arena.AddExtra(pats)
			return p.Arena.Add(ast.Node{
				Kind:     ast.KindPatTuple,
				TokenIdx: tokIdx,
				Left:     start,
				Right:    count,
			})
		}
		p.expect(token.TokenRParen)
		return first
	case token.TokenLBrack:
		p.advance()
		var pats []ast.NodeID
		for !p.isEOF() && p.peek().Kind != token.TokenRBrack {
			pats = append(pats, p.parsePattern())
			if !p.match(token.TokenSemicolon) && !p.match(token.TokenSemi) && !p.match(token.TokenComma) {
				break
			}
		}
		p.expect(token.TokenRBrack)
		start, count := p.Arena.AddExtra(pats)
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindPatList,
			TokenIdx: tokIdx,
			Left:     start,
			Right:    count,
		})
	case token.TokenLBrace:
		p.advance()
		var fields []ast.NodeID
		for !p.isEOF() && p.peek().Kind != token.TokenRBrace {
			p.skipSeparators()
			if p.peek().Kind == token.TokenRBrace {
				break
			}
			fTok := p.expect(token.TokenIdent)
			fID := p.internTok(fTok)
			p.expect(token.TokenAssign)
			subPat := p.parsePattern()
			fields = append(fields, p.Arena.Add(ast.Node{
				Kind: ast.KindRecordField,
				Left: uint32(subPat),
				Data: uint64(fID),
			}))
			p.skipSeparators()
		}
		p.expect(token.TokenRBrace)
		start, count := p.Arena.AddExtra(fields)
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindPatRecord,
			TokenIdx: tokIdx,
			Left:     start,
			Right:    count,
		})
	case token.TokenIdent:
		p.advance()
		raw := tk.Slice(p.src)
		sid := p.Arena.Strings.Intern(raw)
		// Uppercase identifier is a DU or Active Pattern constructor
		if len(raw) > 0 && raw[0] >= 'A' && raw[0] <= 'Z' {
			var subPat ast.NodeID
			if p.canStartSubPattern(p.peek().Kind) {
				subPat = p.parseAtomicPattern()
			}
			return p.Arena.Add(ast.Node{
				Kind:     ast.KindPatDU,
				TokenIdx: tokIdx,
				Left:     uint32(subPat),
				Data:     uint64(sid),
			})
		}
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindPatVar,
			TokenIdx: tokIdx,
			Data:     uint64(sid),
		})
	default:
		p.errorf(tk, "unexpected token in pattern: %s", tk.Kind)
		p.advance()
		return p.Arena.Add(ast.Node{Kind: ast.KindPatWildcard})
	}
}

func (p *Parser) canStartSubPattern(k token.TokenKind) bool {
	switch k {
	case token.TokenIdent, token.TokenUnderscore, token.TokenInt, token.TokenFloat,
		token.TokenString, token.TokenTrue, token.TokenFalse, token.TokenUnit,
		token.TokenLParen, token.TokenLBrack, token.TokenLBrace, token.TokenSome, token.TokenNone:
		return true
	default:
		return false
	}
}

// parseActivePattern parses `let (|Even|Odd|) n = body`.
func (p *Parser) parseActivePattern(tokIdx uint32) ast.NodeID {
	p.expect(token.TokenLActive)
	var tags []ast.NodeID
	var flags uint8
	for !p.isEOF() && p.peek().Kind != token.TokenRActive {
		if p.match(token.TokenUnderscore) {
			flags |= ast.FlagPartialPat
		} else {
			tagTok := p.expect(token.TokenIdent)
			tagID := p.internTok(tagTok)
			tags = append(tags, p.Arena.Add(ast.Node{
				Kind: ast.KindIdent,
				Data: uint64(tagID),
			}))
		}
		if !p.match(token.TokenPipe) {
			break
		}
	}
	p.expect(token.TokenRActive)

	var params []ast.NodeID
	for !p.isEOF() && p.peek().Kind != token.TokenAssign {
		param := p.parseParamPattern()
		if param == ast.NoNode {
			break
		}
		params = append(params, param)
	}
	p.expect(token.TokenAssign)
	rhs := p.parseBlockBody()

	for i := len(params) - 1; i >= 0; i-- {
		pNode := params[i]
		rhs = p.Arena.Add(ast.Node{
			Kind:  ast.KindLambda,
			Left:  uint32(pNode),
			Right: uint32(rhs),
			Data:  p.Arena.Get(pNode).Data,
		})
	}

	tags = append(tags, rhs)
	start, count := p.Arena.AddExtra(tags)
	var firstTagName uint64
	if len(tags) > 1 {
		firstTagName = p.Arena.Get(tags[0]).Data
	}
	return p.Arena.Add(ast.Node{
		Kind:     ast.KindActivePattern,
		Flags:    flags,
		TokenIdx: tokIdx,
		Left:     start,
		Right:    count,
		Data:     firstTagName,
	})
}
