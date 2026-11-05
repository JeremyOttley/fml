package repl

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestREPLSessionAndMetaCommands(t *testing.T) {
	var out bytes.Buffer
	sess := NewSession(&out)

	// 1. Units of measure & persistent let bindings across REPL lines
	if _, err := sess.EvalLine("[<Measure>] type kg"); err != nil {
		t.Fatalf("measure decl failed: %v", err)
	}
	if _, err := sess.EvalLine("[<Measure>] type m"); err != nil {
		t.Fatalf("measure decl failed: %v", err)
	}
	res1, err := sess.EvalLine("let mass = 12.5<kg>")
	if err != nil {
		t.Fatalf("let mass failed: %v", err)
	}
	if !strings.Contains(res1, "val mass : float<kg> = 12.5") {
		t.Fatalf("unexpected res1: %q", res1)
	}

	// 2. :type inspector preserves dimensional analysis
	tyOut, err := sess.EvalLine(":type mass * 4.0<m>")
	if err != nil {
		t.Fatalf(":type failed: %v", err)
	}
	if !strings.Contains(tyOut, "float<") {
		t.Fatalf("expected dimensional type from :type, got %q", tyOut)
	}

	// 3. :ir disassembler
	irOut, err := sess.EvalLine(":ir 10 + 32")
	if err != nil {
		t.Fatalf(":ir failed: %v", err)
	}
	if !strings.Contains(irOut, "IRPrim") || !strings.Contains(irOut, "IRConst") {
		t.Fatalf("expected IRPrim and IRConst in :ir output, got:\n%s", irOut)
	}

	// 4. :quote meta-command
	qOut, err := sess.EvalLine(":quote 1 + 2")
	if err != nil {
		t.Fatalf(":quote failed: %v", err)
	}
	if !strings.Contains(qOut, "val it : Expr = QInfix") {
		t.Fatalf("expected Expr ADT from :quote, got %q", qOut)
	}

	// 5. :fmt meta-command & FormatSource via Wadler layout engine
	fmtOut, err := sess.EvalLine(":fmt let add x y = x + y")
	if err != nil {
		t.Fatalf(":fmt failed: %v", err)
	}
	if !strings.Contains(fmtOut, "let add x y = x + y") {
		t.Fatalf("unexpected :fmt output: %q", fmtOut)
	}
}

func TestCheckFileAndInteractiveLoop(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "sample.fml")
	src := `
let double x = x * 2
let ans = double 21
ans
`
	if err := os.WriteFile(path, []byte(src), 0600); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	summary, err := CheckFile(path)
	if err != nil {
		t.Fatalf("CheckFile failed: %v", err)
	}
	if !strings.Contains(summary, "val double : (int -> int)") || !strings.Contains(summary, "val ans : int") {
		t.Fatalf("unexpected CheckFile summary:\n%s", summary)
	}

	in := strings.NewReader("let a = 20\na + 22\n:quit\n")
	var out bytes.Buffer
	if err := RunInteractive(in, &out); err != nil {
		t.Fatalf("RunInteractive failed: %v", err)
	}
	if !strings.Contains(out.String(), "val it : int = 42") {
		t.Fatalf("expected REPL output 'val it : int = 42', got:\n%s", out.String())
	}
}

