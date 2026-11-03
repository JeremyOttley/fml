// Package runtime implements the closed-interface Value representation and
// tail-call-optimized IR execution engine for FML.
//
// Jaime says: Go doesn't have sum types, so we build our own with a closed
// interface and an unexported `isValue()` marker method. Then a Go type switch
// is as close to a hardware jump table as the runtime will give us.
package runtime

import (
	"fmt"
	"strings"

	"fml/internal/ir"
	"fml/internal/token"
)

// Value is a closed sum-type interface representing any runtime FML value.
type Value interface {
	isValue()
	String() string
}

// UnitVal represents the singleton `()` value.
type UnitVal struct{}

func (UnitVal) isValue()       {}
func (UnitVal) String() string { return "()" }

// Unit is the canonical singleton UnitVal.
var Unit Value = UnitVal{}

// BoolVal represents a boolean value.
type BoolVal struct{ V bool }

func (BoolVal) isValue() {}
func (b BoolVal) String() string {
	if b.V {
		return "true"
	}
	return "false"
}

// IntVal represents a 64-bit signed integer value.
type IntVal struct{ V int64 }

func (IntVal) isValue()       {}
func (i IntVal) String() string { return fmt.Sprintf("%d", i.V) }

// FloatVal represents a 64-bit IEEE-754 floating-point value.
type FloatVal struct{ V float64 }

func (FloatVal) isValue()       {}
func (f FloatVal) String() string { return fmt.Sprintf("%g", f.V) }

// StringVal represents an immutable UTF-8 string value.
type StringVal struct{ V string }

func (StringVal) isValue()       {}
func (s StringVal) String() string { return s.V }

// TupleVal represents a heterogeneous tuple `(v1, v2, ...)`.
type TupleVal struct{ Elems []Value }

