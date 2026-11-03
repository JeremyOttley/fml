package types

import (
	"testing"

	"fml/internal/parser"
)

func TestAlgorithmW(t *testing.T) {
	// 1. Polymorphic let, recursive let rec, curried pipelines
	src := []byte(`
let identity x = x
let a = identity 42
let b = identity "hello"
let rec fact n = if n <= 1 then 1 else n * fact (n - 1)
let add x y = x + y
let res = 10 |> add 5
`)
	p, root, err := parser.ParseSource(src, nil)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	chk := NewChecker(p.Arena, nil)
	ty, err := chk.Infer(root)
	if err != nil {
		t.Fatalf("Algorithm W inference failed: %v", err)
	}
	if chk.Types.Find(ty) != TypeInt {
		t.Fatalf("expected final expression type int, got %s", chk.Types.Format(ty))
	}

	// 2. Value Restriction: `let mutable` must remain monomorphic and reject conflicting uses
	unsoundSrc := []byte(`
let mutable box = None
box <- Some 10
box <- Some "unsound"
`)
	p2, root2, err := parser.ParseSource(unsoundSrc, nil)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	chk2 := NewChecker(p2.Arena, nil)
	if _, err := chk2.Infer(root2); err == nil {
		t.Fatalf("expected Value Restriction to reject polymorphic mutation on `let mutable box = None`")
	}

	// 3. Reject mutation of immutable `let` binding
	immutSrc := []byte(`
let x = 10
x <- 20
`)
	p3, root3, err := parser.ParseSource(immutSrc, nil)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	chk3 := NewChecker(p3.Arena, nil)
	if _, err := chk3.Infer(root3); err == nil {
		t.Fatalf("expected error mutating immutable binding x")
	}
}
