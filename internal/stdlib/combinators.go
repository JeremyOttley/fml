package stdlib

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"fml/internal/runtime"
)

// RegisterCombinators binds the higher-order `List`, `Array`, `String`,
// `Option`, `Result`, `Regex`, `Math`, `Time`, and `Test` combinator modules into vm.
//
// Jaime says: Every functional language lives or dies by its collection and
// option/result combinators. Convert singly-linked lists to flat Go slices on
// the way in, run the loop over contiguous memory, and rebuild the list on the
// way out.
func RegisterCombinators(vm *runtime.VM) {
	registerListModule(vm)
	registerArrayModule(vm)
	registerStringModule(vm)
	registerOptionModule(vm)
	registerResultModule(vm)
	registerRegexModule(vm)
	registerMathModule(vm)
	registerTimeModule(vm)
	registerTestModule(vm)
}

func listToSlice(v runtime.Value) ([]runtime.Value, error) {
	lv, ok := runtime.Deref(v).(*runtime.ListVal)
	if !ok {
		return nil, fmt.Errorf("expected List, got %v", v)
	}
	var out []runtime.Value
	for curr := lv; curr != nil && !curr.Empty; curr = curr.Tail {
		out = append(out, runtime.Deref(curr.Head))
	}
	return out, nil
}

func registerListModule(vm *runtime.VM) {
	listIsEmpty := &runtime.BuiltinVal{
		Name:  "List.isEmpty",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			lv, ok := runtime.Deref(args[0]).(*runtime.ListVal)
			return runtime.BoolVal{V: !ok || lv == nil || lv.Empty}, nil
		},
	}
	listLength := &runtime.BuiltinVal{
		Name:  "List.length",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			items, err := listToSlice(args[0])
			if err != nil {
				return nil, err
			}
			return runtime.IntVal{V: int64(len(items))}, nil
		},
	}
	listHead := &runtime.BuiltinVal{
		Name:  "List.head",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			lv, ok := runtime.Deref(args[0]).(*runtime.ListVal)
			if !ok || lv == nil || lv.Empty {
				return nil, fmt.Errorf("List.head: empty list")
			}
			return runtime.Deref(lv.Head), nil
		},
	}
	listTail := &runtime.BuiltinVal{
		Name:  "List.tail",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			lv, ok := runtime.Deref(args[0]).(*runtime.ListVal)
			if !ok || lv == nil || lv.Empty {
				return nil, fmt.Errorf("List.tail: empty list")
			}
			if lv.Tail == nil {
				return runtime.EmptyList, nil
			}
			return lv.Tail, nil
		},
	}
	listRev := &runtime.BuiltinVal{
		Name:  "List.rev",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			items, err := listToSlice(args[0])
			if err != nil {
				return nil, err
			}
			for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
				items[i], items[j] = items[j], items[i]
			}
			return runtime.SliceToList(items), nil
		},
	}
	listMap := &runtime.BuiltinVal{
		Name:  "List.map",
		Arity: 2,
		Fn: func(vm *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			fn := runtime.Deref(args[0])
			items, err := listToSlice(args[1])
			if err != nil {
				return nil, err
			}
			mapped := make([]runtime.Value, len(items))
			for i, elem := range items {
				v, err := vm.Apply(fn, elem)
				if err != nil {
					return nil, err
				}
				mapped[i] = v
			}
			return runtime.SliceToList(mapped), nil
		},
	}
	listFilter := &runtime.BuiltinVal{
		Name:  "List.filter",
		Arity: 2,
		Fn: func(vm *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			pred := runtime.Deref(args[0])
			items, err := listToSlice(args[1])
			if err != nil {
				return nil, err
			}
			var kept []runtime.Value
			for _, elem := range items {
				res, err := vm.Apply(pred, elem)
				if err != nil {
					return nil, err
				}
				if bv, ok := runtime.Deref(res).(runtime.BoolVal); ok && bv.V {
					kept = append(kept, elem)
				}
			}
			return runtime.SliceToList(kept), nil
		},
	}
	listFold := &runtime.BuiltinVal{
		Name:  "List.fold",
		Arity: 3,
		Fn: func(vm *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			folder := runtime.Deref(args[0])
			state := runtime.Deref(args[1])
			items, err := listToSlice(args[2])
			if err != nil {
				return nil, err
			}
			for _, elem := range items {
				partial, err := vm.Apply(folder, state)
				if err != nil {
					return nil, err
				}
				state, err = vm.Apply(partial, elem)
				if err != nil {
					return nil, err
				}
			}
			return state, nil
		},
	}
	listSortBy := &runtime.BuiltinVal{
		Name:  "List.sortBy",
		Arity: 2,
		Fn: func(vm *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			proj := runtime.Deref(args[0])
			items, err := listToSlice(args[1])
			if err != nil {
				return nil, err
			}
			keys := make([]runtime.Value, len(items))
			for i, elem := range items {
				k, err := vm.Apply(proj, elem)
				if err != nil {
					return nil, err
				}
				keys[i] = runtime.Deref(k)
			}
			indices := make([]int, len(items))
			for i := range indices {
				indices[i] = i
			}
			sort.SliceStable(indices, func(i, j int) bool {
				return runtime.CompareValues(keys[indices[i]], keys[indices[j]]) < 0
			})
			sorted := make([]runtime.Value, len(items))
			for i, idx := range indices {
				sorted[i] = items[idx]
			}
			return runtime.SliceToList(sorted), nil
		},
	}
	listSum := &runtime.BuiltinVal{
		Name:  "List.sum",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			items, err := listToSlice(args[0])
			if err != nil {
				return nil, err
			}
			if len(items) > 0 {
				if _, isFloat := items[0].(runtime.FloatVal); isFloat {
					var sum float64
					for _, it := range items {
						sum += it.(runtime.FloatVal).V
					}
					return runtime.FloatVal{V: sum}, nil
				}
			}
			var sum int64
			for _, it := range items {
				sum += it.(runtime.IntVal).V
			}
			return runtime.IntVal{V: sum}, nil
		},
	}

	defineModuleRecord(vm, "List", map[string]runtime.Value{
		"empty":   runtime.EmptyList,
		"isEmpty": listIsEmpty,
		"length":  listLength,
		"head":    listHead,
		"tail":    listTail,
		"rev":     listRev,
		"map":     listMap,
		"filter":  listFilter,
		"fold":    listFold,
		"sortBy":  listSortBy,
		"sum":     listSum,
	})
}

