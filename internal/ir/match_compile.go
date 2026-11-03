package ir

import (
	"fml/internal/ast"
	"fml/internal/token"
)

// compileMatch compiles a KindMatch AST node into an IRSwitch decision tree,
// grouping constructor/constant tags without redundant tag checks and chaining
// `when` guard expressions into fallback branches.
//
// Jaime says: Pattern matching isn't a linear chain of slow reflection checks.
// Group arms by constructor tag into a single flat IRSwitch table, extract
// payloads with IRLoad, and jump directly to the matching branch.
func (l *Lowerer) compileMatch(n ast.Node, tailPos bool) InstrID {
	scrutInstr := l.lowerNode(ast.NodeID(n.Left), false)
	scrutSym := l.freshTemp("scrut")
	bindScrut := l.Prog.Emit(IRInstr{
		Kind: IRLet,
		Arg0: uint32(scrutInstr),
		Arg1: uint32(NoInstr),
		Data: uint64(scrutSym),
	})
	scrutVar := l.Prog.Emit(IRInstr{Kind: IRVar, Data: uint64(scrutSym)})

	armsBlock := l.AST.Get(ast.NodeID(n.Right))
	arms := l.AST.ExtraSlice(armsBlock.Left, armsBlock.Right)

	// Check if matching via an Active Pattern
	var activeFnSym token.StringID
	for _, armID := range arms {
		arm := l.AST.Get(ast.NodeID(armID))
		pat := l.AST.Get(ast.NodeID(arm.Left))
		if pat.Kind == ast.KindPatDU {
			if fnSym, ok := l.ActivePatFns[token.StringID(pat.Data)]; ok {
				activeFnSym = fnSym
				break
			}
		}
	}

	dispatchVal := scrutVar
	if activeFnSym != token.NoString {
		fnVar := l.Prog.Emit(IRInstr{Kind: IRVar, Data: uint64(activeFnSym)})
		dispatchVal = l.Prog.Emit(IRInstr{
			Kind: IRApp,
			Arg0: uint32(fnVar),
			Arg1: uint32(scrutVar),
		})
	}

	treeID := l.buildDecisionTree(arms, scrutVar, dispatchVal, tailPos)
	start, count := l.Prog.AddExtra([]InstrID{bindScrut, treeID})
	return l.Prog.Emit(IRInstr{
		Kind:  IRPrim,
		SubOp: PrimSeqBlock,
		Arg0:  start,
		Arg1:  count,
	})
}

func (l *Lowerer) buildDecisionTree(arms []uint32, scrutVar, dispatchVal InstrID, tailPos bool) InstrID {
	if len(arms) == 0 {
		return l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstUnit})
	}

	// Group unguarded top-level constructor / constant arms into a single IRSwitch
	// up to the first guarded or catch-all arm.
	var switchCases []SwitchCase
	seenTags := make(map[uint32]bool)
	isADTSwitch := false
	isListSwitch := false
	idx := 0

	for idx < len(arms) {
		arm := *l.AST.Get(ast.NodeID(arms[idx]))
		if arm.Data != 0 {
			// Guarded arm must be evaluated as a conditional branch with fallback to remaining arms
			break
		}
		pat := *l.AST.Get(ast.NodeID(arm.Left))
		if pat.Kind == ast.KindPatWildcard || pat.Kind == ast.KindPatVar ||
			pat.Kind == ast.KindPatTuple || pat.Kind == ast.KindPatRecord {
			break
		}

		tag, adtMode, listMode, ok := l.extractPatternSwitchTag(pat)
		if !ok || (len(switchCases) > 0 && (adtMode != isADTSwitch || listMode != isListSwitch)) {
			break
		}
		isADTSwitch = adtMode
		isListSwitch = listMode
		if seenTags[tag] {
			// Deduplicate: skip redundant duplicate tag check
			idx++
			continue
		}
		seenTags[tag] = true
		target := l.compileArmBody(arm, scrutVar, tailPos)
		switchCases = append(switchCases, SwitchCase{Tag: tag, Target: target})
		idx++
	}

	var fallback InstrID
	if idx < len(arms) {
		fallback = l.compileRemainingArm(arms, idx, scrutVar, dispatchVal, tailPos)
	} else {
		fallback = l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstUnit})
	}

	if len(switchCases) == 0 {
		return fallback
	}

	switchScrut := dispatchVal
	if isADTSwitch {
		switchScrut = l.Prog.Emit(IRInstr{
			Kind:  IRLoad,
			SubOp: LoadADTTag,
			Arg0:  uint32(dispatchVal),
		})
	} else if isListSwitch {
		switchScrut = l.Prog.Emit(IRInstr{
			Kind:  IRLoad,
			SubOp: LoadListIsEmpty,
			Arg0:  uint32(dispatchVal),
		})
	}

	start, count := l.Prog.AddSwitchCases(switchCases)
	return l.Prog.Emit(IRInstr{
		Kind: IRSwitch,
		Arg0: uint32(switchScrut),
		Arg1: start,
		Aux:  uint32(fallback),
		Data: uint64(count),
	})
}

