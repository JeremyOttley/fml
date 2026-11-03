// Package ir defines the flat OCaml Lambda-style intermediate representation,
// AST-to-IR lowering pass, pattern-matching decision tree compiler, and closure
// converter for FML.
//
// Jaime says: The AST is what the user wrote; the IR is what the machine
// understands. Strip away the syntax sugar. Turn pattern matching into decision
// trees. If your IR has more than 10 instruction types, you've failed to distill
// the semantics.
package ir

import "fml/internal/token"

// InstrID is a 0-based index into Program.Instrs (NoInstr = ^uint32(0)).
type InstrID uint32

// NoInstr is the sentinel index representing an absent instruction operand.
const NoInstr InstrID = ^InstrID(0)

// IRKind enumerates the 10 core instruction opcodes of the FML IR.
type IRKind uint8

const (
	IRConst  IRKind = iota // 0: Constant literal (Int, Float, Bool, String, Unit)
	IRVar                  // 1: Variable reference by StringID
	IRLet                  // 2: Value binding (let x = Arg0 [in Arg1])
	IRFn                   // 3: Function abstraction (param = Data, body = Arg0)
	IRApp                  // 4: Function application (fn = Arg0, arg = Arg1)
	IRSwitch               // 5: Decision-tree tag/value switch & conditional branch
	IRPrim                 // 6: Primitive arithmetic/logic/sequence/control operation
	IRAlloc                // 7: Structured allocation (ADT, Tuple, Record, List, Array, Ref)
	IRLoad                 // 8: Structured field/index/payload/ref load
	IRStore                // 9: Mutable variable/ref/array store

	irKindCount // Must remain <= 10
)

// Compile-time assertion that the IR has at most 10 instruction types.
const _ = uint(10 - int(irKindCount))

// Instruction flags stored in IRInstr.Flags.
const (
	IRFlagTailCall uint8 = 1 << iota
	IRFlagRec
	IRFlagMutable
	IRFlagDispose // Automatic `use` disposal scope
	IRFlagClosed  // Free-variable analysis completed on IRFn
)

// Sub-opcodes for IRConst.
const (
	ConstUnit uint16 = iota
	ConstBool
	ConstInt
	ConstFloat
	ConstString
)

// Sub-opcodes for IRPrim.
const (
	PrimSeqBlock uint16 = iota // Evaluates Extra[Arg0 : Arg0+Arg1] sequentially, returning last
	PrimAdd
	PrimSub
	PrimMul
	PrimDiv
	PrimMod
	PrimNeg
	PrimEq
	PrimNeq
	PrimLt
	PrimLe
	PrimGt
	PrimGe
	PrimAnd
	PrimOr
	PrimNot
	PrimCons
	PrimAppend
	PrimRange
	PrimWhile      // Loop while Arg0 is true evaluating Arg1
	PrimForIn      // Iterate var(Data) over Arg0 evaluating Arg1
	PrimForTo      // Iterate var(Data) over range Arg0 evaluating Arg1
	PrimTryWith    // Try Arg0, on error switch Arg1
	PrimTryFinally // Try Arg0, always run cleanup Arg1
	PrimRaise      // Raise error Arg0
	PrimSeqGen     // Wrap body Arg0 as lazy sequence generator
)

// Sub-opcodes for IRAlloc.
const (
	AllocTuple uint16 = iota
	AllocList
	AllocArray
	AllocRecord
	AllocADT
	AllocRef
	AllocClosure
)

// Sub-opcodes for IRLoad.
const (
	LoadTupleElem uint16 = iota
	LoadRecordField
	LoadADTPayload
	LoadADTTag
	LoadListHead
	LoadListTail
	LoadListIsEmpty
	LoadRef
)

// Sub-opcodes for IRStore.
const (
	StoreVar uint16 = iota
	StoreRef
	StoreArrayElem
)

// SwitchCase represents a single branch in an IRSwitch decision tree node,
// packed as two uint32 entries in Program.Extra (TagOrConst, TargetInstrID).
type SwitchCase struct {
	Tag    uint32  // Constructor StringID, integer constant, or 1/0 for bool
	Target InstrID // Target instruction evaluated when Tag matches
}

// IRInstr is a 24-byte flat instruction node stored contiguously in Program.Instrs.
//
// Jaime says: 24 bytes per IR instruction. No interfaces, no pointer trees,
// no heap-allocated operand slices. Everything lives in flat arrays.
type IRInstr struct {
	Kind  IRKind
	Flags uint8
	SubOp uint16
	Arg0  uint32 // Primary operand InstrID or start offset in Program.Extra
	Arg1  uint32 // Secondary operand InstrID or count in Program.Extra
	Aux   uint32 // Default branch InstrID for IRSwitch or captured env count
	Data  uint64 // Literal payload or symbol StringID
}

// Program holds the lowered flat instruction array, auxiliary operand indices,
// and shared StringTable.
type Program struct {
	Instrs  []IRInstr
	Extra   []uint32
	Root    InstrID
	Strings *token.StringTable
}

// NewProgram allocates a flat IR Program container.
func NewProgram(instrCap int, strings *token.StringTable) *Program {
	if instrCap < 128 {
		instrCap = 128
	}
	if strings == nil {
		strings = token.NewStringTable(4096, 512)
	}
	return &Program{
		Instrs:  make([]IRInstr, 0, instrCap),
		Extra:   make([]uint32, 0, instrCap),
		Root:    NoInstr,
		Strings: strings,
	}
}

// Emit appends an IRInstr to Program.Instrs and returns its InstrID.
func (p *Program) Emit(ins IRInstr) InstrID {
	id := InstrID(len(p.Instrs))
	p.Instrs = append(p.Instrs, ins)
	return id
}

// AddExtra appends a slice of InstrIDs to Program.Extra and returns (start, count).
func (p *Program) AddExtra(ids []InstrID) (start uint32, count uint32) {
	start = uint32(len(p.Extra))
	count = uint32(len(ids))
	for _, id := range ids {
		p.Extra = append(p.Extra, uint32(id))
	}
	return start, count
}

// AddSwitchCases packs a slice of SwitchCase pairs into Program.Extra.
func (p *Program) AddSwitchCases(cases []SwitchCase) (start uint32, count uint32) {
	start = uint32(len(p.Extra))
	count = uint32(len(cases))
	for _, c := range cases {
		p.Extra = append(p.Extra, c.Tag, uint32(c.Target))
	}
	return start, count
}
