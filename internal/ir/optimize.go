package ir

import (
	"math"

	"fml/internal/token"
)

// Optimize performs an in-place multi-pass optimization pipeline over prog:
//  1. Constant & copy propagation across immutable single-assignment IRLet bindings.
//  2. Beta-reduction / inlining of small non-recursive IRFn lambdas applied to constants.
//  3. Constant folding on arithmetic, comparison, boolean, and string IRPrim instructions.
//  4. Dead-branch elimination on IRSwitch instructions when the scrutinee is a known IRConst.
//  5. Dead-code elimination (DCE) of unused pure IRLet bindings and PrimSeqBlock statements.
//
// Jaime says: Because IRInstr is a flat 24-byte value in a contiguous slice
// ordered bottom-up (operands always have lower InstrIDs than their consumers),
// constant propagation, beta-reduction, constant folding, and dead-code
// elimination run as linear index sweeps over the flat array. Zero pointer
// graphs, zero heap churn.
func Optimize(prog *Program) {
	if prog == nil || len(prog.Instrs) == 0 || prog.Root == NoInstr {
		return
	}

	unsafeSyms := collectUnsafeSymbols(prog)

	for pass := 0; pass < 8; pass++ {
		changed := false
		constEnv := make(map[token.StringID]IRInstr)
		copyEnv := make(map[token.StringID]token.StringID)
		fnEnv := make(map[token.StringID]IRInstr)

		// Collect single-assignment immutable IRLet bindings first so scoped `let ... in body`
		// bindings (where IRLet has a higher InstrID than body) propagate in a single pass.
		for _, ins := range prog.Instrs {
			if ins.Kind != IRLet {
				continue
			}
			sym := token.StringID(ins.Data)
			if sym != token.NoString && ins.Flags == 0 && !unsafeSyms[sym] && int(ins.Arg0) < len(prog.Instrs) {
				rhs := prog.Instrs[ins.Arg0]
				switch rhs.Kind {
				case IRConst:
					constEnv[sym] = rhs
				case IRVar:
					srcSym := token.StringID(rhs.Data)
					if srcSym != token.NoString && !unsafeSyms[srcSym] {
						if cIns, ok := constEnv[srcSym]; ok {
							constEnv[sym] = cIns
						} else {
							copyEnv[sym] = srcSym
						}
					}
				case IRFn:
					fnEnv[sym] = rhs
				}
			}
		}

		for i := 0; i < len(prog.Instrs); i++ {
			ins := prog.Instrs[i]
			switch ins.Kind {
			case IRVar:
				sym := token.StringID(ins.Data)
				if cIns, ok := constEnv[sym]; ok {
					prog.Instrs[i] = cIns
					changed = true
				} else if targetSym, ok := copyEnv[sym]; ok && targetSym != sym {
					prog.Instrs[i].Data = uint64(targetSym)
					changed = true
				}

			case IRApp:
				if inlined, ok := tryInlineApp(prog, ins, fnEnv, unsafeSyms); ok {
					prog.Instrs[i] = inlined
					changed = true
				}

			case IRPrim:
				if folded, ok := tryFoldPrim(prog, ins, unsafeSyms); ok {
					prog.Instrs[i] = folded
					changed = true
				}

			case IRSwitch:
				if target, ok := tryFoldSwitch(prog, ins); ok && int(target) < len(prog.Instrs) {
					prog.Instrs[i] = prog.Instrs[target]
					changed = true
				}
			}
		}

		if eliminateCommonSubexprs(prog, prog.Root, make(map[cseKey]token.StringID), unsafeSyms) {
			changed = true
		}
		if eliminateDeadBindings(prog, unsafeSyms) {
			changed = true
		}
		if !changed {
			break
		}
	}

	ConvertClosures(prog)
}

