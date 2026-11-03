// Package types implements the Hindley-Milner type inference engine (Algorithm W),
// Union-Find disjoint-set type arena, Units of Measure abelian group solver,
// pattern exhaustiveness checker, and parametric functor verifier for FML.
//
// Jaime says: Type inference isn't magic; it's just graph unification.
// Represent type variables as a disjoint-set forest in a flat slice of structs.
// When you unify `a` and `Int`, you're just pointing index `a` to index `Int`.
// If you find yourself writing a recursive pointer-chasing nightmare, delete it
// and start over.
package types

import "fml/internal/token"

// TypeID is an index into TypeArena.Nodes.
type TypeID uint32

// Canonical built-in TypeIDs pre-allocated in every TypeArena.
const (
	NoType TypeID = iota
	TypeUnit
	TypeBool
	TypeInt
	TypeFloat
	TypeString
)

// TypeKind identifies the constructor of a TypeNode.
type TypeKind uint8

const (
	TyInvalid TypeKind = iota
	TyVar              // Unbound or linked type variable in Union-Find forest
	TyUnit             // unit
	TyBool             // bool
	TyInt              // int
	TyFloat            // float
	TyString           // string
	TyArrow            // Left -> Right
	TyTuple            // Extra[Left : Left+Right]
	TyList             // Left = element TypeID
	TyArray            // Left = element TypeID
	TySeq              // Left = element TypeID
	TyOption           // Left = element TypeID
	TyResult           // Left = Ok TypeID, Right = Error TypeID
	TyRecord           // Name = record name, Extra[Left : Left+Right] = field TypeIDs
	TyADT              // Name = DU type name, Extra[Left : Left+Right] = type arg TypeIDs
	TyMeasure          // Left = underlying numeric TypeID (TyFloat/TyInt), Right = MeasureID
	TyMeasureUnit      // Right = MeasureID (pure unit of measure in TypeArena.Measures)
)

// TypeFlags stores metadata on TypeNode.Flags.
const (
	TypeFlagRigid uint8 = 1 << iota
	TypeFlagErased
)

// TypeNode is a 20-byte flat struct representing a node in the Union-Find type forest.
//
// Jaime says: 20 bytes per type node. Over three nodes fit in a single 64-byte
// cache line. Parent is a 32-bit index, not a pointer.
type TypeNode struct {
	Kind   TypeKind
	Flags  uint8
	Rank   uint16
	Parent TypeID         // For TyVar: self if root, or unified target TypeID
	Left   uint32         // First child TypeID or start offset in Extra
	Right  uint32         // Second child TypeID, MeasureID, or count in Extra
	Name   token.StringID // Symbol name for Record/ADT/TypeVar
}

// MeasureTerm represents a single base unit and its non-zero integer exponent
// (e.g., kg^1, m^1, s^-2) inside an abelian unit group expression.
type MeasureTerm struct {
	Unit token.StringID
	Exp  int8
}

// MeasureExpr represents a normalized product of base unit powers.
// If VarID != NoType, it represents an ununified measure variable.
type MeasureExpr struct {
	Terms []MeasureTerm
	VarID TypeID
}

// TypeArena manages the contiguous slice of TypeNodes, auxiliary child lists,
// and reusable worklists for zero-allocation unification.
type TypeArena struct {
	Nodes     []TypeNode
	Extra     []TypeID
	Measures  []MeasureExpr
	Strings   *token.StringTable
	RowFields map[TypeID]map[token.StringID]TypeID
	workPairs [][2]TypeID
	workStack []TypeID
}

