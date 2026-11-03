package ir

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"fml/internal/ast"
	"fml/internal/token"
	"fml/internal/types"
)

// Lowerer translates a typed ast.Arena into a flat 10-opcode ir.Program.
//
// Jaime says: Lowering is where syntax dies and execution begins. Pipelines
// become direct function applications. Composition becomes a flat lambda.
// Nested `let` bindings are lifted into linear sequences. Units of measure
// disappear completely.
type Lowerer struct {
	AST            *ast.Arena
	Types          *types.TypeArena
	Prog           *Program
	DUConstructors    map[token.StringID]bool // true if constructor takes a payload
	ActivePatFns      map[token.StringID]token.StringID
	PartialActivePats map[token.StringID]token.StringID
	ModuleExports     map[token.StringID][]token.StringID
	FunctorExports map[token.StringID][]token.StringID
	BaseDir        string
	tempCounter    int
}

// Lower lowers the AST rooted at root into a flat ir.Program.
func Lower(astArena *ast.Arena, typeArena *types.TypeArena, root ast.NodeID) *Program {
	return LowerInDir(astArena, typeArena, root, "")
}

// LowerInDir lowers the AST rooted at root with baseDir for relative type provider schemas.
func LowerInDir(astArena *ast.Arena, typeArena *types.TypeArena, root ast.NodeID, baseDir string) *Program {
	if typeArena != nil {
		typeArena.EraseMeasures()
	}
	prog := NewProgram(len(astArena.Nodes)*2+32, astArena.Strings)
	l := &Lowerer{
		AST:            astArena,
		Types:          typeArena,
		Prog:           prog,
		DUConstructors:    make(map[token.StringID]bool),
		ActivePatFns:      make(map[token.StringID]token.StringID),
		PartialActivePats: make(map[token.StringID]token.StringID),
		ModuleExports:     make(map[token.StringID][]token.StringID),
		FunctorExports: make(map[token.StringID][]token.StringID),
		BaseDir:        baseDir,
	}
	l.registerBuiltinDUs()
	prog.Root = l.lowerNode(root, false)
	ConvertClosures(prog)
	return prog
}

func (l *Lowerer) registerBuiltinDUs() {
	s := l.AST.Strings
	l.DUConstructors[s.InternString("QUnit")] = false
	l.DUConstructors[s.InternString("QInt")] = true
	l.DUConstructors[s.InternString("QFloat")] = true
	l.DUConstructors[s.InternString("QBool")] = true
	l.DUConstructors[s.InternString("QString")] = true
	l.DUConstructors[s.InternString("QVar")] = true
	l.DUConstructors[s.InternString("QInfix")] = true
	l.DUConstructors[s.InternString("QApp")] = true
	l.DUConstructors[s.InternString("QLambda")] = true
	l.DUConstructors[s.InternString("QLet")] = true
}

func (l *Lowerer) freshTemp(prefix string) token.StringID {
	l.tempCounter++
	return l.AST.Strings.InternString(fmt.Sprintf("__%s_%d", prefix, l.tempCounter))
}