func (TupleVal) isValue() {}
func (t TupleVal) String() string {
	parts := make([]string, len(t.Elems))
	for i, e := range t.Elems {
		parts[i] = e.String()
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

// ListVal represents an immutable singly-linked list node (`Head :: Tail` or empty `nil`).
type ListVal struct {
	Empty bool
	Head  Value
	Tail  *ListVal
}

func (*ListVal) isValue() {}
func (l *ListVal) String() string {
	if l == nil || l.Empty {
		return "[]"
	}
	var parts []string
	curr := l
	for curr != nil && !curr.Empty {
		parts = append(parts, curr.Head.String())
		curr = curr.Tail
	}
	return "[" + strings.Join(parts, "; ") + "]"
}

// EmptyList is the singleton empty ListVal `[]`.
var EmptyList = &ListVal{Empty: true}

// ArrayVal represents a fixed-size mutable-in-place contiguous array `[| ... |]`.
type ArrayVal struct{ Elems []Value }

func (*ArrayVal) isValue() {}
func (a *ArrayVal) String() string {
	parts := make([]string, len(a.Elems))
	for i, e := range a.Elems {
		parts[i] = e.String()
	}
	return "[|" + strings.Join(parts, "; ") + "|]"
}

// RecordVal represents a structural record or module namespace table.
type RecordVal struct {
	Fields map[token.StringID]Value
	ByName map[string]Value
}

func (*RecordVal) isValue() {}
func (r *RecordVal) String() string {
	if len(r.ByName) == 0 {
		return "{}"
	}
	var parts []string
	for k, v := range r.ByName {
		parts = append(parts, fmt.Sprintf("%s = %s", k, v.String()))
	}
	return "{ " + strings.Join(parts, "; ") + " }"
}

// ADTVal represents an Algebraic Data Type (Discriminated Union, Option, or Result) variant.
type ADTVal struct {
	Tag     token.StringID
	TagName string
	Payload Value
}

func (ADTVal) isValue() {}
func (a ADTVal) String() string {
	if a.Payload == nil {
		return a.TagName
	}
	return fmt.Sprintf("%s(%s)", a.TagName, a.Payload.String())
}

// RefCell boxes a mutable variable (`let mutable`) so closures and loops mutate
// the shared storage location directly.
type RefCell struct {
	Val Value
}

func (*RefCell) isValue()       {}
func (r *RefCell) String() string { return r.Val.String() }

// ClosureVal represents a first-class function closure with its flat captured environment.
type ClosureVal struct {
	Param   token.StringID
	Body    ir.InstrID
	Env     map[token.StringID]Value
	SelfSym token.StringID
}

func (*ClosureVal) isValue()       {}
func (c *ClosureVal) String() string { return "<fun>" }

// BuiltinVal represents a curried native Go standard library function exposed to FML.
type BuiltinVal struct {
	Name  string
	Arity int
	Args  []Value
	Fn    func(vm *VM, args []Value) (Value, error)
}

func (*BuiltinVal) isValue()       {}
func (b *BuiltinVal) String() string { return fmt.Sprintf("<builtin:%s>", b.Name) }

// DisposableVal wraps a resource that implements deterministic `use` cleanup.
type DisposableVal struct {
	Inner     Value
	Disposed  bool
	OnDispose func()
}

func (*DisposableVal) isValue()       {}
func (d *DisposableVal) String() string { return d.Inner.String() }

// Dispose invokes the cleanup callback at most once.
func (d *DisposableVal) Dispose() {
	if !d.Disposed {
		d.Disposed = true
		if d.OnDispose != nil {
			d.OnDispose()
		}
	}
}

// NativeVal wraps specialized standard library structures (persistent AVL Map/Set,
// lazy Seq generators, Wadler Format documents, and MailboxProcessor actors)
// inside the closed Value sum type.
type NativeVal struct {
	Kind    string
	Payload any
	Format  func() string
}

func (*NativeVal) isValue() {}
func (n *NativeVal) String() string {
	if n.Format != nil {
		return n.Format()
	}
	return fmt.Sprintf("<%s>", n.Kind)
}

// ValuesEqual performs structural equality across FML Values.
func ValuesEqual(a, b Value) bool {
	a = Deref(a)
	b = Deref(b)
	switch va := a.(type) {
	case UnitVal:
		_, ok := b.(UnitVal)
		return ok
	case BoolVal:
		vb, ok := b.(BoolVal)
		return ok && va.V == vb.V
	case IntVal:
		vb, ok := b.(IntVal)
		return ok && va.V == vb.V
	case FloatVal:
		vb, ok := b.(FloatVal)
		return ok && va.V == vb.V
	case StringVal:
		vb, ok := b.(StringVal)
		return ok && va.V == vb.V
	case TupleVal:
		vb, ok := b.(TupleVal)
		if !ok || len(va.Elems) != len(vb.Elems) {
			return false
		}
		for i := range va.Elems {
			if !ValuesEqual(va.Elems[i], vb.Elems[i]) {
				return false
			}
		}
		return true
	case *ListVal:
		vb, ok := b.(*ListVal)
		if !ok {
			return false
		}
		ca, cb := va, vb
		for ca != nil && !ca.Empty && cb != nil && !cb.Empty {
			if !ValuesEqual(ca.Head, cb.Head) {
				return false
			}
			ca = ca.Tail
			cb = cb.Tail
		}
		return (ca == nil || ca.Empty) && (cb == nil || cb.Empty)
	case ADTVal:
		vb, ok := b.(ADTVal)
		if !ok || va.Tag != vb.Tag {
			return false
		}
		if va.Payload == nil && vb.Payload == nil {
			return true
		}
		if va.Payload == nil || vb.Payload == nil {
			return false
		}
		return ValuesEqual(va.Payload, vb.Payload)
	case *RecordVal:
		vb, ok := b.(*RecordVal)
		if !ok || len(va.ByName) != len(vb.ByName) {
			return false
		}
		for k, fv := range va.ByName {
			other, exists := vb.ByName[k]
			if !exists || !ValuesEqual(fv, other) {
				return false
			}
		}
		return true
	default:
		return a == b
	}
}

// CompareValues orders two FML values (-1 if a < b, 0 if a == b, +1 if a > b).
func CompareValues(a, b Value) int {
	a = Deref(a)
	b = Deref(b)
	switch va := a.(type) {
	case IntVal:
		if vb, ok := b.(IntVal); ok {
			if va.V < vb.V {
				return -1
			}
			if va.V > vb.V {
				return 1
			}
			return 0
		}
	case FloatVal:
		if vb, ok := b.(FloatVal); ok {
			if va.V < vb.V {
				return -1
			}
			if va.V > vb.V {
				return 1
			}
			return 0
		}
	case StringVal:
		if vb, ok := b.(StringVal); ok {
			return strings.Compare(va.V, vb.V)
		}
	case BoolVal:
		if vb, ok := b.(BoolVal); ok {
			if va.V == vb.V {
				return 0
			}
			if !va.V && vb.V {
				return -1
			}
			return 1
		}
	}
	if ValuesEqual(a, b) {
		return 0
	}
	return strings.Compare(a.String(), b.String())
}

// Deref unwraps a RefCell or DisposableVal to its underlying Value.
func Deref(v Value) Value {
	for {
		switch cell := v.(type) {
		case *RefCell:
			v = cell.Val
		case *DisposableVal:
			v = cell.Inner
		default:
			return v
		}
	}
}
