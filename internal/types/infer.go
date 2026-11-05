package types

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"fml/internal/ast"
	"fml/internal/token"
)

// TypeScheme represents a universally quantified polymorphic type `forall a1..an. Body`.
type TypeScheme struct {
	Vars []TypeID
	Body TypeID
}

// Binding stores a symbol's TypeScheme and mutability flag in the typing environment.
type Binding struct {
	Scheme  TypeScheme
	Mutable bool
}

// ConstructorInfo stores the parent ADT type and optional payload TypeID for a DU case.
type ConstructorInfo struct {
	ADTType TypeID
	Payload TypeID // NoType for nullary constructors
}

// RecordInfo stores the canonical field list for a declared record type.
type RecordInfo struct {
	RecordType TypeID
	FieldNames []token.StringID
	FieldTypes []TypeID
}

// Checker holds the typing environment, AST-to-TypeID table, and Functor/Exhaustiveness state.
//
// Jaime says: Keep the type checker state flat. NodeTypes is a direct slice
// indexed by ast.NodeID. Looking up the type of any AST node is a single slice
// index `NodeTypes[nodeID]`.
type Checker struct {
	AST             *ast.Arena
	Types           *TypeArena
	NodeTypes       []TypeID
	Scopes          []map[token.StringID]Binding
	Constructors    map[token.StringID]ConstructorInfo
	Records         map[token.StringID]RecordInfo
	FieldToRec      map[token.StringID]token.StringID
	NamedTypes      map[token.StringID]TypeID
	ActivePats        map[token.StringID][]token.StringID
	ActivePatInputs   map[token.StringID]TypeScheme
	ActivePatPayloads map[token.StringID]TypeScheme
	Functors          *FunctorResolver
	BaseDir         string
	ActiveBuilder   token.StringID
	ProviderSchemas map[token.StringID]string
	Source          []byte
	Tokens          []token.Token
}

// NewChecker initializes a Hindley-Milner type checker for astArena.
func NewChecker(astArena *ast.Arena, typeArena *TypeArena) *Checker {
	if typeArena == nil {
		typeArena = NewTypeArena(len(astArena.Nodes)*2+64, astArena.Strings)
	}
	c := &Checker{
		AST:             astArena,
		Types:           typeArena,
		NodeTypes:       make([]TypeID, len(astArena.Nodes)+16),
		Scopes:          []map[token.StringID]Binding{make(map[token.StringID]Binding)},
		Constructors:    make(map[token.StringID]ConstructorInfo),
		Records:         make(map[token.StringID]RecordInfo),
		FieldToRec:      make(map[token.StringID]token.StringID),
		NamedTypes:      make(map[token.StringID]TypeID),
		ActivePats:        make(map[token.StringID][]token.StringID),
		ActivePatInputs:   make(map[token.StringID]TypeScheme),
		ActivePatPayloads: make(map[token.StringID]TypeScheme),
		ProviderSchemas:   make(map[token.StringID]string),
	}
	c.Functors = NewFunctorResolver(c)
	c.registerBuiltins()
	return c
}

// Infer runs Algorithm W over root and returns its inferred TypeID.
func (c *Checker) Infer(root ast.NodeID) (TypeID, error) {
	if int(root) >= len(c.NodeTypes) {
		grown := make([]TypeID, len(c.AST.Nodes)+16)
		copy(grown, c.NodeTypes)
		c.NodeTypes = grown
	}
	return c.inferNode(root)
}

func (c *Checker) registerBuiltins() {
	s := c.AST.Strings
	c.NamedTypes[s.InternString("unit")] = TypeUnit
	c.NamedTypes[s.InternString("bool")] = TypeBool
	c.NamedTypes[s.InternString("int")] = TypeInt
	c.NamedTypes[s.InternString("float")] = TypeFloat
	c.NamedTypes[s.InternString("string")] = TypeString

	// Built-in polymorphic print / printfn / ignore / fst / snd / box / id
	a := c.Types.FreshVar()
	b := c.Types.FreshVar()
	printTy := c.Types.Add(TypeNode{Kind: TyArrow, Left: uint32(a), Right: uint32(TypeUnit)})
	c.define(s.InternString("print"), TypeScheme{Vars: []TypeID{a}, Body: printTy}, false)
	c.define(s.InternString("printfn"), TypeScheme{Vars: []TypeID{a}, Body: printTy}, false)
	c.define(s.InternString("ignore"), TypeScheme{Vars: []TypeID{a}, Body: printTy}, false)

	idTy := c.Types.Add(TypeNode{Kind: TyArrow, Left: uint32(a), Right: uint32(a)})
	c.define(s.InternString("id"), TypeScheme{Vars: []TypeID{a}, Body: idTy}, false)

	start, count := c.Types.AddExtra([]TypeID{a, b})
	pairTy := c.Types.Add(TypeNode{Kind: TyTuple, Left: start, Right: count})
	fstTy := c.Types.Add(TypeNode{Kind: TyArrow, Left: uint32(pairTy), Right: uint32(a)})
	sndTy := c.Types.Add(TypeNode{Kind: TyArrow, Left: uint32(pairTy), Right: uint32(b)})
	c.define(s.InternString("fst"), TypeScheme{Vars: []TypeID{a, b}, Body: fstTy}, false)
	c.define(s.InternString("snd"), TypeScheme{Vars: []TypeID{a, b}, Body: sndTy}, false)

	// Built-in Expr ADT for first-class AST quotations <@ ... @>
	exprName := s.InternString("Expr")
	exprTy := c.Types.Add(TypeNode{Kind: TyADT, Name: exprName})
	c.NamedTypes[exprName] = exprTy
	c.Constructors[s.InternString("QUnit")] = ConstructorInfo{ADTType: exprTy, Payload: NoType}
	c.Constructors[s.InternString("QInt")] = ConstructorInfo{ADTType: exprTy, Payload: TypeInt}
	c.Constructors[s.InternString("QFloat")] = ConstructorInfo{ADTType: exprTy, Payload: TypeFloat}
	c.Constructors[s.InternString("QBool")] = ConstructorInfo{ADTType: exprTy, Payload: TypeBool}
	c.Constructors[s.InternString("QString")] = ConstructorInfo{ADTType: exprTy, Payload: TypeString}
	c.Constructors[s.InternString("QVar")] = ConstructorInfo{ADTType: exprTy, Payload: TypeString}

	infixStart, infixCount := c.Types.AddExtra([]TypeID{TypeString, exprTy, exprTy})
	infixPayload := c.Types.Add(TypeNode{Kind: TyTuple, Left: infixStart, Right: infixCount})
	c.Constructors[s.InternString("QInfix")] = ConstructorInfo{ADTType: exprTy, Payload: infixPayload}

	appStart, appCount := c.Types.AddExtra([]TypeID{exprTy, exprTy})
	appPayload := c.Types.Add(TypeNode{Kind: TyTuple, Left: appStart, Right: appCount})
	c.Constructors[s.InternString("QApp")] = ConstructorInfo{ADTType: exprTy, Payload: appPayload}

	lamStart, lamCount := c.Types.AddExtra([]TypeID{TypeString, exprTy})
	lamPayload := c.Types.Add(TypeNode{Kind: TyTuple, Left: lamStart, Right: lamCount})
	c.Constructors[s.InternString("QLambda")] = ConstructorInfo{ADTType: exprTy, Payload: lamPayload}

	letStart, letCount := c.Types.AddExtra([]TypeID{TypeString, exprTy, exprTy})
	letPayload := c.Types.Add(TypeNode{Kind: TyTuple, Left: letStart, Right: letCount})
	c.Constructors[s.InternString("QLet")] = ConstructorInfo{ADTType: exprTy, Payload: letPayload}

	c.registerCombinatorTypes()
}

