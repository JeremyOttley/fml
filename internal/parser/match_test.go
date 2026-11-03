package parser

import (
	"testing"

	"fml/internal/ast"
)

func TestParsePatternMatching(t *testing.T) {
	src := []byte(`
let (|Even|Odd|) n = if n % 2 == 0 then Even else Odd
let classify x =
    match x with
    | 1 -> "one"
    | a when a > 0 -> "positive"
    | _ -> "other"
`)
	p, root, err := ParseSource(src, nil)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	prog := p.Arena.Get(root)
	items := p.Arena.ExtraSlice(prog.Left, prog.Right)
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	if p.Arena.Get(ast.NodeID(items[0])).Kind != ast.KindActivePattern {
		t.Fatalf("expected KindActivePattern, got %v", p.Arena.Get(ast.NodeID(items[0])).Kind)
	}

	fnLet := p.Arena.Get(ast.NodeID(items[1]))
	lam := p.Arena.Get(ast.NodeID(fnLet.Left))
	matchNode := p.Arena.Get(ast.NodeID(lam.Right))
	if matchNode.Kind != ast.KindMatch {
		t.Fatalf("expected KindMatch inside classify body, got %v", matchNode.Kind)
	}
	armBlock := p.Arena.Get(ast.NodeID(matchNode.Right))
	arms := p.Arena.ExtraSlice(armBlock.Left, armBlock.Right)
	if len(arms) != 3 {
		t.Fatalf("expected 3 match arms, got %d", len(arms))
	}
	secondArm := p.Arena.Get(ast.NodeID(arms[1]))
	if secondArm.Data == 0 || p.Arena.Get(ast.NodeID(secondArm.Data)).Kind != ast.KindGuard {
		t.Fatalf("expected KindGuard on second match arm")
	}
}
