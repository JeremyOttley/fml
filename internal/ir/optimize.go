package ir

import (
	"math"
	"strings"

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
		allocEnv := make(map[token.StringID]IRInstr)
		userSyms := make(map[token.StringID]bool)

		// Collect single-assignment immutable IRLet bindings first so scoped `let ... in body`
		// bindings (where IRLet has a higher InstrID than body) propagate in a single pass.
		for _, ins := range prog.Instrs {
			if ins.Kind == IRFn && ins.Data != 0 {
				userSyms[token.StringID(ins.Data)] = true
			}
			if ins.Kind != IRLet {
				continue
			}
			sym := token.StringID(ins.Data)
			if sym != token.NoString {
				userSyms[sym] = true
			}
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
						if aIns, ok := allocEnv[srcSym]; ok {
							allocEnv[sym] = aIns
						}
					}
				case IRFn:
					fnEnv[sym] = rhs
				case IRAlloc:
					if isPureExpr(prog, rhs, unsafeSyms) {
						allocEnv[sym] = rhs
					}
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
				if inlined, ok := tryInlineApp(prog, ins, fnEnv, allocEnv, userSyms, unsafeSyms); ok {
					prog.Instrs[i] = inlined
					changed = true
				}

			case IRPrim:
				if folded, ok := tryFoldPrim(prog, ins, unsafeSyms); ok {
					prog.Instrs[i] = folded
					changed = true
				}

			case IRLoad:
				if folded, ok := tryFoldLoad(prog, ins, allocEnv, unsafeSyms); ok {
					prog.Instrs[i] = folded
					changed = true
				}

			case IRSwitch:
				if folded, ok := tryFoldSwitchInstr(prog, ins, allocEnv, unsafeSyms); ok {
					prog.Instrs[i] = folded
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

func resolveKnownAlloc(prog *Program, id InstrID, allocEnv map[token.StringID]IRInstr, unsafeSyms map[token.StringID]bool) (IRInstr, bool) {
	if id == NoInstr || int(id) >= len(prog.Instrs) {
		return IRInstr{}, false
	}
	ins := prog.Instrs[id]
	if ins.Kind == IRVar {
		sym := token.StringID(ins.Data)
		if !unsafeSyms[sym] {
			if lookedUp, ok := allocEnv[sym]; ok {
				ins = lookedUp
			}
		}
	}
	if ins.Kind == IRAlloc && isPureExpr(prog, ins, unsafeSyms) {
		return ins, true
	}
	return IRInstr{}, false
}

func tryInlineApp(prog *Program, appIns IRInstr, fnEnv, allocEnv map[token.StringID]IRInstr, userSyms, unsafeSyms map[token.StringID]bool) (IRInstr, bool) {
	if int(appIns.Arg0) >= len(prog.Instrs) || int(appIns.Arg1) >= len(prog.Instrs) {
		return IRInstr{}, false
	}
	fnIns := prog.Instrs[appIns.Arg0]
	if fnIns.Kind == IRVar {
		fnSym := token.StringID(fnIns.Data)
		if !userSyms[fnSym] && !unsafeSyms[fnSym] && prog.Strings != nil {
			switch prog.Strings.String(fnSym) {
			case "id":
				return prog.Instrs[appIns.Arg1], true
			case "fst":
				return IRInstr{Kind: IRLoad, SubOp: LoadTupleElem, Arg0: appIns.Arg1, Arg1: 0}, true
			case "snd":
				return IRInstr{Kind: IRLoad, SubOp: LoadTupleElem, Arg0: appIns.Arg1, Arg1: 1}, true
			}
		}
	}

	// Unwind curried application spine `f arg0 arg1 ...` (including desugared `|>` pipelines).
	var revArgs []InstrID
	currApp := appIns
	var headIns IRInstr
	for len(revArgs) < 8 {
		if int(currApp.Arg0) >= len(prog.Instrs) || int(currApp.Arg1) >= len(prog.Instrs) {
			return IRInstr{}, false
		}
		revArgs = append(revArgs, InstrID(currApp.Arg1))
		nextIns := prog.Instrs[currApp.Arg0]
		if nextIns.Kind == IRApp {
			currApp = nextIns
			continue
		}
		headIns = nextIns
		break
	}
	if len(revArgs) == 0 {
		return IRInstr{}, false
	}

	// Option / Result / List stdlib intrinsic fusion on known allocations
	if prog.Strings != nil {
		var qualName string
		if headIns.Kind == IRLoad && headIns.SubOp == LoadRecordField && headIns.Aux != 0 {
			qSym := token.StringID(headIns.Aux)
			if !userSyms[qSym] && !unsafeSyms[qSym] {
				qualName = prog.Strings.String(qSym)
			}
		} else if headIns.Kind == IRVar && headIns.Data != 0 {
			qSym := token.StringID(headIns.Data)
			if !userSyms[qSym] && !unsafeSyms[qSym] {
				qualName = prog.Strings.String(qSym)
			}
		}
		if qualName != "" {
			if fused, ok := tryFuseStdlibCombinator(prog, qualName, revArgs, fnEnv, allocEnv, userSyms, unsafeSyms); ok {
				return fused, true
			}
		}
	}

	if headIns.Kind == IRVar {
		lookedUp, ok := fnEnv[token.StringID(headIns.Data)]
		if !ok {
			return IRInstr{}, false
		}
		headIns = lookedUp
	}
	if headIns.Kind != IRFn {
		return IRInstr{}, false
	}

	argIDs := make([]InstrID, len(revArgs))
	for i := range revArgs {
		argIDs[i] = revArgs[len(revArgs)-1-i]
	}

	paramToArg := make(map[token.StringID]InstrID, len(argIDs))
	currFn := headIns
	var bodyID InstrID
	for idx, argID := range argIDs {
		if currFn.Kind != IRFn {
			return IRInstr{}, false
		}
		param := token.StringID(currFn.Data)
		if param == token.NoString || int(currFn.Arg0) >= len(prog.Instrs) {
			return IRInstr{}, false
		}
		if _, dup := paramToArg[param]; dup {
			return IRInstr{}, false
		}
		if !isPureExpr(prog, prog.Instrs[argID], unsafeSyms) {
			return IRInstr{}, false
		}
		paramToArg[param] = argID
		bodyID = InstrID(currFn.Arg0)
		if idx+1 < len(argIDs) {
			currFn = prog.Instrs[bodyID]
		}
	}

	bodyIns := prog.Instrs[bodyID]
	if bodyIns.Kind == IRFn {
		// Partial application; wait for outer spine to supply remaining arguments.
		return IRInstr{}, false
	}

	paramUses := make(map[token.StringID]int, len(paramToArg))
	markReachableUses(prog, bodyID, paramUses, make(map[InstrID]bool))
	allConstArgs := true
	maxParamUses := 0
	for param, argID := range paramToArg {
		uses := paramUses[param]
		if uses > maxParamUses {
			maxParamUses = uses
		}
		argIns := prog.Instrs[argID]
		if argIns.Kind != IRConst {
			allConstArgs = false
		}
		if uses > 1 && argIns.Kind != IRConst && argIns.Kind != IRVar {
			return IRInstr{}, false
		}
	}

	return tryInlineBodyExpr(prog, bodyID, bodyIns, paramToArg, allConstArgs, maxParamUses, unsafeSyms)
}

func tryInlineBodyExpr(prog *Program, bodyID InstrID, bodyIns IRInstr, paramToArg map[token.StringID]InstrID, allConstArgs bool, maxParamUses int, unsafeSyms map[token.StringID]bool) (IRInstr, bool) {
	switch bodyIns.Kind {
	case IRConst:
		return bodyIns, true
	case IRVar:
		if argID, ok := paramToArg[token.StringID(bodyIns.Data)]; ok {
			return prog.Instrs[argID], true
		}
		return bodyIns, true
	case IRAlloc:
		if bodyIns.SubOp == AllocADT {
			if InstrID(bodyIns.Arg0) == NoInstr {
				return bodyIns, true
			}
			if int(bodyIns.Arg0) < len(prog.Instrs) {
				pIns := prog.Instrs[bodyIns.Arg0]
				if pIns.Kind == IRVar {
					if argID, ok := paramToArg[token.StringID(pIns.Data)]; ok {
						return IRInstr{Kind: IRAlloc, SubOp: AllocADT, Arg0: uint32(argID), Data: bodyIns.Data}, true
					}
					return bodyIns, true
				}
				if inlinedPayload, ok := tryInlineBodyExpr(prog, InstrID(bodyIns.Arg0), pIns, paramToArg, allConstArgs, maxParamUses, unsafeSyms); ok {
					pID := prog.Emit(inlinedPayload)
					return IRInstr{Kind: IRAlloc, SubOp: AllocADT, Arg0: uint32(pID), Data: bodyIns.Data}, true
				}
			}
		}
	case IRPrim:
		if bodyIns.SubOp == PrimSeqBlock || bodyIns.SubOp == PrimWhile || bodyIns.SubOp == PrimForIn || bodyIns.SubOp == PrimForTo || int(bodyIns.Arg0) >= len(prog.Instrs) {
			return IRInstr{}, false
		}
		origLhs := prog.Instrs[bodyIns.Arg0]
		if bodyIns.SubOp == PrimNeg || bodyIns.SubOp == PrimNot {
			if origLhs.Kind == IRConst || origLhs.Kind == IRVar {
				arg0ID := InstrID(bodyIns.Arg0)
				if origLhs.Kind == IRVar {
					if mapped, ok := paramToArg[token.StringID(origLhs.Data)]; ok {
						arg0ID = mapped
					}
				}
				inlined := IRInstr{
					Kind:  IRPrim,
					SubOp: bodyIns.SubOp,
					Arg0:  uint32(arg0ID),
					Arg1:  uint32(NoInstr),
				}
				if folded, ok := tryFoldPrim(prog, inlined, unsafeSyms); ok {
					return folded, true
				}
				return inlined, true
			}
			if allConstArgs && maxParamUses <= 1 {
				return evalInlineConstExpr(prog, bodyID, paramToArg, 0)
			}
			return IRInstr{}, false
		}

		if bodyIns.Arg1 == uint32(NoInstr) || int(bodyIns.Arg1) >= len(prog.Instrs) {
			return IRInstr{}, false
		}
		origRhs := prog.Instrs[bodyIns.Arg1]
		if !allConstArgs && (bodyIns.SubOp == PrimDiv || bodyIns.SubOp == PrimMod) {
			return IRInstr{}, false
		}
		if (origLhs.Kind == IRConst || origLhs.Kind == IRVar) && (origRhs.Kind == IRConst || origRhs.Kind == IRVar) {
			arg0ID := InstrID(bodyIns.Arg0)
			if origLhs.Kind == IRVar {
				if mapped, ok := paramToArg[token.StringID(origLhs.Data)]; ok {
					arg0ID = mapped
				}
			}
			arg1ID := InstrID(bodyIns.Arg1)
			if origRhs.Kind == IRVar {
				if mapped, ok := paramToArg[token.StringID(origRhs.Data)]; ok {
					arg1ID = mapped
				}
			}
			inlined := IRInstr{
				Kind:  IRPrim,
				SubOp: bodyIns.SubOp,
				Arg0:  uint32(arg0ID),
				Arg1:  uint32(arg1ID),
				Data:  bodyIns.Data,
				Aux:   bodyIns.Aux,
			}
			if folded, ok := tryFoldPrim(prog, inlined, unsafeSyms); ok {
				return folded, true
			}
			return inlined, true
		}

		if allConstArgs && maxParamUses <= 1 {
			return evalInlineConstExpr(prog, bodyID, paramToArg, 0)
		}
	}
	return IRInstr{}, false
}

func tryFuseStdlibCombinator(prog *Program, qualName string, revArgs []InstrID, fnEnv, allocEnv map[token.StringID]IRInstr, userSyms, unsafeSyms map[token.StringID]bool) (IRInstr, bool) {
	switch len(revArgs) {
	case 1:
		argID := revArgs[0]
		allocIns, ok := resolveKnownAlloc(prog, argID, allocEnv, unsafeSyms)
		if !ok {
			return IRInstr{}, false
		}
		switch qualName {
		case "Option.isSome":
			if allocIns.SubOp == AllocADT {
				tag := prog.Strings.String(token.StringID(allocIns.Data))
				return boolConstInstr(tag == "Some"), true
			}
		case "Option.isNone":
			if allocIns.SubOp == AllocADT {
				tag := prog.Strings.String(token.StringID(allocIns.Data))
				return boolConstInstr(tag == "None"), true
			}
		case "Result.isOk":
			if allocIns.SubOp == AllocADT {
				tag := prog.Strings.String(token.StringID(allocIns.Data))
				return boolConstInstr(tag == "Ok"), true
			}
		case "Result.isError":
			if allocIns.SubOp == AllocADT {
				tag := prog.Strings.String(token.StringID(allocIns.Data))
				return boolConstInstr(tag == "Error"), true
			}
		case "List.isEmpty":
			if allocIns.SubOp == AllocList {
				return boolConstInstr(allocIns.Arg1 == 0), true
			}
		case "List.length":
			if allocIns.SubOp == AllocList {
				return IRInstr{Kind: IRConst, SubOp: ConstInt, Data: uint64(allocIns.Arg1)}, true
			}
		case "List.head":
			if allocIns.SubOp == AllocList && allocIns.Arg1 > 0 && int(allocIns.Arg0) < len(prog.Extra) {
				headID := prog.Extra[allocIns.Arg0]
				if int(headID) < len(prog.Instrs) {
					return prog.Instrs[headID], true
				}
			}
		}

	case 2:
		firstArg := revArgs[1]
		secondArg := revArgs[0]
		if int(firstArg) >= len(prog.Instrs) || int(secondArg) >= len(prog.Instrs) {
			return IRInstr{}, false
		}
		firstIns := prog.Instrs[firstArg]
		adtIns, ok := resolveKnownAlloc(prog, secondArg, allocEnv, unsafeSyms)
		if !ok || adtIns.SubOp != AllocADT {
			return IRInstr{}, false
		}
		tag := prog.Strings.String(token.StringID(adtIns.Data))
		switch qualName {
		case "Option.defaultValue":
			if tag == "Some" && InstrID(adtIns.Arg0) != NoInstr && int(adtIns.Arg0) < len(prog.Instrs) && isPureExpr(prog, firstIns, unsafeSyms) {
				return prog.Instrs[adtIns.Arg0], true
			}
			if tag == "None" {
				return firstIns, true
			}
		case "Result.defaultValue":
			if tag == "Ok" && InstrID(adtIns.Arg0) != NoInstr && int(adtIns.Arg0) < len(prog.Instrs) && isPureExpr(prog, firstIns, unsafeSyms) {
				return prog.Instrs[adtIns.Arg0], true
			}
			if tag == "Error" {
				return firstIns, true
			}
		case "Option.orElse":
			if tag == "Some" && isPureExpr(prog, firstIns, unsafeSyms) {
				return adtIns, true
			}
			if tag == "None" {
				return firstIns, true
			}
		case "Result.orElse":
			if tag == "Ok" && isPureExpr(prog, firstIns, unsafeSyms) {
				return adtIns, true
			}
			if tag == "Error" {
				return firstIns, true
			}
		case "Option.bind":
			if tag == "Some" && InstrID(adtIns.Arg0) != NoInstr {
				app := IRInstr{Kind: IRApp, Arg0: uint32(firstArg), Arg1: adtIns.Arg0}
				if inlined, okIn := tryInlineApp(prog, app, fnEnv, allocEnv, userSyms, unsafeSyms); okIn {
					return inlined, true
				}
				return app, true
			}
			if tag == "None" && isPureExpr(prog, firstIns, unsafeSyms) {
				return adtIns, true
			}
		case "Result.bind":
			if tag == "Ok" && InstrID(adtIns.Arg0) != NoInstr {
				app := IRInstr{Kind: IRApp, Arg0: uint32(firstArg), Arg1: adtIns.Arg0}
				if inlined, okIn := tryInlineApp(prog, app, fnEnv, allocEnv, userSyms, unsafeSyms); okIn {
					return inlined, true
				}
				return app, true
			}
			if tag == "Error" && isPureExpr(prog, firstIns, unsafeSyms) {
				return adtIns, true
			}
		case "Option.map":
			if tag == "None" && isPureExpr(prog, firstIns, unsafeSyms) {
				return adtIns, true
			}
			if tag == "Some" && InstrID(adtIns.Arg0) != NoInstr {
				app := IRInstr{Kind: IRApp, Arg0: uint32(firstArg), Arg1: adtIns.Arg0}
				if inlined, okIn := tryInlineApp(prog, app, fnEnv, allocEnv, userSyms, unsafeSyms); okIn {
					valID := prog.Emit(inlined)
					return IRInstr{Kind: IRAlloc, SubOp: AllocADT, Arg0: uint32(valID), Data: adtIns.Data}, true
				}
			}
		case "Result.map":
			if tag == "Error" && isPureExpr(prog, firstIns, unsafeSyms) {
				return adtIns, true
			}
			if tag == "Ok" && InstrID(adtIns.Arg0) != NoInstr {
				app := IRInstr{Kind: IRApp, Arg0: uint32(firstArg), Arg1: adtIns.Arg0}
				if inlined, okIn := tryInlineApp(prog, app, fnEnv, allocEnv, userSyms, unsafeSyms); okIn {
					valID := prog.Emit(inlined)
					return IRInstr{Kind: IRAlloc, SubOp: AllocADT, Arg0: uint32(valID), Data: adtIns.Data}, true
				}
			}
		}
	}
	return IRInstr{}, false
}

func evalInlineConstExpr(prog *Program, id InstrID, paramToArg map[token.StringID]InstrID, depth int) (IRInstr, bool) {
	if depth > 4 || id == NoInstr || int(id) >= len(prog.Instrs) {
		return IRInstr{}, false
	}
	ins := prog.Instrs[id]
	switch ins.Kind {
	case IRConst:
		return ins, true
	case IRVar:
		if argID, ok := paramToArg[token.StringID(ins.Data)]; ok && int(argID) < len(prog.Instrs) {
			argIns := prog.Instrs[argID]
			if argIns.Kind == IRConst {
				return argIns, true
			}
		}
		return IRInstr{}, false
	case IRPrim:
		if ins.SubOp == PrimSeqBlock || int(ins.Arg0) >= len(prog.Instrs) {
			return IRInstr{}, false
		}
		lhs, ok := evalInlineConstExpr(prog, InstrID(ins.Arg0), paramToArg, depth+1)
		if !ok {
			return IRInstr{}, false
		}
		if ins.SubOp == PrimNeg || ins.SubOp == PrimNot {
			return evalFoldedOperands(prog, ins.SubOp, lhs, IRInstr{}, false)
		}
		if ins.Arg1 == uint32(NoInstr) || int(ins.Arg1) >= len(prog.Instrs) {
			return IRInstr{}, false
		}
		rhs, ok := evalInlineConstExpr(prog, InstrID(ins.Arg1), paramToArg, depth+1)
		if !ok {
			return IRInstr{}, false
		}
		return evalFoldedOperands(prog, ins.SubOp, lhs, rhs, true)
	}
	return IRInstr{}, false
}

func eliminateDeadBindings(prog *Program, unsafeSyms map[token.StringID]bool) bool {
	changed := false

	// 1. Eliminate scoped `let x = rhs in x` and `let unused = pure in body`
	for i := 0; i < len(prog.Instrs); i++ {
		ins := prog.Instrs[i]
		if ins.Kind == IRLet && ins.Flags == 0 && InstrID(ins.Arg1) != NoInstr &&
			int(ins.Arg0) < len(prog.Instrs) && int(ins.Arg1) < len(prog.Instrs) {
			sym := token.StringID(ins.Data)
			if !unsafeSyms[sym] {
				bodyIns := prog.Instrs[ins.Arg1]
				if bodyIns.Kind == IRVar && token.StringID(bodyIns.Data) == sym {
					prog.Instrs[i] = prog.Instrs[ins.Arg0]
					changed = true
					continue
				}
				if isPureExpr(prog, prog.Instrs[ins.Arg0], unsafeSyms) {
					bodyUses := make(map[token.StringID]int)
					markReachableUses(prog, InstrID(ins.Arg1), bodyUses, make(map[InstrID]bool))
					if bodyUses[sym] == 0 {
						prog.Instrs[i] = prog.Instrs[ins.Arg1]
						changed = true
						continue
					}
					rhs := prog.Instrs[ins.Arg0]
					if bodyUses[sym] == 1 && isInlinablePurePrim(prog, rhs, unsafeSyms) &&
						countSameScopeVarUses(prog, InstrID(ins.Arg1), sym) == 1 {
						replaceSameScopeVar(prog, InstrID(ins.Arg1), sym, rhs)
						prog.Instrs[i] = prog.Instrs[ins.Arg1]
						changed = true
					}
				}
			}
		}
	}

	// 2. Recompute reachable variable uses from Root (excluding dead top-level IRLet RHSs)
	usedSyms := computeLiveSymbolUses(prog)

	// 3. Prune dead pure non-final elements and fuse single-use pure bindings in PrimSeqBlock instructions
	for i := 0; i < len(prog.Instrs); i++ {
		ins := prog.Instrs[i]
		if ins.Kind != IRPrim || ins.SubOp != PrimSeqBlock || ins.Arg1 == 0 {
			continue
		}
		if ins.Arg1 == 1 {
			chID := InstrID(prog.Extra[ins.Arg0])
			if int(chID) < len(prog.Instrs) {
				prog.Instrs[i] = prog.Instrs[chID]
				changed = true
			}
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
					rhs := prog.Instrs[chIns.Arg0]
					if !unsafeSyms[sym] && usedSyms[sym] == 0 && isPureExpr(prog, rhs, unsafeSyms) {
						changed = true
						continue
					}
					if !unsafeSyms[sym] && usedSyms[sym] == 1 && isInlinablePurePrim(prog, rhs, unsafeSyms) {
						remUses := 0
						for k := j + 1; k < ins.Arg1; k++ {
							remUses += countSameScopeVarUses(prog, InstrID(prog.Extra[ins.Arg0+k]), sym)
						}
						if remUses == 1 {
							for k := j + 1; k < ins.Arg1; k++ {
								replaceSameScopeVar(prog, InstrID(prog.Extra[ins.Arg0+k]), sym, rhs)
							}
							usedSyms[sym] = 0
							changed = true
							continue
						}
					}
				}
			}
			// If the final child of a PrimSeqBlock is itself a PrimSeqBlock, splice its children flat
			if isLast && int(chID) < len(prog.Instrs) {
				chIns := prog.Instrs[chID]
				if chIns.Kind == IRPrim && chIns.SubOp == PrimSeqBlock && chIns.Arg1 > 0 && int(chIns.Arg0+chIns.Arg1) <= len(prog.Extra) {
					for k := uint32(0); k < chIns.Arg1; k++ {
						kept = append(kept, InstrID(prog.Extra[chIns.Arg0+k]))
					}
					changed = true
					continue
				}
			}
			kept = append(kept, chID)
		}
		// Collapse trailing `[..., IRLet(x, rhs), IRVar(x)]` into `[..., rhs]` when x has 1 use
		if len(kept) >= 2 {
			prevID := kept[len(kept)-2]
			lastID := kept[len(kept)-1]
			if int(prevID) < len(prog.Instrs) && int(lastID) < len(prog.Instrs) {
				prevIns := prog.Instrs[prevID]
				lastIns := prog.Instrs[lastID]
				if prevIns.Kind == IRLet && prevIns.Flags == 0 && InstrID(prevIns.Arg1) == NoInstr &&
					lastIns.Kind == IRVar && lastIns.Data == prevIns.Data && prevIns.Data != 0 {
					sym := token.StringID(prevIns.Data)
					if !unsafeSyms[sym] && usedSyms[sym] == 1 {
						kept[len(kept)-2] = InstrID(prevIns.Arg0)
						kept = kept[:len(kept)-1]
						usedSyms[sym] = 0
						changed = true
					}
				}
			}
		}
		if len(kept) == 1 && int(kept[0]) < len(prog.Instrs) {
			prog.Instrs[i] = prog.Instrs[kept[0]]
			changed = true
		} else if uint32(len(kept)) != ins.Arg1 {
			start, count := prog.AddExtra(kept)
			prog.Instrs[i].Arg0 = start
			prog.Instrs[i].Arg1 = count
			changed = true
		}
	}

	return changed
}