func (c *Checker) registerCombinatorTypes() {
	s := c.AST.Strings
	arrow := func(l, r TypeID) TypeID {
		return c.Types.Add(TypeNode{Kind: TyArrow, Left: uint32(l), Right: uint32(r)})
	}
	listOf := func(elem TypeID) TypeID {
		return c.Types.Add(TypeNode{Kind: TyList, Left: uint32(elem)})
	}
	arrOf := func(elem TypeID) TypeID {
		return c.Types.Add(TypeNode{Kind: TyArray, Left: uint32(elem)})
	}
	optOf := func(elem TypeID) TypeID {
		return c.Types.Add(TypeNode{Kind: TyOption, Left: uint32(elem)})
	}
	resOf := func(ok, err TypeID) TypeID {
		return c.Types.Add(TypeNode{Kind: TyResult, Left: uint32(ok), Right: uint32(err)})
	}
	setName := s.InternString("Set")
	setOf := func(elem TypeID) TypeID {
		start, count := c.Types.AddExtra([]TypeID{elem})
		return c.Types.Add(TypeNode{Kind: TyADT, Name: setName, Left: start, Right: count})
	}
	mapName := s.InternString("Map")
	mapOf := func(k, v TypeID) TypeID {
		start, count := c.Types.AddExtra([]TypeID{k, v})
		return c.Types.Add(TypeNode{Kind: TyADT, Name: mapName, Left: start, Right: count})
	}

	a := c.Types.FreshVar()
	b := c.Types.FreshVar()
	e := c.Types.FreshVar()

	// List combinators
	zipStart, zipCount := c.Types.AddExtra([]TypeID{a, b})
	zipPairTy := c.Types.Add(TypeNode{Kind: TyTuple, Left: zipStart, Right: zipCount})
	partStart, partCount := c.Types.AddExtra([]TypeID{listOf(a), listOf(a)})
	partPairTy := c.Types.Add(TypeNode{Kind: TyTuple, Left: partStart, Right: partCount})
	c.define(s.InternString("List.empty"), TypeScheme{Vars: []TypeID{a}, Body: listOf(a)}, false)
	c.define(s.InternString("List.isEmpty"), TypeScheme{Vars: []TypeID{a}, Body: arrow(listOf(a), TypeBool)}, false)
	c.define(s.InternString("List.length"), TypeScheme{Vars: []TypeID{a}, Body: arrow(listOf(a), TypeInt)}, false)
	c.define(s.InternString("List.head"), TypeScheme{Vars: []TypeID{a}, Body: arrow(listOf(a), a)}, false)
	c.define(s.InternString("List.tail"), TypeScheme{Vars: []TypeID{a}, Body: arrow(listOf(a), listOf(a))}, false)
	c.define(s.InternString("List.rev"), TypeScheme{Vars: []TypeID{a}, Body: arrow(listOf(a), listOf(a))}, false)
	c.define(s.InternString("List.map"), TypeScheme{Vars: []TypeID{a, b}, Body: arrow(arrow(a, b), arrow(listOf(a), listOf(b)))}, false)
	c.define(s.InternString("List.filter"), TypeScheme{Vars: []TypeID{a}, Body: arrow(arrow(a, TypeBool), arrow(listOf(a), listOf(a)))}, false)
	c.define(s.InternString("List.fold"), TypeScheme{Vars: []TypeID{a, b}, Body: arrow(arrow(a, arrow(b, a)), arrow(a, arrow(listOf(b), a)))}, false)
	c.define(s.InternString("List.sortBy"), TypeScheme{Vars: []TypeID{a, b}, Body: arrow(arrow(a, b), arrow(listOf(a), listOf(a)))}, false)
	c.define(s.InternString("List.sum"), TypeScheme{Vars: []TypeID{a}, Body: arrow(listOf(a), a)}, false)
	c.define(s.InternString("List.tryFind"), TypeScheme{Vars: []TypeID{a}, Body: arrow(arrow(a, TypeBool), arrow(listOf(a), optOf(a)))}, false)
	c.define(s.InternString("List.exists"), TypeScheme{Vars: []TypeID{a}, Body: arrow(arrow(a, TypeBool), arrow(listOf(a), TypeBool))}, false)
	c.define(s.InternString("List.forall"), TypeScheme{Vars: []TypeID{a}, Body: arrow(arrow(a, TypeBool), arrow(listOf(a), TypeBool))}, false)
	c.define(s.InternString("List.zip"), TypeScheme{Vars: []TypeID{a, b}, Body: arrow(listOf(a), arrow(listOf(b), listOf(zipPairTy)))}, false)
	c.define(s.InternString("List.partition"), TypeScheme{Vars: []TypeID{a}, Body: arrow(arrow(a, TypeBool), arrow(listOf(a), partPairTy))}, false)
	c.define(s.InternString("List.choose"), TypeScheme{Vars: []TypeID{a, b}, Body: arrow(arrow(a, optOf(b)), arrow(listOf(a), listOf(b)))}, false)
	c.define(s.InternString("List.collect"), TypeScheme{Vars: []TypeID{a, b}, Body: arrow(arrow(a, listOf(b)), arrow(listOf(a), listOf(b)))}, false)

	// Array combinators
	c.define(s.InternString("Array.length"), TypeScheme{Vars: []TypeID{a}, Body: arrow(arrOf(a), TypeInt)}, false)
	c.define(s.InternString("Array.get"), TypeScheme{Vars: []TypeID{a}, Body: arrow(arrOf(a), arrow(TypeInt, a))}, false)
	c.define(s.InternString("Array.set"), TypeScheme{Vars: []TypeID{a}, Body: arrow(arrOf(a), arrow(TypeInt, arrow(a, TypeUnit)))}, false)
	c.define(s.InternString("Array.map"), TypeScheme{Vars: []TypeID{a, b}, Body: arrow(arrow(a, b), arrow(arrOf(a), arrOf(b)))}, false)
	c.define(s.InternString("Array.filter"), TypeScheme{Vars: []TypeID{a}, Body: arrow(arrow(a, TypeBool), arrow(arrOf(a), arrOf(a)))}, false)
	c.define(s.InternString("Array.fold"), TypeScheme{Vars: []TypeID{a, b}, Body: arrow(arrow(a, arrow(b, a)), arrow(a, arrow(arrOf(b), a)))}, false)
	c.define(s.InternString("Array.ofList"), TypeScheme{Vars: []TypeID{a}, Body: arrow(listOf(a), arrOf(a))}, false)
	c.define(s.InternString("Array.toList"), TypeScheme{Vars: []TypeID{a}, Body: arrow(arrOf(a), listOf(a))}, false)

	// String combinators
	c.define(s.InternString("String.length"), TypeScheme{Body: arrow(TypeString, TypeInt)}, false)
	c.define(s.InternString("String.trim"), TypeScheme{Body: arrow(TypeString, TypeString)}, false)
	c.define(s.InternString("String.split"), TypeScheme{Body: arrow(TypeString, arrow(TypeString, listOf(TypeString)))}, false)
	c.define(s.InternString("String.join"), TypeScheme{Body: arrow(TypeString, arrow(listOf(TypeString), TypeString))}, false)
	c.define(s.InternString("String.contains"), TypeScheme{Body: arrow(TypeString, arrow(TypeString, TypeBool))}, false)
	c.define(s.InternString("String.toUpper"), TypeScheme{Body: arrow(TypeString, TypeString)}, false)
	c.define(s.InternString("String.toLower"), TypeScheme{Body: arrow(TypeString, TypeString)}, false)

	// Option combinators
	c.define(s.InternString("Option.isSome"), TypeScheme{Vars: []TypeID{a}, Body: arrow(optOf(a), TypeBool)}, false)
	c.define(s.InternString("Option.isNone"), TypeScheme{Vars: []TypeID{a}, Body: arrow(optOf(a), TypeBool)}, false)
	c.define(s.InternString("Option.defaultValue"), TypeScheme{Vars: []TypeID{a}, Body: arrow(a, arrow(optOf(a), a))}, false)
	c.define(s.InternString("Option.map"), TypeScheme{Vars: []TypeID{a, b}, Body: arrow(arrow(a, b), arrow(optOf(a), optOf(b)))}, false)
	c.define(s.InternString("Option.bind"), TypeScheme{Vars: []TypeID{a, b}, Body: arrow(arrow(a, optOf(b)), arrow(optOf(a), optOf(b)))}, false)

	// Result combinators
	c.define(s.InternString("Result.isOk"), TypeScheme{Vars: []TypeID{a, e}, Body: arrow(resOf(a, e), TypeBool)}, false)
	c.define(s.InternString("Result.isError"), TypeScheme{Vars: []TypeID{a, e}, Body: arrow(resOf(a, e), TypeBool)}, false)
	c.define(s.InternString("Result.defaultValue"), TypeScheme{Vars: []TypeID{a, e}, Body: arrow(a, arrow(resOf(a, e), a))}, false)
	c.define(s.InternString("Result.map"), TypeScheme{Vars: []TypeID{a, b, e}, Body: arrow(arrow(a, b), arrow(resOf(a, e), resOf(b, e)))}, false)
	c.define(s.InternString("Result.mapError"), TypeScheme{Vars: []TypeID{a, b, e}, Body: arrow(arrow(e, b), arrow(resOf(a, e), resOf(a, b)))}, false)
	c.define(s.InternString("Result.bind"), TypeScheme{Vars: []TypeID{a, b, e}, Body: arrow(arrow(a, resOf(b, e)), arrow(resOf(a, e), resOf(b, e)))}, false)

	// Async concurrency combinators
	c.define(s.InternString("Async.RunSynchronously"), TypeScheme{Vars: []TypeID{a}, Body: arrow(a, a)}, false)
	c.define(s.InternString("Async.Parallel"), TypeScheme{Vars: []TypeID{a}, Body: arrow(listOf(a), listOf(a))}, false)
	c.define(s.InternString("Async.Sleep"), TypeScheme{Body: arrow(TypeInt, TypeUnit)}, false)
	c.define(s.InternString("Async.Start"), TypeScheme{Vars: []TypeID{a}, Body: arrow(a, TypeUnit)}, false)
	c.define(s.InternString("Async.Return"), TypeScheme{Vars: []TypeID{a}, Body: arrow(a, a)}, false)

	// Test & property-testing combinators
	c.define(s.InternString("Test.assert"), TypeScheme{Body: arrow(TypeString, arrow(TypeBool, TypeUnit))}, false)
	c.define(s.InternString("Test.assertEq"), TypeScheme{Vars: []TypeID{a}, Body: arrow(TypeString, arrow(a, arrow(a, TypeUnit)))}, false)
	c.define(s.InternString("Test.forall"), TypeScheme{Vars: []TypeID{a}, Body: arrow(TypeString, arrow(listOf(a), arrow(arrow(a, TypeBool), TypeUnit)))}, false)

	// Persistent Map combinators
	kvStart, kvCount := c.Types.AddExtra([]TypeID{a, b})
	kvPairTy := c.Types.Add(TypeNode{Kind: TyTuple, Left: kvStart, Right: kvCount})
	c.define(s.InternString("Map.empty"), TypeScheme{Vars: []TypeID{a, b}, Body: mapOf(a, b)}, false)
	c.define(s.InternString("Map.add"), TypeScheme{Vars: []TypeID{a, b}, Body: arrow(a, arrow(b, arrow(mapOf(a, b), mapOf(a, b))))}, false)
	c.define(s.InternString("Map.remove"), TypeScheme{Vars: []TypeID{a, b}, Body: arrow(a, arrow(mapOf(a, b), mapOf(a, b)))}, false)
	c.define(s.InternString("Map.find"), TypeScheme{Vars: []TypeID{a, b}, Body: arrow(a, arrow(mapOf(a, b), b))}, false)
	c.define(s.InternString("Map.tryFind"), TypeScheme{Vars: []TypeID{a, b}, Body: arrow(a, arrow(mapOf(a, b), optOf(b)))}, false)
	c.define(s.InternString("Map.containsKey"), TypeScheme{Vars: []TypeID{a, b}, Body: arrow(a, arrow(mapOf(a, b), TypeBool))}, false)
	c.define(s.InternString("Map.count"), TypeScheme{Vars: []TypeID{a, b}, Body: arrow(mapOf(a, b), TypeInt)}, false)
	c.define(s.InternString("Map.keys"), TypeScheme{Vars: []TypeID{a, b}, Body: arrow(mapOf(a, b), listOf(a))}, false)
	c.define(s.InternString("Map.values"), TypeScheme{Vars: []TypeID{a, b}, Body: arrow(mapOf(a, b), listOf(b))}, false)
	c.define(s.InternString("Map.toList"), TypeScheme{Vars: []TypeID{a, b}, Body: arrow(mapOf(a, b), listOf(kvPairTy))}, false)
	c.define(s.InternString("Map.ofList"), TypeScheme{Vars: []TypeID{a, b}, Body: arrow(listOf(kvPairTy), mapOf(a, b))}, false)
	c.define(s.InternString("Map.map"), TypeScheme{Vars: []TypeID{a, b, e}, Body: arrow(arrow(a, arrow(b, e)), arrow(mapOf(a, b), mapOf(a, e)))}, false)
	c.define(s.InternString("Map.filter"), TypeScheme{Vars: []TypeID{a, b}, Body: arrow(arrow(a, arrow(b, TypeBool)), arrow(mapOf(a, b), mapOf(a, b)))}, false)
	c.define(s.InternString("Map.fold"), TypeScheme{Vars: []TypeID{a, b, e}, Body: arrow(arrow(e, arrow(a, arrow(b, e))), arrow(e, arrow(mapOf(a, b), e)))}, false)

	// Persistent Set combinators
	c.define(s.InternString("Set.empty"), TypeScheme{Vars: []TypeID{a}, Body: setOf(a)}, false)
	c.define(s.InternString("Set.add"), TypeScheme{Vars: []TypeID{a}, Body: arrow(a, arrow(setOf(a), setOf(a)))}, false)
	c.define(s.InternString("Set.remove"), TypeScheme{Vars: []TypeID{a}, Body: arrow(a, arrow(setOf(a), setOf(a)))}, false)
	c.define(s.InternString("Set.contains"), TypeScheme{Vars: []TypeID{a}, Body: arrow(a, arrow(setOf(a), TypeBool))}, false)
	c.define(s.InternString("Set.count"), TypeScheme{Vars: []TypeID{a}, Body: arrow(setOf(a), TypeInt)}, false)
	c.define(s.InternString("Set.union"), TypeScheme{Vars: []TypeID{a}, Body: arrow(setOf(a), arrow(setOf(a), setOf(a)))}, false)
	c.define(s.InternString("Set.intersect"), TypeScheme{Vars: []TypeID{a}, Body: arrow(setOf(a), arrow(setOf(a), setOf(a)))}, false)
	c.define(s.InternString("Set.diff"), TypeScheme{Vars: []TypeID{a}, Body: arrow(setOf(a), arrow(setOf(a), setOf(a)))}, false)
	c.define(s.InternString("Set.isSubset"), TypeScheme{Vars: []TypeID{a}, Body: arrow(setOf(a), arrow(setOf(a), TypeBool))}, false)
	c.define(s.InternString("Set.toList"), TypeScheme{Vars: []TypeID{a}, Body: arrow(setOf(a), listOf(a))}, false)
	c.define(s.InternString("Set.ofList"), TypeScheme{Vars: []TypeID{a}, Body: arrow(listOf(a), setOf(a))}, false)
	c.define(s.InternString("Set.map"), TypeScheme{Vars: []TypeID{a, b}, Body: arrow(arrow(a, b), arrow(setOf(a), setOf(b)))}, false)
	c.define(s.InternString("Set.filter"), TypeScheme{Vars: []TypeID{a}, Body: arrow(arrow(a, TypeBool), arrow(setOf(a), setOf(a)))}, false)
	c.define(s.InternString("Set.fold"), TypeScheme{Vars: []TypeID{a, b}, Body: arrow(arrow(a, arrow(b, a)), arrow(a, arrow(setOf(b), a)))}, false)

	// Regex combinators
	c.define(s.InternString("Regex.isMatch"), TypeScheme{Body: arrow(TypeString, arrow(TypeString, TypeBool))}, false)
	c.define(s.InternString("Regex.find"), TypeScheme{Body: arrow(TypeString, arrow(TypeString, optOf(TypeString)))}, false)
	c.define(s.InternString("Regex.findAll"), TypeScheme{Body: arrow(TypeString, arrow(TypeString, listOf(TypeString)))}, false)
	c.define(s.InternString("Regex.replace"), TypeScheme{Body: arrow(TypeString, arrow(TypeString, arrow(TypeString, TypeString)))}, false)
	c.define(s.InternString("Regex.split"), TypeScheme{Body: arrow(TypeString, arrow(TypeString, listOf(TypeString)))}, false)
	c.define(s.InternString("Regex.groups"), TypeScheme{Body: arrow(TypeString, arrow(TypeString, optOf(listOf(TypeString))))}, false)

	// Math combinators
	c.define(s.InternString("Math.pi"), TypeScheme{Body: TypeFloat}, false)
	c.define(s.InternString("Math.abs"), TypeScheme{Vars: []TypeID{a}, Body: arrow(a, a)}, false)
	c.define(s.InternString("Math.min"), TypeScheme{Vars: []TypeID{a}, Body: arrow(a, arrow(a, a))}, false)
	c.define(s.InternString("Math.max"), TypeScheme{Vars: []TypeID{a}, Body: arrow(a, arrow(a, a))}, false)
	c.define(s.InternString("Math.sqrt"), TypeScheme{Body: arrow(TypeFloat, TypeFloat)}, false)
	c.define(s.InternString("Math.pow"), TypeScheme{Body: arrow(TypeFloat, arrow(TypeFloat, TypeFloat))}, false)
	c.define(s.InternString("Math.sin"), TypeScheme{Body: arrow(TypeFloat, TypeFloat)}, false)
	c.define(s.InternString("Math.cos"), TypeScheme{Body: arrow(TypeFloat, TypeFloat)}, false)
	c.define(s.InternString("Math.floor"), TypeScheme{Body: arrow(TypeFloat, TypeFloat)}, false)
	c.define(s.InternString("Math.ceil"), TypeScheme{Body: arrow(TypeFloat, TypeFloat)}, false)
	c.define(s.InternString("Math.round"), TypeScheme{Body: arrow(TypeFloat, TypeFloat)}, false)

	// Time combinators
	tStart, tCount := c.Types.AddExtra([]TypeID{a, TypeInt})
	measuredPairTy := c.Types.Add(TypeNode{Kind: TyTuple, Left: tStart, Right: tCount})
	c.define(s.InternString("Time.nowUnixMs"), TypeScheme{Body: arrow(TypeUnit, TypeInt)}, false)
	c.define(s.InternString("Time.nowMs"), TypeScheme{Body: arrow(TypeUnit, TypeInt)}, false)
	c.define(s.InternString("Time.nowIso"), TypeScheme{Body: arrow(TypeUnit, TypeString)}, false)
	c.define(s.InternString("Time.sleepMs"), TypeScheme{Body: arrow(TypeInt, TypeUnit)}, false)
	c.define(s.InternString("Time.measureMs"), TypeScheme{Vars: []TypeID{a}, Body: arrow(arrow(TypeUnit, a), measuredPairTy)}, false)
	c.define(s.InternString("Time.parseIso"), TypeScheme{Body: arrow(TypeString, resOf(TypeInt, TypeString))}, false)
	c.define(s.InternString("Time.formatIso"), TypeScheme{Body: arrow(TypeInt, TypeString)}, false)

	// Sys combinators
	c.define(s.InternString("Sys.args"), TypeScheme{Body: arrow(TypeUnit, listOf(TypeString))}, false)
	c.define(s.InternString("Sys.getenv"), TypeScheme{Body: arrow(TypeString, optOf(TypeString))}, false)
	c.define(s.InternString("Sys.setenv"), TypeScheme{Body: arrow(TypeString, arrow(TypeString, TypeUnit))}, false)
	c.define(s.InternString("Sys.cwd"), TypeScheme{Body: arrow(TypeUnit, TypeString)}, false)
	c.define(s.InternString("Sys.fileExists"), TypeScheme{Body: arrow(TypeString, TypeBool)}, false)

	// Crypto combinators
	c.define(s.InternString("Crypto.sha256"), TypeScheme{Body: arrow(TypeString, TypeString)}, false)
	c.define(s.InternString("Crypto.hmacSha256"), TypeScheme{Body: arrow(TypeString, arrow(TypeString, TypeString))}, false)
	c.define(s.InternString("Crypto.base64Encode"), TypeScheme{Body: arrow(TypeString, TypeString)}, false)
	c.define(s.InternString("Crypto.base64Decode"), TypeScheme{Body: arrow(TypeString, resOf(TypeString, TypeString))}, false)
	c.define(s.InternString("Crypto.uuid4"), TypeScheme{Body: arrow(TypeUnit, TypeString)}, false)

	// File, Json, and Http combinators
	c.define(s.InternString("File.readText"), TypeScheme{Body: arrow(TypeString, TypeString)}, false)
	c.define(s.InternString("File.writeText"), TypeScheme{Body: arrow(TypeString, arrow(TypeString, TypeUnit))}, false)
	c.define(s.InternString("File.exists"), TypeScheme{Body: arrow(TypeString, TypeBool)}, false)
	c.define(s.InternString("File.openReader"), TypeScheme{Body: arrow(TypeString, TypeString)}, false)

	c.define(s.InternString("Json.encode"), TypeScheme{Vars: []TypeID{a}, Body: arrow(a, TypeString)}, false)
	c.define(s.InternString("Json.decode"), TypeScheme{Vars: []TypeID{a}, Body: arrow(TypeString, a)}, false)
	c.define(s.InternString("Json.tryDecode"), TypeScheme{Vars: []TypeID{a}, Body: arrow(TypeString, resOf(a, TypeString))}, false)

	c.define(s.InternString("Http.get"), TypeScheme{Body: arrow(TypeString, TypeString)}, false)
	c.define(s.InternString("Http.getResult"), TypeScheme{Body: arrow(TypeString, resOf(TypeString, TypeString))}, false)
	c.define(s.InternString("Http.post"), TypeScheme{Body: arrow(TypeString, arrow(TypeString, arrow(TypeString, TypeString)))}, false)
	c.define(s.InternString("Http.serveOnce"), TypeScheme{Vars: []TypeID{a, b}, Body: arrow(TypeString, arrow(arrow(a, b), TypeString))}, false)
}

