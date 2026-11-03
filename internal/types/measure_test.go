package types

import (
	"testing"

	"fml/internal/parser"
)

func TestUnitsOfMeasure(t *testing.T) {
	// Valid dimensional analysis: kg * (m / s^2) == kg*m/s^2
	validSrc := []byte(`
[<Measure>] type kg
[<Measure>] type m
[<Measure>] type s
let mass = 5.0<kg>
let dist = 10.0<m>
let time = 2.0<s>
let accel = dist / (time * time)
let force = mass * accel
let expectedForce = 25.0<kg*m/s^2>
let totalForce = force + expectedForce
`)
	p, root, err := parser.ParseSource(validSrc, nil)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	chk := NewChecker(p.Arena, nil)
	ty, err := chk.Infer(root)
	if err != nil {
		t.Fatalf("expected valid dimensional analysis to succeed, got: %v", err)
	}
	r := chk.Types.Find(ty)
	if chk.Types.Nodes[r].Kind != TyMeasure {
		t.Fatalf("expected TyMeasure before erasure, got %s", chk.Types.Format(r))
	}

	// Verify unit erasure reduces TyMeasure to underlying TyFloat
	chk.Types.EraseMeasures()
	if chk.Types.Find(ty) != TypeFloat {
		t.Fatalf("expected TypeFloat after EraseMeasures, got %s", chk.Types.Format(ty))
	}

	// Invalid dimensional addition: float<kg> + float<m> must fail
	invalidSrc := []byte(`
[<Measure>] type kg
[<Measure>] type m
let bad = 5.0<kg> + 10.0<m>
`)
	p2, root2, err := parser.ParseSource(invalidSrc, nil)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	chk2 := NewChecker(p2.Arena, nil)
	if _, err := chk2.Infer(root2); err == nil {
		t.Fatalf("expected compile-time error when adding 5.0<kg> + 10.0<m>")
	}
}
