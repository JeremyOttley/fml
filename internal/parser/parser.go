// Package parser implements a recursive descent and Pratt precedence parser
// for FML, lowering token streams directly into a flat ast.Arena.
//
// Jaime says: A parser shouldn't build a heap-allocated tree of interface
// nodes just to throw it away in semantic analysis. Consume the flat token
// array, resolve precedence with a tight Pratt loop, and write 24-byte Node
// structs straight into the Arena.
package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"fml/internal/ast"
	"fml/internal/lexer"
	"fml/internal/token"
)

// Parser lowers a flat token slice into an ast.Arena.
//
// Jaime says: Keep parser state flat: source bytes, token slice, current
// cursor, and a pointer to the AST arena.
type Parser struct {
	src           []byte
	tokens        []token.Token
	pos           int
	Arena         *ast.Arena
	BaseDir       string
	LoadedModules map[string]bool
	Errors        []string
}

// New creates a Parser over src and tokens writing into arena.
func New(src []byte, tokens []token.Token, arena *ast.Arena) *Parser {
	if arena == nil {
		arena = ast.NewArena(512, 512, token.NewStringTable(4096, 512))
	}
	return &Parser{
		src:           src,
		tokens:        tokens,
		pos:           0,
		Arena:         arena,
		LoadedModules: make(map[string]bool),
	}
}

// Source returns the raw byte buffer backing the parsed tokens.
func (p *Parser) Source() []byte { return p.src }

// Tokens returns the flat token slice produced by the lexer.
func (p *Parser) Tokens() []token.Token { return p.tokens }

// ParseSource is a convenience helper that lexes src (with off-side rules)
// and parses the full compilation unit into a fresh or provided Arena.
func ParseSource(src []byte, arena *ast.Arena) (*Parser, ast.NodeID, error) {
	return ParseSourceInDir(src, "", arena)
}

// ParseSourceInDir lexes and parses src with BaseDir configured for multi-file
// `open` module resolution and compile-time type provider file loading.
func ParseSourceInDir(src []byte, baseDir string, arena *ast.Arena) (*Parser, ast.NodeID, error) {
	lex := lexer.New(src, nil)
	toks := lex.ScanAll()
	p := New(src, toks, arena)
	p.BaseDir = baseDir
	root := p.ParseProgram()
	if len(p.Errors) > 0 {
		return p, root, fmt.Errorf("parse error: %s", p.Errors[0])
	}
	return p, root, nil
}

// ParseFile reads path from disk and parses it with multi-file `open` resolution
// rooted at filepath.Dir(path).
func ParseFile(path string, arena *ast.Arena) (*Parser, ast.NodeID, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, ast.NoNode, err
	}
	baseDir := filepath.Dir(path)
	lex := lexer.New(src, nil)
	toks := lex.ScanAll()
	p := New(src, toks, arena)
	p.BaseDir = baseDir
	baseName := filepath.Base(path)
	if ext := filepath.Ext(baseName); ext != "" {
		baseName = baseName[:len(baseName)-len(ext)]
	}
	p.LoadedModules[baseName] = true
	root := p.ParseProgram()
	if len(p.Errors) > 0 {
		return p, root, fmt.Errorf("parse error: %s", p.Errors[0])
	}
	return p, root, nil
}

// ParseProgram parses a full sequence of top-level declarations and expressions.
func (p *Parser) ParseProgram() ast.NodeID {
	var stmts []ast.NodeID
	p.skipSeparators()
	for !p.isEOF() {
		stmt := p.parseTopLevelItem()
		if stmt != ast.NoNode {
			stmts = append(stmts, stmt)
		}
		p.skipSeparators()
	}
	start, count := p.Arena.AddExtra(stmts)
	return p.Arena.Add(ast.Node{
		Kind:  ast.KindProgram,
		Left:  start,
		Right: count,
	})
}

func (p *Parser) parseTopLevelItem() ast.NodeID {
	p.skipSeparators()
	switch p.peek().Kind {
	case token.TokenLAttr:
		return p.parseAttributePrefixed()
	case token.TokenNamespace:
		return p.parseNamespace()
	case token.TokenModule:
		return p.parseModuleOrFunctor()
	case token.TokenOpen:
		return p.parseOpen()
	case token.TokenType:
		return p.parseTypeDecl(false)
	case token.TokenLet, token.TokenUse:
		return p.parseLetOrUse()
	default:
		return p.parseExpr(0)
	}
}

func (p *Parser) parseBlockBody() ast.NodeID {
	if p.match(token.TokenIndent) {
		var items []ast.NodeID
		p.skipSeparators()
		for !p.isEOF() && p.peek().Kind != token.TokenDedent {
			item := p.parseStatementOrExpr()
			if item != ast.NoNode {
				items = append(items, item)
			}
			p.skipSeparators()
		}
		p.match(token.TokenDedent)
		if len(items) == 1 {
			return items[0]
		}
		start, count := p.Arena.AddExtra(items)
		return p.Arena.Add(ast.Node{
			Kind:  ast.KindBlock,
			Left:  start,
			Right: count,
		})
	}
	return p.parseStatementOrExpr()
}

