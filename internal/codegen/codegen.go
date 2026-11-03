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
	buf.WriteString("\t\"os\"\n\n")
	buf.WriteString("\t\"fml/internal/ir\"\n")
	buf.WriteString("\t\"fml/internal/runtime\"\n")
	buf.WriteString("\t\"fml/internal/stdlib\"\n")
	buf.WriteString("\t\"fml/internal/token\"\n")
	buf.WriteString(")\n\n")

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
	selfTailCalls := analyzeSelfTailCalls(prog)
	buf.WriteString("func evalAOT(vm *runtime.VM, id ir.InstrID, env map[token.StringID]runtime.Value) (runtime.Value, error) {\n")
	buf.WriteString("tcoLoop:\n")
	buf.WriteString("\tfor {\n")
	buf.WriteString("\t\tswitch id {\n")
	for i, ins := range prog.Instrs {
		emitCompiledCase(&buf, prog, ir.InstrID(i), ins, selfTailCalls)
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

func emitCompiledCase(buf *bytes.Buffer, prog *ir.Program, id ir.InstrID, ins ir.IRInstr, selfTailCalls map[ir.InstrID]*selfTailCallInfo) {
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
		fmt.Fprintf(buf, "\t\t\tscrutVal, err := evalAOT(vm, %d, env)\n", ins.Arg0)
		buf.WriteString("\t\t\tif err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n")
		buf.WriteString("\t\t\tscrutTag := runtime.ExtractSwitchTag(runtime.Deref(scrutVal), vm.Prog.Strings)\n")
		buf.WriteString("\t\t\tswitch scrutTag {\n")
		count := uint32(ins.Data)
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
		emitCompiledPrim(buf, prog, ins)

	case ir.IRAlloc:
		emitCompiledAlloc(buf, prog, ins)

	case ir.IRLoad:
		emitCompiledLoad(buf, prog, ins)

	case ir.IRStore:
		fmt.Fprintf(buf, "\t\t\tval, err := evalAOT(vm, %d, env)\n", ins.Arg1)
		buf.WriteString("\t\t\tif err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n")
		buf.WriteString("\t\t\tval = runtime.Deref(val)\n")
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

func emitCompiledPrim(buf *bytes.Buffer, prog *ir.Program, ins ir.IRInstr) {
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
		fmt.Fprintf(buf, "\t\t\tv, err := evalAOT(vm, %d, env)\n", ins.Arg0)
		buf.WriteString("\t\t\tif err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n")
		buf.WriteString("\t\t\tswitch nv := runtime.Deref(v).(type) {\n")
		buf.WriteString("\t\t\tcase runtime.IntVal:\n\t\t\t\treturn runtime.IntVal{V: -nv.V}, nil\n")
		buf.WriteString("\t\t\tcase runtime.FloatVal:\n\t\t\t\treturn runtime.FloatVal{V: -nv.V}, nil\n")
		buf.WriteString("\t\t\tdefault:\n\t\t\t\treturn nil, fmt.Errorf(\"unary negation requires numeric value\")\n\t\t\t}\n")
		return

	case ir.PrimNot:
		fmt.Fprintf(buf, "\t\t\tv, err := evalAOT(vm, %d, env)\n", ins.Arg0)
		buf.WriteString("\t\t\tif err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n")
		buf.WriteString("\t\t\tbv, ok := runtime.Deref(v).(runtime.BoolVal)\n")
		buf.WriteString("\t\t\tif !ok {\n\t\t\t\treturn nil, fmt.Errorf(\"not requires bool\")\n\t\t\t}\n")
		buf.WriteString("\t\t\treturn runtime.BoolVal{V: !bv.V}, nil\n")
		return
	}

	// Binary primitives
	fmt.Fprintf(buf, "\t\t\tlhs, err := evalAOT(vm, %d, env)\n", ins.Arg0)
	buf.WriteString("\t\t\tif err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n")
	fmt.Fprintf(buf, "\t\t\trhs, err := evalAOT(vm, %d, env)\n", ins.Arg1)
	buf.WriteString("\t\t\tif err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n")
	fmt.Fprintf(buf, "\t\t\treturn runtime.EvalBinaryPrim(%d, %d, lhs, rhs)\n", ins.SubOp, ins.Flags)
}

func emitCompiledAlloc(buf *bytes.Buffer, prog *ir.Program, ins ir.IRInstr) {
	switch ins.SubOp {
	case ir.AllocTuple, ir.AllocArray, ir.AllocList:
		fmt.Fprintf(buf, "\t\t\telems := make([]runtime.Value, %d)\n", ins.Arg1)
		if ins.Arg1 > 0 {
			buf.WriteString("\t\t\tvar v runtime.Value\n\t\t\tvar err error\n")
		}
		for i := uint32(0); i < ins.Arg1; i++ {
			ch := ir.InstrID(prog.Extra[ins.Arg0+i])
			fmt.Fprintf(buf, "\t\t\tv, err = evalAOT(vm, %d, env)\n", ch)
			buf.WriteString("\t\t\tif err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n")
			fmt.Fprintf(buf, "\t\t\telems[%d] = runtime.Deref(v)\n", i)
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
		if ins.Arg1 > 0 {
			buf.WriteString("\t\t\tvar fv runtime.Value\n\t\t\tvar fErr error\n")
		}
		for i := uint32(0); i < ins.Arg1; i += 2 {
			fID := token.StringID(prog.Extra[ins.Arg0+i])
			valInstr := ir.InstrID(prog.Extra[ins.Arg0+i+1])
			fName := prog.Strings.String(fID)
			fmt.Fprintf(buf, "\t\t\tfv, fErr = evalAOT(vm, %d, env)\n", valInstr)
			buf.WriteString("\t\t\tif fErr != nil {\n\t\t\t\treturn nil, fErr\n\t\t\t}\n")
			buf.WriteString("\t\t\tfv = runtime.Deref(fv)\n")
			fmt.Fprintf(buf, "\t\t\tfields[token.StringID(%d)] = fv\n", fID)
			fmt.Fprintf(buf, "\t\t\tbyName[%q] = fv\n", fName)
		}
		buf.WriteString("\t\t\treturn &runtime.RecordVal{Fields: fields, ByName: byName}, nil\n")

	case ir.AllocADT:
		tag := token.StringID(ins.Data)
		tagName := prog.Strings.String(tag)
		if ir.InstrID(ins.Arg0) != ir.NoInstr {
			fmt.Fprintf(buf, "\t\t\tv, err := evalAOT(vm, %d, env)\n", ins.Arg0)
			buf.WriteString("\t\t\tif err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n")
			fmt.Fprintf(buf, "\t\t\treturn runtime.ADTVal{Tag: token.StringID(%d), TagName: %q, Payload: runtime.Deref(v)}, nil\n", tag, tagName)
		} else {
			fmt.Fprintf(buf, "\t\t\treturn runtime.ADTVal{Tag: token.StringID(%d), TagName: %q}, nil\n", tag, tagName)
		}

	default:
		buf.WriteString("\t\t\treturn runtime.Unit, nil\n")
	}
}

func emitCompiledLoad(buf *bytes.Buffer, prog *ir.Program, ins ir.IRInstr) {
	if ins.SubOp == ir.LoadRecordField && ins.Aux != 0 {
		qualSym := token.StringID(ins.Aux)
		fmt.Fprintf(buf, "\t\t\tif v, ok := env[token.StringID(%d)]; ok {\n", qualSym)
		buf.WriteString("\t\t\t\treturn runtime.Deref(v), nil\n\t\t\t}\n")
		fmt.Fprintf(buf, "\t\t\tif v, ok := vm.Globals[token.StringID(%d)]; ok {\n", qualSym)
		buf.WriteString("\t\t\t\treturn runtime.Deref(v), nil\n\t\t\t}\n")
	}

	fmt.Fprintf(buf, "\t\t\ttarget, err := evalAOT(vm, %d, env)\n", ins.Arg0)
	buf.WriteString("\t\t\tif err != nil {\n\t\t\t\treturn nil, err\n\t\t\t}\n")
	buf.WriteString("\t\t\ttarget = runtime.Deref(target)\n")

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
