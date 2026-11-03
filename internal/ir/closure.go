package ir

import "fml/internal/token"

// ConvertClosures performs free-variable analysis across all IRFn instructions
// in prog and records the flat list of captured StringIDs in Program.Extra
// (IRFn.Arg1 = start, IRFn.Aux = count).
//
// Jaime says: A closure is just a function code index paired with a flat array
// of captured values. Don't capture the entire lexical universe—compute the
// exact free variables once at compile time and pack them tightly.
func ConvertClosures(prog *Program) {
	for i := 0; i < len(prog.Instrs); i++ {
		if prog.Instrs[i].Kind != IRFn {
			continue
		}
		paramSym := token.StringID(prog.Instrs[i].Data)
		bodyID := InstrID(prog.Instrs[i].Arg0)

		bound := make(map[token.StringID]bool)
		if paramSym != token.NoString {
			bound[paramSym] = true
		}
		freeMap := make(map[token.StringID]bool)
		var freeList []InstrID

		collectFreeIR(prog, bodyID, bound, freeMap, &freeList)
		prog.Instrs[i].Flags |= IRFlagClosed
		if len(freeList) > 0 {
			start, count := prog.AddExtra(freeList)
			prog.Instrs[i].Arg1 = start
			prog.Instrs[i].Aux = count
		} else {
			prog.Instrs[i].Arg1 = 0
			prog.Instrs[i].Aux = 0
		}
	}
}

func collectFreeIR(prog *Program, id InstrID, bound map[token.StringID]bool, freeMap map[token.StringID]bool, freeList *[]InstrID) {
	if id == NoInstr || int(id) >= len(prog.Instrs) {
		return
	}
	ins := prog.Instrs[id]

	switch ins.Kind {
	case IRConst:
		return
	case IRVar:
		sym := token.StringID(ins.Data)
		if sym != token.NoString && !bound[sym] && !freeMap[sym] {
			freeMap[sym] = true
			*freeList = append(*freeList, InstrID(sym))
		}
	case IRLet:
		sym := token.StringID(ins.Data)
		if ins.Flags&IRFlagRec != 0 && sym != token.NoString {
			bound[sym] = true
		}
		collectFreeIR(prog, InstrID(ins.Arg0), bound, freeMap, freeList)
		if sym != token.NoString {
			bound[sym] = true
		}
		if InstrID(ins.Arg1) != NoInstr {
			subBound := cloneBoundMap(bound)
			collectFreeIR(prog, InstrID(ins.Arg1), subBound, freeMap, freeList)
		}
	case IRFn:
		subBound := cloneBoundMap(bound)
		if p := token.StringID(ins.Data); p != token.NoString {
			subBound[p] = true
		}
		collectFreeIR(prog, InstrID(ins.Arg0), subBound, freeMap, freeList)
	case IRApp:
		collectFreeIR(prog, InstrID(ins.Arg0), bound, freeMap, freeList)
		collectFreeIR(prog, InstrID(ins.Arg1), bound, freeMap, freeList)
	case IRSwitch:
		collectFreeIR(prog, InstrID(ins.Arg0), bound, freeMap, freeList)
		count := uint32(ins.Data)
		for i := uint32(0); i < count; i++ {
			target := InstrID(prog.Extra[ins.Arg1+i*2+1])
			collectFreeIR(prog, target, cloneBoundMap(bound), freeMap, freeList)
		}
		collectFreeIR(prog, InstrID(ins.Aux), cloneBoundMap(bound), freeMap, freeList)
	case IRPrim:
		if ins.SubOp == PrimSeqBlock {
			for i := uint32(0); i < ins.Arg1; i++ {
				ch := InstrID(prog.Extra[ins.Arg0+i])
				collectFreeIR(prog, ch, bound, freeMap, freeList)
			}
			return
		}
		if ins.SubOp == PrimForIn || ins.SubOp == PrimForTo {
			collectFreeIR(prog, InstrID(ins.Arg0), bound, freeMap, freeList)
			if sym := token.StringID(ins.Data); sym != token.NoString {
				bound[sym] = true
			}
			collectFreeIR(prog, InstrID(ins.Arg1), bound, freeMap, freeList)
			return
		}
		collectFreeIR(prog, InstrID(ins.Arg0), bound, freeMap, freeList)
		collectFreeIR(prog, InstrID(ins.Arg1), bound, freeMap, freeList)
	case IRAlloc:
		if ins.SubOp == AllocADT {
			collectFreeIR(prog, InstrID(ins.Arg0), bound, freeMap, freeList)
			return
		}
		if ins.SubOp == AllocRecord {
			if ins.Flags != 0 {
				collectFreeIR(prog, InstrID(ins.Aux), bound, freeMap, freeList)
			}
			for i := uint32(0); i < ins.Arg1; i += 2 {
				valID := InstrID(prog.Extra[ins.Arg0+i+1])
				collectFreeIR(prog, valID, bound, freeMap, freeList)
			}
			return
		}
		for i := uint32(0); i < ins.Arg1; i++ {
			ch := InstrID(prog.Extra[ins.Arg0+i])
			collectFreeIR(prog, ch, bound, freeMap, freeList)
		}
	case IRLoad:
		collectFreeIR(prog, InstrID(ins.Arg0), bound, freeMap, freeList)
	case IRStore:
		sym := token.StringID(ins.Data)
		if sym != token.NoString && !bound[sym] && !freeMap[sym] {
			freeMap[sym] = true
			*freeList = append(*freeList, InstrID(sym))
		}
		collectFreeIR(prog, InstrID(ins.Arg0), bound, freeMap, freeList)
		collectFreeIR(prog, InstrID(ins.Arg1), bound, freeMap, freeList)
	}
}

func cloneBoundMap(src map[token.StringID]bool) map[token.StringID]bool {
	dst := make(map[token.StringID]bool, len(src)+2)
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
