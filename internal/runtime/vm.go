package runtime

import (
	"fmt"
	"io"
	"math"
	"os"

	"fml/internal/ir"
	"fml/internal/token"
)

// VM executes a lowered 10-opcode ir.Program with tail-call optimization.
//
// Jaime says: Ten opcodes, one switch loop, and an explicit environment map.
// Tail calls jump back to the top of Eval without growing the Go call stack.
type VM struct {
	Prog       *ir.Program
	Globals    map[token.StringID]Value
	Stdout     io.Writer
	CustomEval func(vm *VM, id ir.InstrID, env map[token.StringID]Value) (Value, error)
}

// NewVM initializes a VM for prog with core built-in functions registered.
func NewVM(prog *ir.Program) *VM {
	vm := &VM{
		Prog:    prog,
		Globals: make(map[token.StringID]Value),
		Stdout:  os.Stdout,
	}
	vm.registerCoreBuiltins()
	return vm
}

// DefineGlobal binds a named value in the root global environment.
func (vm *VM) DefineGlobal(name string, val Value) {
	id := vm.Prog.Strings.InternString(name)
	vm.Globals[id] = val
}

// DefineBuiltin registers a curried native function in the root environment.
func (vm *VM) DefineBuiltin(name string, arity int, fn func(vm *VM, args []Value) (Value, error)) {
	vm.DefineGlobal(name, &BuiltinVal{
		Name:  name,
		Arity: arity,
		Fn:    fn,
	})
}

func (vm *VM) registerCoreBuiltins() {
	vm.DefineBuiltin("id", 1, func(_ *VM, args []Value) (Value, error) {
		return Deref(args[0]), nil
	})
	vm.DefineBuiltin("ignore", 1, func(_ *VM, _ []Value) (Value, error) {
		return Unit, nil
	})
	vm.DefineBuiltin("fst", 1, func(_ *VM, args []Value) (Value, error) {
		tup, ok := Deref(args[0]).(TupleVal)
		if !ok || len(tup.Elems) < 1 {
			return nil, fmt.Errorf("fst expects non-empty tuple")
		}
		return tup.Elems[0], nil
	})
	vm.DefineBuiltin("snd", 1, func(_ *VM, args []Value) (Value, error) {
		tup, ok := Deref(args[0]).(TupleVal)
		if !ok || len(tup.Elems) < 2 {
			return nil, fmt.Errorf("snd expects 2-tuple")
		}
		return tup.Elems[1], nil
	})
	vm.DefineBuiltin("print", 1, func(v *VM, args []Value) (Value, error) {
		fmt.Fprint(v.Stdout, Deref(args[0]).String())
		return Unit, nil
	})
	vm.DefineBuiltin("printfn", 1, func(v *VM, args []Value) (Value, error) {
		a0 := Deref(args[0])
		if sv, ok := a0.(StringVal); ok && (sv.V == "%d" || sv.V == "%s" || sv.V == "%v") {
			return &BuiltinVal{
				Name:  "printfn_fmt",
				Arity: 1,
				Fn: func(v2 *VM, a2 []Value) (Value, error) {
					fmt.Fprintln(v2.Stdout, Deref(a2[0]).String())
					return Unit, nil
				},
			}, nil
		}
		fmt.Fprintln(v.Stdout, a0.String())
		return Unit, nil
	})
}

// Run executes the root instruction of the VM's Program.
func (vm *VM) Run() (Value, error) {
	if vm.Prog.Root == ir.NoInstr {
		return Unit, nil
	}
	env := make(map[token.StringID]Value, len(vm.Globals)+16)
	for k, v := range vm.Globals {
		env[k] = v
	}
	val, err := vm.Eval(vm.Prog.Root, env)
	if err != nil {
		return nil, err
	}
	for k, v := range env {
		vm.Globals[k] = v
	}
	return Deref(val), nil
}

