package ir

import (
	"testing"

	"fml/internal/parser"
	"fml/internal/types"
)

func TestPeepholeConstantFoldingAndDeadBranchElimination(t *testing.T) {
	src := []byte(`
if (10 + 32) == 42 && not false then
    (20 * 2) + (10 / 5)
else
    999
`)
	p, root, err := parser.ParseSource(src, nil)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	chk := types.NewChecker(p.Arena, nil)
	if _, err := chk.Infer(root); err != nil {
		t.Fatalf("infer failed: %v", err)
	}
	prog := Lower(p.Arena, chk.Types, root)
	Optimize(prog)

	rootInstr := prog.Instrs[prog.Root]
	if rootInstr.Kind != IRConst || rootInstr.SubOp != ConstInt || int64(rootInstr.Data) != 42 {
		t.Fatalf("expected entire conditional & arithmetic tree to constant-fold into IRConst(42), got %+v", rootInstr)
	}
}

func TestCopyPropagationInliningAndDCE(t *testing.T) {
	src := []byte(`
let a = 10
let b = a
let unusedTop = 12345
let double x = x * 2
let res =
    let unusedScoped = 999 in
    double (b + 11)
res
`)
	p, root, err := parser.ParseSource(src, nil)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	chk := types.NewChecker(p.Arena, nil)
	if _, err := chk.Infer(root); err != nil {
		t.Fatalf("infer failed: %v", err)
	}
	prog := Lower(p.Arena, chk.Types, root)
	Optimize(prog)

	rootInstr := prog.Instrs[prog.Root]
	if rootInstr.Kind != IRConst || rootInstr.SubOp != ConstInt || int64(rootInstr.Data) != 42 {
		t.Fatalf("expected copy propagation, lambda inlining, and DCE to collapse program into IRConst(42), got %+v", rootInstr)
	}
}

func TestAlgebraicIdentitiesAndCSE(t *testing.T) {
	// Even with unknown function parameters x and y, CSE canonicalizes (x + y) and (y + x)
	// into the same value number, algebraic simplification folds (s1 - s2) -> 0,
	// (x * 0) -> 0, (y * 1 - y) -> 0, and (s1 == s2) -> true, collapsing the function body to 42.
	src := []byte(`
fun (x: int) (y: int) ->
    let s1 = x + y in
    let s2 = y + x in
    let zeroDiff = s1 - s2 in
    let zeroMul = (x * 0) + ((y * 1) - y) in
    if (s1 == s2) && (not (not true)) then
        42 + zeroDiff + zeroMul
    else
        0
`)
	p, root, err := parser.ParseSource(src, nil)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	chk := types.NewChecker(p.Arena, nil)
	if _, err := chk.Infer(root); err != nil {
		t.Fatalf("infer failed: %v", err)
	}
	prog := Lower(p.Arena, chk.Types, root)
	Optimize(prog)

	// Root is IRFn(x) -> IRFn(y) -> IRConst(42)
	outerFn := prog.Instrs[prog.Root]
	if outerFn.Kind != IRFn {
		t.Fatalf("expected outer IRFn, got %+v", outerFn)
	}
	innerFn := prog.Instrs[outerFn.Arg0]
	if innerFn.Kind != IRFn {
		t.Fatalf("expected inner IRFn, got %+v", innerFn)
	}
	bodyInstr := prog.Instrs[innerFn.Arg0]
	if bodyInstr.Kind != IRConst || bodyInstr.SubOp != ConstInt || int64(bodyInstr.Data) != 42 {
		t.Fatalf("expected CSE and algebraic identities to collapse function body into IRConst(42), got %+v", bodyInstr)
	}
}


