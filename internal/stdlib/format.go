package stdlib

import (
	"strings"

	"fml/internal/runtime"
)

// DocKind identifies a node in Wadler's pretty-printing document algebra.
type DocKind uint8

const (
	DocNil DocKind = iota
	DocText
	DocLine
	DocCat
	DocNest
	DocGroup
)

// Doc represents an algebraic pretty-printing document node (Wadler's "A Prettier Printer").
//
// Jaime says: OCaml's Format module got this right decades ago. Separate
// document structure from layout width, then flatten groups that fit on the
// current line and break the ones that don't.
type Doc struct {
	Kind   DocKind
	Text   string
	Indent int
	Left   *Doc
	Right  *Doc
}

type layoutPair struct {
	indent int
	mode   bool // true = flat, false = break
	doc    *Doc
}

// Pretty renders doc within maxWidth columns using Wadler's linear-time layout algorithm.
func Pretty(maxWidth int, doc *Doc) string {
	var out strings.Builder
	col := 0
	work := []layoutPair{{indent: 0, mode: false, doc: doc}}

	for len(work) > 0 {
		curr := work[len(work)-1]
		work = work[:len(work)-1]
		if curr.doc == nil {
			continue
		}

		switch curr.doc.Kind {
		case DocNil:
			continue
		case DocText:
			out.WriteString(curr.doc.Text)
			col += len(curr.doc.Text)
		case DocLine:
			if curr.mode {
				out.WriteByte(' ')
				col++
			} else {
				out.WriteByte('\n')
				for i := 0; i < curr.indent; i++ {
					out.WriteByte(' ')
				}
				col = curr.indent
			}
		case DocCat:
			work = append(work,
				layoutPair{indent: curr.indent, mode: curr.mode, doc: curr.doc.Right},
				layoutPair{indent: curr.indent, mode: curr.mode, doc: curr.doc.Left},
			)
		case DocNest:
			work = append(work, layoutPair{
				indent: curr.indent + curr.doc.Indent,
				mode:   curr.mode,
				doc:    curr.doc.Left,
			})
		case DocGroup:
			flatCandidate := append([]layoutPair{}, work...)
			flatCandidate = append(flatCandidate, layoutPair{
				indent: curr.indent,
				mode:   true,
				doc:    curr.doc.Left,
			})
			if fits(maxWidth-col, flatCandidate) {
				work = append(work, layoutPair{
					indent: curr.indent,
					mode:   true,
					doc:    curr.doc.Left,
				})
			} else {
				work = append(work, layoutPair{
					indent: curr.indent,
					mode:   false,
					doc:    curr.doc.Left,
				})
			}
		}
	}
	return out.String()
}

func fits(remaining int, stack []layoutPair) bool {
	for remaining >= 0 && len(stack) > 0 {
		curr := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if curr.doc == nil {
			continue
		}
		switch curr.doc.Kind {
		case DocNil:
			continue
		case DocText:
			remaining -= len(curr.doc.Text)
		case DocLine:
			if curr.mode {
				remaining--
			} else {
				return true
			}
		case DocCat:
			stack = append(stack,
				layoutPair{indent: curr.indent, mode: curr.mode, doc: curr.doc.Right},
				layoutPair{indent: curr.indent, mode: curr.mode, doc: curr.doc.Left},
			)
		case DocNest:
			stack = append(stack, layoutPair{
				indent: curr.indent + curr.doc.Indent,
				mode:   curr.mode,
				doc:    curr.doc.Left,
			})
		case DocGroup:
			stack = append(stack, layoutPair{
				indent: curr.indent,
				mode:   curr.mode,
				doc:    curr.doc.Left,
			})
		}
	}
	return remaining >= 0
}

func wrapDoc(d *Doc) *runtime.NativeVal {
	return &runtime.NativeVal{
		Kind:    "Format.Doc",
		Payload: d,
		Format:  func() string { return Pretty(80, d) },
	}
}

func unwrapDoc(v runtime.Value) *Doc {
	nv, ok := runtime.Deref(v).(*runtime.NativeVal)
	if !ok || nv.Payload == nil {
		return &Doc{Kind: DocNil}
	}
	d, _ := nv.Payload.(*Doc)
	return d
}

// RegisterFormat binds OCaml-inspired Wadler pretty-printing primitives into vm.
func RegisterFormat(vm *runtime.VM) {
	docEmpty := wrapDoc(&Doc{Kind: DocNil})
	docLine := wrapDoc(&Doc{Kind: DocLine})

	docText := &runtime.BuiltinVal{
		Name:  "Format.text",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			s := runtime.Deref(args[0]).String()
			return wrapDoc(&Doc{Kind: DocText, Text: s}), nil
		},
	}

	docCat := &runtime.BuiltinVal{
		Name:  "Format.cat",
		Arity: 2,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			l := unwrapDoc(args[0])
			r := unwrapDoc(args[1])
			return wrapDoc(&Doc{Kind: DocCat, Left: l, Right: r}), nil
		},
	}

	docNest := &runtime.BuiltinVal{
		Name:  "Format.nest",
		Arity: 2,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			ind := int(runtime.Deref(args[0]).(runtime.IntVal).V)
			d := unwrapDoc(args[1])
			return wrapDoc(&Doc{Kind: DocNest, Indent: ind, Left: d}), nil
		},
	}

	docGroup := &runtime.BuiltinVal{
		Name:  "Format.group",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			d := unwrapDoc(args[0])
			return wrapDoc(&Doc{Kind: DocGroup, Left: d}), nil
		},
	}

	docPretty := &runtime.BuiltinVal{
		Name:  "Format.pretty",
		Arity: 2,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			width := int(runtime.Deref(args[0]).(runtime.IntVal).V)
			d := unwrapDoc(args[1])
			return runtime.StringVal{V: Pretty(width, d)}, nil
		},
	}

	defineModuleRecord(vm, "Format", map[string]runtime.Value{
		"empty":  docEmpty,
		"line":   docLine,
		"doc":    docText,
		"text":   docText,
		"cat":    docCat,
		"concat": docCat,
		"nest":   docNest,
		"group":  docGroup,
		"pretty": docPretty,
	})
}
