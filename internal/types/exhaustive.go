package types

import (
	"fmt"

	"fml/internal/ast"
	"fml/internal/token"
)

// CheckExhaustive verifies Maranget-style pattern match exhaustiveness and
// redundancy for a KindMatch AST node against the inferred scrutinee type.
//
// Jaime says: If a compiler lets you match on an Option or an ADT without
// handling every case, you've just reinvented a null pointer exception with
// extra steps. Check the constructor matrix at compile time.
func (c *Checker) CheckExhaustive(matchNodeID ast.NodeID, scrutTy TypeID) error {
	matchNode := c.AST.Get(matchNodeID)
	armsBlock := c.AST.Get(ast.NodeID(matchNode.Right))
	arms := c.AST.ExtraSlice(armsBlock.Left, armsBlock.Right)
	if len(arms) == 0 {
		return fmt.Errorf("empty match expression")
	}

	scrutTy = c.Types.Find(scrutTy)
	scrutNode := c.Types.Nodes[scrutTy]

	hasCatchAll := false
	seenTags := make(map[string]bool)
	var activeGroup []token.StringID

	for i, armID := range arms {
		if hasCatchAll {
			return fmt.Errorf("redundant pattern match arm at index %d after unguarded catch-all", i)
		}
		arm := c.AST.Get(ast.NodeID(armID))
		isGuarded := arm.Data != 0
		pat := c.AST.Get(ast.NodeID(arm.Left))

		switch pat.Kind {
		case ast.KindPatWildcard, ast.KindPatVar:
			if !isGuarded {
				hasCatchAll = true
			}
		case ast.KindPatTuple, ast.KindPatRecord:
			if !isGuarded && pSubIsTotal(c.AST, ast.NodeID(arm.Left)) {
				hasCatchAll = true
			}
		case ast.KindPatConst:
			if !isGuarded && pat.Left != 0 {
				lit := c.AST.Get(ast.NodeID(pat.Left))
				if lit.Kind == ast.KindBool {
					if lit.Data == 1 {
						seenTags["true"] = true
					} else {
						seenTags["false"] = true
					}
				}
			}
		case ast.KindPatList:
			if !isGuarded && pat.Right == 0 {
				seenTags["[]"] = true
			}
		case ast.KindPatCons:
			if !isGuarded {
				seenTags["::"] = true
			}
		case ast.KindPatDU:
			tagID := token.StringID(pat.Data)
			tagName := c.AST.Strings.String(tagID)
			if group, ok := c.ActivePats[tagID]; ok {
				activeGroup = group
			}
			if !isGuarded && pSubIsTotal(c.AST, ast.NodeID(pat.Left)) {
				seenTags[tagName] = true
			}
		}
	}

	if hasCatchAll {
		return nil
	}

	// Check complete active pattern coverage if matched via active patterns
	if len(activeGroup) > 0 {
		for _, tagID := range activeGroup {
			tName := c.AST.Strings.String(tagID)
			if !seenTags[tName] {
				return fmt.Errorf("non-exhaustive active pattern match: missing case %s", tName)
			}
		}
		return nil
	}

	switch scrutNode.Kind {
	case TyBool:
		if !seenTags["true"] || !seenTags["false"] {
			return fmt.Errorf("non-exhaustive boolean match: must cover both true and false")
		}
		return nil
	case TyOption:
		if !seenTags["Some"] || !seenTags["None"] {
			return fmt.Errorf("non-exhaustive option match: missing Some or None case")
		}
		return nil
	case TyResult:
		if !seenTags["Ok"] || !seenTags["Error"] {
			return fmt.Errorf("non-exhaustive Result match: missing Ok or Error case")
		}
		return nil
	case TyList:
		if !seenTags["[]"] || !seenTags["::"] {
			return fmt.Errorf("non-exhaustive list match: missing [] or h :: t case")
		}
		return nil
	case TyADT:
		var missing []string
		for ctorID, info := range c.Constructors {
			if c.Types.Find(info.ADTType) == scrutTy {
				name := c.AST.Strings.String(ctorID)
				if !seenTags[name] {
					missing = append(missing, name)
				}
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("non-exhaustive ADT match on %s: missing constructor(s) %v",
				c.AST.Strings.String(scrutNode.Name), missing)
		}
		return nil
	default:
		return fmt.Errorf("non-exhaustive match on %s: missing wildcard `_` or variable catch-all pattern",
			c.Types.Format(scrutTy))
	}
}

func pSubIsTotal(arena *ast.Arena, subPatID ast.NodeID) bool {
	if subPatID == ast.NoNode {
		return true
	}
	n := arena.Get(subPatID)
	switch n.Kind {
	case ast.KindPatWildcard, ast.KindPatVar:
		return true
	case ast.KindPatTuple:
		for _, ch := range arena.ExtraSlice(n.Left, n.Right) {
			if !pSubIsTotal(arena, ast.NodeID(ch)) {
				return false
			}
		}
		return true
	case ast.KindPatRecord:
		for _, ch := range arena.ExtraSlice(n.Left, n.Right) {
			fn := arena.Get(ast.NodeID(ch))
			if !pSubIsTotal(arena, ast.NodeID(fn.Left)) {
				return false
			}
		}
		return true
	default:
		return false
	}
}
