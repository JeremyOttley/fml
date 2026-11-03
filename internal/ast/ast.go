// Package ast defines the syntax tree for FML.
//
// Jaime says: The AST is a data structure, not an object hierarchy.
// We don't use interfaces for AST nodes because dynamic dispatch in a tight
// compiler loop is a fool's errand. We use a flat slice of Node structs.
// A Node is 24 bytes (well under 32 bytes). We pack the Kind, Flags, TokenIdx,
// Children indices, and inline Data tightly. Let the CPU prefetcher do its job.
package ast

import "fml/internal/token"

// NodeID is an index into Arena.Nodes. Index 0 is reserved for NoNode.
// Jaime says: A uint32 index can address 4 billion AST nodes while taking half
// the space of a 64-bit pointer and zero GC mark time.
type NodeID uint32

// NoNode is the null sentinel index in Arena.Nodes.
const NoNode NodeID = 0

// NodeKind identifies the variant of the FML syntax node.
type NodeKind uint8

const (
	KindInvalid NodeKind = iota

	// Root & Blocks
	KindProgram
	KindBlock

	// Literals & Identifiers
	KindIdent
	KindInt
	KindFloat
	KindString
	KindBool
	KindUnit

	// Bindings & Variables
	KindLet        // let x = expr
	KindLetMutable // let mutable x = expr
	KindMutate     // x <- expr
	KindLetRec     // let rec f x = expr
	KindUse        // use r = expr (or let use r = expr)

	// Functions & Operators
	KindLambda    // fun x -> expr
	KindApply     // f x
	KindInfix     // a + b, a == b, etc. (Op stored in Node.Op)
	KindPrefix    // -x, not x
	KindPipeRight // x |> f
	KindPipeLeft  // f <| x
	KindCompose   // f >> g or f << g

	// Data Types & Structures
	KindTuple       // (a, b, ...)
	KindRecordLit   // { Name = "Alice"; Age = 30 }
	KindRecordField // Field = Expr or Field: Type
	KindFieldAccess // r.Field
	KindRecordType  // type Person = { Name: string; Age: int }
	KindDUType      // type Shape = Circle of float | Rect of float * float
	KindDUCase      // Circle of float
	KindList        // [1; 2; 3]
	KindArray       // [| 1; 2; 3 |]
	KindRange       // a .. b
	KindSeq         // seq { ... }
	KindOption      // Some x / None
	KindResult      // Ok x / Error e
	KindMeasure     // [<Measure>] type kg / 5.0<kg>
	KindMeasureUnit // unit expression inside <...>

	// Pattern Matching
	KindMatch         // match x with | p -> e
	KindMatchArm      // | pat when guard -> body
	KindGuard         // when cond
	KindPatWildcard   // _
	KindPatVar        // identifier pattern
	KindPatConst      // literal pattern
	KindPatTuple      // (p1, p2)
	KindPatCons       // h :: t
	KindPatList       // [p1; p2]
	KindPatRecord     // { Field = p }
	KindPatDU         // Tag p
	KindPatTypeTest   // :? ExnType as e
	KindActivePattern // let (|Even|Odd|) n = ...

	// Control Flow
	KindIf         // if cond then t else f
	KindForIn      // for x in xs do body
	KindForTo      // for i = start to end do body
	KindWhile      // while cond do body
	KindTryWith    // try body with handlers
	KindTryFinally // try body finally cleanup
	KindRaise      // raise expr

	// Object-Oriented Constructs
	KindClass     // type MyClass(x: int) = member _.X = x
	KindInterface // type IPrintable = abstract member Print: unit -> unit
	KindMember    // member this.Name = ... / abstract member ...
	KindObjExpr   // { new IPrintable with member _.Print() = ... }
	KindTypeExt   // type System.Int32 with member this.IsEven = ...

	// Computation Expressions
	KindCompExpr // async { ... }, seq { ... }, task { ... }, custom { ... }
	KindBindBang // let! x = expr, do! expr, use! x = expr
	KindReturn   // return expr, return! expr, yield expr

	// Modules, Namespaces & Functors
	KindModule    // module M = ...
	KindNamespace // namespace N
	KindOpen      // open M
	KindFunctor   // module F (X: Sig) = ...

	// Advanced / Metaprogramming
	KindQuote        // <@ expr @>
	KindTypeProvider // type DB = SqlDataProvider<...>
	KindAttr         // [<CLIMutable>], [<Measure>]

	// Type Annotations
	KindTypeAnnot // expr : Type or param : Type
	KindTypeIdent // int, float, string, bool, unit, 'a
	KindTypeArrow // T1 -> T2
	KindTypeTuple // T1 * T2
	KindTypeApp   // float<kg>, list<int>, option<int>
)

// Binding and node flag constants stored in Node.Flags.
const (
	FlagMutable uint8 = 1 << iota
	FlagRec
	FlagUse
	FlagBang       // let!, do!, use!, return!
	FlagBackward   // << or <| or downto
	FlagAbstract   // abstract member
	FlagStatic     // static member
	FlagPartialPat // (|Tag|_|) partial active pattern
)

// Node represents a single AST element.
//
// Jaime says: Keep this struct small. If it grows past 32 bytes (half a cache
// line), you are storing too much metadata. Right now it is 24 bytes flat.
type Node struct {
	Kind     NodeKind
	Flags    uint8
	Op       uint16 // Operator TokenKind or sub-tag
	TokenIdx uint32 // Index into the lexer's token array
	Left     uint32 // Index of LHS child in the AST arena (or start index in Extra)
	Right    uint32 // Index of RHS child in the AST arena (or count in Extra)
	Data     uint64 // Inline payload (e.g., uint64 int/float bits or StringID)
}

// Arena manages flat contiguous slices of AST Nodes and auxiliary child indices.
//
// Jaime says: Allocate the arena once per compilation unit. Appending a node
// is a slice bounds check and an index increment. Freeing the entire tree is
// resetting the slice length to 1.
type Arena struct {
	Nodes   []Node
	Extra   []uint32
	Strings *token.StringTable
}

// NewArena creates a pre-allocated AST Arena with index 0 reserved as NoNode.
func NewArena(nodeCap, extraCap int, strings *token.StringTable) *Arena {
	if nodeCap < 256 {
		nodeCap = 256
	}
	if extraCap < 256 {
		extraCap = 256
	}
	if strings == nil {
		strings = token.NewStringTable(4096, 512)
	}
	nodes := make([]Node, 1, nodeCap) // Index 0 is NoNode
	extra := make([]uint32, 0, extraCap)
	return &Arena{
		Nodes:   nodes,
		Extra:   extra,
		Strings: strings,
	}
}

// Add appends a Node to the arena and returns its NodeID.
func (a *Arena) Add(n Node) NodeID {
	id := NodeID(len(a.Nodes))
	a.Nodes = append(a.Nodes, n)
	return id
}

// Get returns a pointer to the Node at id.
func (a *Arena) Get(id NodeID) *Node {
	return &a.Nodes[id]
}

// AddExtra appends a slice of NodeIDs to the auxiliary Extra array and returns
// the starting offset and count for storage in Node.Left and Node.Right.
func (a *Arena) AddExtra(children []NodeID) (start uint32, count uint32) {
	start = uint32(len(a.Extra))
	count = uint32(len(children))
	for _, c := range children {
		a.Extra = append(a.Extra, uint32(c))
	}
	return start, count
}

// ExtraSlice returns the raw uint32 slice of auxiliary children for a start/count pair.
func (a *Arena) ExtraSlice(start, count uint32) []uint32 {
	if count == 0 {
		return nil
	}
	return a.Extra[start : start+count]
}
