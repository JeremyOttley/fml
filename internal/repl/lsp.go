package repl

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"fml/internal/ast"
	"fml/internal/parser"
	"fml/internal/token"
	"fml/internal/types"
)

// LSPServer implements a zero-dependency Language Server Protocol (JSON-RPC 2.0)
// endpoint for FML supporting real-time diagnostics (`textDocument/publishDiagnostics`)
// and type hover (`textDocument/hover`).
//
// Jaime says: An LSP server shouldn't pull in fifty megabytes of framework
// dependencies. Read the Content-Length header, unmarshal the JSON-RPC envelope,
// run the flat Arena parser and Union-Find type checker on the buffer, and map
// the cursor's (Line, Col) directly against our 16-byte Token array.
type LSPServer struct {
	Docs map[string]string
	Out  io.Writer
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   any             `json:"error,omitempty"`
}

type lspPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type lspRange struct {
	Start lspPosition `json:"start"`
	End   lspPosition `json:"end"`
}

type lspDiagnostic struct {
	Range    lspRange `json:"range"`
	Severity int      `json:"severity"`
	Source   string   `json:"source"`
	Message  string   `json:"message"`
}

type lspLocation struct {
	URI   string   `json:"uri"`
	Range lspRange `json:"range"`
}

type lspDocumentSymbol struct {
	Name           string    `json:"name"`
	Detail         string    `json:"detail,omitempty"`
	Kind           int       `json:"kind"`
	Range          lspRange  `json:"range"`
	SelectionRange lspRange  `json:"selectionRange"`
}

type lspCompletionItem struct {
	Label  string `json:"label"`
	Kind   int    `json:"kind"`
	Detail string `json:"detail,omitempty"`
}

type lspTextEdit struct {
	Range   lspRange `json:"range"`
	NewText string   `json:"newText"`
}

type lspWorkspaceEdit struct {
	Changes map[string][]lspTextEdit `json:"changes"`
}

type lspParameterInformation struct {
	Label string `json:"label"`
}

type lspSignatureInformation struct {
	Label           string                    `json:"label"`
	Parameters      []lspParameterInformation `json:"parameters,omitempty"`
	ActiveParameter int                       `json:"activeParameter"`
}

type lspSignatureHelp struct {
	Signatures      []lspSignatureInformation `json:"signatures"`
	ActiveSignature int                       `json:"activeSignature"`
	ActiveParameter int                       `json:"activeParameter"`
}

type lspCodeAction struct {
	Title string            `json:"title"`
	Kind  string            `json:"kind"`
	Edit  *lspWorkspaceEdit `json:"edit,omitempty"`
}

type lspInlayHint struct {
	Position    lspPosition `json:"position"`
	Label       string      `json:"label"`
	Kind        int         `json:"kind,omitempty"`
	PaddingLeft bool        `json:"paddingLeft,omitempty"`
}

type lspWorkspaceSymbol struct {
	Name     string      `json:"name"`
	Kind     int         `json:"kind"`
	Location lspLocation `json:"location"`
}

type lspSemanticTokens struct {
	Data []int `json:"data"`
}

type lspCommand struct {
	Title     string `json:"title"`
	Command   string `json:"command"`
	Arguments []any  `json:"arguments,omitempty"`
}

type lspCodeLens struct {
	Range   lspRange    `json:"range"`
	Command *lspCommand `json:"command,omitempty"`
}

type lspFoldingRange struct {
	StartLine int    `json:"startLine"`
	EndLine   int    `json:"endLine"`
	Kind      string `json:"kind,omitempty"`
}

// NewLSPServer initializes an in-memory LSP server writing responses to out.
func NewLSPServer(out io.Writer) *LSPServer {
	return &LSPServer{
		Docs: make(map[string]string),
		Out:  out,
	}
}

// RunLSP runs the Language Server Protocol JSON-RPC loop over in and out.
func RunLSP(in io.Reader, out io.Writer) error {
	srv := NewLSPServer(out)
	reader := bufio.NewReader(in)

	for {
		payload, err := readLSPPayload(reader)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if exit := srv.HandleJSONRPC(payload); exit {
			return nil
		}
	}
}

func readLSPPayload(r *bufio.Reader) ([]byte, error) {
	contentLen := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if err == io.EOF && len(strings.TrimSpace(line)) > 0 {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "{") {
					return []byte(trimmed), nil
				}
			}
			return nil, err
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			if contentLen >= 0 {
				break
			}
			continue
		}
		// Support raw line-delimited JSON-RPC in addition to Content-Length headers
		if contentLen < 0 && strings.HasPrefix(trimmed, "{") {
			return []byte(trimmed), nil
		}
		if strings.HasPrefix(strings.ToLower(trimmed), "content-length:") {
			valStr := strings.TrimSpace(trimmed[len("content-length:"):])
			if n, err := strconv.Atoi(valStr); err == nil {
				contentLen = n
			}
		}
	}
	buf := make([]byte, contentLen)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// HandleJSONRPC processes a single JSON-RPC 2.0 request/notification payload