func (l *Lowerer) lowerNode(id ast.NodeID, tailPos bool) InstrID {
	if id == ast.NoNode {
		return l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstUnit})
	}
	n := *l.AST.Get(id)

	switch n.Kind {
	case ast.KindProgram, ast.KindBlock:
		children := l.AST.ExtraSlice(n.Left, n.Right)
		return l.lowerBlockSlice(children, tailPos)

	case ast.KindUnit:
		return l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstUnit})
	case ast.KindBool:
		return l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstBool, Data: n.Data})
	case ast.KindInt:
		return l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstInt, Data: n.Data})
	case ast.KindFloat:
		return l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstFloat, Data: n.Data})
	case ast.KindString:
		return l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstString, Data: n.Data})

	case ast.KindIdent:
		name := token.StringID(n.Data)
		if hasPayload, isDU := l.DUConstructors[name]; isDU && !hasPayload {
			return l.Prog.Emit(IRInstr{
				Kind:  IRAlloc,
				SubOp: AllocADT,
				Arg0:  uint32(NoInstr),
				Data:  uint64(name),
			})
		}
		return l.Prog.Emit(IRInstr{Kind: IRVar, Data: n.Data})

	case ast.KindMeasure:
		if n.Left == 0 {
			return l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstUnit})
		}
		// Erase unit-of-measure wrapper and lower underlying literal directly
		return l.lowerNode(ast.NodeID(n.Left), tailPos)

	case ast.KindTypeAnnot, ast.KindAttr:
		if n.Left != 0 {
			return l.lowerNode(ast.NodeID(n.Left), tailPos)
		}
		return l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstUnit})

	case ast.KindQuote:
		return l.lowerQuote(ast.NodeID(n.Left))

	case ast.KindLet, ast.KindLetMutable, ast.KindLetRec, ast.KindUse:
		return l.lowerLet(n, tailPos)

	case ast.KindMutate:
		valID := l.lowerNode(ast.NodeID(n.Right), false)
		return l.Prog.Emit(IRInstr{
			Kind:  IRStore,
			SubOp: StoreVar,
			Arg0:  uint32(NoInstr),
			Arg1:  uint32(valID),
			Data:  n.Data,
		})

	case ast.KindLambda:
		paramName := token.StringID(n.Data)
		bodyID := l.lowerNode(ast.NodeID(n.Right), true)
		return l.Prog.Emit(IRInstr{
			Kind: IRFn,
			Arg0: uint32(bodyID),
			Arg1: uint32(NoInstr),
			Data: uint64(paramName),
		})

	case ast.KindApply:
		// Check if applying a DU constructor with payload: `Circle 5.0`
		fnAst := l.AST.Get(ast.NodeID(n.Left))
		if fnAst.Kind == ast.KindIdent {
			cName := token.StringID(fnAst.Data)
			if hasPayload, isDU := l.DUConstructors[cName]; isDU && hasPayload {
				argID := l.lowerNode(ast.NodeID(n.Right), false)
				return l.Prog.Emit(IRInstr{
					Kind:  IRAlloc,
					SubOp: AllocADT,
					Arg0:  uint32(argID),
					Data:  uint64(cName),
				})
			}
		}
		fnID := l.lowerNode(ast.NodeID(n.Left), false)
		argID := l.lowerNode(ast.NodeID(n.Right), false)
		var flags uint8
		if tailPos {
			flags |= IRFlagTailCall
		}
		return l.Prog.Emit(IRInstr{
			Kind:  IRApp,
			Flags: flags,
			Arg0:  uint32(fnID),
			Arg1:  uint32(argID),
		})

	case ast.KindPipeRight:
		// Desugar `x |> f` into `f x`
		argID := l.lowerNode(ast.NodeID(n.Left), false)
		fnID := l.lowerNode(ast.NodeID(n.Right), false)
		var flags uint8
		if tailPos {
			flags |= IRFlagTailCall
		}
		return l.Prog.Emit(IRInstr{
			Kind:  IRApp,
			Flags: flags,
			Arg0:  uint32(fnID),
			Arg1:  uint32(argID),
		})

	case ast.KindPipeLeft:
		// Desugar `f <| x` into `f x`
		fnID := l.lowerNode(ast.NodeID(n.Left), false)
		argID := l.lowerNode(ast.NodeID(n.Right), false)
		var flags uint8
		if tailPos {
			flags |= IRFlagTailCall
		}
		return l.Prog.Emit(IRInstr{
			Kind:  IRApp,
			Flags: flags,
			Arg0:  uint32(fnID),
			Arg1:  uint32(argID),
		})

	case ast.KindCompose:
		// Desugar `f >> g` into `fun __x -> g (f __x)` (or `f (g __x)` for `<<`)
		paramSym := l.freshTemp("comp")
		fID := l.lowerNode(ast.NodeID(n.Left), false)
		gID := l.lowerNode(ast.NodeID(n.Right), false)
		varRef := l.Prog.Emit(IRInstr{Kind: IRVar, Data: uint64(paramSym)})
		var innerApp, outerApp InstrID
		if n.Flags&ast.FlagBackward != 0 {
			innerApp = l.Prog.Emit(IRInstr{Kind: IRApp, Arg0: uint32(gID), Arg1: uint32(varRef)})
			outerApp = l.Prog.Emit(IRInstr{Kind: IRApp, Flags: IRFlagTailCall, Arg0: uint32(fID), Arg1: uint32(innerApp)})
		} else {
			innerApp = l.Prog.Emit(IRInstr{Kind: IRApp, Arg0: uint32(fID), Arg1: uint32(varRef)})
			outerApp = l.Prog.Emit(IRInstr{Kind: IRApp, Flags: IRFlagTailCall, Arg0: uint32(gID), Arg1: uint32(innerApp)})
		}
		return l.Prog.Emit(IRInstr{
			Kind: IRFn,
			Arg0: uint32(outerApp),
			Arg1: uint32(NoInstr),
			Data: uint64(paramSym),
		})

	case ast.KindInfix:
		return l.lowerInfix(n)

	case ast.KindPrefix:
		opID := l.lowerNode(ast.NodeID(n.Left), false)
		sub := PrimNeg
		if token.TokenKind(n.Op) == token.TokenNot {
			sub = PrimNot
		}
		return l.Prog.Emit(IRInstr{
			Kind:  IRPrim,
			SubOp: sub,
			Arg0:  uint32(opID),
			Arg1:  uint32(NoInstr),
		})

	case ast.KindTuple, ast.KindList, ast.KindArray:
		children := l.AST.ExtraSlice(n.Left, n.Right)
		elemIDs := make([]InstrID, len(children))
		for i, ch := range children {
			elemIDs[i] = l.lowerNode(ast.NodeID(ch), false)
		}
		start, count := l.Prog.AddExtra(elemIDs)
		allocKind := AllocTuple
		if n.Kind == ast.KindList {
			allocKind = AllocList
		} else if n.Kind == ast.KindArray {
			allocKind = AllocArray
		}
		return l.Prog.Emit(IRInstr{
			Kind:  IRAlloc,
			SubOp: allocKind,
			Arg0:  start,
			Arg1:  count,
		})

	case ast.KindRange:
		sID := l.lowerNode(ast.NodeID(n.Left), false)
		eID := l.lowerNode(ast.NodeID(n.Right), false)
		return l.Prog.Emit(IRInstr{
			Kind:  IRPrim,
			Flags: n.Flags,
			SubOp: PrimRange,
			Arg0:  uint32(sID),
			Arg1:  uint32(eID),
		})

	case ast.KindOption:
		var tag token.StringID
		var payload InstrID = NoInstr
		if n.Data == 1 {
			tag = l.AST.Strings.InternString("Some")
			payload = l.lowerNode(ast.NodeID(n.Left), false)
		} else {
			tag = l.AST.Strings.InternString("None")
		}
		return l.Prog.Emit(IRInstr{
			Kind:  IRAlloc,
			SubOp: AllocADT,
			Arg0:  uint32(payload),
			Data:  uint64(tag),
		})

	case ast.KindResult:
		var tag token.StringID
		if n.Data == 1 {
			tag = l.AST.Strings.InternString("Ok")
		} else {
			tag = l.AST.Strings.InternString("Error")
		}
		payload := l.lowerNode(ast.NodeID(n.Left), false)
		return l.Prog.Emit(IRInstr{
			Kind:  IRAlloc,
			SubOp: AllocADT,
			Arg0:  uint32(payload),
			Data:  uint64(tag),
		})

	case ast.KindRecordLit:
		var flags uint8
		var aux uint32
		if n.Data != 0 {
			baseID := l.lowerNode(ast.NodeID(n.Data), false)
			flags = 1
			aux = uint32(baseID)
		}
		fields := l.AST.ExtraSlice(n.Left, n.Right)
		var kvPairs []InstrID
		for _, fID := range fields {
			fn := l.AST.Get(ast.NodeID(fID))
			valID := l.lowerNode(ast.NodeID(fn.Left), false)
			kvPairs = append(kvPairs, InstrID(fn.Data), valID)
		}
		start, count := l.Prog.AddExtra(kvPairs)
		return l.Prog.Emit(IRInstr{
			Kind:  IRAlloc,
			SubOp: AllocRecord,
			Flags: flags,
			Arg0:  start,
			Arg1:  count,
			Aux:   aux,
		})

	case ast.KindFieldAccess:
		recvID := l.lowerNode(ast.NodeID(n.Left), false)
		// Also support qualified identifier lookup `Module.member` if receiver is IRVar
		if l.Prog.Instrs[recvID].Kind == IRVar {
			modName := l.AST.Strings.String(token.StringID(l.Prog.Instrs[recvID].Data))
			fieldName := l.AST.Strings.String(token.StringID(n.Data))
			qualSym := l.AST.Strings.InternString(modName + "." + fieldName)
			return l.Prog.Emit(IRInstr{
				Kind:  IRLoad,
				SubOp: LoadRecordField,
				Arg0:  uint32(recvID),
				Aux:   uint32(qualSym),
				Data:  n.Data,
			})
		}
		return l.Prog.Emit(IRInstr{
			Kind:  IRLoad,
			SubOp: LoadRecordField,
			Arg0:  uint32(recvID),
			Data:  n.Data,
		})

	case ast.KindDUType:
		cases := l.AST.ExtraSlice(n.Left, n.Right)
		for _, cID := range cases {
			cn := l.AST.Get(ast.NodeID(cID))
			cName := token.StringID(cn.Data)
			l.DUConstructors[cName] = cn.Left != 0
		}
		return l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstUnit})

	case ast.KindActivePattern:
		items := l.AST.ExtraSlice(n.Left, n.Right)
		if len(items) == 0 {
			return l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstUnit})
		}
		fnAstID := ast.NodeID(items[len(items)-1])
		fnSym := l.freshTemp("active_pat")
		for i := 0; i < len(items)-1; i++ {
			tn := l.AST.Get(ast.NodeID(items[i]))
			tName := token.StringID(tn.Data)
			l.DUConstructors[tName] = false
			if n.Flags&ast.FlagPartialPat != 0 {
				l.PartialActivePats[tName] = fnSym
			} else {
				l.ActivePatFns[tName] = fnSym
			}
		}
		fnInstr := l.lowerNode(fnAstID, false)
		return l.Prog.Emit(IRInstr{
			Kind: IRLet,
			Arg0: uint32(fnInstr),
			Arg1: uint32(NoInstr),
			Data: uint64(fnSym),
		})

	case ast.KindMatch:
		return l.compileMatch(n, tailPos)

	case ast.KindIf:
		// Lower `if cond then t else f` into IRSwitch on boolean tag 1
		condID := l.lowerNode(ast.NodeID(n.Left), false)
		thenID := l.lowerNode(ast.NodeID(n.Right), tailPos)
		var elseID InstrID
		if n.Data != 0 {
			elseID = l.lowerNode(ast.NodeID(n.Data), tailPos)
		} else {
			elseID = l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstUnit})
		}
		start, count := l.Prog.AddSwitchCases([]SwitchCase{{Tag: 1, Target: thenID}})
		return l.Prog.Emit(IRInstr{
			Kind: IRSwitch,
			Arg0: uint32(condID),
			Arg1: start,
			Aux:  uint32(elseID),
			Data: uint64(count),
		})

	case ast.KindWhile:
		condID := l.lowerNode(ast.NodeID(n.Left), false)
		bodyID := l.lowerNode(ast.NodeID(n.Right), false)
		return l.Prog.Emit(IRInstr{
			Kind:  IRPrim,
			SubOp: PrimWhile,
			Arg0:  uint32(condID),
			Arg1:  uint32(bodyID),
		})

	case ast.KindForIn:
		collID := l.lowerNode(ast.NodeID(n.Left), false)
		bodyID := l.lowerNode(ast.NodeID(n.Right), false)
		return l.Prog.Emit(IRInstr{
			Kind:  IRPrim,
			SubOp: PrimForIn,
			Arg0:  uint32(collID),
			Arg1:  uint32(bodyID),
			Data:  n.Data,
		})

	case ast.KindForTo:
		rangeID := l.lowerNode(ast.NodeID(n.Left), false)
		bodyID := l.lowerNode(ast.NodeID(n.Right), false)
		return l.Prog.Emit(IRInstr{
			Kind:  IRPrim,
			Flags: n.Flags,
			SubOp: PrimForTo,
			Arg0:  uint32(rangeID),
			Arg1:  uint32(bodyID),
			Data:  n.Data,
		})

	case ast.KindTryFinally:
		bodyID := l.lowerNode(ast.NodeID(n.Left), false)
		cleanupID := l.lowerNode(ast.NodeID(n.Right), false)
		return l.Prog.Emit(IRInstr{
			Kind:  IRPrim,
			SubOp: PrimTryFinally,
			Arg0:  uint32(bodyID),
			Arg1:  uint32(cleanupID),
		})

	case ast.KindTryWith:
		bodyID := l.lowerNode(ast.NodeID(n.Left), false)
		armsBlock := l.AST.Get(ast.NodeID(n.Right))
		arms := l.AST.ExtraSlice(armsBlock.Left, armsBlock.Right)
		var handlerID InstrID
		if len(arms) > 0 {
			firstArm := l.AST.Get(ast.NodeID(arms[0]))
			handlerID = l.lowerNode(ast.NodeID(firstArm.Right), false)
		} else {
			handlerID = l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstUnit})
		}
		return l.Prog.Emit(IRInstr{
			Kind:  IRPrim,
			SubOp: PrimTryWith,
			Arg0:  uint32(bodyID),
			Arg1:  uint32(handlerID),
		})

	case ast.KindRaise:
		argID := l.lowerNode(ast.NodeID(n.Left), false)
		return l.Prog.Emit(IRInstr{
			Kind:  IRPrim,
			SubOp: PrimRaise,
			Arg0:  uint32(argID),
		})

	case ast.KindSeq:
		bodyID := l.lowerNode(ast.NodeID(n.Left), false)
		return l.Prog.Emit(IRInstr{
			Kind:  IRPrim,
			SubOp: PrimSeqGen,
			Arg0:  uint32(bodyID),
		})

	case ast.KindCompExpr:
		return l.lowerCompExpr(n, tailPos)

	case ast.KindBindBang:
		// Desugar standalone/async `let! x = rhs`, `do! rhs`, `use! x = rhs` into IRLet
		valID := l.lowerNode(ast.NodeID(n.Left), false)
		var bodyID InstrID = NoInstr
		if n.Right != 0 {
			bodyID = l.lowerNode(ast.NodeID(n.Right), tailPos)
		}
		var flags uint8
		if n.Flags&ast.FlagUse != 0 {
			flags |= IRFlagDispose
		}
		return l.Prog.Emit(IRInstr{
			Kind:  IRLet,
			Flags: flags,
			Arg0:  uint32(valID),
			Arg1:  uint32(bodyID),
			Data:  n.Data,
		})

	case ast.KindReturn:
		return l.lowerNode(ast.NodeID(n.Left), tailPos)

	case ast.KindModule, ast.KindFunctor:
		return l.lowerModuleOrFunctor(n)

	case ast.KindOpen:
		return l.lowerOpen(n)

	case ast.KindTypeProvider:
		return l.lowerTypeProvider(n)

	case ast.KindClass:
		return l.lowerClass(n)

	case ast.KindObjExpr:
		return l.lowerObjExpr(n)

	default:
		return l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstUnit})
	}
}