func registerArrayModule(vm *runtime.VM) {
	arrLength := &runtime.BuiltinVal{
		Name:  "Array.length",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			av, ok := runtime.Deref(args[0]).(*runtime.ArrayVal)
			if !ok {
				return nil, fmt.Errorf("Array.length: expected Array, got %v", args[0])
			}
			return runtime.IntVal{V: int64(len(av.Elems))}, nil
		},
	}
	arrGet := &runtime.BuiltinVal{
		Name:  "Array.get",
		Arity: 2,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			av, ok := runtime.Deref(args[0]).(*runtime.ArrayVal)
			if !ok {
				return nil, fmt.Errorf("Array.get: expected Array, got %v", args[0])
			}
			idx := int(runtime.Deref(args[1]).(runtime.IntVal).V)
			if idx < 0 || idx >= len(av.Elems) {
				return nil, fmt.Errorf("Array.get: index %d out of bounds [0, %d)", idx, len(av.Elems))
			}
			return runtime.Deref(av.Elems[idx]), nil
		},
	}
	arrSet := &runtime.BuiltinVal{
		Name:  "Array.set",
		Arity: 3,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			av, ok := runtime.Deref(args[0]).(*runtime.ArrayVal)
			if !ok {
				return nil, fmt.Errorf("Array.set: expected Array, got %v", args[0])
			}
			idx := int(runtime.Deref(args[1]).(runtime.IntVal).V)
			if idx < 0 || idx >= len(av.Elems) {
				return nil, fmt.Errorf("Array.set: index %d out of bounds [0, %d)", idx, len(av.Elems))
			}
			av.Elems[idx] = runtime.Deref(args[2])
			return runtime.Unit, nil
		},
	}
	arrMap := &runtime.BuiltinVal{
		Name:  "Array.map",
		Arity: 2,
		Fn: func(vm *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			fn := runtime.Deref(args[0])
			av, ok := runtime.Deref(args[1]).(*runtime.ArrayVal)
			if !ok {
				return nil, fmt.Errorf("Array.map: expected Array, got %v", args[1])
			}
			out := make([]runtime.Value, len(av.Elems))
			for i, elem := range av.Elems {
				v, err := vm.Apply(fn, runtime.Deref(elem))
				if err != nil {
					return nil, err
				}
				out[i] = v
			}
			return &runtime.ArrayVal{Elems: out}, nil
		},
	}
	arrOfList := &runtime.BuiltinVal{
		Name:  "Array.ofList",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			items, err := listToSlice(args[0])
			if err != nil {
				return nil, err
			}
			return &runtime.ArrayVal{Elems: items}, nil
		},
	}
	arrToList := &runtime.BuiltinVal{
		Name:  "Array.toList",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			av, ok := runtime.Deref(args[0]).(*runtime.ArrayVal)
			if !ok {
				return nil, fmt.Errorf("Array.toList: expected Array, got %v", args[0])
			}
			return runtime.SliceToList(av.Elems), nil
		},
	}

	defineModuleRecord(vm, "Array", map[string]runtime.Value{
		"length": arrLength,
		"get":    arrGet,
		"set":    arrSet,
		"map":    arrMap,
		"ofList": arrOfList,
		"toList": arrToList,
	})
}