// and returns true if the server received an `exit` notification.
func (s *LSPServer) HandleJSONRPC(payload []byte) bool {
	var msg rpcMessage
	if err := json.Unmarshal(payload, &msg); err != nil {
		return false
	}

	switch msg.Method {
	case "initialize":
		s.sendResponse(msg.ID, map[string]any{
			"capabilities": map[string]any{
				"textDocumentSync":           1,
				"hoverProvider":              true,
				"definitionProvider":         true,
				"documentSymbolProvider":     true,
				"referencesProvider":         true,
				"renameProvider":             true,
				"documentFormattingProvider": true,
				"codeActionProvider":         true,
				"inlayHintProvider":          true,
				"workspaceSymbolProvider":    true,
				"foldingRangeProvider":       true,
				"codeLensProvider": map[string]any{
					"resolveProvider": false,
				},
				"semanticTokensProvider": map[string]any{
					"legend": map[string]any{
						"tokenTypes": []string{
							"keyword",
							"type",
							"enumMember",
							"function",
							"parameter",
							"variable",
							"number",
							"string",
							"operator",
						},
						"tokenModifiers": []string{
							"declaration",
							"readonly",
						},
					},
					"full": true,
				},
				"signatureHelpProvider": map[string]any{
					"triggerCharacters": []string{" ", "("},
				},
				"completionProvider": map[string]any{
					"triggerCharacters": []string{"."},
				},
			},
			"serverInfo": map[string]any{
				"name":    "fml-lsp",
				"version": "1.0.0",
			},
		})

	case "initialized":
		// No response required for notification

	case "textDocument/didOpen":
		var params struct {
			TextDocument struct {
				URI  string `json:"uri"`
				Text string `json:"text"`
			} `json:"textDocument"`
		}
		if json.Unmarshal(msg.Params, &params) == nil {
			s.Docs[params.TextDocument.URI] = params.TextDocument.Text
			s.analyzeAndPublishDiagnostics(params.TextDocument.URI, params.TextDocument.Text)
		}

	case "textDocument/didChange":
		var params struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
			ContentChanges []struct {
				Text string `json:"text"`
			} `json:"contentChanges"`
		}
		if json.Unmarshal(msg.Params, &params) == nil && len(params.ContentChanges) > 0 {
			latest := params.ContentChanges[len(params.ContentChanges)-1].Text
			s.Docs[params.TextDocument.URI] = latest
			s.analyzeAndPublishDiagnostics(params.TextDocument.URI, latest)
		}

	case "textDocument/hover":
		var params struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
			Position lspPosition `json:"position"`
		}
		if json.Unmarshal(msg.Params, &params) == nil {
			hoverMD := s.computeHover(params.TextDocument.URI, params.Position)
			if hoverMD == "" {
				s.sendResponse(msg.ID, nil)
			} else {
				s.sendResponse(msg.ID, map[string]any{
					"contents": map[string]any{
						"kind":  "markdown",
						"value": hoverMD,
					},
				})
			}
		}

	case "textDocument/definition":
		var params struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
			Position lspPosition `json:"position"`
		}
		if json.Unmarshal(msg.Params, &params) == nil {
			loc := s.computeDefinition(params.TextDocument.URI, params.Position)
			if loc == nil {
				s.sendResponse(msg.ID, nil)
			} else {
				s.sendResponse(msg.ID, loc)
			}
		}

	case "textDocument/documentSymbol":
		var params struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
		}
		if json.Unmarshal(msg.Params, &params) == nil {
			syms := s.computeDocumentSymbols(params.TextDocument.URI)
			s.sendResponse(msg.ID, syms)
		}

	case "textDocument/completion":
		var params struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
			Position lspPosition `json:"position"`
		}
		if json.Unmarshal(msg.Params, &params) == nil {
			items := s.computeCompletion(params.TextDocument.URI, params.Position)
			s.sendResponse(msg.ID, items)
		}

	case "textDocument/references":
		var params struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
			Position lspPosition `json:"position"`
		}
		if json.Unmarshal(msg.Params, &params) == nil {
			locs := s.computeReferences(params.TextDocument.URI, params.Position)
			s.sendResponse(msg.ID, locs)
		}

	case "textDocument/rename":
		var params struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
			Position lspPosition `json:"position"`
			NewName  string      `json:"newName"`
		}
		if json.Unmarshal(msg.Params, &params) == nil {
			edit := s.computeRename(params.TextDocument.URI, params.Position, params.NewName)
			if edit == nil {
				s.sendResponse(msg.ID, nil)
			} else {
				s.sendResponse(msg.ID, edit)
			}
		}

	case "textDocument/formatting":
		var params struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
		}
		if json.Unmarshal(msg.Params, &params) == nil {
			edits := s.computeFormatting(params.TextDocument.URI)
			s.sendResponse(msg.ID, edits)
		}

	case "textDocument/signatureHelp":
		var params struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
			Position lspPosition `json:"position"`
		}
		if json.Unmarshal(msg.Params, &params) == nil {
			sigHelp := s.computeSignatureHelp(params.TextDocument.URI, params.Position)
			if sigHelp == nil {
				s.sendResponse(msg.ID, nil)
			} else {
				s.sendResponse(msg.ID, sigHelp)
			}
		}

	case "textDocument/codeAction":
		var params struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
			Range lspRange `json:"range"`
		}
		if json.Unmarshal(msg.Params, &params) == nil {
			actions := s.computeCodeActions(params.TextDocument.URI, params.Range)
			s.sendResponse(msg.ID, actions)
		}

	case "textDocument/inlayHint":
		var params struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
			Range lspRange `json:"range"`
		}
		if json.Unmarshal(msg.Params, &params) == nil {
			hints := s.computeInlayHints(params.TextDocument.URI, params.Range)
			s.sendResponse(msg.ID, hints)
		}

	case "workspace/symbol":
		var params struct {
			Query string `json:"query"`
		}
		if json.Unmarshal(msg.Params, &params) == nil {
			syms := s.computeWorkspaceSymbols(params.Query)
			s.sendResponse(msg.ID, syms)
		}

	case "textDocument/semanticTokens/full":
		var params struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
		}
		if json.Unmarshal(msg.Params, &params) == nil {
			toks := s.computeSemanticTokens(params.TextDocument.URI)
			s.sendResponse(msg.ID, toks)
		}

	case "textDocument/codeLens":
		var params struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
		}
		if json.Unmarshal(msg.Params, &params) == nil {
			lenses := s.computeCodeLenses(params.TextDocument.URI)
			s.sendResponse(msg.ID, lenses)
		}

	case "textDocument/foldingRange":
		var params struct {
			TextDocument struct {
				URI string `json:"uri"`
			} `json:"textDocument"`
		}
		if json.Unmarshal(msg.Params, &params) == nil {
			ranges := s.computeFoldingRanges(params.TextDocument.URI)
			s.sendResponse(msg.ID, ranges)
		}

	case "shutdown":
		s.sendResponse(msg.ID, nil)

	case "exit":
		return true
	}
	return false
}

var diagLineColRe = regexp.MustCompile(`line (\d+), col (\d+)`)

func (s *LSPServer) analyzeAndPublishDiagnostics(uri, text string) {
	diags := make([]lspDiagnostic, 0)
	p, root, err := parser.ParseSource([]byte(text), nil)
	if err == nil {
		chk := types.NewChecker(p.Arena, nil)
		chk.AttachSource(p.Source(), p.Tokens())
		_, err = chk.Infer(root)
	}
	if err != nil {
		lineIdx, colIdx := 0, 0
		if m := diagLineColRe.FindStringSubmatch(err.Error()); len(m) == 3 {
			if l, e1 := strconv.Atoi(m[1]); e1 == nil && l > 0 {
				lineIdx = l - 1
			}
			if c, e2 := strconv.Atoi(m[2]); e2 == nil && c > 0 {
				colIdx = c - 1
			}
		}
		diags = append(diags, lspDiagnostic{
			Range: lspRange{
				Start: lspPosition{Line: lineIdx, Character: colIdx},
				End:   lspPosition{Line: lineIdx, Character: colIdx + 1},
			},
			Severity: 1,
			Source:   "fml",
			Message:  err.Error(),
		})
	}
	s.sendNotification("textDocument/publishDiagnostics", map[string]any{
		"uri":         uri,
		"diagnostics": diags,
	})
}

