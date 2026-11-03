package stdlib

import (
	"testing"

	"fml/internal/ir"
	"fml/internal/parser"
	"fml/internal/runtime"
	"fml/internal/types"
)

func evalFML(t *testing.T, src string) runtime.Value {
	t.Helper()
	p, root, err := parser.ParseSource([]byte(src), nil)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	chk := types.NewChecker(p.Arena, nil)
	if _, err := chk.Infer(root); err != nil {
		t.Fatalf("infer failed: %v", err)
	}
	prog := ir.Lower(p.Arena, chk.Types, root)
	vm := runtime.NewVM(prog)
	RegisterAll(vm)
	val, err := vm.Run()
	if err != nil {
		t.Fatalf("runtime failed: %v", err)
	}
	return val
}

func TestListArrayStringOptionResultCombinators(t *testing.T) {
	src := `
let nums = [4; 1; 3; 2; 5]
let sortedEvens =
    nums
    |> List.sortBy (fun x -> x)
    |> List.filter (fun x -> x % 2 == 0)
    |> List.map (fun x -> x * 10)

let listFolded = List.fold (fun acc x -> acc + x) 0 sortedEvens

let arr = Array.ofList [10; 20; 30]
Array.set arr 1 50
let arrMapped = Array.map (fun x -> x + 1) arr
let arrVal = Array.get arrMapped 1

let csv = "  alpha,beta,gamma  " |> String.trim
let words = String.split "," csv
let upperWords = List.map String.toUpper words
let joined = String.join "-" upperWords
let strLen = String.length joined

let optVal =
    Some 12
    |> Option.map (fun x -> x + 3)
    |> Option.bind (fun x -> if x > 10 then Some (x * 2) else None)
    |> Option.defaultValue 0

let resVal =
    Ok 7
    |> Result.map (fun x -> x * 3)
    |> Result.defaultValue 0

listFolded + arrVal + strLen + optVal + resVal
`
	val := evalFML(t, src)
	// sortedEvens = [20; 40] -> listFolded = 60
	// arrVal = 50 + 1 = 51
	// joined = "ALPHA-BETA-GAMMA" (len 16)
	// optVal = (12 + 3) * 2 = 30
	// resVal = 21
	// total = 60 + 51 + 16 + 30 + 21 = 178
	if iv, ok := val.(runtime.IntVal); !ok || iv.V != 178 {
		t.Fatalf("expected 178, got %v", val)
	}
}

func TestAsyncParallelWorkflows(t *testing.T) {
	src := `
let computeSquare n = async {
    do! Async.Sleep 5
    return n * n
}

let wf = async {
    let! results = Async.Parallel [computeSquare 3; computeSquare 4; computeSquare 5]
    return List.sum results
}

Async.RunSynchronously wf
`
	val := evalFML(t, src)
	// 9 + 16 + 25 = 50
	if iv, ok := val.(runtime.IntVal); !ok || iv.V != 50 {
		t.Fatalf("expected 50 from concurrent Async.Parallel workflow, got %v", val)
	}
}

func TestRegexModule(t *testing.T) {
	src := `
let text = "id:101, id:202, id:303"
let hasDigits = Regex.isMatch "[0-9]+" text
let firstMatch = Regex.find "[0-9]+" text |> Option.defaultValue "0"
let allMatches = Regex.findAll "[0-9]+" text
let replaced = Regex.replace "[0-9]+" "X" text
let parts = Regex.split ", *" replaced

if hasDigits && firstMatch == "101" && (List.length allMatches == 3) && (List.length parts == 3) then
    String.length replaced
else
    0
`
	val := evalFML(t, src)
	// replaced = "id:X, id:X, id:X" -> length 16
	if iv, ok := val.(runtime.IntVal); !ok || iv.V != 16 {
		t.Fatalf("expected 16 from Regex operations, got %v", val)
	}
}

func TestMathAndTimeModules(t *testing.T) {
	src := `
let a = Math.abs (0 - 42)
let mn = Math.min 10 25
let mx = Math.max 10 25
let rt = Math.sqrt 81.0
let pw = Math.pow 2.0 5.0
let fl = Math.floor 3.9
let cl = Math.ceil 3.1
let rn = Math.round 3.6
let t0 = Time.nowUnixMs ()
let measured = Time.measureMs (fun () -> a + mn + mx)
let computed = fst measured
let elapsed = snd measured

if (t0 > 0) && (elapsed >= 0) && (rt == 9.0) && (pw == 32.0) && (fl == 3.0) && (cl == 4.0) && (rn == 4.0) && (Math.pi > 3.14) then
    computed
else
    0
`
	val := evalFML(t, src)
	// 42 + 10 + 25 = 77
	if iv, ok := val.(runtime.IntVal); !ok || iv.V != 77 {
		t.Fatalf("expected 77 from Math and Time operations, got %v", val)
	}
}