func collectUnsafeSymbols(prog *Program) map[token.StringID]bool {
	bindCounts := make(map[token.StringID]int)
	unsafe := make(map[token.StringID]bool)

	for _, ins := range prog.Instrs {
		switch ins.Kind {
		case IRLet:
			sym := token.StringID(ins.Data)
			if sym != token.NoString {
				bindCounts[sym]++
				if ins.Flags&(IRFlagMutable|IRFlagRec|IRFlagDispose) != 0 {
					unsafe[sym] = true
				}
			}
		case IRFn:
			param := token.StringID(ins.Data)
			if param != token.NoString {
				bindCounts[param]++
			}
		case IRPrim:
			if ins.SubOp == PrimForIn || ins.SubOp == PrimForTo {
				sym := token.StringID(ins.Data)
				if sym != token.NoString {
					unsafe[sym] = true
				}
			}
		case IRStore:
			sym := token.StringID(ins.Data)
			if sym != token.NoString {
				unsafe[sym] = true
			}
		}
	}

	for sym, count := range bindCounts {
		if count > 1 {
			unsafe[sym] = true
		}
	}
	return unsafe
}

func tryInlineApp(prog *Program, appIns IRInstr, fnEnv map[token.StringID]IRInstr, unsafeSyms map[token.StringID]bool) (IRInstr, bool) {
	if int(appIns.Arg0) >= len(prog.Instrs) || int(appIns.Arg1) >= len(prog.Instrs) {
		return IRInstr{}, false
	}
	argIns := prog.Instrs[appIns.Arg1]
	if argIns.Kind != IRConst {
		return IRInstr{}, false
	}

	fnIns := prog.Instrs[appIns.Arg0]
	if fnIns.Kind == IRVar {
		lookedUp, ok := fnEnv[token.StringID(fnIns.Data)]
		if !ok {
			return IRInstr{}, false
		}
		fnIns = lookedUp
	}
	if fnIns.Kind != IRFn {
		return IRInstr{}, false
	}

	param := token.StringID(fnIns.Data)
	if param == token.NoString || unsafeSyms[param] || int(fnIns.Arg0) >= len(prog.Instrs) {
		return IRInstr{}, false
	}
	bodyIns := prog.Instrs[fnIns.Arg0]

	switch bodyIns.Kind {
	case IRConst:
		return bodyIns, true
	case IRVar:
		if token.StringID(bodyIns.Data) == param {
			return argIns, true
		}
		return bodyIns, true
	case IRPrim:
		if bodyIns.SubOp == PrimSeqBlock || int(bodyIns.Arg0) >= len(prog.Instrs) {
			return IRInstr{}, false
		}
		lhs := prog.Instrs[bodyIns.Arg0]
		if lhs.Kind == IRVar && token.StringID(lhs.Data) == param {
			lhs = argIns
		}
		if bodyIns.SubOp == PrimNeg || bodyIns.SubOp == PrimNot {
			return evalFoldedOperands(prog, bodyIns.SubOp, lhs, IRInstr{}, false)
		}
		if bodyIns.Arg1 == uint32(NoInstr) || int(bodyIns.Arg1) >= len(prog.Instrs) {
			return IRInstr{}, false
		}
		rhs := prog.Instrs[bodyIns.Arg1]
		if rhs.Kind == IRVar && token.StringID(rhs.Data) == param {
			rhs = argIns
		}
		return evalFoldedOperands(prog, bodyIns.SubOp, lhs, rhs, true)
	}
	return IRInstr{}, false
}

