package parser

import (
	"os"
	"path/filepath"
	"strings"

	"fml/internal/ast"
	"fml/internal/lexer"
	"fml/internal/token"
)

// parseIf parses `if cond then t [elif c2 then t2] [else f]`.
func (p *Parser) parseIf() ast.NodeID {
	tokIdx := uint32(p.pos)
	p.expect(token.TokenIf)
	cond := p.parseExpr(0)
	p.expect(token.TokenThen)
	thenBranch := p.parseBlockBody()

	var elseBranch ast.NodeID
	if p.match(token.TokenElif) {
		elseBranch = p.parseElifChain()
	} else if p.match(token.TokenElse) {
		elseBranch = p.parseBlockBody()
	}

	return p.Arena.Add(ast.Node{
		Kind:     ast.KindIf,
		TokenIdx: tokIdx,
		Left:     uint32(cond),
		Right:    uint32(thenBranch),
		Data:     uint64(elseBranch),
	})
}

func (p *Parser) parseElifChain() ast.NodeID {
	tokIdx := uint32(p.pos - 1)
	cond := p.parseExpr(0)
	p.expect(token.TokenThen)
	thenBranch := p.parseBlockBody()
	var elseBranch ast.NodeID
	if p.match(token.TokenElif) {
		elseBranch = p.parseElifChain()
	} else if p.match(token.TokenElse) {
		elseBranch = p.parseBlockBody()
	}
	return p.Arena.Add(ast.Node{
		Kind:     ast.KindIf,
		TokenIdx: tokIdx,
		Left:     uint32(cond),
		Right:    uint32(thenBranch),
		Data:     uint64(elseBranch),
	})
}

// parseFor parses both `for x in xs do body` and `for i = start to|downto end do body`.
func (p *Parser) parseFor() ast.NodeID {
	tokIdx := uint32(p.pos)
	p.expect(token.TokenFor)
	varTok := p.expect(token.TokenIdent)
	varID := p.internTok(varTok)

	if p.match(token.TokenIn) {
		coll := p.parseExpr(0)
		p.expect(token.TokenDo)
		body := p.parseBlockBody()
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindForIn,
			TokenIdx: tokIdx,
			Left:     uint32(coll),
			Right:    uint32(body),
			Data:     uint64(varID),
		})
	}

	p.expect(token.TokenAssign)
	startExpr := p.parseExpr(0)
	var flags uint8
	if p.match(token.TokenDownto) {
		flags |= ast.FlagBackward
	} else {
		p.expect(token.TokenTo)
	}
	endExpr := p.parseExpr(0)
	p.expect(token.TokenDo)
	body := p.parseBlockBody()

	rangeNode := p.Arena.Add(ast.Node{
		Kind:  ast.KindRange,
		Flags: flags,
		Left:  uint32(startExpr),
		Right: uint32(endExpr),
	})
	return p.Arena.Add(ast.Node{
		Kind:     ast.KindForTo,
		Flags:    flags,
		TokenIdx: tokIdx,
		Left:     uint32(rangeNode),
		Right:    uint32(body),
		Data:     uint64(varID),
	})
}

// parseWhile parses `while cond do body`.
func (p *Parser) parseWhile() ast.NodeID {
	tokIdx := uint32(p.pos)
	p.expect(token.TokenWhile)
	cond := p.parseExpr(0)
	p.expect(token.TokenDo)
	body := p.parseBlockBody()
	return p.Arena.Add(ast.Node{
		Kind:     ast.KindWhile,
		TokenIdx: tokIdx,
		Left:     uint32(cond),
		Right:    uint32(body),
	})
}

// parseTry parses `try body with arms` or `try body finally cleanup`.
func (p *Parser) parseTry() ast.NodeID {
	tokIdx := uint32(p.pos)
	p.expect(token.TokenTry)
	body := p.parseBlockBody()
	p.skipSeparators()

	if p.match(token.TokenFinally) {
		cleanup := p.parseBlockBody()
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindTryFinally,
			TokenIdx: tokIdx,
			Left:     uint32(body),
			Right:    uint32(cleanup),
		})
	}

	p.expect(token.TokenWith)
	arms := p.parseMatchArms()
	return p.Arena.Add(ast.Node{
		Kind:     ast.KindTryWith,
		TokenIdx: tokIdx,
		Left:     uint32(body),
		Right:    uint32(arms),
	})
}