// NewTypeArena initializes a flat TypeArena with canonical primitive types at indices 1..5.
func NewTypeArena(nodeCap int, strings *token.StringTable) *TypeArena {
	if nodeCap < 256 {
		nodeCap = 256
	}
	if strings == nil {
		strings = token.NewStringTable(4096, 512)
	}
	ta := &TypeArena{
		Nodes:     make([]TypeNode, 6, nodeCap),
		Extra:     make([]TypeID, 0, nodeCap),
		Measures:  make([]MeasureExpr, 1, 64), // Index 0 is dimensionless (1)
		Strings:   strings,
		RowFields: make(map[TypeID]map[token.StringID]TypeID),
		workPairs: make([][2]TypeID, 0, 64),
		workStack: make([]TypeID, 0, 64),
	}
	ta.Nodes[TypeUnit] = TypeNode{Kind: TyUnit, Parent: TypeUnit}
	ta.Nodes[TypeBool] = TypeNode{Kind: TyBool, Parent: TypeBool}
	ta.Nodes[TypeInt] = TypeNode{Kind: TyInt, Parent: TypeInt}
	ta.Nodes[TypeFloat] = TypeNode{Kind: TyFloat, Parent: TypeFloat}
	ta.Nodes[TypeString] = TypeNode{Kind: TyString, Parent: TypeString}
	return ta
}

// SetRowField records a structural field constraint on TypeID id.
func (ta *TypeArena) SetRowField(id TypeID, field token.StringID, fieldTy TypeID) {
	m := ta.RowFields[id]
	if m == nil {
		m = make(map[token.StringID]TypeID, 4)
		ta.RowFields[id] = m
	}
	m[field] = fieldTy
}

// FreshVar allocates a new unbound type variable in the Union-Find forest.
func (ta *TypeArena) FreshVar() TypeID {
	id := TypeID(len(ta.Nodes))
	ta.Nodes = append(ta.Nodes, TypeNode{
		Kind:   TyVar,
		Parent: id,
	})
	return id
}

// Add appends a composite TypeNode to the arena and sets its initial Parent to self.
func (ta *TypeArena) Add(n TypeNode) TypeID {
	id := TypeID(len(ta.Nodes))
	n.Parent = id
	ta.Nodes = append(ta.Nodes, n)
	return id
}

// AddExtra appends a list of child TypeIDs to the flat Extra slice.
func (ta *TypeArena) AddExtra(children []TypeID) (start uint32, count uint32) {
	start = uint32(len(ta.Extra))
	count = uint32(len(children))
	ta.Extra = append(ta.Extra, children...)
	return start, count
}

// ExtraSlice returns the slice of child TypeIDs for start and count.
func (ta *TypeArena) ExtraSlice(start, count uint32) []TypeID {
	if count == 0 {
		return nil
	}
	return ta.Extra[start : start+count]
}

// Find follows Parent links iteratively with path compression to return the
// canonical representative TypeID for id.
//
// Jaime says: Two passes over integer indices. Zero recursion, zero stack frames,
// zero heap allocations.
func (ta *TypeArena) Find(id TypeID) TypeID {
	if id == NoType || int(id) >= len(ta.Nodes) {
		return NoType
	}
	root := id
	for ta.Nodes[root].Kind == TyVar && ta.Nodes[root].Parent != root {
		root = ta.Nodes[root].Parent
	}
	// Iterative path compression
	curr := id
	for curr != root && ta.Nodes[curr].Kind == TyVar {
		next := ta.Nodes[curr].Parent
		ta.Nodes[curr].Parent = root
		curr = next
	}
	return root
}

// Union links two TypeIDs in the Union-Find forest and returns the merged representative.
func (ta *TypeArena) Union(a, b TypeID) TypeID {
	ra := ta.Find(a)
	rb := ta.Find(b)
	if ra == rb {
		return ra
	}
	// Always point a TyVar at a concrete type when one side is concrete
	if ta.Nodes[ra].Kind == TyVar && ta.Nodes[rb].Kind != TyVar {
		ta.Nodes[ra].Parent = rb
		return rb
	}
	if ta.Nodes[rb].Kind == TyVar && ta.Nodes[ra].Kind != TyVar {
		ta.Nodes[rb].Parent = ra
		return ra
	}
	// Both are TyVars: union by rank
	if ta.Nodes[ra].Rank < ta.Nodes[rb].Rank {
		ta.Nodes[ra].Parent = rb
		return rb
	}
	ta.Nodes[rb].Parent = ra
	if ta.Nodes[ra].Rank == ta.Nodes[rb].Rank {
		ta.Nodes[ra].Rank++
	}
	return ra
}