func (s *LSPServer) computeHover(uri string, pos lspPosition) string {
	text, ok := s.Docs[uri]
	if !ok {
		return ""
	}
	p, root, err := parser.ParseSource([]byte(text), nil)
	if err != nil {
		return ""
	}
	chk := types.NewChecker(p.Arena, nil)
	chk.AttachSource(p.Source(), p.Tokens())
	if _, err := chk.Infer(root); err != nil {
		return ""
	}

	targetLine := uint16(pos.Line + 1)
	targetCol := uint16(pos.Character + 1)

	// Find the token under (targetLine, targetCol)
	tokIdx := -1
	var symName string
	for i, tk := range p.Tokens() {
		if tk.Line == targetLine {
			spanLen := uint16(tk.End - tk.Start)
			if spanLen == 0 {
				spanLen = 1
			}
			if targetCol >= tk.Col && targetCol <= tk.Col+spanLen {
				tokIdx = i
				symName = string(tk.Slice(p.Source()))
				break
			}
		}
	}

	// 1. Check if an AST node directly references tokIdx and has an inferred type
	if tokIdx >= 0 {
		for id := 1; id < len(p.Arena.Nodes); id++ {
			n := p.Arena.Nodes[id]
			if int(n.TokenIdx) == tokIdx && id < len(chk.NodeTypes) && chk.NodeTypes[id] != types.NoType {
				tyStr := chk.Types.Format(chk.NodeTypes[id])
				if n.Data != 0 && (n.Kind == ast.KindLet || n.Kind == ast.KindLetRec || n.Kind == ast.KindLetMutable || n.Kind == ast.KindIdent) {
					name := p.Arena.Strings.String(token.StringID(n.Data))
					return fmt.Sprintf("```fml\nval %s : %s\n```", name, tyStr)
				}
				return fmt.Sprintf("```fml\n%s\n```", tyStr)
			}
		}
	}

	// 2. Fallback: look up symbol by name in top-level declarations or global scope
	if symName != "" {
		symID := p.Arena.Strings.InternString(symName)
		for id := 1; id < len(p.Arena.Nodes); id++ {
			n := p.Arena.Nodes[id]
			if (n.Kind == ast.KindLet || n.Kind == ast.KindLetRec || n.Kind == ast.KindLetMutable) &&
				token.StringID(n.Data) == symID && id < len(chk.NodeTypes) && chk.NodeTypes[id] != types.NoType {
				return fmt.Sprintf("```fml\nval %s : %s\n```", symName, chk.Types.Format(chk.NodeTypes[id]))
			}
		}
		if b, found := chk.Scopes[0][symID]; found {
			return fmt.Sprintf("```fml\nval %s : %s\n```", symName, chk.Types.Format(b.Scheme.Body))
		}
	}
	return ""
}

func tokenRangeForDecl(p *parser.Parser, startTokIdx int, symName string) lspRange {
	toks := p.Tokens()
	if startTokIdx < 0 || startTokIdx >= len(toks) {
		return lspRange{}
	}
	chosen := toks[startTokIdx]
	for i := startTokIdx; i < len(toks) && i <= startTokIdx+5; i++ {
		if string(toks[i].Slice(p.Source())) == symName {
			chosen = toks[i]
			break
		}
	}
	line := int(chosen.Line) - 1
	if line < 0 {
		line = 0
	}
	col := int(chosen.Col) - 1
	if col < 0 {
		col = 0
	}
	spanLen := int(chosen.End - chosen.Start)
	if spanLen <= 0 {
		spanLen = len(symName)
	}
	return lspRange{
		Start: lspPosition{Line: line, Character: col},
		End:   lspPosition{Line: line, Character: col + spanLen},
	}
}

func isDeclarationNode(k ast.NodeKind) bool {
	switch k {
	case ast.KindLet, ast.KindLetRec, ast.KindLetMutable,
		ast.KindRecordType, ast.KindDUType, ast.KindDUCase,
		ast.KindActivePattern, ast.KindLambda, ast.KindModule:
		return true
	default:
		return false
	}
}

func (s *LSPServer) computeDefinition(uri string, pos lspPosition) *lspLocation {
	text, ok := s.Docs[uri]
	if !ok {
		return nil
	}
	p, _, err := parser.ParseSource([]byte(text), nil)
	if err != nil {
		return nil
	}

	targetLine := uint16(pos.Line + 1)
	targetCol := uint16(pos.Character + 1)

	tokIdx := -1
	var symName string
	for i, tk := range p.Tokens() {
		if tk.Line == targetLine {
			spanLen := uint16(tk.End - tk.Start)
			if spanLen == 0 {
				spanLen = 1
			}
			if targetCol >= tk.Col && targetCol <= tk.Col+spanLen {
				tokIdx = i
				symName = string(tk.Slice(p.Source()))
				break
			}
		}
	}
	if symName == "" {
		return nil
	}
	symID := p.Arena.Strings.InternString(symName)

	bestNodeIdx := -1
	bestTokIdx := -1
	fallbackNodeIdx := -1
	for id := 1; id < len(p.Arena.Nodes); id++ {
		n := p.Arena.Nodes[id]
		if !isDeclarationNode(n.Kind) || token.StringID(n.Data) != symID {
			continue
		}
		if fallbackNodeIdx == -1 {
			fallbackNodeIdx = id
		}
		if int(n.TokenIdx) <= tokIdx && int(n.TokenIdx) >= bestTokIdx {
			bestTokIdx = int(n.TokenIdx)
			bestNodeIdx = id
		}
	}
	if bestNodeIdx == -1 {
		bestNodeIdx = fallbackNodeIdx
	}
	if bestNodeIdx == -1 {
		return nil
	}
	declNode := p.Arena.Nodes[bestNodeIdx]
	return &lspLocation{
		URI:   uri,
		Range: tokenRangeForDecl(p, int(declNode.TokenIdx), symName),
	}
}

