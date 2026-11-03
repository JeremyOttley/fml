package runtime

import (
	"testing"

	"fml/internal/ir"
	"fml/internal/parser"
	"fml/internal/types"
)

func evalSource(t *testing.T, src string) Value {
	t.Helper()
	p, root, err := parser.ParseSource([]byte(src), nil)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	chk := types.NewChecker(p.Arena, nil)
	if _, err := chk.Infer(root); err != nil {
		t.Fatalf("type error: %v", err)
	}
	prog := ir.Lower(p.Arena, chk.Types, root)
	vm := NewVM(prog)
	val, err := vm.Run()
	if err != nil {
		t.Fatalf("runtime error: %v", err)
	}
	return val
}

func TestVMEvaluatorAndClosures(t *testing.T) {
	// 1. Recursive factorial, curried closures, pipelines, composition
	src1 := `
let rec fact n = if n <= 1 then 1 else n * fact (n - 1)
let makeAdder k = fun x -> x + k
let add5 = makeAdder 5
let double x = x * 2
let pipelineRes = 5 |> fact |> add5 |> (add5 >> double)
pipelineRes
`
	v1 := evalSource(t, src1)
	// fact 5 = 120; add5 120 = 125; (add5 >> double) 125 = (125 + 5) * 2 = 260
	if iv, ok := v1.(IntVal); !ok || iv.V != 260 {
		t.Fatalf("expected 260, got %v", v1)
	}

	// 2. Mutable variables and while/for loops
	src2 := `
let mutable sum = 0
for i = 1 to 5 do sum <- sum + i
let mutable count = 3
while count > 0 do
    sum <- sum + 10
    count <- count - 1
sum
`
	v2 := evalSource(t, src2)
	// 1+2+3+4+5 = 15; + 30 = 45
	if iv, ok := v2.(IntVal); !ok || iv.V != 45 {
		t.Fatalf("expected 45, got %v", v2)
	}

	// 3. ADT, Tuple, Record, Option, Active Patterns, and Pattern Guards
	src3 := `
type Shape = Circle of int | Rect of int * int
let (|Big|Small|) n = if n >= 50 then Big else Small
let evalShape s =
    match s with
    | Circle r when r > 10 -> r * 10
    | Circle r -> r * 2
    | Rect (w, h) -> w * h
let r1 = evalShape (Circle 4)
let r2 = evalShape (Rect (6, 9))
let classify n =
    match n with
    | Big -> 100
    | Small -> 1
r1 + classify r2
`
	v3 := evalSource(t, src3)
	// r1 = 8; r2 = 54 (Big -> 100); r1 + 100 = 108
	if iv, ok := v3.(IntVal); !ok || iv.V != 108 {
		t.Fatalf("expected 108, got %v", v3)
	}
}

func TestVMModulesFunctorsClassesAndNestedPatterns(t *testing.T) {
	src := `
type Shape = Circle of int | Rect of int * int

let evalNested s =
    match s with
    | Circle 0 -> 999
    | Circle r -> r * 2
    | Rect (0, h) -> h
    | Rect (w, h) -> w * h

let n1 = evalNested (Circle 0)
let n2 = evalNested (Circle 7)
let n3 = evalNested (Rect (0, 15))
let n4 = evalNested (Rect (3, 4))

module IntOrd =
    let eq a b = if a == b then 10 else 0

module MakeChecker (Ord: COMPARABLE) =
    let check x y = Ord.eq x y

module IntChecker = MakeChecker(IntOrd)
let fRes = IntChecker.check 5 5

type Counter(init: int) =
    member _.Get = init + 1

let c = Counter 41
let obj = { new IPrintable with member _.Val = 8 }

n1 + n2 + n3 + n4 + fRes + c.Get + obj.Val
`
	v := evalSource(t, src)
	// n1 (999) + n2 (14) + n3 (15) + n4 (12) + fRes (10) + c.Get (42) + obj.Val (8) = 1100
	if iv, ok := v.(IntVal); !ok || iv.V != 1100 {
		t.Fatalf("expected 1100, got %v", v)
	}
}