// Apply invokes a callable Value (ClosureVal or BuiltinVal) with arg.
func (vm *VM) Apply(fnVal, argVal Value) (Value, error) {
	fnVal = Deref(fnVal)
	argVal = Deref(argVal)

	switch fn := fnVal.(type) {
	case *ClosureVal:
		callEnv := make(map[token.StringID]Value, len(fn.Env)+2)
		for k, v := range fn.Env {
			callEnv[k] = v
		}
		if fn.SelfSym != token.NoString {
			callEnv[fn.SelfSym] = fn
		}
		if fn.Param != token.NoString {
			callEnv[fn.Param] = argVal
		}
		if vm.CustomEval != nil {
			return vm.CustomEval(vm, fn.Body, callEnv)
		}
		return vm.Eval(fn.Body, callEnv)

	case *BuiltinVal:
		nextArgs := make([]Value, len(fn.Args)+1)
		copy(nextArgs, fn.Args)
		nextArgs[len(fn.Args)] = argVal
		if len(nextArgs) < fn.Arity {
			return &BuiltinVal{
				Name:  fn.Name,
				Arity: fn.Arity,
				Args:  nextArgs,
				Fn:    fn.Fn,
			}, nil
		}
		return fn.Fn(vm, nextArgs)

	default:
		return nil, fmt.Errorf("attempt to call non-function value: %v (%T)", fnVal, fnVal)
	}
}

// Eval evaluates instruction id in env using a tail-call-optimized loop.
func (vm *VM) Eval(id ir.InstrID, env map[token.StringID]Value) (Value, error) {
tcoLoop:
	for {
		if id == ir.NoInstr || int(id) >= len(vm.Prog.Instrs) {
			return Unit, nil
		}
		ins := vm.Prog.Instrs[id]

		switch ins.Kind {
		case ir.IRConst:
			switch ins.SubOp {
			case ir.ConstUnit:
				return Unit, nil
			case ir.ConstBool:
				return BoolVal{V: ins.Data != 0}, nil
			case ir.ConstInt:
				return IntVal{V: int64(ins.Data)}, nil
			case ir.ConstFloat:
				return FloatVal{V: math.Float64frombits(ins.Data)}, nil
			case ir.ConstString:
				return StringVal{V: vm.Prog.Strings.String(token.StringID(ins.Data))}, nil
			default:
				return Unit, nil
			}

		case ir.IRVar:
			sym := token.StringID(ins.Data)
			if v, ok := env[sym]; ok {
				return Deref(v), nil
			}
			if v, ok := vm.Globals[sym]; ok {
				return Deref(v), nil
			}
			return nil, fmt.Errorf("unbound identifier %q", vm.Prog.Strings.String(sym))

		case ir.IRLet:
			sym := token.StringID(ins.Data)
			val, err := vm.Eval(ir.InstrID(ins.Arg0), env)
			if err != nil {
				return nil, err
			}
			if ins.Flags&ir.IRFlagRec != 0 {
				if clos, ok := val.(*ClosureVal); ok {
					clos.SelfSym = sym
					clos.Env[sym] = clos
					for prevSym, prevVal := range env {
						if prevClos, ok := prevVal.(*ClosureVal); ok && prevClos.SelfSym != token.NoString {
							prevClos.Env[sym] = clos
							clos.Env[prevSym] = prevClos
						}
					}
				}
			}
			if ins.Flags&ir.IRFlagMutable != 0 {
				val = &RefCell{Val: Deref(val)}
			}
			if sym != token.NoString {
				env[sym] = val
			}
			if ir.InstrID(ins.Arg1) != ir.NoInstr {
				if ins.Flags&ir.IRFlagDispose != 0 {
					res, err := vm.Eval(ir.InstrID(ins.Arg1), env)
					disposeValue(val)
					return res, err
				}
				id = ir.InstrID(ins.Arg1)
				continue tcoLoop
			}
			return Deref(val), nil

		case ir.IRFn:
			param := token.StringID(ins.Data)
			body := ir.InstrID(ins.Arg0)
			captured := make(map[token.StringID]Value, ins.Aux+2)
			if ins.Aux > 0 {
				for i := uint32(0); i < ins.Aux; i++ {
					capSym := token.StringID(vm.Prog.Extra[ins.Arg1+i])
					if v, ok := env[capSym]; ok {
						captured[capSym] = v
					} else if v, ok := vm.Globals[capSym]; ok {
						captured[capSym] = v
					}
				}
			} else if ins.Flags&ir.IRFlagClosed == 0 {
				for k, v := range env {
					captured[k] = v
				}
			}
			return &ClosureVal{
				Param: param,
				Body:  body,
				Env:   captured,
			}, nil

		case ir.IRApp:
			fnVal, err := vm.Eval(ir.InstrID(ins.Arg0), env)
			if err != nil {
				return nil, err
			}
			argVal, err := vm.Eval(ir.InstrID(ins.Arg1), env)
			if err != nil {
				return nil, err
			}
			fnVal = Deref(fnVal)
			argVal = Deref(argVal)

			// Tail-call optimization for ClosureVal
			if ins.Flags&ir.IRFlagTailCall != 0 {
				if clos, ok := fnVal.(*ClosureVal); ok {
					nextEnv := make(map[token.StringID]Value, len(clos.Env)+2)
					for k, v := range clos.Env {
						nextEnv[k] = v
					}
					if clos.SelfSym != token.NoString {
						nextEnv[clos.SelfSym] = clos
					}
					if clos.Param != token.NoString {
						nextEnv[clos.Param] = argVal
					}
					env = nextEnv
					id = clos.Body
					continue tcoLoop
				}
			}
			return vm.Apply(fnVal, argVal)

		case ir.IRSwitch:
			scrutVal, err := vm.Eval(ir.InstrID(ins.Arg0), env)
			if err != nil {
				return nil, err
			}
			scrutTag := extractSwitchTag(Deref(scrutVal), vm.Prog.Strings)
			count := uint32(ins.Data)
			matchedTarget := ir.InstrID(ins.Aux)
			for i := uint32(0); i < count; i++ {
				caseTag := vm.Prog.Extra[ins.Arg1+i*2]
				target := ir.InstrID(vm.Prog.Extra[ins.Arg1+i*2+1])
				if scrutTag == caseTag {
					matchedTarget = target
					break
				}
			}
			id = matchedTarget
			continue tcoLoop

		case ir.IRPrim:
			if ins.SubOp == ir.PrimSeqBlock {
				if ins.Arg1 == 0 {
					return Unit, nil
				}
				for i := uint32(0); i < ins.Arg1-1; i++ {
					ch := ir.InstrID(vm.Prog.Extra[ins.Arg0+i])
					if _, err := vm.Eval(ch, env); err != nil {
						return nil, err
					}
				}
				id = ir.InstrID(vm.Prog.Extra[ins.Arg0+ins.Arg1-1])
				continue tcoLoop
			}
			return vm.evalPrim(ins, env)

		case ir.IRAlloc:
			return vm.evalAlloc(ins, env)

		case ir.IRLoad:
			return vm.evalLoad(ins, env)

		case ir.IRStore:
			return vm.evalStore(ins, env)

		default:
			return nil, fmt.Errorf("unknown IR opcode %d", ins.Kind)
		}
	}
}