// parseComputationExpr parses `seq { ... }`, `async { ... }`, `task { ... }`, or `builder { ... }`.
func (p *Parser) parseComputationExpr() ast.NodeID {
	tokIdx := uint32(p.pos)
	builderTok := p.advance()
	builderID := p.internTok(builderTok)
	p.expect(token.TokenLBrace)

	indented := p.match(token.TokenIndent)
	var items []ast.NodeID
	p.skipSeparators()
	for !p.isEOF() && p.peek().Kind != token.TokenRBrace {
		if indented && p.peek().Kind == token.TokenDedent {
			p.advance()
			indented = false
			if p.peek().Kind == token.TokenRBrace {
				break
			}
		}
		stmt := p.parseStatementOrExpr()
		if stmt != ast.NoNode {
			items = append(items, stmt)
		}
		p.skipSeparators()
	}
	if indented {
		p.match(token.TokenDedent)
	}
	p.expect(token.TokenRBrace)

	start, count := p.Arena.AddExtra(items)
	block := p.Arena.Add(ast.Node{
		Kind:  ast.KindBlock,
		Left:  start,
		Right: count,
	})
	nodeKind := ast.KindCompExpr
	if builderTok.Kind == token.TokenSeq {
		nodeKind = ast.KindSeq
	}
	return p.Arena.Add(ast.Node{
		Kind:     nodeKind,
		TokenIdx: tokIdx,
		Left:     uint32(block),
		Data:     uint64(builderID),
	})
}

func (p *Parser) looksLikeCompExprBrace() bool {
	// Distinguish `builder { let! ... }` or `seq { 1 .. 10 }` from `f { Field = 1 }`
	// Record literals have `{ Ident = ... }` or `{ new ... }`.
	if p.peekAt(1).Kind != token.TokenLBrace {
		return false
	}
	idx := 2
	if p.peekAt(idx).Kind == token.TokenIndent {
		idx++
	}
	t1 := p.peekAt(idx)
	t2 := p.peekAt(idx + 1)
	if t1.Kind == token.TokenNew {
		return false
	}
	if t1.Kind == token.TokenIdent && (t2.Kind == token.TokenAssign || t2.Kind == token.TokenWith) {
		return false
	}
	return true
}

// parseBangBinding parses `let! x = expr`, `do! expr`, and `use! x = expr`.
func (p *Parser) parseBangBinding() ast.NodeID {
	tokIdx := uint32(p.pos)
	bangTok := p.advance()
	flags := ast.FlagBang
	if bangTok.Kind == token.TokenUseBang {
		flags |= ast.FlagUse
	}

	var nameID token.StringID
	var rhs ast.NodeID
	if bangTok.Kind == token.TokenDoBang {
		rhs = p.parseExpr(0)
	} else {
		nameTok := p.expect(token.TokenIdent)
		nameID = p.internTok(nameTok)
		p.expect(token.TokenAssign)
		rhs = p.parseExpr(0)
	}

	var inBody ast.NodeID
	if p.match(token.TokenIn) {
		inBody = p.parseBlockBody()
	}

	return p.Arena.Add(ast.Node{
		Kind:     ast.KindBindBang,
		Flags:    flags,
		Op:       uint16(bangTok.Kind),
		TokenIdx: tokIdx,
		Left:     uint32(rhs),
		Right:    uint32(inBody),
		Data:     uint64(nameID),
	})
}

func (p *Parser) parseReturnOrYield() ast.NodeID {
	tokIdx := uint32(p.pos)
	tk := p.advance()
	var flags uint8
	if tk.Kind == token.TokenReturnBang {
		flags |= ast.FlagBang
	}
	expr := p.parseExpr(0)
	return p.Arena.Add(ast.Node{
		Kind:     ast.KindReturn,
		Flags:    flags,
		Op:       uint16(tk.Kind),
		TokenIdx: tokIdx,
		Left:     uint32(expr),
	})
}

// parseNamespace parses `namespace MyCompany.Project`.
func (p *Parser) parseNamespace() ast.NodeID {
	tokIdx := uint32(p.pos)
	p.expect(token.TokenNamespace)
	nsID := p.parseQualifiedIdent()
	return p.Arena.Add(ast.Node{
		Kind:     ast.KindNamespace,
		TokenIdx: tokIdx,
		Data:     uint64(nsID),
	})
}

