// Package stdlib implements the "batteries-included" standard library for FML,
// providing persistent AVL Map/Set collections backed by sync.Pool, pull-based
// lazy Seq generators, Wadler's Format pretty-printing algorithm, Erlang-style
// MailboxProcessor actors over goroutines/channels, and native HTTP/JSON/File I/O.
//
// Jaime says: A standard library shouldn't be a 3rd party dependency. Expose
// Go's goroutines as FML Mailbox processors. Expose Go's sync.Pool as FML's
// persistent map backing. Give them HTTP, JSON, and File I/O out of the box so
// they can actually build something.
package stdlib

import (
	"fmt"
	"sync"

	"fml/internal/runtime"
	"fml/internal/token"
)

// AVLNode represents an immutable node in a persistent AVL Map or Set tree.
//
// Jaime says: Persistent functional data structures allocate path nodes on
// updates. Pool the nodes with sync.Pool and keep the tree height-balanced so
// lookup is strictly O(log N).
type AVLNode struct {
	Key    runtime.Value
	Val    runtime.Value
	Height int8
	Left   *AVLNode
	Right  *AVLNode
}

var avlNodePool = sync.Pool{
	New: func() any {
		return &AVLNode{}
	},
}

// AcquireAVLNode fetches a clean AVLNode from the global sync.Pool.
func AcquireAVLNode(k, v runtime.Value, left, right *AVLNode) *AVLNode {
	n := avlNodePool.Get().(*AVLNode)
	n.Key = k
	n.Val = v
	n.Left = left
	n.Right = right
	n.Height = 1 + maxHeight(nodeHeight(left), nodeHeight(right))
	return n
}

// ReleaseAVLNode returns an unshared AVLNode to the global sync.Pool.
func ReleaseAVLNode(n *AVLNode) {
	if n == nil {
		return
	}
	n.Key = nil
	n.Val = nil
	n.Left = nil
	n.Right = nil
	n.Height = 0
	avlNodePool.Put(n)
}

func nodeHeight(n *AVLNode) int8 {
	if n == nil {
		return 0
	}
	return n.Height
}

func maxHeight(a, b int8) int8 {
	if a > b {
		return a
	}
	return b
}

func balanceFactor(n *AVLNode) int8 {
	if n == nil {
		return 0
	}
	return nodeHeight(n.Left) - nodeHeight(n.Right)
}

func rotateRight(y *AVLNode) *AVLNode {
	x := y.Left
	t2 := x.Right
	newY := AcquireAVLNode(y.Key, y.Val, t2, y.Right)
	return AcquireAVLNode(x.Key, x.Val, x.Left, newY)
}

func rotateLeft(x *AVLNode) *AVLNode {
	y := x.Right
	t2 := y.Left
	newX := AcquireAVLNode(x.Key, x.Val, x.Left, t2)
	return AcquireAVLNode(y.Key, y.Val, newX, y.Right)
}

func rebalance(n *AVLNode) *AVLNode {
	bf := balanceFactor(n)
	if bf > 1 {
		if balanceFactor(n.Left) < 0 {
			leftRot := rotateLeft(n.Left)
			tmp := AcquireAVLNode(n.Key, n.Val, leftRot, n.Right)
			res := rotateRight(tmp)
			ReleaseAVLNode(tmp)
			return res
		}
		return rotateRight(n)
	}
	if bf < -1 {
		if balanceFactor(n.Right) > 0 {
			rightRot := rotateRight(n.Right)
			tmp := AcquireAVLNode(n.Key, n.Val, n.Left, rightRot)
			res := rotateLeft(tmp)
			ReleaseAVLNode(tmp)
			return res
		}
		return rotateLeft(n)
	}
	return n
}

// AVLInsert returns a new persistent AVL tree with (k, v) inserted or updated,
// leaving the original tree root completely unmodified.
func AVLInsert(root *AVLNode, k, v runtime.Value) *AVLNode {
	if root == nil {
		return AcquireAVLNode(k, v, nil, nil)
	}
	cmp := runtime.CompareValues(k, root.Key)
	if cmp == 0 {
		return AcquireAVLNode(k, v, root.Left, root.Right)
	}
	if cmp < 0 {
		newLeft := AVLInsert(root.Left, k, v)
		return rebalance(AcquireAVLNode(root.Key, root.Val, newLeft, root.Right))
	}
	newRight := AVLInsert(root.Right, k, v)
	return rebalance(AcquireAVLNode(root.Key, root.Val, root.Left, newRight))
}

// AVLFind searches for key k in root in O(log N) time.
func AVLFind(root *AVLNode, k runtime.Value) (runtime.Value, bool) {
	curr := root
	for curr != nil {
		cmp := runtime.CompareValues(k, curr.Key)
		if cmp == 0 {
			return curr.Val, true
		}
		if cmp < 0 {
			curr = curr.Left
		} else {
			curr = curr.Right
		}
	}
	return nil, false
}

func avlMinNode(n *AVLNode) *AVLNode {
	curr := n
	for curr != nil && curr.Left != nil {
		curr = curr.Left
	}
	return curr
}

