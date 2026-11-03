package types

import (
	"testing"

	"fml/internal/parser"
	"fml/internal/token"
)

func TestExhaustiveAndFunctors(t *testing.T) {
	// 1. Non-exhaustive Option match (missing None) must fail
	nonExOpt := []byte(`
let unwrap x =
    match x with
    | Some v -> v
`)
	p1, r1, err := parser.ParseSource(nonExOpt, nil)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	c1 := NewChecker(p1.Arena, nil)
	if _, err := c1.Infer(r1); err == nil {
		t.Fatalf("expected non-exhaustive Option match error when None is missing")
	}

	// 2. Non-exhaustive ADT match (missing Rect) must fail
	nonExADT := []byte(`
type Shape = Circle of float | Rect of float * float
let area s =
    match s with
    | Circle r -> r * r
`)
	p2, r2, err := parser.ParseSource(nonExADT, nil)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	c2 := NewChecker(p2.Arena, nil)
	if _, err := c2.Infer(r2); err == nil {
		t.Fatalf("expected non-exhaustive ADT match error when Rect is missing")
	}

	// 3. Complete ADT match and SML Parametric Module Functor instantiation
	validSrc := []byte(`
type Shape = Circle of float | Rect of float * float
let area s =
    match s with
    | Circle r -> r * r
    | Rect p -> 0.0

module IntOrd =
    let compare a b = if a == b then 0 else 1

module MakeMap (Ord: COMPARABLE) =
    let check x y = Ord.compare x y

module IntMap = MakeMap(IntOrd)
`)
	p3, r3, err := parser.ParseSource(validSrc, nil)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	c3 := NewChecker(p3.Arena, nil)
	// Register COMPARABLE signature requiring `compare : 'a -> 'a -> int`
	tv := c3.Types.FreshVar()
	cmpTy := c3.Types.Add(TypeNode{
		Kind:  TyArrow,
		Left:  uint32(tv),
		Right: uint32(c3.Types.Add(TypeNode{Kind: TyArrow, Left: uint32(tv), Right: uint32(TypeInt)})),
	})
	c3.Functors.RegisterSignature(ModuleSig{
		Name: c3.AST.Strings.InternString("COMPARABLE"),
		Values: map[token.StringID]TypeID{
			c3.AST.Strings.InternString("compare"): cmpTy,
		},
	})

	if _, err := c3.Infer(r3); err != nil {
		t.Fatalf("expected exhaustive ADT match and functor instantiation to pass, got: %v", err)
	}

	// 4. Nested non-total tuple pattern `(1, x)` without catch-all must fail exhaustiveness
	nonTotalTuple := []byte(`
let badPair p =
    match p with
    | (1, x) -> x
`)
	p4, r4, err := parser.ParseSource(nonTotalTuple, nil)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	c4 := NewChecker(p4.Arena, nil)
	if _, err := c4.Infer(r4); err == nil {
		t.Fatalf("expected non-exhaustive error for tuple pattern with constant element (1, x)")
	}

	// 5. Partial Active Pattern without catch-all `_` must fail exhaustiveness
	nonExPartial := []byte(`
let (|Positive|_|) n = if n > 0 then Some n else None
let check x =
    match x with
    | Positive v -> v
`)
	p5, r5, err := parser.ParseSource(nonExPartial, nil)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	c5 := NewChecker(p5.Arena, nil)
	if _, err := c5.Infer(r5); err == nil {
		t.Fatalf("expected non-exhaustive error when matching partial active pattern without fallback")
	}

	// 6. Active Pattern domain type mismatch (matching string against int active pattern) must fail
	badActiveDomain := []byte(`
let (|Even|Odd|) n = if n % 2 == 0 then Even else Odd
let bad =
    match "not_an_int" with
    | Even -> 1
    | Odd -> 0
`)
	p6, r6, err := parser.ParseSource(badActiveDomain, nil)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	c6 := NewChecker(p6.Arena, nil)
	if _, err := c6.Infer(r6); err == nil {
		t.Fatalf("expected type error when matching string against int active pattern")
	}
}