func (l *Lowerer) compileRemainingArm(arms []uint32, idx int, scrutVar, dispatchVal InstrID, tailPos bool) InstrID {
	arm := *l.AST.Get(ast.NodeID(arms[idx]))
	patID := ast.NodeID(arm.Left)
	pat := *l.AST.Get(patID)

	nextFallback := func() InstrID {
		if idx+1 < len(arms) {
			return l.buildDecisionTree(arms[idx+1:], scrutVar, dispatchVal, tailPos)
		}
		return l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstUnit})
	}

	// Build pattern condition check if not a wildcard/var
	condInstr := l.emitPatternCondition(pat, dispatchVal)
	bindings := l.emitPatternBindings(patID, scrutVar)

	var bodyWithGuard InstrID
	if arm.Data != 0 {
		guardNode := l.AST.Get(ast.NodeID(arm.Data))
		guardExpr := l.lowerNode(ast.NodeID(guardNode.Left), false)
		bodyExpr := l.lowerNode(ast.NodeID(arm.Right), tailPos)
		fb := nextFallback()
		start, count := l.Prog.AddSwitchCases([]SwitchCase{{Tag: 1, Target: bodyExpr}})
		guardSwitch := l.Prog.Emit(IRInstr{
			Kind: IRSwitch,
			Arg0: uint32(guardExpr),
			Arg1: start,
			Aux:  uint32(fb),
			Data: uint64(count),
		})
		if len(bindings) > 0 {
			seq := append(bindings, guardSwitch)
			s, c := l.Prog.AddExtra(seq)
			bodyWithGuard = l.Prog.Emit(IRInstr{Kind: IRPrim, SubOp: PrimSeqBlock, Arg0: s, Arg1: c})
		} else {
			bodyWithGuard = guardSwitch
		}
	} else {
		bodyWithGuard = l.compileArmBody(arm, scrutVar, tailPos)
	}

	if condInstr == NoInstr {
		return bodyWithGuard
	}
	fb := nextFallback()
	start, count := l.Prog.AddSwitchCases([]SwitchCase{{Tag: 1, Target: bodyWithGuard}})
	return l.Prog.Emit(IRInstr{
		Kind: IRSwitch,
		Arg0: uint32(condInstr),
		Arg1: start,
		Aux:  uint32(fb),
		Data: uint64(count),
	})
}

func (l *Lowerer) compileArmBody(arm ast.Node, scrutVar InstrID, tailPos bool) InstrID {
	bindings := l.emitPatternBindings(ast.NodeID(arm.Left), scrutVar)
	bodyID := l.lowerNode(ast.NodeID(arm.Right), tailPos)
	if len(bindings) == 0 {
		return bodyID
	}
	bindings = append(bindings, bodyID)
	start, count := l.Prog.AddExtra(bindings)
	return l.Prog.Emit(IRInstr{
		Kind:  IRPrim,
		SubOp: PrimSeqBlock,
		Arg0:  start,
		Arg1:  count,
	})
}