func (c *Checker) pushScope() {
	c.Scopes = append(c.Scopes, make(map[token.StringID]Binding))
}

func (c *Checker) popScope() {
	if len(c.Scopes) > 1 {
		c.Scopes = c.Scopes[:len(c.Scopes)-1]
	}
}

func (c *Checker) define(name token.StringID, scheme TypeScheme, mutable bool) {
	if name == token.NoString {
		return
	}
	c.Scopes[len(c.Scopes)-1][name] = Binding{Scheme: scheme, Mutable: mutable}
}

func (c *Checker) lookup(name token.StringID) (Binding, bool) {
	for i := len(c.Scopes) - 1; i >= 0; i-- {
		if b, ok := c.Scopes[i][name]; ok {
			return b, true
		}
	}
	return Binding{}, false
}

// Generalize quantifies type variables in ty that do not occur free in the current environment.
func (c *Checker) Generalize(ty TypeID) TypeScheme {
	envFree := make(map[TypeID]bool)
	for _, scope := range c.Scopes {
		for _, b := range scope {
			bound := make(map[TypeID]bool, len(b.Scheme.Vars))
			for _, v := range b.Scheme.Vars {
				bound[c.Types.Find(v)] = true
			}
			c.collectFreeVars(b.Scheme.Body, bound, envFree)
		}
	}

	tyFree := make(map[TypeID]bool)
	c.collectFreeVars(ty, nil, tyFree)
	var vars []TypeID
	for v := range tyFree {
		if !envFree[v] {
			vars = append(vars, v)
		}
	}
	return TypeScheme{Vars: vars, Body: ty}
}

// Instantiate replaces quantified type variables in scheme with fresh type variables.
func (c *Checker) Instantiate(scheme TypeScheme) TypeID {
	if len(scheme.Vars) == 0 {
		return scheme.Body
	}
	subst := make(map[TypeID]TypeID, len(scheme.Vars))
	for _, v := range scheme.Vars {
		subst[c.Types.Find(v)] = c.Types.FreshVar()
	}
	for _, v := range scheme.Vars {
		rv := c.Types.Find(v)
		if rowMap, ok := c.Types.RowFields[rv]; ok && len(rowMap) > 0 {
			freshVar := subst[rv]
			for fName, fTy := range rowMap {
				c.Types.SetRowField(freshVar, fName, c.applySubst(fTy, subst))
			}
		}
	}
	return c.applySubst(scheme.Body, subst)
}

