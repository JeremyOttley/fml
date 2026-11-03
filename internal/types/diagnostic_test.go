package types

import (
	"strings"
	"testing"

	"fml/internal/parser"
)

func TestSourceDiagnosticsWithLineColAndCarets(t *testing.T) {
	src := []byte(`let x = 10
let y = "hello"
let bad = x + y
`)
	p, root, err := parser.ParseSource(src, nil)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	chk := NewChecker(p.Arena, nil)
	chk.AttachSource(p.Source(), p.Tokens())
	_, err = chk.Infer(root)
	if err == nil {
		t.Fatalf("expected type error on x + y")
	}
	msg := err.Error()
	if !strings.Contains(msg, "--> line 3") || !strings.Contains(msg, "let bad = x + y") || !strings.Contains(msg, "^") {
		t.Fatalf("expected rich diagnostic with line 3, source snippet, and caret underline; got:\n%s", msg)
	}
}
