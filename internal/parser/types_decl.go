package parser

import (
	"fml/internal/ast"
	"fml/internal/token"
)

// parseTypeDecl parses F# `type` declarations:
// - `[<Measure>] type kg`
// - Records: `type Person = { Name: string; Age: int }`
// - Discriminated Unions: `type Shape = Circle of float | Rect of float * float`
// - Classes: `type MyClass(x: int) = member _.X = x`
// - Interfaces: `type IPrintable = abstract member Print: unit -> unit`
// - Type Extensions: `type System.Int32 with member this.IsEven = ...`
// - Type Providers: `type DB = SqlDataProvider<...>`
func (p *Parser) parseTypeDecl(isMeasure bool) ast.NodeID {
	tokIdx := uint32(p.pos)
	p.expect(token.TokenType)

	// Parse qualified type name (e.g., System.Int32 or Person)
	nameTok := p.expect(token.TokenIdent)
	fullName := string(nameTok.Slice(p.src))
	for p.match(token.TokenDot) {
		part := p.expect(token.TokenIdent)
		fullName += "." + string(part.Slice(p.src))
	}
	nameID := p.Arena.Strings.InternString(fullName)

	// Optional generic type parameters: `'a` or `<'a>`
	if p.match(token.TokenLt) {
		for !p.isEOF() && p.peek().Kind != token.TokenGt {
			p.advance()
		}
		p.match(token.TokenGt)
	}

	// Unit of Measure declaration: `[<Measure>] type kg`
	if isMeasure && p.peek().Kind != token.TokenAssign {
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindMeasure,
			TokenIdx: tokIdx,
			Data:     uint64(nameID),
		})
	}

	// Type extension: `type System.Int32 with member ...`
	if p.match(token.TokenWith) {
		members := p.parseMemberList()
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindTypeExt,
			TokenIdx: tokIdx,
			Left:     uint32(members),
			Data:     uint64(nameID),
		})
	}

	// Primary constructor class: `type MyClass(x: int) = ...`
	if p.peek().Kind == token.TokenLParen {
		ctorParam := p.parseParamPattern()
		p.expect(token.TokenAssign)
		members := p.parseMemberList()
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindClass,
			TokenIdx: tokIdx,
			Left:     uint32(ctorParam),
			Right:    uint32(members),
			Data:     uint64(nameID),
		})
	}

	if !p.match(token.TokenAssign) {
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindMeasure,
			TokenIdx: tokIdx,
			Data:     uint64(nameID),
		})
	}

	p.match(token.TokenIndent)
	defer p.match(token.TokenDedent)

	// Record type: `type Person = { Name: string; Age: int }`
	if p.peek().Kind == token.TokenLBrace {
		p.advance()
		var fields []ast.NodeID
		for !p.isEOF() && p.peek().Kind != token.TokenRBrace {
			p.skipSeparators()
			if p.peek().Kind == token.TokenRBrace {
				break
			}
			fMut := p.match(token.TokenMutable)
			fTok := p.expect(token.TokenIdent)
			fID := p.internTok(fTok)
			p.expect(token.TokenColon)
			fType := p.parseTypeExpr()
			var flags uint8
			if fMut {
				flags |= ast.FlagMutable
			}
			fields = append(fields, p.Arena.Add(ast.Node{
				Kind:  ast.KindRecordField,
				Flags: flags,
				Left:  uint32(fType),
				Data:  uint64(fID),
			}))
			p.skipSeparators()
		}
		p.expect(token.TokenRBrace)
		start, count := p.Arena.AddExtra(fields)
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindRecordType,
			TokenIdx: tokIdx,
			Left:     start,
			Right:    count,
			Data:     uint64(nameID),
		})
	}

	// Interface / Abstract Member declaration: `type IPrintable = abstract member ...`
	if p.peek().Kind == token.TokenAbstract || p.peek().Kind == token.TokenInterface {
		members := p.parseMemberList()
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindInterface,
			TokenIdx: tokIdx,
			Left:     uint32(members),
			Data:     uint64(nameID),
		})
	}

	// Type Provider: `type DB = SqlDataProvider<...>`
	if p.peek().Kind == token.TokenIdent && p.peekAt(1).Kind == token.TokenLt {
		provTok := p.advance()
		provID := p.internTok(provTok)
		provNode := p.Arena.Add(ast.Node{
			Kind: ast.KindIdent,
			Data: uint64(provID),
		})
		p.expect(token.TokenLt)
		var argNode ast.NodeID
		if p.peek().Kind != token.TokenGt {
			if p.peek().Kind == token.TokenRange || (p.peek().Kind == token.TokenDot && p.peekAt(1).Kind == token.TokenRange) {
				for !p.isEOF() && p.peek().Kind != token.TokenGt {
					p.advance()
				}
			} else {
				argNode = p.parsePrimary()
				for !p.isEOF() && p.peek().Kind != token.TokenGt {
					p.advance()
				}
			}
		}
		p.expect(token.TokenGt)
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindTypeProvider,
			TokenIdx: tokIdx,
			Left:     uint32(provNode),
			Right:    uint32(argNode),
			Data:     uint64(nameID),
		})
	}

	// Discriminated Union (ADT): `type Shape = Circle of float | Rect of float * float`
	p.match(token.TokenPipe)
	var cases []ast.NodeID
	for !p.isEOF() {
		caseTok := p.expect(token.TokenIdent)
		caseID := p.internTok(caseTok)
		var payloadType ast.NodeID
		if p.match(token.TokenOf) {
			payloadType = p.parseTypeExpr()
		}
		cases = append(cases, p.Arena.Add(ast.Node{
			Kind: ast.KindDUCase,
			Left: uint32(payloadType),
			Data: uint64(caseID),
		}))
		if !p.match(token.TokenPipe) {
			break
		}
	}
	start, count := p.Arena.AddExtra(cases)
	return p.Arena.Add(ast.Node{
		Kind:     ast.KindDUType,
		TokenIdx: tokIdx,
		Left:     start,
		Right:    count,
		Data:     uint64(nameID),
	})
}