func (s *LSPServer) computeDocumentSymbols(uri string) []lspDocumentSymbol {
	text, ok := s.Docs[uri]
	if !ok {
		return nil
	}
	p, root, err := parser.ParseSource([]byte(text), nil)
	if err != nil {
		return nil
	}
	chk := types.NewChecker(p.Arena, nil)
	chk.AttachSource(p.Source(), p.Tokens())
	_, _ = chk.Infer(root)

	var stmts []uint32
	rootNode := p.Arena.Get(root)
	if rootNode.Kind == ast.KindProgram || rootNode.Kind == ast.KindBlock {
		stmts = p.Arena.ExtraSlice(rootNode.Left, rootNode.Right)
	} else {
		stmts = []uint32{uint32(root)}
	}

	out := make([]lspDocumentSymbol, 0, len(stmts))
	for _, rawID := range stmts {
		id := ast.NodeID(rawID)
		n := p.Arena.Get(id)
		if n.Data == 0 {
			continue
		}
		var symKind int
		switch n.Kind {
		case ast.KindLet, ast.KindLetRec, ast.KindLetMutable:
			symKind = 13 // Variable
			if ast.NodeID(n.Left) != ast.NoNode && p.Arena.Get(ast.NodeID(n.Left)).Kind == ast.KindLambda {
				symKind = 12 // Function
			}
		case ast.KindRecordType:
			symKind = 23 // Struct
		case ast.KindDUType:
			symKind = 10 // Enum
		case ast.KindModule:
			symKind = 2 // Module
		case ast.KindActivePattern:
			symKind = 12 // Function
		default:
			continue
		}

		name := p.Arena.Strings.String(token.StringID(n.Data))
		var detail string
		if int(id) < len(chk.NodeTypes) && chk.NodeTypes[id] != types.NoType {
			detail = chk.Types.Format(chk.NodeTypes[id])
		}
		rng := tokenRangeForDecl(p, int(n.TokenIdx), name)
		out = append(out, lspDocumentSymbol{
			Name:           name,
			Detail:         detail,
			Kind:           symKind,
			Range:          rng,
			SelectionRange: rng,
		})
	}
	return out
}

var (
	dotCompletionRe   = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\.([A-Za-z0-9_]*)$`)
	identCompletionRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)$`)
)

func (s *LSPServer) computeCompletion(uri string, pos lspPosition) []lspCompletionItem {
	text, ok := s.Docs[uri]
	if !ok {
		return nil
	}
	lines := strings.Split(text, "\n")
	var linePrefix string
	if pos.Line >= 0 && pos.Line < len(lines) {
		line := lines[pos.Line]
		if pos.Character >= 0 && pos.Character <= len(line) {
			linePrefix = line[:pos.Character]
		} else {
			linePrefix = line
		}
	}

	arena := ast.NewArena(64, 64, nil)
	chk := types.NewChecker(arena, nil)

	// 1. Dot-triggered module member completion (`Module.prefix`)
	if m := dotCompletionRe.FindStringSubmatch(linePrefix); len(m) == 3 {
		modName := m[1]
		memPrefix := m[2]
		prefix := modName + "."
		var items []lspCompletionItem
		for symID, b := range chk.Scopes[0] {
			fullName := arena.Strings.String(symID)
			if strings.HasPrefix(fullName, prefix) {
				member := strings.TrimPrefix(fullName, prefix)
				if strings.HasPrefix(member, memPrefix) {
					items = append(items, lspCompletionItem{
						Label:  member,
						Kind:   3, // Function
						Detail: chk.Types.Format(b.Scheme.Body),
					})
				}
			}
		}
		sort.Slice(items, func(i, j int) bool {
			return items[i].Label < items[j].Label
		})
		return items
	}

	// 2. Identifier / module prefix completion
	var identPrefix string
	if m := identCompletionRe.FindStringSubmatch(linePrefix); len(m) == 2 {
		identPrefix = m[1]
	}

	seen := make(map[string]bool)
	var items []lspCompletionItem

	for symID, b := range chk.Scopes[0] {
		fullName := arena.Strings.String(symID)
		if idx := strings.IndexByte(fullName, '.'); idx > 0 {
			modName := fullName[:idx]
			if !seen[modName] && strings.HasPrefix(modName, identPrefix) {
				seen[modName] = true
				items = append(items, lspCompletionItem{
					Label:  modName,
					Kind:   9, // Module
					Detail: "module " + modName,
				})
			}
			continue
		}
		if !seen[fullName] && strings.HasPrefix(fullName, identPrefix) {
			seen[fullName] = true
			items = append(items, lspCompletionItem{
				Label:  fullName,
				Kind:   3, // Function
				Detail: chk.Types.Format(b.Scheme.Body),
			})
		}
	}

	if p, root, err := parser.ParseSource([]byte(text), nil); err == nil {
		docChk := types.NewChecker(p.Arena, nil)
		_, _ = docChk.Infer(root)
		for id := 1; id < len(p.Arena.Nodes); id++ {
			n := p.Arena.Nodes[id]
			if !isDeclarationNode(n.Kind) || n.Data == 0 {
				continue
			}
			name := p.Arena.Strings.String(token.StringID(n.Data))
			if name == "" || seen[name] || !strings.HasPrefix(name, identPrefix) {
				continue
			}
			seen[name] = true
			var detail string
			if id < len(docChk.NodeTypes) && docChk.NodeTypes[id] != types.NoType {
				detail = docChk.Types.Format(docChk.NodeTypes[id])
			}
			items = append(items, lspCompletionItem{
				Label:  name,
				Kind:   6, // Variable
				Detail: detail,
			})
		}
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].Label < items[j].Label
	})
	return items
}

func (s *LSPServer) computeReferences(uri string, pos lspPosition) []lspLocation {
	text, ok := s.Docs[uri]
	if !ok {
		return nil
	}
	p, _, err := parser.ParseSource([]byte(text), nil)
	if err != nil {
		return nil
	}

	targetLine := uint16(pos.Line + 1)
	targetCol := uint16(pos.Character + 1)

	var symName string
	for _, tk := range p.Tokens() {
		if tk.Line == targetLine {
			spanLen := uint16(tk.End - tk.Start)
			if spanLen == 0 {
				spanLen = 1
			}
			if targetCol >= tk.Col && targetCol <= tk.Col+spanLen {
				symName = string(tk.Slice(p.Source()))
				break
			}
		}
	}
	if symName == "" {
		return nil
	}

	var locs []lspLocation
	for _, tk := range p.Tokens() {
		if tk.Kind != token.TokenIdent {
			continue
		}
		if string(tk.Slice(p.Source())) != symName {
			continue
		}
		line := int(tk.Line) - 1
		if line < 0 {
			line = 0
		}
		col := int(tk.Col) - 1
		if col < 0 {
			col = 0
		}
		spanLen := int(tk.End - tk.Start)
		if spanLen <= 0 {
			spanLen = len(symName)
		}
		locs = append(locs, lspLocation{
			URI: uri,
			Range: lspRange{
				Start: lspPosition{Line: line, Character: col},
				End:   lspPosition{Line: line, Character: col + spanLen},
			},
		})
	}
	return locs
}

