package types

import (
	"bytes"
	"fmt"
	"strings"

	"fml/internal/ast"
	"fml/internal/token"
)

// AttachSource binds the original source buffer and token slice to c so type
// inference and exhaustiveness errors include source line/column numbers and
// caret underlines.
//
// Jaime says: Every token already knows its 16-bit Line, 16-bit Col, and 32-bit
// byte offsets. Don't make the programmer play hide-and-seek when two types
// fail to unify—slice the source buffer and point straight at the offending token.
func (c *Checker) AttachSource(src []byte, tokens []token.Token) {
	c.Source = src
	c.Tokens = tokens
}

func (c *Checker) findNodeToken(id ast.NodeID) (token.Token, bool) {
	if id == ast.NoNode || len(c.Tokens) == 0 {
		return token.Token{}, false
	}
	n := *c.AST.Get(id)
	if n.TokenIdx > 0 && int(n.TokenIdx) < len(c.Tokens) {
		tk := c.Tokens[n.TokenIdx]
		if tk.Line > 0 {
			return tk, true
		}
	}
	if n.Left != 0 && int(n.Left) < len(c.AST.Nodes) {
		if tk, ok := c.findNodeToken(ast.NodeID(n.Left)); ok {
			return tk, true
		}
	}
	if n.Right != 0 && int(n.Right) < len(c.AST.Nodes) {
		if tk, ok := c.findNodeToken(ast.NodeID(n.Right)); ok {
			return tk, true
		}
	}
	return token.Token{}, false
}

// FormatDiagnostic formats err with a source snippet and `^~~~` caret underline
// pointing at the token of nodeID.
func (c *Checker) FormatDiagnostic(nodeID ast.NodeID, err error) error {
	if err == nil || len(c.Source) == 0 || len(c.Tokens) == 0 {
		return err
	}
	msg := err.Error()
	if strings.Contains(msg, "--> line ") {
		return err
	}
	tk, ok := c.findNodeToken(nodeID)
	if !ok || tk.Line == 0 {
		return err
	}

	lines := bytes.Split(c.Source, []byte("\n"))
	lineIdx := int(tk.Line) - 1
	if lineIdx < 0 || lineIdx >= len(lines) {
		return fmt.Errorf("%w\n  --> line %d, col %d", err, tk.Line, tk.Col)
	}
	srcLine := string(lines[lineIdx])
	col := int(tk.Col)
	if col < 1 {
		col = 1
	}
	spanLen := int(tk.End) - int(tk.Start)
	if spanLen < 1 {
		spanLen = 1
	}
	caret := "^"
	if spanLen > 1 {
		caret += strings.Repeat("~", spanLen-1)
	}
	pad := strings.Repeat(" ", col-1)

	return fmt.Errorf("%w\n  --> line %d, col %d\n  %4d | %s\n       | %s%s",
		err, tk.Line, col, tk.Line, srcLine, pad, caret)
}