func (c *Checker) collectFreeVars(id TypeID, bound map[TypeID]bool, out map[TypeID]bool) {
	id = c.Types.Find(id)
	if id == NoType {
		return
	}
	n := c.Types.Nodes[id]
	switch n.Kind {
	case TyVar:
		if bound == nil || !bound[id] {
			out[id] = true
		}
		for _, fTy := range c.Types.RowFields[id] {
			c.collectFreeVars(fTy, bound, out)
		}
	case TyArrow, TyResult:
		c.collectFreeVars(TypeID(n.Left), bound, out)
		c.collectFreeVars(TypeID(n.Right), bound, out)
	case TyList, TyArray, TySeq, TyOption, TyMeasure:
		c.collectFreeVars(TypeID(n.Left), bound, out)
	case TyTuple, TyRecord, TyADT:
		for i := uint32(0); i < n.Right; i++ {
			c.collectFreeVars(c.Types.Extra[n.Left+i], bound, out)
		}
		if n.Kind == TyRecord {
			for _, fTy := range c.Types.RowFields[id] {
				c.collectFreeVars(fTy, bound, out)
			}
		}
	}
}

func (c *Checker) applySubst(id TypeID, subst map[TypeID]TypeID) TypeID {
	id = c.Types.Find(id)
	if id == NoType {
		return NoType
	}
	n := c.Types.Nodes[id]
	switch n.Kind {
	case TyVar:
		if repl, ok := subst[id]; ok {
			return repl
		}
		return id
	case TyArrow, TyResult:
		l := c.applySubst(TypeID(n.Left), subst)
		r := c.applySubst(TypeID(n.Right), subst)
		if l == TypeID(n.Left) && r == TypeID(n.Right) {
			return id
		}
		return c.Types.Add(TypeNode{Kind: n.Kind, Left: uint32(l), Right: uint32(r)})
	case TyList, TyArray, TySeq, TyOption:
		l := c.applySubst(TypeID(n.Left), subst)
		if l == TypeID(n.Left) {
			return id
		}
		return c.Types.Add(TypeNode{Kind: n.Kind, Left: uint32(l)})
	case TyMeasure:
		l := c.applySubst(TypeID(n.Left), subst)
		if l == TypeID(n.Left) {
			return id
		}
		return c.Types.Add(TypeNode{Kind: TyMeasure, Left: uint32(l), Right: n.Right})
	case TyTuple, TyRecord, TyADT:
		changed := false
		elems := make([]TypeID, n.Right)
		for i := uint32(0); i < n.Right; i++ {
			orig := c.Types.Extra[n.Left+i]
			elems[i] = c.applySubst(orig, subst)
			if elems[i] != orig {
				changed = true
			}
		}
		if !changed {
			return id
		}
		start, count := c.Types.AddExtra(elems)
		return c.Types.Add(TypeNode{Kind: n.Kind, Name: n.Name, Left: start, Right: count})
	default:
		return id
	}
}

