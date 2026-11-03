// Package repl implements the interactive Read-Eval-Print Loop (`fml repl`) and
// static type/signature inspector (`fml check`) for FML.
//
// Jaime says: An ML compiler without a REPL and `:type` inspector is like an
// oscilloscope without a probe. Let the programmer inspect inferred principal
// types, units of measure, AST quotations, and lowered 10-opcode IR on the fly.
package repl

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"

	"fml/internal/ast"
	"fml/internal/codegen"
	"fml/internal/ir"
	"fml/internal/parser"
	"fml/internal/runtime"
	"fml/internal/stdlib"
	"fml/internal/token"
	"fml/internal/types"
)

// CheckFile parses and type-checks path without executing it, returning a
// formatted summary of all top-level inferred types and bindings.
func CheckFile(path string) (string, error) {
	p, root, err := parser.ParseFile(path, nil)
	if err != nil {
		return "", err
	}
	chk := types.NewChecker(p.Arena, nil)
	chk.BaseDir = filepath.Dir(path)
	chk.AttachSource(p.Source(), p.Tokens())
	rootTy, err := chk.Infer(root)
	if err != nil {
		return "", err
	}

	var buf bytes.Buffer
	progNode := p.Arena.Get(root)
	children := p.Arena.ExtraSlice(progNode.Left, progNode.Right)
	for _, chID := range children {
		n := p.Arena.Get(ast.NodeID(chID))
		switch n.Kind {
		case ast.KindLet, ast.KindLetRec, ast.KindLetMutable, ast.KindUse:
			name := token.StringID(n.Data)
			if name != token.NoString {
				ty := chk.NodeTypes[chID]
				fmt.Fprintf(&buf, "val %s : %s\n", p.Arena.Strings.String(name), chk.Types.Format(ty))
			}
		case ast.KindModule:
			modName := token.StringID(n.Data)
			fmt.Fprintf(&buf, "module %s\n", p.Arena.Strings.String(modName))
		}
	}
	fmt.Fprintf(&buf, "result : %s\n", chk.Types.Format(rootTy))
	return buf.String(), nil
}

// Session maintains persistent declaration history for an interactive REPL session.
type Session struct {
	History []string
	Stdout  io.Writer
}

// NewSession creates a new interactive REPL Session.
func NewSession(stdout io.Writer) *Session {
	return &Session{
		Stdout: stdout,
	}
}

