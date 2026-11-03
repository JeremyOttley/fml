package types

import (
	"fmt"
	"sort"

	"fml/internal/ast"
	"fml/internal/token"
)

// AddMeasure registers a normalized MeasureExpr in TypeArena.Measures and returns its index.
//
// Jaime says: Units of measure are compile-time dimensional analysis. Normalize
// the base unit exponents into a sorted array of (StringID, int8) pairs, compare
// them in O(k), and erase them before code generation so the CPU only sees raw
// floats and ints.
func (ta *TypeArena) AddMeasure(terms []MeasureTerm) uint32 {
	norm := normalizeMeasureTerms(terms)
	if len(norm) == 0 {
		return 0 // Index 0 is dimensionless "1"
	}
	for i := 1; i < len(ta.Measures); i++ {
		if measureTermsEqual(ta.Measures[i].Terms, norm) && ta.Measures[i].VarID == NoType {
			return uint32(i)
		}
	}
	id := uint32(len(ta.Measures))
	ta.Measures = append(ta.Measures, MeasureExpr{Terms: norm})
	return id
}

// MakeMeasureType wraps a numeric TypeID (TyFloat or TyInt) with a MeasureID.
// If measureID == 0 (dimensionless), it returns baseType directly.
func (ta *TypeArena) MakeMeasureType(baseType TypeID, measureID uint32) TypeID {
	baseType = ta.Find(baseType)
	if measureID == 0 {
		return baseType
	}
	return ta.Add(TypeNode{
		Kind:  TyMeasure,
		Left:  uint32(baseType),
		Right: measureID,
	})
}

// MultiplyMeasures computes m1 * m2 (or m1 / m2 when invertRight is true).
func (ta *TypeArena) MultiplyMeasures(m1, m2 uint32, invertRight bool) uint32 {
	var combined []MeasureTerm
	if int(m1) < len(ta.Measures) {
		combined = append(combined, ta.Measures[m1].Terms...)
	}
	if int(m2) < len(ta.Measures) {
		for _, t := range ta.Measures[m2].Terms {
			if invertRight {
				combined = append(combined, MeasureTerm{Unit: t.Unit, Exp: -t.Exp})
			} else {
				combined = append(combined, t)
			}
		}
	}
	return ta.AddMeasure(combined)
}

func (ta *TypeArena) unifyMeasures(m1, m2 uint32) error {
	if m1 == m2 {
		return nil
	}
	var t1, t2 []MeasureTerm
	if int(m1) < len(ta.Measures) {
		t1 = ta.Measures[m1].Terms
	}
	if int(m2) < len(ta.Measures) {
		t2 = ta.Measures[m2].Terms
	}
	if !measureTermsEqual(t1, t2) {
		return fmt.Errorf("unit-of-measure mismatch: <%s> vs <%s>", ta.FormatMeasure(m1), ta.FormatMeasure(m2))
	}
	return nil
}

// BuildMeasureFromAST lowers an ast.KindMeasureUnit subtree into a normalized MeasureID.
func (ta *TypeArena) BuildMeasureFromAST(arena *ast.Arena, nodeID ast.NodeID) uint32 {
	if nodeID == ast.NoNode {
		return 0
	}
	terms := extractASTMeasureTerms(arena, nodeID, 1)
	return ta.AddMeasure(terms)
}

func extractASTMeasureTerms(arena *ast.Arena, nodeID ast.NodeID, scale int8) []MeasureTerm {
	if nodeID == ast.NoNode {
		return nil
	}
	n := arena.Get(nodeID)
	if n.Kind != ast.KindMeasureUnit {
		return nil
	}
	if n.Op == 0 {
		unitID := token.StringID(n.Data)
		if unitID == token.NoString || arena.Strings.String(unitID) == "1" {
			return nil
		}
		return []MeasureTerm{{Unit: unitID, Exp: scale}}
	}
	switch token.TokenKind(n.Op) {
	case token.TokenStar:
		lhs := extractASTMeasureTerms(arena, ast.NodeID(n.Left), scale)
		rhs := extractASTMeasureTerms(arena, ast.NodeID(n.Right), scale)
		return append(lhs, rhs...)
	case token.TokenSlash:
		lhs := extractASTMeasureTerms(arena, ast.NodeID(n.Left), scale)
		rhs := extractASTMeasureTerms(arena, ast.NodeID(n.Right), -scale)
		return append(lhs, rhs...)
	case token.TokenCaret:
		expNode := arena.Get(ast.NodeID(n.Right))
		exp := int8(1)
		if expNode.Kind == ast.KindInt {
			exp = int8(expNode.Data)
		}
		return extractASTMeasureTerms(arena, ast.NodeID(n.Left), scale*exp)
	default:
		return nil
	}
}

// EraseMeasures strips TyMeasure wrappers across all TypeNodes in the arena
// so downstream IR lowering sees zero-cost primitive numeric types.
func (ta *TypeArena) EraseMeasures() {
	for i := 1; i < len(ta.Nodes); i++ {
		if ta.Nodes[i].Kind == TyMeasure {
			base := ta.Find(TypeID(ta.Nodes[i].Left))
			ta.Nodes[i].Kind = TyVar
			ta.Nodes[i].Flags |= TypeFlagErased
			ta.Nodes[i].Parent = base
		}
	}
}

// FormatMeasure formats a MeasureID as a canonical string (e.g., "kg*m/s^2").
func (ta *TypeArena) FormatMeasure(mID uint32) string {
	if mID == 0 || int(mID) >= len(ta.Measures) || len(ta.Measures[mID].Terms) == 0 {
		return "1"
	}
	out := ""
	for i, t := range ta.Measures[mID].Terms {
		if i > 0 {
			out += "*"
		}
		name := ta.Strings.String(t.Unit)
		if t.Exp == 1 {
			out += name
		} else {
			out += fmt.Sprintf("%s^%d", name, t.Exp)
		}
	}
	return out
}

func normalizeMeasureTerms(terms []MeasureTerm) []MeasureTerm {
	if len(terms) == 0 {
		return nil
	}
	merged := make([]MeasureTerm, len(terms))
	copy(merged, terms)
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].Unit < merged[j].Unit
	})
	out := merged[:0]
	for _, t := range merged {
		if t.Unit == token.NoString || t.Exp == 0 {
			continue
		}
		if len(out) > 0 && out[len(out)-1].Unit == t.Unit {
			out[len(out)-1].Exp += t.Exp
			if out[len(out)-1].Exp == 0 {
				out = out[:len(out)-1]
			}
		} else {
			out = append(out, t)
		}
	}
	return out
}

func measureTermsEqual(a, b []MeasureTerm) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Unit != b[i].Unit || a[i].Exp != b[i].Exp {
			return false
		}
	}
	return true
}