func eliminateDeadBindings(prog *Program, unsafeSyms map[token.StringID]bool) bool {
	changed := false

	// 1. Eliminate scoped `let unused = pure in body`
	for i := 0; i < len(prog.Instrs); i++ {
		ins := prog.Instrs[i]
		if ins.Kind == IRLet && ins.Flags == 0 && InstrID(ins.Arg1) != NoInstr &&
			int(ins.Arg0) < len(prog.Instrs) && int(ins.Arg1) < len(prog.Instrs) {
			sym := token.StringID(ins.Data)
			if !unsafeSyms[sym] && isPureExpr(prog, prog.Instrs[ins.Arg0], unsafeSyms) {
				bodyUses := make(map[token.StringID]int)
				markReachableUses(prog, InstrID(ins.Arg1), bodyUses, make(map[InstrID]bool))
				if bodyUses[sym] == 0 {
					prog.Instrs[i] = prog.Instrs[ins.Arg1]
					changed = true
				}
			}
		}
	}

	// 2. Recompute reachable variable uses from Root (excluding dead top-level IRLet RHSs)
	usedSyms := computeLiveSymbolUses(prog)

	// 3. Prune dead pure non-final elements in PrimSeqBlock instructions
	for i := 0; i < len(prog.Instrs); i++ {
		ins := prog.Instrs[i]
		if ins.Kind != IRPrim || ins.SubOp != PrimSeqBlock || ins.Arg1 <= 1 {
			continue
		}
		var kept []InstrID
		for j := uint32(0); j < ins.Arg1; j++ {
			chID := InstrID(prog.Extra[ins.Arg0+j])
			isLast := j == ins.Arg1-1
			if !isLast && int(chID) < len(prog.Instrs) {
				chIns := prog.Instrs[chID]
				if chIns.Kind == IRConst || chIns.Kind == IRVar {
					changed = true
					continue
				}
				if chIns.Kind == IRLet && chIns.Flags == 0 && InstrID(chIns.Arg1) == NoInstr &&
					int(chIns.Arg0) < len(prog.Instrs) {
					sym := token.StringID(chIns.Data)
					if !unsafeSyms[sym] && usedSyms[sym] == 0 && isPureExpr(prog, prog.Instrs[chIns.Arg0], unsafeSyms) {
						changed = true
						continue
					}
				}
			}
			kept = append(kept, chID)
		}
		if len(kept) == 1 && int(kept[0]) < len(prog.Instrs) {
			prog.Instrs[i] = prog.Instrs[kept[0]]
			changed = true
		} else if uint32(len(kept)) < ins.Arg1 {
			start, count := prog.AddExtra(kept)
			prog.Instrs[i].Arg0 = start
			prog.Instrs[i].Arg1 = count
			changed = true
		}
	}

	return changed
}

func isPureExpr(prog *Program, ins IRInstr, unsafeSyms map[token.StringID]bool) bool {
	switch ins.Kind {
	case IRConst, IRFn:
		return true
	case IRVar:
		return !unsafeSyms[token.StringID(ins.Data)]
	case IRPrim:
		switch ins.SubOp {
		case PrimNeg, PrimNot:
			return int(ins.Arg0) < len(prog.Instrs) && isPureExpr(prog, prog.Instrs[ins.Arg0], unsafeSyms)
		case PrimAdd, PrimSub, PrimMul, PrimEq, PrimNeq, PrimLt, PrimLe, PrimGt, PrimGe, PrimAnd, PrimOr:
			return int(ins.Arg0) < len(prog.Instrs) && int(ins.Arg1) < len(prog.Instrs) &&
				isPureExpr(prog, prog.Instrs[ins.Arg0], unsafeSyms) &&
				isPureExpr(prog, prog.Instrs[ins.Arg1], unsafeSyms)
		}
	}
	return false
}

func computeLiveSymbolUses(prog *Program) map[token.StringID]int {
	uses := make(map[token.StringID]int)
	visited := make(map[InstrID]bool)
	markReachableUses(prog, prog.Root, uses, visited)
	return uses
}