func (p *Parser) parseTypeExpr() ast.NodeID {
	lhs := p.parseAtomicType()
	// Tuple type: `T1 * T2`
	if p.peek().Kind == token.TokenStar {
		elems := []ast.NodeID{lhs}
		for p.match(token.TokenStar) {
			elems = append(elems, p.parseAtomicType())
		}
		start, count := p.Arena.AddExtra(elems)
		lhs = p.Arena.Add(ast.Node{
			Kind:  ast.KindTypeTuple,
			Left:  start,
			Right: count,
		})
	}
	// Function arrow type: `T1 -> T2`
	if p.match(token.TokenArrow) {
		rhs := p.parseTypeExpr()
		lhs = p.Arena.Add(ast.Node{
			Kind:  ast.KindTypeArrow,
			Left:  uint32(lhs),
			Right: uint32(rhs),
		})
	}
	return lhs
}

func (p *Parser) parseAtomicType() ast.NodeID {
	if p.match(token.TokenApostrophe) {
		tvTok := p.expect(token.TokenIdent)
		sid := p.Arena.Strings.InternString("'" + string(tvTok.Slice(p.src)))
		return p.Arena.Add(ast.Node{
			Kind: ast.KindTypeIdent,
			Data: uint64(sid),
		})
	}
	if p.match(token.TokenLParen) {
		inner := p.parseTypeExpr()
		p.expect(token.TokenRParen)
		return inner
	}
	tk := p.expect(token.TokenIdent)
	sid := p.internTok(tk)
	base := p.Arena.Add(ast.Node{
		Kind: ast.KindTypeIdent,
		Data: uint64(sid),
	})
	// Check for generic or unit-of-measure application: `float<kg>`
	if p.match(token.TokenLt) {
		unitOrArg := p.parseMeasureUnitExpr()
		p.expect(token.TokenGt)
		return p.Arena.Add(ast.Node{
			Kind:  ast.KindTypeApp,
			Left:  uint32(base),
			Right: uint32(unitOrArg),
			Data:  uint64(sid),
		})
	}
	return base
}

