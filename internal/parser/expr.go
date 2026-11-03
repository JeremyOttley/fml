package parser

import (
	"math"

	"fml/internal/ast"
	"fml/internal/token"
)

// Precedence levels for Pratt expression parsing.
//
// Jaime says: Don't write ten levels of mutually recursive parseTerm,
// parseFactor, parseComparison functions. A single Pratt loop with a binding
// power table handles every operator from `<-` to function application in one
// screen of code.
const (
	precNone       = 0
	precMutate     = 10 // <-
	precPipe       = 20 // |>, <|
	precCompose    = 25 // >>, <<
	precOr         = 30 // ||
	precAnd        = 35 // &&
	precCompare    = 40 // =, ==, <>, !=, <, <=, >, >=
	precConsAppend = 50 // ::, @, ..
	precSum        = 60 // +, -
	precProduct    = 70 // *, /, %
	precPower      = 75 // ^
	precPrefix     = 80 // unary -, not
	precApply      = 90 // function application f x
	precDot        = 95 // record/member field access r.Field
)

func (p *Parser) parseExpr(minPrec int) ast.NodeID {
	lhs := p.parsePrefix()
	if lhs == ast.NoNode {
		return ast.NoNode
	}

	for !p.isEOF() {
		tk := p.peek()

		// Field access: expr.Field (and optional method invocation)
		if tk.Kind == token.TokenDot && precDot >= minPrec {
			p.advance()
			fieldTok := p.expect(token.TokenIdent)
			fieldID := p.internTok(fieldTok)
			lhs = p.Arena.Add(ast.Node{
				Kind:     ast.KindFieldAccess,
				TokenIdx: uint32(p.pos - 1),
				Left:     uint32(lhs),
				Data:     uint64(fieldID),
			})
			continue
		}

		// Unit-of-measure literal suffix: 5.0<kg> or 10<m/s>
		if (p.Arena.Get(lhs).Kind == ast.KindFloat || p.Arena.Get(lhs).Kind == ast.KindInt) &&
			tk.Kind == token.TokenLt && p.looksLikeMeasureSuffix() {
			p.advance() // consume '<'
			unitNode := p.parseMeasureUnitExpr()
			p.expect(token.TokenGt)
			lhs = p.Arena.Add(ast.Node{
				Kind:  ast.KindMeasure,
				Left:  uint32(lhs),
				Right: uint32(unitNode),
			})
			continue
		}

		// Juxtaposition function application: `f x`
		if precApply > minPrec && p.canStartApplicationArg(tk.Kind) {
			arg := p.parsePrimary()
			lhs = p.Arena.Add(ast.Node{
				Kind:     ast.KindApply,
				TokenIdx: uint32(p.pos - 1),
				Left:     uint32(lhs),
				Right:    uint32(arg),
			})
			continue
		}

		prec, rightAssoc := infixPrecedence(tk.Kind)
		if prec == precNone || prec <= minPrec {
			break
		}

		opTok := p.advance()
		nextMinPrec := prec
		if rightAssoc {
			nextMinPrec = prec - 1
		}
		rhs := p.parseExpr(nextMinPrec)

		switch opTok.Kind {
		case token.TokenMutate:
			var targetName uint64
			if p.Arena.Get(lhs).Kind == ast.KindIdent {
				targetName = p.Arena.Get(lhs).Data
			}
			lhs = p.Arena.Add(ast.Node{
				Kind:     ast.KindMutate,
				TokenIdx: uint32(p.pos - 1),
				Left:     uint32(lhs),
				Right:    uint32(rhs),
				Data:     targetName,
			})
		case token.TokenPipeRight:
			lhs = p.Arena.Add(ast.Node{
				Kind:     ast.KindPipeRight,
				TokenIdx: uint32(p.pos - 1),
				Left:     uint32(lhs),
				Right:    uint32(rhs),
			})
		case token.TokenPipeLeft:
			lhs = p.Arena.Add(ast.Node{
				Kind:     ast.KindPipeLeft,
				Flags:    ast.FlagBackward,
				TokenIdx: uint32(p.pos - 1),
				Left:     uint32(lhs),
				Right:    uint32(rhs),
			})
		case token.TokenComposeRight:
			lhs = p.Arena.Add(ast.Node{
				Kind:     ast.KindCompose,
				TokenIdx: uint32(p.pos - 1),
				Left:     uint32(lhs),
				Right:    uint32(rhs),
			})
		case token.TokenComposeLeft:
			lhs = p.Arena.Add(ast.Node{
				Kind:     ast.KindCompose,
				Flags:    ast.FlagBackward,
				TokenIdx: uint32(p.pos - 1),
				Left:     uint32(lhs),
				Right:    uint32(rhs),
			})
		case token.TokenRange:
			lhs = p.Arena.Add(ast.Node{
				Kind:     ast.KindRange,
				TokenIdx: uint32(p.pos - 1),
				Left:     uint32(lhs),
				Right:    uint32(rhs),
			})
		default:
			lhs = p.Arena.Add(ast.Node{
				Kind:     ast.KindInfix,
				Op:       uint16(opTok.Kind),
				TokenIdx: uint32(p.pos - 1),
				Left:     uint32(lhs),
				Right:    uint32(rhs),
			})
		}
	}

	return lhs
}