func (l *Lowerer) extractPatternSwitchTag(pat ast.Node) (tag uint32, isADT bool, isList bool, ok bool) {
	switch pat.Kind {
	case ast.KindPatDU:
		if _, isPartial := l.PartialActivePats[token.StringID(pat.Data)]; isPartial {
			return 0, false, false, false
		}
		if l.hasNestedPatternCondition(ast.NodeID(pat.Left)) {
			return 0, false, false, false
		}
		return uint32(pat.Data), true, false, true
	case ast.KindPatConst:
		if pat.Left != 0 {
			lit := l.AST.Get(ast.NodeID(pat.Left))
			if lit.Kind == ast.KindInt || lit.Kind == ast.KindBool || lit.Kind == ast.KindString {
				return uint32(lit.Data), false, false, true
			}
		}
		return 0, false, false, false
	case ast.KindPatList:
		if pat.Right == 0 {
			// Empty list `[]` has IsEmpty == 1
			return 1, false, true, true
		}
		return 0, false, false, false
	case ast.KindPatCons:
		if l.hasNestedPatternCondition(ast.NodeID(pat.Left)) || l.hasNestedPatternCondition(ast.NodeID(pat.Right)) {
			return 0, false, false, false
		}
		// Non-empty list `h :: t` has IsEmpty == 0
		return 0, false, true, true
	default:
		return 0, false, false, false
	}
}

func (l *Lowerer) hasNestedPatternCondition(patID ast.NodeID) bool {
	if patID == ast.NoNode {
		return false
	}
	pat := *l.AST.Get(patID)
	switch pat.Kind {
	case ast.KindPatWildcard, ast.KindPatVar:
		return false
	case ast.KindPatTuple, ast.KindPatRecord:
		subs := l.AST.ExtraSlice(pat.Left, pat.Right)
		for _, s := range subs {
			n := l.AST.Get(ast.NodeID(s))
			if n.Kind == ast.KindRecordField {
				if l.hasNestedPatternCondition(ast.NodeID(n.Left)) {
					return true
				}
			} else if l.hasNestedPatternCondition(ast.NodeID(s)) {
				return true
			}
		}
		return false
	default:
		return true
	}
}