func registerStringModule(vm *runtime.VM) {
	strLength := &runtime.BuiltinVal{
		Name:  "String.length",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			s := runtime.Deref(args[0]).(runtime.StringVal).V
			return runtime.IntVal{V: int64(len(s))}, nil
		},
	}
	strTrim := &runtime.BuiltinVal{
		Name:  "String.trim",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			s := runtime.Deref(args[0]).(runtime.StringVal).V
			return runtime.StringVal{V: strings.TrimSpace(s)}, nil
		},
	}
	strSplit := &runtime.BuiltinVal{
		Name:  "String.split",
		Arity: 2,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			sep := runtime.Deref(args[0]).(runtime.StringVal).V
			s := runtime.Deref(args[1]).(runtime.StringVal).V
			parts := strings.Split(s, sep)
			vals := make([]runtime.Value, len(parts))
			for i, p := range parts {
				vals[i] = runtime.StringVal{V: p}
			}
			return runtime.SliceToList(vals), nil
		},
	}
	strJoin := &runtime.BuiltinVal{
		Name:  "String.join",
		Arity: 2,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			sep := runtime.Deref(args[0]).(runtime.StringVal).V
			items, err := listToSlice(args[1])
			if err != nil {
				return nil, err
			}
			parts := make([]string, len(items))
			for i, it := range items {
				parts[i] = it.String()
			}
			return runtime.StringVal{V: strings.Join(parts, sep)}, nil
		},
	}
	strContains := &runtime.BuiltinVal{
		Name:  "String.contains",
		Arity: 2,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			sub := runtime.Deref(args[0]).(runtime.StringVal).V
			s := runtime.Deref(args[1]).(runtime.StringVal).V
			return runtime.BoolVal{V: strings.Contains(s, sub)}, nil
		},
	}
	strToUpper := &runtime.BuiltinVal{
		Name:  "String.toUpper",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			s := runtime.Deref(args[0]).(runtime.StringVal).V
			return runtime.StringVal{V: strings.ToUpper(s)}, nil
		},
	}
	strToLower := &runtime.BuiltinVal{
		Name:  "String.toLower",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			s := runtime.Deref(args[0]).(runtime.StringVal).V
			return runtime.StringVal{V: strings.ToLower(s)}, nil
		},
	}

	defineModuleRecord(vm, "String", map[string]runtime.Value{
		"length":   strLength,
		"trim":     strTrim,
		"split":    strSplit,
		"join":     strJoin,
		"contains": strContains,
		"toUpper":  strToUpper,
		"toLower":  strToLower,
	})
}