func markReachableUses(prog *Program, id InstrID, uses map[token.StringID]int, visited map[InstrID]bool) {
	if id == NoInstr || int(id) >= len(prog.Instrs) || visited[id] {
		return
	}
	visited[id] = true
	ins := prog.Instrs[id]

	switch ins.Kind {
	case IRConst:
		return
	case IRVar:
		sym := token.StringID(ins.Data)
		if sym != token.NoString {
			uses[sym]++
		}
	case IRLet:
		markReachableUses(prog, InstrID(ins.Arg0), uses, visited)
		markReachableUses(prog, InstrID(ins.Arg1), uses, visited)
	case IRFn:
		markReachableUses(prog, InstrID(ins.Arg0), uses, visited)
	case IRApp:
		markReachableUses(prog, InstrID(ins.Arg0), uses, visited)
		markReachableUses(prog, InstrID(ins.Arg1), uses, visited)
	case IRSwitch:
		markReachableUses(prog, InstrID(ins.Arg0), uses, visited)
		count := uint32(ins.Data)
		for i := uint32(0); i < count; i++ {
			idx := ins.Arg1 + i*2 + 1
			if int(idx) < len(prog.Extra) {
				markReachableUses(prog, InstrID(prog.Extra[idx]), uses, visited)
			}
		}
		markReachableUses(prog, InstrID(ins.Aux), uses, visited)
	case IRPrim:
		if ins.SubOp == PrimSeqBlock {
			for i := uint32(0); i < ins.Arg1; i++ {
				idx := ins.Arg0 + i
				if int(idx) < len(prog.Extra) {
					markReachableUses(prog, InstrID(prog.Extra[idx]), uses, visited)
				}
			}
			return
		}
		markReachableUses(prog, InstrID(ins.Arg0), uses, visited)
		markReachableUses(prog, InstrID(ins.Arg1), uses, visited)
	case IRAlloc:
		if ins.SubOp == AllocADT {
			markReachableUses(prog, InstrID(ins.Arg0), uses, visited)
			return
		}
		if ins.SubOp == AllocRecord {
			if ins.Flags != 0 {
				markReachableUses(prog, InstrID(ins.Aux), uses, visited)
			}
			for i := uint32(0); i < ins.Arg1; i += 2 {
				idx := ins.Arg0 + i + 1
				if int(idx) < len(prog.Extra) {
					markReachableUses(prog, InstrID(prog.Extra[idx]), uses, visited)
				}
			}
			return
		}
		for i := uint32(0); i < ins.Arg1; i++ {
			idx := ins.Arg0 + i
			if int(idx) < len(prog.Extra) {
				markReachableUses(prog, InstrID(prog.Extra[idx]), uses, visited)
			}
		}
	case IRLoad:
		if ins.SubOp == LoadRecordField && ins.Aux != 0 {
			uses[token.StringID(ins.Aux)]++
		}
		markReachableUses(prog, InstrID(ins.Arg0), uses, visited)
	case IRStore:
		sym := token.StringID(ins.Data)
		if sym != token.NoString {
			uses[sym]++
		}
		markReachableUses(prog, InstrID(ins.Arg0), uses, visited)
		markReachableUses(prog, InstrID(ins.Arg1), uses, visited)
		markReachableUses(prog, InstrID(ins.Aux), uses, visited)
	}
}

type cseKey struct {
	subOp  uint16
	flags  uint8
	lhsK   IRKind
	lhsSub uint16
	lhsDat uint64
	rhsK   IRKind
	rhsSub uint16
	rhsDat uint64
}

func cloneCSEMap(m map[cseKey]token.StringID) map[cseKey]token.StringID {
	out := make(map[cseKey]token.StringID, len(m)+4)
	for k, v := range m {
		out[k] = v
	}
	return out
}

func isCSELeaf(ins IRInstr, unsafeSyms map[token.StringID]bool) bool {
	if ins.Kind == IRConst {
		return true
	}
	if ins.Kind == IRVar {
		sym := token.StringID(ins.Data)
		return sym != token.NoString && !unsafeSyms[sym]
	}
	return false
}

func buildCSEKey(prog *Program, ins IRInstr, unsafeSyms map[token.StringID]bool) (cseKey, bool) {
	if ins.Kind != IRPrim || int(ins.Arg0) >= len(prog.Instrs) {
		return cseKey{}, false
	}
	lhs := prog.Instrs[ins.Arg0]
	if !isCSELeaf(lhs, unsafeSyms) {
		return cseKey{}, false
	}
	if ins.SubOp == PrimNeg || ins.SubOp == PrimNot {
		return cseKey{
			subOp:  ins.SubOp,
			flags:  ins.Flags,
			lhsK:   lhs.Kind,
			lhsSub: lhs.SubOp,
			lhsDat: lhs.Data,
		}, true
	}

	switch ins.SubOp {
	case PrimAdd, PrimSub, PrimMul, PrimDiv, PrimMod,
		PrimEq, PrimNeq, PrimLt, PrimLe, PrimGt, PrimGe,
		PrimAnd, PrimOr:
	default:
		return cseKey{}, false
	}
	if ins.Arg1 == uint32(NoInstr) || int(ins.Arg1) >= len(prog.Instrs) {
		return cseKey{}, false
	}
	rhs := prog.Instrs[ins.Arg1]
	if !isCSELeaf(rhs, unsafeSyms) {
		return cseKey{}, false
	}

	k := cseKey{
		subOp:  ins.SubOp,
		flags:  ins.Flags,
		lhsK:   lhs.Kind,
		lhsSub: lhs.SubOp,
		lhsDat: lhs.Data,
		rhsK:   rhs.Kind,
		rhsSub: rhs.SubOp,
		rhsDat: rhs.Data,
	}

	// Canonicalize commutative operators so (a + b) and (b + a) share the same value number
	if ins.SubOp == PrimAdd || ins.SubOp == PrimMul || ins.SubOp == PrimEq ||
		ins.SubOp == PrimNeq || ins.SubOp == PrimAnd || ins.SubOp == PrimOr {
		if k.lhsK > k.rhsK || (k.lhsK == k.rhsK && (k.lhsSub > k.rhsSub || (k.lhsSub == k.rhsSub && k.lhsDat > k.rhsDat))) {
			k.lhsK, k.rhsK = k.rhsK, k.lhsK
			k.lhsSub, k.rhsSub = k.rhsSub, k.lhsSub
			k.lhsDat, k.rhsDat = k.rhsDat, k.lhsDat
		}
	}
	return k, true
}