func (s *LSPServer) computeRename(uri string, pos lspPosition, newName string) *lspWorkspaceEdit {
	newName = strings.TrimSpace(newName)
	if newName == "" {
		return nil
	}
	locs := s.computeReferences(uri, pos)
	if len(locs) == 0 {
		return nil
	}
	edits := make([]lspTextEdit, len(locs))
	for i, loc := range locs {
		edits[i] = lspTextEdit{
			Range:   loc.Range,
			NewText: newName,
		}
	}
	return &lspWorkspaceEdit{
		Changes: map[string][]lspTextEdit{
			uri: edits,
		},
	}
}

func (s *LSPServer) computeFormatting(uri string) []lspTextEdit {
	text, ok := s.Docs[uri]
	if !ok {
		return nil
	}
	formatted, err := FormatSource([]byte(text), 80)
	if err != nil {
		return nil
	}
	lines := strings.Split(text, "\n")
	endLine := len(lines) - 1
	if endLine < 0 {
		endLine = 0
	}
	endChar := 0
	if len(lines) > 0 {
		endChar = len(lines[len(lines)-1])
	}
	return []lspTextEdit{
		{
			Range: lspRange{
				Start: lspPosition{Line: 0, Character: 0},
				End:   lspPosition{Line: endLine, Character: endChar},
			},
			NewText: formatted,
		},
	}
}

func (s *LSPServer) computeSignatureHelp(uri string, pos lspPosition) *lspSignatureHelp {
	text, ok := s.Docs[uri]
	if !ok {
		return nil
	}
	lines := strings.Split(text, "\n")
	if pos.Line < 0 || pos.Line >= len(lines) {
		return nil
	}
	line := lines[pos.Line]
	col := pos.Character
	if col > len(line) {
		col = len(line)
	}
	if col < 0 {
		col = 0
	}
	prefix := line[:col]
	if eqIdx := strings.LastIndex(prefix, "="); eqIdx >= 0 && (eqIdx == 0 || prefix[eqIdx-1] != '=') {
		prefix = prefix[eqIdx+1:]
	}
	if pipeIdx := strings.LastIndex(prefix, "|>"); pipeIdx >= 0 {
		prefix = prefix[pipeIdx+2:]
	}

	tokens := splitCallTokens(prefix)
	if len(tokens) == 0 {
		return nil
	}
	fnName := tokens[0]
	activeParam := len(tokens) - 1
	if len(prefix) > 0 && prefix[len(prefix)-1] != ' ' && prefix[len(prefix)-1] != '\t' && len(tokens) > 1 {
		activeParam = len(tokens) - 2
	}
	if activeParam < 0 {
		activeParam = 0
	}

	arena := ast.NewArena(64, 64, nil)
	chk := types.NewChecker(arena, nil)

	// Also attempt to type-check preceding lines or full document so user-defined functions resolve
	priorText := strings.Join(lines[:pos.Line], "\n") + "\n"
	if p, root, err := parser.ParseSource([]byte(text), nil); err == nil {
		docChk := types.NewChecker(p.Arena, nil)
		_, _ = docChk.Infer(root)
		arena = p.Arena
		chk = docChk
	} else if p2, root2, err2 := parser.ParseSource([]byte(priorText), nil); err2 == nil {
		docChk := types.NewChecker(p2.Arena, nil)
		_, _ = docChk.Infer(root2)
		arena = p2.Arena
		chk = docChk
	}

	symID := arena.Strings.InternString(fnName)
	b, found := chk.Scopes[0][symID]
	if !found {
		return nil
	}
	ty := chk.Types.Find(b.Scheme.Body)
	var params []lspParameterInformation
	curr := ty
	for int(curr) < len(chk.Types.Nodes) && chk.Types.Nodes[curr].Kind == types.TyArrow {
		n := chk.Types.Nodes[curr]
		paramTy := chk.Types.Format(types.TypeID(n.Left))
		params = append(params, lspParameterInformation{Label: paramTy})
		curr = chk.Types.Find(types.TypeID(n.Right))
	}
	if len(params) == 0 {
		return nil
	}
	if activeParam >= len(params) {
		activeParam = len(params) - 1
	}
	sigInfo := lspSignatureInformation{
		Label:           fmt.Sprintf("%s : %s", fnName, chk.Types.Format(ty)),
		Parameters:      params,
		ActiveParameter: activeParam,
	}
	return &lspSignatureHelp{
		Signatures:      []lspSignatureInformation{sigInfo},
		ActiveSignature: 0,
		ActiveParameter: activeParam,
	}
}

