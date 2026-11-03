package stdlib

import (
	"fmt"

	"fml/internal/runtime"
)

// RegisterCompAndQuotes registers the built-in `option` and `result` computation
// expression builders (`Bind`, `Return`, `ReturnFrom`, `Zero`) as well as the
// `Quote` introspection module (`Quote.toString`, `Quote.eval`) for first-class
// AST quotations `<@ ... @>`.
//
// Jaime says: Computation expressions shouldn't require compiler magic per monad.
// A builder is just a 4-field record of functions `{ Bind; Return; ReturnFrom; Zero }`.
// Quotations are just an ordinary algebraic data type (`Expr`) that user code
// can pattern-match or fold like any other tree.
func RegisterCompAndQuotes(vm *runtime.VM) {
	someID := vm.Prog.Strings.InternString("Some")
	noneID := vm.Prog.Strings.InternString("None")
	okID := vm.Prog.Strings.InternString("Ok")
	errID := vm.Prog.Strings.InternString("Error")

	// 1. `option { ... }` computation expression builder
	optBind := &runtime.BuiltinVal{
		Name:  "option.Bind",
		Arity: 2,
		Fn: func(vm *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			m, ok := runtime.Deref(args[0]).(runtime.ADTVal)
			if !ok {
				return nil, fmt.Errorf("option.Bind: expected Option ADT, got %v", args[0])
			}
			if m.Tag == noneID || m.TagName == "None" {
				return runtime.ADTVal{Tag: noneID, TagName: "None"}, nil
			}
			return vm.Apply(runtime.Deref(args[1]), m.Payload)
		},
	}
	optReturn := &runtime.BuiltinVal{
		Name:  "option.Return",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			return runtime.ADTVal{Tag: someID, TagName: "Some", Payload: runtime.Deref(args[0])}, nil
		},
	}
	optReturnFrom := &runtime.BuiltinVal{
		Name:  "option.ReturnFrom",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			return runtime.Deref(args[0]), nil
		},
	}
	optZero := &runtime.BuiltinVal{
		Name:  "option.Zero",
		Arity: 1,
		Fn: func(_ *runtime.VM, _ []runtime.Value) (runtime.Value, error) {
			return runtime.ADTVal{Tag: noneID, TagName: "None"}, nil
		},
	}
	defineModuleRecord(vm, "option", map[string]runtime.Value{
		"Bind":       optBind,
		"Return":     optReturn,
		"ReturnFrom": optReturnFrom,
		"Zero":       optZero,
	})

	// 2. `result { ... }` computation expression builder
	resBind := &runtime.BuiltinVal{
		Name:  "result.Bind",
		Arity: 2,
		Fn: func(vm *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			m, ok := runtime.Deref(args[0]).(runtime.ADTVal)
			if !ok {
				return nil, fmt.Errorf("result.Bind: expected Result ADT, got %v", args[0])
			}
			if m.Tag == errID || m.TagName == "Error" {
				return m, nil
			}
			return vm.Apply(runtime.Deref(args[1]), m.Payload)
		},
	}
	resReturn := &runtime.BuiltinVal{
		Name:  "result.Return",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			return runtime.ADTVal{Tag: okID, TagName: "Ok", Payload: runtime.Deref(args[0])}, nil
		},
	}
	resReturnFrom := &runtime.BuiltinVal{
		Name:  "result.ReturnFrom",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			return runtime.Deref(args[0]), nil
		},
	}
	resZero := &runtime.BuiltinVal{
		Name:  "result.Zero",
		Arity: 1,
		Fn: func(_ *runtime.VM, _ []runtime.Value) (runtime.Value, error) {
			return runtime.ADTVal{Tag: okID, TagName: "Ok", Payload: runtime.Unit}, nil
		},
	}
	defineModuleRecord(vm, "result", map[string]runtime.Value{
		"Bind":       resBind,
		"Return":     resReturn,
		"ReturnFrom": resReturnFrom,
		"Zero":       resZero,
	})

	// 3. `Quote` module for first-class AST quotations <@ ... @>
	quoteToString := &runtime.BuiltinVal{
		Name:  "Quote.toString",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			return runtime.StringVal{V: formatQuotedExpr(runtime.Deref(args[0]))}, nil
		},
	}
	quoteEval := &runtime.BuiltinVal{
		Name:  "Quote.eval",
		Arity: 1,
		Fn: func(vm *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			return evalQuotedExpr(vm, runtime.Deref(args[0]), map[string]runtime.Value{})
		},
	}
	defineModuleRecord(vm, "Quote", map[string]runtime.Value{
		"toString": quoteToString,
		"eval":     quoteEval,
	})
}