// AVLDelete returns a new persistent AVL tree with key k removed in O(log N)
// time, leaving the original tree root unmodified.
func AVLDelete(root *AVLNode, k runtime.Value) *AVLNode {
	if root == nil {
		return nil
	}
	cmp := runtime.CompareValues(k, root.Key)
	if cmp < 0 {
		newLeft := AVLDelete(root.Left, k)
		return rebalance(AcquireAVLNode(root.Key, root.Val, newLeft, root.Right))
	}
	if cmp > 0 {
		newRight := AVLDelete(root.Right, k)
		return rebalance(AcquireAVLNode(root.Key, root.Val, root.Left, newRight))
	}
	if root.Left == nil {
		return root.Right
	}
	if root.Right == nil {
		return root.Left
	}
	succ := avlMinNode(root.Right)
	newRight := AVLDelete(root.Right, succ.Key)
	return rebalance(AcquireAVLNode(succ.Key, succ.Val, root.Left, newRight))
}

// AVLInOrder appends all nodes of root in ascending key order into out.
func AVLInOrder(root *AVLNode, out *[]*AVLNode) {
	if root == nil {
		return
	}
	AVLInOrder(root.Left, out)
	*out = append(*out, root)
	AVLInOrder(root.Right, out)
}

// AVLCount returns the total number of entries in root.
func AVLCount(root *AVLNode) int64 {
	if root == nil {
		return 0
	}
	return 1 + AVLCount(root.Left) + AVLCount(root.Right)
}

func wrapMap(root *AVLNode) *runtime.NativeVal {
	return &runtime.NativeVal{
		Kind:    "Map",
		Payload: root,
		Format: func() string {
			return fmt.Sprintf("map[count=%d]", AVLCount(root))
		},
	}
}

func wrapSet(root *AVLNode) *runtime.NativeVal {
	return &runtime.NativeVal{
		Kind:    "Set",
		Payload: root,
		Format: func() string {
			return fmt.Sprintf("set[count=%d]", AVLCount(root))
		},
	}
}

func unwrapAVL(v runtime.Value) *AVLNode {
	nv, ok := runtime.Deref(v).(*runtime.NativeVal)
	if !ok || nv.Payload == nil {
		return nil
	}
	node, _ := nv.Payload.(*AVLNode)
	return node
}

