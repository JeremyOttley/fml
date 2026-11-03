package stdlib

import (
	"testing"

	"fml/internal/ir"
	"fml/internal/parser"
	"fml/internal/runtime"
	"fml/internal/types"
)

func runWithStdlib(t *testing.T, src string) runtime.Value {
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
	vm := runtime.NewVM(prog)
	RegisterCollections(vm)
	RegisterSeq(vm)
	RegisterFormat(vm)
	RegisterMailbox(vm)
	val, err := vm.Run()
	if err != nil {
		t.Fatalf("runtime error: %v", err)
	}
	return val
}

func TestCollections(t *testing.T) {
	// Test sync.Pool node reuse directly
	n := AcquireAVLNode(runtime.IntVal{V: 1}, runtime.StringVal{V: "one"}, nil, nil)
	ReleaseAVLNode(n)
	n2 := AcquireAVLNode(runtime.IntVal{V: 2}, runtime.StringVal{V: "two"}, nil, nil)
	if n2.Height != 1 {
		t.Fatalf("expected height 1 on pooled AVLNode, got %d", n2.Height)
	}

	// Test persistent immutability across branches in FML
	src := `
let m0 = Map.empty
let m1 = Map.add "a" 10 m0
let m2 = Map.add "b" 20 m1
let m3 = Map.add "a" 99 m2

let v1 = Map.find "a" m1
let v3 = Map.find "a" m3
let hasBInM1 = Map.containsKey "b" m1

let s0 = Set.empty
let s1 = Set.add 42 (Set.add 7 s0)
let has42 = Set.contains 42 s1
let has99 = Set.contains 99 s1

if (not hasBInM1) && has42 && (not has99) then v1 + v3 else 0
`
	res := runWithStdlib(t, src)
	// v1 (10) + v3 (99) = 109
	if iv, ok := res.(runtime.IntVal); !ok || iv.V != 109 {
		t.Fatalf("expected 109 from persistent Map/Set operations, got %v", res)
	}
}

func TestSetAlgebraAndMapRemoveKeysValues(t *testing.T) {
	src := `
let s1 = Set.ofList [10; 20; 30; 40]
let s2 = Set.ofList [30; 40; 50]
let u = Set.union s1 s2
let inter = Set.intersect s1 s2
let d = Set.diff s1 s2
let dRemoved = Set.remove 10 d
let isSub = Set.isSubset inter u

let m = Map.add "x" 5 (Map.add "y" 15 (Map.add "z" 25 Map.empty))
let m2 = Map.remove "y" m
let kCount = Map.count m2

if isSub && (Set.count u == 5) && (Set.count inter == 2) && (Set.count dRemoved == 1) then
    kCount + (Map.find "x" m2) + (Map.find "z" m2)
else
    0
`
	res := runWithStdlib(t, src)
	// kCount (2) + 5 + 25 = 32
	if iv, ok := res.(runtime.IntVal); !ok || iv.V != 32 {
		t.Fatalf("expected 32 from Set algebra and Map.remove, got %v", res)
	}
}

