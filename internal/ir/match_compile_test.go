package ir

import (
	"testing"

	"fml/internal/parser"
	"fml/internal/types"
)

func TestMatchDecisionTree(t *testing.T) {
	src := []byte(`
type Shape = Circle of float | Rect of float * float | Point
let classify s =
    match s with
    | Circle r -> r
    | Rect (w, h) -> w * h
    | Point -> 0.0
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

	// Verify all three ADT cases are grouped into a single IRSwitch instruction
	switchCount := 0
	for _, ins := range prog.Instrs {
		if ins.Kind == IRSwitch {
			switchCount++
			if ins.Data != 3 {
				t.Fatalf("expected 3 grouped cases in single IRSwitch decision tree, got %d", ins.Data)
			}
		}
	}
	if switchCount != 1 {
		t.Fatalf("expected exactly 1 IRSwitch instruction for 3-way ADT match, got %d", switchCount)
	}
}