func (vm *VM) evalPrim(ins ir.IRInstr, env map[token.StringID]Value) (Value, error) {
	switch ins.SubOp {
	case ir.PrimWhile:
		for {
			cVal, err := vm.Eval(ir.InstrID(ins.Arg0), env)
			if err != nil {
				return nil, err
			}
			bv, ok := Deref(cVal).(BoolVal)
			if !ok || !bv.V {
				break
			}
			if _, err := vm.Eval(ir.InstrID(ins.Arg1), env); err != nil {
				return nil, err
			}
		}
		return Unit, nil

	case ir.PrimForTo:
		rVal, err := vm.Eval(ir.InstrID(ins.Arg0), env)
		if err != nil {
			return nil, err
		}
		varSym := token.StringID(ins.Data)
		elems := listToSlice(Deref(rVal))
		for _, elem := range elems {
			env[varSym] = elem
			if _, err := vm.Eval(ir.InstrID(ins.Arg1), env); err != nil {
				return nil, err
			}
		}
		return Unit, nil

	case ir.PrimForIn:
		cVal, err := vm.Eval(ir.InstrID(ins.Arg0), env)
		if err != nil {
			return nil, err
		}
		varSym := token.StringID(ins.Data)
		elems := listToSlice(Deref(cVal))
		for _, elem := range elems {
			env[varSym] = elem
			if _, err := vm.Eval(ir.InstrID(ins.Arg1), env); err != nil {
				return nil, err
			}
		}
		return Unit, nil

	case ir.PrimTryFinally:
		res, err := vm.Eval(ir.InstrID(ins.Arg0), env)
		cleanupVal, cErr := vm.Eval(ir.InstrID(ins.Arg1), env)
		if ins.Flags&ir.IRFlagDispose != 0 {
			if symVal, ok := env[token.StringID(vm.Prog.Instrs[ins.Arg1].Data)]; ok {
				disposeValue(symVal)
			} else {
				disposeValue(cleanupVal)
			}
		}
		if err != nil {
			return nil, err
		}
		if cErr != nil {
			return nil, cErr
		}
		return res, nil

	case ir.PrimTryWith:
		res, err := vm.Eval(ir.InstrID(ins.Arg0), env)
		if err != nil {
			return vm.Eval(ir.InstrID(ins.Arg1), env)
		}
		return res, nil

	case ir.PrimRaise:
		v, err := vm.Eval(ir.InstrID(ins.Arg0), env)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("exception: %s", Deref(v).String())

	case ir.PrimSeqGen:
		return vm.Eval(ir.InstrID(ins.Arg0), env)

	case ir.PrimNeg:
		v, err := vm.Eval(ir.InstrID(ins.Arg0), env)
		if err != nil {
			return nil, err
		}
		switch nv := Deref(v).(type) {
		case IntVal:
			return IntVal{V: -nv.V}, nil
		case FloatVal:
			return FloatVal{V: -nv.V}, nil
		default:
			return nil, fmt.Errorf("unary negation requires numeric value")
		}

	case ir.PrimNot:
		v, err := vm.Eval(ir.InstrID(ins.Arg0), env)
		if err != nil {
			return nil, err
		}
		bv, ok := Deref(v).(BoolVal)
		if !ok {
			return nil, fmt.Errorf("not requires bool")
		}
		return BoolVal{V: !bv.V}, nil
	}

	lhs, err := vm.Eval(ir.InstrID(ins.Arg0), env)
	if err != nil {
		return nil, err
	}
	rhs, err := vm.Eval(ir.InstrID(ins.Arg1), env)
	if err != nil {
		return nil, err
	}
	return EvalBinaryPrim(ins.SubOp, ins.Flags, lhs, rhs)
}