func (l *Lowerer) lowerCompExpr(n ast.Node, tailPos bool) InstrID {
	builderSym := token.StringID(n.Data)
	if builderSym == token.NoString {
		return l.lowerNode(ast.NodeID(n.Left), tailPos)
	}
	bodyNode := *l.AST.Get(ast.NodeID(n.Left))
	var stmts []uint32
	if bodyNode.Kind == ast.KindBlock {
		orig := l.AST.ExtraSlice(bodyNode.Left, bodyNode.Right)
		stmts = make([]uint32, len(orig))
		copy(stmts, orig)
	} else {
		stmts = []uint32{n.Left}
	}
	return l.lowerCompStmts(builderSym, stmts, tailPos)
}

func (l *Lowerer) lowerCompStmts(builderSym token.StringID, stmts []uint32, tailPos bool) InstrID {
	if len(stmts) == 0 {
		zeroFn := l.emitBuilderMethod(builderSym, "Zero")
		unitArg := l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstUnit})
		return l.Prog.Emit(IRInstr{
			Kind:  IRApp,
			Flags: tailFlags(tailPos),
			Arg0:  uint32(zeroFn),
			Arg1:  uint32(unitArg),
		})
	}

	firstID := ast.NodeID(stmts[0])
	first := *l.AST.Get(firstID)
	rest := stmts[1:]

	switch first.Kind {
	case ast.KindBindBang:
		if first.Right != 0 {
			rest = append([]uint32{first.Right}, rest...)
		}
		rhsID := l.lowerNode(ast.NodeID(first.Left), false)
		paramSym := token.StringID(first.Data)
		if paramSym == token.NoString {
			paramSym = l.freshTemp("do_bang")
		}
		contBody := l.lowerCompStmts(builderSym, rest, true)
		contFn := l.Prog.Emit(IRInstr{
			Kind: IRFn,
			Arg0: uint32(contBody),
			Arg1: uint32(NoInstr),
			Data: uint64(paramSym),
		})
		bindFn := l.emitBuilderMethod(builderSym, "Bind")
		app1 := l.Prog.Emit(IRInstr{
			Kind: IRApp,
			Arg0: uint32(bindFn),
			Arg1: uint32(rhsID),
		})
		return l.Prog.Emit(IRInstr{
			Kind:  IRApp,
			Flags: tailFlags(tailPos),
			Arg0:  uint32(app1),
			Arg1:  uint32(contFn),
		})

	case ast.KindReturn:
		valID := l.lowerNode(ast.NodeID(first.Left), false)
		method := "Return"
		if first.Flags&ast.FlagBang != 0 {
			method = "ReturnFrom"
		}
		retFn := l.emitBuilderMethod(builderSym, method)
		return l.Prog.Emit(IRInstr{
			Kind:  IRApp,
			Flags: tailFlags(tailPos),
			Arg0:  uint32(retFn),
			Arg1:  uint32(valID),
		})

	case ast.KindLet, ast.KindLetMutable, ast.KindLetRec, ast.KindUse:
		letInstr := l.lowerLet(first, len(rest) == 0 && tailPos)
		if len(rest) == 0 {
			return letInstr
		}
		restInstr := l.lowerCompStmts(builderSym, rest, tailPos)
		start, count := l.Prog.AddExtra([]InstrID{letInstr, restInstr})
		return l.Prog.Emit(IRInstr{
			Kind:  IRPrim,
			SubOp: PrimSeqBlock,
			Arg0:  start,
			Arg1:  count,
		})

	default:
		firstInstr := l.lowerNode(firstID, len(rest) == 0 && tailPos)
		if len(rest) == 0 {
			return firstInstr
		}
		restInstr := l.lowerCompStmts(builderSym, rest, tailPos)
		start, count := l.Prog.AddExtra([]InstrID{firstInstr, restInstr})
		return l.Prog.Emit(IRInstr{
			Kind:  IRPrim,
			SubOp: PrimSeqBlock,
			Arg0:  start,
			Arg1:  count,
		})
	}
}