func isInlinablePurePrim(prog *Program, ins IRInstr, unsafeSyms map[token.StringID]bool) bool {
	if ins.Kind != IRPrim {
		return false
	}
	switch ins.SubOp {
	case PrimNeg, PrimNot, PrimAdd, PrimSub, PrimMul, PrimEq, PrimNeq, PrimLt, PrimLe, PrimGt, PrimGe, PrimAnd, PrimOr:
		return isPureExpr(prog, ins, unsafeSyms)
	}
	return false
}

func countSameScopeVarUses(prog *Program, id InstrID, target token.StringID) int {
	if id == NoInstr || int(id) >= len(prog.Instrs) {
		return 0
	}
	ins := prog.Instrs[id]
	switch ins.Kind {
	case IRConst, IRFn:
		return 0
	case IRVar:
		if token.StringID(ins.Data) == target {
			return 1
		}
		return 0
	case IRLet:
		c := countSameScopeVarUses(prog, InstrID(ins.Arg0), target)
		if InstrID(ins.Arg1) != NoInstr {
			c += countSameScopeVarUses(prog, InstrID(ins.Arg1), target)
		}
		return c
	case IRApp:
		return countSameScopeVarUses(prog, InstrID(ins.Arg0), target) +
			countSameScopeVarUses(prog, InstrID(ins.Arg1), target)
	case IRSwitch:
		c := countSameScopeVarUses(prog, InstrID(ins.Arg0), target)
		count := uint32(ins.Data)
		for i := uint32(0); i < count; i++ {
			idx := ins.Arg1 + i*2 + 1
			if int(idx) < len(prog.Extra) {
				c += countSameScopeVarUses(prog, InstrID(prog.Extra[idx]), target)
			}
		}
		if InstrID(ins.Aux) != NoInstr {
			c += countSameScopeVarUses(prog, InstrID(ins.Aux), target)
		}
		return c
	case IRPrim:
		if ins.SubOp == PrimWhile || ins.SubOp == PrimForIn || ins.SubOp == PrimForTo {
			return 0
		}
		if ins.SubOp == PrimSeqBlock {
			c := 0
			for i := uint32(0); i < ins.Arg1; i++ {
				if int(ins.Arg0+i) < len(prog.Extra) {
					c += countSameScopeVarUses(prog, InstrID(prog.Extra[ins.Arg0+i]), target)
				}
			}
			return c
		}
		c := countSameScopeVarUses(prog, InstrID(ins.Arg0), target)
		if InstrID(ins.Arg1) != NoInstr {
			c += countSameScopeVarUses(prog, InstrID(ins.Arg1), target)
		}
		return c
	case IRAlloc:
		if ins.SubOp == AllocADT {
			return countSameScopeVarUses(prog, InstrID(ins.Arg0), target)
		}
		if ins.SubOp == AllocRecord {
			c := 0
			if ins.Flags != 0 {
				c += countSameScopeVarUses(prog, InstrID(ins.Aux), target)
			}
			for i := uint32(0); i+1 < ins.Arg1; i += 2 {
				if int(ins.Arg0+i+1) < len(prog.Extra) {
					c += countSameScopeVarUses(prog, InstrID(prog.Extra[ins.Arg0+i+1]), target)
				}
			}
			return c
		}
		c := 0
		for i := uint32(0); i < ins.Arg1; i++ {
			if int(ins.Arg0+i) < len(prog.Extra) {
				c += countSameScopeVarUses(prog, InstrID(prog.Extra[ins.Arg0+i]), target)
			}
		}
		return c
	case IRLoad:
		return countSameScopeVarUses(prog, InstrID(ins.Arg0), target)
	}
	return 0
}

