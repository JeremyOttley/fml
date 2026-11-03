package ir

import (
	"testing"
	"unsafe"
)

func TestIRSpec(t *testing.T) {
	if irKindCount > 10 {
		t.Fatalf("IR instruction count %d exceeds architectural ceiling of 10", irKindCount)
	}
	if sz := unsafe.Sizeof(IRInstr{}); sz > 32 {
		t.Fatalf("IRInstr size %d exceeds 32-byte cache-line packing requirement", sz)
	}

	prog := NewProgram(64, nil)
	c1 := prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstInt, Data: 10})
	c2 := prog.Emit(IRInstr{Kind: IRConst, SubOp: ConstInt, Data: 20})
	sum := prog.Emit(IRInstr{Kind: IRPrim, SubOp: PrimAdd, Arg0: uint32(c1), Arg1: uint32(c2)})
	prog.Root = sum

	if len(prog.Instrs) != 3 || prog.Instrs[sum].Kind != IRPrim {
		t.Fatalf("unexpected emitted instructions: %+v", prog.Instrs)
	}
}