func registerOptionModule(vm *runtime.VM) {
	someID := vm.Prog.Strings.InternString("Some")
	noneID := vm.Prog.Strings.InternString("None")

	optIsSome := &runtime.BuiltinVal{
		Name:  "Option.isSome",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			adt, ok := runtime.Deref(args[0]).(runtime.ADTVal)
			return runtime.BoolVal{V: ok && (adt.Tag == someID || adt.TagName == "Some")}, nil
		},
	}
	optIsNone := &runtime.BuiltinVal{
		Name:  "Option.isNone",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			adt, ok := runtime.Deref(args[0]).(runtime.ADTVal)
			return runtime.BoolVal{V: !ok || adt.Tag == noneID || adt.TagName == "None"}, nil
		},
	}
	optDefaultValue := &runtime.BuiltinVal{
		Name:  "Option.defaultValue",
		Arity: 2,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			def := runtime.Deref(args[0])
			adt, ok := runtime.Deref(args[1]).(runtime.ADTVal)
			if !ok || adt.Tag == noneID || adt.TagName == "None" {
				return def, nil
			}
			return runtime.Deref(adt.Payload), nil
		},
	}
	optMap := &runtime.BuiltinVal{
		Name:  "Option.map",
		Arity: 2,
		Fn: func(vm *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			fn := runtime.Deref(args[0])
			adt, ok := runtime.Deref(args[1]).(runtime.ADTVal)
			if !ok || adt.Tag == noneID || adt.TagName == "None" {
				return runtime.ADTVal{Tag: noneID, TagName: "None"}, nil
			}
			mapped, err := vm.Apply(fn, runtime.Deref(adt.Payload))
			if err != nil {
				return nil, err
			}
			return runtime.ADTVal{Tag: someID, TagName: "Some", Payload: mapped}, nil
		},
	}
	optBind := &runtime.BuiltinVal{
		Name:  "Option.bind",
		Arity: 2,
		Fn: func(vm *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			fn := runtime.Deref(args[0])
			adt, ok := runtime.Deref(args[1]).(runtime.ADTVal)
			if !ok || adt.Tag == noneID || adt.TagName == "None" {
				return runtime.ADTVal{Tag: noneID, TagName: "None"}, nil
			}
			return vm.Apply(fn, runtime.Deref(adt.Payload))
		},
	}

	defineModuleRecord(vm, "Option", map[string]runtime.Value{
		"isSome":       optIsSome,
		"isNone":       optIsNone,
		"defaultValue": optDefaultValue,
		"map":          optMap,
		"bind":         optBind,
	})
}