// parseOpen parses `open ModuleName` and automatically loads external `<ModuleName>.fml`
// files from p.BaseDir if present and not yet loaded.
func (p *Parser) parseOpen() ast.NodeID {
	tokIdx := uint32(p.pos)
	p.expect(token.TokenOpen)
	modID := p.parseQualifiedIdent()
	modName := p.Arena.Strings.String(modID)

	var loadedModNode ast.NodeID
	if p.BaseDir != "" && !p.LoadedModules[modName] {
		candidates := []string{
			filepath.Join(p.BaseDir, modName+".fml"),
			filepath.Join(p.BaseDir, strings.ToLower(modName)+".fml"),
			filepath.Join(p.BaseDir, toSnakeCase(modName)+".fml"),
		}
		for _, cand := range candidates {
			raw, err := os.ReadFile(cand)
			if err == nil {
				p.LoadedModules[modName] = true
				subLex := lexer.New(raw, nil)
				subToks := subLex.ScanAll()
				subP := New(raw, subToks, p.Arena)
				subP.BaseDir = p.BaseDir
				subP.LoadedModules = p.LoadedModules
				subRoot := subP.ParseProgram()
				p.Errors = append(p.Errors, subP.Errors...)

				progNode := *p.Arena.Get(subRoot)
				bodyBlock := p.Arena.Add(ast.Node{
					Kind:  ast.KindBlock,
					Left:  progNode.Left,
					Right: progNode.Right,
				})
				loadedModNode = p.Arena.Add(ast.Node{
					Kind:     ast.KindModule,
					TokenIdx: tokIdx,
					Left:     uint32(bodyBlock),
					Data:     uint64(modID),
				})
				break
			}
		}
	}

	return p.Arena.Add(ast.Node{
		Kind:     ast.KindOpen,
		TokenIdx: tokIdx,
		Left:     uint32(loadedModNode),
		Data:     uint64(modID),
	})
}

func toSnakeCase(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r + ('a' - 'A'))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// parseModuleOrFunctor parses `module M [= ...]` or SML-style parametric functor
// `module MakeMap (Ord: COMPARABLE) = ...`.
func (p *Parser) parseModuleOrFunctor() ast.NodeID {
	tokIdx := uint32(p.pos)
	p.expect(token.TokenModule)
	nameID := p.parseQualifiedIdent()

	// Check for Parametric Module Functor parameter `(ArgMod: Sig)`
	if p.peek().Kind == token.TokenLParen {
		p.advance()
		argTok := p.expect(token.TokenIdent)
		argID := p.internTok(argTok)
		var sigID token.StringID
		if p.match(token.TokenColon) {
			sigID = p.parseQualifiedIdent()
		}
		p.expect(token.TokenRParen)
		paramNode := p.Arena.Add(ast.Node{
			Kind: ast.KindIdent,
			Left: uint32(sigID),
			Data: uint64(argID),
		})
		p.expect(token.TokenAssign)
		body := p.parseModuleBody()
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindFunctor,
			TokenIdx: tokIdx,
			Left:     uint32(paramNode),
			Right:    uint32(body),
			Data:     uint64(nameID),
		})
	}

	if p.match(token.TokenAssign) {
		body := p.parseModuleBody()
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindModule,
			TokenIdx: tokIdx,
			Left:     uint32(body),
			Data:     uint64(nameID),
		})
	}

	return p.Arena.Add(ast.Node{
		Kind:     ast.KindModule,
		TokenIdx: tokIdx,
		Data:     uint64(nameID),
	})
}

func (p *Parser) parseModuleBody() ast.NodeID {
	// Support functor application `module M = MakeMap(IntOrd)`
	if p.peek().Kind == token.TokenIdent && p.peekAt(1).Kind == token.TokenLParen {
		fnTok := p.advance()
		fnID := p.internTok(fnTok)
		p.expect(token.TokenLParen)
		argTok := p.expect(token.TokenIdent)
		argID := p.internTok(argTok)
		p.expect(token.TokenRParen)
		fnNode := p.Arena.Add(ast.Node{Kind: ast.KindIdent, Data: uint64(fnID)})
		argNode := p.Arena.Add(ast.Node{Kind: ast.KindIdent, Data: uint64(argID)})
		return p.Arena.Add(ast.Node{
			Kind:  ast.KindApply,
			Left:  uint32(fnNode),
			Right: uint32(argNode),
		})
	}
	return p.parseBlockBody()
}

func (p *Parser) parseQualifiedIdent() token.StringID {
	first := p.expect(token.TokenIdent)
	full := string(first.Slice(p.src))
	for p.match(token.TokenDot) {
		part := p.expect(token.TokenIdent)
		full += "." + string(part.Slice(p.src))
	}
	return p.Arena.Strings.InternString(full)
}

func (p *Parser) parseMemberList() ast.NodeID {
	indented := p.match(token.TokenIndent)
	var members []ast.NodeID
	for !p.isEOF() {
		p.skipSeparators()
		if indented && p.peek().Kind == token.TokenDedent {
			break
		}
		if p.peek().Kind != token.TokenMember && p.peek().Kind != token.TokenAbstract && p.peek().Kind != token.TokenStatic {
			break
		}
		members = append(members, p.parseSingleMember())
		p.skipSeparators()
	}
	if indented {
		p.match(token.TokenDedent)
	}
	start, count := p.Arena.AddExtra(members)
	return p.Arena.Add(ast.Node{
		Kind:  ast.KindBlock,
		Left:  start,
		Right: count,
	})
}