// EvalBinaryPrim executes a binary IRPrim sub-operation safely without unchecked host panics.
func EvalBinaryPrim(subOp uint16, flags uint8, lhs, rhs Value) (Value, error) {
	lhs = Deref(lhs)
	rhs = Deref(rhs)

	switch subOp {
	case ir.PrimAdd:
		switch l := lhs.(type) {
		case IntVal:
			r, ok := rhs.(IntVal)
			if !ok {
				return nil, fmt.Errorf("type error: '+' expected int on rhs, got %v", rhs)
			}
			return IntVal{V: l.V + r.V}, nil
		case FloatVal:
			r, ok := rhs.(FloatVal)
			if !ok {
				return nil, fmt.Errorf("type error: '+' expected float on rhs, got %v", rhs)
			}
			return FloatVal{V: l.V + r.V}, nil
		case StringVal:
			r, ok := rhs.(StringVal)
			if !ok {
				return nil, fmt.Errorf("type error: '+' expected string on rhs, got %v", rhs)
			}
			return StringVal{V: l.V + r.V}, nil
		default:
			return nil, fmt.Errorf("type error: '+' unsupported on %v", lhs)
		}
	case ir.PrimSub:
		switch l := lhs.(type) {
		case IntVal:
			r, ok := rhs.(IntVal)
			if !ok {
				return nil, fmt.Errorf("type error: '-' expected int on rhs, got %v", rhs)
			}
			return IntVal{V: l.V - r.V}, nil
		case FloatVal:
			r, ok := rhs.(FloatVal)
			if !ok {
				return nil, fmt.Errorf("type error: '-' expected float on rhs, got %v", rhs)
			}
			return FloatVal{V: l.V - r.V}, nil
		default:
			return nil, fmt.Errorf("type error: '-' unsupported on %v", lhs)
		}
	case ir.PrimMul:
		switch l := lhs.(type) {
		case IntVal:
			r, ok := rhs.(IntVal)
			if !ok {
				return nil, fmt.Errorf("type error: '*' expected int on rhs, got %v", rhs)
			}
			return IntVal{V: l.V * r.V}, nil
		case FloatVal:
			r, ok := rhs.(FloatVal)
			if !ok {
				return nil, fmt.Errorf("type error: '*' expected float on rhs, got %v", rhs)
			}
			return FloatVal{V: l.V * r.V}, nil
		default:
			return nil, fmt.Errorf("type error: '*' unsupported on %v", lhs)
		}
	case ir.PrimDiv:
		switch l := lhs.(type) {
		case IntVal:
			r, ok := rhs.(IntVal)
			if !ok {
				return nil, fmt.Errorf("type error: '/' expected int on rhs, got %v", rhs)
			}
			if r.V == 0 {
				return nil, fmt.Errorf("division by zero")
			}
			return IntVal{V: l.V / r.V}, nil
		case FloatVal:
			r, ok := rhs.(FloatVal)
			if !ok {
				return nil, fmt.Errorf("type error: '/' expected float on rhs, got %v", rhs)
			}
			return FloatVal{V: l.V / r.V}, nil
		default:
			return nil, fmt.Errorf("type error: '/' unsupported on %v", lhs)
		}
	case ir.PrimMod:
		l, okL := lhs.(IntVal)
		r, okR := rhs.(IntVal)
		if !okL || !okR {
			return nil, fmt.Errorf("type error: '%%' requires int operands, got %v and %v", lhs, rhs)
		}
		if r.V == 0 {
			return nil, fmt.Errorf("modulo by zero")
		}
		return IntVal{V: l.V % r.V}, nil
	case ir.PrimEq:
		return BoolVal{V: ValuesEqual(lhs, rhs)}, nil
	case ir.PrimNeq:
		return BoolVal{V: !ValuesEqual(lhs, rhs)}, nil
	case ir.PrimLt:
		return BoolVal{V: CompareValues(lhs, rhs) < 0}, nil
	case ir.PrimLe:
		return BoolVal{V: CompareValues(lhs, rhs) <= 0}, nil
	case ir.PrimGt:
		return BoolVal{V: CompareValues(lhs, rhs) > 0}, nil
	case ir.PrimGe:
		return BoolVal{V: CompareValues(lhs, rhs) >= 0}, nil
	case ir.PrimAnd:
		l, okL := lhs.(BoolVal)
		r, okR := rhs.(BoolVal)
		if !okL || !okR {
			return nil, fmt.Errorf("type error: '&&' requires bool operands")
		}
		return BoolVal{V: l.V && r.V}, nil
	case ir.PrimOr:
		l, okL := lhs.(BoolVal)
		r, okR := rhs.(BoolVal)
		if !okL || !okR {
			return nil, fmt.Errorf("type error: '||' requires bool operands")
		}
		return BoolVal{V: l.V || r.V}, nil
	case ir.PrimCons:
		tail, _ := rhs.(*ListVal)
		if tail == nil {
			tail = EmptyList
		}
		return &ListVal{Head: lhs, Tail: tail}, nil
	case ir.PrimAppend:
		s1 := listToSlice(lhs)
		s2 := listToSlice(rhs)
		return SliceToList(append(s1, s2...)), nil
	case ir.PrimRange:
		startVal, okL := lhs.(IntVal)
		endVal, okR := rhs.(IntVal)
		if !okL || !okR {
			return nil, fmt.Errorf("type error: '..' range requires int bounds")
		}
		start := startVal.V
		end := endVal.V
		var items []Value
		if flags != 0 && start >= end {
			for i := start; i >= end; i-- {
				items = append(items, IntVal{V: i})
			}
		} else {
			for i := start; i <= end; i++ {
				items = append(items, IntVal{V: i})
			}
		}
		return SliceToList(items), nil
	}
	return Unit, nil
}