func eliminateCommonSubexprs(prog *Program, id InstrID, scopedCSE map[cseKey]token.StringID, unsafeSyms map[token.StringID]bool) bool {
	if id == NoInstr || int(id) >= len(prog.Instrs) {
		return false
	}
	changed := false
	ins := prog.Instrs[id]

	switch ins.Kind {
	case IRConst, IRVar:
		return false

	case IRLet:
		if eliminateCommonSubexprs(prog, InstrID(ins.Arg0), scopedCSE, unsafeSyms) {
			changed = true
		}
		sym := token.StringID(ins.Data)
		nextCSE := scopedCSE
		if sym != token.NoString && ins.Flags == 0 && !unsafeSyms[sym] && int(ins.Arg0) < len(prog.Instrs) {
			rhs := prog.Instrs[ins.Arg0]
			if key, ok := buildCSEKey(prog, rhs, unsafeSyms); ok {
				if existingSym, found := scopedCSE[key]; found && existingSym != sym {
					prog.Instrs[ins.Arg0] = IRInstr{Kind: IRVar, Data: uint64(existingSym)}
					changed = true
				} else {
					nextCSE = cloneCSEMap(scopedCSE)
					nextCSE[key] = sym
				}
			}
		}
		if InstrID(ins.Arg1) != NoInstr {
			if eliminateCommonSubexprs(prog, InstrID(ins.Arg1), nextCSE, unsafeSyms) {
				changed = true
			}
		}
		return changed

	case IRFn:
		return eliminateCommonSubexprs(prog, InstrID(ins.Arg0), cloneCSEMap(scopedCSE), unsafeSyms)

	case IRApp:
		if eliminateCommonSubexprs(prog, InstrID(ins.Arg0), scopedCSE, unsafeSyms) {
			changed = true
		}
		if eliminateCommonSubexprs(prog, InstrID(ins.Arg1), scopedCSE, unsafeSyms) {
			changed = true
		}
		return changed

	case IRSwitch:
		if eliminateCommonSubexprs(prog, InstrID(ins.Arg0), scopedCSE, unsafeSyms) {
			changed = true
		}
		count := uint32(ins.Data)
		for i := uint32(0); i < count; i++ {
			idx := ins.Arg1 + i*2 + 1
			if int(idx) < len(prog.Extra) {
				if eliminateCommonSubexprs(prog, InstrID(prog.Extra[idx]), cloneCSEMap(scopedCSE), unsafeSyms) {
					changed = true
				}
			}
		}
		if eliminateCommonSubexprs(prog, InstrID(ins.Aux), cloneCSEMap(scopedCSE), unsafeSyms) {
			changed = true
		}
		return changed

	case IRPrim:
		if ins.SubOp == PrimSeqBlock {
			blockCSE := cloneCSEMap(scopedCSE)
			for i := uint32(0); i < ins.Arg1; i++ {
				idx := ins.Arg0 + i
				if int(idx) >= len(prog.Extra) {
					continue
				}
				chID := InstrID(prog.Extra[idx])
				if eliminateCommonSubexprs(prog, chID, blockCSE, unsafeSyms) {
					changed = true
				}
				if int(chID) < len(prog.Instrs) {
					chIns := prog.Instrs[chID]
					if chIns.Kind == IRLet && chIns.Flags == 0 && InstrID(chIns.Arg1) == NoInstr && int(chIns.Arg0) < len(prog.Instrs) {
						sym := token.StringID(chIns.Data)
						if sym != token.NoString && !unsafeSyms[sym] {
							if key, ok := buildCSEKey(prog, prog.Instrs[chIns.Arg0], unsafeSyms); ok {
								if _, exists := blockCSE[key]; !exists {
									blockCSE[key] = sym
								}
							}
						}
					}
				}
			}
			return changed
		}

		if eliminateCommonSubexprs(prog, InstrID(ins.Arg0), scopedCSE, unsafeSyms) {
			changed = true
		}
		if InstrID(ins.Arg1) != NoInstr {
			if eliminateCommonSubexprs(prog, InstrID(ins.Arg1), scopedCSE, unsafeSyms) {
				changed = true
			}
		}
		if key, ok := buildCSEKey(prog, prog.Instrs[id], unsafeSyms); ok {
			if existingSym, found := scopedCSE[key]; found {
				prog.Instrs[id] = IRInstr{Kind: IRVar, Data: uint64(existingSym)}
				changed = true
			}
		}
		return changed
	}
	return false
}