func (l *Lowerer) emitPatternCondition(pat ast.Node, scrutVar InstrID) InstrID {
	switch pat.Kind {
	case ast.KindPatWildcard, ast.KindPatVar:
		return NoInstr

	case ast.KindPatConst:
		if pat.Left != 0 {
			litID := l.lowerNode(ast.NodeID(pat.Left), false)
			return l.Prog.Emit(IRInstr{
				Kind:  IRPrim,
				SubOp: PrimEq,
				Arg0:  uint32(scrutVar),
				Arg1:  uint32(litID),
			})
		}
		return NoInstr

	case ast.KindPatDU:
		if pFnSym, isPartial := l.PartialActivePats[token.StringID(pat.Data)]; isPartial {
			fnVar := l.Prog.Emit(IRInstr{Kind: IRVar, Data: uint64(pFnSym)})
			appRes := l.Prog.Emit(IRInstr{Kind: IRApp, Arg0: uint32(fnVar), Arg1: uint32(scrutVar)})
			tagLoad := l.Prog.Emit(IRInstr{Kind: IRLoad, SubOp: LoadADTTag, Arg0: uint32(appRes)})
			someTag := l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstInt, Data: uint64(l.AST.Strings.InternString("Some"))})
			tagEq := l.Prog.Emit(IRInstr{Kind: IRPrim, SubOp: PrimEq, Arg0: uint32(tagLoad), Arg1: uint32(someTag)})
			if pat.Left == 0 {
				return tagEq
			}
			payload := l.Prog.Emit(IRInstr{Kind: IRLoad, SubOp: LoadADTPayload, Arg0: uint32(appRes)})
			subPat := *l.AST.Get(ast.NodeID(pat.Left))
			subCond := l.emitPatternCondition(subPat, payload)
			return l.combineConditions(tagEq, subCond)
		}
		tagLoad := l.Prog.Emit(IRInstr{
			Kind:  IRLoad,
			SubOp: LoadADTTag,
			Arg0:  uint32(scrutVar),
		})
		expectedTag := l.Prog.Emit(IRInstr{
			Kind:  IRConst,
			SubOp: ConstInt,
			Data:  pat.Data,
		})
		tagEq := l.Prog.Emit(IRInstr{
			Kind:  IRPrim,
			SubOp: PrimEq,
			Arg0:  uint32(tagLoad),
			Arg1:  uint32(expectedTag),
		})
		if pat.Left == 0 {
			return tagEq
		}
		payload := l.Prog.Emit(IRInstr{
			Kind:  IRLoad,
			SubOp: LoadADTPayload,
			Arg0:  uint32(scrutVar),
		})
		subPat := *l.AST.Get(ast.NodeID(pat.Left))
		subCond := l.emitPatternCondition(subPat, payload)
		return l.combineConditions(tagEq, subCond)

	case ast.KindPatTuple:
		subs := l.AST.ExtraSlice(pat.Left, pat.Right)
		var cond InstrID = NoInstr
		for i, sub := range subs {
			elemLoad := l.Prog.Emit(IRInstr{
				Kind:  IRLoad,
				SubOp: LoadTupleElem,
				Arg0:  uint32(scrutVar),
				Arg1:  uint32(i),
			})
			subPat := *l.AST.Get(ast.NodeID(sub))
			subCond := l.emitPatternCondition(subPat, elemLoad)
			cond = l.combineConditions(cond, subCond)
		}
		return cond

	case ast.KindPatCons:
		isEmpty := l.Prog.Emit(IRInstr{
			Kind:  IRLoad,
			SubOp: LoadListIsEmpty,
			Arg0:  uint32(scrutVar),
		})
		zero := l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstInt, Data: 0})
		nonEmpty := l.Prog.Emit(IRInstr{
			Kind:  IRPrim,
			SubOp: PrimEq,
			Arg0:  uint32(isEmpty),
			Arg1:  uint32(zero),
		})
		headLoad := l.Prog.Emit(IRInstr{
			Kind:  IRLoad,
			SubOp: LoadListHead,
			Arg0:  uint32(scrutVar),
		})
		tailLoad := l.Prog.Emit(IRInstr{
			Kind:  IRLoad,
			SubOp: LoadListTail,
			Arg0:  uint32(scrutVar),
		})
		hCond := l.emitPatternCondition(*l.AST.Get(ast.NodeID(pat.Left)), headLoad)
		tCond := l.emitPatternCondition(*l.AST.Get(ast.NodeID(pat.Right)), tailLoad)
		return l.combineConditions(nonEmpty, l.combineConditions(hCond, tCond))

	case ast.KindPatList:
		subs := l.AST.ExtraSlice(pat.Left, pat.Right)
		curr := scrutVar
		var cond InstrID = NoInstr
		zero := l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstInt, Data: 0})
		one := l.Prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstInt, Data: 1})
		for _, sub := range subs {
			isEmpty := l.Prog.Emit(IRInstr{Kind: IRLoad, SubOp: LoadListIsEmpty, Arg0: uint32(curr)})
			nonEmpty := l.Prog.Emit(IRInstr{Kind: IRPrim, SubOp: PrimEq, Arg0: uint32(isEmpty), Arg1: uint32(zero)})
			cond = l.combineConditions(cond, nonEmpty)
			head := l.Prog.Emit(IRInstr{Kind: IRLoad, SubOp: LoadListHead, Arg0: uint32(curr)})
			subCond := l.emitPatternCondition(*l.AST.Get(ast.NodeID(sub)), head)
			cond = l.combineConditions(cond, subCond)
			curr = l.Prog.Emit(IRInstr{Kind: IRLoad, SubOp: LoadListTail, Arg0: uint32(curr)})
		}
		endEmpty := l.Prog.Emit(IRInstr{Kind: IRLoad, SubOp: LoadListIsEmpty, Arg0: uint32(curr)})
		endCheck := l.Prog.Emit(IRInstr{Kind: IRPrim, SubOp: PrimEq, Arg0: uint32(endEmpty), Arg1: uint32(one)})
		return l.combineConditions(cond, endCheck)

	case ast.KindPatRecord:
		fields := l.AST.ExtraSlice(pat.Left, pat.Right)
		var cond InstrID = NoInstr
		for _, fID := range fields {
			fn := l.AST.Get(ast.NodeID(fID))
			fLoad := l.Prog.Emit(IRInstr{
				Kind:  IRLoad,
				SubOp: LoadRecordField,
				Arg0:  uint32(scrutVar),
				Data:  fn.Data,
			})
			subCond := l.emitPatternCondition(*l.AST.Get(ast.NodeID(fn.Left)), fLoad)
			cond = l.combineConditions(cond, subCond)
		}
		return cond

	default:
		return NoInstr
	}
}

