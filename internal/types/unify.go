package types

import "fmt"

// Unify unifies two types in the TypeArena using an explicit iterative worklist.
//
// Jaime says: Unification is graph reachability. Push pairs onto a flat worklist
// slice, pop them in a loop, and check for cycles iteratively. No call-stack
// depth bombs allowed.
func (ta *TypeArena) Unify(a, b TypeID) error {
	ta.workPairs = ta.workPairs[:0]
	ta.workPairs = append(ta.workPairs, [2]TypeID{a, b})

	for len(ta.workPairs) > 0 {
		pair := ta.workPairs[len(ta.workPairs)-1]
		ta.workPairs = ta.workPairs[:len(ta.workPairs)-1]

		ra := ta.Find(pair[0])
		rb := ta.Find(pair[1])
		if ra == rb {
			continue
		}

		na := ta.Nodes[ra]
		nb := ta.Nodes[rb]

		if na.Kind == TyVar {
			if ta.OccursIn(ra, rb) {
				return fmt.Errorf("occurs check failed: infinite type %s in %s", ta.Format(ra), ta.Format(rb))
			}
			if err := ta.unifyRowVar(ra, rb); err != nil {
				return err
			}
			ta.Union(ra, rb)
			continue
		}
		if nb.Kind == TyVar {
			if ta.OccursIn(rb, ra) {
				return fmt.Errorf("occurs check failed: infinite type %s in %s", ta.Format(rb), ta.Format(ra))
			}
			if err := ta.unifyRowVar(rb, ra); err != nil {
				return err
			}
			ta.Union(rb, ra)
			continue
		}

		if na.Kind != nb.Kind {
			return fmt.Errorf("type mismatch: cannot unify %s with %s", ta.Format(ra), ta.Format(rb))
		}

		switch na.Kind {
		case TyUnit, TyBool, TyInt, TyFloat, TyString:
			// Identical primitive kinds unify trivially
		case TyArrow, TyResult:
			ta.workPairs = append(ta.workPairs,
				[2]TypeID{TypeID(na.Right), TypeID(nb.Right)},
				[2]TypeID{TypeID(na.Left), TypeID(nb.Left)},
			)
		case TyList, TyArray, TySeq, TyOption:
			ta.workPairs = append(ta.workPairs, [2]TypeID{TypeID(na.Left), TypeID(nb.Left)})
		case TyTuple:
			if na.Right != nb.Right {
				return fmt.Errorf("tuple arity mismatch: %d vs %d", na.Right, nb.Right)
			}
			for i := uint32(0); i < na.Right; i++ {
				ta.workPairs = append(ta.workPairs, [2]TypeID{
					ta.Extra[na.Left+i],
					ta.Extra[nb.Left+i],
				})
			}
		case TyRecord:
			if na.Name != 0 && nb.Name != 0 {
				if na.Name != nb.Name || na.Right != nb.Right {
					return fmt.Errorf("nominal type mismatch: %s vs %s", ta.Format(ra), ta.Format(rb))
				}
				for i := uint32(0); i < na.Right; i++ {
					ta.workPairs = append(ta.workPairs, [2]TypeID{
						ta.Extra[na.Left+i],
						ta.Extra[nb.Left+i],
					})
				}
			} else if err := ta.unifyRowRecords(ra, rb); err != nil {
				return err
			}
		case TyADT:
			if na.Name != nb.Name || na.Right != nb.Right {
				return fmt.Errorf("nominal type mismatch: %s vs %s", ta.Format(ra), ta.Format(rb))
			}
			for i := uint32(0); i < na.Right; i++ {
				ta.workPairs = append(ta.workPairs, [2]TypeID{
					ta.Extra[na.Left+i],
					ta.Extra[nb.Left+i],
				})
			}
		case TyMeasure:
			if err := ta.unifyMeasures(na.Right, nb.Right); err != nil {
				return err
			}
			ta.workPairs = append(ta.workPairs, [2]TypeID{TypeID(na.Left), TypeID(nb.Left)})
		case TyMeasureUnit:
			if err := ta.unifyMeasures(na.Right, nb.Right); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported unification kind: %d", na.Kind)
		}
	}
	return nil
}

