package parser

import (
	"testing"

	"fml/internal/ast"
)

func TestParseDataTypes(t *testing.T) {
	src := []byte(`
let t = (1, "hello")
type Person = { Name: string; Age: int }
type Shape = Circle of float | Rect of float * float
let xs = [1; 2; 3]
let arr = [| 1; 2; 3 |]
let s = seq { 1 .. 10 }
let optSome = Some 5
let optNone = None
[<Measure>] type kg
let w = 5.0<kg>
`)
	p, root, err := ParseSource(src, nil)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}

	prog := p.Arena.Get(root)
	items := p.Arena.ExtraSlice(prog.Left, prog.Right)
	if len(items) != 10 {
		t.Fatalf("expected 10 items, got %d", len(items))
	}

	// Verify tuple, record type, DU type, list, array, seq, options, measure
	if p.Arena.Get(ast.NodeID(p.Arena.Get(ast.NodeID(items[0])).Left)).Kind != ast.KindTuple {
		t.Fatalf("expected KindTuple")
	}
	if p.Arena.Get(ast.NodeID(items[1])).Kind != ast.KindRecordType {
		t.Fatalf("expected KindRecordType")
	}
	if p.Arena.Get(ast.NodeID(items[2])).Kind != ast.KindDUType {
		t.Fatalf("expected KindDUType")
	}
	if p.Arena.Get(ast.NodeID(p.Arena.Get(ast.NodeID(items[3])).Left)).Kind != ast.KindList {
		t.Fatalf("expected KindList")
	}
	if p.Arena.Get(ast.NodeID(p.Arena.Get(ast.NodeID(items[4])).Left)).Kind != ast.KindArray {
		t.Fatalf("expected KindArray")
	}
	if p.Arena.Get(ast.NodeID(p.Arena.Get(ast.NodeID(items[5])).Left)).Kind != ast.KindSeq {
		t.Fatalf("expected KindSeq")
	}
	if p.Arena.Get(ast.NodeID(p.Arena.Get(ast.NodeID(items[6])).Left)).Kind != ast.KindOption {
		t.Fatalf("expected KindOption for Some 5")
	}
	if p.Arena.Get(ast.NodeID(items[8])).Kind != ast.KindMeasure {
		t.Fatalf("expected KindMeasure for [<Measure>] type kg")
	}
	if p.Arena.Get(ast.NodeID(p.Arena.Get(ast.NodeID(items[9])).Left)).Kind != ast.KindMeasure {
		t.Fatalf("expected KindMeasure for 5.0<kg>")
	}
}
