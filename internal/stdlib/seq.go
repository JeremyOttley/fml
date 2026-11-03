package stdlib

import (
	"fmt"

	"fml/internal/runtime"
)

// SeqIter is a pull-based lazy iterator factory `func() func() (runtime.Value, bool, error)`.
//
// Jaime says: A lazy sequence shouldn't spin up a goroutine and a channel per
// element. It's a pull-based closure state machine. Call `next()` when you want
// a value; stop calling it when you're done.
type SeqIter func() func() (runtime.Value, bool, error)

func wrapSeq(iter SeqIter) *runtime.NativeVal {
	return &runtime.NativeVal{
		Kind:    "Seq",
		Payload: iter,
		Format:  func() string { return "<seq>" },
	}
}

func unwrapSeq(v runtime.Value) (SeqIter, error) {
	v = runtime.Deref(v)
	if nv, ok := v.(*runtime.NativeVal); ok {
		if it, ok := nv.Payload.(SeqIter); ok {
			return it, nil
		}
	}
	if lv, ok := v.(*runtime.ListVal); ok {
		return func() func() (runtime.Value, bool, error) {
			curr := lv
			return func() (runtime.Value, bool, error) {
				if curr == nil || curr.Empty {
					return nil, false, nil
				}
				val := curr.Head
				curr = curr.Tail
				return val, true, nil
			}
		}, nil
	}
	return nil, fmt.Errorf("expected Seq or List value, got %v", v)
}

// RegisterSeq registers the lazy `Seq` module in vm.
func RegisterSeq(vm *runtime.VM) {
	seqInit := &runtime.BuiltinVal{
		Name:  "Seq.init",
		Arity: 2,
		Fn: func(v *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			count := runtime.Deref(args[0]).(runtime.IntVal).V
			fn := runtime.Deref(args[1])
			return wrapSeq(func() func() (runtime.Value, bool, error) {
				idx := int64(0)
				return func() (runtime.Value, bool, error) {
					if idx >= count {
						return nil, false, nil
					}
					cur := idx
					idx++
					res, err := v.Apply(fn, runtime.IntVal{V: cur})
					return res, err == nil, err
				}
			}), nil
		},
	}

	seqInitInfinite := &runtime.BuiltinVal{
		Name:  "Seq.initInfinite",
		Arity: 1,
		Fn: func(v *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			fn := runtime.Deref(args[0])
			return wrapSeq(func() func() (runtime.Value, bool, error) {
				idx := int64(0)
				return func() (runtime.Value, bool, error) {
					cur := idx
					idx++
					res, err := v.Apply(fn, runtime.IntVal{V: cur})
					return res, err == nil, err
				}
			}), nil
		},
	}

	seqMap := &runtime.BuiltinVal{
		Name:  "Seq.map",
		Arity: 2,
		Fn: func(v *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			fn := runtime.Deref(args[0])
			srcIter, err := unwrapSeq(args[1])
			if err != nil {
				return nil, err
			}
			return wrapSeq(func() func() (runtime.Value, bool, error) {
				next := srcIter()
				return func() (runtime.Value, bool, error) {
					elem, ok, err := next()
					if !ok || err != nil {
						return nil, ok, err
					}
					mapped, err := v.Apply(fn, elem)
					return mapped, err == nil, err
				}
			}), nil
		},
	}

	seqFilter := &runtime.BuiltinVal{
		Name:  "Seq.filter",
		Arity: 2,
		Fn: func(v *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			pred := runtime.Deref(args[0])
			srcIter, err := unwrapSeq(args[1])
			if err != nil {
				return nil, err
			}
			return wrapSeq(func() func() (runtime.Value, bool, error) {
				next := srcIter()
				return func() (runtime.Value, bool, error) {
					for {
						elem, ok, err := next()
						if !ok || err != nil {
							return nil, ok, err
						}
						keepVal, err := v.Apply(pred, elem)
						if err != nil {
							return nil, false, err
						}
						if bv, ok := runtime.Deref(keepVal).(runtime.BoolVal); ok && bv.V {
							return elem, true, nil
						}
					}
				}
			}), nil
		},
	}

	seqTake := &runtime.BuiltinVal{
		Name:  "Seq.take",
		Arity: 2,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			limit := runtime.Deref(args[0]).(runtime.IntVal).V
			srcIter, err := unwrapSeq(args[1])
			if err != nil {
				return nil, err
			}
			return wrapSeq(func() func() (runtime.Value, bool, error) {
				next := srcIter()
				taken := int64(0)
				return func() (runtime.Value, bool, error) {
					if taken >= limit {
						return nil, false, nil
					}
					taken++
					return next()
				}
			}), nil
		},
	}

	seqToList := &runtime.BuiltinVal{
		Name:  "Seq.toList",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			srcIter, err := unwrapSeq(args[0])
			if err != nil {
				return nil, err
			}
			next := srcIter()
			var items []runtime.Value
			for {
				elem, ok, err := next()
				if err != nil {
					return nil, err
				}
				if !ok {
					break
				}
				items = append(items, runtime.Deref(elem))
			}
			return runtime.SliceToList(items), nil
		},
	}

	seqSum := &runtime.BuiltinVal{
		Name:  "Seq.sum",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			srcIter, err := unwrapSeq(args[0])
			if err != nil {
				return nil, err
			}
			next := srcIter()
			var total int64
			for {
				elem, ok, err := next()
				if err != nil {
					return nil, err
				}
				if !ok {
					break
				}
				total += runtime.Deref(elem).(runtime.IntVal).V
			}
			return runtime.IntVal{V: total}, nil
		},
	}

	defineModuleRecord(vm, "Seq", map[string]runtime.Value{
		"init":         seqInit,
		"initInfinite": seqInitInfinite,
		"map":          seqMap,
		"filter":       seqFilter,
		"take":         seqTake,
		"toList":       seqToList,
		"sum":          seqSum,
	})
}