// EvalLine evaluates a single REPL input or meta-command (`:type`, `:ir`, `:quote`, `:help`).
func (s *Session) EvalLine(line string) (string, error) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return "", nil
	}

	switch {
	case trimmed == ":help" || trimmed == ":h":
		return "Commands:\n  :type <expr>   Inspect inferred Hindley-Milner type (with Units of Measure)\n  :ir <expr>     Disassemble lowered 10-opcode Lambda IR\n  :quote <expr>  Reify and format first-class AST quotation <@ expr @>\n  :fmt <expr>    Pretty-print FML source via Wadler layout engine\n  :quit          Exit the REPL", nil

	case strings.HasPrefix(trimmed, ":fmt "):
		expr := strings.TrimSpace(strings.TrimPrefix(trimmed, ":fmt "))
		return FormatSource([]byte(expr), 80)

	case strings.HasPrefix(trimmed, ":type ") || strings.HasPrefix(trimmed, ":t "):
		expr := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(trimmed, ":type"), ":t"))
		return s.inspectType(expr)

	case strings.HasPrefix(trimmed, ":ir "):
		expr := strings.TrimSpace(strings.TrimPrefix(trimmed, ":ir "))
		return s.inspectIR(expr)

	case strings.HasPrefix(trimmed, ":quote "):
		expr := strings.TrimSpace(strings.TrimPrefix(trimmed, ":quote "))
		return s. EvalLine("<@ " + expr + " @>")
	}

	fullSrc := s.buildSource(trimmed)
	p, root, err := parser.ParseSource([]byte(fullSrc), nil)
	if err != nil {
		return "", err
	}
	chk := types.NewChecker(p.Arena, nil)
	chk.AttachSource(p.Source(), p.Tokens())
	ty, err := chk.Infer(root)
	if err != nil {
		return "", err
	}
	// Format inferred type before Lower erases units of measure!
	tyStr := chk.Types.Format(ty)

	prog := ir.Lower(p.Arena, chk.Types, root)
	vm := runtime.NewVM(prog)
	if s.Stdout != nil {
		vm.Stdout = s.Stdout
	}
	stdlib.RegisterAll(vm)
	val, err := vm.Run()
	if err != nil {
		return "", err
	}

	bindingName := "it"
	progNode := p.Arena.Get(root)
	children := p.Arena.ExtraSlice(progNode.Left, progNode.Right)
	if len(children) > 0 {
		lastNode := p.Arena.Get(ast.NodeID(children[len(children)-1]))
		if lastNode.Kind == ast.KindLet || lastNode.Kind == ast.KindLetRec || lastNode.Kind == ast.KindLetMutable {
			if lastNode.Data != 0 {
				bindingName = p.Arena.Strings.String(token.StringID(lastNode.Data))
			}
		}
	}

	if isPersistentDecl(trimmed) {
		s.History = append(s.History, trimmed)
	}

	valStr := "()"
	if val != nil {
		valStr = val.String()
	}
	return fmt.Sprintf("val %s : %s = %s", bindingName, tyStr, valStr), nil
}

func (s *Session) inspectType(expr string) (string, error) {
	fullSrc := s.buildSource(expr)
	p, root, err := parser.ParseSource([]byte(fullSrc), nil)
	if err != nil {
		return "", err
	}
	chk := types.NewChecker(p.Arena, nil)
	chk.AttachSource(p.Source(), p.Tokens())
	ty, err := chk.Infer(root)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s : %s", expr, chk.Types.Format(ty)), nil
}

func (s *Session) inspectIR(expr string) (string, error) {
	fullSrc := s.buildSource(expr)
	p, root, err := parser.ParseSource([]byte(fullSrc), nil)
	if err != nil {
		return "", err
	}
	chk := types.NewChecker(p.Arena, nil)
	chk.AttachSource(p.Source(), p.Tokens())
	if _, err := chk.Infer(root); err != nil {
		return "", err
	}
	prog := ir.Lower(p.Arena, chk.Types, root)
	return strings.TrimRight(codegen.Disassemble(prog), "\n"), nil
}

func (s *Session) buildSource(current string) string {
	if len(s.History) == 0 {
		return current + "\n"
	}
	return strings.Join(s.History, "\n") + "\n" + current + "\n"
}

func isPersistentDecl(line string) bool {
	for _, prefix := range []string{"let ", "type ", "module ", "open ", "[<"} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// RunInteractive runs an interactive line-by-line REPL loop over in and out.
func RunInteractive(in io.Reader, out io.Writer) error {
	sess := NewSession(out)
	scanner := bufio.NewScanner(in)
	fmt.Fprintln(out, "FML Interactive REPL (type :help for commands, :quit to exit)")
	for {
		fmt.Fprint(out, "fml> ")
		if !scanner.Scan() {
			fmt.Fprintln(out)
			return scanner.Err()
		}
		line := strings.TrimSpace(scanner.Text())
		if line == ":quit" || line == ":q" || line == "exit" {
			return nil
		}
		res, err := sess.EvalLine(line)
		if err != nil {
			fmt.Fprintf(out, "error: %v\n", err)
			continue
		}
		if res != "" {
			fmt.Fprintln(out, res)
		}
	}
}

// FormatFile parses an FML source file and formats it using Wadler's Pretty layout engine.
func FormatFile(path string, maxWidth int) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return FormatSource(b, maxWidth)
}