func registerResultModule(vm *runtime.VM) {
	okID := vm.Prog.Strings.InternString("Ok")
	errID := vm.Prog.Strings.InternString("Error")

	resIsOk := &runtime.BuiltinVal{
		Name:  "Result.isOk",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			adt, ok := runtime.Deref(args[0]).(runtime.ADTVal)
			return runtime.BoolVal{V: ok && (adt.Tag == okID || adt.TagName == "Ok")}, nil
		},
	}
	resIsError := &runtime.BuiltinVal{
		Name:  "Result.isError",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			adt, ok := runtime.Deref(args[0]).(runtime.ADTVal)
			return runtime.BoolVal{V: ok && (adt.Tag == errID || adt.TagName == "Error")}, nil
		},
	}
	resDefaultValue := &runtime.BuiltinVal{
		Name:  "Result.defaultValue",
		Arity: 2,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			def := runtime.Deref(args[0])
			adt, ok := runtime.Deref(args[1]).(runtime.ADTVal)
			if !ok || adt.Tag == errID || adt.TagName == "Error" {
				return def, nil
			}
			return runtime.Deref(adt.Payload), nil
		},
	}
	resMap := &runtime.BuiltinVal{
		Name:  "Result.map",
		Arity: 2,
		Fn: func(vm *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			fn := runtime.Deref(args[0])
			adt, ok := runtime.Deref(args[1]).(runtime.ADTVal)
			if !ok || adt.Tag == errID || adt.TagName == "Error" {
				return adt, nil
			}
			mapped, err := vm.Apply(fn, runtime.Deref(adt.Payload))
			if err != nil {
				return nil, err
			}
			return runtime.ADTVal{Tag: okID, TagName: "Ok", Payload: mapped}, nil
		},
	}
	resMapError := &runtime.BuiltinVal{
		Name:  "Result.mapError",
		Arity: 2,
		Fn: func(vm *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			fn := runtime.Deref(args[0])
			adt, ok := runtime.Deref(args[1]).(runtime.ADTVal)
			if !ok || adt.Tag == okID || adt.TagName == "Ok" {
				return adt, nil
			}
			mappedErr, err := vm.Apply(fn, runtime.Deref(adt.Payload))
			if err != nil {
				return nil, err
			}
			return runtime.ADTVal{Tag: errID, TagName: "Error", Payload: mappedErr}, nil
		},
	}
	resBind := &runtime.BuiltinVal{
		Name:  "Result.bind",
		Arity: 2,
		Fn: func(vm *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			fn := runtime.Deref(args[0])
			adt, ok := runtime.Deref(args[1]).(runtime.ADTVal)
			if !ok || adt.Tag == errID || adt.TagName == "Error" {
				return adt, nil
			}
			return vm.Apply(fn, runtime.Deref(adt.Payload))
		},
	}

	defineModuleRecord(vm, "Result", map[string]runtime.Value{
		"isOk":         resIsOk,
		"isError":      resIsError,
		"defaultValue": resDefaultValue,
		"map":          resMap,
		"mapError":     resMapError,
		"bind":         resBind,
	})
}

func registerTestModule(vm *runtime.VM) {
	testAssert := &runtime.BuiltinVal{
		Name:  "Test.assert",
		Arity: 2,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			label := runtime.Deref(args[0]).String()
			bv, ok := runtime.Deref(args[1]).(runtime.BoolVal)
			if !ok || !bv.V {
				return nil, fmt.Errorf("assertion failed [%s]", label)
			}
			return runtime.Unit, nil
		},
	}

	testAssertEq := &runtime.BuiltinVal{
		Name:  "Test.assertEq",
		Arity: 3,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			label := runtime.Deref(args[0]).String()
			expected := runtime.Deref(args[1])
			actual := runtime.Deref(args[2])
			if !runtime.ValuesEqual(expected, actual) {
				return nil, fmt.Errorf("assertion failed [%s]: expected %s, got %s", label, expected.String(), actual.String())
			}
			return runtime.Unit, nil
		},
	}

	testForall := &runtime.BuiltinVal{
		Name:  "Test.forall",
		Arity: 3,
		Fn: func(vm *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			label := runtime.Deref(args[0]).String()
			inputs, err := listToSlice(args[1])
			if err != nil {
				return nil, fmt.Errorf("Test.forall [%s]: %w", label, err)
			}
			pred := runtime.Deref(args[2])
			for i, sample := range inputs {
				res, err := vm.Apply(pred, sample)
				if err != nil {
					return nil, fmt.Errorf("property [%s] error on sample #%d (%s): %w", label, i, sample.String(), err)
				}
				bv, ok := runtime.Deref(res).(runtime.BoolVal)
				if !ok || !bv.V {
					return nil, fmt.Errorf("property [%s] falsified at sample #%d (counterexample: %s)", label, i, sample.String())
				}
			}
			return runtime.Unit, nil
		},
	}

	defineModuleRecord(vm, "Test", map[string]runtime.Value{
		"assert":   testAssert,
		"assertEq": testAssertEq,
		"forall":   testForall,
	})
}

var regexCache sync.Map