func formatQuotedExpr(v runtime.Value) string {
	adt, ok := runtime.Deref(v).(runtime.ADTVal)
	if !ok {
		return v.String()
	}
	switch adt.TagName {
	case "QUnit":
		return "()"
	case "QInt", "QFloat", "QBool", "QVar":
		return runtime.Deref(adt.Payload).String()
	case "QString":
		return fmt.Sprintf("%q", runtime.Deref(adt.Payload).String())
	case "QInfix":
		tup, ok := runtime.Deref(adt.Payload).(runtime.TupleVal)
		if !ok || len(tup.Elems) < 3 {
			return adt.String()
		}
		op := runtime.Deref(tup.Elems[0]).String()
		l := formatQuotedExpr(tup.Elems[1])
		r := formatQuotedExpr(tup.Elems[2])
		return fmt.Sprintf("(%s %s %s)", l, op, r)
	case "QApp":
		tup, ok := runtime.Deref(adt.Payload).(runtime.TupleVal)
		if !ok || len(tup.Elems) < 2 {
			return adt.String()
		}
		fn := formatQuotedExpr(tup.Elems[0])
		arg := formatQuotedExpr(tup.Elems[1])
		return fmt.Sprintf("(%s %s)", fn, arg)
	case "QLambda":
		tup, ok := runtime.Deref(adt.Payload).(runtime.TupleVal)
		if !ok || len(tup.Elems) < 2 {
			return adt.String()
		}
		param := runtime.Deref(tup.Elems[0]).String()
		body := formatQuotedExpr(tup.Elems[1])
		return fmt.Sprintf("(fun %s -> %s)", param, body)
	case "QLet":
		tup, ok := runtime.Deref(adt.Payload).(runtime.TupleVal)
		if !ok || len(tup.Elems) < 3 {
			return adt.String()
		}
		name := runtime.Deref(tup.Elems[0]).String()
		rhs := formatQuotedExpr(tup.Elems[1])
		body := formatQuotedExpr(tup.Elems[2])
		return fmt.Sprintf("(let %s = %s in %s)", name, rhs, body)
	default:
		return adt.String()
	}
}

func evalQuotedExpr(vm *runtime.VM, v runtime.Value, env map[string]runtime.Value) (runtime.Value, error) {
	adt, ok := runtime.Deref(v).(runtime.ADTVal)
	if !ok {
		return v, nil
	}
	switch adt.TagName {
	case "QUnit":
		return runtime.Unit, nil
	case "QInt", "QFloat", "QBool", "QString":
		return runtime.Deref(adt.Payload), nil
	case "QVar":
		name := runtime.Deref(adt.Payload).String()
		if val, ok := env[name]; ok {
			return val, nil
		}
		id := vm.Prog.Strings.InternString(name)
		if val, found := vm.Globals[id]; found {
			return runtime.Deref(val), nil
		}
		return nil, fmt.Errorf("Quote.eval: unbound variable %q", name)
	case "QInfix":
		tup, ok := runtime.Deref(adt.Payload).(runtime.TupleVal)
		if !ok || len(tup.Elems) < 3 {
			return nil, fmt.Errorf("Quote.eval: malformed QInfix payload")
		}
		op := runtime.Deref(tup.Elems[0]).String()
		lv, err := evalQuotedExpr(vm, tup.Elems[1], env)
		if err != nil {
			return nil, err
		}
		rv, err := evalQuotedExpr(vm, tup.Elems[2], env)
		if err != nil {
			return nil, err
		}
		return evalQuotedInfix(op, lv, rv)
	case "QLet":
		tup, ok := runtime.Deref(adt.Payload).(runtime.TupleVal)
		if !ok || len(tup.Elems) < 3 {
			return nil, fmt.Errorf("Quote.eval: malformed QLet payload")
		}
		name := runtime.Deref(tup.Elems[0]).String()
		rhsVal, err := evalQuotedExpr(vm, tup.Elems[1], env)
		if err != nil {
			return nil, err
		}
		childEnv := make(map[string]runtime.Value, len(env)+1)
		for k, val := range env {
			childEnv[k] = val
		}
		childEnv[name] = rhsVal
		return evalQuotedExpr(vm, tup.Elems[2], childEnv)
	case "QLambda":
		tup, ok := runtime.Deref(adt.Payload).(runtime.TupleVal)
		if !ok || len(tup.Elems) < 2 {
			return nil, fmt.Errorf("Quote.eval: malformed QLambda payload")
		}
		param := runtime.Deref(tup.Elems[0]).String()
		body := tup.Elems[1]
		captured := make(map[string]runtime.Value, len(env))
		for k, val := range env {
			captured[k] = val
		}
		return &runtime.BuiltinVal{
			Name:  "quoted_lambda",
			Arity: 1,
			Fn: func(vm *runtime.VM, args []runtime.Value) (runtime.Value, error) {
				callEnv := make(map[string]runtime.Value, len(captured)+1)
				for k, val := range captured {
					callEnv[k] = val
				}
				callEnv[param] = runtime.Deref(args[0])
				return evalQuotedExpr(vm, body, callEnv)
			},
		}, nil
	case "QApp":
		tup, ok := runtime.Deref(adt.Payload).(runtime.TupleVal)
		if !ok || len(tup.Elems) < 2 {
			return nil, fmt.Errorf("Quote.eval: malformed QApp payload")
		}
		fnVal, err := evalQuotedExpr(vm, tup.Elems[0], env)
		if err != nil {
			return nil, err
		}
		argVal, err := evalQuotedExpr(vm, tup.Elems[1], env)
		if err != nil {
			return nil, err
		}
		return vm.Apply(fnVal, argVal)
	default:
		return adt, nil
	}
}