func (p *Parser) parseMemberListUntilBrace() ast.NodeID {
	var members []ast.NodeID
	for !p.isEOF() && p.peek().Kind != token.TokenRBrace {
		p.skipSeparators()
		if p.peek().Kind == token.TokenRBrace {
			break
		}
		if p.peek().Kind == token.TokenMember || p.peek().Kind == token.TokenAbstract {
			members = append(members, p.parseSingleMember())
		} else {
			break
		}
		p.skipSeparators()
	}
	start, count := p.Arena.AddExtra(members)
	return p.Arena.Add(ast.Node{
		Kind:  ast.KindBlock,
		Left:  start,
		Right: count,
	})
}

func (p *Parser) parseSingleMember() ast.NodeID {
	tokIdx := uint32(p.pos)
	var flags uint8
	if p.match(token.TokenStatic) {
		flags |= ast.FlagStatic
	}
	if p.match(token.TokenAbstract) {
		flags |= ast.FlagAbstract
		p.match(token.TokenMember)
		nameTok := p.expect(token.TokenIdent)
		nameID := p.internTok(nameTok)
		p.expect(token.TokenColon)
		sig := p.parseTypeExpr()
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindMember,
			Flags:    flags,
			TokenIdx: tokIdx,
			Left:     uint32(sig),
			Data:     uint64(nameID),
		})
	}

	p.expect(token.TokenMember)
	// Parse `this.Prop` or `_.Method()`
	if p.peek().Kind == token.TokenUnderscore {
		p.advance()
	} else {
		p.expect(token.TokenIdent)
	}
	p.expect(token.TokenDot)
	memTok := p.expect(token.TokenIdent)
	memID := p.internTok(memTok)

	// Optional parameters before `=`
	var params []ast.NodeID
	for !p.isEOF() && p.peek().Kind != token.TokenAssign {
		param := p.parseParamPattern()
		if param == ast.NoNode {
			break
		}
		params = append(params, param)
	}
	p.expect(token.TokenAssign)
	body := p.parseExpr(0)
	for i := len(params) - 1; i >= 0; i-- {
		pNode := params[i]
		body = p.Arena.Add(ast.Node{
			Kind:  ast.KindLambda,
			Left:  uint32(pNode),
			Right: uint32(body),
			Data:  p.Arena.Get(pNode).Data,
		})
	}
	return p.Arena.Add(ast.Node{
		Kind:     ast.KindMember,
		Flags:    flags,
		TokenIdx: tokIdx,
		Left:     uint32(body),
		Data:     uint64(memID),
	})
}

// parseAttributePrefixed parses `[<Measure>]` or `[<CLIMutable>]` followed by a declaration.
func (p *Parser) parseAttributePrefixed() ast.NodeID {
	tokIdx := uint32(p.pos)
	p.expect(token.TokenLAttr)
	var attrID token.StringID
	isMeasure := false
	if p.match(token.TokenMeasure) {
		isMeasure = true
		attrID = p.Arena.Strings.InternString("Measure")
	} else {
		attrTok := p.expect(token.TokenIdent)
		attrID = p.internTok(attrTok)
	}
	p.expect(token.TokenRAttr)
	p.skipSeparators()

	if isMeasure && p.peek().Kind == token.TokenType {
		return p.parseTypeDecl(true)
	}

	var target ast.NodeID
	if !p.isEOF() && p.peek().Kind != token.TokenDedent {
		if p.peek().Kind == token.TokenType {
			target = p.parseTypeDecl(false)
		} else if p.peek().Kind == token.TokenLet {
			target = p.parseLetOrUse()
		}
	}
	return p.Arena.Add(ast.Node{
		Kind:     ast.KindAttr,
		TokenIdx: tokIdx,
		Left:     uint32(target),
		Data:     uint64(attrID),
	})
}

// parseQuotation parses `<@ expr @>`.
func (p *Parser) parseQuotation() ast.NodeID {
	tokIdx := uint32(p.pos)
	p.expect(token.TokenLQuote)
	inner := p.parseExpr(0)
	p.expect(token.TokenRQuote)
	return p.Arena.Add(ast.Node{
		Kind:     ast.KindQuote,
		TokenIdx: tokIdx,
		Left:     uint32(inner),
	})
}