// isSyntacticValue enforces the ML Value Restriction: only non-expansive
// syntactic values may be generalized into polymorphic TypeSchemes.
func (c *Checker) isSyntacticValue(nodeID ast.NodeID) bool {
	if nodeID == ast.NoNode {
		return true
	}
	n := c.AST.Get(nodeID)
	switch n.Kind {
	case ast.KindLambda, ast.KindIdent, ast.KindInt, ast.KindFloat,
		ast.KindString, ast.KindBool, ast.KindUnit:
		return true
	case ast.KindOption, ast.KindResult, ast.KindFieldAccess:
		return c.isSyntacticValue(ast.NodeID(n.Left))
	case ast.KindTuple, ast.KindList:
		children := c.AST.ExtraSlice(n.Left, n.Right)
		for _, ch := range children {
			if !c.isSyntacticValue(ast.NodeID(ch)) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func (c *Checker) recordNodeType(id ast.NodeID, ty TypeID) TypeID {
	if int(id) < len(c.NodeTypes) {
		c.NodeTypes[id] = ty
	}
	return ty
}

func (c *Checker) inferNode(id ast.NodeID) (ty TypeID, err error) {
	if id == ast.NoNode {
		return TypeUnit, nil
	}
	defer func() {
		if err != nil {
			err = c.FormatDiagnostic(id, err)
		}
	}()
	n := *c.AST.Get(id)

	switch n.Kind {
	case ast.KindProgram, ast.KindBlock:
		children := c.AST.ExtraSlice(n.Left, n.Right)
		if n.Kind == ast.KindBlock {
			c.pushScope()
			defer c.popScope()
		}
		lastTy := TypeUnit
		for _, ch := range children {
			ty, err := c.inferNode(ast.NodeID(ch))
			if err != nil {
				return NoType, err
			}
			lastTy = ty
		}
		return c.recordNodeType(id, lastTy), nil

	case ast.KindInt:
		return c.recordNodeType(id, TypeInt), nil
	case ast.KindFloat:
		return c.recordNodeType(id, TypeFloat), nil
	case ast.KindString:
		return c.recordNodeType(id, TypeString), nil
	case ast.KindBool:
		return c.recordNodeType(id, TypeBool), nil
	case ast.KindUnit:
		return c.recordNodeType(id, TypeUnit), nil

	case ast.KindIdent:
		name := token.StringID(n.Data)
		if b, ok := c.lookup(name); ok {
			return c.recordNodeType(id, c.Instantiate(b.Scheme)), nil
		}
		if ctor, ok := c.Constructors[name]; ok {
			if ctor.Payload == NoType {
				return c.recordNodeType(id, ctor.ADTType), nil
			}
			fnTy := c.Types.Add(TypeNode{
				Kind:  TyArrow,
				Left:  uint32(ctor.Payload),
				Right: uint32(ctor.ADTType),
			})
			return c.recordNodeType(id, fnTy), nil
		}
		// Unbound external/stdlib symbol: bind a fresh variable so inference can proceed
		fv := c.Types.FreshVar()
		c.define(name, TypeScheme{Body: fv}, false)
		return c.recordNodeType(id, fv), nil

	case ast.KindLet, ast.KindLetMutable, ast.KindLetRec, ast.KindUse:
		return c.inferLet(id, n)

	case ast.KindMutate:
		targetName := token.StringID(n.Data)
		if targetName != token.NoString {
			if b, ok := c.lookup(targetName); ok && !b.Mutable {
				return NoType, fmt.Errorf("cannot mutate immutable binding %q", c.AST.Strings.String(targetName))
			}
		}
		lhsTy, err := c.inferNode(ast.NodeID(n.Left))
		if err != nil {
			return NoType, err
		}
		rhsTy, err := c.inferNode(ast.NodeID(n.Right))
		if err != nil {
			return NoType, err
		}
		if err := c.Types.Unify(lhsTy, rhsTy); err != nil {
			return NoType, err
		}
		return c.recordNodeType(id, TypeUnit), nil

	case ast.KindLambda:
		c.pushScope()
		defer c.popScope()
		paramNode := c.AST.Get(ast.NodeID(n.Left))
		paramTy := c.Types.FreshVar()
		paramName := token.StringID(n.Data)
		if paramNode.Kind == ast.KindUnit {
			paramTy = TypeUnit
		} else if paramNode.Kind == ast.KindTypeAnnot {
			annotTy := c.resolveTypeAST(ast.NodeID(paramNode.Right))
			paramTy = annotTy
		}
		if paramName != token.NoString {
			c.define(paramName, TypeScheme{Body: paramTy}, false)
		}
		bodyTy, err := c.inferNode(ast.NodeID(n.Right))
		if err != nil {
			return NoType, err
		}
		arrow := c.Types.Add(TypeNode{
			Kind:  TyArrow,
			Left:  uint32(paramTy),
			Right: uint32(bodyTy),
		})
		return c.recordNodeType(id, arrow), nil

	case ast.KindApply:
		fnTy, err := c.inferNode(ast.NodeID(n.Left))
		if err != nil {
			return NoType, err
		}
		argTy, err := c.inferNode(ast.NodeID(n.Right))
		if err != nil {
			return NoType, err
		}
		retTy := c.Types.FreshVar()
		expectedFn := c.Types.Add(TypeNode{
			Kind:  TyArrow,
			Left:  uint32(argTy),
			Right: uint32(retTy),
		})
		if err := c.Types.Unify(fnTy, expectedFn); err != nil {
			return NoType, err
		}
		return c.recordNodeType(id, c.Types.Find(retTy)), nil

	case ast.KindPipeRight:
		argTy, err := c.inferNode(ast.NodeID(n.Left))
		if err != nil {
			return NoType, err
		}
		fnTy, err := c.inferNode(ast.NodeID(n.Right))
		if err != nil {
			return NoType, err
		}
		retTy := c.Types.FreshVar()
		expectedFn := c.Types.Add(TypeNode{
			Kind:  TyArrow,
			Left:  uint32(argTy),
			Right: uint32(retTy),
		})
		if err := c.Types.Unify(fnTy, expectedFn); err != nil {
			return NoType, err
		}
		return c.recordNodeType(id, c.Types.Find(retTy)), nil

	case ast.KindPipeLeft:
		fnTy, err := c.inferNode(ast.NodeID(n.Left))
		if err != nil {
			return NoType, err
		}
		argTy, err := c.inferNode(ast.NodeID(n.Right))
		if err != nil {
			return NoType, err
		}
		retTy := c.Types.FreshVar()
		expectedFn := c.Types.Add(TypeNode{
			Kind:  TyArrow,
			Left:  uint32(argTy),
			Right: uint32(retTy),
		})
		if err := c.Types.Unify(fnTy, expectedFn); err != nil {
			return NoType, err
		}
		return c.recordNodeType(id, c.Types.Find(retTy)), nil

	case ast.KindCompose:
		fTy, err := c.inferNode(ast.NodeID(n.Left))
		if err != nil {
			return NoType, err
		}
		gTy, err := c.inferNode(ast.NodeID(n.Right))
		if err != nil {
			return NoType, err
		}
		a := c.Types.FreshVar()
		b := c.Types.FreshVar()
		d := c.Types.FreshVar()
		if n.Flags&ast.FlagBackward != 0 {
			// f << g : (b -> d) -> (a -> b) -> (a -> d)
			if err := c.Types.Unify(gTy, c.Types.Add(TypeNode{Kind: TyArrow, Left: uint32(a), Right: uint32(b)})); err != nil {
				return NoType, err
			}
			if err := c.Types.Unify(fTy, c.Types.Add(TypeNode{Kind: TyArrow, Left: uint32(b), Right: uint32(d)})); err != nil {
				return NoType, err
			}
		} else {
			// f >> g : (a -> b) -> (b -> d) -> (a -> d)
			if err := c.Types.Unify(fTy, c.Types.Add(TypeNode{Kind: TyArrow, Left: uint32(a), Right: uint32(b)})); err != nil {
				return NoType, err
			}
			if err := c.Types.Unify(gTy, c.Types.Add(TypeNode{Kind: TyArrow, Left: uint32(b), Right: uint32(d)})); err != nil {
				return NoType, err
			}
		}
		compTy := c.Types.Add(TypeNode{Kind: TyArrow, Left: uint32(a), Right: uint32(d)})
		return c.recordNodeType(id, compTy), nil

	case ast.KindInfix:
		return c.inferInfix(id, n)

	case ast.KindPrefix:
		opTy, err := c.inferNode(ast.NodeID(n.Left))
		if err != nil {
			return NoType, err
		}
		if token.TokenKind(n.Op) == token.TokenNot {
			if err := c.Types.Unify(opTy, TypeBool); err != nil {
				return NoType, err
			}
			return c.recordNodeType(id, TypeBool), nil
		}
		return c.recordNodeType(id, opTy), nil

	case ast.KindTuple:
		children := c.AST.ExtraSlice(n.Left, n.Right)
		elemTys := make([]TypeID, len(children))
		for i, ch := range children {
			t, err := c.inferNode(ast.NodeID(ch))
			if err != nil {
				return NoType, err
			}
			elemTys[i] = t
		}
		start, count := c.Types.AddExtra(elemTys)
		return c.recordNodeType(id, c.Types.Add(TypeNode{Kind: TyTuple, Left: start, Right: count})), nil

	case ast.KindList, ast.KindArray:
		children := c.AST.ExtraSlice(n.Left, n.Right)
		elemTy := c.Types.FreshVar()
		for _, ch := range children {
			t, err := c.inferNode(ast.NodeID(ch))
			if err != nil {
				return NoType, err
			}
			if err := c.Types.Unify(elemTy, t); err != nil {
				return NoType, err
			}
		}
		k := TyList
		if n.Kind == ast.KindArray {
			k = TyArray
		}
		return c.recordNodeType(id, c.Types.Add(TypeNode{Kind: k, Left: uint32(elemTy)})), nil

	case ast.KindRange:
		sTy, err := c.inferNode(ast.NodeID(n.Left))
		if err != nil {
			return NoType, err
		}
		eTy, err := c.inferNode(ast.NodeID(n.Right))
		if err != nil {
			return NoType, err
		}
		if err := c.Types.Unify(sTy, TypeInt); err != nil {
			return NoType, err
		}
		if err := c.Types.Unify(eTy, TypeInt); err != nil {
			return NoType, err
		}
		return c.recordNodeType(id, c.Types.Add(TypeNode{Kind: TySeq, Left: uint32(TypeInt)})), nil

	case ast.KindSeq:
		bodyTy, err := c.inferNode(ast.NodeID(n.Left))
		if err != nil {
			return NoType, err
		}
		r := c.Types.Find(bodyTy)
		if c.Types.Nodes[r].Kind == TySeq {
			return c.recordNodeType(id, r), nil
		}
		return c.recordNodeType(id, c.Types.Add(TypeNode{Kind: TySeq, Left: uint32(r)})), nil

	case ast.KindOption:
		elemTy := c.Types.FreshVar()
		if n.Data == 1 && n.Left != 0 {
			pTy, err := c.inferNode(ast.NodeID(n.Left))
			if err != nil {
				return NoType, err
			}
			elemTy = pTy
		}
		return c.recordNodeType(id, c.Types.Add(TypeNode{Kind: TyOption, Left: uint32(elemTy)})), nil

	case ast.KindResult:
		okTy := c.Types.FreshVar()
		errTy := c.Types.FreshVar()
		pTy, err := c.inferNode(ast.NodeID(n.Left))
		if err != nil {
			return NoType, err
		}
		if n.Data == 1 {
			okTy = pTy
		} else {
			errTy = pTy
		}
		return c.recordNodeType(id, c.Types.Add(TypeNode{Kind: TyResult, Left: uint32(okTy), Right: uint32(errTy)})), nil

	case ast.KindMeasure:
		if n.Left == 0 {
			// `[<Measure>] type kg`
			return c.recordNodeType(id, TypeUnit), nil
		}
		baseTy, err := c.inferNode(ast.NodeID(n.Left))
		if err != nil {
			return NoType, err
		}
		mID := c.Types.BuildMeasureFromAST(c.AST, ast.NodeID(n.Right))
		return c.recordNodeType(id, c.Types.MakeMeasureType(baseTy, mID)), nil

	case ast.KindRecordType:
		recName := token.StringID(n.Data)
		fields := c.AST.ExtraSlice(n.Left, n.Right)
		fNames := make([]token.StringID, len(fields))
		fTypes := make([]TypeID, len(fields))
		for i, fNodeID := range fields {
			fn := c.AST.Get(ast.NodeID(fNodeID))
			fNames[i] = token.StringID(fn.Data)
			fTypes[i] = c.resolveTypeAST(ast.NodeID(fn.Left))
			c.FieldToRec[fNames[i]] = recName
		}
		start, count := c.Types.AddExtra(fTypes)
		recTy := c.Types.Add(TypeNode{Kind: TyRecord, Name: recName, Left: start, Right: count})
		for i, fName := range fNames {
			c.Types.SetRowField(recTy, fName, fTypes[i])
		}
		c.Records[recName] = RecordInfo{RecordType: recTy, FieldNames: fNames, FieldTypes: fTypes}
		c.NamedTypes[recName] = recTy
		return c.recordNodeType(id, TypeUnit), nil

	case ast.KindRecordLit:
		fields := c.AST.ExtraSlice(n.Left, n.Right)
		fNames := make([]token.StringID, len(fields))
		fTypes := make([]TypeID, len(fields))
		for i, fNodeID := range fields {
			fn := c.AST.Get(ast.NodeID(fNodeID))
			fNames[i] = token.StringID(fn.Data)
			fValTy, err := c.inferNode(ast.NodeID(fn.Left))
			if err != nil {
				return NoType, err
			}
			fTypes[i] = fValTy
		}
		if n.Data != 0 {
			baseTy, err := c.inferNode(ast.NodeID(n.Data))
			if err != nil {
				return NoType, err
			}
			baseRoot := c.Types.Find(baseTy)
			if c.Types.Nodes[baseRoot].Kind == TyRecord {
				for i, fName := range fNames {
					if declTy, ok := c.Types.RowFields[baseRoot][fName]; ok {
						if err := c.Types.Unify(fTypes[i], declTy); err != nil {
							return NoType, err
						}
					} else if c.Types.Nodes[baseRoot].Name != 0 {
						return NoType, fmt.Errorf("record %s has no field '%s'", c.Types.Format(baseRoot), c.AST.Strings.String(fName))
					}
				}
				return c.recordNodeType(id, baseRoot), nil
			}
			for i, fName := range fNames {
				if rowTy, ok := c.Types.RowFields[baseRoot][fName]; ok {
					if err := c.Types.Unify(fTypes[i], rowTy); err != nil {
						return NoType, err
					}
				} else {
					c.Types.SetRowField(baseRoot, fName, fTypes[i])
				}
			}
			return c.recordNodeType(id, baseRoot), nil
		}

		// Check if the literal's field set matches a declared nominal record
		var matchedRec token.StringID
		if len(fNames) > 0 {
			if rn, ok := c.FieldToRec[fNames[0]]; ok {
				recInfo := c.Records[rn]
				if len(recInfo.FieldNames) == len(fNames) {
					allMatch := true
					for _, fn := range fNames {
						found := false
						for _, df := range recInfo.FieldNames {
							if df == fn {
								found = true
								break
							}
						}
						if !found {
							allMatch = false
							break
						}
					}
					if allMatch {
						matchedRec = rn
					}
				}
			}
		}
		if matchedRec != 0 {
			recInfo := c.Records[matchedRec]
			for i, fName := range fNames {
				for idx, declField := range recInfo.FieldNames {
					if declField == fName {
						if err := c.Types.Unify(fTypes[i], recInfo.FieldTypes[idx]); err != nil {
							return NoType, err
						}
					}
				}
			}
			return c.recordNodeType(id, recInfo.RecordType), nil
		}
		// Structural / anonymous record literal
		recTy := c.Types.Add(TypeNode{Kind: TyRecord, Name: token.NoString})
		for i, fName := range fNames {
			c.Types.SetRowField(recTy, fName, fTypes[i])
		}
		return c.recordNodeType(id, recTy), nil

	case ast.KindFieldAccess:
		leftNode := c.AST.Get(ast.NodeID(n.Left))
		fName := token.StringID(n.Data)
		if leftNode.Kind == ast.KindIdent {
			modPrefix := c.AST.Strings.String(token.StringID(leftNode.Data))
			qualID := c.AST.Strings.InternString(modPrefix + "." + c.AST.Strings.String(fName))
			if b, ok := c.lookup(qualID); ok {
				return c.recordNodeType(id, c.Instantiate(b.Scheme)), nil
			}
			if len(modPrefix) > 0 && modPrefix[0] >= 'A' && modPrefix[0] <= 'Z' {
				if _, isBound := c.lookup(token.StringID(leftNode.Data)); !isBound {
					return c.recordNodeType(id, c.Types.FreshVar()), nil
				}
			}
		}
		recvTy, err := c.inferNode(ast.NodeID(n.Left))
		if err != nil {
			return NoType, err
		}
		recvRoot := c.Types.Find(recvTy)
		if c.Types.Nodes[recvRoot].Kind == TyRecord {
			if fTy, ok := c.Types.RowFields[recvRoot][fName]; ok {
				return c.recordNodeType(id, fTy), nil
			}
			return NoType, fmt.Errorf("record %s has no field '%s'", c.Types.Format(recvRoot), c.AST.Strings.String(fName))
		}
		if c.Types.Nodes[recvRoot].Kind == TyVar {
			if fTy, ok := c.Types.RowFields[recvRoot][fName]; ok {
				return c.recordNodeType(id, fTy), nil
			}
			fTy := c.Types.FreshVar()
			if rn, ok := c.FieldToRec[fName]; ok {
				recInfo := c.Records[rn]
				for i, dn := range recInfo.FieldNames {
					if dn == fName {
						_ = c.Types.Unify(fTy, recInfo.FieldTypes[i])
						break
					}
				}
			}
			c.Types.SetRowField(recvRoot, fName, fTy)
			return c.recordNodeType(id, fTy), nil
		}
		return c.recordNodeType(id, c.Types.FreshVar()), nil

	case ast.KindDUType:
		duName := token.StringID(n.Data)
		duTy := c.Types.Add(TypeNode{Kind: TyADT, Name: duName})
		c.NamedTypes[duName] = duTy
		cases := c.AST.ExtraSlice(n.Left, n.Right)
		for _, cID := range cases {
			cn := c.AST.Get(ast.NodeID(cID))
			caseName := token.StringID(cn.Data)
			var payloadTy TypeID
			if cn.Left != 0 {
				payloadTy = c.resolveTypeAST(ast.NodeID(cn.Left))
			}
			c.Constructors[caseName] = ConstructorInfo{ADTType: duTy, Payload: payloadTy}
		}
		return c.recordNodeType(id, TypeUnit), nil

	case ast.KindMatch:
		return c.inferMatch(id, n)

	case ast.KindActivePattern:
		items := c.AST.ExtraSlice(n.Left, n.Right)
		var tagNames []token.StringID
		for i := 0; i < len(items)-1; i++ {
			tn := c.AST.Get(ast.NodeID(items[i]))
			tName := token.StringID(tn.Data)
			tagNames = append(tagNames, tName)
			c.Constructors[tName] = ConstructorInfo{ADTType: c.Types.FreshVar()}
		}
		groupTags := tagNames
		if n.Flags&ast.FlagPartialPat != 0 {
			groupTags = append(append([]token.StringID{}, tagNames...), c.AST.Strings.InternString("_"))
		}
		for _, tName := range tagNames {
			c.ActivePats[tName] = groupTags
		}
		if len(items) > 0 {
			fnNodeID := ast.NodeID(items[len(items)-1])
			fnTy, err := c.inferNode(fnNodeID)
			if err != nil {
				return NoType, err
			}
			fnRoot := c.Types.Find(fnTy)
			if c.Types.Nodes[fnRoot].Kind == TyArrow {
				inScheme := c.Generalize(TypeID(c.Types.Nodes[fnRoot].Left))
				retRoot := c.Types.Find(TypeID(c.Types.Nodes[fnRoot].Right))
				for _, tName := range tagNames {
					c.ActivePatInputs[tName] = inScheme
					if c.Types.Nodes[retRoot].Kind == TyOption {
						c.ActivePatPayloads[tName] = c.Generalize(TypeID(c.Types.Nodes[retRoot].Left))
					}
				}
			}
		}
		return c.recordNodeType(id, TypeUnit), nil

	case ast.KindIf:
		condTy, err := c.inferNode(ast.NodeID(n.Left))
		if err != nil {
			return NoType, err
		}
		if err := c.Types.Unify(condTy, TypeBool); err != nil {
			return NoType, err
		}
		thenTy, err := c.inferNode(ast.NodeID(n.Right))
		if err != nil {
			return NoType, err
		}
		if n.Data != 0 {
			elseTy, err := c.inferNode(ast.NodeID(n.Data))
			if err != nil {
				return NoType, err
			}
			if err := c.Types.Unify(thenTy, elseTy); err != nil {
				return NoType, err
			}
		}
		return c.recordNodeType(id, thenTy), nil

	case ast.KindForIn:
		c.pushScope()
		defer c.popScope()
		collTy, err := c.inferNode(ast.NodeID(n.Left))
		if err != nil {
			return NoType, err
		}
		elemTy := c.Types.FreshVar()
		rColl := c.Types.Find(collTy)
		switch c.Types.Nodes[rColl].Kind {
		case TyList, TyArray, TySeq:
			_ = c.Types.Unify(elemTy, TypeID(c.Types.Nodes[rColl].Left))
		}
		c.define(token.StringID(n.Data), TypeScheme{Body: elemTy}, false)
		if _, err := c.inferNode(ast.NodeID(n.Right)); err != nil {
			return NoType, err
		}
		return c.recordNodeType(id, TypeUnit), nil

	case ast.KindForTo:
		c.pushScope()
		defer c.popScope()
		if _, err := c.inferNode(ast.NodeID(n.Left)); err != nil {
			return NoType, err
		}
		c.define(token.StringID(n.Data), TypeScheme{Body: TypeInt}, false)
		if _, err := c.inferNode(ast.NodeID(n.Right)); err != nil {
			return NoType, err
		}
		return c.recordNodeType(id, TypeUnit), nil

	case ast.KindWhile:
		condTy, err := c.inferNode(ast.NodeID(n.Left))
		if err != nil {
			return NoType, err
		}
		if err := c.Types.Unify(condTy, TypeBool); err != nil {
			return NoType, err
		}
		if _, err := c.inferNode(ast.NodeID(n.Right)); err != nil {
			return NoType, err
		}
		return c.recordNodeType(id, TypeUnit), nil

	case ast.KindTryWith:
		bodyTy, err := c.inferNode(ast.NodeID(n.Left))
		if err != nil {
			return NoType, err
		}
		armsBlock := c.AST.Get(ast.NodeID(n.Right))
		arms := c.AST.ExtraSlice(armsBlock.Left, armsBlock.Right)
		for _, armID := range arms {
			arm := c.AST.Get(ast.NodeID(armID))
			armTy, err := c.inferNode(ast.NodeID(arm.Right))
			if err != nil {
				return NoType, err
			}
			if err := c.Types.Unify(bodyTy, armTy); err != nil {
				return NoType, err
			}
		}
		return c.recordNodeType(id, bodyTy), nil

	case ast.KindTryFinally:
		bodyTy, err := c.inferNode(ast.NodeID(n.Left))
		if err != nil {
			return NoType, err
		}
		if _, err := c.inferNode(ast.NodeID(n.Right)); err != nil {
			return NoType, err
		}
		return c.recordNodeType(id, bodyTy), nil

	case ast.KindCompExpr:
		prevBuilder := c.ActiveBuilder
		c.ActiveBuilder = token.StringID(n.Data)
		defer func() { c.ActiveBuilder = prevBuilder }()
		bodyTy, err := c.inferNode(ast.NodeID(n.Left))
		if err != nil {
			return NoType, err
		}
		return c.recordNodeType(id, bodyTy), nil

	case ast.KindBindBang:
		rhsTy, err := c.inferNode(ast.NodeID(n.Left))
		if err != nil {
			return NoType, err
		}
		rRhs := c.Types.Find(rhsTy)
		elemTy := rhsTy
		switch c.Types.Nodes[rRhs].Kind {
		case TyOption, TyResult:
			elemTy = TypeID(c.Types.Nodes[rRhs].Left)
		}
		name := token.StringID(n.Data)
		if name != token.NoString {
			c.define(name, TypeScheme{Body: elemTy}, false)
		}
		if n.Right != 0 {
			return c.inferNode(ast.NodeID(n.Right))
		}
		return c.recordNodeType(id, TypeUnit), nil

	case ast.KindReturn:
		valTy, err := c.inferNode(ast.NodeID(n.Left))
		if err != nil {
			return NoType, err
		}
		if n.Flags&ast.FlagBang == 0 {
			switch c.AST.Strings.String(c.ActiveBuilder) {
			case "option":
				valTy = c.Types.Add(TypeNode{Kind: TyOption, Left: uint32(valTy)})
			case "result":
				valTy = c.Types.Add(TypeNode{Kind: TyResult, Left: uint32(valTy), Right: uint32(c.Types.FreshVar())})
			}
		}
		return c.recordNodeType(id, valTy), nil

	case ast.KindModule, ast.KindFunctor:
		return c.inferModuleOrFunctor(id, n)

	case ast.KindOpen:
		if n.Left != 0 {
			if _, err := c.inferNode(ast.NodeID(n.Left)); err != nil {
				return NoType, err
			}
		}
		modName := token.StringID(n.Data)
		if modDef, ok := c.Functors.Modules[modName]; ok {
			for sym, scheme := range modDef.Exports {
				c.define(sym, scheme, false)
			}
		}
		return c.recordNodeType(id, TypeUnit), nil

	case ast.KindTypeProvider:
		return c.inferTypeProvider(id, n)

	case ast.KindQuote:
		if _, err := c.inferNode(ast.NodeID(n.Left)); err != nil {
			return NoType, err
		}
		exprTy := c.NamedTypes[c.AST.Strings.InternString("Expr")]
		return c.recordNodeType(id, exprTy), nil

	case ast.KindTypeAnnot:
		exprTy, err := c.inferNode(ast.NodeID(n.Left))
		if err != nil {
			return NoType, err
		}
		annotTy := c.resolveTypeAST(ast.NodeID(n.Right))
		if err := c.Types.Unify(exprTy, annotTy); err != nil {
			return NoType, err
		}
		return c.recordNodeType(id, annotTy), nil

	case ast.KindAttr:
		if n.Left != 0 {
			return c.inferNode(ast.NodeID(n.Left))
		}
		return c.recordNodeType(id, TypeUnit), nil

	default:
		return c.recordNodeType(id, TypeUnit), nil
	}
}

func (c *Checker) inferTypeProvider(id ast.NodeID, n ast.Node) (TypeID, error) {
	typeName := token.StringID(n.Data)
	provNode := c.AST.Get(ast.NodeID(n.Left))
	provName := c.AST.Strings.String(token.StringID(provNode.Data))
	argNode := c.AST.Get(ast.NodeID(n.Right))
	argStr := c.AST.Strings.String(token.StringID(argNode.Data))

	switch provName {
	case "CsvProvider":
		return c.inferCsvTypeProvider(id, typeName, argStr)
	case "EnvProvider":
		return c.inferEnvTypeProvider(id, typeName, argStr)
	}

	rawJSON := strings.TrimSpace(argStr)
	if !strings.HasPrefix(rawJSON, "{") {
		path := argStr
		if c.BaseDir != "" && !filepath.IsAbs(path) {
			path = filepath.Join(c.BaseDir, path)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return NoType, fmt.Errorf("type provider %s: cannot read schema %q: %w",
				c.AST.Strings.String(typeName), argStr, err)
		}
		rawJSON = string(b)
	}

	dec := json.NewDecoder(strings.NewReader(rawJSON))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return NoType, fmt.Errorf("type provider %s: invalid JSON schema: %w",
			c.AST.Strings.String(typeName), err)
	}

	c.ProviderSchemas[typeName] = rawJSON
	recTy := c.synthesizeJSONRecord(typeName, obj)

	typeStr := c.AST.Strings.String(typeName)
	parseTy := c.Types.Add(TypeNode{Kind: TyArrow, Left: uint32(TypeString), Right: uint32(recTy)})
	c.Scopes[0][c.AST.Strings.InternString(typeStr+".Parse")] = Binding{Scheme: TypeScheme{Body: parseTy}}
	c.Scopes[0][c.AST.Strings.InternString(typeStr+".Load")] = Binding{Scheme: TypeScheme{Body: parseTy}}
	c.Scopes[0][c.AST.Strings.InternString(typeStr+".Sample")] = Binding{Scheme: TypeScheme{Body: recTy}}

	return c.recordNodeType(id, TypeUnit), nil
}

func (c *Checker) inferCsvTypeProvider(id ast.NodeID, typeName token.StringID, argStr string) (TypeID, error) {
	rawCSV := strings.TrimSpace(argStr)
	if !strings.Contains(rawCSV, ",") && !strings.Contains(rawCSV, "\n") {
		path := argStr
		if c.BaseDir != "" && !filepath.IsAbs(path) {
			path = filepath.Join(c.BaseDir, path)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return NoType, fmt.Errorf("CsvProvider %s: cannot read %q: %w", c.AST.Strings.String(typeName), argStr, err)
		}
		rawCSV = string(b)
	}
	records, err := csv.NewReader(strings.NewReader(strings.TrimSpace(rawCSV))).ReadAll()
	if err != nil || len(records) == 0 {
		return NoType, fmt.Errorf("CsvProvider %s: invalid or empty CSV schema", c.AST.Strings.String(typeName))
	}
	headers := records[0]
	var firstRow []string
	if len(records) > 1 {
		firstRow = records[1]
	}

	fNames := make([]token.StringID, len(headers))
	fTypes := make([]TypeID, len(headers))
	for i, h := range headers {
		col := strings.TrimSpace(h)
		fid := c.AST.Strings.InternString(col)
		fNames[i] = fid
		sampleCell := ""
		if i < len(firstRow) {
			sampleCell = strings.TrimSpace(firstRow[i])
		}
		fTypes[i] = inferScalarStringType(sampleCell)
		c.FieldToRec[fid] = typeName
	}
	start, count := c.Types.AddExtra(fTypes)
	rowTy := c.Types.Add(TypeNode{Kind: TyRecord, Name: typeName, Left: start, Right: count})
	for i, fName := range fNames {
		c.Types.SetRowField(rowTy, fName, fTypes[i])
	}
	c.Records[typeName] = RecordInfo{RecordType: rowTy, FieldNames: fNames, FieldTypes: fTypes}
	c.NamedTypes[typeName] = rowTy

	listTy := c.Types.Add(TypeNode{Kind: TyList, Left: uint32(rowTy)})
	typeStr := c.AST.Strings.String(typeName)
	parseTy := c.Types.Add(TypeNode{Kind: TyArrow, Left: uint32(TypeString), Right: uint32(listTy)})
	c.Scopes[0][c.AST.Strings.InternString(typeStr+".Parse")] = Binding{Scheme: TypeScheme{Body: parseTy}}
	c.Scopes[0][c.AST.Strings.InternString(typeStr+".Load")] = Binding{Scheme: TypeScheme{Body: parseTy}}
	c.Scopes[0][c.AST.Strings.InternString(typeStr+".Sample")] = Binding{Scheme: TypeScheme{Body: listTy}}
	return c.recordNodeType(id, TypeUnit), nil
}

func (c *Checker) inferEnvTypeProvider(id ast.NodeID, typeName token.StringID, argStr string) (TypeID, error) {
	rawEnv := strings.TrimSpace(argStr)
	if !strings.Contains(rawEnv, "=") {
		path := argStr
		if c.BaseDir != "" && !filepath.IsAbs(path) {
			path = filepath.Join(c.BaseDir, path)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return NoType, fmt.Errorf("EnvProvider %s: cannot read %q: %w", c.AST.Strings.String(typeName), argStr, err)
		}
		rawEnv = string(b)
	}
	var fNames []token.StringID
	var fTypes []TypeID
	for _, line := range strings.Split(rawEnv, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		idx := strings.IndexByte(trimmed, '=')
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:idx])
		valStr := strings.Trim(strings.TrimSpace(trimmed[idx+1:]), `"'`)
		fid := c.AST.Strings.InternString(key)
		fNames = append(fNames, fid)
		fTypes = append(fTypes, inferScalarStringType(valStr))
		c.FieldToRec[fid] = typeName
	}
	start, count := c.Types.AddExtra(fTypes)
	recTy := c.Types.Add(TypeNode{Kind: TyRecord, Name: typeName, Left: start, Right: count})
	for i, fName := range fNames {
		c.Types.SetRowField(recTy, fName, fTypes[i])
	}
	c.Records[typeName] = RecordInfo{RecordType: recTy, FieldNames: fNames, FieldTypes: fTypes}
	c.NamedTypes[typeName] = recTy

	typeStr := c.AST.Strings.String(typeName)
	parseTy := c.Types.Add(TypeNode{Kind: TyArrow, Left: uint32(TypeString), Right: uint32(recTy)})
	c.Scopes[0][c.AST.Strings.InternString(typeStr+".Parse")] = Binding{Scheme: TypeScheme{Body: parseTy}}
	c.Scopes[0][c.AST.Strings.InternString(typeStr+".Load")] = Binding{Scheme: TypeScheme{Body: parseTy}}
	c.Scopes[0][c.AST.Strings.InternString(typeStr+".Sample")] = Binding{Scheme: TypeScheme{Body: recTy}}
	return c.recordNodeType(id, TypeUnit), nil
}