func isIntConstVal(ins IRInstr, val int64) bool {
	return ins.Kind == IRConst && ins.SubOp == ConstInt && int64(ins.Data) == val
}

func isFloatConstVal(ins IRInstr, val float64) bool {
	return ins.Kind == IRConst && ins.SubOp == ConstFloat && math.Float64frombits(ins.Data) == val
}

func isBoolConstVal(ins IRInstr, val bool) bool {
	return ins.Kind == IRConst && ins.SubOp == ConstBool && (ins.Data != 0) == val
}

func samePureOperand(lhs, rhs IRInstr, unsafeSyms map[token.StringID]bool) bool {
	if lhs.Kind != rhs.Kind || lhs.SubOp != rhs.SubOp || lhs.Data != rhs.Data {
		return false
	}
	if lhs.Kind == IRConst {
		return lhs.SubOp != ConstFloat // avoid NaN == NaN folding
	}
	if lhs.Kind == IRVar {
		sym := token.StringID(lhs.Data)
		return sym != token.NoString && !unsafeSyms[sym]
	}
	return false
}

func tryFoldPrim(prog *Program, ins IRInstr, unsafeSyms map[token.StringID]bool) (IRInstr, bool) {
	if ins.SubOp == PrimSeqBlock && ins.Arg1 == 1 {
		childID := prog.Extra[ins.Arg0]
		if int(childID) < len(prog.Instrs) {
			return prog.Instrs[childID], true
		}
	}

	if int(ins.Arg0) >= len(prog.Instrs) {
		return IRInstr{}, false
	}
	lhs := prog.Instrs[ins.Arg0]
	if ins.SubOp == PrimNeg || ins.SubOp == PrimNot {
		if folded, ok := evalFoldedOperands(prog, ins.SubOp, lhs, IRInstr{}, false); ok {
			return folded, true
		}
		// Double negation elimination: -(-x) -> x, not (not x) -> x
		if lhs.Kind == IRPrim && lhs.SubOp == ins.SubOp && int(lhs.Arg0) < len(prog.Instrs) {
			return prog.Instrs[lhs.Arg0], true
		}
		return IRInstr{}, false
	}
	if ins.Arg1 == uint32(NoInstr) || int(ins.Arg1) >= len(prog.Instrs) {
		return IRInstr{}, false
	}
	rhs := prog.Instrs[ins.Arg1]
	if folded, ok := evalFoldedOperands(prog, ins.SubOp, lhs, rhs, true); ok {
		return folded, true
	}
	return tryAlgebraicSimplify(prog, ins, lhs, rhs, unsafeSyms)
}