func compileCachedRegex(pattern string) (*regexp.Regexp, error) {
	if cached, ok := regexCache.Load(pattern); ok {
		return cached.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid regex %q: %w", pattern, err)
	}
	regexCache.Store(pattern, re)
	return re, nil
}

func registerRegexModule(vm *runtime.VM) {
	someID := vm.Prog.Strings.InternString("Some")
	noneID := vm.Prog.Strings.InternString("None")

	reIsMatch := &runtime.BuiltinVal{
		Name:  "Regex.isMatch",
		Arity: 2,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			pat := runtime.Deref(args[0]).(runtime.StringVal).V
			input := runtime.Deref(args[1]).(runtime.StringVal).V
			re, err := compileCachedRegex(pat)
			if err != nil {
				return nil, err
			}
			return runtime.BoolVal{V: re.MatchString(input)}, nil
		},
	}

	reFind := &runtime.BuiltinVal{
		Name:  "Regex.find",
		Arity: 2,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			pat := runtime.Deref(args[0]).(runtime.StringVal).V
			input := runtime.Deref(args[1]).(runtime.StringVal).V
			re, err := compileCachedRegex(pat)
			if err != nil {
				return nil, err
			}
			loc := re.FindStringIndex(input)
			if loc == nil {
				return runtime.ADTVal{Tag: noneID, TagName: "None"}, nil
			}
			return runtime.ADTVal{
				Tag:     someID,
				TagName: "Some",
				Payload: runtime.StringVal{V: input[loc[0]:loc[1]]},
			}, nil
		},
	}

	reFindAll := &runtime.BuiltinVal{
		Name:  "Regex.findAll",
		Arity: 2,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			pat := runtime.Deref(args[0]).(runtime.StringVal).V
			input := runtime.Deref(args[1]).(runtime.StringVal).V
			re, err := compileCachedRegex(pat)
			if err != nil {
				return nil, err
			}
			matches := re.FindAllString(input, -1)
			vals := make([]runtime.Value, len(matches))
			for i, m := range matches {
				vals[i] = runtime.StringVal{V: m}
			}
			return runtime.SliceToList(vals), nil
		},
	}

	reReplace := &runtime.BuiltinVal{
		Name:  "Regex.replace",
		Arity: 3,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			pat := runtime.Deref(args[0]).(runtime.StringVal).V
			repl := runtime.Deref(args[1]).(runtime.StringVal).V
			input := runtime.Deref(args[2]).(runtime.StringVal).V
			re, err := compileCachedRegex(pat)
			if err != nil {
				return nil, err
			}
			return runtime.StringVal{V: re.ReplaceAllString(input, repl)}, nil
		},
	}

	reSplit := &runtime.BuiltinVal{
		Name:  "Regex.split",
		Arity: 2,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			pat := runtime.Deref(args[0]).(runtime.StringVal).V
			input := runtime.Deref(args[1]).(runtime.StringVal).V
			re, err := compileCachedRegex(pat)
			if err != nil {
				return nil, err
			}
			parts := re.Split(input, -1)
			vals := make([]runtime.Value, len(parts))
			for i, p := range parts {
				vals[i] = runtime.StringVal{V: p}
			}
			return runtime.SliceToList(vals), nil
		},
	}

	defineModuleRecord(vm, "Regex", map[string]runtime.Value{
		"isMatch": reIsMatch,
		"find":    reFind,
		"findAll": reFindAll,
		"replace": reReplace,
		"split":   reSplit,
	})
}

func asFloat(v runtime.Value) float64 {
	switch nv := runtime.Deref(v).(type) {
	case runtime.FloatVal:
		return nv.V
	case runtime.IntVal:
		return float64(nv.V)
	default:
		return 0
	}
}

