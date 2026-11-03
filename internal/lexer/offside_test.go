package lexer

import (
	"testing"

	"fml/internal/token"
)

func TestOffside(t *testing.T) {
	src := []byte(`let calc x =
    let a = x + 1
    let b = a * 2
    b |> double
let next = 10
`)
	buf := make([]token.Token, 0, 128)
	l := New(src, buf)
	toks := l.ScanAll()

	var indentCount, dedentCount, semiCount int
	for _, tk := range toks {
		switch tk.Kind {
		case token.TokenIndent:
			indentCount++
		case token.TokenDedent:
			dedentCount++
		case token.TokenSemi:
			semiCount++
		}
	}

	if indentCount != 1 {
		t.Fatalf("expected 1 TokenIndent, got %d", indentCount)
	}
	if dedentCount != 1 {
		t.Fatalf("expected 1 TokenDedent, got %d", dedentCount)
	}
	if semiCount < 2 {
		t.Fatalf("expected at least 2 TokenSemi tokens inside indented block, got %d", semiCount)
	}

	allocs := testing.AllocsPerRun(100, func() {
		l.Reset(src, true)
		_ = l.ScanAll()
	})
	if allocs != 0 {
		t.Fatalf("expected 0 allocs/op in off-side lexer, got %v", allocs)
	}
}
