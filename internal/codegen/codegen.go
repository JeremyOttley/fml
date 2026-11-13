// Package codegen implements the Ahead-of-Time (AOT) Go backend for FML,
// translating a lowered 10-opcode ir.Program into standalone Go source with a
// direct per-instruction compiled Go jump table and invoking `go build` to
// produce a native executable binary.
//
// Jaime says: Ten opcodes is all you need. Once type inference, units of
// measure, exhaustiveness checking, and closure conversion are done, specialize
// each instruction ID into a direct Go switch case so constants, variable
// loads, arithmetic, comparisons, and calls execute as native compiled Go.
package codegen

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"go/format"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"fml/internal/ir"
	"fml/internal/parser"
	"fml/internal/token"
	"fml/internal/types"
)

type cacheEntry struct {
	digest [32]byte
	prog   *ir.Program
	hits   uint64
}

// CompileCache provides a thread-safe incremental query cache for compiled FML units.
//
// Jaime says: Don't re-lex, re-parse, and re-unify files whose bytes haven't
// changed. Hash the source and its local module/schema directory, and return a
// flat copy of the lowered 10-opcode IR in O(1).
type CompileCache struct {
	mu      sync.RWMutex
	entries map[string]*cacheEntry
}

// DefaultCache is the process-wide incremental compilation query cache.
var DefaultCache = &CompileCache{
	entries: make(map[string]*cacheEntry),
}

// InvalidateCache clears all entries in DefaultCache.
func InvalidateCache() {
	DefaultCache.mu.Lock()
	DefaultCache.entries = make(map[string]*cacheEntry)
	DefaultCache.mu.Unlock()
}

// CacheHits returns the number of incremental cache hits recorded for srcPath.
func CacheHits(srcPath string) uint64 {
	abs, err := filepath.Abs(srcPath)
	if err != nil {
		abs = srcPath
	}
	DefaultCache.mu.RLock()
	defer DefaultCache.mu.RUnlock()
	if e, ok := DefaultCache.entries[abs]; ok {
		return e.hits
	}
	return 0
}