func (ta *TypeArena) unifyRowVar(varID, targetID TypeID) error {
	rowA := ta.RowFields[varID]
	if len(rowA) == 0 {
		return nil
	}
	nt := ta.Nodes[targetID]
	if nt.Kind == TyVar {
		for fName, fTyA := range rowA {
			if fTyB, ok := ta.RowFields[targetID][fName]; ok {
				ta.workPairs = append(ta.workPairs, [2]TypeID{fTyA, fTyB})
			} else {
				ta.SetRowField(targetID, fName, fTyA)
			}
		}
		return nil
	}
	if nt.Kind != TyRecord {
		return fmt.Errorf("type mismatch: expected record with fields, got %s", ta.Format(targetID))
	}
	rowB := ta.RowFields[targetID]
	for fName, fTyA := range rowA {
		fTyB, ok := rowB[fName]
		if !ok {
			return fmt.Errorf("record %s is missing required field '%s'", ta.Format(targetID), ta.Strings.String(fName))
		}
		ta.workPairs = append(ta.workPairs, [2]TypeID{fTyA, fTyB})
	}
	return nil
}

func (ta *TypeArena) unifyRowRecords(ra, rb TypeID) error {
	rowA := ta.RowFields[ra]
	rowB := ta.RowFields[rb]
	for fName, fTyA := range rowA {
		fTyB, ok := rowB[fName]
		if !ok {
			return fmt.Errorf("structural record mismatch: missing field '%s'", ta.Strings.String(fName))
		}
		ta.workPairs = append(ta.workPairs, [2]TypeID{fTyA, fTyB})
	}
	for fName := range rowB {
		if _, ok := rowA[fName]; !ok {
			return fmt.Errorf("structural record mismatch: unexpected field '%s'", ta.Strings.String(fName))
		}
	}
	return nil
}

// OccursIn checks whether type variable varID appears anywhere inside targetID
// using an explicit iterative stack.
func (ta *TypeArena) OccursIn(varID, targetID TypeID) bool {
	varID = ta.Find(varID)
	ta.workStack = ta.workStack[:0]
	ta.workStack = append(ta.workStack, targetID)

	for len(ta.workStack) > 0 {
		curr := ta.Find(ta.workStack[len(ta.workStack)-1])
		ta.workStack = ta.workStack[:len(ta.workStack)-1]

		if curr == varID {
			return true
		}
		n := ta.Nodes[curr]
		switch n.Kind {
		case TyArrow, TyResult:
			ta.workStack = append(ta.workStack, TypeID(n.Left), TypeID(n.Right))
		case TyList, TyArray, TySeq, TyOption, TyMeasure:
			ta.workStack = append(ta.workStack, TypeID(n.Left))
		case TyTuple, TyRecord, TyADT:
			for i := uint32(0); i < n.Right; i++ {
				ta.workStack = append(ta.workStack, ta.Extra[n.Left+i])
			}
			if n.Kind == TyRecord {
				for _, fTy := range ta.RowFields[curr] {
					ta.workStack = append(ta.workStack, fTy)
				}
			}
		}
	}
	return false
}

// Format returns a human-readable string representation of TypeID id.
func (ta *TypeArena) Format(id TypeID) string {
	id = ta.Find(id)
	if id == NoType || int(id) >= len(ta.Nodes) {
		return "<invalid>"
	}
	n := ta.Nodes[id]
	switch n.Kind {
	case TyVar:
		return fmt.Sprintf("'t%d", id)
	case TyUnit:
		return "unit"
	case TyBool:
		return "bool"
	case TyInt:
		return "int"
	case TyFloat:
		return "float"
	case TyString:
		return "string"
	case TyArrow:
		return fmt.Sprintf("(%s -> %s)", ta.Format(TypeID(n.Left)), ta.Format(TypeID(n.Right)))
	case TyList:
		return fmt.Sprintf("%s list", ta.Format(TypeID(n.Left)))
	case TyArray:
		return fmt.Sprintf("%s array", ta.Format(TypeID(n.Left)))
	case TySeq:
		return fmt.Sprintf("%s seq", ta.Format(TypeID(n.Left)))
	case TyOption:
		return fmt.Sprintf("%s option", ta.Format(TypeID(n.Left)))
	case TyResult:
		return fmt.Sprintf("Result<%s, %s>", ta.Format(TypeID(n.Left)), ta.Format(TypeID(n.Right)))
	case TyTuple:
		out := "("
		for i := uint32(0); i < n.Right; i++ {
			if i > 0 {
				out += " * "
			}
			out += ta.Format(ta.Extra[n.Left+i])
		}
		return out + ")"
	case TyRecord:
		if n.Name != 0 {
			return ta.Strings.String(n.Name)
		}
		return "{ .. }"
	case TyADT:
		return ta.Strings.String(n.Name)
	case TyMeasure:
		return fmt.Sprintf("%s<%s>", ta.Format(TypeID(n.Left)), ta.FormatMeasure(n.Right))
	default:
		return fmt.Sprintf("type#%d", id)
	}
}