func replaceSameScopeVar(prog *Program, id InstrID, target token.StringID, replacement IRInstr) {
	if id == NoInstr || int(id) >= len(prog.Instrs) {
		return
	}
	ins := prog.Instrs[id]
	switch ins.Kind {
	case IRConst, IRFn:
		return
	case IRVar:
		if token.StringID(ins.Data) == target {
			prog.Instrs[id] = replacement
		}
	case IRLet:
		replaceSameScopeVar(prog, InstrID(ins.Arg0), target, replacement)
		if InstrID(ins.Arg1) != NoInstr {
			replaceSameScopeVar(prog, InstrID(ins.Arg1), target, replacement)
		}
	case IRApp:
		replaceSameScopeVar(prog, InstrID(ins.Arg0), target, replacement)
		replaceSameScopeVar(prog, InstrID(ins.Arg1), target, replacement)
	case IRSwitch:
		replaceSameScopeVar(prog, InstrID(ins.Arg0), target, replacement)
		count := uint32(ins.Data)
		for i := uint32(0); i < count; i++ {
			idx := ins.Arg1 + i*2 + 1
			if int(idx) < len(prog.Extra) {
				replaceSameScopeVar(prog, InstrID(prog.Extra[idx]), target, replacement)
			}
		}
		if InstrID(ins.Aux) != NoInstr {
			replaceSameScopeVar(prog, InstrID(ins.Aux), target, replacement)
		}
	case IRPrim:
		if ins.SubOp == PrimWhile || ins.SubOp == PrimForIn || ins.SubOp == PrimForTo {
			return
		}
		if ins.SubOp == PrimSeqBlock {
			for i := uint32(0); i < ins.Arg1; i++ {
				if int(ins.Arg0+i) < len(prog.Extra) {
					replaceSameScopeVar(prog, InstrID(prog.Extra[ins.Arg0+i]), target, replacement)
				}
			}
			return
		}
		replaceSameScopeVar(prog, InstrID(ins.Arg0), target, replacement)
		if InstrID(ins.Arg1) != NoInstr {
			replaceSameScopeVar(prog, InstrID(ins.Arg1), target, replacement)
		}
	case IRAlloc:
		if ins.SubOp == AllocADT {
			replaceSameScopeVar(prog, InstrID(ins.Arg0), target, replacement)
			return
		}
		if ins.SubOp == AllocRecord {
			if ins.Flags != 0 {
				replaceSameScopeVar(prog, InstrID(ins.Aux), target, replacement)
			}
			for i := uint32(0); i+1 < ins.Arg1; i += 2 {
				if int(ins.Arg0+i+1) < len(prog.Extra) {
					replaceSameScopeVar(prog, InstrID(prog.Extra[ins.Arg0+i+1]), target, replacement)
				}
			}
			return
		}
		for i := uint32(0); i < ins.Arg1; i++ {
			if int(ins.Arg0+i) < len(prog.Extra) {
				replaceSameScopeVar(prog, InstrID(prog.Extra[ins.Arg0+i]), target, replacement)
			}
		}
	case IRLoad:
		replaceSameScopeVar(prog, InstrID(ins.Arg0), target, replacement)
	}
}

