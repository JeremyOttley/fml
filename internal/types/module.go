package types

import (
	"fmt"

	"fml/internal/ast"
	"fml/internal/token"
)

// ModuleSig defines a named structural interface required by a Parametric Functor.
type ModuleSig struct {
	Name   token.StringID
	Values map[token.StringID]TypeID
}

// ModuleDef stores the exported value bindings of a concrete module.
type ModuleDef struct {
	Name    token.StringID
	Exports map[token.StringID]TypeScheme
}

// FunctorDef stores a parameterized SML-style module functor `module F (X: Sig) = Body`.
type FunctorDef struct {
	Name      token.StringID
	ParamName token.StringID
	SigName   token.StringID
	BodyNode  ast.NodeID
}

// FunctorResolver manages module signatures, concrete modules, and static
// functor instantiations.
//
// Jaime says: SML functors are just compile-time functions from modules to
// modules. Verify the argument module's exports against the signature and
// specialize the module bindings statically. Zero vtables required.
type FunctorResolver struct {
	checker    *Checker
	Signatures map[token.StringID]ModuleSig
	Modules    map[token.StringID]ModuleDef
	Functors   map[token.StringID]FunctorDef
}

// NewFunctorResolver creates a FunctorResolver bound to checker.
func NewFunctorResolver(checker *Checker) *FunctorResolver {
	return &FunctorResolver{
		checker:    checker,
		Signatures: make(map[token.StringID]ModuleSig),
		Modules:    make(map[token.StringID]ModuleDef),
		Functors:   make(map[token.StringID]FunctorDef),
	}
}

// RegisterSignature registers a module signature requirement (e.g., COMPARABLE).
func (fr *FunctorResolver) RegisterSignature(sig ModuleSig) {
	fr.Signatures[sig.Name] = sig
}

func (c *Checker) inferModuleOrFunctor(id ast.NodeID, n ast.Node) (TypeID, error) {
	modName := token.StringID(n.Data)

	if n.Kind == ast.KindFunctor {
		paramNode := c.AST.Get(ast.NodeID(n.Left))
		paramName := token.StringID(paramNode.Data)
		sigName := token.StringID(paramNode.Left)
		c.Functors.Functors[modName] = FunctorDef{
			Name:      modName,
			ParamName: paramName,
			SigName:   sigName,
			BodyNode:  ast.NodeID(n.Right),
		}
		return c.recordNodeType(id, TypeUnit), nil
	}

	if n.Left == 0 {
		return c.recordNodeType(id, TypeUnit), nil
	}

	bodyNode := c.AST.Get(ast.NodeID(n.Left))
	// Check if this is a Functor instantiation: `module IntSet = MakeSet(IntOrd)`
	if bodyNode.Kind == ast.KindApply {
		fnNode := c.AST.Get(ast.NodeID(bodyNode.Left))
		argNode := c.AST.Get(ast.NodeID(bodyNode.Right))
		functorName := token.StringID(fnNode.Data)
		argModName := token.StringID(argNode.Data)
		if err := c.Functors.InstantiateFunctor(modName, functorName, argModName); err != nil {
			return NoType, err
		}
		return c.recordNodeType(id, TypeUnit), nil
	}

	c.pushScope()
	if _, err := c.inferNode(ast.NodeID(n.Left)); err != nil {
		c.popScope()
		return NoType, err
	}
	exports := make(map[token.StringID]TypeScheme)
	topScope := c.Scopes[len(c.Scopes)-1]
	for k, v := range topScope {
		exports[k] = v.Scheme
		// Also bind `Module.member` in outer scope
		qual := c.AST.Strings.InternString(c.AST.Strings.String(modName) + "." + c.AST.Strings.String(k))
		c.Scopes[0][qual] = v
	}
	c.popScope()

	c.Functors.Modules[modName] = ModuleDef{
		Name:    modName,
		Exports: exports,
	}
	return c.recordNodeType(id, TypeUnit), nil
}

// InstantiateFunctor verifies that argModName satisfies the signature of functorName
// and type-checks the functor body to produce targetModName.
func (fr *FunctorResolver) InstantiateFunctor(targetModName, functorName, argModName token.StringID) error {
	fDef, ok := fr.Functors[functorName]
	if !ok {
		return fmt.Errorf("unknown functor %q", fr.checker.AST.Strings.String(functorName))
	}
	argMod, ok := fr.Modules[argModName]
	if !ok {
		return fmt.Errorf("unknown argument module %q for functor %q",
			fr.checker.AST.Strings.String(argModName),
			fr.checker.AST.Strings.String(functorName))
	}

	// Verify signature requirements if the signature is registered
	if sig, hasSig := fr.Signatures[fDef.SigName]; hasSig {
		for reqSymbol, reqType := range sig.Values {
			expScheme, found := argMod.Exports[reqSymbol]
			if !found {
				return fmt.Errorf("module %q does not implement required member %q of signature %q",
					fr.checker.AST.Strings.String(argModName),
					fr.checker.AST.Strings.String(reqSymbol),
					fr.checker.AST.Strings.String(fDef.SigName))
			}
			instTy := fr.checker.Instantiate(expScheme)
			reqCopy := fr.checker.Instantiate(fr.checker.Generalize(reqType))
			if err := fr.checker.Types.Unify(instTy, reqCopy); err != nil {
				return fmt.Errorf("signature mismatch on %s.%s: %w",
					fr.checker.AST.Strings.String(argModName),
					fr.checker.AST.Strings.String(reqSymbol), err)
			}
		}
	}

	fr.checker.pushScope()
	paramPrefix := fr.checker.AST.Strings.String(fDef.ParamName)
	for sym, scheme := range argMod.Exports {
		qual := fr.checker.AST.Strings.InternString(paramPrefix + "." + fr.checker.AST.Strings.String(sym))
		fr.checker.define(qual, scheme, false)
	}

	if _, err := fr.checker.inferNode(fDef.BodyNode); err != nil {
		fr.checker.popScope()
		return err
	}

	exports := make(map[token.StringID]TypeScheme)
	topScope := fr.checker.Scopes[len(fr.checker.Scopes)-1]
	targetPrefix := fr.checker.AST.Strings.String(targetModName)
	for sym, b := range topScope {
		exports[sym] = b.Scheme
		qual := fr.checker.AST.Strings.InternString(targetPrefix + "." + fr.checker.AST.Strings.String(sym))
		fr.checker.Scopes[0][qual] = b
	}
	fr.checker.popScope()

	fr.Modules[targetModName] = ModuleDef{
		Name:    targetModName,
		Exports: exports,
	}
	return nil
}