func (p *Parser) parseMeasureUnitExpr() ast.NodeID {
	lhs := p.parseMeasureAtom()
	for {
		tk := p.peek()
		if tk.Kind == token.TokenStar || tk.Kind == token.TokenSlash {
			op := p.advance()
			rhs := p.parseMeasureAtom()
			lhs = p.Arena.Add(ast.Node{
				Kind:  ast.KindMeasureUnit,
				Op:    uint16(op.Kind),
				Left:  uint32(lhs),
				Right: uint32(rhs),
			})
			continue
		}
		break
	}
	return lhs
}

func (p *Parser) parseMeasureAtom() ast.NodeID {
	tk := p.peek()
	if tk.Kind == token.TokenInt {
		p.advance()
		val := p.parseIntTok(tk)
		return p.Arena.Add(ast.Node{
			Kind: ast.KindInt,
			Data: uint64(val),
		})
	}
	idTok := p.expect(token.TokenIdent)
	sid := p.internTok(idTok)
	atom := p.Arena.Add(ast.Node{
		Kind: ast.KindMeasureUnit,
		Data: uint64(sid),
	})
	if p.match(token.TokenCaret) {
		expTok := p.expect(token.TokenInt)
		expVal := p.parseIntTok(expTok)
		expNode := p.Arena.Add(ast.Node{
			Kind: ast.KindInt,
			Data: uint64(expVal),
		})
		atom = p.Arena.Add(ast.Node{
			Kind:  ast.KindMeasureUnit,
			Op:    uint16(token.TokenCaret),
			Left:  uint32(atom),
			Right: uint32(expNode),
		})
	}
	return atom
}

func (p *Parser) looksLikeMeasureSuffix() bool {
	// Look ahead from `<` for matching `>` on the same line containing only measure tokens
	if p.peek().Kind != token.TokenLt {
		return false
	}
	line := p.peek().Line
	for i := 1; i <= 10; i++ {
		tk := p.peekAt(i)
		if tk.Kind == token.TokenEOF || tk.Line != line {
			return false
		}
		if tk.Kind == token.TokenGt {
			return i > 1
		}
		switch tk.Kind {
		case token.TokenIdent, token.TokenInt, token.TokenStar, token.TokenSlash, token.TokenCaret:
			continue
		default:
			return false
		}
	}
	return false
}

func (p *Parser) parseParenOrTuple() ast.NodeID {
	tokIdx := uint32(p.pos)
	p.expect(token.TokenLParen)
	if p.match(token.TokenRParen) {
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindUnit,
			TokenIdx: tokIdx,
		})
	}
	first := p.parseExpr(0)
	if p.match(token.TokenColon) {
		tyNode := p.parseTypeExpr()
		p.expect(token.TokenRParen)
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindTypeAnnot,
			TokenIdx: tokIdx,
			Left:     uint32(first),
			Right:    uint32(tyNode),
		})
	}
	if p.match(token.TokenComma) {
		elems := []ast.NodeID{first}
		for !p.isEOF() && p.peek().Kind != token.TokenRParen {
			elems = append(elems, p.parseExpr(0))
			if !p.match(token.TokenComma) {
				break
			}
		}
		p.expect(token.TokenRParen)
		start, count := p.Arena.AddExtra(elems)
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindTuple,
			TokenIdx: tokIdx,
			Left:     start,
			Right:    count,
		})
	}
	p.expect(token.TokenRParen)
	return first
}

func (p *Parser) parseListLiteral() ast.NodeID {
	tokIdx := uint32(p.pos)
	p.expect(token.TokenLBrack)
	var elems []ast.NodeID
	p.skipSeparators()
	for !p.isEOF() && p.peek().Kind != token.TokenRBrack {
		expr := p.parseExpr(0)
		elems = append(elems, expr)
		if !p.match(token.TokenSemicolon) && !p.match(token.TokenSemi) && !p.match(token.TokenComma) {
			break
		}
		p.skipSeparators()
	}
	p.expect(token.TokenRBrack)
	start, count := p.Arena.AddExtra(elems)
	return p.Arena.Add(ast.Node{
		Kind:     ast.KindList,
		TokenIdx: tokIdx,
		Left:     start,
		Right:    count,
	})
}