// FormatSource parses src into an ast.Arena and pretty-prints it within maxWidth columns
// using the Wadler algebraic document layout engine (stdlib.Doc / stdlib.Pretty).
func FormatSource(src []byte, maxWidth int) (string, error) {
	if maxWidth <= 0 {
		maxWidth = 80
	}
	p, root, err := parser.ParseSource(src, nil)
	if err != nil {
		return "", err
	}
	prog := p.Arena.Get(root)
	children := p.Arena.ExtraSlice(prog.Left, prog.Right)
	var lines []string
	for _, chID := range children {
		doc := astToDoc(p.Arena, ast.NodeID(chID))
		lines = append(lines, stdlib.Pretty(maxWidth, doc))
	}
	return strings.Join(lines, "\n") + "\n", nil
}

func dText(s string) *stdlib.Doc {
	return &stdlib.Doc{Kind: stdlib.DocText, Text: s}
}

func dLine() *stdlib.Doc {
	return &stdlib.Doc{Kind: stdlib.DocLine}
}

func dCat(a, b *stdlib.Doc) *stdlib.Doc {
	return &stdlib.Doc{Kind: stdlib.DocCat, Left: a, Right: b}
}

func dSeq(docs ...*stdlib.Doc) *stdlib.Doc {
	var res *stdlib.Doc
	for _, d := range docs {
		if d == nil {
			continue
		}
		if res == nil {
			res = d
		} else {
			res = dCat(res, d)
		}
	}
	if res == nil {
		return &stdlib.Doc{Kind: stdlib.DocNil}
	}
	return res
}

func dGroup(d *stdlib.Doc) *stdlib.Doc {
	return &stdlib.Doc{Kind: stdlib.DocGroup, Left: d}
}

func dNest(indent int, d *stdlib.Doc) *stdlib.Doc {
	return &stdlib.Doc{Kind: stdlib.DocNest, Indent: indent, Left: d}
}