func (p *Parser) parsePrefix() ast.NodeID {
	tk := p.peek()
	switch tk.Kind {
	case token.TokenLet, token.TokenUse:
		return p.parseLetOrUse()
	case token.TokenLetBang, token.TokenDoBang, token.TokenUseBang:
		return p.parseBangBinding()
	case token.TokenReturn, token.TokenReturnBang, token.TokenYield:
		return p.parseReturnOrYield()
	case token.TokenFun:
		return p.parseLambda()
	case token.TokenFunction:
		return p.parseFunctionMatchLambda()
	case token.TokenMatch:
		return p.parseMatch()
	case token.TokenIf:
		return p.parseIf()
	case token.TokenFor:
		return p.parseFor()
	case token.TokenWhile:
		return p.parseWhile()
	case token.TokenTry:
		return p.parseTry()
	case token.TokenRaise:
		p.advance()
		arg := p.parseExpr(precPrefix)
		return p.Arena.Add(ast.Node{
			Kind: ast.KindRaise,
			Left: uint32(arg),
		})
	case token.TokenSeq, token.TokenAsync, token.TokenTask:
		if p.peekAt(1).Kind == token.TokenLBrace {
			return p.parseComputationExpr()
		}
		return p.parsePrimary()
	case token.TokenMinus, token.TokenNot:
		opTok := p.advance()
		operand := p.parseExpr(precPrefix)
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindPrefix,
			Op:       uint16(opTok.Kind),
			TokenIdx: uint32(p.pos - 1),
			Left:     uint32(operand),
		})
	case token.TokenSome, token.TokenNone:
		return p.parseOptionLiteral()
	case token.TokenOk, token.TokenError:
		return p.parseResultLiteral()
	default:
		return p.parsePrimary()
	}
}

func (p *Parser) parsePrimary() ast.NodeID {
	tk := p.peek()
	tokIdx := uint32(p.pos)

	switch tk.Kind {
	case token.TokenInt:
		p.advance()
		val := p.parseIntTok(tk)
		node := p.Arena.Add(ast.Node{
			Kind:     ast.KindInt,
			TokenIdx: tokIdx,
			Data:     uint64(val),
		})
		if p.peek().Kind == token.TokenLt && p.looksLikeMeasureSuffix() {
			p.advance()
			unitNode := p.parseMeasureUnitExpr()
			p.expect(token.TokenGt)
			node = p.Arena.Add(ast.Node{
				Kind:  ast.KindMeasure,
				Left:  uint32(node),
				Right: uint32(unitNode),
			})
		}
		return node
	case token.TokenFloat:
		p.advance()
		val := p.parseFloatTok(tk)
		node := p.Arena.Add(ast.Node{
			Kind:     ast.KindFloat,
			TokenIdx: tokIdx,
			Data:     math.Float64bits(val),
		})
		if p.peek().Kind == token.TokenLt && p.looksLikeMeasureSuffix() {
			p.advance()
			unitNode := p.parseMeasureUnitExpr()
			p.expect(token.TokenGt)
			node = p.Arena.Add(ast.Node{
				Kind:  ast.KindMeasure,
				Left:  uint32(node),
				Right: uint32(unitNode),
			})
		}
		return node
	case token.TokenString:
		p.advance()
		raw := tk.Slice(p.src)
		if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
			raw = raw[1 : len(raw)-1]
		}
		raw = unescapeStringBytes(raw)
		sid := p.Arena.Strings.Intern(raw)
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindString,
			TokenIdx: tokIdx,
			Data:     uint64(sid),
		})
	case token.TokenTrue:
		p.advance()
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindBool,
			TokenIdx: tokIdx,
			Data:     1,
		})
	case token.TokenFalse:
		p.advance()
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindBool,
			TokenIdx: tokIdx,
			Data:     0,
		})
	case token.TokenUnit:
		p.advance()
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindUnit,
			TokenIdx: tokIdx,
		})
	case token.TokenNone:
		p.advance()
		return p.Arena.Add(ast.Node{
			Kind:     ast.KindOption,
			TokenIdx: tokIdx,
			Data:     0,
		})
	case token.TokenIdent, token.TokenSeq, token.TokenAsync, token.TokenTask:
		// Check for custom computation expression `builder { ... }`
		if p.peekAt(1).Kind == token.TokenLBrace && p.looksLikeCompExprBrace() {
			return p.parseComputationExpr()
		}
		p.advance()
		sid := p.internTok(tk)
		node := p.Arena.Add(ast.Node{
			Kind:     ast.KindIdent,
			TokenIdx: tokIdx,
			Data:     uint64(sid),
		})
		for p.peek().Kind == token.TokenDot {
			p.advance()
			fTok := p.expect(token.TokenIdent)
			fID := p.internTok(fTok)
			node = p.Arena.Add(ast.Node{
				Kind:     ast.KindFieldAccess,
				TokenIdx: uint32(p.pos - 1),
				Left:     uint32(node),
				Data:     uint64(fID),
			})
		}
		return node
	case token.TokenLParen:
		return p.parseParenOrTuple()
	case token.TokenLBrack:
		return p.parseListLiteral()
	case token.TokenLArray:
		return p.parseArrayLiteral()
	case token.TokenLBrace:
		return p.parseBraceRecordOrObjExpr()
	case token.TokenLQuote:
		return p.parseQuotation()
	default:
		p.errorf(tk, "unexpected token in expression: %s (%q)", tk.Kind, tk.Slice(p.src))
		p.advance()
		return ast.NoNode
	}
}