func tryFoldLoad(prog *Program, ins IRInstr, allocEnv map[token.StringID]IRInstr, unsafeSyms map[token.StringID]bool) (IRInstr, bool) {
	if int(ins.Arg0) >= len(prog.Instrs) {
		return IRInstr{}, false
	}
	srcIns := prog.Instrs[ins.Arg0]
	if srcIns.Kind == IRVar {
		sym := token.StringID(srcIns.Data)
		if !unsafeSyms[sym] {
			if lookedUp, ok := allocEnv[sym]; ok {
				srcIns = lookedUp
			}
		}
	}
	if srcIns.Kind != IRAlloc || !isPureExpr(prog, srcIns, unsafeSyms) {
		return IRInstr{}, false
	}

	switch ins.SubOp {
	case LoadTupleElem:
		if srcIns.SubOp == AllocTuple && ins.Arg1 < srcIns.Arg1 && int(srcIns.Arg0+ins.Arg1) < len(prog.Extra) {
			elemID := prog.Extra[srcIns.Arg0+ins.Arg1]
			if int(elemID) < len(prog.Instrs) {
				return prog.Instrs[elemID], true
			}
		}
	case LoadRecordField:
		if srcIns.SubOp == AllocRecord {
			wantField := uint32(ins.Data)
			for j := uint32(0); j+1 < srcIns.Arg1; j += 2 {
				if int(srcIns.Arg0+j+1) < len(prog.Extra) && prog.Extra[srcIns.Arg0+j] == wantField {
					valID := prog.Extra[srcIns.Arg0+j+1]
					if int(valID) < len(prog.Instrs) {
						return prog.Instrs[valID], true
					}
				}
			}
		}
	case LoadADTTag:
		if srcIns.SubOp == AllocADT {
			return IRInstr{Kind: IRConst, SubOp: ConstInt, Data: srcIns.Data}, true
		}
	case LoadADTPayload:
		if srcIns.SubOp == AllocADT && InstrID(srcIns.Arg0) != NoInstr && int(srcIns.Arg0) < len(prog.Instrs) {
			return prog.Instrs[srcIns.Arg0], true
		}
	case LoadListIsEmpty:
		if srcIns.SubOp == AllocList {
			isEmpty := uint64(0)
			if srcIns.Arg1 == 0 {
				isEmpty = 1
			}
			return IRInstr{Kind: IRConst, SubOp: ConstInt, Data: isEmpty}, true
		}
	case LoadListHead:
		if srcIns.SubOp == AllocList && srcIns.Arg1 > 0 && int(srcIns.Arg0) < len(prog.Extra) {
			headID := prog.Extra[srcIns.Arg0]
			if int(headID) < len(prog.Instrs) {
				return prog.Instrs[headID], true
			}
		}
	}
	return IRInstr{}, false
}