func (p *Parser) parseStatementOrExpr() ast.NodeID {
	p.skipSeparators()
	switch p.peek().Kind {
	case token.TokenLAttr:
		return p.parseAttributePrefixed()
	case token.TokenType:
		return p.parseTypeDecl(false)
	case token.TokenModule:
		return p.parseModuleOrFunctor()
	case token.TokenOpen:
		return p.parseOpen()
	case token.TokenLet, token.TokenUse:
		return p.parseLetOrUse()
	case token.TokenLetBang, token.TokenDoBang, token.TokenUseBang:
		return p.parseBangBinding()
	case token.TokenReturn, token.TokenReturnBang, token.TokenYield:
		return p.parseReturnOrYield()
	default:
		return p.parseExpr(0)
	}
}

// parseLetOrUse parses `let`, `let mutable`, `let rec`, `let use`, `use`, and
// Active Pattern definitions `let (|A|B|) x = ...`.
func (p *Parser) parseLetOrUse() ast.NodeID {
	tokIdx := uint32(p.pos)
	isUse := false
	if p.match(token.TokenUse) {
		isUse = true
	} else {
		p.expect(token.TokenLet)
		if p.match(token.TokenUse) {
			isUse = true
		}
	}

	isMutable := p.match(token.TokenMutable)
	isRec := p.match(token.TokenRec)

	// Check for Active Pattern: let (|Even|Odd|) x = ...
	if p.peek().Kind == token.TokenLActive {
		return p.parseActivePattern(tokIdx)
	}

	// Check for tuple destructuring: let (a, b) = ...
	var patNode ast.NodeID
	var nameID token.StringID
	if p.peek().Kind == token.TokenLParen {
		patNode = p.parsePattern()
	} else {
		nameTok := p.expect(token.TokenIdent)
		nameID = p.internTok(nameTok)
	}

	// Collect optional curried parameters: let add x (y: int) = ...
	var params []ast.NodeID
	for !p.isEOF() && p.peek().Kind != token.TokenAssign && p.peek().Kind != token.TokenColon {
		param := p.parseParamPattern()
		if param == ast.NoNode {
			break
		}
		params = append(params, param)
	}

	// Optional return type annotation: `: Type`
	if p.match(token.TokenColon) {
		_ = p.parseTypeExpr()
	}

	p.expect(token.TokenAssign)
	rhs := p.parseBlockBody()
	rhs = p.wrapLambdaParams(tokIdx, params, rhs)

	var inBody ast.NodeID
	if isRec && p.match(token.TokenAnd) {
		inBody = p.parseRecAndBinding()
	} else if p.match(token.TokenIn) {
		inBody = p.parseBlockBody()
	}

	kind := ast.KindLet
	var flags uint8
	if isUse {
		kind = ast.KindUse
		flags |= ast.FlagUse
	} else if isMutable {
		kind = ast.KindLetMutable
		flags |= ast.FlagMutable
	} else if isRec {
		kind = ast.KindLetRec
		flags |= ast.FlagRec
	}

	node := ast.Node{
		Kind:     kind,
		Flags:    flags,
		TokenIdx: tokIdx,
		Left:     uint32(rhs),
		Right:    uint32(inBody),
		Data:     uint64(nameID),
	}
	if patNode != ast.NoNode {
		node.Op = 1
		start, _ := p.Arena.AddExtra([]ast.NodeID{patNode, rhs, inBody})
		node.Left = start
	}
	return p.Arena.Add(node)
}

func (p *Parser) parseRecAndBinding() ast.NodeID {
	tokIdx := uint32(p.pos)
	nameTok := p.expect(token.TokenIdent)
	nameID := p.internTok(nameTok)

	var params []ast.NodeID
	for !p.isEOF() && p.peek().Kind != token.TokenAssign && p.peek().Kind != token.TokenColon {
		param := p.parseParamPattern()
		if param == ast.NoNode {
			break
		}
		params = append(params, param)
	}
	if p.match(token.TokenColon) {
		_ = p.parseTypeExpr()
	}
	p.expect(token.TokenAssign)
	rhs := p.parseBlockBody()
	rhs = p.wrapLambdaParams(tokIdx, params, rhs)

	var inBody ast.NodeID
	if p.match(token.TokenAnd) {
		inBody = p.parseRecAndBinding()
	} else if p.match(token.TokenIn) {
		inBody = p.parseBlockBody()
	}
	return p.Arena.Add(ast.Node{
		Kind:     ast.KindLetRec,
		Flags:    ast.FlagRec,
		TokenIdx: tokIdx,
		Left:     uint32(rhs),
		Right:    uint32(inBody),
		Data:     uint64(nameID),
	})
}