func splitCallTokens(s string) []string {
	var out []string
	var cur strings.Builder
	depth := 0
	inStr := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch == '"' && (i == 0 || s[i-1] != '\\') {
			inStr = !inStr
			cur.WriteByte(ch)
			continue
		}
		if inStr {
			cur.WriteByte(ch)
			continue
		}
		if ch == '(' || ch == '[' || ch == '{' {
			depth++
			cur.WriteByte(ch)
			continue
		}
		if ch == ')' || ch == ']' || ch == '}' {
			if depth > 0 {
				depth--
			}
			cur.WriteByte(ch)
			continue
		}
		if (ch == ' ' || ch == '\t') && depth == 0 {
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
			continue
		}
		cur.WriteByte(ch)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

var missingADTRe = regexp.MustCompile(`missing constructor\(s\) \[([^\]]+)\]`)

func (s *LSPServer) computeCodeActions(uri string, _ lspRange) []lspCodeAction {
	text, ok := s.Docs[uri]
	if !ok {
		return nil
	}
	p, root, err := parser.ParseSource([]byte(text), nil)
	if err != nil {
		return nil
	}
	chk := types.NewChecker(p.Arena, nil)
	chk.AttachSource(p.Source(), p.Tokens())
	_, err = chk.Infer(root)
	if err == nil || !strings.Contains(err.Error(), "non-exhaustive") {
		return nil
	}

	errMsg := err.Error()
	var missingPats []string

	if m := missingADTRe.FindStringSubmatch(errMsg); len(m) == 2 {
		names := strings.Fields(m[1])
		sort.Strings(names)
		for _, ctorName := range names {
			cID := p.Arena.Strings.InternString(ctorName)
			if info, hasCtor := chk.Constructors[cID]; hasCtor && info.Payload != types.NoType {
				missingPats = append(missingPats, ctorName+" _")
			} else {
				missingPats = append(missingPats, ctorName)
			}
		}
	} else if strings.Contains(errMsg, "non-exhaustive option match") {
		if !strings.Contains(text, "| None") {
			missingPats = append(missingPats, "None")
		} else {
			missingPats = append(missingPats, "Some _")
		}
	} else if strings.Contains(errMsg, "non-exhaustive Result match") {
		if !strings.Contains(text, "| Error") {
			missingPats = append(missingPats, "Error _")
		} else {
			missingPats = append(missingPats, "Ok _")
		}
	} else if strings.Contains(errMsg, "non-exhaustive boolean match") {
		if !strings.Contains(text, "| false") {
			missingPats = append(missingPats, "false")
		} else {
			missingPats = append(missingPats, "true")
		}
	} else {
		missingPats = append(missingPats, "_")
	}

	lines := strings.Split(text, "\n")
	insertLine := len(lines) - 1
	indent := "    "
	rhsDefault := "0"
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "|") {
			insertLine = i + 1
			leadLen := len(lines[i]) - len(strings.TrimLeft(lines[i], " \t"))
			indent = lines[i][:leadLen]
			if arrowIdx := strings.Index(trimmed, "->"); arrowIdx >= 0 {
				rhsSample := strings.TrimSpace(trimmed[arrowIdx+2:])
				if strings.HasPrefix(rhsSample, "\"") {
					rhsDefault = "\"\""
				} else if rhsSample == "true" || rhsSample == "false" {
					rhsDefault = "false"
				} else if strings.Contains(rhsSample, ".") {
					rhsDefault = "0.0"
				}
			}
			break
		}
	}

	var stub strings.Builder
	for _, pat := range missingPats {
		fmt.Fprintf(&stub, "%s| %s -> %s\n", indent, pat, rhsDefault)
	}

	pos := lspPosition{Line: insertLine, Character: 0}
	return []lspCodeAction{
		{
			Title: fmt.Sprintf("Add missing match case(s): %s", strings.Join(missingPats, ", ")),
			Kind:  "quickfix",
			Edit: &lspWorkspaceEdit{
				Changes: map[string][]lspTextEdit{
					uri: {
						{
							Range:   lspRange{Start: pos, End: pos},
							NewText: stub.String(),
						},
					},
				},
			},
		},
	}
}

func (s *LSPServer) computeInlayHints(uri string, rng lspRange) []lspInlayHint {
	text, ok := s.Docs[uri]
	if !ok {
		return nil
	}
	p, root, err := parser.ParseSource([]byte(text), nil)
	if err != nil {
		return nil
	}
	chk := types.NewChecker(p.Arena, nil)
	chk.AttachSource(p.Source(), p.Tokens())
	_, _ = chk.Infer(root)

	toks := p.Tokens()
	src := p.Source()
	filterRange := rng.End.Line > 0 || rng.End.Character > 0

	var hints []lspInlayHint
	seenPos := make(map[[2]int]bool)
	addHint := func(pos lspPosition, label string) {
		if filterRange && (pos.Line < rng.Start.Line || pos.Line > rng.End.Line) {
			return
		}
		key := [2]int{pos.Line, pos.Character}
		if seenPos[key] {
			return
		}
		seenPos[key] = true
		hints = append(hints, lspInlayHint{
			Position: pos,
			Label:    label,
			Kind:     1, // InlayHintKind.Type
		})
	}

	for id := 1; id < len(p.Arena.Nodes); id++ {
		n := p.Arena.Nodes[id]
		switch n.Kind {
		case ast.KindLet, ast.KindLetRec, ast.KindLetMutable, ast.KindUse:
			if n.Op == 1 || n.Data == 0 {
				continue
			}
			symName := p.Arena.Strings.String(token.StringID(n.Data))
			if symName == "" || symName == "_" || strings.HasPrefix(symName, "__") {
				continue
			}
			lhsID := ast.NodeID(n.Left)
			if lhsID != ast.NoNode {
				lhsKind := p.Arena.Get(lhsID).Kind
				if lhsKind == ast.KindLambda || lhsKind == ast.KindTypeAnnot {
					continue
				}
			}
			nameTokIdx := -1
			startIdx := int(n.TokenIdx)
			for i := startIdx; i < len(toks) && i <= startIdx+5; i++ {
				if string(toks[i].Slice(src)) == symName {
					nameTokIdx = i
					break
				}
			}
			if nameTokIdx < 0 || (nameTokIdx+1 < len(toks) && toks[nameTokIdx+1].Kind == token.TokenColon) {
				continue
			}
			if id < len(chk.NodeTypes) && chk.NodeTypes[id] != types.NoType {
				tyStr := chk.Types.Format(chk.NodeTypes[id])
				tk := toks[nameTokIdx]
				spanLen := int(tk.End - tk.Start)
				if spanLen <= 0 {
					spanLen = len(symName)
				}
				pos := lspPosition{
					Line:      max(0, int(tk.Line)-1),
					Character: max(0, int(tk.Col)-1) + spanLen,
				}
				addHint(pos, ": "+tyStr)
			}

		case ast.KindLambda:
			paramID := ast.NodeID(n.Left)
			if paramID == ast.NoNode {
				continue
			}
			paramNode := p.Arena.Get(paramID)
			if paramNode.Kind != ast.KindIdent || paramNode.Data == 0 {
				continue
			}
			paramName := p.Arena.Strings.String(token.StringID(paramNode.Data))
			if paramName == "" || paramName == "_" || strings.HasPrefix(paramName, "__") {
				continue
			}
			pTokIdx := int(paramNode.TokenIdx)
			if pTokIdx < 0 || pTokIdx >= len(toks) {
				continue
			}
			if id < len(chk.NodeTypes) && chk.NodeTypes[id] != types.NoType {
				lamTy := chk.Types.Find(chk.NodeTypes[id])
				if lamTy != types.NoType && int(lamTy) < len(chk.Types.Nodes) {
					tNode := chk.Types.Nodes[lamTy]
					if tNode.Kind == types.TyArrow {
						paramTy := chk.Types.Find(types.TypeID(tNode.Left))
						tyStr := chk.Types.Format(paramTy)
						tk := toks[pTokIdx]
						spanLen := int(tk.End - tk.Start)
						if spanLen <= 0 {
							spanLen = len(paramName)
						}
						pos := lspPosition{
							Line:      max(0, int(tk.Line)-1),
							Character: max(0, int(tk.Col)-1) + spanLen,
						}
						addHint(pos, ": "+tyStr)
					}
				}
			}
		}
	}

	sort.Slice(hints, func(i, j int) bool {
		if hints[i].Position.Line != hints[j].Position.Line {
			return hints[i].Position.Line < hints[j].Position.Line
		}
		return hints[i].Position.Character < hints[j].Position.Character
	})
	return hints
}

