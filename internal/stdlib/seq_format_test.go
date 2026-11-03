package stdlib

import (
	"strings"
	"testing"

	"fml/internal/runtime"
)

func TestSeq(t *testing.T) {
	// Evaluate an infinite lazy sequence pipeline: naturals -> filter even -> map square -> take 5 -> sum
	// Even naturals: 0, 2, 4, 6, 8 -> squares: 0, 4, 16, 36, 64 -> sum = 120
	src := `
let naturals = Seq.initInfinite (fun i -> i)
let evens = Seq.filter (fun x -> x % 2 == 0) naturals
let squares = Seq.map (fun x -> x * x) evens
let firstFive = Seq.take 5 squares
Seq.sum firstFive
`
	res := runWithStdlib(t, src)
	if iv, ok := res.(runtime.IntVal); !ok || iv.V != 120 {
		t.Fatalf("expected lazy sequence sum 120, got %v", res)
	}
}

func TestFormat(t *testing.T) {
	src := `
let header = Format.text "begin"
let body = Format.cat Format.line (Format.text "let x = 42")
let nested = Format.nest 4 body
let footer = Format.cat Format.line (Format.text "end")
let doc = Format.group (Format.cat (Format.cat header nested) footer)
let wide = Format.pretty 80 doc
let narrow = Format.pretty 10 doc
(wide, narrow)
`
	res := runWithStdlib(t, src)
	tup, ok := res.(runtime.TupleVal)
	if !ok || len(tup.Elems) != 2 {
		t.Fatalf("expected 2-tuple of formatted strings, got %v", res)
	}
	wide := tup.Elems[0].(runtime.StringVal).V
	narrow := tup.Elems[1].(runtime.StringVal).V

	if wide != "begin let x = 42 end" {
		t.Fatalf("expected single-line layout at width 80, got %q", wide)
	}
	if !strings.Contains(narrow, "\n    let x = 42\nend") {
		t.Fatalf("expected multi-line indented layout at width 10, got %q", narrow)
	}
}