func (l *Lowerer) combineConditions(a, b InstrID) InstrID {
	if a == NoInstr {
		return b
	}
	if b == NoInstr {
		return a
	}
	return l.Prog.Emit(IRInstr{
		Kind:  IRPrim,
		SubOp: PrimAnd,
		Arg0:  uint32(a),
		Arg1:  uint32(b),
	})
}

func (l *Lowerer) emitPatternBindings(patID ast.NodeID, valInstr InstrID) []InstrID {
	if patID == ast.NoNode {
		return nil
	}
	pat := *l.AST.Get(patID)
	switch pat.Kind {
	case ast.KindPatVar:
		return []InstrID{
			l.Prog.Emit(IRInstr{
				Kind: IRLet,
				Arg0: uint32(valInstr),
				Arg1: uint32(NoInstr),
				Data: pat.Data,
			}),
		}
	case ast.KindPatDU:
		if pat.Left == 0 {
			return nil
		}
		if pFnSym, isPartial := l.PartialActivePats[token.StringID(pat.Data)]; isPartial {
			fnVar := l.Prog.Emit(IRInstr{Kind: IRVar, Data: uint64(pFnSym)})
			appRes := l.Prog.Emit(IRInstr{Kind: IRApp, Arg0: uint32(fnVar), Arg1: uint32(valInstr)})
			payload := l.Prog.Emit(IRInstr{
				Kind:  IRLoad,
				SubOp: LoadADTPayload,
				Arg0:  uint32(appRes),
			})
			return l.emitPatternBindings(ast.NodeID(pat.Left), payload)
		}
		payload := l.Prog.Emit(IRInstr{
			Kind:  IRLoad,
			SubOp: LoadADTPayload,
			Arg0:  uint32(valInstr),
		})
		return l.emitPatternBindings(ast.NodeID(pat.Left), payload)
	case ast.KindPatTuple:
		subs := l.AST.ExtraSlice(pat.Left, pat.Right)
		var out []InstrID
		for i, sub := range subs {
			elemLoad := l.Prog.Emit(IRInstr{
				Kind:  IRLoad,
				SubOp: LoadTupleElem,
				Arg0:  uint32(valInstr),
				Arg1:  uint32(i),
			})
			out = append(out, l.emitPatternBindings(ast.NodeID(sub), elemLoad)...)
		}
		return out
	case ast.KindPatCons:
		headLoad := l.Prog.Emit(IRInstr{
			Kind:  IRLoad,
			SubOp: LoadListHead,
			Arg0:  uint32(valInstr),
		})
		tailLoad := l.Prog.Emit(IRInstr{
			Kind:  IRLoad,
			SubOp: LoadListTail,
			Arg0:  uint32(valInstr),
		})
		out := l.emitPatternBindings(ast.NodeID(pat.Left), headLoad)
		out = append(out, l.emitPatternBindings(ast.NodeID(pat.Right), tailLoad)...)
		return out
	case ast.KindPatList:
		subs := l.AST.ExtraSlice(pat.Left, pat.Right)
		curr := valInstr
		var out []InstrID
		for _, sub := range subs {
			headLoad := l.Prog.Emit(IRInstr{
				Kind:  IRLoad,
				SubOp: LoadListHead,
				Arg0:  uint32(curr),
			})
			out = append(out, l.emitPatternBindings(ast.NodeID(sub), headLoad)...)
			curr = l.Prog.Emit(IRInstr{
				Kind:  IRLoad,
				SubOp: LoadListTail,
				Arg0:  uint32(curr),
			})
		}
		return out
	case ast.KindPatRecord:
		fields := l.AST.ExtraSlice(pat.Left, pat.Right)
		var out []InstrID
		for _, fID := range fields {
			fn := l.AST.Get(ast.NodeID(fID))
			fLoad := l.Prog.Emit(IRInstr{
				Kind:  IRLoad,
				SubOp: LoadRecordField,
				Arg0:  uint32(valInstr),
				Data:  fn.Data,
			})
			out = append(out, l.emitPatternBindings(ast.NodeID(fn.Left), fLoad)...)
		}
		return out
	default:
		return nil
	}
}