func registerMathModule(vm *runtime.VM) {
	mathAbs := &runtime.BuiltinVal{
		Name:  "Math.abs",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			switch nv := runtime.Deref(args[0]).(type) {
			case runtime.IntVal:
				if nv.V < 0 {
					return runtime.IntVal{V: -nv.V}, nil
				}
				return nv, nil
			case runtime.FloatVal:
				return runtime.FloatVal{V: math.Abs(nv.V)}, nil
			default:
				return nil, fmt.Errorf("Math.abs requires int or float")
			}
		},
	}

	mathMin := &runtime.BuiltinVal{
		Name:  "Math.min",
		Arity: 2,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			a := runtime.Deref(args[0])
			b := runtime.Deref(args[1])
			if runtime.CompareValues(a, b) <= 0 {
				return a, nil
			}
			return b, nil
		},
	}

	mathMax := &runtime.BuiltinVal{
		Name:  "Math.max",
		Arity: 2,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			a := runtime.Deref(args[0])
			b := runtime.Deref(args[1])
			if runtime.CompareValues(a, b) >= 0 {
				return a, nil
			}
			return b, nil
		},
	}

	mathSqrt := &runtime.BuiltinVal{
		Name:  "Math.sqrt",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			return runtime.FloatVal{V: math.Sqrt(asFloat(args[0]))}, nil
		},
	}

	mathPow := &runtime.BuiltinVal{
		Name:  "Math.pow",
		Arity: 2,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			return runtime.FloatVal{V: math.Pow(asFloat(args[0]), asFloat(args[1]))}, nil
		},
	}

	mathSin := &runtime.BuiltinVal{
		Name:  "Math.sin",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			return runtime.FloatVal{V: math.Sin(asFloat(args[0]))}, nil
		},
	}

	mathCos := &runtime.BuiltinVal{
		Name:  "Math.cos",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			return runtime.FloatVal{V: math.Cos(asFloat(args[0]))}, nil
		},
	}

	mathFloor := &runtime.BuiltinVal{
		Name:  "Math.floor",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			return runtime.FloatVal{V: math.Floor(asFloat(args[0]))}, nil
		},
	}

	mathCeil := &runtime.BuiltinVal{
		Name:  "Math.ceil",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			return runtime.FloatVal{V: math.Ceil(asFloat(args[0]))}, nil
		},
	}

	mathRound := &runtime.BuiltinVal{
		Name:  "Math.round",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			return runtime.FloatVal{V: math.Round(asFloat(args[0]))}, nil
		},
	}

	defineModuleRecord(vm, "Math", map[string]runtime.Value{
		"pi":    runtime.FloatVal{V: math.Pi},
		"abs":   mathAbs,
		"min":   mathMin,
		"max":   mathMax,
		"sqrt":  mathSqrt,
		"pow":   mathPow,
		"sin":   mathSin,
		"cos":   mathCos,
		"floor": mathFloor,
		"ceil":  mathCeil,
		"round": mathRound,
	})
}

func registerTimeModule(vm *runtime.VM) {
	timeNowUnixMs := &runtime.BuiltinVal{
		Name:  "Time.nowUnixMs",
		Arity: 1,
		Fn: func(_ *runtime.VM, _ []runtime.Value) (runtime.Value, error) {
			return runtime.IntVal{V: time.Now().UnixMilli()}, nil
		},
	}

	timeSleepMs := &runtime.BuiltinVal{
		Name:  "Time.sleepMs",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			if iv, ok := runtime.Deref(args[0]).(runtime.IntVal); ok && iv.V > 0 {
				time.Sleep(time.Duration(iv.V) * time.Millisecond)
			}
			return runtime.Unit, nil
		},
	}

	timeMeasureMs := &runtime.BuiltinVal{
		Name:  "Time.measureMs",
		Arity: 1,
		Fn: func(vm *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			thunk := runtime.Deref(args[0])
			start := time.Now()
			res, err := vm.Apply(thunk, runtime.Unit)
			if err != nil {
				return nil, err
			}
			elapsed := time.Since(start).Milliseconds()
			return runtime.TupleVal{Elems: []runtime.Value{res, runtime.IntVal{V: elapsed}}}, nil
		},
	}

	defineModuleRecord(vm, "Time", map[string]runtime.Value{
		"nowUnixMs": timeNowUnixMs,
		"sleepMs":   timeSleepMs,
		"measureMs": timeMeasureMs,
	})
}