func computeCompilationDigest(srcPath string) ([32]byte, error) {
	h := sha256.New()
	src, err := os.ReadFile(srcPath)
	if err != nil {
		return [32]byte{}, err
	}
	h.Write(src)
	baseDir := filepath.Dir(srcPath)
	if entries, err := os.ReadDir(baseDir); err == nil {
		var names []string
		for _, e := range entries {
			if !e.IsDir() && (filepath.Ext(e.Name()) == ".fml" || filepath.Ext(e.Name()) == ".json") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, name := range names {
			h.Write([]byte(name))
			if b, err := os.ReadFile(filepath.Join(baseDir, name)); err == nil {
				h.Write(b)
			}
		}
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

func cloneProgram(src *ir.Program) *ir.Program {
	stringsCopy := token.NewStringTable(4096, src.Strings.Len()+64)
	for id := 1; id <= src.Strings.Len(); id++ {
		stringsCopy.InternString(src.Strings.String(token.StringID(id)))
	}
	instrs := make([]ir.IRInstr, len(src.Instrs))
	copy(instrs, src.Instrs)
	extra := make([]uint32, len(src.Extra))
	copy(extra, src.Extra)
	return &ir.Program{
		Instrs:  instrs,
		Extra:   extra,
		Root:    src.Root,
		Strings: stringsCopy,
	}
}

var irKindNames = [...]string{
	ir.IRConst:  "IRConst",
	ir.IRVar:    "IRVar",
	ir.IRLet:    "IRLet",
	ir.IRFn:     "IRFn",
	ir.IRApp:    "IRApp",
	ir.IRSwitch: "IRSwitch",
	ir.IRPrim:   "IRPrim",
	ir.IRAlloc:  "IRAlloc",
	ir.IRLoad:   "IRLoad",
	ir.IRStore:  "IRStore",
}

// Disassemble renders a human-readable disassembly of a lowered 10-opcode ir.Program.
func Disassemble(prog *ir.Program) string {
	var buf bytes.Buffer
	for i, ins := range prog.Instrs {
		kindStr := "UNKNOWN"
		if int(ins.Kind) < len(irKindNames) {
			kindStr = irKindNames[ins.Kind]
		}
		var sym string
		if (ins.Kind == ir.IRLet || ins.Kind == ir.IRVar || ins.Kind == ir.IRFn ||
			(ins.Kind == ir.IRAlloc && ins.SubOp == ir.AllocADT) ||
			(ins.Kind == ir.IRLoad && ins.SubOp == ir.LoadRecordField)) && ins.Data != 0 {
			sym = prog.Strings.String(token.StringID(ins.Data))
		} else if ins.Kind == ir.IRConst && ins.SubOp == ir.ConstString {
			sym = fmt.Sprintf("%q", prog.Strings.String(token.StringID(ins.Data)))
		} else if ins.Kind == ir.IRConst && ins.SubOp == ir.ConstInt {
			sym = fmt.Sprintf("%d", int64(ins.Data))
		}
		marker := " "
		if ir.InstrID(i) == prog.Root {
			marker = "*"
		}
		fmt.Fprintf(&buf, "%s[%03d] %-8s sub=%d flags=0x%02x arg0=%d arg1=%d aux=%d data=%d %s\n",
			marker, i, kindStr, ins.SubOp, ins.Flags, ins.Arg0, ins.Arg1, ins.Aux, ins.Data, sym)
	}
	return buf.String()
}

// CompileFile runs the full front-end and middle-end pipeline (Lexer -> Offside ->
// Parser -> Multi-file Module Loader -> HM Type Inference -> Exhaustiveness ->
// Measure Erasure -> 10-Opcode IR Lowering -> Closure Conversion) on srcPath,
// consulting DefaultCache to skip recompilation when source and dependencies are unchanged.
func CompileFile(srcPath string) (*ir.Program, error) {
	absPath, err := filepath.Abs(srcPath)
	if err != nil {
		absPath = srcPath
	}
	digest, hasDigest := [32]byte{}, false
	if d, dErr := computeCompilationDigest(absPath); dErr == nil {
		digest = d
		hasDigest = true
		DefaultCache.mu.Lock()
		if entry, ok := DefaultCache.entries[absPath]; ok && entry.digest == digest {
			entry.hits++
			cached := cloneProgram(entry.prog)
			DefaultCache.mu.Unlock()
			return cached, nil
		}
		DefaultCache.mu.Unlock()
	}

	p, root, err := parser.ParseFile(srcPath, nil)
	if err != nil {
		return nil, err
	}
	baseDir := filepath.Dir(srcPath)
	chk := types.NewChecker(p.Arena, nil)
	chk.BaseDir = baseDir
	chk.AttachSource(p.Source(), p.Tokens())
	if _, err := chk.Infer(root); err != nil {
		return nil, err
	}
	prog := ir.LowerInDir(p.Arena, chk.Types, root, baseDir)
	ir.Optimize(prog)

	if hasDigest {
		DefaultCache.mu.Lock()
		DefaultCache.entries[absPath] = &cacheEntry{
			digest: digest,
			prog:   cloneProgram(prog),
		}
		DefaultCache.mu.Unlock()
	}
	return prog, nil
}

// EmitGoSource emits a self-contained Go `package main` source file for prog,
// compiling all 10 IR opcodes (closures, ADTs, pattern switches, allocations,
// primitives, and tail calls) directly into a native Go `evalAOT` jump table
// with zero fallback to the interpreted `runtime.VM.Eval` loop.
func EmitGoSource(prog *ir.Program) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("// Code generated by fml AOT compiler. DO NOT EDIT.\n")
	buf.WriteString("package main\n\n")
	buf.WriteString("import (\n")
	buf.WriteString("\t\"fmt\"\n")
	buf.WriteString("\t\"math\"\n")
	buf.WriteString("\t\"os\"\n")
	buf.WriteString("\t\"strings\"\n\n")
	buf.WriteString("\t\"fml/internal/ir\"\n")
	buf.WriteString("\t\"fml/internal/runtime\"\n")
	buf.WriteString("\t\"fml/internal/stdlib\"\n")
	buf.WriteString("\t\"fml/internal/token\"\n")
	buf.WriteString(")\n\n")
	buf.WriteString("var _ = math.Pi\n")
	buf.WriteString("var _ = strings.TrimSpace\n\n")

	// Emit human-readable disassembly comments
	buf.WriteString("// Lowered 10-Opcode IR Disassembly:\n")
	for _, line := range bytes.Split([]byte(Disassemble(prog)), []byte("\n")) {
		if len(line) > 0 {
			buf.WriteString("// ")
			buf.Write(line)
			buf.WriteByte('\n')
		}
	}
	buf.WriteByte('\n')

	// Emit direct compiled Go instruction evaluator `evalAOT` with TCO jump loop
	// and unboxed scalar specialization (`int64`, `float64`, `bool`).
	selfTailCalls := analyzeSelfTailCalls(prog)
	scalars := analyzeAOTScalars(prog)
	emitAOTScalarHelpers(&buf)
	emitAOTNativeFunctions(&buf, prog, scalars)
	buf.WriteString("func evalAOT(vm *runtime.VM, id ir.InstrID, env map[token.StringID]runtime.Value) (runtime.Value, error) {\n")
	buf.WriteString("tcoLoop:\n")
	buf.WriteString("\tfor {\n")
	buf.WriteString("\t\tswitch id {\n")
	for i, ins := range prog.Instrs {
		emitCompiledCase(&buf, prog, ir.InstrID(i), ins, selfTailCalls, scalars)
	}
	buf.WriteString("\t\tdefault:\n")
	buf.WriteString("\t\t\treturn runtime.Unit, nil\n")
	buf.WriteString("\t\t}\n")
	buf.WriteString("\t}\n")
	buf.WriteString("}\n\n")

	buf.WriteString("func buildProgram() *ir.Program {\n")
	buf.WriteString("\tstrings := token.NewStringTable(4096, 512)\n")
	for id := 1; id <= prog.Strings.Len(); id++ {
		s := prog.Strings.String(token.StringID(id))
		fmt.Fprintf(&buf, "\tstrings.InternString(%q)\n", s)
	}
	buf.WriteString("\treturn &ir.Program{\n")
	buf.WriteString("\t\tStrings: strings,\n")
	fmt.Fprintf(&buf, "\t\tRoot:    ir.InstrID(%d),\n", prog.Root)
	buf.WriteString("\t}\n")
	buf.WriteString("}\n\n")

	buf.WriteString("func main() {\n")
	buf.WriteString("\tprog := buildProgram()\n")
	buf.WriteString("\tvm := runtime.NewVM(prog)\n")
	buf.WriteString("\tvm.CustomEval = evalAOT\n")
	buf.WriteString("\tstdlib.RegisterAll(vm)\n")
	buf.WriteString("\tenv := make(map[token.StringID]runtime.Value, len(vm.Globals)+16)\n")
	buf.WriteString("\tfor k, v := range vm.Globals {\n")
	buf.WriteString("\t\tenv[k] = v\n")
	buf.WriteString("\t}\n")
	fmt.Fprintf(&buf, "\tval, err := evalAOT(vm, ir.InstrID(%d), env)\n", prog.Root)
	buf.WriteString("\tif err != nil {\n")
	buf.WriteString("\t\tfmt.Fprintf(os.Stderr, \"runtime error: %v\\n\", err)\n")
	buf.WriteString("\t\tos.Exit(1)\n")
	buf.WriteString("\t}\n")
	buf.WriteString("\tif val != nil && val != runtime.Unit {\n")
	buf.WriteString("\t\tfmt.Fprintln(os.Stdout, val.String())\n")
	buf.WriteString("\t}\n")
	buf.WriteString("}\n")

	formatted, err := format.Source(buf.Bytes())
	if err != nil {
		return buf.Bytes(), nil
	}
	return formatted, nil
}

type selfTailCallInfo struct {
	fnSym      token.StringID
	params     []token.StringID
	argIDs     []ir.InstrID
	targetBody ir.InstrID
	inPlaceEnv bool
}

// analyzeSelfTailCalls detects direct self-recursive IRApp instructions marked
// with IRFlagTailCall inside recursive functions (`let rec f p0 ... pn = body`)
// so AOT codegen can unroll multi-parameter curried tail calls directly into
// in-place parameter updates and `continue tcoLoop` transitions.
func analyzeSelfTailCalls(prog *ir.Program) map[ir.InstrID]*selfTailCallInfo {
	out := make(map[ir.InstrID]*selfTailCallInfo)
	for _, ins := range prog.Instrs {
		if ins.Kind != ir.IRLet || ins.Flags&ir.IRFlagRec == 0 || ins.Data == 0 {
			continue
		}
		fnSym := token.StringID(ins.Data)
		var params []token.StringID
		curr := ir.InstrID(ins.Arg0)
		for curr != ir.NoInstr && int(curr) < len(prog.Instrs) && prog.Instrs[curr].Kind == ir.IRFn {
			params = append(params, token.StringID(prog.Instrs[curr].Data))
			curr = ir.InstrID(prog.Instrs[curr].Arg0)
		}
		bodyID := curr
		if len(params) == 0 || bodyID == ir.NoInstr || int(bodyID) >= len(prog.Instrs) {
			continue
		}

		bodyInstrs := make(map[ir.InstrID]bool)
		definedSyms := make(map[token.StringID]bool)
		hasInnerFn := false
		collectFunctionBodyInstrs(prog, bodyID, bodyInstrs, definedSyms, &hasInnerFn)

		// Ensure neither fnSym nor any parameter is shadowed by a local let inside bodyID
		if definedSyms[fnSym] {
			continue
		}
		shadowed := false
		for _, p := range params {
			if p != token.NoString && definedSyms[p] {
				shadowed = true
				break
			}
		}
		if shadowed {
			continue
		}

		for candID := range bodyInstrs {
			cand := prog.Instrs[candID]
			if cand.Kind != ir.IRApp || cand.Flags&ir.IRFlagTailCall == 0 {
				continue
			}
			var revArgs []ir.InstrID
			spine := candID
			for spine != ir.NoInstr && int(spine) < len(prog.Instrs) && prog.Instrs[spine].Kind == ir.IRApp {
				revArgs = append(revArgs, ir.InstrID(prog.Instrs[spine].Arg1))
				spine = ir.InstrID(prog.Instrs[spine].Arg0)
			}
			if spine == ir.NoInstr || int(spine) >= len(prog.Instrs) {
				continue
			}
			head := prog.Instrs[spine]
			if head.Kind != ir.IRVar || token.StringID(head.Data) != fnSym || len(revArgs) != len(params) {
				continue
			}
			argIDs := make([]ir.InstrID, len(revArgs))
			for j := range revArgs {
				argIDs[j] = revArgs[len(revArgs)-1-j]
			}
			out[candID] = &selfTailCallInfo{
				fnSym:      fnSym,
				params:     params,
				argIDs:     argIDs,
				targetBody: bodyID,
				inPlaceEnv: !hasInnerFn,
			}
		}
	}
	return out
}

func collectFunctionBodyInstrs(prog *ir.Program, id ir.InstrID, visited map[ir.InstrID]bool, definedSyms map[token.StringID]bool, hasInnerFn *bool) {
	if id == ir.NoInstr || int(id) >= len(prog.Instrs) || visited[id] {
		return
	}
	visited[id] = true
	ins := prog.Instrs[id]

	switch ins.Kind {
	case ir.IRConst, ir.IRVar:
		return
	case ir.IRLet:
		if sym := token.StringID(ins.Data); sym != token.NoString {
			definedSyms[sym] = true
		}
		collectFunctionBodyInstrs(prog, ir.InstrID(ins.Arg0), visited, definedSyms, hasInnerFn)
		if ir.InstrID(ins.Arg1) != ir.NoInstr {
			collectFunctionBodyInstrs(prog, ir.InstrID(ins.Arg1), visited, definedSyms, hasInnerFn)
		}
	case ir.IRFn:
		*hasInnerFn = true
	case ir.IRApp:
		collectFunctionBodyInstrs(prog, ir.InstrID(ins.Arg0), visited, definedSyms, hasInnerFn)
		collectFunctionBodyInstrs(prog, ir.InstrID(ins.Arg1), visited, definedSyms, hasInnerFn)
	case ir.IRSwitch:
		collectFunctionBodyInstrs(prog, ir.InstrID(ins.Arg0), visited, definedSyms, hasInnerFn)
		count := uint32(ins.Data)
		for i := uint32(0); i < count; i++ {
			target := ir.InstrID(prog.Extra[ins.Arg1+i*2+1])
			collectFunctionBodyInstrs(prog, target, visited, definedSyms, hasInnerFn)
		}
		collectFunctionBodyInstrs(prog, ir.InstrID(ins.Aux), visited, definedSyms, hasInnerFn)
	case ir.IRPrim:
		if ins.SubOp == ir.PrimSeqBlock {
			for i := uint32(0); i < ins.Arg1; i++ {
				ch := ir.InstrID(prog.Extra[ins.Arg0+i])
				collectFunctionBodyInstrs(prog, ch, visited, definedSyms, hasInnerFn)
			}
			return
		}
		if ins.SubOp == ir.PrimForIn || ins.SubOp == ir.PrimForTo {
			if sym := token.StringID(ins.Data); sym != token.NoString {
				definedSyms[sym] = true
			}
		}
		collectFunctionBodyInstrs(prog, ir.InstrID(ins.Arg0), visited, definedSyms, hasInnerFn)
		collectFunctionBodyInstrs(prog, ir.InstrID(ins.Arg1), visited, definedSyms, hasInnerFn)
	case ir.IRAlloc:
		if ins.SubOp == ir.AllocADT {
			collectFunctionBodyInstrs(prog, ir.InstrID(ins.Arg0), visited, definedSyms, hasInnerFn)
			return
		}
		if ins.SubOp == ir.AllocRecord {
			if ins.Flags != 0 {
				collectFunctionBodyInstrs(prog, ir.InstrID(ins.Aux), visited, definedSyms, hasInnerFn)
			}
			for i := uint32(0); i < ins.Arg1; i += 2 {
				valID := ir.InstrID(prog.Extra[ins.Arg0+i+1])
				collectFunctionBodyInstrs(prog, valID, visited, definedSyms, hasInnerFn)
			}
			return
		}
		for i := uint32(0); i < ins.Arg1; i++ {
			ch := ir.InstrID(prog.Extra[ins.Arg0+i])
			collectFunctionBodyInstrs(prog, ch, visited, definedSyms, hasInnerFn)
		}
	case ir.IRLoad:
		collectFunctionBodyInstrs(prog, ir.InstrID(ins.Arg0), visited, definedSyms, hasInnerFn)
	case ir.IRStore:
		collectFunctionBodyInstrs(prog, ir.InstrID(ins.Arg0), visited, definedSyms, hasInnerFn)
		collectFunctionBodyInstrs(prog, ir.InstrID(ins.Arg1), visited, definedSyms, hasInnerFn)
		collectFunctionBodyInstrs(prog, ir.InstrID(ins.Aux), visited, definedSyms, hasInnerFn)
	}
}

type scalarKind uint8

const (
	scalarUnknown scalarKind = iota
	scalarInt
	scalarFloat
	scalarBool
	scalarString
	scalarConflict
)

func joinScalar(a, b scalarKind) scalarKind {
	if a == scalarUnknown {
		return b
	}
	if b == scalarUnknown {
		return a
	}
	if a == b {
		return a
	}
	return scalarConflict
}

type compKind uint8

const (
	compNone compKind = iota
	compTuple
	compRecord
	compADT
	compList
	compArray
)

var canonicalPayloadScalars = [...]scalarKind{scalarInt, scalarFloat, scalarBool, scalarString}

type aotCompositeShape struct {
	kind       compKind
	components []scalarKind                  // Flattened unboxed Go scalar types
	fieldIDs   []token.StringID              // For compRecord: sorted field StringIDs
	adtTags    map[token.StringID]scalarKind // For compADT: constructor StringID -> payload scalarKind (scalarUnknown if no payload)
}

func newADTCompositeShape(tags map[token.StringID]scalarKind) *aotCompositeShape {
	copied := make(map[token.StringID]scalarKind, len(tags))
	presentKinds := make(map[scalarKind]bool, 4)
	for tag, pk := range tags {
		copied[tag] = pk
		if isNativeScalarKind(pk) {
			presentKinds[pk] = true
		}
	}
	comps := []scalarKind{scalarInt}
	for _, pk := range canonicalPayloadScalars {
		if presentKinds[pk] {
			comps = append(comps, pk)
		}
	}
	return &aotCompositeShape{
		kind:       compADT,
		components: comps,
		adtTags:    copied,
	}
}

func (s *aotCompositeShape) adtPayloadSlot(pk scalarKind) int {
	if s == nil || s.kind != compADT || !isNativeScalarKind(pk) {
		return -1
	}
	for i := 1; i < len(s.components); i++ {
		if s.components[i] == pk {
			return i
		}
	}
	return -1
}

func (s *aotCompositeShape) singleADTPayloadKind() scalarKind {
	if s == nil || s.kind != compADT || len(s.components) != 2 {
		return scalarUnknown
	}
	return s.components[1]
}

func sameCompositeShape(a, b *aotCompositeShape) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil || a.kind != b.kind || len(a.components) != len(b.components) || len(a.fieldIDs) != len(b.fieldIDs) || len(a.adtTags) != len(b.adtTags) {
		return false
	}
	for i := range a.components {
		if a.components[i] != b.components[i] {
			return false
		}
	}
	for i := range a.fieldIDs {
		if a.fieldIDs[i] != b.fieldIDs[i] {
			return false
		}
	}
	for k, v := range a.adtTags {
		if bv, ok := b.adtTags[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

func mergeCompositeShapes(a, b *aotCompositeShape) (*aotCompositeShape, bool) {
	if a == nil {
		return b, true
	}
	if b == nil {
		return a, true
	}
	if a.kind != b.kind {
		return nil, false
	}
	switch a.kind {
	case compTuple:
		if len(a.components) != len(b.components) {
			return nil, false
		}
		comps := make([]scalarKind, len(a.components))
		for i := range a.components {
			m := joinScalar(a.components[i], b.components[i])
			if !isNativeScalarKind(m) {
				return nil, false
			}
			comps[i] = m
		}
		return &aotCompositeShape{kind: compTuple, components: comps}, true

	case compRecord:
		if len(a.fieldIDs) != len(b.fieldIDs) {
			return nil, false
		}
		comps := make([]scalarKind, len(a.components))
		for i := range a.fieldIDs {
			if a.fieldIDs[i] != b.fieldIDs[i] {
				return nil, false
			}
			m := joinScalar(a.components[i], b.components[i])
			if !isNativeScalarKind(m) {
				return nil, false
			}
			comps[i] = m
		}
		return &aotCompositeShape{kind: compRecord, components: comps, fieldIDs: a.fieldIDs}, true

	case compADT:
		mergedTags := make(map[token.StringID]scalarKind, len(a.adtTags)+len(b.adtTags))
		for tag, pk := range a.adtTags {
			mergedTags[tag] = pk
		}
		for tag, pk := range b.adtTags {
			if existing, exists := mergedTags[tag]; exists {
				m := joinScalar(existing, pk)
				if m == scalarConflict {
					return nil, false
				}
				mergedTags[tag] = m
			} else {
				mergedTags[tag] = pk
			}
		}
		return newADTCompositeShape(mergedTags), true

	case compList, compArray:
		if len(a.components) != 1 || len(b.components) != 1 {
			return nil, false
		}
		m := joinScalar(a.components[0], b.components[0])
		if m == scalarConflict {
			return nil, false
		}
		return &aotCompositeShape{kind: a.kind, components: []scalarKind{m}}, true
	}
	return nil, false
}

func isNativeSliceShape(sh *aotCompositeShape) bool {
	return sh != nil && (sh.kind == compList || sh.kind == compArray) && len(sh.components) == 1 && isNativeScalarKind(sh.components[0])
}

func goTypeForCompositeComponent(sh *aotCompositeShape, idx int) string {
	if sh != nil && (sh.kind == compList || sh.kind == compArray) {
		return "[]" + goTypeForScalar(sh.components[0])
	}
	return goTypeForScalar(sh.components[idx])
}

func zeroLiteralForCompositeComponent(sh *aotCompositeShape, idx int) string {
	if sh != nil && (sh.kind == compList || sh.kind == compArray) {
		return fmt.Sprintf("[]%s(nil)", goTypeForScalar(sh.components[0]))
	}
	return zeroLiteralForScalar(sh.components[idx])
}

func resolveLambdaChain(prog *ir.Program, id ir.InstrID, info *aotScalarInfo) ([]ir.InstrID, ir.InstrID, bool) {
	if prog == nil || id == ir.NoInstr || int(id) >= len(prog.Instrs) {
		return nil, ir.NoInstr, false
	}
	curr := id
	if prog.Instrs[curr].Kind == ir.IRVar && info != nil {
		if defID, ok := info.varDef[curr]; ok && defID != ir.NoInstr && int(defID) < len(prog.Instrs) && prog.Instrs[defID].Kind == ir.IRLet {
			curr = ir.InstrID(prog.Instrs[defID].Arg0)
		}
	}
	var paramDefs []ir.InstrID
	for curr != ir.NoInstr && int(curr) < len(prog.Instrs) && prog.Instrs[curr].Kind == ir.IRFn {
		paramDefs = append(paramDefs, curr)
		curr = ir.InstrID(prog.Instrs[curr].Arg0)
	}
	if len(paramDefs) == 0 || curr == ir.NoInstr || int(curr) >= len(prog.Instrs) {
		return nil, ir.NoInstr, false
	}
	return paramDefs, curr, true
}

type aotNativeFn struct {
	letID       ir.InstrID
	name        string
	paramDefs   []ir.InstrID
	paramKinds  []scalarKind
	bodyID      ir.InstrID
	retKind     scalarKind
	retComp     *aotCompositeShape
	bodyCode    string
	hasTailLoop bool
}

type aotScalarInfo struct {
	instr      []scalarKind
	def        map[ir.InstrID]scalarKind
	compInstr  []*aotCompositeShape
	compDef    map[ir.InstrID]*aotCompositeShape
	adtCaseTag map[ir.InstrID]token.StringID
	varDef     map[ir.InstrID]ir.InstrID
	storeDef   map[ir.InstrID]ir.InstrID
	nativeFns  map[ir.InstrID]*aotNativeFn
}

func emitAOTScalarHelpers(buf *bytes.Buffer) {
	buf.WriteString("func aotLoadValue(vm *runtime.VM, env map[token.StringID]runtime.Value, sym token.StringID) runtime.Value {\n")
	buf.WriteString("\tif v, ok := env[sym]; ok {\n")
	buf.WriteString("\t\treturn runtime.Deref(v)\n")
	buf.WriteString("\t}\n")
	buf.WriteString("\tif v, ok := vm.Globals[sym]; ok {\n")
	buf.WriteString("\t\treturn runtime.Deref(v)\n")
	buf.WriteString("\t}\n")
	buf.WriteString("\treturn runtime.Unit\n")
	buf.WriteString("}\n\n")

	buf.WriteString("func aotLoadADTTag(vm *runtime.VM, env map[token.StringID]runtime.Value, sym token.StringID) int64 {\n")
	buf.WriteString("\tif v, ok := env[sym]; ok {\n")
	buf.WriteString("\t\tif adt, ok := runtime.Deref(v).(runtime.ADTVal); ok {\n")
	buf.WriteString("\t\t\treturn int64(adt.Tag)\n")
	buf.WriteString("\t\t}\n")
	buf.WriteString("\t}\n")
	buf.WriteString("\tif v, ok := vm.Globals[sym]; ok {\n")
	buf.WriteString("\t\tif adt, ok := runtime.Deref(v).(runtime.ADTVal); ok {\n")
	buf.WriteString("\t\t\treturn int64(adt.Tag)\n")
	buf.WriteString("\t\t}\n")
	buf.WriteString("\t}\n")
	buf.WriteString("\treturn 0\n")
	buf.WriteString("}\n\n")

	buf.WriteString("func aotLoadInt(vm *runtime.VM, env map[token.StringID]runtime.Value, sym token.StringID) int64 {\n")
	buf.WriteString("\tif v, ok := env[sym]; ok {\n")
	buf.WriteString("\t\tif iv, ok := runtime.Deref(v).(runtime.IntVal); ok {\n")
	buf.WriteString("\t\t\treturn iv.V\n")
	buf.WriteString("\t\t}\n")
	buf.WriteString("\t}\n")
	buf.WriteString("\tif v, ok := vm.Globals[sym]; ok {\n")
	buf.WriteString("\t\tif iv, ok := runtime.Deref(v).(runtime.IntVal); ok {\n")
	buf.WriteString("\t\t\treturn iv.V\n")
	buf.WriteString("\t\t}\n")
	buf.WriteString("\t}\n")
	buf.WriteString("\treturn 0\n")
	buf.WriteString("}\n\n")

	buf.WriteString("func aotLoadFloat(vm *runtime.VM, env map[token.StringID]runtime.Value, sym token.StringID) float64 {\n")
	buf.WriteString("\tif v, ok := env[sym]; ok {\n")
	buf.WriteString("\t\tif fv, ok := runtime.Deref(v).(runtime.FloatVal); ok {\n")
	buf.WriteString("\t\t\treturn fv.V\n")
	buf.WriteString("\t\t}\n")
	buf.WriteString("\t}\n")
	buf.WriteString("\tif v, ok := vm.Globals[sym]; ok {\n")
	buf.WriteString("\t\tif fv, ok := runtime.Deref(v).(runtime.FloatVal); ok {\n")
	buf.WriteString("\t\t\treturn fv.V\n")
	buf.WriteString("\t\t}\n")
	buf.WriteString("\t}\n")
	buf.WriteString("\treturn 0\n")
	buf.WriteString("}\n\n")

	buf.WriteString("func aotLoadBool(vm *runtime.VM, env map[token.StringID]runtime.Value, sym token.StringID) bool {\n")
	buf.WriteString("\tif v, ok := env[sym]; ok {\n")
	buf.WriteString("\t\tif bv, ok := runtime.Deref(v).(runtime.BoolVal); ok {\n")
	buf.WriteString("\t\t\treturn bv.V\n")
	buf.WriteString("\t\t}\n")
	buf.WriteString("\t}\n")
	buf.WriteString("\tif v, ok := vm.Globals[sym]; ok {\n")
	buf.WriteString("\t\tif bv, ok := runtime.Deref(v).(runtime.BoolVal); ok {\n")
	buf.WriteString("\t\t\treturn bv.V\n")
	buf.WriteString("\t\t}\n")
	buf.WriteString("\t}\n")
	buf.WriteString("\treturn false\n")
	buf.WriteString("}\n\n")

	buf.WriteString("func aotLoadString(vm *runtime.VM, env map[token.StringID]runtime.Value, sym token.StringID) string {\n")
	buf.WriteString("\tif v, ok := env[sym]; ok {\n")
	buf.WriteString("\t\tif sv, ok := runtime.Deref(v).(runtime.StringVal); ok {\n")
	buf.WriteString("\t\t\treturn sv.V\n")
	buf.WriteString("\t\t}\n")
	buf.WriteString("\t}\n")
	buf.WriteString("\tif v, ok := vm.Globals[sym]; ok {\n")
	buf.WriteString("\t\tif sv, ok := runtime.Deref(v).(runtime.StringVal); ok {\n")
	buf.WriteString("\t\t\treturn sv.V\n")
	buf.WriteString("\t\t}\n")
	buf.WriteString("\t}\n")
	buf.WriteString("\treturn \"\"\n")
	buf.WriteString("}\n\n")

	buf.WriteString("func aotSubstring(start, length int64, s string) string {\n")
	buf.WriteString("\tif start < 0 {\n\t\tstart = 0\n\t}\n")
	buf.WriteString("\tif int(start) >= len(s) || length <= 0 {\n\t\treturn \"\"\n\t}\n")
	buf.WriteString("\tend := int(start + length)\n")
	buf.WriteString("\tif end > len(s) {\n\t\tend = len(s)\n\t}\n")
	buf.WriteString("\treturn s[int(start):end]\n")
	buf.WriteString("}\n\n")

	buf.WriteString("func aotAbsInt(x int64) int64 {\n\tif x < 0 {\n\t\treturn -x\n\t}\n\treturn x\n}\n\n")
	buf.WriteString("func aotMinInt(a, b int64) int64 {\n\tif a < b {\n\t\treturn a\n\t}\n\treturn b\n}\n\n")
	buf.WriteString("func aotMaxInt(a, b int64) int64 {\n\tif a > b {\n\t\treturn a\n\t}\n\treturn b\n}\n\n")
	buf.WriteString("func aotRangeInt(start, end int64, flags uint8) []int64 {\n")
	buf.WriteString("\tif flags != 0 && start >= end {\n")
	buf.WriteString("\t\tn := int(start - end + 1)\n")
	buf.WriteString("\t\tout := make([]int64, n)\n")
	buf.WriteString("\t\tfor i := 0; i < n; i++ {\n\t\t\tout[i] = start - int64(i)\n\t\t}\n")
	buf.WriteString("\t\treturn out\n")
	buf.WriteString("\t}\n")
	buf.WriteString("\tif end < start {\n\t\treturn nil\n\t}\n")
	buf.WriteString("\tn := int(end - start + 1)\n")
	buf.WriteString("\tout := make([]int64, n)\n")
	buf.WriteString("\tfor i := 0; i < n; i++ {\n\t\tout[i] = start + int64(i)\n\t}\n")
	buf.WriteString("\treturn out\n")
	buf.WriteString("}\n\n")
}

func resolveMathSymbol(prog *ir.Program, id ir.InstrID) string {
	if prog == nil || id == ir.NoInstr || int(id) >= len(prog.Instrs) {
		return ""
	}
	ins := prog.Instrs[id]
	if ins.Kind == ir.IRLoad && ins.SubOp == ir.LoadRecordField && ins.Aux != 0 {
		return prog.Strings.String(token.StringID(ins.Aux))
	}
	if ins.Kind == ir.IRVar && ins.Data != 0 {
		return prog.Strings.String(token.StringID(ins.Data))
	}
	return ""
}

// analyzeAOTScalars resolves lexical variable bindings and performs fixed-point
// scalar and composite type propagation across prog so pure int64, float64,
// bool, string, Tuple, Record, Option, and Result expressions and functions
// can be emitted as unboxed native Go code.
func analyzeAOTScalars(prog *ir.Program) *aotScalarInfo {
	info := &aotScalarInfo{
		instr:      make([]scalarKind, len(prog.Instrs)),
		def:        make(map[ir.InstrID]scalarKind),
		compInstr:  make([]*aotCompositeShape, len(prog.Instrs)),
		compDef:    make(map[ir.InstrID]*aotCompositeShape),
		adtCaseTag: make(map[ir.InstrID]token.StringID),
		varDef:     make(map[ir.InstrID]ir.InstrID),
		storeDef:   make(map[ir.InstrID]ir.InstrID),
	}
	if prog == nil || len(prog.Instrs) == 0 || prog.Root == ir.NoInstr {
		return info
	}

	resolveLexicalDefs(prog, prog.Root, make(map[token.StringID]ir.InstrID), info, make(map[ir.InstrID]bool))
	markADTSwitchCasePayloads(prog, info)

	type fnSig struct {
		paramDefs []ir.InstrID
		bodyID    ir.InstrID
	}
	fnSigs := make(map[ir.InstrID]fnSig)
	for i, ins := range prog.Instrs {
		if ins.Kind != ir.IRLet {
			continue
		}
		var paramDefs []ir.InstrID
		curr := ir.InstrID(ins.Arg0)
		for curr != ir.NoInstr && int(curr) < len(prog.Instrs) && prog.Instrs[curr].Kind == ir.IRFn {
			paramDefs = append(paramDefs, curr)
			curr = ir.InstrID(prog.Instrs[curr].Arg0)
		}
		if len(paramDefs) > 0 && curr != ir.NoInstr && int(curr) < len(prog.Instrs) {
			fnSigs[ir.InstrID(i)] = fnSig{
				paramDefs: paramDefs,
				bodyID:    curr,
			}
		}
	}

	setInstr := func(id ir.InstrID, k scalarKind) bool {
		if id == ir.NoInstr || int(id) >= len(info.instr) || k == scalarUnknown {
			return false
		}
		merged := joinScalar(info.instr[id], k)
		if merged != info.instr[id] {
			info.instr[id] = merged
			return true
		}
		return false
	}

	setDef := func(id ir.InstrID, k scalarKind) bool {
		if id == ir.NoInstr || k == scalarUnknown {
			return false
		}
		merged := joinScalar(info.def[id], k)
		if merged != info.def[id] {
			info.def[id] = merged
			return true
		}
		return false
	}

	setCompInstr := func(id ir.InstrID, sh *aotCompositeShape) bool {
		if id == ir.NoInstr || int(id) >= len(info.compInstr) || sh == nil {
			return false
		}
		merged, ok := mergeCompositeShapes(info.compInstr[id], sh)
		if !ok {
			if info.compInstr[id] != nil {
				info.compInstr[id] = nil
				return true
			}
			return false
		}
		if !sameCompositeShape(info.compInstr[id], merged) {
			info.compInstr[id] = merged
			return true
		}
		return false
	}

	setCompDef := func(id ir.InstrID, sh *aotCompositeShape) bool {
		if id == ir.NoInstr || sh == nil {
			return false
		}
		merged, ok := mergeCompositeShapes(info.compDef[id], sh)
		if !ok {
			if info.compDef[id] != nil {
				delete(info.compDef, id)
				return true
			}
			return false
		}
		if !sameCompositeShape(info.compDef[id], merged) {
			info.compDef[id] = merged
			return true
		}
		return false
	}

	for pass := 0; pass < 10; pass++ {
		changed := false
		for i, ins := range prog.Instrs {
			id := ir.InstrID(i)
			switch ins.Kind {
			case ir.IRConst:
				switch ins.SubOp {
				case ir.ConstInt:
					changed = setInstr(id, scalarInt) || changed
				case ir.ConstFloat:
					changed = setInstr(id, scalarFloat) || changed
				case ir.ConstBool:
					changed = setInstr(id, scalarBool) || changed
				case ir.ConstString:
					changed = setInstr(id, scalarString) || changed
				}

			case ir.IRFn:
				changed = setInstr(id, scalarConflict) || changed

			case ir.IRAlloc:
				changed = setInstr(id, scalarConflict) || changed
				switch ins.SubOp {
				case ir.AllocTuple:
					if ins.Arg1 > 0 && int(ins.Arg0+ins.Arg1) <= len(prog.Extra) {
						comps := make([]scalarKind, ins.Arg1)
						allOK := true
						for j := uint32(0); j < ins.Arg1; j++ {
							ch := ir.InstrID(prog.Extra[ins.Arg0+j])
							if int(ch) >= len(info.instr) || !isNativeScalarKind(info.instr[ch]) {
								allOK = false
								break
							}
							comps[j] = info.instr[ch]
						}
						if allOK {
							changed = setCompInstr(id, &aotCompositeShape{kind: compTuple, components: comps}) || changed
						}
					}

				case ir.AllocRecord:
					if int(ins.Arg0+ins.Arg1) <= len(prog.Extra) {
						fieldMap := make(map[token.StringID]scalarKind, ins.Arg1/2+4)
						allOK := true
						if ins.Flags != 0 {
							if int(ins.Aux) < len(info.compInstr) && info.compInstr[ins.Aux] != nil && info.compInstr[ins.Aux].kind == compRecord {
								baseSh := info.compInstr[ins.Aux]
								for idx, fID := range baseSh.fieldIDs {
									fieldMap[fID] = baseSh.components[idx]
								}
							} else {
								allOK = false
							}
						}
						for j := uint32(0); allOK && j+1 < ins.Arg1; j += 2 {
							fID := token.StringID(prog.Extra[ins.Arg0+j])
							valID := ir.InstrID(prog.Extra[ins.Arg0+j+1])
							if int(valID) >= len(info.instr) || !isNativeScalarKind(info.instr[valID]) {
								allOK = false
								break
							}
							fieldMap[fID] = info.instr[valID]
						}
						if allOK && len(fieldMap) > 0 {
							fIDs := make([]token.StringID, 0, len(fieldMap))
							for fID := range fieldMap {
								fIDs = append(fIDs, fID)
							}
							sort.Slice(fIDs, func(a, b int) bool { return fIDs[a] < fIDs[b] })
							comps := make([]scalarKind, len(fIDs))
							for idx, fID := range fIDs {
								comps[idx] = fieldMap[fID]
							}
							changed = setCompInstr(id, &aotCompositeShape{kind: compRecord, components: comps, fieldIDs: fIDs}) || changed
						}
					}

				case ir.AllocADT:
					tag := token.StringID(ins.Data)
					if ir.InstrID(ins.Arg0) == ir.NoInstr {
						changed = setCompInstr(id, newADTCompositeShape(map[token.StringID]scalarKind{tag: scalarUnknown})) || changed
					} else if int(ins.Arg0) < len(info.instr) && isNativeScalarKind(info.instr[ins.Arg0]) {
						pk := info.instr[ins.Arg0]
						changed = setCompInstr(id, newADTCompositeShape(map[token.StringID]scalarKind{tag: pk})) || changed
					}

				case ir.AllocList, ir.AllocArray:
					cKind := compList
					if ins.SubOp == ir.AllocArray {
						cKind = compArray
					}
					if ins.Arg1 == 0 {
						changed = setCompInstr(id, &aotCompositeShape{kind: cKind, components: []scalarKind{scalarUnknown}}) || changed
					} else if int(ins.Arg0+ins.Arg1) <= len(prog.Extra) {
						var elemK scalarKind
						allOK := true
						for j := uint32(0); j < ins.Arg1; j++ {
							ch := ir.InstrID(prog.Extra[ins.Arg0+j])
							if int(ch) >= len(info.instr) || !isNativeScalarKind(info.instr[ch]) {
								allOK = false
								break
							}
							elemK = joinScalar(elemK, info.instr[ch])
						}
						if allOK && isNativeScalarKind(elemK) {
							changed = setCompInstr(id, &aotCompositeShape{kind: cKind, components: []scalarKind{elemK}}) || changed
						}
					}
				}

			case ir.IRApp:
				var revArgs []ir.InstrID
				spine := id
				for spine != ir.NoInstr && int(spine) < len(prog.Instrs) && prog.Instrs[spine].Kind == ir.IRApp {
					revArgs = append(revArgs, ir.InstrID(prog.Instrs[spine].Arg1))
					spine = ir.InstrID(prog.Instrs[spine].Arg0)
				}
				switch resolveMathSymbol(prog, spine) {
				case "Math.sqrt", "Math.sin", "Math.cos", "Math.floor", "Math.ceil", "Math.round":
					if len(revArgs) == 1 {
						changed = setInstr(revArgs[0], scalarFloat) || changed
						changed = setInstr(id, scalarFloat) || changed
					}
				case "Math.abs":
					if len(revArgs) == 1 && int(revArgs[0]) < len(info.instr) {
						m := joinScalar(info.instr[id], info.instr[revArgs[0]])
						if m == scalarInt || m == scalarFloat || m == scalarConflict {
							changed = setInstr(id, m) || changed
							changed = setInstr(revArgs[0], m) || changed
						}
					}
				case "Math.pow":
					if len(revArgs) == 2 {
						changed = setInstr(revArgs[1], scalarFloat) || changed
						changed = setInstr(revArgs[0], scalarFloat) || changed
						changed = setInstr(id, scalarFloat) || changed
					}
				case "Math.min", "Math.max":
					if len(revArgs) == 2 && int(revArgs[1]) < len(info.instr) && int(revArgs[0]) < len(info.instr) {
						m := joinScalar(info.instr[id], joinScalar(info.instr[revArgs[1]], info.instr[revArgs[0]]))
						if m == scalarInt || m == scalarFloat || m == scalarConflict {
							changed = setInstr(id, m) || changed
							changed = setInstr(revArgs[1], m) || changed
							changed = setInstr(revArgs[0], m) || changed
						}
					}
				case "String.length":
					if len(revArgs) == 1 {
						changed = setInstr(revArgs[0], scalarString) || changed
						changed = setInstr(id, scalarInt) || changed
					}
				case "String.trim", "String.toUpper", "String.toLower":
					if len(revArgs) == 1 {
						changed = setInstr(revArgs[0], scalarString) || changed
						changed = setInstr(id, scalarString) || changed
					}
				case "String.contains", "String.startsWith", "String.endsWith":
					if len(revArgs) == 2 {
						changed = setInstr(revArgs[1], scalarString) || changed
						changed = setInstr(revArgs[0], scalarString) || changed
						changed = setInstr(id, scalarBool) || changed
					}
				case "String.indexOf":
					if len(revArgs) == 2 {
						changed = setInstr(revArgs[1], scalarString) || changed
						changed = setInstr(revArgs[0], scalarString) || changed
						changed = setInstr(id, scalarInt) || changed
					}
				case "String.replace":
					if len(revArgs) == 3 {
						changed = setInstr(revArgs[2], scalarString) || changed
						changed = setInstr(revArgs[1], scalarString) || changed
						changed = setInstr(revArgs[0], scalarString) || changed
						changed = setInstr(id, scalarString) || changed
					}
				case "String.substring":
					if len(revArgs) == 3 {
						changed = setInstr(revArgs[2], scalarInt) || changed
						changed = setInstr(revArgs[1], scalarInt) || changed
						changed = setInstr(revArgs[0], scalarString) || changed
						changed = setInstr(id, scalarString) || changed
					}
				case "Option.isSome", "Option.isNone", "Result.isOk", "Result.isError":
					if len(revArgs) == 1 {
						changed = setInstr(id, scalarBool) || changed
					}
				case "Option.defaultValue", "Result.defaultValue":
					if len(revArgs) == 2 && int(revArgs[1]) < len(info.instr) {
						defKind := info.instr[revArgs[1]]
						if isNativeScalarKind(defKind) {
							changed = setInstr(id, defKind) || changed
						}
					}
				case "List.length", "Array.length":
					if len(revArgs) == 1 {
						changed = setInstr(id, scalarInt) || changed
					}
				case "List.isEmpty":
					if len(revArgs) == 1 {
						changed = setInstr(id, scalarBool) || changed
					}
				case "List.head":
					if len(revArgs) == 1 && int(revArgs[0]) < len(info.compInstr) && isNativeSliceShape(info.compInstr[revArgs[0]]) {
						changed = setInstr(id, info.compInstr[revArgs[0]].components[0]) || changed
					}
				case "List.tail", "List.rev", "Array.rev":
					if len(revArgs) == 1 && int(revArgs[0]) < len(info.compInstr) && isNativeSliceShape(info.compInstr[revArgs[0]]) {
						changed = setInstr(id, scalarConflict) || changed
						changed = setCompInstr(id, info.compInstr[revArgs[0]]) || changed
					}
				case "Array.get":
					if len(revArgs) == 2 {
						changed = setInstr(revArgs[0], scalarInt) || changed
						if int(revArgs[1]) < len(info.compInstr) && isNativeSliceShape(info.compInstr[revArgs[1]]) {
							changed = setInstr(id, info.compInstr[revArgs[1]].components[0]) || changed
						}
					}
				case "Array.append":
					if len(revArgs) == 2 {
						changed = setInstr(id, scalarConflict) || changed
						if int(revArgs[1]) < len(info.compInstr) && isNativeSliceShape(info.compInstr[revArgs[1]]) {
							changed = setCompInstr(id, info.compInstr[revArgs[1]]) || changed
						}
						if int(revArgs[0]) < len(info.compInstr) && isNativeSliceShape(info.compInstr[revArgs[0]]) {
							changed = setCompInstr(id, info.compInstr[revArgs[0]]) || changed
						}
					}
				case "Array.ofList":
					if len(revArgs) == 1 && int(revArgs[0]) < len(info.compInstr) && isNativeSliceShape(info.compInstr[revArgs[0]]) {
						changed = setInstr(id, scalarConflict) || changed
						changed = setCompInstr(id, &aotCompositeShape{kind: compArray, components: info.compInstr[revArgs[0]].components}) || changed
					}
				case "Array.toList":
					if len(revArgs) == 1 && int(revArgs[0]) < len(info.compInstr) && isNativeSliceShape(info.compInstr[revArgs[0]]) {
						changed = setInstr(id, scalarConflict) || changed
						changed = setCompInstr(id, &aotCompositeShape{kind: compList, components: info.compInstr[revArgs[0]].components}) || changed
					}
				case "List.sum":
					if len(revArgs) == 1 && int(revArgs[0]) < len(info.compInstr) && isNativeSliceShape(info.compInstr[revArgs[0]]) {
						ek := info.compInstr[revArgs[0]].components[0]
						if ek == scalarInt || ek == scalarFloat {
							changed = setInstr(id, ek) || changed
						}
					}
				case "List.init", "Array.init":
					if len(revArgs) == 2 {
						changed = setInstr(revArgs[1], scalarInt) || changed
						if pDefs, bodyID, okL := resolveLambdaChain(prog, revArgs[0], info); okL && len(pDefs) == 1 {
							changed = setDef(pDefs[0], scalarInt) || changed
							if int(bodyID) < len(info.instr) && isNativeScalarKind(info.instr[bodyID]) {
								outKind := compList
								if resolveMathSymbol(prog, spine) == "Array.init" {
									outKind = compArray
								}
								changed = setInstr(id, scalarConflict) || changed
								changed = setCompInstr(id, &aotCompositeShape{kind: outKind, components: []scalarKind{info.instr[bodyID]}}) || changed
							}
						}
					}
				case "List.map", "Array.map":
					if len(revArgs) == 2 && int(revArgs[0]) < len(info.compInstr) && isNativeSliceShape(info.compInstr[revArgs[0]]) {
						inSh := info.compInstr[revArgs[0]]
						if pDefs, bodyID, okL := resolveLambdaChain(prog, revArgs[1], info); okL && len(pDefs) == 1 {
							changed = setDef(pDefs[0], inSh.components[0]) || changed
							if int(bodyID) < len(info.instr) && isNativeScalarKind(info.instr[bodyID]) {
								outKind := compList
								if resolveMathSymbol(prog, spine) == "Array.map" {
									outKind = compArray
								}
								changed = setInstr(id, scalarConflict) || changed
								changed = setCompInstr(id, &aotCompositeShape{kind: outKind, components: []scalarKind{info.instr[bodyID]}}) || changed
							}
						}
					}
				case "List.filter", "Array.filter":
					if len(revArgs) == 2 && int(revArgs[0]) < len(info.compInstr) && isNativeSliceShape(info.compInstr[revArgs[0]]) {
						inSh := info.compInstr[revArgs[0]]
						if pDefs, bodyID, okL := resolveLambdaChain(prog, revArgs[1], info); okL && len(pDefs) == 1 {
							changed = setDef(pDefs[0], inSh.components[0]) || changed
							changed = setInstr(bodyID, scalarBool) || changed
							changed = setInstr(id, scalarConflict) || changed
							changed = setCompInstr(id, inSh) || changed
						}
					}
				case "List.fold", "Array.fold":
					if len(revArgs) == 3 && int(revArgs[0]) < len(info.compInstr) && isNativeSliceShape(info.compInstr[revArgs[0]]) && int(revArgs[1]) < len(info.instr) {
						inSh := info.compInstr[revArgs[0]]
						accK := info.instr[revArgs[1]]
						if isNativeScalarKind(accK) {
							if pDefs, bodyID, okL := resolveLambdaChain(prog, revArgs[2], info); okL && len(pDefs) == 2 {
								changed = setDef(pDefs[0], accK) || changed
								changed = setDef(pDefs[1], inSh.components[0]) || changed
								changed = setInstr(bodyID, accK) || changed
								changed = setInstr(id, accK) || changed
							}
						}
					}
				case "List.exists", "Array.exists", "List.forall", "Array.forall":
					if len(revArgs) == 2 && int(revArgs[0]) < len(info.compInstr) && isNativeSliceShape(info.compInstr[revArgs[0]]) {
						inSh := info.compInstr[revArgs[0]]
						if pDefs, bodyID, okL := resolveLambdaChain(prog, revArgs[1], info); okL && len(pDefs) == 1 {
							changed = setDef(pDefs[0], inSh.components[0]) || changed
							changed = setInstr(bodyID, scalarBool) || changed
							changed = setInstr(id, scalarBool) || changed
						}
					}
				}
				if defID, ok := info.varDef[spine]; ok {
					if sig, okSig := fnSigs[defID]; okSig && len(revArgs) <= len(sig.paramDefs) {
						for k := 0; k < len(revArgs); k++ {
							argID := revArgs[len(revArgs)-1-k]
							if int(argID) < len(info.instr) {
								changed = setDef(sig.paramDefs[k], info.instr[argID]) || changed
							}
						}
						if len(revArgs) == len(sig.paramDefs) && int(sig.bodyID) < len(info.instr) {
							changed = setInstr(id, info.instr[sig.bodyID]) || changed
							if info.compInstr[sig.bodyID] != nil {
								changed = setCompInstr(id, info.compInstr[sig.bodyID]) || changed
							}
						}
					}
				}

			case ir.IRVar:
				if defID, ok := info.varDef[id]; ok && defID != ir.NoInstr {
					m := joinScalar(info.instr[id], info.def[defID])
					changed = setInstr(id, m) || changed
					changed = setDef(defID, m) || changed
					if sh := info.compDef[defID]; sh != nil {
						changed = setCompInstr(id, sh) || changed
					}
				} else if resolveMathSymbol(prog, id) == "Math.pi" {
					changed = setInstr(id, scalarFloat) || changed
				}

			case ir.IRLet:
				arg0 := ir.InstrID(ins.Arg0)
				if int(arg0) < len(info.instr) {
					m := joinScalar(info.def[id], info.instr[arg0])
					changed = setDef(id, m) || changed
					changed = setInstr(arg0, m) || changed
					if sh := info.compInstr[arg0]; sh != nil {
						changed = setCompDef(id, sh) || changed
					}
				}
				if ir.InstrID(ins.Arg1) != ir.NoInstr && int(ins.Arg1) < len(info.instr) {
					arg1 := ir.InstrID(ins.Arg1)
					m := joinScalar(info.instr[id], info.instr[arg1])
					changed = setInstr(id, m) || changed
					changed = setInstr(arg1, m) || changed
					if sh := info.compInstr[arg1]; sh != nil {
						changed = setCompInstr(id, sh) || changed
					}
				} else if int(arg0) < len(info.instr) {
					m := joinScalar(info.instr[id], info.instr[arg0])
					changed = setInstr(id, m) || changed
					changed = setInstr(arg0, m) || changed
					if sh := info.compInstr[arg0]; sh != nil {
						changed = setCompInstr(id, sh) || changed
					}
				}

			case ir.IRStore:
				if ins.SubOp != ir.StoreArrayElem {
					if defID, ok := info.storeDef[id]; ok && defID != ir.NoInstr && int(ins.Arg1) < len(info.instr) {
						arg1 := ir.InstrID(ins.Arg1)
						m := joinScalar(info.def[defID], info.instr[arg1])
						changed = setDef(defID, m) || changed
						changed = setInstr(arg1, m) || changed
					}
				}

			case ir.IRLoad:
				if ins.SubOp == ir.LoadADTTag || ins.SubOp == ir.LoadListIsEmpty {
					changed = setInstr(id, scalarInt) || changed
				} else if resolveMathSymbol(prog, id) == "Math.pi" {
					changed = setInstr(id, scalarFloat) || changed
				} else if int(ins.Arg0) < len(info.compInstr) && info.compInstr[ins.Arg0] != nil {
					sh := info.compInstr[ins.Arg0]
					switch ins.SubOp {
					case ir.LoadTupleElem:
						if sh.kind == compTuple && int(ins.Arg1) < len(sh.components) {
							changed = setInstr(id, sh.components[ins.Arg1]) || changed
						}
					case ir.LoadRecordField:
						if sh.kind == compRecord {
							wantField := token.StringID(ins.Data)
							for idx, fID := range sh.fieldIDs {
								if fID == wantField {
									changed = setInstr(id, sh.components[idx]) || changed
									break
								}
							}
						}
					case ir.LoadADTPayload:
						if sh.kind == compADT {
							if cTag, hasTag := info.adtCaseTag[id]; hasTag {
								if pk := sh.adtTags[cTag]; isNativeScalarKind(pk) {
									changed = setInstr(id, pk) || changed
								}
							} else if pk := sh.singleADTPayloadKind(); isNativeScalarKind(pk) {
								changed = setInstr(id, pk) || changed
							}
						}
					case ir.LoadListHead:
						if sh.kind == compList && isNativeSliceShape(sh) {
							changed = setInstr(id, sh.components[0]) || changed
						}
					case ir.LoadListTail:
						if sh.kind == compList && isNativeSliceShape(sh) {
							changed = setInstr(id, scalarConflict) || changed
							changed = setCompInstr(id, sh) || changed
						}
					}
				}

			case ir.IRSwitch:
				var branchKind scalarKind
				var branchComp *aotCompositeShape
				compValid := true
				count := uint32(ins.Data)
				for j := uint32(0); j < count; j++ {
					target := ir.InstrID(prog.Extra[ins.Arg1+j*2+1])
					if isIgnorableSwitchBranch(prog, target) {
						continue
					}
					branchKind = joinScalar(branchKind, info.instr[target])
					if compValid {
						if info.compInstr[target] == nil {
							compValid = false
						} else if mergedC, okC := mergeCompositeShapes(branchComp, info.compInstr[target]); okC {
							branchComp = mergedC
						} else {
							compValid = false
						}
					}
				}
				aux := ir.InstrID(ins.Aux)
				if !isIgnorableSwitchBranch(prog, aux) {
					branchKind = joinScalar(branchKind, info.instr[aux])
					if compValid {
						if info.compInstr[aux] == nil {
							compValid = false
						} else if mergedC, okC := mergeCompositeShapes(branchComp, info.compInstr[aux]); okC {
							branchComp = mergedC
						} else {
							compValid = false
						}
					}
				}
				merged := joinScalar(info.instr[id], branchKind)
				if merged != scalarUnknown {
					changed = setInstr(id, merged) || changed
					for j := uint32(0); j < count; j++ {
						target := ir.InstrID(prog.Extra[ins.Arg1+j*2+1])
						if !isIgnorableSwitchBranch(prog, target) {
							changed = setInstr(target, merged) || changed
						}
					}
					if !isIgnorableSwitchBranch(prog, aux) {
						changed = setInstr(aux, merged) || changed
					}
				}
				if compValid && branchComp != nil {
					changed = setCompInstr(id, branchComp) || changed
				}

			case ir.IRPrim:
				arg0 := ir.InstrID(ins.Arg0)
				arg1 := ir.InstrID(ins.Arg1)
				switch ins.SubOp {
				case ir.PrimSeqBlock:
					if ins.Arg1 > 0 {
						last := ir.InstrID(prog.Extra[ins.Arg0+ins.Arg1-1])
						if int(last) < len(info.instr) {
							m := joinScalar(info.instr[id], info.instr[last])
							changed = setInstr(id, m) || changed
							changed = setInstr(last, m) || changed
							if sh := info.compInstr[last]; sh != nil {
								changed = setCompInstr(id, sh) || changed
							}
						}
					}
				case ir.PrimForTo:
					changed = setDef(id, scalarInt) || changed
				case ir.PrimMod:
					changed = setInstr(id, scalarInt) || changed
					changed = setInstr(arg0, scalarInt) || changed
					changed = setInstr(arg1, scalarInt) || changed
				case ir.PrimNot:
					changed = setInstr(id, scalarBool) || changed
					changed = setInstr(arg0, scalarBool) || changed
				case ir.PrimAnd, ir.PrimOr:
					changed = setInstr(id, scalarBool) || changed
					changed = setInstr(arg0, scalarBool) || changed
					changed = setInstr(arg1, scalarBool) || changed
				case ir.PrimNeg:
					if int(arg0) < len(info.instr) {
						m := joinScalar(info.instr[id], info.instr[arg0])
						if m == scalarInt || m == scalarFloat || m == scalarConflict {
							changed = setInstr(id, m) || changed
							changed = setInstr(arg0, m) || changed
						}
					}
				case ir.PrimSub, ir.PrimMul, ir.PrimDiv:
					if int(arg0) < len(info.instr) && int(arg1) < len(info.instr) {
						m := joinScalar(info.instr[id], joinScalar(info.instr[arg0], info.instr[arg1]))
						if m == scalarInt || m == scalarFloat || m == scalarConflict {
							changed = setInstr(id, m) || changed
							changed = setInstr(arg0, m) || changed
							changed = setInstr(arg1, m) || changed
						}
					}
				case ir.PrimAdd:
					if int(arg0) < len(info.instr) && int(arg1) < len(info.instr) {
						m := joinScalar(info.instr[id], joinScalar(info.instr[arg0], info.instr[arg1]))
						if m == scalarInt || m == scalarFloat || m == scalarString || m == scalarConflict {
							changed = setInstr(id, m) || changed
							changed = setInstr(arg0, m) || changed
							changed = setInstr(arg1, m) || changed
						}
					}
				case ir.PrimLt, ir.PrimLe, ir.PrimGt, ir.PrimGe, ir.PrimEq, ir.PrimNeq:
					changed = setInstr(id, scalarBool) || changed
					if int(arg0) < len(info.instr) && int(arg1) < len(info.instr) {
						opKind := joinScalar(info.instr[arg0], info.instr[arg1])
						if opKind != scalarUnknown {
							changed = setInstr(arg0, opKind) || changed
							changed = setInstr(arg1, opKind) || changed
						}
					}
				case ir.PrimCons:
					changed = setInstr(id, scalarConflict) || changed
					if int(arg0) < len(info.instr) && isNativeScalarKind(info.instr[arg0]) {
						sh := &aotCompositeShape{kind: compList, components: []scalarKind{info.instr[arg0]}}
						changed = setCompInstr(id, sh) || changed
						changed = setCompInstr(arg1, sh) || changed
					}
				case ir.PrimAppend:
					changed = setInstr(id, scalarConflict) || changed
					if int(arg0) < len(info.compInstr) && isNativeSliceShape(info.compInstr[arg0]) {
						changed = setCompInstr(id, info.compInstr[arg0]) || changed
						changed = setCompInstr(arg1, info.compInstr[arg0]) || changed
					}
					if int(arg1) < len(info.compInstr) && isNativeSliceShape(info.compInstr[arg1]) {
						changed = setCompInstr(id, info.compInstr[arg1]) || changed
						changed = setCompInstr(arg0, info.compInstr[arg1]) || changed
					}
				case ir.PrimRange:
					changed = setInstr(id, scalarConflict) || changed
					changed = setInstr(arg0, scalarInt) || changed
					changed = setInstr(arg1, scalarInt) || changed
					changed = setCompInstr(id, &aotCompositeShape{kind: compList, components: []scalarKind{scalarInt}}) || changed
				}
			}
		}
		if !changed {
			break
		}
	}

	candidates := make(map[ir.InstrID]*aotNativeFn)
	for letID, sig := range fnSigs {
		retKind := info.instr[sig.bodyID]
		retComp := info.compInstr[sig.bodyID]
		if retComp != nil && (retComp.kind == compList || retComp.kind == compArray) && !isNativeSliceShape(retComp) {
			retComp = nil
		}
		if !isNativeScalarKind(retKind) && retComp == nil {
			continue
		}
		paramKinds := make([]scalarKind, len(sig.paramDefs))
		allScalar := true
		for k, pDef := range sig.paramDefs {
			pk := info.def[pDef]
			if !isNativeScalarKind(pk) {
				allScalar = false
				break
			}
			paramKinds[k] = pk
		}
		if !allScalar {
			continue
		}
		fnName := prog.Strings.String(token.StringID(prog.Instrs[letID].Data))
		candidates[letID] = &aotNativeFn{
			letID:      letID,
			name:       fnName,
			paramDefs:  sig.paramDefs,
			paramKinds: paramKinds,
			bodyID:     sig.bodyID,
			retKind:    retKind,
			retComp:    retComp,
		}
	}

	for len(candidates) > 0 {
		removed := false
		for letID, fn := range candidates {
			localVars := make(map[ir.InstrID]string, len(fn.paramDefs)+4)
			for k, pDef := range fn.paramDefs {
				localVars[pDef] = fmt.Sprintf("p_%d", k)
			}
			compVars := make(map[ir.InstrID][]string)
			bodyCode, ok := emitNativeBlock(prog, fn.bodyID, fn.retKind, fn.retComp, info, candidates, localVars, compVars, letID)
			if !ok {
				delete(candidates, letID)
				removed = true
			} else {
				fn.bodyCode = bodyCode
				fn.hasTailLoop = strings.Contains(bodyCode, "continue aotTailLoop")
			}
		}
		if !removed {
			break
		}
	}
	info.nativeFns = candidates
	return info
}

func markADTSwitchCasePayloads(prog *ir.Program, info *aotScalarInfo) {
	for _, ins := range prog.Instrs {
		if ins.Kind != ir.IRSwitch || int(ins.Arg0) >= len(prog.Instrs) {
			continue
		}
		tagIns := prog.Instrs[ins.Arg0]
		if tagIns.Kind != ir.IRLoad || tagIns.SubOp != ir.LoadADTTag {
			continue
		}
		scrutID := ir.InstrID(tagIns.Arg0)
		scrutDef := info.varDef[scrutID]
		count := uint32(ins.Data)
		for j := uint32(0); j < count; j++ {
			if int(ins.Arg1+j*2+1) >= len(prog.Extra) {
				continue
			}
			caseTag := token.StringID(prog.Extra[ins.Arg1+j*2])
			target := ir.InstrID(prog.Extra[ins.Arg1+j*2+1])
			markCasePayloadLoads(prog, target, scrutID, scrutDef, caseTag, info, make(map[ir.InstrID]bool))
		}
	}
}

func markCasePayloadLoads(prog *ir.Program, id, scrutID, scrutDef ir.InstrID, caseTag token.StringID, info *aotScalarInfo, visited map[ir.InstrID]bool) {
	if id == ir.NoInstr || int(id) >= len(prog.Instrs) || visited[id] {
		return
	}
	visited[id] = true
	ins := prog.Instrs[id]
	switch ins.Kind {
	case ir.IRLoad:
		if ins.SubOp == ir.LoadADTPayload {
			arg0 := ir.InstrID(ins.Arg0)
			if arg0 == scrutID || (scrutDef != ir.NoInstr && info.varDef[arg0] == scrutDef) {
				info.adtCaseTag[id] = caseTag
			}
		}
		markCasePayloadLoads(prog, ir.InstrID(ins.Arg0), scrutID, scrutDef, caseTag, info, visited)
	case ir.IRLet:
		markCasePayloadLoads(prog, ir.InstrID(ins.Arg0), scrutID, scrutDef, caseTag, info, visited)
		markCasePayloadLoads(prog, ir.InstrID(ins.Arg1), scrutID, scrutDef, caseTag, info, visited)
	case ir.IRPrim:
		if ins.SubOp == ir.PrimSeqBlock {
			for i := uint32(0); i < ins.Arg1; i++ {
				if int(ins.Arg0+i) < len(prog.Extra) {
					markCasePayloadLoads(prog, ir.InstrID(prog.Extra[ins.Arg0+i]), scrutID, scrutDef, caseTag, info, visited)
				}
			}
			return
		}
		markCasePayloadLoads(prog, ir.InstrID(ins.Arg0), scrutID, scrutDef, caseTag, info, visited)
		markCasePayloadLoads(prog, ir.InstrID(ins.Arg1), scrutID, scrutDef, caseTag, info, visited)
	case ir.IRApp:
		markCasePayloadLoads(prog, ir.InstrID(ins.Arg0), scrutID, scrutDef, caseTag, info, visited)
		markCasePayloadLoads(prog, ir.InstrID(ins.Arg1), scrutID, scrutDef, caseTag, info, visited)
	}
}

func isNativeScalarKind(k scalarKind) bool {
	return k == scalarInt || k == scalarFloat || k == scalarBool || k == scalarString
}

func goTypeForScalar(k scalarKind) string {
	switch k {
	case scalarInt:
		return "int64"
	case scalarFloat:
		return "float64"
	case scalarBool:
		return "bool"
	case scalarString:
		return "string"
	default:
		return "int64"
	}
}

func zeroLiteralForScalar(k scalarKind) string {
	switch k {
	case scalarInt:
		return "int64(0)"
	case scalarFloat:
		return "float64(0)"
	case scalarBool:
		return "false"
	case scalarString:
		return "\"\""
	default:
		return "int64(0)"
	}
}

func boxScalarExpr(k scalarKind, expr string) string {
	switch k {
	case scalarInt:
		return fmt.Sprintf("runtime.IntVal{V: %s}", expr)
	case scalarFloat:
		return fmt.Sprintf("runtime.FloatVal{V: %s}", expr)
	case scalarBool:
		return fmt.Sprintf("runtime.BoolVal{V: %s}", expr)
	case scalarString:
		return fmt.Sprintf("runtime.StringVal{V: %s}", expr)
	default:
		return "runtime.Unit"
	}
}

func goReturnTypeForFn(fn *aotNativeFn) string {
	if fn.retComp != nil {
		if fn.retComp.kind == compList || fn.retComp.kind == compArray {
			return goTypeForCompositeComponent(fn.retComp, 0)
		}
		parts := make([]string, len(fn.retComp.components))
		for i, ck := range fn.retComp.components {
			parts[i] = goTypeForScalar(ck)
		}
		return fmt.Sprintf("(%s)", strings.Join(parts, ", "))
	}
	return goTypeForScalar(fn.retKind)
}

func emitAOTNativeFunctions(buf *bytes.Buffer, _ *ir.Program, scalars *aotScalarInfo) {
	if scalars == nil || len(scalars.nativeFns) == 0 {
		return
	}
	var ids []int
	for id := range scalars.nativeFns {
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	for _, rawID := range ids {
		fn := scalars.nativeFns[ir.InstrID(rawID)]
		var params []string
		for k, pk := range fn.paramKinds {
			params = append(params, fmt.Sprintf("p_%d %s", k, goTypeForScalar(pk)))
		}
		fmt.Fprintf(buf, "// Native unboxed Go function for %s\n", fn.name)
		fmt.Fprintf(buf, "func aotFn_%d(%s) %s {\n", fn.letID, strings.Join(params, ", "), goReturnTypeForFn(fn))
		if fn.hasTailLoop {
			buf.WriteString("aotTailLoop:\n\tfor {\n")
			buf.WriteString(fn.bodyCode)
			buf.WriteString("\t}\n")
		} else {
			buf.WriteString(fn.bodyCode)
		}
		buf.WriteString("}\n\n")
	}
}

func cloneLocalVars(src map[ir.InstrID]string) map[ir.InstrID]string {
	dst := make(map[ir.InstrID]string, len(src)+4)
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func cloneCompVars(src map[ir.InstrID][]string) map[ir.InstrID][]string {
	dst := make(map[ir.InstrID][]string, len(src)+4)
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func isLetOnlySeqBlock(prog *ir.Program, id ir.InstrID) bool {
	if id == ir.NoInstr || int(id) >= len(prog.Instrs) {
		return false
	}
	ins := prog.Instrs[id]
	if ins.Kind != ir.IRPrim || ins.SubOp != ir.PrimSeqBlock || ins.Arg1 == 0 {
		return false
	}
	for i := uint32(0); i < ins.Arg1; i++ {
		if int(ins.Arg0+i) >= len(prog.Extra) {
			return false
		}
		ch := ir.InstrID(prog.Extra[ins.Arg0+i])
		if int(ch) >= len(prog.Instrs) {
			return false
		}
		chIns := prog.Instrs[ch]
		if chIns.Kind == ir.IRLet && chIns.Flags == 0 && ir.InstrID(chIns.Arg1) == ir.NoInstr {
			continue
		}
		if isLetOnlySeqBlock(prog, ch) {
			continue
		}
		return false
	}
	return true
}

func flattenSeqBlockChildren(prog *ir.Program, ins ir.IRInstr) []ir.InstrID {
	var out []ir.InstrID
	var appendChildren func(bIns ir.IRInstr, allLets bool)
	appendChildren = func(bIns ir.IRInstr, allLets bool) {
		for i := uint32(0); i < bIns.Arg1; i++ {
			if int(bIns.Arg0+i) >= len(prog.Extra) {
				continue
			}
			ch := ir.InstrID(prog.Extra[bIns.Arg0+i])
			isLast := !allLets && (i == bIns.Arg1-1)
			if !isLast && isLetOnlySeqBlock(prog, ch) {
				appendChildren(prog.Instrs[ch], true)
			} else {
				out = append(out, ch)
			}
		}
	}
	appendChildren(ins, false)
	return out
}

func emitNativeLetBinding(prog *ir.Program, letID, rhsID ir.InstrID, scalars *aotScalarInfo, candidates map[ir.InstrID]*aotNativeFn, nextVars map[ir.InstrID]string, nextCompVars map[ir.InstrID][]string) (string, bool) {
	if compShape := scalars.compDef[letID]; compShape != nil {
		cVars := make([]string, len(compShape.components))
		for i := range compShape.components {
			cVars[i] = fmt.Sprintf("local_%d_c%d", letID, i)
		}
		stmtCode, ok := emitNativeCompositeDecl(prog, rhsID, compShape, cVars, scalars, candidates, nextVars, nextCompVars)
		if !ok {
			return "", false
		}
		nextCompVars[letID] = cVars
		var sb strings.Builder
		sb.WriteString(stmtCode)
		for _, cv := range cVars {
			fmt.Fprintf(&sb, "\t_ = %s\n", cv)
		}
		return sb.String(), true
	}

	k := scalars.def[letID]
	if !isNativeScalarKind(k) {
		return "", false
	}
	rhsExpr, ok := nativeScalarExpr(prog, rhsID, k, scalars, candidates, nextVars, nextCompVars)
	if !ok {
		return "", false
	}
	localName := fmt.Sprintf("local_%d", letID)
	nextVars[letID] = localName
	return fmt.Sprintf("\t%s := %s\n\t_ = %s\n", localName, rhsExpr, localName), true
}

func emitNativeBlock(prog *ir.Program, id ir.InstrID, retKind scalarKind, retComp *aotCompositeShape, scalars *aotScalarInfo, candidates map[ir.InstrID]*aotNativeFn, localVars map[ir.InstrID]string, compVars map[ir.InstrID][]string, selfLetID ir.InstrID) (string, bool) {
	if id == ir.NoInstr || int(id) >= len(prog.Instrs) {
		return "", false
	}
	ins := prog.Instrs[id]

	switch ins.Kind {
	case ir.IRLet:
		if ins.Flags != 0 || ir.InstrID(ins.Arg1) == ir.NoInstr {
			return "", false
		}
		nextVars := cloneLocalVars(localVars)
		nextCompVars := cloneCompVars(compVars)
		bindCode, ok := emitNativeLetBinding(prog, id, ir.InstrID(ins.Arg0), scalars, candidates, nextVars, nextCompVars)
		if !ok {
			return "", false
		}
		restCode, ok := emitNativeBlock(prog, ir.InstrID(ins.Arg1), retKind, retComp, scalars, candidates, nextVars, nextCompVars, selfLetID)
		if !ok {
			return "", false
		}
		return bindCode + restCode, true

	case ir.IRPrim:
		if ins.SubOp == ir.PrimSeqBlock && ins.Arg1 > 0 {
			children := flattenSeqBlockChildren(prog, ins)
			if len(children) == 0 {
				return "", false
			}
			nextVars := cloneLocalVars(localVars)
			nextCompVars := cloneCompVars(compVars)
			var out strings.Builder
			for j := 0; j < len(children)-1; j++ {
				ch := children[j]
				if int(ch) >= len(prog.Instrs) {
					return "", false
				}
				chIns := prog.Instrs[ch]
				if chIns.Kind != ir.IRLet || chIns.Flags != 0 || ir.InstrID(chIns.Arg1) != ir.NoInstr {
					return "", false
				}
				bindCode, ok := emitNativeLetBinding(prog, ch, ir.InstrID(chIns.Arg0), scalars, candidates, nextVars, nextCompVars)
				if !ok {
					return "", false
				}
				out.WriteString(bindCode)
			}
			lastChild := children[len(children)-1]
			tailCode, ok := emitNativeBlock(prog, lastChild, retKind, retComp, scalars, candidates, nextVars, nextCompVars, selfLetID)
			if !ok {
				return "", false
			}
			out.WriteString(tailCode)
			return out.String(), true
		}

	case ir.IRSwitch:
		count := uint32(ins.Data)
		if count == 1 && int(ins.Arg1+1) < len(prog.Extra) {
			caseTag := prog.Extra[ins.Arg1]
			target := ir.InstrID(prog.Extra[ins.Arg1+1])
			if caseTag == 0 || caseTag == 1 {
				if condExpr, ok := nativeScalarExpr(prog, ir.InstrID(ins.Arg0), scalarBool, scalars, candidates, localVars, compVars); ok {
					targetCode, okT := emitNativeBlock(prog, target, retKind, retComp, scalars, candidates, localVars, compVars, selfLetID)
					auxCode, okA := emitNativeBlock(prog, ir.InstrID(ins.Aux), retKind, retComp, scalars, candidates, localVars, compVars, selfLetID)
					if okT && okA {
						if caseTag == 1 {
							return fmt.Sprintf("\tif %s {\n%s\t}\n%s", condExpr, targetCode, auxCode), true
						}
						return fmt.Sprintf("\tif !(%s) {\n%s\t}\n%s", condExpr, targetCode, auxCode), true
					}
				}
			}
		}
		if scrExpr, ok := nativeScalarExpr(prog, ir.InstrID(ins.Arg0), scalarInt, scalars, candidates, localVars, compVars); ok {
			var sb strings.Builder
			fmt.Fprintf(&sb, "\tswitch uint32(%s) {\n", scrExpr)
			seenTags := make(map[uint32]bool, count)
			for i := uint32(0); i < count; i++ {
				caseTag := prog.Extra[ins.Arg1+i*2]
				target := ir.InstrID(prog.Extra[ins.Arg1+i*2+1])
				if seenTags[caseTag] {
					continue
				}
				seenTags[caseTag] = true
				cCode, okC := emitNativeBlock(prog, target, retKind, retComp, scalars, candidates, localVars, compVars, selfLetID)
				if !okC {
					return "", false
				}
				fmt.Fprintf(&sb, "\tcase %d:\n%s", caseTag, cCode)
			}
			var dCode string
			if isIgnorableSwitchBranch(prog, ir.InstrID(ins.Aux)) {
				if retComp != nil {
					zeros := make([]string, len(retComp.components))
					for i := range retComp.components {
						zeros[i] = zeroLiteralForCompositeComponent(retComp, i)
					}
					dCode = fmt.Sprintf("\treturn %s\n", strings.Join(zeros, ", "))
				} else {
					dCode = fmt.Sprintf("\treturn %s\n", zeroLiteralForScalar(retKind))
				}
			} else {
				var okD bool
				dCode, okD = emitNativeBlock(prog, ir.InstrID(ins.Aux), retKind, retComp, scalars, candidates, localVars, compVars, selfLetID)
				if !okD {
					return "", false
				}
			}
			fmt.Fprintf(&sb, "\tdefault:\n%s\t}\n", dCode)
			return sb.String(), true
		}
		return "", false

	case ir.IRApp:
		if selfFn, okSelf := candidates[selfLetID]; okSelf && selfFn != nil {
			var revArgs []ir.InstrID
			spine := id
			for spine != ir.NoInstr && int(spine) < len(prog.Instrs) && prog.Instrs[spine].Kind == ir.IRApp {
				revArgs = append(revArgs, ir.InstrID(prog.Instrs[spine].Arg1))
				spine = ir.InstrID(prog.Instrs[spine].Arg0)
			}
			if defID, okDef := scalars.varDef[spine]; okDef && defID == selfLetID && len(revArgs) == len(selfFn.paramDefs) {
				lhsParams := make([]string, len(revArgs))
				rhsArgs := make([]string, len(revArgs))
				allArgsOK := true
				for k := 0; k < len(revArgs); k++ {
					argID := revArgs[len(revArgs)-1-k]
					aExpr, okA := nativeScalarExpr(prog, argID, selfFn.paramKinds[k], scalars, candidates, localVars, compVars)
					if !okA {
						allArgsOK = false
						break
					}
					lhsParams[k] = fmt.Sprintf("p_%d", k)
					rhsArgs[k] = aExpr
				}
				if allArgsOK {
					return fmt.Sprintf("\t// Native tail-call loop step (zero-allocation TCO)\n\t%s = %s\n\tcontinue aotTailLoop\n",
						strings.Join(lhsParams, ", "), strings.Join(rhsArgs, ", ")), true
				}
			}
		}
	}

	if retComp != nil {
		if callExpr, ok := nativeCompositeCallExpr(prog, id, retComp, scalars, candidates, localVars, compVars); ok {
			return fmt.Sprintf("\treturn %s\n", callExpr), true
		}
		exprs, ok := nativeCompositeExprs(prog, id, retComp, scalars, candidates, localVars, compVars)
		if !ok {
			return "", false
		}
		return fmt.Sprintf("\treturn %s\n", strings.Join(exprs, ", ")), true
	}

	expr, ok := nativeScalarExpr(prog, id, retKind, scalars, candidates, localVars, compVars)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("\treturn %s\n", expr), true
}

func nativeCompositeCallExpr(prog *ir.Program, id ir.InstrID, wantShape *aotCompositeShape, scalars *aotScalarInfo, candidates map[ir.InstrID]*aotNativeFn, localVars map[ir.InstrID]string, compVars map[ir.InstrID][]string) (string, bool) {
	if scalars == nil || candidates == nil || wantShape == nil || id == ir.NoInstr || int(id) >= len(prog.Instrs) {
		return "", false
	}
	if prog.Instrs[id].Kind != ir.IRApp {
		return "", false
	}
	var revArgs []ir.InstrID
	spine := id
	for spine != ir.NoInstr && int(spine) < len(prog.Instrs) && prog.Instrs[spine].Kind == ir.IRApp {
		revArgs = append(revArgs, ir.InstrID(prog.Instrs[spine].Arg1))
		spine = ir.InstrID(prog.Instrs[spine].Arg0)
	}
	defID, ok := scalars.varDef[spine]
	if !ok {
		return "", false
	}
	fn, okFn := candidates[defID]
	if !okFn || fn.retComp == nil || len(revArgs) != len(fn.paramDefs) {
		return "", false
	}
	if len(fn.retComp.components) != len(wantShape.components) {
		return "", false
	}
	for i := range wantShape.components {
		if fn.retComp.components[i] != wantShape.components[i] {
			return "", false
		}
	}
	argExprs := make([]string, len(revArgs))
	for k := 0; k < len(revArgs); k++ {
		argID := revArgs[len(revArgs)-1-k]
		aExpr, okA := nativeScalarExpr(prog, argID, fn.paramKinds[k], scalars, candidates, localVars, compVars)
		if !okA {
			return "", false
		}
		argExprs[k] = aExpr
	}
	return fmt.Sprintf("aotFn_%d(%s)", defID, strings.Join(argExprs, ", ")), true
}

func emitNativeCompositeDecl(prog *ir.Program, id ir.InstrID, wantShape *aotCompositeShape, targetVars []string, scalars *aotScalarInfo, candidates map[ir.InstrID]*aotNativeFn, localVars map[ir.InstrID]string, compVars map[ir.InstrID][]string) (string, bool) {
	if callExpr, ok := nativeCompositeCallExpr(prog, id, wantShape, scalars, candidates, localVars, compVars); ok {
		return fmt.Sprintf("\t%s := %s\n", strings.Join(targetVars, ", "), callExpr), true
	}
	if exprs, ok := nativeCompositeExprs(prog, id, wantShape, scalars, candidates, localVars, compVars); ok {
		return fmt.Sprintf("\t%s := %s\n", strings.Join(targetVars, ", "), strings.Join(exprs, ", ")), true
	}
	var sb strings.Builder
	for i := range wantShape.components {
		fmt.Fprintf(&sb, "\tvar %s %s\n", targetVars[i], goTypeForCompositeComponent(wantShape, i))
	}
	assignCode, ok := emitNativeCompositeAssign(prog, id, wantShape, targetVars, scalars, candidates, localVars, compVars)
	if !ok {
		return "", false
	}
	sb.WriteString(assignCode)
	return sb.String(), true
}

func emitNativeCompositeAssign(prog *ir.Program, id ir.InstrID, wantShape *aotCompositeShape, targetVars []string, scalars *aotScalarInfo, candidates map[ir.InstrID]*aotNativeFn, localVars map[ir.InstrID]string, compVars map[ir.InstrID][]string) (string, bool) {
	if id == ir.NoInstr || int(id) >= len(prog.Instrs) {
		return "", false
	}
	if callExpr, ok := nativeCompositeCallExpr(prog, id, wantShape, scalars, candidates, localVars, compVars); ok {
		return fmt.Sprintf("\t%s = %s\n", strings.Join(targetVars, ", "), callExpr), true
	}
	if exprs, ok := nativeCompositeExprs(prog, id, wantShape, scalars, candidates, localVars, compVars); ok {
		return fmt.Sprintf("\t%s = %s\n", strings.Join(targetVars, ", "), strings.Join(exprs, ", ")), true
	}
	ins := prog.Instrs[id]
	switch ins.Kind {
	case ir.IRLet:
		if ins.Flags != 0 || ir.InstrID(ins.Arg1) == ir.NoInstr {
			return "", false
		}
		nextVars := cloneLocalVars(localVars)
		nextCompVars := cloneCompVars(compVars)
		bindCode, ok := emitNativeLetBinding(prog, id, ir.InstrID(ins.Arg0), scalars, candidates, nextVars, nextCompVars)
		if !ok {
			return "", false
		}
		restCode, ok := emitNativeCompositeAssign(prog, ir.InstrID(ins.Arg1), wantShape, targetVars, scalars, candidates, nextVars, nextCompVars)
		if !ok {
			return "", false
		}
		return bindCode + restCode, true

	case ir.IRPrim:
		if ins.SubOp == ir.PrimSeqBlock && ins.Arg1 > 0 {
			children := flattenSeqBlockChildren(prog, ins)
			if len(children) == 0 {
				return "", false
			}
			nextVars := cloneLocalVars(localVars)
			nextCompVars := cloneCompVars(compVars)
			var out strings.Builder
			for j := 0; j < len(children)-1; j++ {
				ch := children[j]
				if int(ch) >= len(prog.Instrs) {
					return "", false
				}
				chIns := prog.Instrs[ch]
				if chIns.Kind != ir.IRLet || chIns.Flags != 0 || ir.InstrID(chIns.Arg1) != ir.NoInstr {
					return "", false
				}
				bindCode, ok := emitNativeLetBinding(prog, ch, ir.InstrID(chIns.Arg0), scalars, candidates, nextVars, nextCompVars)
				if !ok {
					return "", false
				}
				out.WriteString(bindCode)
			}
			lastChild := children[len(children)-1]
			tailCode, ok := emitNativeCompositeAssign(prog, lastChild, wantShape, targetVars, scalars, candidates, nextVars, nextCompVars)
			if !ok {
				return "", false
			}
			out.WriteString(tailCode)
			return out.String(), true
		}

	case ir.IRSwitch:
		count := uint32(ins.Data)
		if count == 1 && int(ins.Arg1+1) < len(prog.Extra) {
			caseTag := prog.Extra[ins.Arg1]
			target := ir.InstrID(prog.Extra[ins.Arg1+1])
			if caseTag == 0 || caseTag == 1 {
				if condExpr, ok := nativeScalarExpr(prog, ir.InstrID(ins.Arg0), scalarBool, scalars, candidates, localVars, compVars); ok {
					tCode, okT := emitNativeCompositeAssign(prog, target, wantShape, targetVars, scalars, candidates, localVars, compVars)
					aCode, okA := emitNativeCompositeAssign(prog, ir.InstrID(ins.Aux), wantShape, targetVars, scalars, candidates, localVars, compVars)
					if okT && okA {
						if caseTag == 1 {
							return fmt.Sprintf("\tif %s {\n%s\t} else {\n%s\t}\n", condExpr, tCode, aCode), true
						}
						return fmt.Sprintf("\tif !(%s) {\n%s\t} else {\n%s\t}\n", condExpr, tCode, aCode), true
					}
				}
			}
		}
		if scrExpr, ok := nativeScalarExpr(prog, ir.InstrID(ins.Arg0), scalarInt, scalars, candidates, localVars, compVars); ok {
			var sb strings.Builder
			fmt.Fprintf(&sb, "\tswitch uint32(%s) {\n", scrExpr)
			seenTags := make(map[uint32]bool, count)
			for i := uint32(0); i < count; i++ {
				caseTag := prog.Extra[ins.Arg1+i*2]
				target := ir.InstrID(prog.Extra[ins.Arg1+i*2+1])
				if seenTags[caseTag] {
					continue
				}
				seenTags[caseTag] = true
				cCode, okC := emitNativeCompositeAssign(prog, target, wantShape, targetVars, scalars, candidates, localVars, compVars)
				if !okC {
					return "", false
				}
				fmt.Fprintf(&sb, "\tcase %d:\n%s", caseTag, cCode)
			}
			if !isIgnorableSwitchBranch(prog, ir.InstrID(ins.Aux)) {
				dCode, okD := emitNativeCompositeAssign(prog, ir.InstrID(ins.Aux), wantShape, targetVars, scalars, candidates, localVars, compVars)
				if !okD {
					return "", false
				}
				fmt.Fprintf(&sb, "\tdefault:\n%s", dCode)
			}
			sb.WriteString("\t}\n")
			return sb.String(), true
		}
	}
	return "", false
}

func nativeCompositeExprs(prog *ir.Program, id ir.InstrID, wantShape *aotCompositeShape, scalars *aotScalarInfo, candidates map[ir.InstrID]*aotNativeFn, localVars map[ir.InstrID]string, compVars map[ir.InstrID][]string) ([]string, bool) {
	if scalars == nil || wantShape == nil || id == ir.NoInstr || int(id) >= len(prog.Instrs) {
		return nil, false
	}
	ins := prog.Instrs[id]

	switch ins.Kind {
	case ir.IRVar:
		if compVars == nil {
			return nil, false
		}
		defID, ok := scalars.varDef[id]
		if !ok || defID == ir.NoInstr {
			return nil, false
		}
		vars, exists := compVars[defID]
		defShape := scalars.compDef[defID]
		if !exists || defShape == nil || defShape.kind != wantShape.kind {
			return nil, false
		}
		if defShape.kind == compADT {
			out := make([]string, len(wantShape.components))
			out[0] = vars[0]
			for i := 1; i < len(wantShape.components); i++ {
				pk := wantShape.components[i]
				if sIdx := defShape.adtPayloadSlot(pk); sIdx >= 1 && sIdx < len(vars) {
					out[i] = vars[sIdx]
				} else {
					out[i] = zeroLiteralForScalar(pk)
				}
			}
			return out, true
		}
		if len(vars) != len(wantShape.components) {
			return nil, false
		}
		return vars, true

	case ir.IRAlloc:
		switch ins.SubOp {
		case ir.AllocTuple:
			if wantShape.kind != compTuple || int(ins.Arg1) != len(wantShape.components) || int(ins.Arg0+ins.Arg1) > len(prog.Extra) {
				return nil, false
			}
			out := make([]string, ins.Arg1)
			for i := uint32(0); i < ins.Arg1; i++ {
				ch := ir.InstrID(prog.Extra[ins.Arg0+i])
				e, ok := nativeScalarExpr(prog, ch, wantShape.components[i], scalars, candidates, localVars, compVars)
				if !ok {
					return nil, false
				}
				out[i] = e
			}
			return out, true

		case ir.AllocRecord:
			if wantShape.kind != compRecord || int(ins.Arg0+ins.Arg1) > len(prog.Extra) {
				return nil, false
			}
			fieldExprs := make(map[token.StringID]string, len(wantShape.fieldIDs))
			if ins.Flags != 0 {
				baseShape := scalars.compInstr[ins.Aux]
				if baseShape == nil || baseShape.kind != compRecord {
					return nil, false
				}
				baseExprs, ok := nativeCompositeExprs(prog, ir.InstrID(ins.Aux), baseShape, scalars, candidates, localVars, compVars)
				if !ok {
					return nil, false
				}
				for idx, fID := range baseShape.fieldIDs {
					fieldExprs[fID] = baseExprs[idx]
				}
			}
			fieldKindMap := make(map[token.StringID]scalarKind, len(wantShape.fieldIDs))
			for idx, fID := range wantShape.fieldIDs {
				fieldKindMap[fID] = wantShape.components[idx]
			}
			for j := uint32(0); j+1 < ins.Arg1; j += 2 {
				fID := token.StringID(prog.Extra[ins.Arg0+j])
				valID := ir.InstrID(prog.Extra[ins.Arg0+j+1])
				fk, okK := fieldKindMap[fID]
				if !okK {
					return nil, false
				}
				e, okE := nativeScalarExpr(prog, valID, fk, scalars, candidates, localVars, compVars)
				if !okE {
					return nil, false
				}
				fieldExprs[fID] = e
			}
			out := make([]string, len(wantShape.fieldIDs))
			for idx, fID := range wantShape.fieldIDs {
				e, exists := fieldExprs[fID]
				if !exists {
					return nil, false
				}
				out[idx] = e
			}
			return out, true

		case ir.AllocADT:
			if wantShape.kind != compADT {
				return nil, false
			}
			tag := token.StringID(ins.Data)
			out := make([]string, len(wantShape.components))
			out[0] = fmt.Sprintf("int64(%d)", tag)
			for i := 1; i < len(wantShape.components); i++ {
				out[i] = zeroLiteralForScalar(wantShape.components[i])
			}
			if ir.InstrID(ins.Arg0) != ir.NoInstr {
				pk := wantShape.adtTags[tag]
				slot := wantShape.adtPayloadSlot(pk)
				if slot < 1 {
					return nil, false
				}
				pExpr, ok := nativeScalarExpr(prog, ir.InstrID(ins.Arg0), pk, scalars, candidates, localVars, compVars)
				if !ok {
					return nil, false
				}
				out[slot] = pExpr
			}
			return out, true

		case ir.AllocList, ir.AllocArray:
			if !isNativeSliceShape(wantShape) || int(ins.Arg0+ins.Arg1) > len(prog.Extra) {
				return nil, false
			}
			if (ins.SubOp == ir.AllocList && wantShape.kind != compList) || (ins.SubOp == ir.AllocArray && wantShape.kind != compArray) {
				return nil, false
			}
			elemK := wantShape.components[0]
			elemGoTy := goTypeForScalar(elemK)
			elems := make([]string, ins.Arg1)
			for i := uint32(0); i < ins.Arg1; i++ {
				ch := ir.InstrID(prog.Extra[ins.Arg0+i])
				e, ok := nativeScalarExpr(prog, ch, elemK, scalars, candidates, localVars, compVars)
				if !ok {
					return nil, false
				}
				elems[i] = e
			}
			return []string{fmt.Sprintf("[]%s{%s}", elemGoTy, strings.Join(elems, ", "))}, true
		}

	case ir.IRPrim:
		if !isNativeSliceShape(wantShape) || wantShape.kind != compList {
			return nil, false
		}
		elemK := wantShape.components[0]
		elemGoTy := goTypeForScalar(elemK)
		switch ins.SubOp {
		case ir.PrimCons:
			hExpr, okH := nativeScalarExpr(prog, ir.InstrID(ins.Arg0), elemK, scalars, candidates, localVars, compVars)
			tExpr, okT := nativeSliceExpr(prog, ir.InstrID(ins.Arg1), wantShape, scalars, candidates, localVars, compVars)
			if okH && okT {
				return []string{fmt.Sprintf("append([]%s{%s}, %s...)", elemGoTy, hExpr, tExpr)}, true
			}
		case ir.PrimAppend:
			lExpr, okL := nativeSliceExpr(prog, ir.InstrID(ins.Arg0), wantShape, scalars, candidates, localVars, compVars)
			rExpr, okR := nativeSliceExpr(prog, ir.InstrID(ins.Arg1), wantShape, scalars, candidates, localVars, compVars)
			if okL && okR {
				return []string{fmt.Sprintf("append(append([]%s(nil), %s...), %s...)", elemGoTy, lExpr, rExpr)}, true
			}
		case ir.PrimRange:
			if elemK == scalarInt {
				sExpr, okS := nativeScalarExpr(prog, ir.InstrID(ins.Arg0), scalarInt, scalars, candidates, localVars, compVars)
				eExpr, okE := nativeScalarExpr(prog, ir.InstrID(ins.Arg1), scalarInt, scalars, candidates, localVars, compVars)
				if okS && okE {
					return []string{fmt.Sprintf("aotRangeInt(%s, %s, %d)", sExpr, eExpr, ins.Flags)}, true
				}
			}
		}

	case ir.IRLoad:
		if ins.SubOp == ir.LoadListTail && isNativeSliceShape(wantShape) && wantShape.kind == compList {
			if srcExpr, okS := nativeSliceExpr(prog, ir.InstrID(ins.Arg0), wantShape, scalars, candidates, localVars, compVars); okS {
				return []string{fmt.Sprintf("%s[1:]", srcExpr)}, true
			}
		}

	case ir.IRApp:
		if !isNativeSliceShape(wantShape) {
			return nil, false
		}
		elemK := wantShape.components[0]
		elemGoTy := goTypeForScalar(elemK)
		var revArgs []ir.InstrID
		spine := id
		for spine != ir.NoInstr && int(spine) < len(prog.Instrs) && prog.Instrs[spine].Kind == ir.IRApp {
			revArgs = append(revArgs, ir.InstrID(prog.Instrs[spine].Arg1))
			spine = ir.InstrID(prog.Instrs[spine].Arg0)
		}
		switch mathSym := resolveMathSymbol(prog, spine); mathSym {
		case "List.tail":
			if wantShape.kind == compList && len(revArgs) == 1 {
				if srcExpr, okS := nativeSliceExpr(prog, revArgs[0], wantShape, scalars, candidates, localVars, compVars); okS {
					return []string{fmt.Sprintf("%s[1:]", srcExpr)}, true
				}
			}
		case "List.rev", "Array.rev":
			if len(revArgs) == 1 {
				if srcExpr, okS := nativeSliceExpr(prog, revArgs[0], wantShape, scalars, candidates, localVars, compVars); okS {
					return []string{fmt.Sprintf("func() []%s { s := %s; out := make([]%s, len(s)); for i := range s { out[len(s)-1-i] = s[i] }; return out }()", elemGoTy, srcExpr, elemGoTy)}, true
				}
			}
		case "Array.append":
			if wantShape.kind == compArray && len(revArgs) == 2 {
				lExpr, okL := nativeSliceExpr(prog, revArgs[1], wantShape, scalars, candidates, localVars, compVars)
				rExpr, okR := nativeSliceExpr(prog, revArgs[0], wantShape, scalars, candidates, localVars, compVars)
				if okL && okR {
					return []string{fmt.Sprintf("append(append([]%s(nil), %s...), %s...)", elemGoTy, lExpr, rExpr)}, true
				}
			}
		case "Array.ofList":
			if wantShape.kind == compArray && len(revArgs) == 1 {
				listShape := &aotCompositeShape{kind: compList, components: wantShape.components}
				if srcExpr, okS := nativeSliceExpr(prog, revArgs[0], listShape, scalars, candidates, localVars, compVars); okS {
					return []string{srcExpr}, true
				}
			}
		case "Array.toList":
			if wantShape.kind == compList && len(revArgs) == 1 {
				arrShape := &aotCompositeShape{kind: compArray, components: wantShape.components}
				if srcExpr, okS := nativeSliceExpr(prog, revArgs[0], arrShape, scalars, candidates, localVars, compVars); okS {
					return []string{srcExpr}, true
				}
			}
		case "List.init", "Array.init":
			if len(revArgs) == 2 {
				cntExpr, okC := nativeScalarExpr(prog, revArgs[1], scalarInt, scalars, candidates, localVars, compVars)
				pDefs, bodyID, okL := resolveLambdaChain(prog, revArgs[0], scalars)
				if okC && okL && len(pDefs) == 1 {
					idxVar := fmt.Sprintf("idx_%d", id)
					loopVars := cloneLocalVars(localVars)
					loopVars[pDefs[0]] = idxVar
					if bodyExpr, okB := nativeScalarExpr(prog, bodyID, elemK, scalars, candidates, loopVars, compVars); okB {
						return []string{fmt.Sprintf("func() []%s { n := int(%s); if n <= 0 { return nil }; out := make([]%s, n); for i := 0; i < n; i++ { %s := int64(i); _ = %s; out[i] = %s }; return out }()", elemGoTy, cntExpr, elemGoTy, idxVar, idxVar, bodyExpr)}, true
					}
				}
			}
		case "List.map", "Array.map":
			if len(revArgs) == 2 && int(revArgs[0]) < len(scalars.compInstr) && isNativeSliceShape(scalars.compInstr[revArgs[0]]) {
				inShape := scalars.compInstr[revArgs[0]]
				srcExpr, okS := nativeSliceExpr(prog, revArgs[0], inShape, scalars, candidates, localVars, compVars)
				pDefs, bodyID, okL := resolveLambdaChain(prog, revArgs[1], scalars)
				if okS && okL && len(pDefs) == 1 {
					elemVar := fmt.Sprintf("elem_%d", id)
					loopVars := cloneLocalVars(localVars)
					loopVars[pDefs[0]] = elemVar
					if bodyExpr, okB := nativeScalarExpr(prog, bodyID, elemK, scalars, candidates, loopVars, compVars); okB {
						return []string{fmt.Sprintf("func() []%s { src := %s; out := make([]%s, len(src)); for i, %s := range src { out[i] = %s }; return out }()", elemGoTy, srcExpr, elemGoTy, elemVar, bodyExpr)}, true
					}
				}
			}
		case "List.filter", "Array.filter":
			if len(revArgs) == 2 {
				srcExpr, okS := nativeSliceExpr(prog, revArgs[0], wantShape, scalars, candidates, localVars, compVars)
				pDefs, bodyID, okL := resolveLambdaChain(prog, revArgs[1], scalars)
				if okS && okL && len(pDefs) == 1 {
					elemVar := fmt.Sprintf("elem_%d", id)
					loopVars := cloneLocalVars(localVars)
					loopVars[pDefs[0]] = elemVar
					if predExpr, okP := nativeScalarExpr(prog, bodyID, scalarBool, scalars, candidates, loopVars, compVars); okP {
						return []string{fmt.Sprintf("func() []%s { src := %s; out := make([]%s, 0, len(src)); for _, %s := range src { if %s { out = append(out, %s) } }; return out }()", elemGoTy, srcExpr, elemGoTy, elemVar, predExpr, elemVar)}, true
					}
				}
			}
		}
	}
	return nil, false
}

func nativeSliceExpr(prog *ir.Program, id ir.InstrID, wantShape *aotCompositeShape, scalars *aotScalarInfo, candidates map[ir.InstrID]*aotNativeFn, localVars map[ir.InstrID]string, compVars map[ir.InstrID][]string) (string, bool) {
	if !isNativeSliceShape(wantShape) {
		return "", false
	}
	if callExpr, ok := nativeCompositeCallExpr(prog, id, wantShape, scalars, candidates, localVars, compVars); ok {
		return callExpr, true
	}
	if exprs, ok := nativeCompositeExprs(prog, id, wantShape, scalars, candidates, localVars, compVars); ok && len(exprs) == 1 {
		return exprs[0], true
	}
	tmpVars := []string{fmt.Sprintf("tmp_slice_%d", id)}
	declCode, ok := emitNativeCompositeDecl(prog, id, wantShape, tmpVars, scalars, candidates, localVars, compVars)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("func() %s {\n%s\treturn %s\n}()", goTypeForCompositeComponent(wantShape, 0), declCode, tmpVars[0]), true
}

func nativeScalarFromCompositeSlot(prog *ir.Program, srcID ir.InstrID, srcShape *aotCompositeShape, slot int, wantKind scalarKind, scalars *aotScalarInfo, candidates map[ir.InstrID]*aotNativeFn, localVars map[ir.InstrID]string, compVars map[ir.InstrID][]string) (string, bool) {
	if srcShape == nil || slot < 0 || slot >= len(srcShape.components) || srcShape.components[slot] != wantKind {
		return "", false
	}
	if exprs, ok := nativeCompositeExprs(prog, srcID, srcShape, scalars, candidates, localVars, compVars); ok {
		return exprs[slot], true
	}
	tmpVars := make([]string, len(srcShape.components))
	for i := range srcShape.components {
		tmpVars[i] = fmt.Sprintf("tmp_%d_c%d", srcID, i)
	}
	declCode, ok := emitNativeCompositeDecl(prog, srcID, srcShape, tmpVars, scalars, candidates, localVars, compVars)
	if !ok {
		return "", false
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "func() %s {\n%s", goTypeForScalar(wantKind), declCode)
	for i, tv := range tmpVars {
		if i != slot {
			fmt.Fprintf(&sb, "\t_ = %s\n", tv)
		}
	}
	fmt.Fprintf(&sb, "\treturn %s\n}()", tmpVars[slot])
	return sb.String(), true
}

func nativeScalarExpr(prog *ir.Program, id ir.InstrID, wantKind scalarKind, scalars *aotScalarInfo, candidates map[ir.InstrID]*aotNativeFn, localVars map[ir.InstrID]string, compVars ...map[ir.InstrID][]string) (string, bool) {
	if scalars == nil || id == ir.NoInstr || int(id) >= len(prog.Instrs) || scalars.instr[id] != wantKind {
		return "", false
	}
	var cVars map[ir.InstrID][]string
	if len(compVars) > 0 {
		cVars = compVars[0]
	}
	ins := prog.Instrs[id]

	switch ins.Kind {
	case ir.IRConst:
		switch wantKind {
		case scalarInt:
			if ins.SubOp == ir.ConstInt {
				return fmt.Sprintf("int64(%d)", int64(ins.Data)), true
			}
		case scalarFloat:
			if ins.SubOp == ir.ConstFloat {
				f := math.Float64frombits(ins.Data)
				if !math.IsNaN(f) && !math.IsInf(f, 0) {
					return fmt.Sprintf("float64(%s)", strconv.FormatFloat(f, 'g', -1, 64)), true
				}
			}
		case scalarBool:
			if ins.SubOp == ir.ConstBool {
				return fmt.Sprintf("%v", ins.Data != 0), true
			}
		case scalarString:
			if ins.SubOp == ir.ConstString {
				return fmt.Sprintf("%q", prog.Strings.String(token.StringID(ins.Data))), true
			}
		}

	case ir.IRVar:
		if wantKind == scalarFloat && resolveMathSymbol(prog, id) == "Math.pi" {
			return "float64(math.Pi)", true
		}
		defID, ok := scalars.varDef[id]
		if !ok || defID == ir.NoInstr || scalars.def[defID] != wantKind {
			return "", false
		}
		if localVars != nil {
			name, exists := localVars[defID]
			return name, exists
		}
		switch wantKind {
		case scalarInt:
			return fmt.Sprintf("aotLoadInt(vm, env, token.StringID(%d))", ins.Data), true
		case scalarFloat:
			return fmt.Sprintf("aotLoadFloat(vm, env, token.StringID(%d))", ins.Data), true
		case scalarBool:
			return fmt.Sprintf("aotLoadBool(vm, env, token.StringID(%d))", ins.Data), true
		case scalarString:
			return fmt.Sprintf("aotLoadString(vm, env, token.StringID(%d))", ins.Data), true
		}

	case ir.IRLoad:
		if wantKind == scalarFloat && resolveMathSymbol(prog, id) == "Math.pi" {
			return "float64(math.Pi)", true
		}
		srcID := ir.InstrID(ins.Arg0)
		if int(srcID) < len(scalars.compInstr) && scalars.compInstr[srcID] != nil {
			sh := scalars.compInstr[srcID]
			switch ins.SubOp {
			case ir.LoadTupleElem:
				if sh.kind == compTuple && int(ins.Arg1) < len(sh.components) {
					if e, ok := nativeScalarFromCompositeSlot(prog, srcID, sh, int(ins.Arg1), wantKind, scalars, candidates, localVars, cVars); ok {
						return e, true
					}
				}
			case ir.LoadRecordField:
				if sh.kind == compRecord {
					wantField := token.StringID(ins.Data)
					for idx, fID := range sh.fieldIDs {
						if fID == wantField {
							if e, ok := nativeScalarFromCompositeSlot(prog, srcID, sh, idx, wantKind, scalars, candidates, localVars, cVars); ok {
								return e, true
							}
							break
						}
					}
				}
			case ir.LoadADTTag:
				if sh.kind == compADT && wantKind == scalarInt {
					if e, ok := nativeScalarFromCompositeSlot(prog, srcID, sh, 0, scalarInt, scalars, candidates, localVars, cVars); ok {
						return e, true
					}
				}
			case ir.LoadADTPayload:
				if sh.kind == compADT {
					slot := sh.adtPayloadSlot(wantKind)
					if slot >= 1 {
						if e, ok := nativeScalarFromCompositeSlot(prog, srcID, sh, slot, wantKind, scalars, candidates, localVars, cVars); ok {
							return e, true
						}
					}
				}
			case ir.LoadListHead:
				if sh.kind == compList && isNativeSliceShape(sh) && sh.components[0] == wantKind {
					if sExpr, okS := nativeSliceExpr(prog, srcID, sh, scalars, candidates, localVars, cVars); okS {
						return fmt.Sprintf("%s[0]", sExpr), true
					}
				}
			case ir.LoadListIsEmpty:
				if sh.kind == compList && isNativeSliceShape(sh) && wantKind == scalarInt {
					if sExpr, okS := nativeSliceExpr(prog, srcID, sh, scalars, candidates, localVars, cVars); okS {
						return fmt.Sprintf("func() int64 { if len(%s) == 0 { return 1 }; return 0 }()", sExpr), true
					}
				}
			}
		}
		if ins.SubOp == ir.LoadADTTag && wantKind == scalarInt && localVars == nil && int(srcID) < len(prog.Instrs) && prog.Instrs[srcID].Kind == ir.IRVar {
			return fmt.Sprintf("aotLoadADTTag(vm, env, token.StringID(%d))", prog.Instrs[srcID].Data), true
		}

	case ir.IRLet:
		if localVars != nil {
			if blockCode, ok := emitNativeBlock(prog, id, wantKind, nil, scalars, candidates, localVars, cVars, ir.NoInstr); ok {
				return fmt.Sprintf("func() %s {\n%s}()", goTypeForScalar(wantKind), blockCode), true
			}
		}

	case ir.IRSwitch:
		count := uint32(ins.Data)
		if count == 1 && int(ins.Arg1+1) < len(prog.Extra) {
			caseTag := prog.Extra[ins.Arg1]
			target := ir.InstrID(prog.Extra[ins.Arg1+1])
			if caseTag == 0 || caseTag == 1 {
				condExpr, okC := nativeScalarExpr(prog, ir.InstrID(ins.Arg0), scalarBool, scalars, candidates, localVars, cVars)
				tExpr, okT := nativeScalarExpr(prog, target, wantKind, scalars, candidates, localVars, cVars)
				aExpr, okA := nativeScalarExpr(prog, ir.InstrID(ins.Aux), wantKind, scalars, candidates, localVars, cVars)
				if okC && okT && okA {
					goTy := goTypeForScalar(wantKind)
					if caseTag == 1 {
						return fmt.Sprintf("func() %s { if %s { return %s }; return %s }()", goTy, condExpr, tExpr, aExpr), true
					}
					return fmt.Sprintf("func() %s { if !(%s) { return %s }; return %s }()", goTy, condExpr, tExpr, aExpr), true
				}
			}
		}
		if localVars != nil {
			if blockCode, ok := emitNativeBlock(prog, id, wantKind, nil, scalars, candidates, localVars, cVars, ir.NoInstr); ok {
				return fmt.Sprintf("func() %s {\n%s}()", goTypeForScalar(wantKind), blockCode), true
			}
		}

	case ir.IRApp:
		var revArgs []ir.InstrID
		spine := id
		for spine != ir.NoInstr && int(spine) < len(prog.Instrs) && prog.Instrs[spine].Kind == ir.IRApp {
			revArgs = append(revArgs, ir.InstrID(prog.Instrs[spine].Arg1))
			spine = ir.InstrID(prog.Instrs[spine].Arg0)
		}
		switch mathSym := resolveMathSymbol(prog, spine); mathSym {
		case "Math.sqrt", "Math.sin", "Math.cos", "Math.floor", "Math.ceil", "Math.round":
			if wantKind == scalarFloat && len(revArgs) == 1 {
				if a0, ok0 := nativeScalarExpr(prog, revArgs[0], scalarFloat, scalars, candidates, localVars, cVars); ok0 {
					goFn := "math.Sqrt"
					switch mathSym {
					case "Math.sin":
						goFn = "math.Sin"
					case "Math.cos":
						goFn = "math.Cos"
					case "Math.floor":
						goFn = "math.Floor"
					case "Math.ceil":
						goFn = "math.Ceil"
					case "Math.round":
						goFn = "math.Round"
					}
					return fmt.Sprintf("%s(%s)", goFn, a0), true
				}
			}
		case "Math.abs":
			if len(revArgs) == 1 {
				if wantKind == scalarFloat {
					if a0, ok0 := nativeScalarExpr(prog, revArgs[0], scalarFloat, scalars, candidates, localVars, cVars); ok0 {
						return fmt.Sprintf("math.Abs(%s)", a0), true
					}
				} else if wantKind == scalarInt {
					if a0, ok0 := nativeScalarExpr(prog, revArgs[0], scalarInt, scalars, candidates, localVars, cVars); ok0 {
						return fmt.Sprintf("aotAbsInt(%s)", a0), true
					}
				}
			}
		case "Math.pow":
			if wantKind == scalarFloat && len(revArgs) == 2 {
				a0, ok0 := nativeScalarExpr(prog, revArgs[1], scalarFloat, scalars, candidates, localVars, cVars)
				a1, ok1 := nativeScalarExpr(prog, revArgs[0], scalarFloat, scalars, candidates, localVars, cVars)
				if ok0 && ok1 {
					return fmt.Sprintf("math.Pow(%s, %s)", a0, a1), true
				}
			}
		case "Math.min", "Math.max":
			if len(revArgs) == 2 {
				if wantKind == scalarFloat {
					a0, ok0 := nativeScalarExpr(prog, revArgs[1], scalarFloat, scalars, candidates, localVars, cVars)
					a1, ok1 := nativeScalarExpr(prog, revArgs[0], scalarFloat, scalars, candidates, localVars, cVars)
					if ok0 && ok1 {
						if mathSym == "Math.min" {
							return fmt.Sprintf("math.Min(%s, %s)", a0, a1), true
						}
						return fmt.Sprintf("math.Max(%s, %s)", a0, a1), true
					}
				} else if wantKind == scalarInt {
					a0, ok0 := nativeScalarExpr(prog, revArgs[1], scalarInt, scalars, candidates, localVars, cVars)
					a1, ok1 := nativeScalarExpr(prog, revArgs[0], scalarInt, scalars, candidates, localVars, cVars)
					if ok0 && ok1 {
						if mathSym == "Math.min" {
							return fmt.Sprintf("aotMinInt(%s, %s)", a0, a1), true
						}
						return fmt.Sprintf("aotMaxInt(%s, %s)", a0, a1), true
					}
				}
			}
		case "String.length":
			if wantKind == scalarInt && len(revArgs) == 1 {
				if a0, ok0 := nativeScalarExpr(prog, revArgs[0], scalarString, scalars, candidates, localVars, cVars); ok0 {
					return fmt.Sprintf("int64(len(%s))", a0), true
				}
			}
		case "String.trim", "String.toUpper", "String.toLower":
			if wantKind == scalarString && len(revArgs) == 1 {
				if a0, ok0 := nativeScalarExpr(prog, revArgs[0], scalarString, scalars, candidates, localVars, cVars); ok0 {
					goFn := "strings.TrimSpace"
					switch mathSym {
					case "String.toUpper":
						goFn = "strings.ToUpper"
					case "String.toLower":
						goFn = "strings.ToLower"
					}
					return fmt.Sprintf("%s(%s)", goFn, a0), true
				}
			}
		case "String.contains", "String.startsWith", "String.endsWith":
			if wantKind == scalarBool && len(revArgs) == 2 {
				a0, ok0 := nativeScalarExpr(prog, revArgs[1], scalarString, scalars, candidates, localVars, cVars)
				a1, ok1 := nativeScalarExpr(prog, revArgs[0], scalarString, scalars, candidates, localVars, cVars)
				if ok0 && ok1 {
					goFn := "strings.Contains"
					switch mathSym {
					case "String.startsWith":
						goFn = "strings.HasPrefix"
					case "String.endsWith":
						goFn = "strings.HasSuffix"
					}
					return fmt.Sprintf("%s(%s, %s)", goFn, a1, a0), true
				}
			}
		case "String.indexOf":
			if wantKind == scalarInt && len(revArgs) == 2 {
				a0, ok0 := nativeScalarExpr(prog, revArgs[1], scalarString, scalars, candidates, localVars, cVars)
				a1, ok1 := nativeScalarExpr(prog, revArgs[0], scalarString, scalars, candidates, localVars, cVars)
				if ok0 && ok1 {
					return fmt.Sprintf("int64(strings.Index(%s, %s))", a1, a0), true
				}
			}
		case "String.replace":
			if wantKind == scalarString && len(revArgs) == 3 {
				a0, ok0 := nativeScalarExpr(prog, revArgs[2], scalarString, scalars, candidates, localVars, cVars)
				a1, ok1 := nativeScalarExpr(prog, revArgs[1], scalarString, scalars, candidates, localVars, cVars)
				a2, ok2 := nativeScalarExpr(prog, revArgs[0], scalarString, scalars, candidates, localVars, cVars)
				if ok0 && ok1 && ok2 {
					return fmt.Sprintf("strings.ReplaceAll(%s, %s, %s)", a2, a0, a1), true
				}
			}
		case "String.substring":
			if wantKind == scalarString && len(revArgs) == 3 {
				a0, ok0 := nativeScalarExpr(prog, revArgs[2], scalarInt, scalars, candidates, localVars, cVars)
				a1, ok1 := nativeScalarExpr(prog, revArgs[1], scalarInt, scalars, candidates, localVars, cVars)
				a2, ok2 := nativeScalarExpr(prog, revArgs[0], scalarString, scalars, candidates, localVars, cVars)
				if ok0 && ok1 && ok2 {
					return fmt.Sprintf("aotSubstring(%s, %s, %s)", a0, a1, a2), true
				}
			}
		case "Option.isSome", "Option.isNone", "Result.isOk", "Result.isError":
			if wantKind == scalarBool && len(revArgs) == 1 && int(revArgs[0]) < len(scalars.compInstr) {
				if sh := scalars.compInstr[revArgs[0]]; sh != nil && sh.kind == compADT {
					if tagExpr, okT := nativeScalarFromCompositeSlot(prog, revArgs[0], sh, 0, scalarInt, scalars, candidates, localVars, cVars); okT {
						tagName := "Some"
						switch mathSym {
						case "Option.isNone":
							tagName = "None"
						case "Result.isOk":
							tagName = "Ok"
						case "Result.isError":
							tagName = "Error"
						}
						tagID := prog.Strings.InternString(tagName)
						return fmt.Sprintf("(%s == int64(%d))", tagExpr, tagID), true
					}
				}
			}
		case "Option.defaultValue", "Result.defaultValue":
			if len(revArgs) == 2 && int(revArgs[0]) < len(scalars.compInstr) {
				defArg := revArgs[1]
				adtArg := revArgs[0]
				if sh := scalars.compInstr[adtArg]; sh != nil && sh.kind == compADT {
					slot := sh.adtPayloadSlot(wantKind)
					if slot >= 1 {
						defExpr, okD := nativeScalarExpr(prog, defArg, wantKind, scalars, candidates, localVars, cVars)
						if okD {
							tagName := "Some"
							if mathSym == "Result.defaultValue" {
								tagName = "Ok"
							}
							tagID := prog.Strings.InternString(tagName)
							goTy := goTypeForScalar(wantKind)
							if exprs, okE := nativeCompositeExprs(prog, adtArg, sh, scalars, candidates, localVars, cVars); okE {
								return fmt.Sprintf("func() %s { if %s == int64(%d) { return %s }; return %s }()", goTy, exprs[0], tagID, exprs[slot], defExpr), true
							}
							tmpVars := make([]string, len(sh.components))
							for i := range sh.components {
								tmpVars[i] = fmt.Sprintf("tmp_%d_c%d", adtArg, i)
							}
							if declCode, okDecl := emitNativeCompositeDecl(prog, adtArg, sh, tmpVars, scalars, candidates, localVars, cVars); okDecl {
								var sb strings.Builder
								fmt.Fprintf(&sb, "func() %s {\n%s", goTy, declCode)
								for i, tv := range tmpVars {
									if i != 0 && i != slot {
										fmt.Fprintf(&sb, "\t_ = %s\n", tv)
									}
								}
								fmt.Fprintf(&sb, "\tif %s == int64(%d) {\n\t\treturn %s\n\t}\n\treturn %s\n}()", tmpVars[0], tagID, tmpVars[slot], defExpr)
								return sb.String(), true
							}
						}
					}
				}
			}
		case "List.length", "Array.length":
			if wantKind == scalarInt && len(revArgs) == 1 && int(revArgs[0]) < len(scalars.compInstr) {
				if sh := scalars.compInstr[revArgs[0]]; isNativeSliceShape(sh) {
					if sExpr, okS := nativeSliceExpr(prog, revArgs[0], sh, scalars, candidates, localVars, cVars); okS {
						return fmt.Sprintf("int64(len(%s))", sExpr), true
					}
				}
			}
		case "List.isEmpty":
			if wantKind == scalarBool && len(revArgs) == 1 && int(revArgs[0]) < len(scalars.compInstr) {
				if sh := scalars.compInstr[revArgs[0]]; isNativeSliceShape(sh) {
					if sExpr, okS := nativeSliceExpr(prog, revArgs[0], sh, scalars, candidates, localVars, cVars); okS {
						return fmt.Sprintf("(len(%s) == 0)", sExpr), true
					}
				}
			}
		case "List.head":
			if len(revArgs) == 1 && int(revArgs[0]) < len(scalars.compInstr) {
				if sh := scalars.compInstr[revArgs[0]]; isNativeSliceShape(sh) && sh.components[0] == wantKind {
					if sExpr, okS := nativeSliceExpr(prog, revArgs[0], sh, scalars, candidates, localVars, cVars); okS {
						return fmt.Sprintf("%s[0]", sExpr), true
					}
				}
			}
		case "Array.get":
			if len(revArgs) == 2 && int(revArgs[1]) < len(scalars.compInstr) {
				if sh := scalars.compInstr[revArgs[1]]; isNativeSliceShape(sh) && sh.components[0] == wantKind {
					sExpr, okS := nativeSliceExpr(prog, revArgs[1], sh, scalars, candidates, localVars, cVars)
					idxExpr, okI := nativeScalarExpr(prog, revArgs[0], scalarInt, scalars, candidates, localVars, cVars)
					if okS && okI {
						return fmt.Sprintf("%s[%s]", sExpr, idxExpr), true
					}
				}
			}
		case "List.sum":
			if (wantKind == scalarInt || wantKind == scalarFloat) && len(revArgs) == 1 && int(revArgs[0]) < len(scalars.compInstr) {
				if sh := scalars.compInstr[revArgs[0]]; isNativeSliceShape(sh) && sh.components[0] == wantKind {
					if sExpr, okS := nativeSliceExpr(prog, revArgs[0], sh, scalars, candidates, localVars, cVars); okS {
						goTy := goTypeForScalar(wantKind)
						return fmt.Sprintf("func() %s { var acc %s; for _, v := range %s { acc += v }; return acc }()", goTy, goTy, sExpr), true
					}
				}
			}
		case "List.fold", "Array.fold":
			if len(revArgs) == 3 && int(revArgs[0]) < len(scalars.compInstr) {
				if sh := scalars.compInstr[revArgs[0]]; isNativeSliceShape(sh) {
					sExpr, okS := nativeSliceExpr(prog, revArgs[0], sh, scalars, candidates, localVars, cVars)
					initExpr, okI := nativeScalarExpr(prog, revArgs[1], wantKind, scalars, candidates, localVars, cVars)
					pDefs, bodyID, okL := resolveLambdaChain(prog, revArgs[2], scalars)
					if okS && okI && okL && len(pDefs) == 2 {
						accVar := fmt.Sprintf("acc_%d", id)
						elemVar := fmt.Sprintf("elem_%d", id)
						loopVars := cloneLocalVars(localVars)
						loopVars[pDefs[0]] = accVar
						loopVars[pDefs[1]] = elemVar
						if stepExpr, okB := nativeScalarExpr(prog, bodyID, wantKind, scalars, candidates, loopVars, cVars); okB {
							goTy := goTypeForScalar(wantKind)
							return fmt.Sprintf("func() %s { %s := %s; for _, %s := range %s { %s = %s }; return %s }()", goTy, accVar, initExpr, elemVar, sExpr, accVar, stepExpr, accVar), true
						}
					}
				}
			}
		case "List.exists", "Array.exists", "List.forall", "Array.forall":
			if wantKind == scalarBool && len(revArgs) == 2 && int(revArgs[0]) < len(scalars.compInstr) {
				if sh := scalars.compInstr[revArgs[0]]; isNativeSliceShape(sh) {
					sExpr, okS := nativeSliceExpr(prog, revArgs[0], sh, scalars, candidates, localVars, cVars)
					pDefs, bodyID, okL := resolveLambdaChain(prog, revArgs[1], scalars)
					if okS && okL && len(pDefs) == 1 {
						elemVar := fmt.Sprintf("elem_%d", id)
						loopVars := cloneLocalVars(localVars)
						loopVars[pDefs[0]] = elemVar
						if predExpr, okP := nativeScalarExpr(prog, bodyID, scalarBool, scalars, candidates, loopVars, cVars); okP {
							if mathSym == "List.exists" || mathSym == "Array.exists" {
								return fmt.Sprintf("func() bool { for _, %s := range %s { if %s { return true } }; return false }()", elemVar, sExpr, predExpr), true
							}
							return fmt.Sprintf("func() bool { for _, %s := range %s { if !(%s) { return false } }; return true }()", elemVar, sExpr, predExpr), true
						}
					}
				}
			}
		}
		if candidates == nil {
			return "", false
		}
		defID, ok := scalars.varDef[spine]
		if !ok {
			return "", false
		}
		fn, okFn := candidates[defID]
		if !okFn || fn.retComp != nil || fn.retKind != wantKind || len(revArgs) != len(fn.paramDefs) {
			return "", false
		}
		argExprs := make([]string, len(revArgs))
		for k := 0; k < len(revArgs); k++ {
			argID := revArgs[len(revArgs)-1-k]
			aExpr, okA := nativeScalarExpr(prog, argID, fn.paramKinds[k], scalars, candidates, localVars, cVars)
			if !okA {
				return "", false
			}
			argExprs[k] = aExpr
		}
		return fmt.Sprintf("aotFn_%d(%s)", defID, strings.Join(argExprs, ", ")), true

	case ir.IRPrim:
		if ins.SubOp == ir.PrimSeqBlock && localVars != nil {
			if blockCode, ok := emitNativeBlock(prog, id, wantKind, nil, scalars, candidates, localVars, cVars, ir.NoInstr); ok {
				return fmt.Sprintf("func() %s {\n%s}()", goTypeForScalar(wantKind), blockCode), true
			}
			return "", false
		}
		switch wantKind {
		case scalarInt:
			switch ins.SubOp {
			case ir.PrimNeg:
				if op, ok := nativeScalarExpr(prog, ir.InstrID(ins.Arg0), scalarInt, scalars, candidates, localVars, cVars); ok {
					return fmt.Sprintf("(-%s)", op), true
				}
			case ir.PrimAdd, ir.PrimSub, ir.PrimMul:
				lhs, okL := nativeScalarExpr(prog, ir.InstrID(ins.Arg0), scalarInt, scalars, candidates, localVars, cVars)
				rhs, okR := nativeScalarExpr(prog, ir.InstrID(ins.Arg1), scalarInt, scalars, candidates, localVars, cVars)
				if okL && okR {
					op := "+"
					if ins.SubOp == ir.PrimSub {
						op = "-"
					} else if ins.SubOp == ir.PrimMul {
						op = "*"
					}
					return fmt.Sprintf("(%s %s %s)", lhs, op, rhs), true
				}
			case ir.PrimDiv, ir.PrimMod:
				if int(ins.Arg1) < len(prog.Instrs) {
					rIns := prog.Instrs[ins.Arg1]
					if rIns.Kind == ir.IRConst && rIns.SubOp == ir.ConstInt && int64(rIns.Data) != 0 {
						if lhs, okL := nativeScalarExpr(prog, ir.InstrID(ins.Arg0), scalarInt, scalars, candidates, localVars, cVars); okL {
							op := "/"
							if ins.SubOp == ir.PrimMod {
								op = "%"
							}
							return fmt.Sprintf("(%s %s int64(%d))", lhs, op, int64(rIns.Data)), true
						}
					}
				}
				if localVars != nil {
					lhs, okL := nativeScalarExpr(prog, ir.InstrID(ins.Arg0), scalarInt, scalars, candidates, localVars, cVars)
					rhs, okR := nativeScalarExpr(prog, ir.InstrID(ins.Arg1), scalarInt, scalars, candidates, localVars, cVars)
					if okL && okR {
						op := "/"
						if ins.SubOp == ir.PrimMod {
							op = "%"
						}
						return fmt.Sprintf("(%s %s %s)", lhs, op, rhs), true
					}
				}
			}

		case scalarFloat:
			switch ins.SubOp {
			case ir.PrimNeg:
				if op, ok := nativeScalarExpr(prog, ir.InstrID(ins.Arg0), scalarFloat, scalars, candidates, localVars, cVars); ok {
					return fmt.Sprintf("(-%s)", op), true
				}
			case ir.PrimAdd, ir.PrimSub, ir.PrimMul:
				lhs, okL := nativeScalarExpr(prog, ir.InstrID(ins.Arg0), scalarFloat, scalars, candidates, localVars, cVars)
				rhs, okR := nativeScalarExpr(prog, ir.InstrID(ins.Arg1), scalarFloat, scalars, candidates, localVars, cVars)
				if okL && okR {
					op := "+"
					if ins.SubOp == ir.PrimSub {
						op = "-"
					} else if ins.SubOp == ir.PrimMul {
						op = "*"
					}
					return fmt.Sprintf("(%s %s %s)", lhs, op, rhs), true
				}
			case ir.PrimDiv:
				if int(ins.Arg1) < len(prog.Instrs) {
					rIns := prog.Instrs[ins.Arg1]
					if rIns.Kind == ir.IRConst && rIns.SubOp == ir.ConstFloat {
						rf := math.Float64frombits(rIns.Data)
						if rf != 0 && !math.IsNaN(rf) && !math.IsInf(rf, 0) {
							if lhs, okL := nativeScalarExpr(prog, ir.InstrID(ins.Arg0), scalarFloat, scalars, candidates, localVars, cVars); okL {
								return fmt.Sprintf("(%s / float64(%s))", lhs, strconv.FormatFloat(rf, 'g', -1, 64)), true
							}
						}
					}
				}
				if localVars != nil {
					lhs, okL := nativeScalarExpr(prog, ir.InstrID(ins.Arg0), scalarFloat, scalars, candidates, localVars, cVars)
					rhs, okR := nativeScalarExpr(prog, ir.InstrID(ins.Arg1), scalarFloat, scalars, candidates, localVars, cVars)
					if okL && okR {
						return fmt.Sprintf("(%s / %s)", lhs, rhs), true
					}
				}
			}

		case scalarBool:
			switch ins.SubOp {
			case ir.PrimNot:
				if op, ok := nativeScalarExpr(prog, ir.InstrID(ins.Arg0), scalarBool, scalars, candidates, localVars, cVars); ok {
					return fmt.Sprintf("(!%s)", op), true
				}
			case ir.PrimAnd, ir.PrimOr:
				lhs, okL := nativeScalarExpr(prog, ir.InstrID(ins.Arg0), scalarBool, scalars, candidates, localVars, cVars)
				rhs, okR := nativeScalarExpr(prog, ir.InstrID(ins.Arg1), scalarBool, scalars, candidates, localVars, cVars)
				if okL && okR {
					op := "&&"
					if ins.SubOp == ir.PrimOr {
						op = "||"
					}
					return fmt.Sprintf("(%s %s %s)", lhs, op, rhs), true
				}
			case ir.PrimEq, ir.PrimNeq, ir.PrimLt, ir.PrimLe, ir.PrimGt, ir.PrimGe:
				cmpOp := primCmpOpString(ins.SubOp)
				if lhs, okL := nativeScalarExpr(prog, ir.InstrID(ins.Arg0), scalarInt, scalars, candidates, localVars, cVars); okL {
					if rhs, okR := nativeScalarExpr(prog, ir.InstrID(ins.Arg1), scalarInt, scalars, candidates, localVars, cVars); okR {
						return fmt.Sprintf("(%s %s %s)", lhs, cmpOp, rhs), true
					}
				}
				if lhs, okL := nativeScalarExpr(prog, ir.InstrID(ins.Arg0), scalarFloat, scalars, candidates, localVars, cVars); okL {
					if rhs, okR := nativeScalarExpr(prog, ir.InstrID(ins.Arg1), scalarFloat, scalars, candidates, localVars, cVars); okR {
						return fmt.Sprintf("(%s %s %s)", lhs, cmpOp, rhs), true
					}
				}
				if lhs, okL := nativeScalarExpr(prog, ir.InstrID(ins.Arg0), scalarString, scalars, candidates, localVars, cVars); okL {
					if rhs, okR := nativeScalarExpr(prog, ir.InstrID(ins.Arg1), scalarString, scalars, candidates, localVars, cVars); okR {
						return fmt.Sprintf("(%s %s %s)", lhs, cmpOp, rhs), true
					}
				}
				if ins.SubOp == ir.PrimEq || ins.SubOp == ir.PrimNeq {
					if lhs, okL := nativeScalarExpr(prog, ir.InstrID(ins.Arg0), scalarBool, scalars, candidates, localVars, cVars); okL {
						if rhs, okR := nativeScalarExpr(prog, ir.InstrID(ins.Arg1), scalarBool, scalars, candidates, localVars, cVars); okR {
							return fmt.Sprintf("(%s %s %s)", lhs, cmpOp, rhs), true
						}
					}
				}
			}

		case scalarString:
			if ins.SubOp == ir.PrimAdd {
				lhs, okL := nativeScalarExpr(prog, ir.InstrID(ins.Arg0), scalarString, scalars, candidates, localVars, cVars)
				rhs, okR := nativeScalarExpr(prog, ir.InstrID(ins.Arg1), scalarString, scalars, candidates, localVars, cVars)
				if okL && okR {
					return fmt.Sprintf("(%s + %s)", lhs, rhs), true
				}
			}
		}
	}
	return "", false
}

func isIgnorableSwitchBranch(prog *ir.Program, id ir.InstrID) bool {
	if id == ir.NoInstr || int(id) >= len(prog.Instrs) {
		return true
	}
	ins := prog.Instrs[id]
	if ins.Kind == ir.IRConst && ins.SubOp == ir.ConstUnit {
		return true
	}
	if ins.Kind == ir.IRPrim && ins.SubOp == ir.PrimRaise {
		return true
	}
	return false
}

func cloneLexScope(src map[token.StringID]ir.InstrID) map[token.StringID]ir.InstrID {
	dst := make(map[token.StringID]ir.InstrID, len(src)+4)
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func resolveLexicalDefs(prog *ir.Program, id ir.InstrID, scope map[token.StringID]ir.InstrID, info *aotScalarInfo, visited map[ir.InstrID]bool) {
	if id == ir.NoInstr || int(id) >= len(prog.Instrs) || visited[id] {
		return
	}
	visited[id] = true
	ins := prog.Instrs[id]

	switch ins.Kind {
	case ir.IRConst:
		return
	case ir.IRVar:
		sym := token.StringID(ins.Data)
		if defID, ok := scope[sym]; ok {
			info.varDef[id] = defID
		}
	case ir.IRLet:
		sym := token.StringID(ins.Data)
		rhsScope := scope
		if ins.Flags&ir.IRFlagRec != 0 && sym != token.NoString {
			rhsScope = cloneLexScope(scope)
			rhsScope[sym] = id
		}
		resolveLexicalDefs(prog, ir.InstrID(ins.Arg0), rhsScope, info, visited)
		if ir.InstrID(ins.Arg1) != ir.NoInstr {
			bodyScope := cloneLexScope(scope)
			if sym != token.NoString {
				bodyScope[sym] = id
			}
			resolveLexicalDefs(prog, ir.InstrID(ins.Arg1), bodyScope, info, visited)
		}
	case ir.IRFn:
		bodyScope := cloneLexScope(scope)
		if param := token.StringID(ins.Data); param != token.NoString {
			bodyScope[param] = id
		}
		resolveLexicalDefs(prog, ir.InstrID(ins.Arg0), bodyScope, info, visited)
	case ir.IRApp:
		resolveLexicalDefs(prog, ir.InstrID(ins.Arg0), scope, info, visited)
		resolveLexicalDefs(prog, ir.InstrID(ins.Arg1), scope, info, visited)
	case ir.IRSwitch:
		resolveLexicalDefs(prog, ir.InstrID(ins.Arg0), scope, info, visited)
		count := uint32(ins.Data)
		for i := uint32(0); i < count; i++ {
			target := ir.InstrID(prog.Extra[ins.Arg1+i*2+1])
			resolveLexicalDefs(prog, target, scope, info, visited)
		}
		resolveLexicalDefs(prog, ir.InstrID(ins.Aux), scope, info, visited)
	case ir.IRPrim:
		if ins.SubOp == ir.PrimSeqBlock {
			blockScope := cloneLexScope(scope)
			children := flattenSeqBlockChildren(prog, ins)
			for _, ch := range children {
				if int(ch) < len(prog.Instrs) {
					chIns := prog.Instrs[ch]
					if chIns.Kind == ir.IRLet && chIns.Flags&ir.IRFlagRec != 0 && chIns.Data != 0 {
						blockScope[token.StringID(chIns.Data)] = ch
					}
				}
			}
			for _, ch := range children {
				resolveLexicalDefs(prog, ch, blockScope, info, visited)
				if int(ch) < len(prog.Instrs) {
					chIns := prog.Instrs[ch]
					if chIns.Kind == ir.IRLet && ir.InstrID(chIns.Arg1) == ir.NoInstr && chIns.Data != 0 {
						blockScope[token.StringID(chIns.Data)] = ch
					}
				}
			}
			return
		}
		if ins.SubOp == ir.PrimForIn || ins.SubOp == ir.PrimForTo {
			resolveLexicalDefs(prog, ir.InstrID(ins.Arg0), scope, info, visited)
			loopScope := cloneLexScope(scope)
			if sym := token.StringID(ins.Data); sym != token.NoString {
				loopScope[sym] = id
			}
			resolveLexicalDefs(prog, ir.InstrID(ins.Arg1), loopScope, info, visited)
			return
		}
		resolveLexicalDefs(prog, ir.InstrID(ins.Arg0), scope, info, visited)
		resolveLexicalDefs(prog, ir.InstrID(ins.Arg1), scope, info, visited)
	case ir.IRAlloc:
		if ins.SubOp == ir.AllocADT {
			resolveLexicalDefs(prog, ir.InstrID(ins.Arg0), scope, info, visited)
			return
		}
		if ins.SubOp == ir.AllocRecord {
			if ins.Flags != 0 {
				resolveLexicalDefs(prog, ir.InstrID(ins.Aux), scope, info, visited)
			}
			for i := uint32(0); i < ins.Arg1; i += 2 {
				valID := ir.InstrID(prog.Extra[ins.Arg0+i+1])
				resolveLexicalDefs(prog, valID, scope, info, visited)
			}
			return
		}
		for i := uint32(0); i < ins.Arg1; i++ {
			ch := ir.InstrID(prog.Extra[ins.Arg0+i])
			resolveLexicalDefs(prog, ch, scope, info, visited)
		}
	case ir.IRLoad:
		resolveLexicalDefs(prog, ir.InstrID(ins.Arg0), scope, info, visited)
	case ir.IRStore:
		if ins.SubOp != ir.StoreArrayElem {
			if defID, ok := scope[token.StringID(ins.Data)]; ok {
				info.storeDef[id] = defID
			}
		}
		resolveLexicalDefs(prog, ir.InstrID(ins.Arg0), scope, info, visited)
		resolveLexicalDefs(prog, ir.InstrID(ins.Arg1), scope, info, visited)
		resolveLexicalDefs(prog, ir.InstrID(ins.Aux), scope, info, visited)
	}
}

func unboxedIntExpr(prog *ir.Program, id ir.InstrID, scalars *aotScalarInfo) (string, bool) {
	if scalars == nil {
		return "", false
	}
	return nativeScalarExpr(prog, id, scalarInt, scalars, scalars.nativeFns, nil)
}

func unboxedFloatExpr(prog *ir.Program, id ir.InstrID, scalars *aotScalarInfo) (string, bool) {
	if scalars == nil {
		return "", false
	}
	return nativeScalarExpr(prog, id, scalarFloat, scalars, scalars.nativeFns, nil)
}

func unboxedBoolExpr(prog *ir.Program, id ir.InstrID, scalars *aotScalarInfo) (string, bool) {
	if scalars == nil {
		return "", false
	}
	return nativeScalarExpr(prog, id, scalarBool, scalars, scalars.nativeFns, nil)
}

func unboxedStringExpr(prog *ir.Program, id ir.InstrID, scalars *aotScalarInfo) (string, bool) {
	if scalars == nil {
		return "", false
	}
	return nativeScalarExpr(prog, id, scalarString, scalars, scalars.nativeFns, nil)
}

func primCmpOpString(subOp uint16) string {
	switch subOp {
	case ir.PrimEq:
		return "=="
	case ir.PrimNeq:
		return "!="
	case ir.PrimLt:
		return "<"
	case ir.PrimLe:
		return "<="
	case ir.PrimGt:
		return ">"
	case ir.PrimGe:
		return ">="
	default:
		return "=="
	}
}

func unboxedCompositeCallValueExpr(prog *ir.Program, id ir.InstrID, scalars *aotScalarInfo) (string, bool) {
	if scalars == nil || id == ir.NoInstr || int(id) >= len(prog.Instrs) || prog.Instrs[id].Kind != ir.IRApp {
		return "", false
	}
	sh := scalars.compInstr[id]
	if sh == nil {
		return "", false
	}
	callExpr, ok := nativeCompositeCallExpr(prog, id, sh, scalars, scalars.nativeFns, nil, nil)
	if !ok {
		return "", false
	}
	tmpVars := make([]string, len(sh.components))
	for i := range sh.components {
		tmpVars[i] = fmt.Sprintf("c_%d", i)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "func() runtime.Value {\n\t%s := %s\n", strings.Join(tmpVars, ", "), callExpr)
	switch sh.kind {
	case compTuple:
		boxed := make([]string, len(sh.components))
		for i, ck := range sh.components {
			boxed[i] = boxScalarExpr(ck, tmpVars[i])
		}
		fmt.Fprintf(&sb, "\treturn runtime.TupleVal{Elems: []runtime.Value{%s}}\n}()", strings.Join(boxed, ", "))
		return sb.String(), true

	case compRecord:
		fmt.Fprintf(&sb, "\tfields := make(map[token.StringID]runtime.Value, %d)\n", len(sh.fieldIDs))
		fmt.Fprintf(&sb, "\tbyName := make(map[string]runtime.Value, %d)\n", len(sh.fieldIDs))
		for i, fID := range sh.fieldIDs {
			fName := prog.Strings.String(fID)
			bExpr := boxScalarExpr(sh.components[i], tmpVars[i])
			fmt.Fprintf(&sb, "\tfields[token.StringID(%d)] = %s\n", fID, bExpr)
			fmt.Fprintf(&sb, "\tbyName[%q] = fields[token.StringID(%d)]\n", fName, fID)
		}
		sb.WriteString("\treturn &runtime.RecordVal{Fields: fields, ByName: byName}\n}()")
		return sb.String(), true

	case compADT:
		for i := 1; i < len(tmpVars); i++ {
			fmt.Fprintf(&sb, "\t_ = %s\n", tmpVars[i])
		}
		fmt.Fprintf(&sb, "\tswitch token.StringID(%s) {\n", tmpVars[0])
		var tags []int
		for t := range sh.adtTags {
			tags = append(tags, int(t))
		}
		sort.Ints(tags)
		for _, rawTag := range tags {
			tag := token.StringID(rawTag)
			tagName := prog.Strings.String(tag)
			pk := sh.adtTags[tag]
			fmt.Fprintf(&sb, "\tcase token.StringID(%d):\n", tag)
			if isNativeScalarKind(pk) {
				slot := sh.adtPayloadSlot(pk)
				fmt.Fprintf(&sb, "\t\treturn runtime.ADTVal{Tag: token.StringID(%d), TagName: %q, Payload: %s}\n", tag, tagName, boxScalarExpr(pk, tmpVars[slot]))
			} else {
				fmt.Fprintf(&sb, "\t\treturn runtime.ADTVal{Tag: token.StringID(%d), TagName: %q}\n", tag, tagName)
			}
		}
		sb.WriteString("\tdefault:\n\t\treturn runtime.Unit\n\t}\n}()")
		return sb.String(), true

	case compList:
		if !isNativeSliceShape(sh) {
			return "", false
		}
		elemK := sh.components[0]
		fmt.Fprintf(&sb, "\telems := make([]runtime.Value, len(%s))\n", tmpVars[0])
		fmt.Fprintf(&sb, "\tfor i, v := range %s {\n\t\telems[i] = %s\n\t}\n", tmpVars[0], boxScalarExpr(elemK, "v"))
		sb.WriteString("\treturn runtime.SliceToList(elems)\n}()")
		return sb.String(), true

	case compArray:
		if !isNativeSliceShape(sh) {
			return "", false
		}
		elemK := sh.components[0]
		fmt.Fprintf(&sb, "\telems := make([]runtime.Value, len(%s))\n", tmpVars[0])
		fmt.Fprintf(&sb, "\tfor i, v := range %s {\n\t\telems[i] = %s\n\t}\n", tmpVars[0], boxScalarExpr(elemK, "v"))
		sb.WriteString("\treturn &runtime.ArrayVal{Elems: elems}\n}()")
		return sb.String(), true
	}
	return "", false
}

func unboxedValueExpr(prog *ir.Program, id ir.InstrID, scalars *aotScalarInfo) (string, bool) {
	if iExpr, ok := unboxedIntExpr(prog, id, scalars); ok {
		return fmt.Sprintf("runtime.IntVal{V: %s}", iExpr), true
	}
	if fExpr, ok := unboxedFloatExpr(prog, id, scalars); ok {
		return fmt.Sprintf("runtime.FloatVal{V: %s}", fExpr), true
	}
	if bExpr, ok := unboxedBoolExpr(prog, id, scalars); ok {
		return fmt.Sprintf("runtime.BoolVal{V: %s}", bExpr), true
	}
	if sExpr, ok := unboxedStringExpr(prog, id, scalars); ok {
		return fmt.Sprintf("runtime.StringVal{V: %s}", sExpr), true
	}
	if cExpr, ok := unboxedCompositeCallValueExpr(prog, id, scalars); ok {
		return cExpr, true
	}
	return "", false
}

func emitCompiledCase(buf *bytes.Buffer, prog *ir.Program, id ir.InstrID, ins ir.IRInstr, selfTailCalls map[ir.InstrID]*selfTailCallInfo, scalars *aotScalarInfo) {
	fmt.Fprintf(buf, "\t\tcase %d:\n", id)
	switch ins.Kind {
	case ir.IRConst:
		switch ins.SubOp {
		case ir.ConstUnit:
			buf.WriteString("\t\t\treturn runtime.Unit, nil\n")
		case ir.ConstBool:
			fmt.Fprintf(buf, "\t\t\treturn runtime.BoolVal{V: %v}, nil\n", ins.Data != 0)
		case ir.ConstInt:
			fmt.Fprintf(buf, "\t\t\treturn runtime.IntVal{V: %d}, nil\n", int64(ins.Data))
		case ir.ConstFloat:
			fmt.Fprintf(buf, "\t\t\treturn runtime.FloatVal{V: %g}, nil\n", math.Float64frombits(ins.Data))
		case ir.ConstString:
			s := prog.Strings.String(token.StringID(ins.Data))
			fmt.Fprintf(buf, "\t\t\treturn runtime.StringVal{V: %q}, nil\n", s)
		default:
			buf.WriteString("\t\t\treturn runtime.Unit, nil\n")
		}

	case ir.IRVar:
		symName := prog.Strings.String(token.StringID(ins.Data))
		fmt.Fprintf(buf, "\t\t\tif v, ok := env[token.StringID(%d)]; ok {\n", ins.Data)
		buf.WriteString("\t\t\t\treturn runtime.Deref(v), nil\n")
		buf.WriteString("\t\t\t}\n")
		fmt.Fprintf(buf, "\t\t\tif v, ok := vm.Globals[token.StringID(%d)]; ok {\n", ins.Data)
		buf.WriteString("\t\t\t\treturn runtime.Deref(v), nil\n")
		buf.WriteString("\t\t\t}\n")
		fmt.Fprintf(buf, "\t\t\treturn nil, fmt.Errorf(\"unbound identifier %%q\", %q)\n", symName)

	case ir.IRLet:
		sym := token.StringID(ins.Data)
		if ins.Flags&ir.IRFlagRec == 0 {
			if uVal, ok := unboxedValueExpr(prog, ir.InstrID(ins.Arg0), scalars); ok {
				fmt.Fprintf(buf, "\t\t\tval := runtime.Value(%s)\n", uVal)
				if ins.Flags&ir.IRFlagMutable != 0 {
					buf.WriteString("\t\t\tval = &runtime.RefCell{Val: val}\n")
				}
				if sym != token.NoString {
					fmt.Fprintf(buf, "\t\t\tenv[token.StringID(%d)] = val\n", sym)
				}
				if ir.InstrID(ins.Arg1) != ir.NoInstr {
					fmt.Fprintf(buf, "\t\t\tid = %d\n\t\t\tcontinue tcoLoop\n", ins.Arg1)
				} else {
					buf.WriteString("\t\t\treturn runtime.Deref(val), nil\n")
				}
				return
			}
		}
		fmt.Fprintf(buf, "\t\t\tval, err := evalAOT(vm, %d, env)\n", ins.Arg0)
		buf.WriteString("\t\t\tif err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n")
		if ins.Flags&ir.IRFlagRec != 0 {
			fmt.Fprintf(buf, "\t\t\tif clos, ok := val.(*runtime.ClosureVal); ok {\n")
			fmt.Fprintf(buf, "\t\t\t\tclos.SelfSym = token.StringID(%d)\n", sym)
			fmt.Fprintf(buf, "\t\t\t\tclos.Env[token.StringID(%d)] = clos\n", sym)
			buf.WriteString("\t\t\t\tfor prevSym, prevVal := range env {\n")
			buf.WriteString("\t\t\t\t\tif prevClos, ok := prevVal.(*runtime.ClosureVal); ok && prevClos.SelfSym != token.NoString {\n")
			fmt.Fprintf(buf, "\t\t\t\t\t\tprevClos.Env[token.StringID(%d)] = clos\n", sym)
			buf.WriteString("\t\t\t\t\t\tclos.Env[prevSym] = prevClos\n")
			buf.WriteString("\t\t\t\t\t}\n")
			buf.WriteString("\t\t\t\t}\n")
			buf.WriteString("\t\t\t}\n")
		}
		if ins.Flags&ir.IRFlagMutable != 0 {
			buf.WriteString("\t\t\tval = &runtime.RefCell{Val: runtime.Deref(val)}\n")
		}
		if sym != token.NoString {
			fmt.Fprintf(buf, "\t\t\tenv[token.StringID(%d)] = val\n", sym)
		}
		if ir.InstrID(ins.Arg1) != ir.NoInstr {
			if ins.Flags&ir.IRFlagDispose != 0 {
				fmt.Fprintf(buf, "\t\t\tres, err := evalAOT(vm, %d, env)\n", ins.Arg1)
				buf.WriteString("\t\t\truntime.DisposeValue(val)\n")
				buf.WriteString("\t\t\treturn res, err\n")
			} else {
				fmt.Fprintf(buf, "\t\t\tid = %d\n\t\t\tcontinue tcoLoop\n", ins.Arg1)
			}
		} else {
			buf.WriteString("\t\t\treturn runtime.Deref(val), nil\n")
		}

	case ir.IRFn:
		param := token.StringID(ins.Data)
		body := ir.InstrID(ins.Arg0)
		if ins.Aux > 0 {
			fmt.Fprintf(buf, "\t\t\tcaptured := make(map[token.StringID]runtime.Value, %d)\n", ins.Aux+2)
			for i := uint32(0); i < ins.Aux; i++ {
				capSym := token.StringID(prog.Extra[ins.Arg1+i])
				fmt.Fprintf(buf, "\t\t\tif v, ok := env[token.StringID(%d)]; ok {\n", capSym)
				fmt.Fprintf(buf, "\t\t\t\tcaptured[token.StringID(%d)] = v\n", capSym)
				fmt.Fprintf(buf, "\t\t\t} else if v, ok := vm.Globals[token.StringID(%d)]; ok {\n", capSym)
				fmt.Fprintf(buf, "\t\t\t\tcaptured[token.StringID(%d)] = v\n", capSym)
				buf.WriteString("\t\t\t}\n")
			}
		} else if ins.Flags&ir.IRFlagClosed != 0 {
			buf.WriteString("\t\t\tcaptured := make(map[token.StringID]runtime.Value, 2)\n")
		} else {
			buf.WriteString("\t\t\tcaptured := make(map[token.StringID]runtime.Value, len(env)+2)\n")
			buf.WriteString("\t\t\tfor k, v := range env {\n\t\t\t\tcaptured[k] = v\n\t\t\t}\n")
		}
		fmt.Fprintf(buf, "\t\t\treturn &runtime.ClosureVal{Param: token.StringID(%d), Body: %d, Env: captured}, nil\n", param, body)

	case ir.IRApp:
		if tc, ok := selfTailCalls[id]; ok {
			buf.WriteString("\t\t\t// Direct self-recursive tail-call loop lowering\n")
			for i, argID := range tc.argIDs {
				if uVal, ok := unboxedValueExpr(prog, argID, scalars); ok {
					fmt.Fprintf(buf, "\t\t\targVal_%d := runtime.Value(%s)\n", i, uVal)
					continue
				}
				if int(argID) < len(prog.Instrs) {
					aIns := prog.Instrs[argID]
					if aIns.Kind == ir.IRPrim && (aIns.SubOp == ir.PrimDiv || aIns.SubOp == ir.PrimMod) {
						if lhs, okL := unboxedIntExpr(prog, ir.InstrID(aIns.Arg0), scalars); okL {
							if rhs, okR := unboxedIntExpr(prog, ir.InstrID(aIns.Arg1), scalars); okR {
								op := "/"
								if aIns.SubOp == ir.PrimMod {
									op = "%"
								}
								fmt.Fprintf(buf, "\t\t\trhsInt_%d := %s\n", i, rhs)
								fmt.Fprintf(buf, "\t\t\tif rhsInt_%d == 0 {\n\t\t\t\treturn nil, fmt.Errorf(\"division by zero\")\n\t\t\t}\n", i)
								fmt.Fprintf(buf, "\t\t\targVal_%d := runtime.Value(runtime.IntVal{V: (%s %s rhsInt_%d)})\n", i, lhs, op, i)
								continue
							}
						}
					}
				}
				fmt.Fprintf(buf, "\t\t\targVal_%d, err := evalAOT(vm, %d, env)\n", i, argID)
				buf.WriteString("\t\t\tif err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n")
			}
			if tc.inPlaceEnv {
				for i, p := range tc.params {
					if p != token.NoString {
						fmt.Fprintf(buf, "\t\t\tenv[token.StringID(%d)] = runtime.Deref(argVal_%d)\n", p, i)
					}
				}
			} else {
				buf.WriteString("\t\t\tnextEnv := make(map[token.StringID]runtime.Value, len(env)+2)\n")
				buf.WriteString("\t\t\tfor k, v := range env {\n\t\t\t\tnextEnv[k] = v\n\t\t\t}\n")
				for i, p := range tc.params {
					if p != token.NoString {
						fmt.Fprintf(buf, "\t\t\tnextEnv[token.StringID(%d)] = runtime.Deref(argVal_%d)\n", p, i)
					}
				}
				buf.WriteString("\t\t\tenv = nextEnv\n")
			}
			fmt.Fprintf(buf, "\t\t\tid = %d\n\t\t\tcontinue tcoLoop\n", tc.targetBody)
			return
		}
		if uVal, ok := unboxedValueExpr(prog, id, scalars); ok {
			buf.WriteString("\t\t\t// Direct native Go function call specialization\n")
			fmt.Fprintf(buf, "\t\t\treturn %s, nil\n", uVal)
			return
		}
		fmt.Fprintf(buf, "\t\t\tfnVal, err := evalAOT(vm, %d, env)\n", ins.Arg0)
		buf.WriteString("\t\t\tif err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n")
		fmt.Fprintf(buf, "\t\t\targVal, err := evalAOT(vm, %d, env)\n", ins.Arg1)
		buf.WriteString("\t\t\tif err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n")
		if ins.Flags&ir.IRFlagTailCall != 0 {
			buf.WriteString("\t\t\tfnVal = runtime.Deref(fnVal)\n")
			buf.WriteString("\t\t\targVal = runtime.Deref(argVal)\n")
			buf.WriteString("\t\t\tif clos, ok := fnVal.(*runtime.ClosureVal); ok {\n")
			buf.WriteString("\t\t\t\tnextEnv := make(map[token.StringID]runtime.Value, len(clos.Env)+2)\n")
			buf.WriteString("\t\t\t\tfor k, v := range clos.Env {\n\t\t\t\t\tnextEnv[k] = v\n\t\t\t\t}\n")
			buf.WriteString("\t\t\t\tif clos.SelfSym != token.NoString {\n\t\t\t\t\tnextEnv[clos.SelfSym] = clos\n\t\t\t\t}\n")
			buf.WriteString("\t\t\t\tif clos.Param != token.NoString {\n\t\t\t\t\tnextEnv[clos.Param] = argVal\n\t\t\t\t}\n")
			buf.WriteString("\t\t\t\tenv = nextEnv\n")
			buf.WriteString("\t\t\t\tid = clos.Body\n")
			buf.WriteString("\t\t\t\tcontinue tcoLoop\n")
			buf.WriteString("\t\t\t}\n")
		}
		buf.WriteString("\t\t\treturn vm.Apply(fnVal, argVal)\n")

	case ir.IRSwitch:
		count := uint32(ins.Data)
		if count == 1 && int(ins.Arg1+1) < len(prog.Extra) {
			caseTag := prog.Extra[ins.Arg1]
			target := ir.InstrID(prog.Extra[ins.Arg1+1])
			if (caseTag == 0 || caseTag == 1) {
				if condExpr, ok := unboxedBoolExpr(prog, ir.InstrID(ins.Arg0), scalars); ok {
					buf.WriteString("\t\t\t// Unboxed boolean branch specialization\n")
					if caseTag == 1 {
						fmt.Fprintf(buf, "\t\t\tif %s {\n\t\t\t\tid = %d\n\t\t\t\tcontinue tcoLoop\n\t\t\t}\n", condExpr, target)
						fmt.Fprintf(buf, "\t\t\tid = %d\n\t\t\tcontinue tcoLoop\n", ir.InstrID(ins.Aux))
					} else {
						fmt.Fprintf(buf, "\t\t\tif !(%s) {\n\t\t\t\tid = %d\n\t\t\t\tcontinue tcoLoop\n\t\t\t}\n", condExpr, target)
						fmt.Fprintf(buf, "\t\t\tid = %d\n\t\t\tcontinue tcoLoop\n", ir.InstrID(ins.Aux))
					}
					return
				}
			}
		}
		if intExpr, ok := unboxedIntExpr(prog, ir.InstrID(ins.Arg0), scalars); ok {
			buf.WriteString("\t\t\t// Unboxed int64 switch specialization\n")
			fmt.Fprintf(buf, "\t\t\tswitch uint32(%s) {\n", intExpr)
			seenTags := make(map[uint32]bool, count)
			for i := uint32(0); i < count; i++ {
				caseTag := prog.Extra[ins.Arg1+i*2]
				target := ir.InstrID(prog.Extra[ins.Arg1+i*2+1])
				if seenTags[caseTag] {
					continue
				}
				seenTags[caseTag] = true
				fmt.Fprintf(buf, "\t\t\tcase %d:\n\t\t\t\tid = %d\n\t\t\t\tcontinue tcoLoop\n", caseTag, target)
			}
			fmt.Fprintf(buf, "\t\t\tdefault:\n\t\t\t\tid = %d\n\t\t\t\tcontinue tcoLoop\n", ir.InstrID(ins.Aux))
			buf.WriteString("\t\t\t}\n")
			return
		}
		fmt.Fprintf(buf, "\t\t\tscrutVal, err := evalAOT(vm, %d, env)\n", ins.Arg0)
		buf.WriteString("\t\t\tif err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n")
		buf.WriteString("\t\t\tscrutTag := runtime.ExtractSwitchTag(runtime.Deref(scrutVal), vm.Prog.Strings)\n")
		buf.WriteString("\t\t\tswitch scrutTag {\n")
		seenTags := make(map[uint32]bool, count)
		for i := uint32(0); i < count; i++ {
			caseTag := prog.Extra[ins.Arg1+i*2]
			target := ir.InstrID(prog.Extra[ins.Arg1+i*2+1])
			if seenTags[caseTag] {
				continue
			}
			seenTags[caseTag] = true
			fmt.Fprintf(buf, "\t\t\tcase %d:\n\t\t\t\tid = %d\n\t\t\t\tcontinue tcoLoop\n", caseTag, target)
		}
		fmt.Fprintf(buf, "\t\t\tdefault:\n\t\t\t\tid = %d\n\t\t\t\tcontinue tcoLoop\n", ir.InstrID(ins.Aux))
		buf.WriteString("\t\t\t}\n")

	case ir.IRPrim:
		emitCompiledPrim(buf, prog, id, ins, scalars)

	case ir.IRAlloc:
		emitCompiledAlloc(buf, prog, ins, scalars)

	case ir.IRLoad:
		emitCompiledLoad(buf, prog, id, ins, scalars)

	case ir.IRStore:
		if uVal, ok := unboxedValueExpr(prog, ir.InstrID(ins.Arg1), scalars); ok {
			fmt.Fprintf(buf, "\t\t\tval := runtime.Value(%s)\n", uVal)
		} else {
			fmt.Fprintf(buf, "\t\t\tval, err := evalAOT(vm, %d, env)\n", ins.Arg1)
			buf.WriteString("\t\t\tif err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n")
			buf.WriteString("\t\t\tval = runtime.Deref(val)\n")
		}
		if ins.SubOp == ir.StoreArrayElem {
			fmt.Fprintf(buf, "\t\t\tarrVal, err := evalAOT(vm, %d, env)\n", ins.Arg0)
			buf.WriteString("\t\t\tif err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n")
			fmt.Fprintf(buf, "\t\t\tidxVal, err := evalAOT(vm, %d, env)\n", ins.Aux)
			buf.WriteString("\t\t\tif err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n")
			buf.WriteString("\t\t\tarr, okA := runtime.Deref(arrVal).(*runtime.ArrayVal)\n")
			buf.WriteString("\t\t\tiv, okI := runtime.Deref(idxVal).(runtime.IntVal)\n")
			buf.WriteString("\t\t\tif !okA || !okI {\n\t\t\t\treturn nil, fmt.Errorf(\"array store requires array and int index\")\n\t\t\t}\n")
			buf.WriteString("\t\t\tif iv.V < 0 || int(iv.V) >= len(arr.Elems) {\n\t\t\t\treturn nil, fmt.Errorf(\"array index %d out of bounds\", iv.V)\n\t\t\t}\n")
			buf.WriteString("\t\t\tarr.Elems[iv.V] = val\n")
			buf.WriteString("\t\t\treturn runtime.Unit, nil\n")
		} else {
			sym := token.StringID(ins.Data)
			fmt.Fprintf(buf, "\t\t\tif existing, ok := env[token.StringID(%d)]; ok {\n", sym)
			buf.WriteString("\t\t\t\tif cell, isCell := existing.(*runtime.RefCell); isCell {\n")
			buf.WriteString("\t\t\t\t\tcell.Val = val\n\t\t\t\t\treturn runtime.Unit, nil\n")
			buf.WriteString("\t\t\t\t}\n\t\t\t}\n")
			fmt.Fprintf(buf, "\t\t\tif existing, ok := vm.Globals[token.StringID(%d)]; ok {\n", sym)
			buf.WriteString("\t\t\t\tif cell, isCell := existing.(*runtime.RefCell); isCell {\n")
			buf.WriteString("\t\t\t\t\tcell.Val = val\n\t\t\t\t\treturn runtime.Unit, nil\n")
			buf.WriteString("\t\t\t\t}\n\t\t\t}\n")
			fmt.Fprintf(buf, "\t\t\tenv[token.StringID(%d)] = &runtime.RefCell{Val: val}\n", sym)
			buf.WriteString("\t\t\treturn runtime.Unit, nil\n")
		}
	}
}

func emitCompiledPrim(buf *bytes.Buffer, prog *ir.Program, id ir.InstrID, ins ir.IRInstr, scalars *aotScalarInfo) {
	switch ins.SubOp {
	case ir.PrimSeqBlock:
		if ins.Arg1 == 0 {
			buf.WriteString("\t\t\treturn runtime.Unit, nil\n")
			return
		}
		for j := uint32(0); j < ins.Arg1-1; j++ {
			childID := prog.Extra[ins.Arg0+j]
			fmt.Fprintf(buf, "\t\t\tif _, err := evalAOT(vm, %d, env); err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n", childID)
		}
		lastChild := prog.Extra[ins.Arg0+ins.Arg1-1]
		fmt.Fprintf(buf, "\t\t\tid = %d\n\t\t\tcontinue tcoLoop\n", lastChild)
		return

	case ir.PrimWhile:
		if condExpr, ok := unboxedBoolExpr(prog, ir.InstrID(ins.Arg0), scalars); ok {
			buf.WriteString("\t\t\t// Unboxed while-loop condition specialization\n")
			fmt.Fprintf(buf, "\t\t\tfor %s {\n", condExpr)
			fmt.Fprintf(buf, "\t\t\t\tif _, err := evalAOT(vm, %d, env); err != nil {\n\t\t\t\t\treturn nil, err\n\t\t\t\t}\n", ins.Arg1)
			buf.WriteString("\t\t\t}\n")
			buf.WriteString("\t\t\treturn runtime.Unit, nil\n")
			return
		}
		buf.WriteString("\t\t\tfor {\n")
		fmt.Fprintf(buf, "\t\t\t\tcVal, err := evalAOT(vm, %d, env)\n", ins.Arg0)
		buf.WriteString("\t\t\t\tif err != nil {\n\t\t\t\t\treturn nil, err\n\t\t\t\t}\n")
		buf.WriteString("\t\t\t\tbv, ok := runtime.Deref(cVal).(runtime.BoolVal)\n")
		buf.WriteString("\t\t\t\tif !ok || !bv.V {\n\t\t\t\t\tbreak\n\t\t\t\t}\n")
		fmt.Fprintf(buf, "\t\t\t\tif _, err := evalAOT(vm, %d, env); err != nil {\n\t\t\t\t\treturn nil, err\n\t\t\t\t}\n", ins.Arg1)
		buf.WriteString("\t\t\t}\n")
		buf.WriteString("\t\t\treturn runtime.Unit, nil\n")
		return

	case ir.PrimForTo, ir.PrimForIn:
		fmt.Fprintf(buf, "\t\t\trVal, err := evalAOT(vm, %d, env)\n", ins.Arg0)
		buf.WriteString("\t\t\tif err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n")
		fmt.Fprintf(buf, "\t\t\tfor _, elem := range runtime.ListToSlice(runtime.Deref(rVal)) {\n")
		fmt.Fprintf(buf, "\t\t\t\tenv[token.StringID(%d)] = elem\n", token.StringID(ins.Data))
		fmt.Fprintf(buf, "\t\t\t\tif _, err := evalAOT(vm, %d, env); err != nil {\n\t\t\t\t\treturn nil, err\n\t\t\t\t}\n", ins.Arg1)
		buf.WriteString("\t\t\t}\n")
		buf.WriteString("\t\t\treturn runtime.Unit, nil\n")
		return

	case ir.PrimTryFinally:
		fmt.Fprintf(buf, "\t\t\tres, err := evalAOT(vm, %d, env)\n", ins.Arg0)
		fmt.Fprintf(buf, "\t\t\tcleanupVal, cErr := evalAOT(vm, %d, env)\n", ins.Arg1)
		if ins.Flags&ir.IRFlagDispose != 0 && int(ins.Arg1) < len(prog.Instrs) {
			disposeSym := token.StringID(prog.Instrs[ins.Arg1].Data)
			fmt.Fprintf(buf, "\t\t\tif symVal, ok := env[token.StringID(%d)]; ok {\n", disposeSym)
			buf.WriteString("\t\t\t\truntime.DisposeValue(symVal)\n")
			buf.WriteString("\t\t\t} else {\n\t\t\t\truntime.DisposeValue(cleanupVal)\n\t\t\t}\n")
		} else {
			buf.WriteString("\t\t\t_ = cleanupVal\n")
		}
		buf.WriteString("\t\t\tif err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n")
		buf.WriteString("\t\t\tif cErr != nil {\n\t\t\t\treturn nil, cErr\n\t\t\t}\n")
		buf.WriteString("\t\t\treturn res, nil\n")
		return

	case ir.PrimTryWith:
		fmt.Fprintf(buf, "\t\t\tres, err := evalAOT(vm, %d, env)\n", ins.Arg0)
		buf.WriteString("\t\t\tif err != nil {\n")
		fmt.Fprintf(buf, "\t\t\t\tid = %d\n\t\t\t\tcontinue tcoLoop\n", ins.Arg1)
		buf.WriteString("\t\t\t}\n")
		buf.WriteString("\t\t\treturn res, nil\n")
		return

	case ir.PrimRaise:
		fmt.Fprintf(buf, "\t\t\tv, err := evalAOT(vm, %d, env)\n", ins.Arg0)
		buf.WriteString("\t\t\tif err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n")
		buf.WriteString("\t\t\treturn nil, fmt.Errorf(\"exception: %s\", runtime.Deref(v).String())\n")
		return

	case ir.PrimSeqGen:
		fmt.Fprintf(buf, "\t\t\tid = %d\n\t\t\tcontinue tcoLoop\n", ins.Arg0)
		return

	case ir.PrimNeg:
		if iExpr, ok := unboxedIntExpr(prog, id, scalars); ok {
			buf.WriteString("\t\t\t// Unboxed int64 specialization\n")
			fmt.Fprintf(buf, "\t\t\treturn runtime.IntVal{V: %s}, nil\n", iExpr)
			return
		}
		if fExpr, ok := unboxedFloatExpr(prog, id, scalars); ok {
			buf.WriteString("\t\t\t// Unboxed float64 specialization\n")
			fmt.Fprintf(buf, "\t\t\treturn runtime.FloatVal{V: %s}, nil\n", fExpr)
			return
		}
		fmt.Fprintf(buf, "\t\t\tv, err := evalAOT(vm, %d, env)\n", ins.Arg0)
		buf.WriteString("\t\t\tif err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n")
		buf.WriteString("\t\t\tswitch nv := runtime.Deref(v).(type) {\n")
		buf.WriteString("\t\t\tcase runtime.IntVal:\n\t\t\t\treturn runtime.IntVal{V: -nv.V}, nil\n")
		buf.WriteString("\t\t\tcase runtime.FloatVal:\n\t\t\t\treturn runtime.FloatVal{V: -nv.V}, nil\n")
		buf.WriteString("\t\t\tdefault:\n\t\t\t\treturn nil, fmt.Errorf(\"unary negation requires numeric value\")\n\t\t\t}\n")
		return

	case ir.PrimNot:
		if bExpr, ok := unboxedBoolExpr(prog, id, scalars); ok {
			buf.WriteString("\t\t\t// Unboxed bool specialization\n")
			fmt.Fprintf(buf, "\t\t\treturn runtime.BoolVal{V: %s}, nil\n", bExpr)
			return
		}
		fmt.Fprintf(buf, "\t\t\tv, err := evalAOT(vm, %d, env)\n", ins.Arg0)
		buf.WriteString("\t\t\tif err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n")
		buf.WriteString("\t\t\tbv, ok := runtime.Deref(v).(runtime.BoolVal)\n")
		buf.WriteString("\t\t\tif !ok {\n\t\t\t\treturn nil, fmt.Errorf(\"not requires bool\")\n\t\t\t}\n")
		buf.WriteString("\t\t\treturn runtime.BoolVal{V: !bv.V}, nil\n")
		return
	}

	// Unboxed scalar specialization for binary arithmetic, comparison, and boolean logic
	if iExpr, ok := unboxedIntExpr(prog, id, scalars); ok {
		buf.WriteString("\t\t\t// Unboxed int64 specialization\n")
		fmt.Fprintf(buf, "\t\t\treturn runtime.IntVal{V: %s}, nil\n", iExpr)
		return
	}
	if fExpr, ok := unboxedFloatExpr(prog, id, scalars); ok {
		buf.WriteString("\t\t\t// Unboxed float64 specialization\n")
		fmt.Fprintf(buf, "\t\t\treturn runtime.FloatVal{V: %s}, nil\n", fExpr)
		return
	}
	if bExpr, ok := unboxedBoolExpr(prog, id, scalars); ok {
		buf.WriteString("\t\t\t// Unboxed bool specialization\n")
		fmt.Fprintf(buf, "\t\t\treturn runtime.BoolVal{V: %s}, nil\n", bExpr)
		return
	}
	if sExpr, ok := unboxedStringExpr(prog, id, scalars); ok {
		buf.WriteString("\t\t\t// Unboxed string specialization\n")
		fmt.Fprintf(buf, "\t\t\treturn runtime.StringVal{V: %s}, nil\n", sExpr)
		return
	}
	if ins.SubOp == ir.PrimDiv || ins.SubOp == ir.PrimMod {
		if lhs, okL := unboxedIntExpr(prog, ir.InstrID(ins.Arg0), scalars); okL {
			if rhs, okR := unboxedIntExpr(prog, ir.InstrID(ins.Arg1), scalars); okR {
				op := "/"
				if ins.SubOp == ir.PrimMod {
					op = "%"
				}
				buf.WriteString("\t\t\t// Unboxed int64 division/modulo specialization\n")
				fmt.Fprintf(buf, "\t\t\trhsInt := %s\n", rhs)
				buf.WriteString("\t\t\tif rhsInt == 0 {\n\t\t\t\treturn nil, fmt.Errorf(\"division by zero\")\n\t\t\t}\n")
				fmt.Fprintf(buf, "\t\t\treturn runtime.IntVal{V: (%s %s rhsInt)}, nil\n", lhs, op)
				return
			}
		}
		if ins.SubOp == ir.PrimDiv {
			if lhs, okL := unboxedFloatExpr(prog, ir.InstrID(ins.Arg0), scalars); okL {
				if rhs, okR := unboxedFloatExpr(prog, ir.InstrID(ins.Arg1), scalars); okR {
					buf.WriteString("\t\t\t// Unboxed float64 division specialization\n")
					fmt.Fprintf(buf, "\t\t\trhsFloat := %s\n", rhs)
					buf.WriteString("\t\t\tif rhsFloat == 0 {\n\t\t\t\treturn nil, fmt.Errorf(\"float division by zero\")\n\t\t\t}\n")
					fmt.Fprintf(buf, "\t\t\treturn runtime.FloatVal{V: (%s / rhsFloat)}, nil\n", lhs)
					return
				}
			}
		}
	}

	// Binary primitives fallback
	fmt.Fprintf(buf, "\t\t\tlhs, err := evalAOT(vm, %d, env)\n", ins.Arg0)
	buf.WriteString("\t\t\tif err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n")
	fmt.Fprintf(buf, "\t\t\trhs, err := evalAOT(vm, %d, env)\n", ins.Arg1)
	buf.WriteString("\t\t\tif err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n")
	fmt.Fprintf(buf, "\t\t\treturn runtime.EvalBinaryPrim(%d, %d, lhs, rhs)\n", ins.SubOp, ins.Flags)
}

func emitCompiledAlloc(buf *bytes.Buffer, prog *ir.Program, ins ir.IRInstr, scalars *aotScalarInfo) {
	switch ins.SubOp {
	case ir.AllocTuple, ir.AllocArray, ir.AllocList:
		fmt.Fprintf(buf, "\t\t\telems := make([]runtime.Value, %d)\n", ins.Arg1)
		needsEval := false
		for i := uint32(0); i < ins.Arg1; i++ {
			ch := ir.InstrID(prog.Extra[ins.Arg0+i])
			if _, ok := unboxedValueExpr(prog, ch, scalars); !ok {
				needsEval = true
				break
			}
		}
		if needsEval {
			buf.WriteString("\t\t\tvar v runtime.Value\n\t\t\tvar err error\n")
		}
		for i := uint32(0); i < ins.Arg1; i++ {
			ch := ir.InstrID(prog.Extra[ins.Arg0+i])
			if uVal, ok := unboxedValueExpr(prog, ch, scalars); ok {
				fmt.Fprintf(buf, "\t\t\telems[%d] = %s\n", i, uVal)
			} else {
				fmt.Fprintf(buf, "\t\t\tv, err = evalAOT(vm, %d, env)\n", ch)
				buf.WriteString("\t\t\tif err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n")
				fmt.Fprintf(buf, "\t\t\telems[%d] = runtime.Deref(v)\n", i)
			}
		}
		switch ins.SubOp {
		case ir.AllocArray:
			buf.WriteString("\t\t\treturn &runtime.ArrayVal{Elems: elems}, nil\n")
		case ir.AllocList:
			buf.WriteString("\t\t\treturn runtime.SliceToList(elems), nil\n")
		default:
			buf.WriteString("\t\t\treturn runtime.TupleVal{Elems: elems}, nil\n")
		}

	case ir.AllocRecord:
		fmt.Fprintf(buf, "\t\t\tfields := make(map[token.StringID]runtime.Value, %d)\n", ins.Arg1/2+4)
		fmt.Fprintf(buf, "\t\t\tbyName := make(map[string]runtime.Value, %d)\n", ins.Arg1/2+4)
		if ins.Flags != 0 {
			fmt.Fprintf(buf, "\t\t\tbaseVal, err := evalAOT(vm, %d, env)\n", ins.Aux)
			buf.WriteString("\t\t\tif err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n")
			buf.WriteString("\t\t\tif baseRec, ok := runtime.Deref(baseVal).(*runtime.RecordVal); ok {\n")
			buf.WriteString("\t\t\t\tfor k, v := range baseRec.Fields {\n\t\t\t\t\tfields[k] = v\n\t\t\t\t}\n")
			buf.WriteString("\t\t\t\tfor k, v := range baseRec.ByName {\n\t\t\t\t\tbyName[k] = v\n\t\t\t\t}\n")
			buf.WriteString("\t\t\t}\n")
		}
		needsEval := false
		for i := uint32(0); i < ins.Arg1; i += 2 {
			valInstr := ir.InstrID(prog.Extra[ins.Arg0+i+1])
			if _, ok := unboxedValueExpr(prog, valInstr, scalars); !ok {
				needsEval = true
				break
			}
		}
		if needsEval {
			buf.WriteString("\t\t\tvar fv runtime.Value\n\t\t\tvar fErr error\n")
		}
		for i := uint32(0); i < ins.Arg1; i += 2 {
			fID := token.StringID(prog.Extra[ins.Arg0+i])
			valInstr := ir.InstrID(prog.Extra[ins.Arg0+i+1])
			fName := prog.Strings.String(fID)
			if uVal, ok := unboxedValueExpr(prog, valInstr, scalars); ok {
				fmt.Fprintf(buf, "\t\t\tfields[token.StringID(%d)] = %s\n", fID, uVal)
				fmt.Fprintf(buf, "\t\t\tbyName[%q] = fields[token.StringID(%d)]\n", fName, fID)
			} else {
				fmt.Fprintf(buf, "\t\t\tfv, fErr = evalAOT(vm, %d, env)\n", valInstr)
				buf.WriteString("\t\t\tif fErr != nil {\n\t\t\t\treturn nil, fErr\n\t\t\t}\n")
				buf.WriteString("\t\t\tfv = runtime.Deref(fv)\n")
				fmt.Fprintf(buf, "\t\t\tfields[token.StringID(%d)] = fv\n", fID)
				fmt.Fprintf(buf, "\t\t\tbyName[%q] = fv\n", fName)
			}
		}
		buf.WriteString("\t\t\treturn &runtime.RecordVal{Fields: fields, ByName: byName}, nil\n")

	case ir.AllocADT:
		tag := token.StringID(ins.Data)
		tagName := prog.Strings.String(tag)
		if ir.InstrID(ins.Arg0) != ir.NoInstr {
			if uVal, ok := unboxedValueExpr(prog, ir.InstrID(ins.Arg0), scalars); ok {
				fmt.Fprintf(buf, "\t\t\treturn runtime.ADTVal{Tag: token.StringID(%d), TagName: %q, Payload: %s}, nil\n", tag, tagName, uVal)
			} else {
				fmt.Fprintf(buf, "\t\t\tv, err := evalAOT(vm, %d, env)\n", ins.Arg0)
				buf.WriteString("\t\t\tif err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n")
				fmt.Fprintf(buf, "\t\t\treturn runtime.ADTVal{Tag: token.StringID(%d), TagName: %q, Payload: runtime.Deref(v)}, nil\n", tag, tagName)
			}
		} else {
			fmt.Fprintf(buf, "\t\t\treturn runtime.ADTVal{Tag: token.StringID(%d), TagName: %q}, nil\n", tag, tagName)
		}

	default:
		buf.WriteString("\t\t\treturn runtime.Unit, nil\n")
	}
}

func emitCompiledLoad(buf *bytes.Buffer, prog *ir.Program, id ir.InstrID, ins ir.IRInstr, scalars *aotScalarInfo) {
	if ins.SubOp == ir.LoadRecordField && ins.Aux != 0 {
		qualSym := token.StringID(ins.Aux)
		fmt.Fprintf(buf, "\t\t\tif v, ok := env[token.StringID(%d)]; ok {\n", qualSym)
		buf.WriteString("\t\t\t\treturn runtime.Deref(v), nil\n\t\t\t}\n")
		fmt.Fprintf(buf, "\t\t\tif v, ok := vm.Globals[token.StringID(%d)]; ok {\n", qualSym)
		buf.WriteString("\t\t\t\treturn runtime.Deref(v), nil\n\t\t\t}\n")
	} else if uVal, ok := unboxedValueExpr(prog, id, scalars); ok {
		buf.WriteString("\t\t\t// Unboxed composite/ADT load specialization\n")
		fmt.Fprintf(buf, "\t\t\treturn %s, nil\n", uVal)
		return
	}

	if int(ins.Arg0) < len(prog.Instrs) && prog.Instrs[ins.Arg0].Kind == ir.IRVar {
		fmt.Fprintf(buf, "\t\t\ttarget := aotLoadValue(vm, env, token.StringID(%d))\n", prog.Instrs[ins.Arg0].Data)
	} else {
		fmt.Fprintf(buf, "\t\t\ttarget, err := evalAOT(vm, %d, env)\n", ins.Arg0)
		buf.WriteString("\t\t\tif err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n")
		buf.WriteString("\t\t\ttarget = runtime.Deref(target)\n")
	}

	switch ins.SubOp {
	case ir.LoadTupleElem:
		buf.WriteString("\t\t\ttup, ok := target.(runtime.TupleVal)\n")
		fmt.Fprintf(buf, "\t\t\tif !ok || %d >= len(tup.Elems) {\n", ins.Arg1)
		fmt.Fprintf(buf, "\t\t\t\treturn nil, fmt.Errorf(\"tuple element index %d out of bounds\")\n", ins.Arg1)
		buf.WriteString("\t\t\t}\n")
		fmt.Fprintf(buf, "\t\t\treturn tup.Elems[%d], nil\n", ins.Arg1)

	case ir.LoadRecordField:
		fID := token.StringID(ins.Data)
		fName := prog.Strings.String(fID)
		buf.WriteString("\t\t\trec, ok := target.(*runtime.RecordVal)\n")
		buf.WriteString("\t\t\tif !ok {\n\t\t\t\treturn nil, fmt.Errorf(\"field access on non-record value %v\", target)\n\t\t\t}\n")
		fmt.Fprintf(buf, "\t\t\tif v, exists := rec.Fields[token.StringID(%d)]; exists {\n\t\t\t\treturn v, nil\n\t\t\t}\n", fID)
		fmt.Fprintf(buf, "\t\t\tif v, exists := rec.ByName[%q]; exists {\n\t\t\t\treturn v, nil\n\t\t\t}\n", fName)
		fmt.Fprintf(buf, "\t\t\treturn nil, fmt.Errorf(\"missing record field %%q\", %q)\n", fName)

	case ir.LoadADTTag:
		buf.WriteString("\t\t\tif adt, ok := target.(runtime.ADTVal); ok {\n")
		buf.WriteString("\t\t\t\treturn runtime.IntVal{V: int64(adt.Tag)}, nil\n\t\t\t}\n")
		buf.WriteString("\t\t\treturn runtime.IntVal{V: 0}, nil\n")

	case ir.LoadADTPayload:
		buf.WriteString("\t\t\tif adt, ok := target.(runtime.ADTVal); ok && adt.Payload != nil {\n")
		buf.WriteString("\t\t\t\treturn adt.Payload, nil\n\t\t\t}\n")
		buf.WriteString("\t\t\treturn runtime.Unit, nil\n")

	case ir.LoadListIsEmpty:
		buf.WriteString("\t\t\tif lv, ok := target.(*runtime.ListVal); ok && (lv == nil || lv.Empty) {\n")
		buf.WriteString("\t\t\t\treturn runtime.IntVal{V: 1}, nil\n\t\t\t}\n")
		buf.WriteString("\t\t\treturn runtime.IntVal{V: 0}, nil\n")

	case ir.LoadListHead:
		buf.WriteString("\t\t\tif lv, ok := target.(*runtime.ListVal); ok && lv != nil && !lv.Empty {\n")
		buf.WriteString("\t\t\t\treturn lv.Head, nil\n\t\t\t}\n")
		buf.WriteString("\t\t\treturn nil, fmt.Errorf(\"head of empty list\")\n")

	case ir.LoadListTail:
		buf.WriteString("\t\t\tif lv, ok := target.(*runtime.ListVal); ok && lv != nil && !lv.Empty {\n")
		buf.WriteString("\t\t\t\tif lv.Tail == nil {\n\t\t\t\t\treturn runtime.EmptyList, nil\n\t\t\t\t}\n")
		buf.WriteString("\t\t\t\treturn lv.Tail, nil\n\t\t\t}\n")
		buf.WriteString("\t\t\treturn runtime.EmptyList, nil\n")

	default:
		buf.WriteString("\t\t\treturn target, nil\n")
	}
}

// BuildBinary compiles an FML source file at srcPath into a standalone native
// executable at outBinPath using the Go toolchain.
func BuildBinary(srcPath, outBinPath string) error {
	prog, err := CompileFile(srcPath)
	if err != nil {
		return err
	}
	goSrc, err := EmitGoSource(prog)
	if err != nil {
		return err
	}

	absOut, err := filepath.Abs(outBinPath)
	if err != nil {
		return err
	}

	modRoot := findModuleRoot()
	tmpDir, err := os.MkdirTemp(modRoot, ".fml-aot-*")
	if err != nil {
		return fmt.Errorf("create temp build dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	mainGoPath := filepath.Join(tmpDir, "main.go")
	if err := os.WriteFile(mainGoPath, goSrc, 0600); err != nil {
		return fmt.Errorf("write generated Go source: %w", err)
	}

	cmd := exec.Command("go", "build", "-o", absOut, mainGoPath)
	cmd.Dir = modRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build failed: %w\n%s", err, string(out))
	}
	return nil
}

func findModuleRoot() string {
	if _, file, _, ok := runtime.Caller(0); ok {
		dir := filepath.Dir(file)
		for i := 0; i < 5; i++ {
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
				return dir
			}
			dir = filepath.Dir(dir)
		}
	}
	wd, _ := os.Getwd()
	return wd
}
