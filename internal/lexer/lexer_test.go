package lexer

import (
	"testing"

	"fml/internal/token"
)

func TestLexerAllTokens(t *testing.T) {
	src := []byte(`
// Line comment
(* Nested (* block *) comment *)
[<Measure>] type kg
let mutable x = 10
x <- 20
let rec fact n = if n <= 1 then 1 else n * fact (n - 1)
let f = fun a -> a |> g >> h <| 5.0<kg>
let arr = [| 1; 2; 3 |]
let xs = [1 .. 10]
let q = <@ 1 + 1 @>
let (|Even|Odd|) n = ()
async { let! res = op() do! act() use! r = res return res }
match x with | :? Exn -> true | _ -> false
`)

	buf := make([]token.Token, 0, 256)
	l := New(src, buf)
	toks := l.ScanRaw()

	expectedSubset := []token.TokenKind{
		token.TokenLAttr, token.TokenMeasure, token.TokenRAttr, token.TokenType, token.TokenIdent,
		token.TokenLet, token.TokenMutable, token.TokenIdent, token.TokenAssign, token.TokenInt,
		token.TokenIdent, token.TokenMutate, token.TokenInt,
		token.TokenLet, token.TokenRec,
		token.TokenPipeRight, token.TokenComposeRight, token.TokenPipeLeft, token.TokenFloat,
		token.TokenLArray, token.TokenRArray,
		token.TokenRange,
		token.TokenLQuote, token.TokenRQuote,
		token.TokenLActive, token.TokenRActive, token.TokenUnit,
		token.TokenAsync, token.TokenLetBang, token.TokenDoBang, token.TokenUseBang, token.TokenReturn,
		token.TokenMatch, token.TokenWith, token.TokenColonQMark, token.TokenUnderscore,
		token.TokenEOF,
	}

	idx := 0
	for _, tok := range toks {
		if idx < len(expectedSubset) && tok.Kind == expectedSubset[idx] {
			idx++
		}
	}
	if idx != len(expectedSubset) {
		t.Fatalf("failed to match expected token sequence at index %d (%v)", idx, expectedSubset[idx])
	}

	// Verify 0 allocs/op on pre-sized buffer
	allocs := testing.AllocsPerRun(100, func() {
		l.Reset(src, false)
		_ = l.ScanRaw()
	})
	if allocs != 0 {
		t.Fatalf("expected 0 allocs/op in Lexer.ScanRaw, got %v", allocs)
	}
}

func BenchmarkLexerScanAll(b *testing.B) {
	src := []byte(`
let compute x y =
    let a = x + y
    let b = a * 2
    match b with
    | 0 -> 1
    | _ -> b |> transform
`)
	buf := make([]token.Token, 0, 128)
	l := New(src, buf)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.Reset(src, true)
		_ = l.ScanAll()
	}
}