func (p *Parser) parseLambda() ast.NodeID {
	tokIdx := uint32(p.pos)
	p.expect(token.TokenFun)
	var params []ast.NodeID
	for !p.isEOF() && p.peek().Kind != token.TokenArrow {
		param := p.parseParamPattern()
		if param == ast.NoNode {
			break
		}
		params = append(params, param)
	}
	p.expect(token.TokenArrow)
	body := p.parseBlockBody()

	if len(params) == 0 {
		unitNode := p.Arena.Add(ast.Node{Kind: ast.KindUnit})
		params = append(params, unitNode)
	}
	return p.wrapLambdaParams(tokIdx, params, body)
}

func (p *Parser) canStartApplicationArg(k token.TokenKind) bool {
	switch k {
	case token.TokenIdent, token.TokenInt, token.TokenFloat, token.TokenString,
		token.TokenTrue, token.TokenFalse, token.TokenUnit, token.TokenNone,
		token.TokenLParen, token.TokenLBrack, token.TokenLArray, token.TokenLBrace,
		token.TokenLQuote:
		return true
	default:
		return false
	}
}

func infixPrecedence(k token.TokenKind) (prec int, rightAssoc bool) {
	switch k {
	case token.TokenMutate:
		return precMutate, true
	case token.TokenPipeRight:
		return precPipe, false
	case token.TokenPipeLeft:
		return precPipe, true
	case token.TokenComposeRight, token.TokenComposeLeft:
		return precCompose, false
	case token.TokenOrOr:
		return precOr, false
	case token.TokenAndAnd:
		return precAnd, false
	case token.TokenAssign, token.TokenEq, token.TokenNeq,
		token.TokenLt, token.TokenLe, token.TokenGt, token.TokenGe:
		return precCompare, false
	case token.TokenCons, token.TokenAppend, token.TokenRange:
		return precConsAppend, true
	case token.TokenPlus, token.TokenMinus:
		return precSum, false
	case token.TokenStar, token.TokenSlash, token.TokenPercent:
		return precProduct, false
	case token.TokenCaret:
		return precPower, true
	default:
		return precNone, false
	}
}

func unescapeStringBytes(raw []byte) []byte {
	hasEscape := false
	for _, b := range raw {
		if b == '\\' {
			hasEscape = true
			break
		}
	}
	if !hasEscape {
		return raw
	}
	out := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\\' && i+1 < len(raw) {
			i++
			switch raw[i] {
			case 'n':
				out = append(out, '\n')
			case 't':
				out = append(out, '\t')
			case 'r':
				out = append(out, '\r')
			case '"':
				out = append(out, '"')
			case '\\':
				out = append(out, '\\')
			default:
				out = append(out, raw[i])
			}
		} else {
			out = append(out, raw[i])
		}
	}
	return out
}