func tryAlgebraicSimplify(prog *Program, ins IRInstr, lhs, rhs IRInstr, unsafeSyms map[token.StringID]bool) (IRInstr, bool) {
	switch ins.SubOp {
	case PrimAdd:
		if isIntConstVal(rhs, 0) || isFloatConstVal(rhs, 0) {
			return lhs, true
		}
		if isIntConstVal(lhs, 0) || isFloatConstVal(lhs, 0) {
			return rhs, true
		}
	case PrimSub:
		if isIntConstVal(rhs, 0) || isFloatConstVal(rhs, 0) {
			return lhs, true
		}
		if ins.Flags == 0 && samePureOperand(lhs, rhs, unsafeSyms) {
			return IRInstr{Kind: IRConst, SubOp: ConstInt, Data: 0}, true
		}
	case PrimMul:
		if isIntConstVal(rhs, 1) || isFloatConstVal(rhs, 1) {
			return lhs, true
		}
		if isIntConstVal(lhs, 1) || isFloatConstVal(lhs, 1) {
			return rhs, true
		}
		if isIntConstVal(rhs, 0) && isPureExpr(prog, lhs, unsafeSyms) {
			return rhs, true
		}
		if isIntConstVal(lhs, 0) && isPureExpr(prog, rhs, unsafeSyms) {
			return lhs, true
		}
	case PrimDiv:
		if isIntConstVal(rhs, 1) || isFloatConstVal(rhs, 1) {
			return lhs, true
		}
	case PrimAnd:
		if isBoolConstVal(rhs, true) {
			return lhs, true
		}
		if isBoolConstVal(lhs, true) {
			return rhs, true
		}
		if isBoolConstVal(lhs, false) {
			return lhs, true
		}
		if isBoolConstVal(rhs, false) && isPureExpr(prog, lhs, unsafeSyms) {
			return rhs, true
		}
		if samePureOperand(lhs, rhs, unsafeSyms) {
			return lhs, true
		}
	case PrimOr:
		if isBoolConstVal(rhs, false) {
			return lhs, true
		}
		if isBoolConstVal(lhs, false) {
			return rhs, true
		}
		if isBoolConstVal(lhs, true) {
			return lhs, true
		}
		if isBoolConstVal(rhs, true) && isPureExpr(prog, lhs, unsafeSyms) {
			return rhs, true
		}
		if samePureOperand(lhs, rhs, unsafeSyms) {
			return lhs, true
		}
	case PrimEq, PrimLe, PrimGe:
		if samePureOperand(lhs, rhs, unsafeSyms) {
			return boolConstInstr(true), true
		}
	case PrimNeq, PrimLt, PrimGt:
		if samePureOperand(lhs, rhs, unsafeSyms) {
			return boolConstInstr(false), true
		}
	}
	return IRInstr{}, false
}

