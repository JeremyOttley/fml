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

func TestSeqExtendedCombinators(t *testing.T) {
	src := `
let base = Seq.initInfinite (fun i -> i + 1)
let window = base |> Seq.skip 2 |> Seq.takeWhile (fun x -> x <= 6)
let extra = Seq.ofList [10; 20]
let combined = Seq.append window (Seq.append Seq.empty extra)
let zipped = Seq.zip combined (Seq.initInfinite (fun i -> i + 1))
let folded =
    zipped
    |> Seq.fold (fun acc pair -> acc + (fst pair) * (snd pair)) 0
let materialized = Seq.toList combined
folded + List.length materialized
`
	// window = [3; 4; 5; 6]
	// combined = [3; 4; 5; 6; 10; 20] (length = 6)
	// zipped with [1; 2; 3; 4; 5; 6]:
	// 3*1 + 4*2 + 5*3 + 6*4 + 10*5 + 20*6 = 3 + 8 + 15 + 24 + 50 + 120 = 220
	// folded + length = 220 + 6 = 226
	res := evalFML(t, src)
	if iv, ok := res.(runtime.IntVal); !ok || iv.V != 226 {
		t.Fatalf("expected 226 from extended Seq combinators, got %v", res)
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