func (l *Lowerer) emitBuilderMethod(builderSym token.StringID, method string) InstrID {
	recvID := l.Prog.Emit(IRInstr{Kind: IRVar, Data: uint64(builderSym)})
	bName := l.AST.Strings.String(builderSym)
	fieldSym := l.AST.Strings.InternString(method)
	qualSym := l.AST.Strings.InternString(bName + "." + method)
	return l.Prog.Emit(IRInstr{
		Kind:  IRLoad,
		SubOp: LoadRecordField,
		Arg0:  uint32(recvID),
		Aux:   uint32(qualSym),
		Data:  uint64(fieldSym),
	})
}

func tailFlags(tailPos bool) uint8 {
	if tailPos {
		return IRFlagTailCall
	}
	return 0
}

func (l *Lowerer) lowerOpen(n ast.Node) InstrID {
	var seq []InstrID
	if n.Left != 0 {
		seq = append(seq, l.lowerNode(ast.NodeID(n.Left), false))
	}
	modName := token.StringID(n.Data)
	for _, sym := range l.ModuleExports[modName] {
		modVar := l.Prog.Emit(IRInstr{Kind: IRVar, Data: uint64(modName)})
		loadField := l.Prog.Emit(IRInstr{
			Kind:  IRLoad,
			SubOp: LoadRecordField,
			Arg0:  uint32(modVar),
			Data:  uint64(sym),
		})
		seq = append(seq, l.Prog.Emit(IRInstr{
			Kind: IRLet,
			Arg0: uint32(loadField),
			Arg1: uint32(NoInstr),
			Data: uint64(sym),
		}))
	}
	if len(seq) == 0 {
		return l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstUnit})
	}
	if len(seq) == 1 {
		return seq[0]
	}
	start, count := l.Prog.AddExtra(seq)
	return l.Prog.Emit(IRInstr{
		Kind:  IRPrim,
		SubOp: PrimSeqBlock,
		Arg0:  start,
		Arg1:  count,
	})
}