func TestLSPHoverAndDiagnosticsLifecycle(t *testing.T) {
	requests := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didOpen","params":{"textDocument":{"uri":"file:///test.fml","text":"[<Measure>] type kg\nlet mass = 15.0<kg>\nmass\n"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"textDocument/hover","params":{"textDocument":{"uri":"file:///test.fml"},"position":{"line":1,"character":5}}}`,
		`{"jsonrpc":"2.0","method":"textDocument/didChange","params":{"textDocument":{"uri":"file:///test.fml"},"contentChanges":[{"text":"let bad = 10 + \"str\"\n"}]}}`,
		`{"jsonrpc":"2.0","id":3,"method":"shutdown"}`,
		`{"jsonrpc":"2.0","method":"exit"}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	if err := RunLSP(strings.NewReader(requests), &out); err != nil {
		t.Fatalf("RunLSP failed: %v", err)
	}

	resp := out.String()
	if !strings.Contains(resp, `"hoverProvider":true`) {
		t.Fatalf("expected hoverProvider capability in initialize response, got:\n%s", resp)
	}
	if !strings.Contains(resp, "val mass : float<kg>") {
		t.Fatalf("expected hover response with 'val mass : float<kg>', got:\n%s", resp)
	}
	if !strings.Contains(resp, `"method":"textDocument/publishDiagnostics"`) || !strings.Contains(resp, "type mismatch") {
		t.Fatalf("expected publishDiagnostics notification with type mismatch, got:\n%s", resp)
	}
}

func TestLSPDefinitionAndDocumentSymbols(t *testing.T) {
	doc := "type Shape = | Circle of float | Point\nlet computeArea x = x * 2\nlet res = computeArea 21\n"
	requests := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		fmt.Sprintf(`{"jsonrpc":"2.0","method":"textDocument/didOpen","params":{"textDocument":{"uri":"file:///nav.fml","text":%q}}}`, doc),
		`{"jsonrpc":"2.0","id":2,"method":"textDocument/definition","params":{"textDocument":{"uri":"file:///nav.fml"},"position":{"line":2,"character":12}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"textDocument/documentSymbol","params":{"textDocument":{"uri":"file:///nav.fml"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"shutdown"}`,
		`{"jsonrpc":"2.0","method":"exit"}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	if err := RunLSP(strings.NewReader(requests), &out); err != nil {
		t.Fatalf("RunLSP failed: %v", err)
	}

	resp := out.String()
	if !strings.Contains(resp, `"definitionProvider":true`) || !strings.Contains(resp, `"documentSymbolProvider":true`) {
		t.Fatalf("expected definitionProvider and documentSymbolProvider in capabilities, got:\n%s", resp)
	}
	// computeArea is declared on line 1 (0-indexed), starting at character 4 ("let computeArea")
	if !strings.Contains(resp, `"uri":"file:///nav.fml","range":{"start":{"line":1,"character":4}`) {
		t.Fatalf("expected go-to-definition location pointing to line 1 character 4, got:\n%s", resp)
	}
	if !strings.Contains(resp, `"name":"Shape"`) || !strings.Contains(resp, `"name":"computeArea"`) || !strings.Contains(resp, `"name":"res"`) {
		t.Fatalf("expected documentSymbol response containing Shape, computeArea, and res, got:\n%s", resp)
	}
}

func TestLSPCompletionAndReferences(t *testing.T) {
	doc := "let factor = 10\nlet a = factor * 2\nlet b = factor + 5\nlet root = Math.sq\n"
	requests := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		fmt.Sprintf(`{"jsonrpc":"2.0","method":"textDocument/didOpen","params":{"textDocument":{"uri":"file:///comp.fml","text":%q}}}`, doc),
		`{"jsonrpc":"2.0","id":2,"method":"textDocument/completion","params":{"textDocument":{"uri":"file:///comp.fml"},"position":{"line":3,"character":18}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"textDocument/references","params":{"textDocument":{"uri":"file:///comp.fml"},"position":{"line":1,"character":9}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"shutdown"}`,
		`{"jsonrpc":"2.0","method":"exit"}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	if err := RunLSP(strings.NewReader(requests), &out); err != nil {
		t.Fatalf("RunLSP failed: %v", err)
	}

	resp := out.String()
	if !strings.Contains(resp, `"referencesProvider":true`) || !strings.Contains(resp, `"completionProvider"`) {
		t.Fatalf("expected referencesProvider and completionProvider in capabilities, got:\n%s", resp)
	}
	if !strings.Contains(resp, `"label":"sqrt"`) || !strings.Contains(resp, `(float -> float)`) {
		t.Fatalf("expected Math.sq dot-completion to suggest sqrt : (float -> float), got:\n%s", resp)
	}
	// factor appears on lines 0, 1, and 2
	if !strings.Contains(resp, `"start":{"line":0,"character":4}`) ||
		!strings.Contains(resp, `"start":{"line":1,"character":8}`) ||
		!strings.Contains(resp, `"start":{"line":2,"character":8}`) {
		t.Fatalf("expected references response to include all 3 occurrences of factor, got:\n%s", resp)
	}
}

func TestLSPRenameAndFormatting(t *testing.T) {
	doc := "let   oldVal   =   10 + 20\nlet res = oldVal * 2\n"
	requests := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		fmt.Sprintf(`{"jsonrpc":"2.0","method":"textDocument/didOpen","params":{"textDocument":{"uri":"file:///refactor.fml","text":%q}}}`, doc),
		`{"jsonrpc":"2.0","id":2,"method":"textDocument/rename","params":{"textDocument":{"uri":"file:///refactor.fml"},"position":{"line":1,"character":11},"newName":"newVal"}}`,
		`{"jsonrpc":"2.0","id":3,"method":"textDocument/formatting","params":{"textDocument":{"uri":"file:///refactor.fml"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"shutdown"}`,
		`{"jsonrpc":"2.0","method":"exit"}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	if err := RunLSP(strings.NewReader(requests), &out); err != nil {
		t.Fatalf("RunLSP failed: %v", err)
	}

	resp := out.String()
	if !strings.Contains(resp, `"renameProvider":true`) || !strings.Contains(resp, `"documentFormattingProvider":true`) {
		t.Fatalf("expected renameProvider and documentFormattingProvider in capabilities, got:\n%s", resp)
	}
	// Verify rename produces WorkspaceEdit replacing both occurrences of oldVal with newVal
	if !strings.Contains(resp, `"newText":"newVal"`) ||
		!strings.Contains(resp, `"start":{"line":0,"character":6}`) ||
		!strings.Contains(resp, `"start":{"line":1,"character":10}`) {
		t.Fatalf("expected rename WorkspaceEdit for both occurrences of oldVal, got:\n%s", resp)
	}
	// Verify formatting normalizes extra spaces via Wadler layout engine
	if !strings.Contains(resp, `let oldVal = 10 + 20\nlet res = oldVal * 2\n`) {
		t.Fatalf("expected formatted document text in textDocument/formatting response, got:\n%s", resp)
	}
}

func TestLSPSignatureHelpAndExhaustivenessCodeAction(t *testing.T) {
	doc := strings.Join([]string{
		"type Shape = Circle of int | Rect of int * int | Point",
		"let add3 (x: int) (y: int) (z: int) = x + y + z",
		"let call = add3 10 20 ",
		"let area s =",
		"    match s with",
		"    | Circle r -> r * r",
	}, "\n") + "\n"

	requests := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		fmt.Sprintf(`{"jsonrpc":"2.0","method":"textDocument/didOpen","params":{"textDocument":{"uri":"file:///actions.fml","text":%q}}}`, doc),
		`{"jsonrpc":"2.0","id":2,"method":"textDocument/signatureHelp","params":{"textDocument":{"uri":"file:///actions.fml"},"position":{"line":2,"character":22}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"textDocument/codeAction","params":{"textDocument":{"uri":"file:///actions.fml"},"range":{"start":{"line":4,"character":4},"end":{"line":5,"character":20}}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"shutdown"}`,
		`{"jsonrpc":"2.0","method":"exit"}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	if err := RunLSP(strings.NewReader(requests), &out); err != nil {
		t.Fatalf("RunLSP failed: %v", err)
	}

	resp := out.String()
	if !strings.Contains(resp, `"signatureHelpProvider"`) || !strings.Contains(resp, `"codeActionProvider":true`) {
		t.Fatalf("expected signatureHelpProvider and codeActionProvider in capabilities, got:\n%s", resp)
	}
	// On line 2 ("let call = add3 10 20 "), 2 arguments are already provided so activeParameter is 2 (the 3rd parameter)
	if !strings.Contains(resp, `"label":"add3 : (int -> (int -> (int -> int)))"`) || !strings.Contains(resp, `"activeParameter":2`) {
		t.Fatalf("expected signatureHelp for add3 with activeParameter 2, got:\n%s", resp)
	}
	// Exhaustiveness quick-fix should synthesize missing Point and Rect _ arms
	if !strings.Contains(resp, `"title":"Add missing match case(s): Point, Rect _"`) ||
		!strings.Contains(resp, `| Point -> 0\n    | Rect _ -> 0\n`) {
		t.Fatalf("expected quickfix codeAction adding Point and Rect _ match cases, got:\n%s", resp)
	}
}

func TestLSPInlayHints(t *testing.T) {
	doc := strings.Join([]string{
		"[<Measure>] type m",
		"[<Measure>] type s",
		"let dist = 100.0<m>",
		"let time = 10.0<s>",
		"let velocity = dist / time",
		"let scale factor = velocity * factor",
		"let mapped = List.map (fun x -> x + 1) [1; 2; 3]",
	}, "\n") + "\n"

	requests := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		fmt.Sprintf(`{"jsonrpc":"2.0","method":"textDocument/didOpen","params":{"textDocument":{"uri":"file:///hints.fml","text":%q}}}`, doc),
		`{"jsonrpc":"2.0","id":2,"method":"textDocument/inlayHint","params":{"textDocument":{"uri":"file:///hints.fml"},"range":{"start":{"line":0,"character":0},"end":{"line":10,"character":0}}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"shutdown"}`,
		`{"jsonrpc":"2.0","method":"exit"}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	if err := RunLSP(strings.NewReader(requests), &out); err != nil {
		t.Fatalf("RunLSP failed: %v", err)
	}

	resp := out.String()
	expected := []string{
		`"inlayHintProvider":true`,
		`"label":": float<m>"`,
		`"label":": float<s>"`,
		`"label":": float<m*s^-1>"`,
		`"label":": float"`,
		`"label":": int list"`,
		`"label":": int"`,
	}
	for _, exp := range expected {
		if !strings.Contains(resp, exp) {
			t.Fatalf("expected %s in inlayHint response, got:\n%s", exp, resp)
		}
	}
}

func TestLSPSemanticTokensAndWorkspaceSymbols(t *testing.T) {
	doc1 := strings.Join([]string{
		"type Status = Ready | Busy of int",
		"let computeScore x = x + 42",
	}, "\n") + "\n"
	doc2 := "let globalMetric = 100\n"

	requests := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		fmt.Sprintf(`{"jsonrpc":"2.0","method":"textDocument/didOpen","params":{"textDocument":{"uri":"file:///mod1.fml","text":%q}}}`, doc1),
		fmt.Sprintf(`{"jsonrpc":"2.0","method":"textDocument/didOpen","params":{"textDocument":{"uri":"file:///mod2.fml","text":%q}}}`, doc2),
		`{"jsonrpc":"2.0","id":2,"method":"workspace/symbol","params":{"query":"score"}}`,
		`{"jsonrpc":"2.0","id":3,"method":"textDocument/semanticTokens/full","params":{"textDocument":{"uri":"file:///mod1.fml"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"shutdown"}`,
		`{"jsonrpc":"2.0","method":"exit"}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	if err := RunLSP(strings.NewReader(requests), &out); err != nil {
		t.Fatalf("RunLSP failed: %v", err)
	}

	resp := out.String()
	expected := []string{
		`"workspaceSymbolProvider":true`,
		`"semanticTokensProvider"`,
		`"name":"computeScore"`,
		`"uri":"file:///mod1.fml"`,
		`"data":[0,0,4,0,0,0,5,6,1,0`,
	}
	for _, exp := range expected {
		if !strings.Contains(resp, exp) {
			t.Fatalf("expected %s in workspace/symbol or semanticTokens response, got:\n%s", exp, resp)
		}
	}
}

func TestFormatFullSyntaxRoundTrip(t *testing.T) {
	src := strings.Join([]string{
		"[<Measure>] type m",
		"[<Measure>] type s",
		"type Point = { X: int; Y: int }",
		"type Shape = Circle of int | Box of int * int | Origin",
		"let speed = 20.0<m> / 4.0<s>",
		"let area s =",
		"    match s with",
		"    | Circle r when r > 0 -> r * r",
		"    | Box (w, h) -> w * h",
		"    | _ -> 0",
		"let optVal =",
		"    option {",
		"        let! a = Some 10",
		"        let! b = Some 20",
		"        return a + b",
		"    }",
		"let arr = [| 1; 2; 3 |]",
		"let quoted = <@ 1 + 2 @>",
		"let total =",
		"    let mutable acc = 0",
		"    for i = 1 to 3 do",
		"        acc <- acc + i",
		"    acc + area (Box (3, 4)) + Array.length arr",
		"total",
	}, "\n") + "\n"

	formatted1, err := FormatSource([]byte(src), 80)
	if err != nil {
		t.Fatalf("FormatSource first pass failed: %v", err)
	}
	formatted2, err := FormatSource([]byte(formatted1), 80)
	if err != nil {
		t.Fatalf("FormatSource second pass failed: %v\nformatted1:\n%s", err, formatted1)
	}
	if formatted1 != formatted2 {
		t.Fatalf("FormatSource is not idempotent:\n--- pass 1 ---\n%s\n--- pass 2 ---\n%s", formatted1, formatted2)
	}

	var outOrig, outFmt bytes.Buffer
	sessOrig := NewSession(&outOrig)
	resOrig, err := sessOrig.EvalLine(src)
	if err != nil {
		t.Fatalf("EvalLine on original failed: %v", err)
	}
	sessFmt := NewSession(&outFmt)
	resFmt, err := sessFmt.EvalLine(formatted1)
	if err != nil {
		t.Fatalf("EvalLine on formatted failed: %v\nformatted:\n%s", err, formatted1)
	}
	if resOrig != resFmt || !strings.Contains(resFmt, "val it : int = 21") {
		t.Fatalf("expected identical evaluation with 'val it : int = 21', got orig:\n%s\nfmt:\n%s\nformatted:\n%s", resOrig, resFmt, formatted1)
	}
}

func TestLSPCodeLensAndFoldingRanges(t *testing.T) {
	doc := strings.Join([]string{
		"let classify n =",
		"    match n with",
		"    | 0 -> \"zero\"",
		"    | _ -> \"non-zero\"",
		"let test_classify () = classify 0",
		"let bench_classify () = classify 42",
	}, "\n") + "\n"

	requests := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		fmt.Sprintf(`{"jsonrpc":"2.0","method":"textDocument/didOpen","params":{"textDocument":{"uri":"file:///lens.fml","text":%q}}}`, doc),
		`{"jsonrpc":"2.0","id":2,"method":"textDocument/codeLens","params":{"textDocument":{"uri":"file:///lens.fml"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"textDocument/foldingRange","params":{"textDocument":{"uri":"file:///lens.fml"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"shutdown"}`,
		`{"jsonrpc":"2.0","method":"exit"}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	if err := RunLSP(strings.NewReader(requests), &out); err != nil {
		t.Fatalf("RunLSP failed: %v", err)
	}

	resp := out.String()
	expected := []string{
		`"foldingRangeProvider":true`,
		`"codeLensProvider"`,
		`"title":"val classify : (int -> string)"`,
		`"title":"Run test"`,
		`"command":"fml.runTest"`,
		`"title":"Run benchmark"`,
		`"command":"fml.runBench"`,
		`"startLine":0,"endLine":3,"kind":"region"`,
		`"startLine":1,"endLine":3,"kind":"region"`,
	}
	for _, exp := range expected {
		if !strings.Contains(resp, exp) {
			t.Fatalf("expected %s in codeLens or foldingRange response, got:\n%s", exp, resp)
		}
	}
}