func evalFoldedOperands(prog *Program, subOp uint16, lhs, rhs IRInstr, isBinary bool) (IRInstr, bool) {
	if lhs.Kind != IRConst {
		return IRInstr{}, false
	}

	// Unary primitives: PrimNeg, PrimNot
	if !isBinary {
		if subOp == PrimNeg {
			switch lhs.SubOp {
			case ConstInt:
				return IRInstr{Kind: IRConst, SubOp: ConstInt, Data: uint64(-int64(lhs.Data))}, true
			case ConstFloat:
				f := -math.Float64frombits(lhs.Data)
				return IRInstr{Kind: IRConst, SubOp: ConstFloat, Data: math.Float64bits(f)}, true
			}
			return IRInstr{}, false
		}
		if subOp == PrimNot && lhs.SubOp == ConstBool {
			b := uint64(0)
			if lhs.Data == 0 {
				b = 1
			}
			return IRInstr{Kind: IRConst, SubOp: ConstBool, Data: b}, true
		}
		return IRInstr{}, false
	}

	if rhs.Kind != IRConst || lhs.SubOp != rhs.SubOp {
		return IRInstr{}, false
	}

	switch lhs.SubOp {
	case ConstInt:
		a := int64(lhs.Data)
		b := int64(rhs.Data)
		switch subOp {
		case PrimAdd:
			return IRInstr{Kind: IRConst, SubOp: ConstInt, Data: uint64(a + b)}, true
		case PrimSub:
			return IRInstr{Kind: IRConst, SubOp: ConstInt, Data: uint64(a - b)}, true
		case PrimMul:
			return IRInstr{Kind: IRConst, SubOp: ConstInt, Data: uint64(a * b)}, true
		case PrimDiv:
			if b != 0 {
				return IRInstr{Kind: IRConst, SubOp: ConstInt, Data: uint64(a / b)}, true
			}
		case PrimMod:
			if b != 0 {
				return IRInstr{Kind: IRConst, SubOp: ConstInt, Data: uint64(a % b)}, true
			}
		case PrimEq:
			return boolConstInstr(a == b), true
		case PrimNeq:
			return boolConstInstr(a != b), true
		case PrimLt:
			return boolConstInstr(a < b), true
		case PrimLe:
			return boolConstInstr(a <= b), true
		case PrimGt:
			return boolConstInstr(a > b), true
		case PrimGe:
			return boolConstInstr(a >= b), true
		}

	case ConstFloat:
		a := math.Float64frombits(lhs.Data)
		b := math.Float64frombits(rhs.Data)
		switch subOp {
		case PrimAdd:
			return IRInstr{Kind: IRConst, SubOp: ConstFloat, Data: math.Float64bits(a + b)}, true
		case PrimSub:
			return IRInstr{Kind: IRConst, SubOp: ConstFloat, Data: math.Float64bits(a - b)}, true
		case PrimMul:
			return IRInstr{Kind: IRConst, SubOp: ConstFloat, Data: math.Float64bits(a * b)}, true
		case PrimDiv:
			if b != 0 {
				return IRInstr{Kind: IRConst, SubOp: ConstFloat, Data: math.Float64bits(a / b)}, true
			}
		case PrimEq:
			return boolConstInstr(a == b), true
		case PrimNeq:
			return boolConstInstr(a != b), true
		case PrimLt:
			return boolConstInstr(a < b), true
		case PrimLe:
			return boolConstInstr(a <= b), true
		case PrimGt:
			return boolConstInstr(a > b), true
		case PrimGe:
			return boolConstInstr(a >= b), true
		}

	case ConstBool:
		a := lhs.Data != 0
		b := rhs.Data != 0
		switch subOp {
		case PrimAnd:
			return boolConstInstr(a && b), true
		case PrimOr:
			return boolConstInstr(a || b), true
		case PrimEq:
			return boolConstInstr(a == b), true
		case PrimNeq:
			return boolConstInstr(a != b), true
		}

	case ConstString:
		s1 := prog.Strings.String(token.StringID(lhs.Data))
		s2 := prog.Strings.String(token.StringID(rhs.Data))
		switch subOp {
		case PrimAdd:
			joined := prog.Strings.InternString(s1 + s2)
			return IRInstr{Kind: IRConst, SubOp: ConstString, Data: uint64(joined)}, true
		case PrimEq:
			return boolConstInstr(s1 == s2), true
		case PrimNeq:
			return boolConstInstr(s1 != s2), true
		}
	}

	return IRInstr{}, false
}

func tryFoldSwitch(prog *Program, ins IRInstr) (InstrID, bool) {
	if int(ins.Arg0) >= len(prog.Instrs) {
		return NoInstr, false
	}
	scrut := prog.Instrs[ins.Arg0]
	if scrut.Kind != IRConst {
		return NoInstr, false
	}
	if scrut.SubOp != ConstBool && scrut.SubOp != ConstInt {
		return NoInstr, false
	}

	tag := uint32(scrut.Data)
	count := uint32(ins.Data)
	for i := uint32(0); i < count; i++ {
		idx := ins.Arg1 + i*2
		if int(idx+1) < len(prog.Extra) {
			caseTag := prog.Extra[idx]
			target := InstrID(prog.Extra[idx+1])
			if caseTag == tag {
				return target, true
			}
		}
	}
	if ins.Aux != uint32(NoInstr) {
		return InstrID(ins.Aux), true
	}
	return NoInstr, false
}

func boolConstInstr(v bool) IRInstr {
	var d uint64
	if v {
		d = 1
	}
	return IRInstr{Kind: IRConst, SubOp: ConstBool, Data: d}
}