func (l *Lowerer) lowerTypeProvider(n ast.Node) InstrID {
	typeName := token.StringID(n.Data)
	provNode := l.AST.Get(ast.NodeID(n.Left))
	provName := l.AST.Strings.String(token.StringID(provNode.Data))
	argNode := l.AST.Get(ast.NodeID(n.Right))
	argStr := l.AST.Strings.String(token.StringID(argNode.Data))

	rawSchema := strings.TrimSpace(argStr)
	decodeSymName := "Json.decode"
	isInline := strings.HasPrefix(rawSchema, "{")

	switch provName {
	case "CsvProvider":
		decodeSymName = "Csv.decode"
		isInline = strings.Contains(rawSchema, ",") || strings.Contains(rawSchema, "\n")
	case "EnvProvider":
		decodeSymName = "Env.decode"
		isInline = strings.Contains(rawSchema, "=")
	}

	if !isInline {
		path := argStr
		if l.BaseDir != "" && !filepath.IsAbs(path) {
			path = filepath.Join(l.BaseDir, path)
		}
		if b, err := os.ReadFile(path); err == nil {
			rawSchema = string(b)
		}
	}

	s := l.AST.Strings
	decodeSym := s.InternString(decodeSymName)
	fileReadSym := s.InternString("File.readText")

	// Parse = <Provider>.decode
	parseVal := l.Prog.Emit(IRInstr{Kind: IRVar, Data: uint64(decodeSym)})

	// Load = fun __p -> <Provider>.decode (File.readText __p)
	pSym := l.freshTemp("prov_path")
	readFn := l.Prog.Emit(IRInstr{Kind: IRVar, Data: uint64(fileReadSym)})
	pVar := l.Prog.Emit(IRInstr{Kind: IRVar, Data: uint64(pSym)})
	readApp := l.Prog.Emit(IRInstr{Kind: IRApp, Arg0: uint32(readFn), Arg1: uint32(pVar)})
	decFn1 := l.Prog.Emit(IRInstr{Kind: IRVar, Data: uint64(decodeSym)})
	decApp1 := l.Prog.Emit(IRInstr{Kind: IRApp, Flags: IRFlagTailCall, Arg0: uint32(decFn1), Arg1: uint32(readApp)})
	loadVal := l.Prog.Emit(IRInstr{
		Kind: IRFn,
		Arg0: uint32(decApp1),
		Arg1: uint32(NoInstr),
		Data: uint64(pSym),
	})

	// Sample = <Provider>.decode <rawSchema>
	rawSym := s.InternString(rawSchema)
	rawConst := l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstString, Data: uint64(rawSym)})
	decFn2 := l.Prog.Emit(IRInstr{Kind: IRVar, Data: uint64(decodeSym)})
	sampleVal := l.Prog.Emit(IRInstr{Kind: IRApp, Arg0: uint32(decFn2), Arg1: uint32(rawConst)})

	kvPairs := []InstrID{
		InstrID(s.InternString("Parse")), parseVal,
		InstrID(s.InternString("Load")), loadVal,
		InstrID(s.InternString("Sample")), sampleVal,
	}
	start, count := l.Prog.AddExtra(kvPairs)
	recAlloc := l.Prog.Emit(IRInstr{
		Kind:  IRAlloc,
		SubOp: AllocRecord,
		Arg0:  start,
		Arg1:  count,
	})
	return l.Prog.Emit(IRInstr{
		Kind: IRLet,
		Arg0: uint32(recAlloc),
		Arg1: uint32(NoInstr),
		Data: uint64(typeName),
	})
}