func (vm *VM) evalAlloc(ins ir.IRInstr, env map[token.StringID]Value) (Value, error) {
	switch ins.SubOp {
	case ir.AllocTuple, ir.AllocArray:
		elems := make([]Value, ins.Arg1)
		for i := uint32(0); i < ins.Arg1; i++ {
			ch := ir.InstrID(vm.Prog.Extra[ins.Arg0+i])
			v, err := vm.Eval(ch, env)
			if err != nil {
				return nil, err
			}
			elems[i] = Deref(v)
		}
		if ins.SubOp == ir.AllocArray {
			return &ArrayVal{Elems: elems}, nil
		}
		return TupleVal{Elems: elems}, nil

	case ir.AllocList:
		elems := make([]Value, ins.Arg1)
		for i := uint32(0); i < ins.Arg1; i++ {
			ch := ir.InstrID(vm.Prog.Extra[ins.Arg0+i])
			v, err := vm.Eval(ch, env)
			if err != nil {
				return nil, err
			}
			elems[i] = Deref(v)
		}
		return SliceToList(elems), nil

	case ir.AllocRecord:
		fields := make(map[token.StringID]Value, ins.Arg1/2+4)
		byName := make(map[string]Value, ins.Arg1/2+4)
		if ins.Flags != 0 {
			baseVal, err := vm.Eval(ir.InstrID(ins.Aux), env)
			if err != nil {
				return nil, err
			}
			if baseRec, ok := Deref(baseVal).(*RecordVal); ok {
				for k, v := range baseRec.Fields {
					fields[k] = v
				}
				for k, v := range baseRec.ByName {
					byName[k] = v
				}
			}
		}
		for i := uint32(0); i < ins.Arg1; i += 2 {
			fID := token.StringID(vm.Prog.Extra[ins.Arg0+i])
			valInstr := ir.InstrID(vm.Prog.Extra[ins.Arg0+i+1])
			v, err := vm.Eval(valInstr, env)
			if err != nil {
				return nil, err
			}
			dv := Deref(v)
			fields[fID] = dv
			byName[vm.Prog.Strings.String(fID)] = dv
		}
		return &RecordVal{Fields: fields, ByName: byName}, nil

	case ir.AllocADT:
		tag := token.StringID(ins.Data)
		var payload Value
		if ir.InstrID(ins.Arg0) != ir.NoInstr {
			v, err := vm.Eval(ir.InstrID(ins.Arg0), env)
			if err != nil {
				return nil, err
			}
			payload = Deref(v)
		}
		return ADTVal{
			Tag:     tag,
			TagName: vm.Prog.Strings.String(tag),
			Payload: payload,
		}, nil

	default:
		return Unit, nil
	}
}

