package parser

import (
	"testing"

	"fml/internal/ast"
)

func TestParseControlModulesAndMetaprogramming(t *testing.T) {
	src := []byte(`
namespace MyCompany.Project
module MyModule
open System.Collections.Generic

let b = if x > 0 then "positive" else "non-positive"
for x in xs do printfn "%d" x
for i = 1 to 10 do step i
while x > 0 do x <- x - 1
try riskyOp () with | :? System.Exception -> 0
try work () finally cleanup ()

type MyClass(x: int) = member _.X = x
type IPrintable = abstract member Print: unit -> unit
let obj = { new IPrintable with member _.Print() = () }
type System.Int32 with member this.IsEven = this % 2 == 0

let wf = async {
    let! x = fetch ()
    do! log x
    use! r = acquire ()
    return x
}

module MakeSet (Ord: COMPARABLE) =
    let empty = []

let q = <@ 1 + 1 @>
type DB = SqlDataProvider<...>
[<CLIMutable>]
type Row = { Id: int }
`)
	p, root, err := ParseSource(src, nil)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	prog := p.Arena.Get(root)
	items := p.Arena.ExtraSlice(prog.Left, prog.Right)
	if len(items) < 16 {
		t.Fatalf("expected at least 16 top-level constructs, got %d", len(items))
	}

	seen := make(map[ast.NodeKind]bool)
	for i := 1; i < len(p.Arena.Nodes); i++ {
		seen[p.Arena.Nodes[i].Kind] = true
	}

	required := []ast.NodeKind{
		ast.KindNamespace,
		ast.KindModule,
		ast.KindOpen,
		ast.KindIf,
		ast.KindForIn,
		ast.KindForTo,
		ast.KindWhile,
		ast.KindTryWith,
		ast.KindTryFinally,
		ast.KindClass,
		ast.KindInterface,
		ast.KindObjExpr,
		ast.KindTypeExt,
		ast.KindCompExpr,
		ast.KindBindBang,
		ast.KindReturn,
		ast.KindFunctor,
		ast.KindQuote,
		ast.KindTypeProvider,
		ast.KindAttr,
	}
	for _, k := range required {
		if !seen[k] {
			t.Fatalf("missing expected AST NodeKind %v in parsed arena", k)
		}
	}
}