func inferScalarStringType(s string) TypeID {
	if s == "true" || s == "false" {
		return TypeBool
	}
	if _, err := strconv.ParseInt(s, 10, 64); err == nil {
		return TypeInt
	}
	if strings.Contains(s, ".") {
		if _, err := strconv.ParseFloat(s, 64); err == nil {
			return TypeFloat
		}
	}
	return TypeString
}

func (c *Checker) synthesizeJSONRecord(recName token.StringID, obj map[string]any) TypeID {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	fNames := make([]token.StringID, len(keys))
	fTypes := make([]TypeID, len(keys))
	for i, k := range keys {
		fID := c.AST.Strings.InternString(k)
		fNames[i] = fID
		fTypes[i] = c.inferJSONValueType(c.AST.Strings.String(recName)+"_"+k, obj[k])
		c.FieldToRec[fID] = recName
	}
	start, count := c.Types.AddExtra(fTypes)
	recTy := c.Types.Add(TypeNode{Kind: TyRecord, Name: recName, Left: start, Right: count})
	for i, fName := range fNames {
		c.Types.SetRowField(recTy, fName, fTypes[i])
	}
	c.Records[recName] = RecordInfo{RecordType: recTy, FieldNames: fNames, FieldTypes: fTypes}
	c.NamedTypes[recName] = recTy
	return recTy
}

