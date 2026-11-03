package ir

import (
	"testing"

	"fml/internal/parser"
	"fml/internal/types"
)

func TestLowerDesugar(t *testing.T) {
	src := []byte(`
let nested =
    let a = 1 in
    let b = 2 in
    a + b
let inc x = x + 1
let dbl x = x * 2
let comp = inc >> dbl
let piped = 10 |> comp
let wf = async {
    let! v = piped
    return v + 1
}
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
	if prog.Root == NoInstr || len(prog.Instrs) == 0 {
		t.Fatalf("expected non-empty lowered IR program")
	}

	var hasApp, hasFn, hasLet bool
	for _, ins := range prog.Instrs {
		switch ins.Kind {
		case IRApp:
			hasApp = true
		case IRFn:
			hasFn = true
		case IRLet:
			hasLet = true
		}
	}
	if !hasApp || !hasFn || !hasLet {
		t.Fatalf("expected desugared IRApp, IRFn, and IRLet instructions in lowered program")
	}
}