func evalQuotedInfix(op string, l, r runtime.Value) (runtime.Value, error) {
	l = runtime.Deref(l)
	r = runtime.Deref(r)
	switch op {
	case "+":
		if li, ok := l.(runtime.IntVal); ok {
			if ri, okR := r.(runtime.IntVal); okR {
				return runtime.IntVal{V: li.V + ri.V}, nil
			}
		}
		if lf, ok := l.(runtime.FloatVal); ok {
			if rf, okR := r.(runtime.FloatVal); okR {
				return runtime.FloatVal{V: lf.V + rf.V}, nil
			}
		}
		if ls, ok := l.(runtime.StringVal); ok {
			if rs, okR := r.(runtime.StringVal); okR {
				return runtime.StringVal{V: ls.V + rs.V}, nil
			}
		}
	case "-":
		if li, ok := l.(runtime.IntVal); ok {
			if ri, okR := r.(runtime.IntVal); okR {
				return runtime.IntVal{V: li.V - ri.V}, nil
			}
		}
		if lf, ok := l.(runtime.FloatVal); ok {
			if rf, okR := r.(runtime.FloatVal); okR {
				return runtime.FloatVal{V: lf.V - rf.V}, nil
			}
		}
	case "*":
		if li, ok := l.(runtime.IntVal); ok {
			if ri, okR := r.(runtime.IntVal); okR {
				return runtime.IntVal{V: li.V * ri.V}, nil
			}
		}
		if lf, ok := l.(runtime.FloatVal); ok {
			if rf, okR := r.(runtime.FloatVal); okR {
				return runtime.FloatVal{V: lf.V * rf.V}, nil
			}
		}
	case "/":
		if li, ok := l.(runtime.IntVal); ok {
			if ri, okR := r.(runtime.IntVal); okR && ri.V != 0 {
				return runtime.IntVal{V: li.V / ri.V}, nil
			}
		}
		if lf, ok := l.(runtime.FloatVal); ok {
			if rf, okR := r.(runtime.FloatVal); okR {
				return runtime.FloatVal{V: lf.V / rf.V}, nil
			}
		}
	case "%":
		if li, ok := l.(runtime.IntVal); ok {
			if ri, okR := r.(runtime.IntVal); okR && ri.V != 0 {
				return runtime.IntVal{V: li.V % ri.V}, nil
			}
		}
	case "=", "==":
		return runtime.BoolVal{V: runtime.CompareValues(l, r) == 0}, nil
	case "<>":
		return runtime.BoolVal{V: runtime.CompareValues(l, r) != 0}, nil
	case "<":
		return runtime.BoolVal{V: runtime.CompareValues(l, r) < 0}, nil
	case "<=":
		return runtime.BoolVal{V: runtime.CompareValues(l, r) <= 0}, nil
	case ">":
		return runtime.BoolVal{V: runtime.CompareValues(l, r) > 0}, nil
	case ">=":
		return runtime.BoolVal{V: runtime.CompareValues(l, r) >= 0}, nil
	}
	return nil, fmt.Errorf("Quote.eval: unsupported operator %q", op)
}