func (p *Parser) wrapLambdaParams(tokIdx uint32, params []ast.NodeID, body ast.NodeID) ast.NodeID {
	for i := len(params) - 1; i >= 0; i-- {
		paramNode := params[i]
		pn := *p.Arena.Get(paramNode)
		if pn.Kind == ast.KindPatTuple {
			synthSID := p.Arena.Strings.InternString(fmt.Sprintf("__tup_arg_%d_%d", tokIdx, i))
			identNode := p.Arena.Add(ast.Node{
				Kind:     ast.KindIdent,
				TokenIdx: tokIdx,
				Data:     uint64(synthSID),
			})
			start, _ := p.Arena.AddExtra([]ast.NodeID{paramNode, identNode, body})
			body = p.Arena.Add(ast.Node{
				Kind:     ast.KindLet,
				Op:       1,
				TokenIdx: tokIdx,
				Left:     start,
			})
			paramNode = identNode
			pn = *p.Arena.Get(paramNode)
		}
		body = p.Arena.Add(ast.Node{
			Kind:     ast.KindLambda,
			TokenIdx: tokIdx,
			Left:     uint32(paramNode),
			Right:    uint32(body),
			Data:     pn.Data,
		})
	}
	return body
}

func (p *Parser) parseParamPattern() ast.NodeID {
	tk := p.peek()
	switch tk.Kind {
	case token.TokenIdent:
		p.advance()
		sid := p.internTok(tk)
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindIdent,
			TokenIdx: uint32(p.pos - 1),
			Data:     uint64(sid),
		})
	case token.TokenUnderscore:
		p.advance()
		sid := p.Arena.Strings.InternString("_")
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindIdent,
			TokenIdx: uint32(p.pos - 1),
			Data:     uint64(sid),
		})
	case token.TokenUnit:
		p.advance()
		sid := p.Arena.Strings.InternString("()")
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindUnit,
			TokenIdx: uint32(p.pos - 1),
			Data:     uint64(sid),
		})
	case token.TokenLParen:
		if p.peekAt(1).Kind == token.TokenRParen {
			p.advance()
			p.advance()
			return p.Arena.Add(ast.Node{Kind: ast.KindUnit})
		}
		if p.peekAt(1).Kind == token.TokenIdent && (p.peekAt(2).Kind == token.TokenColon || p.peekAt(2).Kind == token.TokenRParen) {
			p.advance()
			idTok := p.advance()
			sid := p.internTok(idTok)
			identNode := p.Arena.Add(ast.Node{
				Kind:     ast.KindIdent,
				TokenIdx: uint32(p.pos - 1),
				Data:     uint64(sid),
			})
			if p.match(token.TokenColon) {
				tyNode := p.parseTypeExpr()
				p.expect(token.TokenRParen)
				return p.Arena.Add(ast.Node{
					Kind:  ast.KindTypeAnnot,
					Left:  uint32(identNode),
					Right: uint32(tyNode),
					Data:  uint64(sid),
				})
			}
			p.expect(token.TokenRParen)
			return identNode
		}
		return p.parseAtomicPattern()
	default:
		return ast.NoNode
	}
}

// Helper methods for token navigation and interning.

func (p *Parser) peek() token.Token {
	if p.pos >= len(p.tokens) {
		return token.Token{Kind: token.TokenEOF}
	}
	return p.tokens[p.pos]
}

func (p *Parser) peekAt(offset int) token.Token {
	idx := p.pos + offset
	if idx < 0 || idx >= len(p.tokens) {
		return token.Token{Kind: token.TokenEOF}
	}
	return p.tokens[idx]
}

func (p *Parser) advance() token.Token {
	tok := p.peek()
	if p.pos < len(p.tokens) {
		p.pos++
	}
	return tok
}

func (p *Parser) isEOF() bool {
	return p.peek().Kind == token.TokenEOF
}

func (p *Parser) match(kind token.TokenKind) bool {
	if p.peek().Kind == kind {
		p.advance()
		return true
	}
	return false
}

func (p *Parser) expect(kind token.TokenKind) token.Token {
	tok := p.peek()
	if tok.Kind == kind {
		p.advance()
		return tok
	}
	p.errorf(tok, "expected token %s, got %s (%q)", kind, tok.Kind, tok.Slice(p.src))
	p.advance()
	return tok
}

func (p *Parser) skipSeparators() {
	for {
		k := p.peek().Kind
		if k == token.TokenSemi || k == token.TokenSemicolon {
			p.advance()
			continue
		}
		break
	}
}

func (p *Parser) internTok(tok token.Token) token.StringID {
	return p.Arena.Strings.Intern(tok.Slice(p.src))
}

func (p *Parser) parseIntTok(tok token.Token) int64 {
	v, err := strconv.ParseInt(string(tok.Slice(p.src)), 10, 64)
	if err != nil {
		p.errorf(tok, "invalid integer literal: %v", err)
		return 0
	}
	return v
}

func (p *Parser) parseFloatTok(tok token.Token) float64 {
	v, err := strconv.ParseFloat(string(tok.Slice(p.src)), 64)
	if err != nil {
		p.errorf(tok, "invalid float literal: %v", err)
		return 0
	}
	return v
}

func (p *Parser) errorf(tok token.Token, format string, args ...any) {
	msg := fmt.Sprintf("line %d:%d: %s", tok.Line, tok.Col, fmt.Sprintf(format, args...))
	p.Errors = append(p.Errors, msg)
}
