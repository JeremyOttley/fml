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

func TestContainerProjectionFoldingAndADTMatchElimination(t *testing.T) {
	src := []byte(`
type Endpoint = { Host: string; Port: int }
let ep = { Host = "127.0.0.1"; Port = 8000 }
let pair = (20, 22)
let wrapped = Some 40

let extracted =
    match wrapped with
    | Some v -> v
    | None -> 0

let tupVal =
    match pair with
    | (a, b) -> a + b

ep.Port + extracted + tupVal + (fst pair) + (snd pair) + (id 16)
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

	// 8000 + 40 + 42 + 20 + 22 + 16 = 8140
	rootInstr := prog.Instrs[prog.Root]
	if rootInstr.Kind != IRConst || rootInstr.SubOp != ConstInt || int64(rootInstr.Data) != 8140 {
		t.Fatalf("expected record, tuple, fst/snd/id, and ADT match projections to fold into IRConst(8140), got %+v", rootInstr)
	}
}

func TestMultiArgInliningAndPipelineFusion(t *testing.T) {
	src := []byte(`
let add a b = a + b
let mul a b = a * b
let mad a b c = a * b + c
let step = add 5 >> mul 2

let piped = 10 |> add 5 |> mul 2
let composed = 10 |> step
let fused3 = 4 |> mad 2 3

piped + composed + fused3
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

	// piped = (5 + 10) * 2 = 30
	// composed = (5 + 10) * 2 = 30
	// fused3 = 2 * 3 + 4 = 10
	// total = 70
	rootInstr := prog.Instrs[prog.Root]
	if rootInstr.Kind != IRConst || rootInstr.SubOp != ConstInt || int64(rootInstr.Data) != 70 {
		t.Fatalf("expected multi-arg curried calls, pipelines, and compositions to fold into IRConst(70), got %+v", rootInstr)
	}

	// Verify non-constant pipeline stages inside a lambda fuse into direct IRPrim ops
	srcFn := []byte(`
let add a b = a + b
let mul a b = a * b
fun x -> x |> add 5 |> mul 2
`)
	p2, root2, err := parser.ParseSource(srcFn, nil)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	chk2 := types.NewChecker(p2.Arena, nil)
	if _, err := chk2.Infer(root2); err != nil {
		t.Fatalf("infer failed: %v", err)
	}
	prog2 := Lower(p2.Arena, chk2.Types, root2)
	Optimize(prog2)

	fnInstr := prog2.Instrs[prog2.Root]
	if fnInstr.Kind != IRFn {
		t.Fatalf("expected root IRFn after DCE of inlined helpers, got %+v", fnInstr)
	}
	bodyInstr := prog2.Instrs[fnInstr.Arg0]
	if bodyInstr.Kind != IRPrim || bodyInstr.SubOp != PrimMul {
		t.Fatalf("expected pipeline body to fuse into IRPrim(PrimMul), got %+v", bodyInstr)
	}
}

func TestDeadBindingEliminationBooleanBranchFoldingAndMatchFusion(t *testing.T) {
	// 1. Verify Option/Result/List combinator fusion, boolean short-circuiting,
	// and dead-binding elimination of complex pure expressions (if/else, match, blocks).
	src := []byte(`
let inc x = x + 1
let wrapDouble x = Some (x * 2)

let deadIf =
    if true then
        let unused = 100 + 200 in
        unused * 3
    else
        0

let deadMatch =
    match Some 999 with
    | Some v -> v + 1
    | None -> 0

let optVal =
    Option.defaultValue 0 (Option.bind wrapDouble (Option.map inc (Some 10)))

let fallbackVal =
    Option.defaultValue 99 (Option.orElse (Some 8) None)

let resVal =
    Result.defaultValue 0 (Result.map inc (Ok 11))

let flagsOk =
    Option.isSome (Some 1) && Option.isNone None &&
    Result.isOk (Ok 1) && Result.isError (Error "bad") &&
    List.isEmpty [] && not (List.isEmpty [1; 2])

let listMetric = (List.length [10; 20; 30]) + (List.head [5; 6; 7])

if flagsOk then
    optVal + fallbackVal + resVal + listMetric
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

	// optVal = (10 + 1) * 2 = 22
	// fallbackVal = 8
	// resVal = 11 + 1 = 12
	// listMetric = 3 + 5 = 8
	// total = 22 + 8 + 12 + 8 = 50
	rootInstr := prog.Instrs[prog.Root]
	if rootInstr.Kind != IRConst || rootInstr.SubOp != ConstInt || int64(rootInstr.Data) != 50 {
		t.Fatalf("expected Option/Result/List fusion and DCE to fold program into IRConst(50), got %+v", rootInstr)
	}

	// 2. Verify immediate match fusion on symbolic expressions (`match Some (x + 5) with ...`),
	// boolean complement/branch identities (`(x == x) || (x != x)`, `if cond then x else x`),
	// and associative constant folding (`(x + 10) - 10 -> x`) inside a lambda.
	srcSymbolic := []byte(`
fun (x: int) ->
    let deadLocal =
        match Ok (x + 99) with
        | Ok k -> k * 2
        | Error _ -> 0
    in
    let matched =
        match Some (x + 5) with
        | Some v -> v - 5
        | None -> 0
    in
    let sameBranch = if x > 0 then matched else matched in
    if (x == x) && ((x > 10) || not (x > 10)) && (not (x != x) == true) then
        let idLet = sameBranch in idLet
    else
        0
`)
	p2, root2, err := parser.ParseSource(srcSymbolic, nil)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	chk2 := types.NewChecker(p2.Arena, nil)
	if _, err := chk2.Infer(root2); err != nil {
		t.Fatalf("infer failed: %v", err)
	}
	prog2 := Lower(p2.Arena, chk2.Types, root2)
	Optimize(prog2)

	// matched = (x + 5) - 5 -> x
	// sameBranch = if x > 0 then x else x -> x
	// condition = true && true && true -> true
	// body of `fun (x: int) -> ...` collapses to IRVar(x)!
	fnInstr := prog2.Instrs[prog2.Root]
	if fnInstr.Kind != IRFn {
		t.Fatalf("expected IRFn at root, got %+v", fnInstr)
	}
	body := prog2.Instrs[fnInstr.Arg0]
	if body.Kind != IRVar || body.Data != fnInstr.Data {
		t.Fatalf("expected lambda body to fuse and simplify to IRVar(x), got %+v", body)
	}
}