func (l *Lowerer) lowerQuote(exprID ast.NodeID) InstrID {
	s := l.AST.Strings
	if exprID == ast.NoNode {
		return l.Prog.Emit(IRInstr{
			Kind:  IRAlloc,
			SubOp: AllocADT,
			Arg0:  uint32(NoInstr),
			Data:  uint64(s.InternString("QUnit")),
		})
	}
	n := *l.AST.Get(exprID)
	switch n.Kind {
	case ast.KindBlock:
		children := l.AST.ExtraSlice(n.Left, n.Right)
		if len(children) == 0 {
			return l.lowerQuote(ast.NoNode)
		}
		if len(children) == 1 {
			return l.lowerQuote(ast.NodeID(children[0]))
		}
		first := *l.AST.Get(ast.NodeID(children[0]))
		if first.Kind == ast.KindLet || first.Kind == ast.KindLetRec || first.Kind == ast.KindLetMutable {
			nameConst := l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstString, Data: first.Data})
			rhsQ := l.lowerQuote(ast.NodeID(first.Left))
			restBlock := l.AST.Add(ast.Node{Kind: ast.KindBlock, Left: n.Left + 1, Right: n.Right - 1})
			bodyQ := l.lowerQuote(restBlock)
			tStart, tCount := l.Prog.AddExtra([]InstrID{nameConst, rhsQ, bodyQ})
			tup := l.Prog.Emit(IRInstr{Kind: IRAlloc, SubOp: AllocTuple, Arg0: tStart, Arg1: tCount})
			return l.Prog.Emit(IRInstr{
				Kind:  IRAlloc,
				SubOp: AllocADT,
				Arg0:  uint32(tup),
				Data:  uint64(s.InternString("QLet")),
			})
		}
		return l.lowerQuote(ast.NodeID(children[len(children)-1]))

	case ast.KindInt:
		c := l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstInt, Data: n.Data})
		return l.Prog.Emit(IRInstr{
			Kind:  IRAlloc,
			SubOp: AllocADT,
			Arg0:  uint32(c),
			Data:  uint64(s.InternString("QInt")),
		})
	case ast.KindFloat:
		c := l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstFloat, Data: n.Data})
		return l.Prog.Emit(IRInstr{
			Kind:  IRAlloc,
			SubOp: AllocADT,
			Arg0:  uint32(c),
			Data:  uint64(s.InternString("QFloat")),
		})
	case ast.KindBool:
		c := l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstBool, Data: n.Data})
		return l.Prog.Emit(IRInstr{
			Kind:  IRAlloc,
			SubOp: AllocADT,
			Arg0:  uint32(c),
			Data:  uint64(s.InternString("QBool")),
		})
	case ast.KindString:
		c := l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstString, Data: n.Data})
		return l.Prog.Emit(IRInstr{
			Kind:  IRAlloc,
			SubOp: AllocADT,
			Arg0:  uint32(c),
			Data:  uint64(s.InternString("QString")),
		})
	case ast.KindIdent:
		c := l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstString, Data: n.Data})
		return l.Prog.Emit(IRInstr{
			Kind:  IRAlloc,
			SubOp: AllocADT,
			Arg0:  uint32(c),
			Data:  uint64(s.InternString("QVar")),
		})
	case ast.KindInfix:
		opSym := s.InternString(token.TokenKind(n.Op).String())
		opConst := l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstString, Data: uint64(opSym)})
		lhsQ := l.lowerQuote(ast.NodeID(n.Left))
		rhsQ := l.lowerQuote(ast.NodeID(n.Right))
		tStart, tCount := l.Prog.AddExtra([]InstrID{opConst, lhsQ, rhsQ})
		tup := l.Prog.Emit(IRInstr{Kind: IRAlloc, SubOp: AllocTuple, Arg0: tStart, Arg1: tCount})
		return l.Prog.Emit(IRInstr{
			Kind:  IRAlloc,
			SubOp: AllocADT,
			Arg0:  uint32(tup),
			Data:  uint64(s.InternString("QInfix")),
		})
	case ast.KindApply, ast.KindPipeLeft:
		fnQ := l.lowerQuote(ast.NodeID(n.Left))
		argQ := l.lowerQuote(ast.NodeID(n.Right))
		tStart, tCount := l.Prog.AddExtra([]InstrID{fnQ, argQ})
		tup := l.Prog.Emit(IRInstr{Kind: IRAlloc, SubOp: AllocTuple, Arg0: tStart, Arg1: tCount})
		return l.Prog.Emit(IRInstr{
			Kind:  IRAlloc,
			SubOp: AllocADT,
			Arg0:  uint32(tup),
			Data:  uint64(s.InternString("QApp")),
		})
	case ast.KindPipeRight:
		argQ := l.lowerQuote(ast.NodeID(n.Left))
		fnQ := l.lowerQuote(ast.NodeID(n.Right))
		tStart, tCount := l.Prog.AddExtra([]InstrID{fnQ, argQ})
		tup := l.Prog.Emit(IRInstr{Kind: IRAlloc, SubOp: AllocTuple, Arg0: tStart, Arg1: tCount})
		return l.Prog.Emit(IRInstr{
			Kind:  IRAlloc,
			SubOp: AllocADT,
			Arg0:  uint32(tup),
			Data:  uint64(s.InternString("QApp")),
		})
	case ast.KindLambda:
		paramConst := l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstString, Data: n.Data})
		bodyQ := l.lowerQuote(ast.NodeID(n.Right))
		tStart, tCount := l.Prog.AddExtra([]InstrID{paramConst, bodyQ})
		tup := l.Prog.Emit(IRInstr{Kind: IRAlloc, SubOp: AllocTuple, Arg0: tStart, Arg1: tCount})
		return l.Prog.Emit(IRInstr{
			Kind:  IRAlloc,
			SubOp: AllocADT,
			Arg0:  uint32(tup),
			Data:  uint64(s.InternString("QLambda")),
		})
	case ast.KindLet, ast.KindLetRec, ast.KindLetMutable:
		nameConst := l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstString, Data: n.Data})
		rhsQ := l.lowerQuote(ast.NodeID(n.Left))
		bodyQ := l.lowerQuote(ast.NodeID(n.Right))
		tStart, tCount := l.Prog.AddExtra([]InstrID{nameConst, rhsQ, bodyQ})
		tup := l.Prog.Emit(IRInstr{Kind: IRAlloc, SubOp: AllocTuple, Arg0: tStart, Arg1: tCount})
		return l.Prog.Emit(IRInstr{
			Kind:  IRAlloc,
			SubOp: AllocADT,
			Arg0:  uint32(tup),
			Data:  uint64(s.InternString("QLet")),
		})
	default:
		return l.Prog.Emit(IRInstr{
			Kind:  IRAlloc,
			SubOp: AllocADT,
			Arg0:  uint32(NoInstr),
			Data:  uint64(s.InternString("QUnit")),
		})
	}
}