func (p *Parser) parseArrayLiteral() ast.NodeID {
	tokIdx := uint32(p.pos)
	p.expect(token.TokenLArray)
	var elems []ast.NodeID
	p.skipSeparators()
	for !p.isEOF() && p.peek().Kind != token.TokenRArray {
		expr := p.parseExpr(0)
		elems = append(elems, expr)
		if !p.match(token.TokenSemicolon) && !p.match(token.TokenSemi) && !p.match(token.TokenComma) {
			break
		}
		p.skipSeparators()
	}
	p.expect(token.TokenRArray)
	start, count := p.Arena.AddExtra(elems)
	return p.Arena.Add(ast.Node{
		Kind:     ast.KindArray,
		TokenIdx: tokIdx,
		Left:     start,
		Right:    count,
	})
}

func (p *Parser) parseBraceRecordOrObjExpr() ast.NodeID {
	tokIdx := uint32(p.pos)
	p.expect(token.TokenLBrace)
	p.skipSeparators()

	// Object expression: `{ new IPrintable with member _.Print() = ... }`
	if p.match(token.TokenNew) {
		ifaceTok := p.expect(token.TokenIdent)
		ifaceID := p.internTok(ifaceTok)
		p.match(token.TokenLParen)
		p.match(token.TokenRParen)
		p.match(token.TokenWith)
		members := p.parseMemberListUntilBrace()
		p.expect(token.TokenRBrace)
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindObjExpr,
			TokenIdx: tokIdx,
			Left:     uint32(members),
			Data:     uint64(ifaceID),
		})
	}

	// Record literal: `{ Name = "Alice"; Age = 30 }` or `{ r with Age = 31 }`
	var baseExpr ast.NodeID
	p.skipSeparators()
	if !(p.peek().Kind == token.TokenIdent && p.peekAt(1).Kind == token.TokenAssign) && p.peek().Kind != token.TokenRBrace {
		baseExpr = p.parseExpr(0)
		p.expect(token.TokenWith)
	}
	var fields []ast.NodeID
	for !p.isEOF() && p.peek().Kind != token.TokenRBrace {
		p.skipSeparators()
		if p.peek().Kind == token.TokenRBrace {
			break
		}
		fTok := p.expect(token.TokenIdent)
		fID := p.internTok(fTok)
		p.expect(token.TokenAssign)
		val := p.parseExpr(0)
		fields = append(fields, p.Arena.Add(ast.Node{
			Kind: ast.KindRecordField,
			Left: uint32(val),
			Data: uint64(fID),
		}))
		p.skipSeparators()
	}
	p.expect(token.TokenRBrace)
	start, count := p.Arena.AddExtra(fields)
	return p.Arena.Add(ast.Node{
		Kind:     ast.KindRecordLit,
		TokenIdx: tokIdx,
		Left:     start,
		Right:    count,
		Data:     uint64(baseExpr),
	})
}

func (p *Parser) parseOptionLiteral() ast.NodeID {
	tokIdx := uint32(p.pos)
	tk := p.advance()
	if tk.Kind == token.TokenNone {
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindOption,
			TokenIdx: tokIdx,
			Data:     0,
		})
	}
	payload := p.parseExpr(precApply)
	return p.Arena.Add(ast.Node{
		Kind:     ast.KindOption,
		TokenIdx: tokIdx,
		Left:     uint32(payload),
		Data:     1,
	})
}

func (p *Parser) parseResultLiteral() ast.NodeID {
	tokIdx := uint32(p.pos)
	tk := p.advance()
	var tag uint64
	if tk.Kind == token.TokenOk {
		tag = 1
	}
	payload := p.parseExpr(precApply)
	return p.Arena.Add(ast.Node{
		Kind:     ast.KindResult,
		TokenIdx: tokIdx,
		Left:     uint32(payload),
		Data:     tag,
	})
}
