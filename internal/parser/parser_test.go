package parser

import (
	"testing"

	"fml/internal/ast"
)

func TestParseBindingsAndExprs(t *testing.T) {
	src := []byte(`
let x = 10
let mutable m = 10
m <- 20
let rec fact n = if n <= 1 then 1 else n * fact (n - 1)
let use reader = openResource "data.txt"
let inc = fun a -> a + 1
let add x y = x + y
let piped = 10 |> inc |> add 5
let composed = inc >> inc
let backPiped = inc <| 42
`)
	p, root, err := ParseSource(src, nil)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	prog := p.Arena.Get(root)
	if prog.Kind != ast.KindProgram {
		t.Fatalf("expected KindProgram, got %v", prog.Kind)
	}

	items := p.Arena.ExtraSlice(prog.Left, prog.Right)
	if len(items) != 10 {
		t.Fatalf("expected 10 top-level statements, got %d", len(items))
	}

	expectedKinds := []ast.NodeKind{
		ast.KindLet,
		ast.KindLetMutable,
		ast.KindMutate,
		ast.KindLetRec,
		ast.KindUse,
		ast.KindLet,
		ast.KindLet,
		ast.KindLet,
		ast.KindLet,
		ast.KindLet,
	}
	for i, exp := range expectedKinds {
		got := p.Arena.Get(ast.NodeID(items[i])).Kind
		if got != exp {
			t.Fatalf("stmt %d: expected %v, got %v", i, exp, got)
		}
	}

	// Verify curried `let add x y = x + y` lowered into nested KindLambda
	addNode := p.Arena.Get(ast.NodeID(items[6]))
	lam1 := p.Arena.Get(ast.NodeID(addNode.Left))
	if lam1.Kind != ast.KindLambda {
		t.Fatalf("expected outer KindLambda on curried add, got %v", lam1.Kind)
	}
	lam2 := p.Arena.Get(ast.NodeID(lam1.Right))
	if lam2.Kind != ast.KindLambda {
		t.Fatalf("expected inner KindLambda on curried add, got %v", lam2.Kind)
	}
}