func (vm *VM) evalLoad(ins ir.IRInstr, env map[token.StringID]Value) (Value, error) {
	// Check if loading a qualified module symbol `Module.member`
	if ins.SubOp == ir.LoadRecordField && ins.Aux != 0 {
		qualSym := token.StringID(ins.Aux)
		if v, ok := env[qualSym]; ok {
			return Deref(v), nil
		}
		if v, ok := vm.Globals[qualSym]; ok {
			return Deref(v), nil
		}
	}

	target, err := vm.Eval(ir.InstrID(ins.Arg0), env)
	if err != nil {
		return nil, err
	}
	target = Deref(target)

	switch ins.SubOp {
	case ir.LoadTupleElem:
		tup, ok := target.(TupleVal)
		if !ok || int(ins.Arg1) >= len(tup.Elems) {
			return nil, fmt.Errorf("tuple element index %d out of bounds", ins.Arg1)
		}
		return tup.Elems[ins.Arg1], nil

	case ir.LoadRecordField:
		rec, ok := target.(*RecordVal)
		if !ok {
			return nil, fmt.Errorf("field access on non-record value %v", target)
		}
		fID := token.StringID(ins.Data)
		if v, exists := rec.Fields[fID]; exists {
			return v, nil
		}
		if v, exists := rec.ByName[vm.Prog.Strings.String(fID)]; exists {
			return v, nil
		}
		return nil, fmt.Errorf("missing record field %q", vm.Prog.Strings.String(fID))

	case ir.LoadADTTag:
		if adt, ok := target.(ADTVal); ok {
			return IntVal{V: int64(adt.Tag)}, nil
		}
		return IntVal{V: 0}, nil

	case ir.LoadADTPayload:
		if adt, ok := target.(ADTVal); ok && adt.Payload != nil {
			return adt.Payload, nil
		}
		return Unit, nil

	case ir.LoadListIsEmpty:
		if lv, ok := target.(*ListVal); ok && (lv == nil || lv.Empty) {
			return IntVal{V: 1}, nil
		}
		return IntVal{V: 0}, nil

	case ir.LoadListHead:
		if lv, ok := target.(*ListVal); ok && lv != nil && !lv.Empty {
			return lv.Head, nil
		}
		return nil, fmt.Errorf("head of empty list")

	case ir.LoadListTail:
		if lv, ok := target.(*ListVal); ok && lv != nil && !lv.Empty {
			if lv.Tail == nil {
				return EmptyList, nil
			}
			return lv.Tail, nil
		}
		return EmptyList, nil

	default:
		return target, nil
	}
}