func isPureExpr(prog *Program, ins IRInstr, unsafeSyms map[token.StringID]bool) bool {
	return isPureExprDepth(prog, ins, unsafeSyms, 0)
}

func isPureExprDepth(prog *Program, ins IRInstr, unsafeSyms map[token.StringID]bool, depth int) bool {
	if depth > 8 {
		return false
	}
	switch ins.Kind {
	case IRConst, IRFn:
		return true
	case IRVar:
		return !unsafeSyms[token.StringID(ins.Data)]
	case IRLet:
		if ins.Flags != 0 || int(ins.Arg0) >= len(prog.Instrs) || !isPureExprDepth(prog, prog.Instrs[ins.Arg0], unsafeSyms, depth+1) {
			return false
		}
		if InstrID(ins.Arg1) != NoInstr {
			return int(ins.Arg1) < len(prog.Instrs) && isPureExprDepth(prog, prog.Instrs[ins.Arg1], unsafeSyms, depth+1)
		}
		return true
	case IRSwitch:
		if int(ins.Arg0) >= len(prog.Instrs) || !isPureExprDepth(prog, prog.Instrs[ins.Arg0], unsafeSyms, depth+1) {
			return false
		}
		count := uint32(ins.Data)
		for i := uint32(0); i < count; i++ {
			idx := ins.Arg1 + i*2 + 1
			if int(idx) >= len(prog.Extra) || int(prog.Extra[idx]) >= len(prog.Instrs) ||
				!isPureExprDepth(prog, prog.Instrs[prog.Extra[idx]], unsafeSyms, depth+1) {
				return false
			}
		}
		if InstrID(ins.Aux) != NoInstr {
			return int(ins.Aux) < len(prog.Instrs) && isPureExprDepth(prog, prog.Instrs[ins.Aux], unsafeSyms, depth+1)
		}
		return true
	case IRPrim:
		switch ins.SubOp {
		case PrimSeqBlock:
			for i := uint32(0); i < ins.Arg1; i++ {
				idx := ins.Arg0 + i
				if int(idx) >= len(prog.Extra) || int(prog.Extra[idx]) >= len(prog.Instrs) ||
					!isPureExprDepth(prog, prog.Instrs[prog.Extra[idx]], unsafeSyms, depth+1) {
					return false
				}
			}
			return true
		case PrimNeg, PrimNot:
			return int(ins.Arg0) < len(prog.Instrs) && isPureExprDepth(prog, prog.Instrs[ins.Arg0], unsafeSyms, depth+1)
		case PrimAdd, PrimSub, PrimMul, PrimEq, PrimNeq, PrimLt, PrimLe, PrimGt, PrimGe, PrimAnd, PrimOr:
			return int(ins.Arg0) < len(prog.Instrs) && int(ins.Arg1) < len(prog.Instrs) &&
				isPureExprDepth(prog, prog.Instrs[ins.Arg0], unsafeSyms, depth+1) &&
				isPureExprDepth(prog, prog.Instrs[ins.Arg1], unsafeSyms, depth+1)
		}
	case IRAlloc:
		switch ins.SubOp {
		case AllocADT:
			if InstrID(ins.Arg0) == NoInstr {
				return true
			}
			return int(ins.Arg0) < len(prog.Instrs) && isPureExprDepth(prog, prog.Instrs[ins.Arg0], unsafeSyms, depth+1)
		case AllocTuple, AllocList:
			for i := uint32(0); i < ins.Arg1; i++ {
				idx := ins.Arg0 + i
				if int(idx) >= len(prog.Extra) || int(prog.Extra[idx]) >= len(prog.Instrs) ||
					!isPureExprDepth(prog, prog.Instrs[prog.Extra[idx]], unsafeSyms, depth+1) {
					return false
				}
			}
			return true
		case AllocRecord:
			if ins.Flags != 0 {
				return false
			}
			for i := uint32(0); i+1 < ins.Arg1; i += 2 {
				valIdx := ins.Arg0 + i + 1
				if int(valIdx) >= len(prog.Extra) || int(prog.Extra[valIdx]) >= len(prog.Instrs) ||
					!isPureExprDepth(prog, prog.Instrs[prog.Extra[valIdx]], unsafeSyms, depth+1) {
					return false
				}
			}
			return true
		}
	case IRLoad:
		switch ins.SubOp {
		case LoadTupleElem, LoadRecordField, LoadADTPayload, LoadADTTag, LoadListHead, LoadListTail, LoadListIsEmpty:
			return int(ins.Arg0) < len(prog.Instrs) && isPureExprDepth(prog, prog.Instrs[ins.Arg0], unsafeSyms, depth+1)
		}
	}
	return false
}