func (s *LSPServer) computeWorkspaceSymbols(query string) []lspWorkspaceSymbol {
	var uris []string
	for uri := range s.Docs {
		uris = append(uris, uri)
	}
	sort.Strings(uris)

	qLower := strings.ToLower(strings.TrimSpace(query))
	out := make([]lspWorkspaceSymbol, 0)
	for _, uri := range uris {
		docSyms := s.computeDocumentSymbols(uri)
		for _, ds := range docSyms {
			if qLower != "" && !strings.Contains(strings.ToLower(ds.Name), qLower) {
				continue
			}
			out = append(out, lspWorkspaceSymbol{
				Name: ds.Name,
				Kind: ds.Kind,
				Location: lspLocation{
					URI:   uri,
					Range: ds.SelectionRange,
				},
			})
		}
	}
	return out
}

func (s *LSPServer) computeSemanticTokens(uri string) *lspSemanticTokens {
	text, ok := s.Docs[uri]
	if !ok {
		return &lspSemanticTokens{Data: []int{}}
	}
	p, root, err := parser.ParseSource([]byte(text), nil)
	if err != nil {
		return &lspSemanticTokens{Data: []int{}}
	}
	chk := types.NewChecker(p.Arena, nil)
	chk.AttachSource(p.Source(), p.Tokens())
	_, _ = chk.Infer(root)

	toks := p.Tokens()
	src := p.Source()

	typeSyms := make(map[token.StringID]bool)
	for _, builtinTy := range []string{"int", "float", "string", "bool", "unit", "list", "array", "option", "Result", "Map", "Set", "Seq", "Async"} {
		typeSyms[p.Arena.Strings.InternString(builtinTy)] = true
	}
	ctorSyms := make(map[token.StringID]bool)
	for cID := range chk.Constructors {
		ctorSyms[cID] = true
	}
	funcSyms := make(map[token.StringID]bool)
	for symID, b := range chk.Scopes[0] {
		ty := chk.Types.Find(b.Scheme.Body)
		if ty != types.NoType && int(ty) < len(chk.Types.Nodes) && chk.Types.Nodes[ty].Kind == types.TyArrow {
			funcSyms[symID] = true
		}
	}
	paramSyms := make(map[token.StringID]bool)
	paramTokIdxs := make(map[int]bool)
	declTokIdxs := make(map[int]bool)

	for id := 1; id < len(p.Arena.Nodes); id++ {
		n := p.Arena.Nodes[id]
		sym := token.StringID(n.Data)
		switch n.Kind {
		case ast.KindRecordType, ast.KindDUType, ast.KindMeasure, ast.KindTypeIdent:
			if sym != token.NoString {
				typeSyms[sym] = true
			}
		case ast.KindDUCase:
			if sym != token.NoString {
				ctorSyms[sym] = true
			}
		case ast.KindLet, ast.KindLetRec, ast.KindLetMutable, ast.KindUse:
			if sym != token.NoString {
				name := p.Arena.Strings.String(sym)
				startIdx := int(n.TokenIdx)
				for i := startIdx; i < len(toks) && i <= startIdx+5; i++ {
					if string(toks[i].Slice(src)) == name {
						declTokIdxs[i] = true
						break
					}
				}
				if ast.NodeID(n.Left) != ast.NoNode && p.Arena.Get(ast.NodeID(n.Left)).Kind == ast.KindLambda {
					funcSyms[sym] = true
				}
			}
		case ast.KindLambda:
			if sym != token.NoString {
				paramSyms[sym] = true
			}
			if ast.NodeID(n.Left) != ast.NoNode {
				pn := p.Arena.Get(ast.NodeID(n.Left))
				if pn.Kind == ast.KindIdent {
					paramTokIdxs[int(pn.TokenIdx)] = true
					declTokIdxs[int(pn.TokenIdx)] = true
				}
			}
		}
	}

	// Token type indices matching legend:
	// 0: keyword, 1: type, 2: enumMember, 3: function, 4: parameter, 5: variable, 6: number, 7: string, 8: operator
	var data []int
	prevLine := 0
	prevCol := 0

	for i, tk := range toks {
		if tk.Kind <= token.TokenSemi || tk.End <= tk.Start {
			continue
		}
		tokType := -1
		tokMod := 0

		switch {
		case tk.Kind == token.TokenInt || tk.Kind == token.TokenFloat:
			tokType = 6
		case tk.Kind == token.TokenString:
			tokType = 7
		case tk.Kind == token.TokenSome || tk.Kind == token.TokenNone || tk.Kind == token.TokenOk || tk.Kind == token.TokenError:
			tokType = 2
		case tk.Kind >= token.TokenTrue && tk.Kind <= token.TokenVal:
			tokType = 0
		case tk.Kind >= token.TokenAssign && tk.Kind <= token.TokenColonQMark:
			tokType = 8
		case tk.Kind == token.TokenIdent:
			lexeme := string(tk.Slice(src))
			sid := p.Arena.Strings.InternString(lexeme)
			if declTokIdxs[i] {
				tokMod |= 1 // declaration
			}
			switch {
			case typeSyms[sid]:
				tokType = 1
			case ctorSyms[sid]:
				tokType = 2
			case paramTokIdxs[i] || paramSyms[sid]:
				tokType = 4
			case funcSyms[sid]:
				tokType = 3
			default:
				tokType = 5
			}
		}

		if tokType < 0 {
			continue
		}

		line := max(0, int(tk.Line)-1)
		col := max(0, int(tk.Col)-1)
		length := int(tk.End - tk.Start)

		deltaLine := line - prevLine
		deltaCol := col
		if deltaLine == 0 {
			deltaCol = col - prevCol
		}
		data = append(data, deltaLine, deltaCol, length, tokType, tokMod)
		prevLine = line
		prevCol = col
	}

	if data == nil {
		data = []int{}
	}
	return &lspSemanticTokens{Data: data}
}

