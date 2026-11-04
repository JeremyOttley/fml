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

func isMultilineNode(arena *ast.Arena, id ast.NodeID) bool {
	if id == ast.NoNode {
		return false
	}
	switch arena.Get(id).Kind {
	case ast.KindBlock, ast.KindMatch, ast.KindCompExpr, ast.KindSeq,
		ast.KindWhile, ast.KindForIn, ast.KindForTo, ast.KindModule:
		return true
	default:
		return false
	}
}

func formatLambdaParam(arena *ast.Arena, rn ast.Node) (string, ast.NodeID) {
	s := arena.Strings
	paramName := s.String(token.StringID(rn.Data))
	nextRHS := ast.NodeID(rn.Right)
	if rn.Left != 0 {
		pn := arena.Get(ast.NodeID(rn.Left))
		switch pn.Kind {
		case ast.KindUnit:
			return "()", nextRHS
		case ast.KindTypeAnnot:
			pName := s.String(token.StringID(pn.Data))
			tyStr := stdlib.Pretty(80, astToDoc(arena, ast.NodeID(pn.Right)))
			return "(" + pName + ": " + tyStr + ")", nextRHS
		}
	}
	if strings.HasPrefix(paramName, "__tup_arg_") && nextRHS != ast.NoNode {
		bn := arena.Get(nextRHS)
		if bn.Kind == ast.KindLet && bn.Op == 1 {
			extra := arena.ExtraSlice(bn.Left, 3)
			patStr := stdlib.Pretty(80, astToDoc(arena, ast.NodeID(extra[0])))
			return patStr, ast.NodeID(extra[2])
		}
	}
	return paramName, nextRHS
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
		str := fmt.Sprintf("%g", math.Float64frombits(n.Data))
		if !strings.ContainsAny(str, ".eE") {
			str += ".0"
		}
		return dText(str)
	case ast.KindString:
		return dText(fmt.Sprintf("%q", s.String(token.StringID(n.Data))))
	case ast.KindIdent, ast.KindTypeIdent:
		return dText(s.String(token.StringID(n.Data)))

	case ast.KindBlock:
		ch := arena.ExtraSlice(n.Left, n.Right)
		var parts []*stdlib.Doc
		for i, c := range ch {
			if i > 0 {
				parts = append(parts, dLine())
			}
			parts = append(parts, astToDoc(arena, ast.NodeID(c)))
		}
		return dSeq(parts...)

	case ast.KindLet, ast.KindLetMutable, ast.KindLetRec, ast.KindUse:
		kw := "let "
		if n.Kind == ast.KindLetMutable {
			kw = "let mutable "
		} else if n.Kind == ast.KindLetRec {
			kw = "let rec "
		} else if n.Kind == ast.KindUse || n.Flags&ast.FlagUse != 0 {
			kw = "let use "
		}
		if n.Op == 1 {
			extra := arena.ExtraSlice(n.Left, 3)
			patDoc := astToDoc(arena, ast.NodeID(extra[0]))
			rhsID := ast.NodeID(extra[1])
			inBodyID := ast.NodeID(extra[2])
			rhsDoc := astToDoc(arena, rhsID)
			letDoc := dSeq(dText(kw), patDoc, dText(" ="), dNest(4, dSeq(dLine(), rhsDoc)))
			if !isMultilineNode(arena, rhsID) {
				letDoc = dGroup(letDoc)
			}
			if inBodyID != ast.NoNode {
				inDoc := astToDoc(arena, inBodyID)
				return dGroup(dSeq(letDoc, dText(" in"), dLine(), inDoc))
			}
			return letDoc
		}
		name := s.String(token.StringID(n.Data))
		var params []string
		rhsID := ast.NodeID(n.Left)
		for rhsID != ast.NoNode {
			rn := *arena.Get(rhsID)
			if rn.Kind == ast.KindLambda {
				pStr, nextRHS := formatLambdaParam(arena, rn)
				params = append(params, pStr)
				rhsID = nextRHS
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
		letDoc := dSeq(dText(header), dNest(4, dSeq(dLine(), rhsDoc)))
		if !isMultilineNode(arena, rhsID) {
			letDoc = dGroup(letDoc)
		}
		if n.Right != 0 {
			inNode := arena.Get(ast.NodeID(n.Right))
			inDoc := astToDoc(arena, ast.NodeID(n.Right))
			if n.Kind == ast.KindLetRec && inNode.Kind == ast.KindLetRec {
				andStr := stdlib.Pretty(80, inDoc)
				andStr = strings.TrimPrefix(andStr, "let rec ")
				return dSeq(letDoc, dLine(), dText("and "+andStr))
			}
			return dGroup(dSeq(letDoc, dText(" in"), dLine(), inDoc))
		}
		return letDoc

	case ast.KindLambda:
		pStr, nextRHS := formatLambdaParam(arena, n)
		body := astToDoc(arena, nextRHS)
		lamDoc := dSeq(dText("fun "+pStr+" ->"), dNest(4, dSeq(dLine(), body)))
		if !isMultilineNode(arena, nextRHS) {
			return dGroup(lamDoc)
		}
		return lamDoc

	case ast.KindTypeAnnot:
		return dSeq(dText("("), astToDoc(arena, ast.NodeID(n.Left)), dText(": "), astToDoc(arena, ast.NodeID(n.Right)), dText(")"))

	case ast.KindInfix:
		lDoc := astToDoc(arena, ast.NodeID(n.Left))
		rDoc := astToDoc(arena, ast.NodeID(n.Right))
		opStr := token.TokenKind(n.Op).String()
		return dGroup(dSeq(lDoc, dText(" "+opStr), dNest(4, dSeq(dLine(), rDoc))))

	case ast.KindPrefix:
		inner := astToDoc(arena, ast.NodeID(n.Left))
		if token.TokenKind(n.Op) == token.TokenNot {
			return dSeq(dText("not "), inner)
		}
		return dSeq(dText("-"), inner)

	case ast.KindPipeRight:
		lDoc := astToDoc(arena, ast.NodeID(n.Left))
		rDoc := astToDoc(arena, ast.NodeID(n.Right))
		return dGroup(dSeq(lDoc, dNest(4, dSeq(dLine(), dText("|> "), rDoc))))

	case ast.KindPipeLeft:
		lDoc := astToDoc(arena, ast.NodeID(n.Left))
		rDoc := astToDoc(arena, ast.NodeID(n.Right))
		return dGroup(dSeq(lDoc, dText(" <| "), rDoc))

	case ast.KindCompose:
		lDoc := astToDoc(arena, ast.NodeID(n.Left))
		rDoc := astToDoc(arena, ast.NodeID(n.Right))
		op := " >> "
		if n.Flags&ast.FlagBackward != 0 {
			op = " << "
		}
		return dGroup(dSeq(lDoc, dText(op), rDoc))

	case ast.KindRange:
		return dSeq(astToDoc(arena, ast.NodeID(n.Left)), dText(" .. "), astToDoc(arena, ast.NodeID(n.Right)))

	case ast.KindMutate:
		return dGroup(dSeq(astToDoc(arena, ast.NodeID(n.Left)), dText(" <- "), astToDoc(arena, ast.NodeID(n.Right))))

	case ast.KindApply:
		fnDoc := astToDoc(arena, ast.NodeID(n.Left))
		argNode := arena.Get(ast.NodeID(n.Right))
		argDoc := astToDoc(arena, ast.NodeID(n.Right))
		if argNode.Kind == ast.KindApply || argNode.Kind == ast.KindInfix || argNode.Kind == ast.KindOption || argNode.Kind == ast.KindResult {
			if argNode.Kind != ast.KindOption || argNode.Left != 0 {
				argDoc = dSeq(dText("("), argDoc, dText(")"))
			}
		}
		return dGroup(dSeq(fnDoc, dText(" "), argDoc))

	case ast.KindOption:
		if n.Data == 0 {
			return dText("None")
		}
		argID := ast.NodeID(n.Left)
		argNode := arena.Get(argID)
		argDoc := astToDoc(arena, argID)
		if argNode.Kind == ast.KindApply || argNode.Kind == ast.KindInfix || argNode.Kind == ast.KindOption || argNode.Kind == ast.KindResult {
			argDoc = dSeq(dText("("), argDoc, dText(")"))
		}
		return dSeq(dText("Some "), argDoc)

	case ast.KindResult:
		ctor := "Error "
		if n.Data != 0 {
			ctor = "Ok "
		}
		argID := ast.NodeID(n.Left)
		argNode := arena.Get(argID)
		argDoc := astToDoc(arena, argID)
		if argNode.Kind == ast.KindApply || argNode.Kind == ast.KindInfix || argNode.Kind == ast.KindOption || argNode.Kind == ast.KindResult {
			argDoc = dSeq(dText("("), argDoc, dText(")"))
		}
		return dSeq(dText(ctor), argDoc)

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

	case ast.KindMatch:
		scrutDoc := astToDoc(arena, ast.NodeID(n.Left))
		armsBlock := arena.Get(ast.NodeID(n.Right))
		arms := arena.ExtraSlice(armsBlock.Left, armsBlock.Right)
		var armDocs []*stdlib.Doc
		for _, armID := range arms {
			armDocs = append(armDocs, dLine(), astToDoc(arena, ast.NodeID(armID)))
		}
		return dSeq(append([]*stdlib.Doc{dText("match "), scrutDoc, dText(" with")}, armDocs...)...)

	case ast.KindMatchArm:
		patDoc := astToDoc(arena, ast.NodeID(n.Left))
		var guardDoc *stdlib.Doc
		if n.Data != 0 {
			gn := arena.Get(ast.NodeID(n.Data))
			guardDoc = dSeq(dText(" when "), astToDoc(arena, ast.NodeID(gn.Left)))
		}
		bodyID := ast.NodeID(n.Right)
		bodyDoc := astToDoc(arena, bodyID)
		armDoc := dSeq(dText("| "), patDoc, guardDoc, dText(" ->"), dNest(4, dSeq(dLine(), bodyDoc)))
		if !isMultilineNode(arena, bodyID) {
			return dGroup(armDoc)
		}
		return armDoc

	case ast.KindPatWildcard:
		return dText("_")
	case ast.KindPatVar:
		return dText(s.String(token.StringID(n.Data)))
	case ast.KindPatConst:
		if n.Left != 0 {
			return astToDoc(arena, ast.NodeID(n.Left))
		}
		return dText("()")
	case ast.KindPatTuple:
		ch := arena.ExtraSlice(n.Left, n.Right)
		var parts []*stdlib.Doc
		for i, c := range ch {
			if i > 0 {
				parts = append(parts, dText(", "))
			}
			parts = append(parts, astToDoc(arena, ast.NodeID(c)))
		}
		return dSeq(dText("("), dSeq(parts...), dText(")"))
	case ast.KindPatList:
		ch := arena.ExtraSlice(n.Left, n.Right)
		var parts []*stdlib.Doc
		for i, c := range ch {
			if i > 0 {
				parts = append(parts, dText("; "))
			}
			parts = append(parts, astToDoc(arena, ast.NodeID(c)))
		}
		return dSeq(dText("["), dSeq(parts...), dText("]"))
	case ast.KindPatCons:
		return dSeq(astToDoc(arena, ast.NodeID(n.Left)), dText(" :: "), astToDoc(arena, ast.NodeID(n.Right)))
	case ast.KindPatRecord:
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
	case ast.KindPatDU:
		ctor := s.String(token.StringID(n.Data))
		if n.Left == 0 {
			return dText(ctor)
		}
		subID := ast.NodeID(n.Left)
		subNode := arena.Get(subID)
		subDoc := astToDoc(arena, subID)
		if subNode.Kind == ast.KindPatDU && subNode.Left != 0 {
			subDoc = dSeq(dText("("), subDoc, dText(")"))
		}
		return dSeq(dText(ctor+" "), subDoc)

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

	case ast.KindArray:
		ch := arena.ExtraSlice(n.Left, n.Right)
		var parts []*stdlib.Doc
		for i, c := range ch {
			if i > 0 {
				parts = append(parts, dText("; "))
			}
			parts = append(parts, astToDoc(arena, ast.NodeID(c)))
		}
		if len(parts) == 0 {
			return dText("[||]")
		}
		return dSeq(dText("[| "), dSeq(parts...), dText(" |]"))

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

	case ast.KindFieldAccess:
		recvDoc := astToDoc(arena, ast.NodeID(n.Left))
		return dSeq(recvDoc, dText("."+s.String(token.StringID(n.Data))))

	case ast.KindMeasure:
		if n.Left == 0 {
			return dText("[<Measure>] type " + s.String(token.StringID(n.Data)))
		}
		return dSeq(astToDoc(arena, ast.NodeID(n.Left)), dText("<"), astToDoc(arena, ast.NodeID(n.Right)), dText(">"))

	case ast.KindMeasureUnit:
		if n.Op == 0 {
			return dText(s.String(token.StringID(n.Data)))
		}
		opStr := token.TokenKind(n.Op).String()
		return dSeq(astToDoc(arena, ast.NodeID(n.Left)), dText(opStr), astToDoc(arena, ast.NodeID(n.Right)))

	case ast.KindRecordType:
		name := s.String(token.StringID(n.Data))
		fields := arena.ExtraSlice(n.Left, n.Right)
		var parts []*stdlib.Doc
		for i, fID := range fields {
			if i > 0 {
				parts = append(parts, dText("; "))
			}
			fn := arena.Get(ast.NodeID(fID))
			fName := s.String(token.StringID(fn.Data))
			parts = append(parts, dSeq(dText(fName+": "), astToDoc(arena, ast.NodeID(fn.Left))))
		}
		return dSeq(dText("type "+name+" = { "), dSeq(parts...), dText(" }"))

	case ast.KindDUType:
		name := s.String(token.StringID(n.Data))
		cases := arena.ExtraSlice(n.Left, n.Right)
		var parts []*stdlib.Doc
		for i, cID := range cases {
			if i > 0 {
				parts = append(parts, dText(" | "))
			}
			cn := arena.Get(ast.NodeID(cID))
			cName := s.String(token.StringID(cn.Data))
			if cn.Left != 0 {
				parts = append(parts, dSeq(dText(cName+" of "), astToDoc(arena, ast.NodeID(cn.Left))))
			} else {
				parts = append(parts, dText(cName))
			}
		}
		return dGroup(dSeq(dText("type "+name+" = "), dSeq(parts...)))

	case ast.KindTypeTuple:
		ch := arena.ExtraSlice(n.Left, n.Right)
		var parts []*stdlib.Doc
		for i, c := range ch {
			if i > 0 {
				parts = append(parts, dText(" * "))
			}
			parts = append(parts, astToDoc(arena, ast.NodeID(c)))
		}
		return dSeq(parts...)

	case ast.KindTypeArrow:
		return dSeq(astToDoc(arena, ast.NodeID(n.Left)), dText(" -> "), astToDoc(arena, ast.NodeID(n.Right)))

	case ast.KindTypeApp:
		return dSeq(astToDoc(arena, ast.NodeID(n.Left)), dText("<"), astToDoc(arena, ast.NodeID(n.Right)), dText(">"))

	case ast.KindTypeProvider:
		name := s.String(token.StringID(n.Data))
		return dSeq(dText("type "+name+" = "), astToDoc(arena, ast.NodeID(n.Left)), dText("<"), astToDoc(arena, ast.NodeID(n.Right)), dText(">"))

	case ast.KindWhile:
		condDoc := astToDoc(arena, ast.NodeID(n.Left))
		bodyDoc := astToDoc(arena, ast.NodeID(n.Right))
		return dSeq(dText("while "), condDoc, dText(" do"), dNest(4, dSeq(dLine(), bodyDoc)))

	case ast.KindForIn:
		varName := s.String(token.StringID(n.Data))
		collDoc := astToDoc(arena, ast.NodeID(n.Left))
		bodyDoc := astToDoc(arena, ast.NodeID(n.Right))
		return dSeq(dText("for "+varName+" in "), collDoc, dText(" do"), dNest(4, dSeq(dLine(), bodyDoc)))

	case ast.KindForTo:
		varName := s.String(token.StringID(n.Data))
		rn := arena.Get(ast.NodeID(n.Left))
		startDoc := astToDoc(arena, ast.NodeID(rn.Left))
		endDoc := astToDoc(arena, ast.NodeID(rn.Right))
		dir := " to "
		if n.Flags&ast.FlagBackward != 0 {
			dir = " downto "
		}
		bodyDoc := astToDoc(arena, ast.NodeID(n.Right))
		return dSeq(dText("for "+varName+" = "), startDoc, dText(dir), endDoc, dText(" do"), dNest(4, dSeq(dLine(), bodyDoc)))

	case ast.KindCompExpr, ast.KindSeq:
		bName := s.String(token.StringID(n.Data))
		bodyDoc := astToDoc(arena, ast.NodeID(n.Left))
		return dSeq(dText(bName+" {"), dNest(4, dSeq(dLine(), bodyDoc)), dLine(), dText("}"))

	case ast.KindBindBang:
		kw := "let! "
		if token.TokenKind(n.Op) == token.TokenDoBang {
			return dSeq(dText("do! "), astToDoc(arena, ast.NodeID(n.Left)))
		} else if token.TokenKind(n.Op) == token.TokenUseBang {
			kw = "use! "
		}
		name := s.String(token.StringID(n.Data))
		return dSeq(dText(kw+name+" = "), astToDoc(arena, ast.NodeID(n.Left)))

	case ast.KindReturn:
		kw := "return "
		if token.TokenKind(n.Op) == token.TokenReturnBang {
			kw = "return! "
		} else if token.TokenKind(n.Op) == token.TokenYield {
			kw = "yield "
		}
		return dSeq(dText(kw), astToDoc(arena, ast.NodeID(n.Left)))

	case ast.KindOpen:
		return dText("open " + s.String(token.StringID(n.Data)))

	case ast.KindModule:
		name := s.String(token.StringID(n.Data))
		bodyDoc := astToDoc(arena, ast.NodeID(n.Left))
		return dSeq(dText("module "+name+" ="), dNest(4, dSeq(dLine(), bodyDoc)))

	case ast.KindQuote:
		return dSeq(dText("<@ "), astToDoc(arena, ast.NodeID(n.Left)), dText(" @>"))

	case ast.KindActivePattern:
		tags := s.String(token.StringID(n.Data))
		paramName := s.String(token.StringID(arena.Get(ast.NodeID(n.Left)).Data))
		bodyID := ast.NodeID(n.Right)
		bodyDoc := astToDoc(arena, bodyID)
		apDoc := dSeq(dText("let ("+tags+") "+paramName+" ="), dNest(4, dSeq(dLine(), bodyDoc)))
		if !isMultilineNode(arena, bodyID) {
			return dGroup(apDoc)
		}
		return apDoc

	default:
		return dText(s.String(token.StringID(n.Data)))
	}
}
