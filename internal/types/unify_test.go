package types

import "testing"

func TestUnify(t *testing.T) {
	ta := NewTypeArena(512, nil)

	// Unify ('a -> int) with (bool -> 'b)
	a := ta.FreshVar()
	b := ta.FreshVar()
	fn1 := ta.Add(TypeNode{Kind: TyArrow, Left: uint32(a), Right: uint32(TypeInt)})
	fn2 := ta.Add(TypeNode{Kind: TyArrow, Left: uint32(TypeBool), Right: uint32(b)})

	if err := ta.Unify(fn1, fn2); err != nil {
		t.Fatalf("expected arrow unification to succeed, got: %v", err)
	}
	if ta.Find(a) != TypeBool {
		t.Fatalf("expected 'a == bool, got %s", ta.Format(a))
	}
	if ta.Find(b) != TypeInt {
		t.Fatalf("expected 'b == int, got %s", ta.Format(b))
	}

	// Verify Occurs Check rejects infinite cyclic type: 'c = 'c -> int
	c := ta.FreshVar()
	selfArrow := ta.Add(TypeNode{Kind: TyArrow, Left: uint32(c), Right: uint32(TypeInt)})
	if err := ta.Unify(c, selfArrow); err == nil {
		t.Fatalf("expected occurs check error on self-referential type a = a -> b")
	}

	// Verify incompatible concrete types fail
	if err := ta.Unify(TypeInt, TypeString); err == nil {
		t.Fatalf("expected error unifying int with string")
	}
}