func (s *LSPServer) computeCodeLenses(uri string) []lspCodeLens {
	text, ok := s.Docs[uri]
	if !ok {
		return []lspCodeLens{}
	}
	p, root, err := parser.ParseSource([]byte(text), nil)
	if err != nil {
		return []lspCodeLens{}
	}
	chk := types.NewChecker(p.Arena, nil)
	chk.AttachSource(p.Source(), p.Tokens())
	_, _ = chk.Infer(root)

	progNode := p.Arena.Get(root)
	var stmts []uint32
	if progNode.Kind == ast.KindProgram || progNode.Kind == ast.KindBlock {
		stmts = p.Arena.ExtraSlice(progNode.Left, progNode.Right)
	} else {
		stmts = []uint32{uint32(root)}
	}

	out := make([]lspCodeLens, 0, len(stmts))
	for _, rawID := range stmts {
		id := ast.NodeID(rawID)
		n := p.Arena.Get(id)
		switch n.Kind {
		case ast.KindLet, ast.KindLetRec, ast.KindLetMutable, ast.KindUse:
			sym := token.StringID(n.Data)
			if sym == token.NoString {
				continue
			}
			name := p.Arena.Strings.String(sym)
			rng := tokenRangeForDecl(p, int(n.TokenIdx), name)
			if int(id) < len(chk.NodeTypes) && chk.NodeTypes[id] != types.NoType {
				tyStr := chk.Types.Format(chk.NodeTypes[id])
				out = append(out, lspCodeLens{
					Range: rng,
					Command: &lspCommand{
						Title:   fmt.Sprintf("val %s : %s", name, tyStr),
						Command: "fml.showType",
					},
				})
			}
			if strings.HasPrefix(name, "test_") {
				out = append(out, lspCodeLens{
					Range: rng,
					Command: &lspCommand{
						Title:     "Run test",
						Command:   "fml.runTest",
						Arguments: []any{uri, name},
					},
				})
			} else if strings.HasPrefix(name, "bench_") {
				out = append(out, lspCodeLens{
					Range: rng,
					Command: &lspCommand{
						Title:     "Run benchmark",
						Command:   "fml.runBench",
						Arguments: []any{uri, name},
					},
				})
			}
		}
	}
	return out
}

func (s *LSPServer) computeFoldingRanges(uri string) []lspFoldingRange {
	text, ok := s.Docs[uri]
	if !ok {
		return []lspFoldingRange{}
	}
	p, root, err := parser.ParseSource([]byte(text), nil)
	if err != nil || root == ast.NoNode {
		return []lspFoldingRange{}
	}
	toks := p.Tokens()

	type linePair struct {
		start int
		end   int
	}
	seen := make(map[linePair]bool)
	var out []lspFoldingRange

	for id := 1; id < len(p.Arena.Nodes); id++ {
		n := p.Arena.Nodes[id]
		switch n.Kind {
		case ast.KindLet, ast.KindLetRec, ast.KindLetMutable, ast.KindMatch,
			ast.KindCompExpr, ast.KindSeq, ast.KindModule, ast.KindRecordType,
			ast.KindDUType, ast.KindWhile, ast.KindForIn, ast.KindForTo:
			minLine, maxLine := nodeLineBounds(p.Arena, toks, ast.NodeID(id), make(map[ast.NodeID]bool))
			if maxLine > minLine && minLine >= 0 {
				pair := linePair{start: minLine, end: maxLine}
				if !seen[pair] {
					seen[pair] = true
					out = append(out, lspFoldingRange{
						StartLine: minLine,
						EndLine:   maxLine,
						Kind:      "region",
					})
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartLine != out[j].StartLine {
			return out[i].StartLine < out[j].StartLine
		}
		return out[i].EndLine < out[j].EndLine
	})
	if out == nil {
		return []lspFoldingRange{}
	}
	return out
}

func nodeLineBounds(arena *ast.Arena, toks []token.Token, id ast.NodeID, visited map[ast.NodeID]bool) (int, int) {
	if id == ast.NoNode || int(id) >= len(arena.Nodes) || visited[id] {
		return -1, -1
	}
	visited[id] = true
	n := arena.Nodes[id]
	minL, maxL := -1, -1
	if n.Kind != ast.KindProgram && n.Kind != ast.KindBlock &&
		(n.TokenIdx > 0 || id == 1) && int(n.TokenIdx) < len(toks) && toks[n.TokenIdx].Line > 0 {
		l := int(toks[n.TokenIdx].Line) - 1
		minL, maxL = l, l
	}
	merge := func(child ast.NodeID) {
		cMin, cMax := nodeLineBounds(arena, toks, child, visited)
		if cMin >= 0 {
			if minL < 0 || cMin < minL {
				minL = cMin
			}
			if cMax > maxL {
				maxL = cMax
			}
		}
	}

	switch n.Kind {
	case ast.KindProgram, ast.KindBlock, ast.KindTuple, ast.KindList, ast.KindArray,
		ast.KindRecordLit, ast.KindRecordType, ast.KindDUType, ast.KindPatTuple,
		ast.KindPatList, ast.KindPatRecord, ast.KindTypeTuple:
		for _, ch := range arena.ExtraSlice(n.Left, n.Right) {
			merge(ast.NodeID(ch))
		}
	case ast.KindLet, ast.KindLetMutable, ast.KindLetRec, ast.KindUse:
		if n.Op == 1 {
			for _, ch := range arena.ExtraSlice(n.Left, 3) {
				merge(ast.NodeID(ch))
			}
		} else {
			merge(ast.NodeID(n.Left))
			merge(ast.NodeID(n.Right))
		}
	case ast.KindIf:
		merge(ast.NodeID(n.Left))
		merge(ast.NodeID(n.Right))
		merge(ast.NodeID(n.Data))
	case ast.KindMatchArm:
		merge(ast.NodeID(n.Left))
		merge(ast.NodeID(n.Data))
		merge(ast.NodeID(n.Right))
	default:
		merge(ast.NodeID(n.Left))
		merge(ast.NodeID(n.Right))
	}
	return minL, maxL
}

func (s *LSPServer) sendResponse(id json.RawMessage, result any) {
	resp := rpcMessage{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	s.writeFrame(resp)
}

func (s *LSPServer) sendNotification(method string, params any) {
	rawParams, _ := json.Marshal(params)
	notif := rpcMessage{
		JSONRPC: "2.0",
		Method:  method,
		Params:  rawParams,
	}
	s.writeFrame(notif)
}

func (s *LSPServer) writeFrame(msg rpcMessage) {
	if s.Out == nil {
		return
	}
	var bodyBuf bytes.Buffer
	enc := json.NewEncoder(&bodyBuf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(msg); err != nil {
		return
	}
	body := bytes.TrimRight(bodyBuf.Bytes(), "\n")
	var frame bytes.Buffer
	fmt.Fprintf(&frame, "Content-Length: %d\r\n\r\n", len(body))
	frame.Write(body)
	_, _ = s.Out.Write(frame.Bytes())
}