func (vm *VM) evalStore(ins ir.IRInstr, env map[token.StringID]Value) (Value, error) {
	val, err := vm.Eval(ir.InstrID(ins.Arg1), env)
	if err != nil {
		return nil, err
	}
	val = Deref(val)
	if ins.SubOp == ir.StoreArrayElem {
		arrVal, err := vm.Eval(ir.InstrID(ins.Arg0), env)
		if err != nil {
			return nil, err
		}
		idxVal, err := vm.Eval(ir.InstrID(ins.Aux), env)
		if err != nil {
			return nil, err
		}
		arr, okA := Deref(arrVal).(*ArrayVal)
		iv, okI := Deref(idxVal).(IntVal)
		if !okA || !okI {
			return nil, fmt.Errorf("array store requires array and int index")
		}
		if iv.V < 0 || int(iv.V) >= len(arr.Elems) {
			return nil, fmt.Errorf("array index %d out of bounds (len %d)", iv.V, len(arr.Elems))
		}
		arr.Elems[iv.V] = val
		return Unit, nil
	}
	sym := token.StringID(ins.Data)
	if existing, ok := env[sym]; ok {
		if cell, isCell := existing.(*RefCell); isCell {
			cell.Val = val
			return Unit, nil
		}
	}
	if existing, ok := vm.Globals[sym]; ok {
		if cell, isCell := existing.(*RefCell); isCell {
			cell.Val = val
			return Unit, nil
		}
	}
	env[sym] = &RefCell{Val: val}
	return Unit, nil
}

// ExtractSwitchTag extracts the 32-bit switch dispatch tag from a runtime Value.
func ExtractSwitchTag(v Value, strings *token.StringTable) uint32 {
	return extractSwitchTag(v, strings)
}

func extractSwitchTag(v Value, strings *token.StringTable) uint32 {
	switch tv := v.(type) {
	case BoolVal:
		if tv.V {
			return 1
		}
		return 0
	case IntVal:
		return uint32(tv.V)
	case StringVal:
		return uint32(strings.InternString(tv.V))
	case ADTVal:
		return uint32(tv.Tag)
	default:
		return 0
	}
}

// DisposeValue deterministically disposes a DisposableVal (or boxed RefCell).
func DisposeValue(v Value) {
	disposeValue(v)
}

func disposeValue(v Value) {
	if dv, ok := v.(*DisposableVal); ok {
		dv.Dispose()
		return
	}
	if cell, ok := v.(*RefCell); ok {
		disposeValue(cell.Val)
	}
}

// SliceToList builds an immutable singly-linked ListVal from a slice of Values.
func SliceToList(items []Value) *ListVal {
	res := EmptyList
	for i := len(items) - 1; i >= 0; i-- {
		res = &ListVal{
			Head: items[i],
			Tail: res,
		}
	}
	return res
}

// ListToSlice materializes a ListVal or ArrayVal into a contiguous Value slice.
func ListToSlice(v Value) []Value {
	return listToSlice(v)
}

func listToSlice(v Value) []Value {
	switch c := v.(type) {
	case *ListVal:
		var out []Value
		curr := c
		for curr != nil && !curr.Empty {
			out = append(out, curr.Head)
			curr = curr.Tail
		}
		return out
	case *ArrayVal:
		return c.Elems
	default:
		return nil
	}
}