// RegisterCollections binds persistent `Map` and `Set` modules into vm.
func RegisterCollections(vm *runtime.VM) {
	emptyMap := wrapMap(nil)
	mapAdd := &runtime.BuiltinVal{
		Name:  "Map.add",
		Arity: 3,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			k := runtime.Deref(args[0])
			v := runtime.Deref(args[1])
			m := unwrapAVL(args[2])
			return wrapMap(AVLInsert(m, k, v)), nil
		},
	}
	mapFind := &runtime.BuiltinVal{
		Name:  "Map.find",
		Arity: 2,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			k := runtime.Deref(args[0])
			m := unwrapAVL(args[1])
			val, ok := AVLFind(m, k)
			if !ok {
				return nil, fmt.Errorf("Map.find: key %v not found", k)
			}
			return val, nil
		},
	}
	mapTryFind := &runtime.BuiltinVal{
		Name:  "Map.tryFind",
		Arity: 2,
		Fn: func(v *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			k := runtime.Deref(args[0])
			m := unwrapAVL(args[1])
			val, ok := AVLFind(m, k)
			if !ok {
				tag := v.Prog.Strings.InternString("None")
				return runtime.ADTVal{Tag: tag, TagName: "None"}, nil
			}
			tag := v.Prog.Strings.InternString("Some")
			return runtime.ADTVal{Tag: tag, TagName: "Some", Payload: val}, nil
		},
	}
	mapContainsKey := &runtime.BuiltinVal{
		Name:  "Map.containsKey",
		Arity: 2,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			k := runtime.Deref(args[0])
			m := unwrapAVL(args[1])
			_, ok := AVLFind(m, k)
			return runtime.BoolVal{V: ok}, nil
		},
	}
	mapCount := &runtime.BuiltinVal{
		Name:  "Map.count",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			return runtime.IntVal{V: AVLCount(unwrapAVL(args[0]))}, nil
		},
	}
	mapRemove := &runtime.BuiltinVal{
		Name:  "Map.remove",
		Arity: 2,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			k := runtime.Deref(args[0])
			m := unwrapAVL(args[1])
			return wrapMap(AVLDelete(m, k)), nil
		},
	}
	mapKeys := &runtime.BuiltinVal{
		Name:  "Map.keys",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			var nodes []*AVLNode
			AVLInOrder(unwrapAVL(args[0]), &nodes)
			out := make([]runtime.Value, len(nodes))
			for i, n := range nodes {
				out[i] = n.Key
			}
			return runtime.SliceToList(out), nil
		},
	}
	mapValues := &runtime.BuiltinVal{
		Name:  "Map.values",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			var nodes []*AVLNode
			AVLInOrder(unwrapAVL(args[0]), &nodes)
			out := make([]runtime.Value, len(nodes))
			for i, n := range nodes {
				out[i] = n.Val
			}
			return runtime.SliceToList(out), nil
		},
	}

	defineModuleRecord(vm, "Map", map[string]runtime.Value{
		"empty":       emptyMap,
		"add":         mapAdd,
		"remove":      mapRemove,
		"find":        mapFind,
		"tryFind":     mapTryFind,
		"containsKey": mapContainsKey,
		"count":       mapCount,
		"keys":        mapKeys,
		"values":      mapValues,
	})

	emptySet := wrapSet(nil)
	setAdd := &runtime.BuiltinVal{
		Name:  "Set.add",
		Arity: 2,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			elem := runtime.Deref(args[0])
			s := unwrapAVL(args[1])
			return wrapSet(AVLInsert(s, elem, runtime.Unit)), nil
		},
	}
	setRemove := &runtime.BuiltinVal{
		Name:  "Set.remove",
		Arity: 2,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			elem := runtime.Deref(args[0])
			s := unwrapAVL(args[1])
			return wrapSet(AVLDelete(s, elem)), nil
		},
	}
	setContains := &runtime.BuiltinVal{
		Name:  "Set.contains",
		Arity: 2,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			elem := runtime.Deref(args[0])
			s := unwrapAVL(args[1])
			_, ok := AVLFind(s, elem)
			return runtime.BoolVal{V: ok}, nil
		},
	}
	setCount := &runtime.BuiltinVal{
		Name:  "Set.count",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			return runtime.IntVal{V: AVLCount(unwrapAVL(args[0]))}, nil
		},
	}
	setUnion := &runtime.BuiltinVal{
		Name:  "Set.union",
		Arity: 2,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			s1 := unwrapAVL(args[0])
			s2 := unwrapAVL(args[1])
			var nodes []*AVLNode
			AVLInOrder(s2, &nodes)
			res := s1
			for _, n := range nodes {
				res = AVLInsert(res, n.Key, runtime.Unit)
			}
			return wrapSet(res), nil
		},
	}
	setIntersect := &runtime.BuiltinVal{
		Name:  "Set.intersect",
		Arity: 2,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			s1 := unwrapAVL(args[0])
			s2 := unwrapAVL(args[1])
			var nodes []*AVLNode
			AVLInOrder(s1, &nodes)
			var res *AVLNode
			for _, n := range nodes {
				if _, ok := AVLFind(s2, n.Key); ok {
					res = AVLInsert(res, n.Key, runtime.Unit)
				}
			}
			return wrapSet(res), nil
		},
	}
	setDiff := &runtime.BuiltinVal{
		Name:  "Set.diff",
		Arity: 2,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			s1 := unwrapAVL(args[0])
			s2 := unwrapAVL(args[1])
			var nodes []*AVLNode
			AVLInOrder(s2, &nodes)
			res := s1
			for _, n := range nodes {
				res = AVLDelete(res, n.Key)
			}
			return wrapSet(res), nil
		},
	}
	setIsSubset := &runtime.BuiltinVal{
		Name:  "Set.isSubset",
		Arity: 2,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			s1 := unwrapAVL(args[0])
			s2 := unwrapAVL(args[1])
			var nodes []*AVLNode
			AVLInOrder(s1, &nodes)
			for _, n := range nodes {
				if _, ok := AVLFind(s2, n.Key); !ok {
					return runtime.BoolVal{V: false}, nil
				}
			}
			return runtime.BoolVal{V: true}, nil
		},
	}
	setToList := &runtime.BuiltinVal{
		Name:  "Set.toList",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			var nodes []*AVLNode
			AVLInOrder(unwrapAVL(args[0]), &nodes)
			out := make([]runtime.Value, len(nodes))
			for i, n := range nodes {
				out[i] = n.Key
			}
			return runtime.SliceToList(out), nil
		},
	}
	setOfList := &runtime.BuiltinVal{
		Name:  "Set.ofList",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			items := runtime.ListToSlice(runtime.Deref(args[0]))
			var res *AVLNode
			for _, elem := range items {
				res = AVLInsert(res, runtime.Deref(elem), runtime.Unit)
			}
			return wrapSet(res), nil
		},
	}

	defineModuleRecord(vm, "Set", map[string]runtime.Value{
		"empty":     emptySet,
		"add":       setAdd,
		"remove":    setRemove,
		"contains":  setContains,
		"count":     setCount,
		"union":     setUnion,
		"intersect": setIntersect,
		"diff":      setDiff,
		"isSubset":  setIsSubset,
		"toList":    setToList,
		"ofList":    setOfList,
	})
}

func defineModuleRecord(vm *runtime.VM, modName string, members map[string]runtime.Value) {
	fields := make(map[token.StringID]runtime.Value, len(members))
	byName := make(map[string]runtime.Value, len(members))
	for k, v := range members {
		fid := vm.Prog.Strings.InternString(k)
		fields[fid] = v
		byName[k] = v
		vm.DefineGlobal(modName+"."+k, v)
	}
	vm.DefineGlobal(modName, &runtime.RecordVal{
		Fields: fields,
		ByName: byName,
	})
}