func (l *Lowerer) lowerModuleOrFunctor(n ast.Node) InstrID {
	modName := token.StringID(n.Data)
	if n.Kind == ast.KindFunctor {
		paramNode := l.AST.Get(ast.NodeID(n.Left))
		paramSym := token.StringID(paramNode.Data)
		modRec, exports := l.lowerModuleRecordBody(ast.NodeID(n.Right))
		l.FunctorExports[modName] = exports
		fnInstr := l.Prog.Emit(IRInstr{
			Kind: IRFn,
			Arg0: uint32(modRec),
			Arg1: uint32(NoInstr),
			Data: uint64(paramSym),
		})
		return l.Prog.Emit(IRInstr{
			Kind: IRLet,
			Arg0: uint32(fnInstr),
			Arg1: uint32(NoInstr),
			Data: uint64(modName),
		})
	}

	if n.Left == 0 {
		return l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstUnit})
	}
	bodyNode := l.AST.Get(ast.NodeID(n.Left))
	// Functor application: `module IntMap = MakeMap(IntOrd)`
	if bodyNode.Kind == ast.KindApply {
		fnNode := l.AST.Get(ast.NodeID(bodyNode.Left))
		l.ModuleExports[modName] = l.FunctorExports[token.StringID(fnNode.Data)]
		appInstr := l.lowerNode(ast.NodeID(n.Left), false)
		return l.Prog.Emit(IRInstr{
			Kind: IRLet,
			Arg0: uint32(appInstr),
			Arg1: uint32(NoInstr),
			Data: uint64(modName),
		})
	}

	modRec, exports := l.lowerModuleRecordBody(ast.NodeID(n.Left))
	l.ModuleExports[modName] = exports
	return l.Prog.Emit(IRInstr{
		Kind: IRLet,
		Arg0: uint32(modRec),
		Arg1: uint32(NoInstr),
		Data: uint64(modName),
	})
}

func (l *Lowerer) lowerModuleRecordBody(bodyAstID ast.NodeID) (InstrID, []token.StringID) {
	bodyNode := *l.AST.Get(bodyAstID)
	var stmts []uint32
	if bodyNode.Kind == ast.KindBlock {
		stmts = l.AST.ExtraSlice(bodyNode.Left, bodyNode.Right)
	} else {
		stmts = []uint32{uint32(bodyAstID)}
	}
	var seq []InstrID
	var kvPairs []InstrID
	var exports []token.StringID
	for _, sID := range stmts {
		sn := *l.AST.Get(ast.NodeID(sID))
		insID := l.lowerNode(ast.NodeID(sID), false)
		seq = append(seq, insID)
		if sn.Kind == ast.KindLet || sn.Kind == ast.KindLetRec || sn.Kind == ast.KindLetMutable {
			curr := sn
			for {
				sym := token.StringID(curr.Data)
				if sym != token.NoString {
					varRef := l.Prog.Emit(IRInstr{Kind: IRVar, Data: uint64(sym)})
					kvPairs = append(kvPairs, InstrID(sym), varRef)
					exports = append(exports, sym)
				}
				if curr.Kind == ast.KindLetRec && curr.Right != 0 && l.AST.Get(ast.NodeID(curr.Right)).Kind == ast.KindLetRec {
					curr = *l.AST.Get(ast.NodeID(curr.Right))
					continue
				}
				break
			}
		}
	}
	startKV, countKV := l.Prog.AddExtra(kvPairs)
	recAlloc := l.Prog.Emit(IRInstr{
		Kind:  IRAlloc,
		SubOp: AllocRecord,
		Arg0:  startKV,
		Arg1:  countKV,
	})
	seq = append(seq, recAlloc)
	startSeq, countSeq := l.Prog.AddExtra(seq)
	return l.Prog.Emit(IRInstr{
		Kind:  IRPrim,
		SubOp: PrimSeqBlock,
		Arg0:  startSeq,
		Arg1:  countSeq,
	}), exports
}

func (l *Lowerer) lowerClass(n ast.Node) InstrID {
	className := token.StringID(n.Data)
	paramNode := l.AST.Get(ast.NodeID(n.Left))
	paramSym := token.StringID(paramNode.Data)
	recAlloc := l.lowerMembersToRecord(ast.NodeID(n.Right))
	ctorFn := l.Prog.Emit(IRInstr{
		Kind: IRFn,
		Arg0: uint32(recAlloc),
		Arg1: uint32(NoInstr),
		Data: uint64(paramSym),
	})
	return l.Prog.Emit(IRInstr{
		Kind: IRLet,
		Arg0: uint32(ctorFn),
		Arg1: uint32(NoInstr),
		Data: uint64(className),
	})
}

func (l *Lowerer) lowerObjExpr(n ast.Node) InstrID {
	return l.lowerMembersToRecord(ast.NodeID(n.Left))
}

func (l *Lowerer) lowerMembersToRecord(memberBlockID ast.NodeID) InstrID {
	if memberBlockID == ast.NoNode {
		return l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstUnit})
	}
	block := l.AST.Get(memberBlockID)
	members := l.AST.ExtraSlice(block.Left, block.Right)
	var kvPairs []InstrID
	for _, mID := range members {
		mn := l.AST.Get(ast.NodeID(mID))
		if mn.Flags&ast.FlagAbstract != 0 {
			continue
		}
		valInstr := l.lowerNode(ast.NodeID(mn.Left), false)
		kvPairs = append(kvPairs, InstrID(mn.Data), valInstr)
	}
	start, count := l.Prog.AddExtra(kvPairs)
	return l.Prog.Emit(IRInstr{
		Kind:  IRAlloc,
		SubOp: AllocRecord,
		Arg0:  start,
		Arg1:  count,
	})
}