func computeLiveSymbolUses(prog *Program) map[token.StringID]int {
	uses := make(map[token.StringID]int)
	visited := make(map[InstrID]bool)
	markReachableUses(prog, prog.Root, uses, visited)
	if prog.Strings != nil {
		for _, ins := range prog.Instrs {
			if ins.Kind == IRLet && ins.Data != 0 {
				sym := token.StringID(ins.Data)
				name := prog.Strings.String(sym)
				if strings.HasPrefix(name, "bench_") || strings.HasPrefix(name, "test_") {
					uses[sym]++
					markReachableUses(prog, InstrID(ins.Arg0), uses, visited)
				}
			}
		}
	}
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

func isKnownBoolExpr(ins IRInstr) bool {
	if ins.Kind == IRConst && ins.SubOp == ConstBool {
		return true
	}
	if ins.Kind == IRPrim {
		switch ins.SubOp {
		case PrimNot, PrimAnd, PrimOr, PrimEq, PrimNeq, PrimLt, PrimLe, PrimGt, PrimGe:
			return true
		}
	}
	return false
}

func samePureExpr(prog *Program, lhs, rhs IRInstr, unsafeSyms map[token.StringID]bool) bool {
	if samePureOperand(lhs, rhs, unsafeSyms) {
		return true
	}
	if lhs.Kind == IRPrim && rhs.Kind == IRPrim && lhs.SubOp == rhs.SubOp && lhs.Flags == rhs.Flags {
		switch lhs.SubOp {
		case PrimNeg, PrimNot:
			return int(lhs.Arg0) < len(prog.Instrs) && int(rhs.Arg0) < len(prog.Instrs) &&
				samePureOperand(prog.Instrs[lhs.Arg0], prog.Instrs[rhs.Arg0], unsafeSyms)
		case PrimAdd, PrimSub, PrimMul, PrimEq, PrimNeq, PrimLt, PrimLe, PrimGt, PrimGe, PrimAnd, PrimOr:
			return int(lhs.Arg0) < len(prog.Instrs) && int(rhs.Arg0) < len(prog.Instrs) &&
				int(lhs.Arg1) < len(prog.Instrs) && int(rhs.Arg1) < len(prog.Instrs) &&
				samePureOperand(prog.Instrs[lhs.Arg0], prog.Instrs[rhs.Arg0], unsafeSyms) &&
				samePureOperand(prog.Instrs[lhs.Arg1], prog.Instrs[rhs.Arg1], unsafeSyms)
		}
	}
	return false
}

func isNegationOf(prog *Program, cand, other IRInstr, unsafeSyms map[token.StringID]bool) bool {
	if cand.Kind != IRPrim || cand.SubOp != PrimNot || int(cand.Arg0) >= len(prog.Instrs) {
		return false
	}
	return samePureExpr(prog, prog.Instrs[cand.Arg0], other, unsafeSyms)
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
		// Negated equality inversion: not (a == b) -> a != b, not (a != b) -> a == b
		if ins.SubOp == PrimNot && lhs.Kind == IRPrim {
			switch lhs.SubOp {
			case PrimEq:
				return IRInstr{Kind: IRPrim, SubOp: PrimNeq, Flags: lhs.Flags, Arg0: lhs.Arg0, Arg1: lhs.Arg1}, true
			case PrimNeq:
				return IRInstr{Kind: IRPrim, SubOp: PrimEq, Flags: lhs.Flags, Arg0: lhs.Arg0, Arg1: lhs.Arg1}, true
			}
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
		// (x - y) + y -> x
		if lhs.Kind == IRPrim && lhs.SubOp == PrimSub && lhs.Flags == 0 &&
			int(lhs.Arg0) < len(prog.Instrs) && int(lhs.Arg1) < len(prog.Instrs) &&
			samePureOperand(prog.Instrs[lhs.Arg1], rhs, unsafeSyms) &&
			isPureExpr(prog, prog.Instrs[lhs.Arg0], unsafeSyms) {
			return prog.Instrs[lhs.Arg0], true
		}
		// (x + c1) + c2 -> x + (c1 + c2) when c1, c2 are int constants
		if lhs.Kind == IRPrim && (lhs.SubOp == PrimAdd || lhs.SubOp == PrimSub) && lhs.Flags == 0 &&
			int(lhs.Arg0) < len(prog.Instrs) && int(lhs.Arg1) < len(prog.Instrs) &&
			rhs.Kind == IRConst && rhs.SubOp == ConstInt {
			l1 := prog.Instrs[lhs.Arg1]
			if l1.Kind == IRConst && l1.SubOp == ConstInt {
				c1 := int64(l1.Data)
				if lhs.SubOp == PrimSub {
					c1 = -c1
				}
				cSum := c1 + int64(rhs.Data)
				if cSum == 0 {
					return prog.Instrs[lhs.Arg0], true
				}
				cID := prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstInt, Data: uint64(cSum)})
				return IRInstr{Kind: IRPrim, SubOp: PrimAdd, Arg0: lhs.Arg0, Arg1: uint32(cID)}, true
			}
		}

	case PrimSub:
		if isIntConstVal(rhs, 0) || isFloatConstVal(rhs, 0) {
			return lhs, true
		}
		if ins.Flags == 0 && samePureOperand(lhs, rhs, unsafeSyms) {
			return IRInstr{Kind: IRConst, SubOp: ConstInt, Data: 0}, true
		}
		// (x + y) - y -> x, (y + x) - y -> x
		if lhs.Kind == IRPrim && lhs.SubOp == PrimAdd && lhs.Flags == 0 &&
			int(lhs.Arg0) < len(prog.Instrs) && int(lhs.Arg1) < len(prog.Instrs) {
			l0 := prog.Instrs[lhs.Arg0]
			l1 := prog.Instrs[lhs.Arg1]
			if samePureOperand(l1, rhs, unsafeSyms) && isPureExpr(prog, l0, unsafeSyms) {
				return l0, true
			}
			if samePureOperand(l0, rhs, unsafeSyms) && isPureExpr(prog, l1, unsafeSyms) {
				return l1, true
			}
		}
		// (x + c1) - c2 -> x + (c1 - c2) when c1, c2 are int constants
		if lhs.Kind == IRPrim && (lhs.SubOp == PrimAdd || lhs.SubOp == PrimSub) && lhs.Flags == 0 &&
			int(lhs.Arg0) < len(prog.Instrs) && int(lhs.Arg1) < len(prog.Instrs) &&
			rhs.Kind == IRConst && rhs.SubOp == ConstInt {
			l1 := prog.Instrs[lhs.Arg1]
			if l1.Kind == IRConst && l1.SubOp == ConstInt {
				c1 := int64(l1.Data)
				if lhs.SubOp == PrimSub {
					c1 = -c1
				}
				cDiff := c1 - int64(rhs.Data)
				if cDiff == 0 {
					return prog.Instrs[lhs.Arg0], true
				}
				cID := prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstInt, Data: uint64(cDiff)})
				return IRInstr{Kind: IRPrim, SubOp: PrimAdd, Arg0: lhs.Arg0, Arg1: uint32(cID)}, true
			}
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
		if isNegationOf(prog, lhs, rhs, unsafeSyms) || isNegationOf(prog, rhs, lhs, unsafeSyms) {
			return boolConstInstr(false), true
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
		if isNegationOf(prog, lhs, rhs, unsafeSyms) || isNegationOf(prog, rhs, lhs, unsafeSyms) {
			return boolConstInstr(true), true
		}
	case PrimEq:
		if samePureOperand(lhs, rhs, unsafeSyms) {
			return boolConstInstr(true), true
		}
		if isKnownBoolExpr(lhs) && rhs.Kind == IRConst && rhs.SubOp == ConstBool {
			if rhs.Data != 0 {
				return lhs, true
			}
			return IRInstr{Kind: IRPrim, SubOp: PrimNot, Arg0: ins.Arg0, Arg1: uint32(NoInstr)}, true
		}
		if isKnownBoolExpr(rhs) && lhs.Kind == IRConst && lhs.SubOp == ConstBool {
			if lhs.Data != 0 {
				return rhs, true
			}
			return IRInstr{Kind: IRPrim, SubOp: PrimNot, Arg0: ins.Arg1, Arg1: uint32(NoInstr)}, true
		}
	case PrimLe, PrimGe:
		if samePureOperand(lhs, rhs, unsafeSyms) {
			return boolConstInstr(true), true
		}
	case PrimNeq:
		if samePureOperand(lhs, rhs, unsafeSyms) {
			return boolConstInstr(false), true
		}
		if isKnownBoolExpr(lhs) && rhs.Kind == IRConst && rhs.SubOp == ConstBool {
			if rhs.Data == 0 {
				return lhs, true
			}
			return IRInstr{Kind: IRPrim, SubOp: PrimNot, Arg0: ins.Arg0, Arg1: uint32(NoInstr)}, true
		}
		if isKnownBoolExpr(rhs) && lhs.Kind == IRConst && lhs.SubOp == ConstBool {
			if lhs.Data == 0 {
				return rhs, true
			}
			return IRInstr{Kind: IRPrim, SubOp: PrimNot, Arg0: ins.Arg1, Arg1: uint32(NoInstr)}, true
		}
	case PrimLt, PrimGt:
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

func tryFoldSwitchInstr(prog *Program, ins IRInstr, allocEnv map[token.StringID]IRInstr, unsafeSyms map[token.StringID]bool) (IRInstr, bool) {
	if target, ok := tryFoldSwitch(prog, ins, allocEnv, unsafeSyms); ok && int(target) < len(prog.Instrs) {
		return prog.Instrs[target], true
	}
	if int(ins.Arg0) >= len(prog.Instrs) || uint32(ins.Data) != 1 || int(ins.Arg1+1) >= len(prog.Extra) || int(ins.Aux) >= len(prog.Instrs) {
		return IRInstr{}, false
	}
	caseTag := prog.Extra[ins.Arg1]
	targetID := InstrID(prog.Extra[ins.Arg1+1])
	auxID := InstrID(ins.Aux)
	if (caseTag != 0 && caseTag != 1) || int(targetID) >= len(prog.Instrs) {
		return IRInstr{}, false
	}
	condIns := prog.Instrs[ins.Arg0]
	thenID, elseID := targetID, auxID
	if caseTag == 0 {
		thenID, elseID = auxID, targetID
	}
	thenIns := prog.Instrs[thenID]
	elseIns := prog.Instrs[elseID]

	// if cond then x else x -> x (when cond is pure)
	if samePureOperand(thenIns, elseIns, unsafeSyms) && isPureExpr(prog, condIns, unsafeSyms) {
		return thenIns, true
	}

	// if cond then true else false -> cond; if cond then false else true -> not cond
	if isKnownBoolExpr(condIns) {
		if isBoolConstVal(thenIns, true) && isBoolConstVal(elseIns, false) {
			return condIns, true
		}
		if isBoolConstVal(thenIns, false) && isBoolConstVal(elseIns, true) {
			return IRInstr{Kind: IRPrim, SubOp: PrimNot, Arg0: ins.Arg0, Arg1: uint32(NoInstr)}, true
		}
	}

	// if not c then t else f -> if c then f else t
	if condIns.Kind == IRPrim && condIns.SubOp == PrimNot && int(condIns.Arg0) < len(prog.Instrs) {
		swappedStart, swappedCount := prog.AddSwitchCases([]SwitchCase{{Tag: caseTag, Target: auxID}})
		return IRInstr{
			Kind: IRSwitch,
			Arg0: condIns.Arg0,
			Arg1: swappedStart,
			Aux:  uint32(targetID),
			Data: uint64(swappedCount),
		}, true
	}

	return IRInstr{}, false
}

func tryFoldSwitch(prog *Program, ins IRInstr, allocEnv map[token.StringID]IRInstr, unsafeSyms map[token.StringID]bool) (InstrID, bool) {
	if int(ins.Arg0) >= len(prog.Instrs) {
		return NoInstr, false
	}
	scrut := prog.Instrs[ins.Arg0]
	var tag uint32
	hasTag := false
	if scrut.Kind == IRConst && (scrut.SubOp == ConstBool || scrut.SubOp == ConstInt) {
		tag = uint32(scrut.Data)
		hasTag = true
	} else if scrut.Kind == IRLoad && scrut.SubOp == LoadADTTag {
		if adtIns, ok := resolveKnownAlloc(prog, InstrID(scrut.Arg0), allocEnv, unsafeSyms); ok && adtIns.SubOp == AllocADT {
			tag = uint32(adtIns.Data)
			hasTag = true
		}
	}
	if !hasTag {
		return NoInstr, false
	}

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