func astToDoc(arena *ast.Arena, id ast.NodeID) *stdlib.Doc {
	if id == ast.NoNode {
		return dText("()")
	}
	n := *arena.Get(id)
	s := arena.Strings

	switch n.Kind {
	case ast.KindUnit:
		return dText("()")
	case ast.KindBool:
		if n.Data != 0 {
			return dText("true")
		}
		return dText("false")
	case ast.KindInt:
		return dText(fmt.Sprintf("%d", int64(n.Data)))
	case ast.KindFloat:
		return dText(fmt.Sprintf("%g", math.Float64frombits(n.Data)))
	case ast.KindString:
		return dText(fmt.Sprintf("%q", s.String(token.StringID(n.Data))))
	case ast.KindIdent:
		return dText(s.String(token.StringID(n.Data)))

	case ast.KindLet, ast.KindLetMutable, ast.KindLetRec, ast.KindUse:
		kw := "let "
		if n.Kind == ast.KindLetMutable {
			kw = "let mutable "
		} else if n.Kind == ast.KindLetRec {
			kw = "let rec "
		} else if n.Kind == ast.KindUse || n.Flags&ast.FlagUse != 0 {
			kw = "let use "
		}
		name := s.String(token.StringID(n.Data))
		var params []string
		rhsID := ast.NodeID(n.Left)
		for rhsID != ast.NoNode {
			rn := *arena.Get(rhsID)
			if rn.Kind == ast.KindLambda {
				params = append(params, s.String(token.StringID(rn.Data)))
				rhsID = ast.NodeID(rn.Right)
			} else {
				break
			}
		}
		header := kw + name
		if len(params) > 0 {
			header += " " + strings.Join(params, " ")
		}
		header += " ="
		rhsDoc := astToDoc(arena, rhsID)
		letDoc := dGroup(dSeq(dText(header), dNest(4, dSeq(dLine(), rhsDoc))))
		if n.Right != 0 {
			inDoc := astToDoc(arena, ast.NodeID(n.Right))
			return dGroup(dSeq(letDoc, dText(" in"), dLine(), inDoc))
		}
		return letDoc

	case ast.KindLambda:
		param := s.String(token.StringID(n.Data))
		body := astToDoc(arena, ast.NodeID(n.Right))
		return dGroup(dSeq(dText("fun "+param+" ->"), dNest(4, dSeq(dLine(), body))))

	case ast.KindInfix:
		lDoc := astToDoc(arena, ast.NodeID(n.Left))
		rDoc := astToDoc(arena, ast.NodeID(n.Right))
		opStr := token.TokenKind(n.Op).String()
		return dGroup(dSeq(lDoc, dText(" "+opStr), dNest(4, dSeq(dLine(), rDoc))))

	case ast.KindPipeRight:
		lDoc := astToDoc(arena, ast.NodeID(n.Left))
		rDoc := astToDoc(arena, ast.NodeID(n.Right))
		return dGroup(dSeq(lDoc, dNest(4, dSeq(dLine(), dText("|> "), rDoc))))

	case ast.KindPipeLeft:
		lDoc := astToDoc(arena, ast.NodeID(n.Left))
		rDoc := astToDoc(arena, ast.NodeID(n.Right))
		return dGroup(dSeq(lDoc, dText(" <| "), rDoc))

	case ast.KindApply:
		fnDoc := astToDoc(arena, ast.NodeID(n.Left))
		argNode := arena.Get(ast.NodeID(n.Right))
		argDoc := astToDoc(arena, ast.NodeID(n.Right))
		if argNode.Kind == ast.KindApply || argNode.Kind == ast.KindInfix {
			argDoc = dSeq(dText("("), argDoc, dText(")"))
		}
		return dGroup(dSeq(fnDoc, dText(" "), argDoc))

	case ast.KindIf:
		cDoc := astToDoc(arena, ast.NodeID(n.Left))
		tDoc := astToDoc(arena, ast.NodeID(n.Right))
		if n.Data != 0 {
			eDoc := astToDoc(arena, ast.NodeID(n.Data))
			return dGroup(dSeq(
				dText("if "), cDoc, dText(" then"),
				dNest(4, dSeq(dLine(), tDoc)),
				dLine(), dText("else"),
				dNest(4, dSeq(dLine(), eDoc)),
			))
		}
		return dGroup(dSeq(dText("if "), cDoc, dText(" then"), dNest(4, dSeq(dLine(), tDoc))))

	case ast.KindTuple:
		ch := arena.ExtraSlice(n.Left, n.Right)
		var parts []*stdlib.Doc
		for i, c := range ch {
			if i > 0 {
				parts = append(parts, dText(", "))
			}
			parts = append(parts, astToDoc(arena, ast.NodeID(c)))
		}
		return dSeq(dText("("), dSeq(parts...), dText(")"))

	case ast.KindList:
		ch := arena.ExtraSlice(n.Left, n.Right)
		var parts []*stdlib.Doc
		for i, c := range ch {
			if i > 0 {
				parts = append(parts, dText("; "))
			}
			parts = append(parts, astToDoc(arena, ast.NodeID(c)))
		}
		return dSeq(dText("["), dSeq(parts...), dText("]"))

	case ast.KindRecordLit:
		fields := arena.ExtraSlice(n.Left, n.Right)
		var parts []*stdlib.Doc
		for i, fID := range fields {
			if i > 0 {
				parts = append(parts, dText("; "))
			}
			fn := arena.Get(ast.NodeID(fID))
			fName := s.String(token.StringID(fn.Data))
			parts = append(parts, dSeq(dText(fName+" = "), astToDoc(arena, ast.NodeID(fn.Left))))
		}
		return dSeq(dText("{ "), dSeq(parts...), dText(" }"))

	default:
		return dText(s.String(token.StringID(n.Data)))
	}
}