// lowerLet flattens nested `let` bindings on the RHS (`let x = (let a = 1 in a + 1)`)
// into linear IRLet instructions so the IR remains flat.
func (l *Lowerer) lowerLet(n ast.Node, tailPos bool) InstrID {
	var flags uint8
	if n.Kind == ast.KindLetRec || (n.Flags&ast.FlagRec != 0) {
		flags |= IRFlagRec
	}
	if n.Kind == ast.KindLetMutable || (n.Flags&ast.FlagMutable != 0) {
		flags |= IRFlagMutable
	}
	if n.Kind == ast.KindUse || (n.Flags&ast.FlagUse != 0) {
		flags |= IRFlagDispose
	}

	// Tuple destructuring `let (a, b) = rhs`
	if n.Op == 1 {
		extra := l.AST.ExtraSlice(n.Left, 3)
		patID := ast.NodeID(extra[0])
		rhsID := ast.NodeID(extra[1])
		inID := ast.NodeID(extra[2])
		valInstr := l.lowerNode(rhsID, false)
		tmpSym := l.freshTemp("tup")
		var seq []InstrID
		seq = append(seq, l.Prog.Emit(IRInstr{
			Kind: IRLet,
			Arg0: uint32(valInstr),
			Arg1: uint32(NoInstr),
			Data: uint64(tmpSym),
		}))
		tmpVar := l.Prog.Emit(IRInstr{Kind: IRVar, Data: uint64(tmpSym)})
		seq = append(seq, l.emitPatternBindings(patID, tmpVar)...)
		if inID != ast.NoNode {
			seq = append(seq, l.lowerNode(inID, tailPos))
		}
		start, count := l.Prog.AddExtra(seq)
		return l.Prog.Emit(IRInstr{
			Kind:  IRPrim,
			SubOp: PrimSeqBlock,
			Arg0:  start,
			Arg1:  count,
		})
	}

	// Flatten nested `let a = v1 in v2` on the RHS into sequential bindings
	var lifted []InstrID
	rhsAstID := ast.NodeID(n.Left)
	for rhsAstID != ast.NoNode {
		rhsNode := *l.AST.Get(rhsAstID)
		if (rhsNode.Kind == ast.KindLet || rhsNode.Kind == ast.KindLetMutable) && rhsNode.Right != 0 && rhsNode.Op == 0 {
			innerVal := l.lowerNode(ast.NodeID(rhsNode.Left), false)
			var innerFlags uint8
			if rhsNode.Kind == ast.KindLetMutable {
				innerFlags |= IRFlagMutable
			}
			lifted = append(lifted, l.Prog.Emit(IRInstr{
				Kind:  IRLet,
				Flags: innerFlags,
				Arg0:  uint32(innerVal),
				Arg1:  uint32(NoInstr),
				Data:  rhsNode.Data,
			}))
			rhsAstID = ast.NodeID(rhsNode.Right)
			continue
		}
		break
	}

	valID := l.lowerNode(rhsAstID, false)
	var inID InstrID = NoInstr
	if n.Right != 0 {
		inID = l.lowerNode(ast.NodeID(n.Right), tailPos)
	}
	mainLet := l.Prog.Emit(IRInstr{
		Kind:  IRLet,
		Flags: flags,
		Arg0:  uint32(valID),
		Arg1:  uint32(inID),
		Data:  n.Data,
	})

	if len(lifted) == 0 {
		return mainLet
	}
	lifted = append(lifted, mainLet)
	start, count := l.Prog.AddExtra(lifted)
	return l.Prog.Emit(IRInstr{
		Kind:  IRPrim,
		SubOp: PrimSeqBlock,
		Arg0:  start,
		Arg1:  count,
	})
}

func (l *Lowerer) lowerBlockSlice(children []uint32, tailPos bool) InstrID {
	if len(children) == 0 {
		return l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstUnit})
	}
	if len(children) == 1 {
		return l.lowerNode(ast.NodeID(children[0]), tailPos)
	}
	var seq []InstrID
	for i, ch := range children {
		isLast := i == len(children)-1
		chID := ast.NodeID(ch)
		chNode := *l.AST.Get(chID)

		// If a `use` binding appears in a block, wrap the remainder of the block
		// in a PrimTryFinally disposal sequence so cleanup is guaranteed.
		if (chNode.Kind == ast.KindUse || (chNode.Flags&ast.FlagUse != 0)) && !isLast {
			letInstr := l.lowerLet(chNode, false)
			seq = append(seq, letInstr)
			restInstr := l.lowerBlockSlice(children[i+1:], tailPos)
			disposeVar := l.Prog.Emit(IRInstr{Kind: IRVar, Data: chNode.Data})
			tryFin := l.Prog.Emit(IRInstr{
				Kind:  IRPrim,
				Flags: IRFlagDispose,
				SubOp: PrimTryFinally,
				Arg0:  uint32(restInstr),
				Arg1:  uint32(disposeVar),
			})
			seq = append(seq, tryFin)
			break
		}

		seq = append(seq, l.lowerNode(chID, isLast && tailPos))
	}
	start, count := l.Prog.AddExtra(seq)
	return l.Prog.Emit(IRInstr{
		Kind:  IRPrim,
		SubOp: PrimSeqBlock,
		Arg0:  start,
		Arg1:  count,
	})
}

func (l *Lowerer) lowerInfix(n ast.Node) InstrID {
	lhs := l.lowerNode(ast.NodeID(n.Left), false)
	rhs := l.lowerNode(ast.NodeID(n.Right), false)
	var sub uint16
	switch token.TokenKind(n.Op) {
	case token.TokenPlus:
		sub = PrimAdd
	case token.TokenMinus:
		sub = PrimSub
	case token.TokenStar:
		sub = PrimMul
	case token.TokenSlash:
		sub = PrimDiv
	case token.TokenPercent:
		sub = PrimMod
	case token.TokenAssign, token.TokenEq:
		sub = PrimEq
	case token.TokenNeq:
		sub = PrimNeq
	case token.TokenLt:
		sub = PrimLt
	case token.TokenLe:
		sub = PrimLe
	case token.TokenGt:
		sub = PrimGt
	case token.TokenGe:
		sub = PrimGe
	case token.TokenAndAnd:
		sub = PrimAnd
	case token.TokenOrOr:
		sub = PrimOr
	case token.TokenCons:
		sub = PrimCons
	case token.TokenAppend:
		sub = PrimAppend
	default:
		sub = PrimAdd
	}
	return l.Prog.Emit(IRInstr{
		Kind:  IRPrim,
		SubOp: sub,
		Arg0:  uint32(lhs),
		Arg1:  uint32(rhs),
	})
}
