package stdlib

import (
	"sync"
	"time"

	"fml/internal/runtime"
	"fml/internal/token"
)

// AsyncTask represents a lazy asynchronous workflow `func(vm *runtime.VM) (runtime.Value, error)`.
//
// Jaime says: An `async { ... }` block shouldn't eagerly spawn a goroutine on
// construction; it builds a lightweight thunk. Then `Async.RunSynchronously` runs
// it on the current thread, or `Async.Parallel` fans a list of thunks out across
// Go goroutines with isolated VM snapshots and joins them with a WaitGroup.
type AsyncTask func(vm *runtime.VM) (runtime.Value, error)

func wrapAsync(task AsyncTask) *runtime.NativeVal {
	return &runtime.NativeVal{
		Kind:    "Async",
		Payload: task,
		Format:  func() string { return "<async>" },
	}
}

func runAsyncVal(vm *runtime.VM, v runtime.Value) (runtime.Value, error) {
	v = runtime.Deref(v)
	if nv, ok := v.(*runtime.NativeVal); ok && nv.Kind == "Async" {
		if task, ok := nv.Payload.(AsyncTask); ok {
			return task(vm)
		}
	}
	return v, nil
}

func cloneVMForAsync(parentVM *runtime.VM) *runtime.VM {
	child := &runtime.VM{
		Prog:       parentVM.Prog,
		Globals:    make(map[token.StringID]runtime.Value, len(parentVM.Globals)),
		Stdout:     parentVM.Stdout,
		CustomEval: parentVM.CustomEval,
	}
	for k, v := range parentVM.Globals {
		child.Globals[k] = v
	}
	return child
}

// RegisterAsync registers the `async` and `task` computation expression builders
// as well as the `Async` concurrency module (`RunSynchronously`, `Parallel`,
// `Sleep`, `Start`, `Return`).
func RegisterAsync(vm *runtime.VM) {
	asyncBind := &runtime.BuiltinVal{
		Name:  "async.Bind",
		Arity: 2,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			m := runtime.Deref(args[0])
			cont := runtime.Deref(args[1])
			return wrapAsync(func(execVM *runtime.VM) (runtime.Value, error) {
				unwrapped, err := runAsyncVal(execVM, m)
				if err != nil {
					return nil, err
				}
				nextWorkflow, err := execVM.Apply(cont, unwrapped)
				if err != nil {
					return nil, err
				}
				return runAsyncVal(execVM, nextWorkflow)
			}), nil
		},
	}
	asyncReturn := &runtime.BuiltinVal{
		Name:  "async.Return",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			val := runtime.Deref(args[0])
			return wrapAsync(func(_ *runtime.VM) (runtime.Value, error) {
				return val, nil
			}), nil
		},
	}
	asyncReturnFrom := &runtime.BuiltinVal{
		Name:  "async.ReturnFrom",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			m := runtime.Deref(args[0])
			return wrapAsync(func(execVM *runtime.VM) (runtime.Value, error) {
				return runAsyncVal(execVM, m)
			}), nil
		},
	}
	asyncZero := &runtime.BuiltinVal{
		Name:  "async.Zero",
		Arity: 1,
		Fn: func(_ *runtime.VM, _ []runtime.Value) (runtime.Value, error) {
			return wrapAsync(func(_ *runtime.VM) (runtime.Value, error) {
				return runtime.Unit, nil
			}), nil
		},
	}

	builderMembers := map[string]runtime.Value{
		"Bind":       asyncBind,
		"Return":     asyncReturn,
		"ReturnFrom": asyncReturnFrom,
		"Zero":       asyncZero,
	}
	defineModuleRecord(vm, "async", builderMembers)
	defineModuleRecord(vm, "task", builderMembers)

	runSync := &runtime.BuiltinVal{
		Name:  "Async.RunSynchronously",
		Arity: 1,
		Fn: func(execVM *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			return runAsyncVal(execVM, args[0])
		},
	}
	parallel := &runtime.BuiltinVal{
		Name:  "Async.Parallel",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			tasks, err := listToSlice(args[0])
			if err != nil {
				return nil, err
			}
			return wrapAsync(func(execVM *runtime.VM) (runtime.Value, error) {
				results := make([]runtime.Value, len(tasks))
				errs := make([]error, len(tasks))
				workers := make([]*runtime.VM, len(tasks))
				for i := range tasks {
					workers[i] = cloneVMForAsync(execVM)
				}
				var wg sync.WaitGroup
				wg.Add(len(tasks))
				for i, tVal := range tasks {
					go func(idx int, wVM *runtime.VM, taskVal runtime.Value) {
						defer wg.Done()
						res, err := runAsyncVal(wVM, taskVal)
						results[idx] = res
						errs[idx] = err
					}(i, workers[i], tVal)
				}
				wg.Wait()
				for _, e := range errs {
					if e != nil {
						return nil, e
					}
				}
				return runtime.SliceToList(results), nil
			}), nil
		},
	}
	sleep := &runtime.BuiltinVal{
		Name:  "Async.Sleep",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			ms := runtime.Deref(args[0]).(runtime.IntVal).V
			return wrapAsync(func(_ *runtime.VM) (runtime.Value, error) {
				if ms > 0 {
					time.Sleep(time.Duration(ms) * time.Millisecond)
				}
				return runtime.Unit, nil
			}), nil
		},
	}
	startBg := &runtime.BuiltinVal{
		Name:  "Async.Start",
		Arity: 1,
		Fn: func(execVM *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			taskVal := runtime.Deref(args[0])
			bgVM := cloneVMForAsync(execVM)
			go func() {
				_, _ = runAsyncVal(bgVM, taskVal)
			}()
			return runtime.Unit, nil
		},
	}

	defineModuleRecord(vm, "Async", map[string]runtime.Value{
		"RunSynchronously": runSync,
		"Parallel":         parallel,
		"Sleep":            sleep,
		"Start":            startBg,
		"Return":           asyncReturn,
	})
}