func (c *Checker) inferJSONValueType(prefix string, val any) TypeID {
	switch v := val.(type) {
	case json.Number:
		if strings.Contains(v.String(), ".") {
			return TypeFloat
		}
		return TypeInt
	case float64:
		return TypeFloat
	case string:
		return TypeString
	case bool:
		return TypeBool
	case []any:
		elemTy := TypeUnit
		if len(v) > 0 {
			elemTy = c.inferJSONValueType(prefix+"_elem", v[0])
		} else {
			elemTy = c.Types.FreshVar()
		}
		return c.Types.Add(TypeNode{Kind: TyList, Left: uint32(elemTy)})
	case map[string]any:
		subName := c.AST.Strings.InternString(prefix)
		return c.synthesizeJSONRecord(subName, v)
	default:
		return c.Types.FreshVar()
	}
}

func (c *Checker) inferLet(id ast.NodeID, n ast.Node) (TypeID, error) {
	name := token.StringID(n.Data)
	isMutable := n.Kind == ast.KindLetMutable || (n.Flags&ast.FlagMutable != 0)
	isRec := n.Kind == ast.KindLetRec || (n.Flags&ast.FlagRec != 0)

	if isRec && n.Right != 0 && c.AST.Get(ast.NodeID(n.Right)).Kind == ast.KindLetRec {
		type recItem struct {
			nodeID ast.NodeID
			name   token.StringID
			rhsID  ast.NodeID
			recVar TypeID
		}
		var group []recItem
		currID := id
		var tailInID ast.NodeID
		for currID != ast.NoNode {
			cn := *c.AST.Get(currID)
			if cn.Kind != ast.KindLetRec {
				tailInID = currID
				break
			}
			rv := c.Types.FreshVar()
			cName := token.StringID(cn.Data)
			c.define(cName, TypeScheme{Body: rv}, false)
			group = append(group, recItem{
				nodeID: currID,
				name:   cName,
				rhsID:  ast.NodeID(cn.Left),
				recVar: rv,
			})
			currID = ast.NodeID(cn.Right)
		}
		var lastTy TypeID
		for _, item := range group {
			rTy, err := c.inferNode(item.rhsID)
			if err != nil {
				return NoType, err
			}
			if err := c.Types.Unify(item.recVar, rTy); err != nil {
				return NoType, err
			}
			c.recordNodeType(item.nodeID, rTy)
			lastTy = rTy
		}
		for _, item := range group {
			resolved := c.Types.Find(item.recVar)
			if c.isSyntacticValue(item.rhsID) {
				c.define(item.name, c.Generalize(resolved), false)
			} else {
				c.define(item.name, TypeScheme{Body: resolved}, false)
			}
		}
		if tailInID != ast.NoNode {
			inTy, err := c.inferNode(tailInID)
			if err != nil {
				return NoType, err
			}
			return c.recordNodeType(id, inTy), nil
		}
		return c.recordNodeType(id, lastTy), nil
	}

	var recVar TypeID
	if isRec && name != token.NoString {
		recVar = c.Types.FreshVar()
		c.define(name, TypeScheme{Body: recVar}, false)
	}

	// Handle tuple pattern destructuring `let (a, b) = rhs`
	if n.Op == 1 {
		extra := c.AST.ExtraSlice(n.Left, 3)
		patID := ast.NodeID(extra[0])
		rhsID := ast.NodeID(extra[1])
		inID := ast.NodeID(extra[2])
		rhsTy, err := c.inferNode(rhsID)
		if err != nil {
			return NoType, err
		}
		if err := c.bindPattern(patID, rhsTy); err != nil {
			return NoType, err
		}
		if inID != ast.NoNode {
			return c.inferNode(inID)
		}
		return c.recordNodeType(id, rhsTy), nil
	}

	rhsID := ast.NodeID(n.Left)
	rhsTy, err := c.inferNode(rhsID)
	if err != nil {
		return NoType, err
	}

	if isRec && recVar != NoType {
		if err := c.Types.Unify(recVar, rhsTy); err != nil {
			return NoType, err
		}
	}

	// Enforce ML Value Restriction: only non-mutable syntactic values generalize!
	var scheme TypeScheme
	if !isMutable && c.isSyntacticValue(rhsID) {
		scheme = c.Generalize(rhsTy)
	} else {
		scheme = TypeScheme{Body: rhsTy}
	}
	c.define(name, scheme, isMutable)

	if n.Right != 0 {
		inTy, err := c.inferNode(ast.NodeID(n.Right))
		if err != nil {
			return NoType, err
		}
		return c.recordNodeType(id, inTy), nil
	}
	return c.recordNodeType(id, rhsTy), nil
}

func (c *Checker) inferInfix(id ast.NodeID, n ast.Node) (TypeID, error) {
	lTy, err := c.inferNode(ast.NodeID(n.Left))
	if err != nil {
		return NoType, err
	}
	rTy, err := c.inferNode(ast.NodeID(n.Right))
	if err != nil {
		return NoType, err
	}
	rl := c.Types.Find(lTy)
	rr := c.Types.Find(rTy)
	op := token.TokenKind(n.Op)

	switch op {
	case token.TokenPlus, token.TokenMinus, token.TokenPercent:
		// Both operands must unify (including identical Units of Measure!)
		if err := c.Types.Unify(lTy, rTy); err != nil {
			return NoType, err
		}
		resolved := c.Types.Find(lTy)
		if op == token.TokenPercent {
			if err := c.Types.Unify(resolved, TypeInt); err != nil {
				return NoType, err
			}
			resolved = TypeInt
		}
		return c.recordNodeType(id, resolved), nil

	case token.TokenStar, token.TokenSlash:
		// Check for Units of Measure multiplication/division: float<m1> * float<m2> -> float<m1*m2>
		nl := c.Types.Nodes[rl]
		nr := c.Types.Nodes[rr]
		if nl.Kind == TyMeasure || nr.Kind == TyMeasure {
			baseL := rl
			mL := uint32(0)
			if nl.Kind == TyMeasure {
				baseL = TypeID(nl.Left)
				mL = nl.Right
			}
			baseR := rr
			mR := uint32(0)
			if nr.Kind == TyMeasure {
				baseR = TypeID(nr.Left)
				mR = nr.Right
			}
			if err := c.Types.Unify(baseL, baseR); err != nil {
				return NoType, err
			}
			resMeasure := c.Types.MultiplyMeasures(mL, mR, op == token.TokenSlash)
			return c.recordNodeType(id, c.Types.MakeMeasureType(baseL, resMeasure)), nil
		}
		if err := c.Types.Unify(lTy, rTy); err != nil {
			return NoType, err
		}
		resolved := c.Types.Find(lTy)
		return c.recordNodeType(id, resolved), nil

	case token.TokenAssign, token.TokenEq, token.TokenNeq,
		token.TokenLt, token.TokenLe, token.TokenGt, token.TokenGe:
		if err := c.Types.Unify(lTy, rTy); err != nil {
			return NoType, err
		}
		return c.recordNodeType(id, TypeBool), nil

	case token.TokenAndAnd, token.TokenOrOr:
		if err := c.Types.Unify(lTy, TypeBool); err != nil {
			return NoType, err
		}
		if err := c.Types.Unify(rTy, TypeBool); err != nil {
			return NoType, err
		}
		return c.recordNodeType(id, TypeBool), nil

	case token.TokenCons:
		listTy := c.Types.Add(TypeNode{Kind: TyList, Left: uint32(lTy)})
		if err := c.Types.Unify(rTy, listTy); err != nil {
			return NoType, err
		}
		return c.recordNodeType(id, listTy), nil

	case token.TokenAppend:
		if err := c.Types.Unify(lTy, rTy); err != nil {
			return NoType, err
		}
		return c.recordNodeType(id, lTy), nil

	default:
		return c.recordNodeType(id, lTy), nil
	}
}

func (c *Checker) inferMatch(id ast.NodeID, n ast.Node) (TypeID, error) {
	scrutTy, err := c.inferNode(ast.NodeID(n.Left))
	if err != nil {
		return NoType, err
	}
	resTy := c.Types.FreshVar()
	armsBlock := c.AST.Get(ast.NodeID(n.Right))
	arms := c.AST.ExtraSlice(armsBlock.Left, armsBlock.Right)

	for _, armID := range arms {
		arm := c.AST.Get(ast.NodeID(armID))
		c.pushScope()
		if err := c.bindPattern(ast.NodeID(arm.Left), scrutTy); err != nil {
			c.popScope()
			return NoType, err
		}
		if arm.Data != 0 {
			guardNode := c.AST.Get(ast.NodeID(arm.Data))
			gTy, err := c.inferNode(ast.NodeID(guardNode.Left))
			if err != nil {
				c.popScope()
				return NoType, err
			}
			if err := c.Types.Unify(gTy, TypeBool); err != nil {
				c.popScope()
				return NoType, err
			}
		}
		bodyTy, err := c.inferNode(ast.NodeID(arm.Right))
		c.popScope()
		if err != nil {
			return NoType, err
		}
		if err := c.Types.Unify(resTy, bodyTy); err != nil {
			return NoType, err
		}
	}

	if err := c.CheckExhaustive(id, scrutTy); err != nil {
		return NoType, err
	}
	return c.recordNodeType(id, c.Types.Find(resTy)), nil
}

func (c *Checker) bindPattern(patID ast.NodeID, targetTy TypeID) error {
	if patID == ast.NoNode {
		return nil
	}
	p := *c.AST.Get(patID)
	switch p.Kind {
	case ast.KindPatWildcard:
		return nil
	case ast.KindPatVar:
		c.define(token.StringID(p.Data), TypeScheme{Body: targetTy}, false)
		return nil
	case ast.KindPatConst:
		if p.Left != 0 {
			litTy, err := c.inferNode(ast.NodeID(p.Left))
			if err != nil {
				return err
			}
			return c.Types.Unify(targetTy, litTy)
		}
		return c.Types.Unify(targetTy, TypeUnit)
	case ast.KindPatTuple:
		subs := c.AST.ExtraSlice(p.Left, p.Right)
		elemTys := make([]TypeID, len(subs))
		for i, sub := range subs {
			ev := c.Types.FreshVar()
			elemTys[i] = ev
			if err := c.bindPattern(ast.NodeID(sub), ev); err != nil {
				return err
			}
		}
		start, count := c.Types.AddExtra(elemTys)
		tupTy := c.Types.Add(TypeNode{Kind: TyTuple, Left: start, Right: count})
		return c.Types.Unify(targetTy, tupTy)
	case ast.KindPatCons:
		elemTy := c.Types.FreshVar()
		listTy := c.Types.Add(TypeNode{Kind: TyList, Left: uint32(elemTy)})
		if err := c.Types.Unify(targetTy, listTy); err != nil {
			return err
		}
		if err := c.bindPattern(ast.NodeID(p.Left), elemTy); err != nil {
			return err
		}
		return c.bindPattern(ast.NodeID(p.Right), listTy)
	case ast.KindPatList:
		elemTy := c.Types.FreshVar()
		listTy := c.Types.Add(TypeNode{Kind: TyList, Left: uint32(elemTy)})
		if err := c.Types.Unify(targetTy, listTy); err != nil {
			return err
		}
		subs := c.AST.ExtraSlice(p.Left, p.Right)
		for _, sub := range subs {
			if err := c.bindPattern(ast.NodeID(sub), elemTy); err != nil {
				return err
			}
		}
		return nil
	case ast.KindPatRecord:
		fields := c.AST.ExtraSlice(p.Left, p.Right)
		rootTy := c.Types.Find(targetTy)
		for _, fNodeID := range fields {
			fn := c.AST.Get(ast.NodeID(fNodeID))
			fName := token.StringID(fn.Data)
			subPatID := ast.NodeID(fn.Left)
			var fieldTy TypeID
			if c.Types.Nodes[rootTy].Kind == TyRecord {
				if existing, ok := c.Types.RowFields[rootTy][fName]; ok {
					fieldTy = existing
				} else if c.Types.Nodes[rootTy].Name != 0 {
					return fmt.Errorf("record %s has no field '%s'", c.Types.Format(rootTy), c.AST.Strings.String(fName))
				} else {
					fieldTy = c.Types.FreshVar()
					c.Types.SetRowField(rootTy, fName, fieldTy)
				}
			} else if c.Types.Nodes[rootTy].Kind == TyVar {
				if existing, ok := c.Types.RowFields[rootTy][fName]; ok {
					fieldTy = existing
				} else {
					fieldTy = c.Types.FreshVar()
					if rn, ok := c.FieldToRec[fName]; ok {
						recInfo := c.Records[rn]
						for i, dn := range recInfo.FieldNames {
							if dn == fName {
								_ = c.Types.Unify(fieldTy, recInfo.FieldTypes[i])
								break
							}
						}
					}
					c.Types.SetRowField(rootTy, fName, fieldTy)
				}
			} else {
				return fmt.Errorf("record pattern cannot match non-record type %s", c.Types.Format(rootTy))
			}
			if err := c.bindPattern(subPatID, fieldTy); err != nil {
				return err
			}
		}
		return nil
	case ast.KindPatDU:
		tagName := token.StringID(p.Data)
		tagStr := c.AST.Strings.String(tagName)
		if tagStr == "None" {
			elemTy := c.Types.FreshVar()
			return c.Types.Unify(targetTy, c.Types.Add(TypeNode{Kind: TyOption, Left: uint32(elemTy)}))
		}
		if tagStr == "Some" {
			elemTy := c.Types.FreshVar()
			if p.Left != 0 {
				if err := c.bindPattern(ast.NodeID(p.Left), elemTy); err != nil {
					return err
				}
			}
			return c.Types.Unify(targetTy, c.Types.Add(TypeNode{Kind: TyOption, Left: uint32(elemTy)}))
		}
		if _, isActive := c.ActivePats[tagName]; isActive {
			if inScheme, hasIn := c.ActivePatInputs[tagName]; hasIn {
				if err := c.Types.Unify(targetTy, c.Instantiate(inScheme)); err != nil {
					return err
				}
			}
			if p.Left != 0 {
				payTy := c.Types.FreshVar()
				if payScheme, hasPay := c.ActivePatPayloads[tagName]; hasPay {
					payTy = c.Instantiate(payScheme)
				}
				return c.bindPattern(ast.NodeID(p.Left), payTy)
			}
			return nil
		}
		if ctor, ok := c.Constructors[tagName]; ok {
			if err := c.Types.Unify(targetTy, ctor.ADTType); err != nil {
				return err
			}
			if p.Left != 0 && ctor.Payload != NoType {
				return c.bindPattern(ast.NodeID(p.Left), ctor.Payload)
			}
		}
		return nil
	default:
		return nil
	}
}

func (c *Checker) resolveTypeAST(nodeID ast.NodeID) TypeID {
	if nodeID == ast.NoNode {
		return TypeUnit
	}
	n := *c.AST.Get(nodeID)
	switch n.Kind {
	case ast.KindTypeIdent:
		name := token.StringID(n.Data)
		if ty, ok := c.NamedTypes[name]; ok {
			return ty
		}
		s := c.AST.Strings.String(name)
		if len(s) > 0 && s[0] == '\'' {
			tv := c.Types.FreshVar()
			c.NamedTypes[name] = tv
			return tv
		}
		return c.Types.Add(TypeNode{Kind: TyADT, Name: name})
	case ast.KindTypeArrow:
		l := c.resolveTypeAST(ast.NodeID(n.Left))
		r := c.resolveTypeAST(ast.NodeID(n.Right))
		return c.Types.Add(TypeNode{Kind: TyArrow, Left: uint32(l), Right: uint32(r)})
	case ast.KindTypeTuple:
		children := c.AST.ExtraSlice(n.Left, n.Right)
		elems := make([]TypeID, len(children))
		for i, ch := range children {
			elems[i] = c.resolveTypeAST(ast.NodeID(ch))
		}
		start, count := c.Types.AddExtra(elems)
		return c.Types.Add(TypeNode{Kind: TyTuple, Left: start, Right: count})
	case ast.KindTypeApp:
		baseTy := c.resolveTypeAST(ast.NodeID(n.Left))
		mID := c.Types.BuildMeasureFromAST(c.AST, ast.NodeID(n.Right))
		return c.Types.MakeMeasureType(baseTy, mID)
	default:
		return c.Types.FreshVar()
	}
}
