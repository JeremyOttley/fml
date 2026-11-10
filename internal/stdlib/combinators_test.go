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

func TestSysAndCryptoModules(t *testing.T) {
	src := `
Sys.setenv "FML_TEST_ENV_KEY" "fml_secret_42"
let envOpt = Sys.getenv "FML_TEST_ENV_KEY"
let envVal = Option.defaultValue "" envOpt
let missingOpt = Sys.getenv "FML_NONEXISTENT_KEY_99999"
let isMissing = Option.isNone missingOpt

let wd = Sys.cwd ()
let args = Sys.args ()
let exists = Sys.fileExists "combinators.go"

let hash = Crypto.sha256 "fml"
let mac = Crypto.hmacSha256 "key" "message"
let b64 = Crypto.base64Encode "hello-fml"
let decoded = Crypto.base64Decode b64 |> Result.defaultValue ""
let badDecode = Crypto.base64Decode "!!!not_base64!!!"
let isDecodeErr = Result.isError badDecode
let id1 = Crypto.uuid4 ()
let id2 = Crypto.uuid4 ()

if (envVal == "fml_secret_42") && isMissing && (String.length wd > 0) && (List.length args >= 1) && exists && (String.length hash == 64) && (String.length mac == 64) && (decoded == "hello-fml") && isDecodeErr && (String.length id1 == 36) && (id1 != id2) then
    100
else
    0
`
	val := evalFML(t, src)
	if iv, ok := val.(runtime.IntVal); !ok || iv.V != 100 {
		t.Fatalf("expected 100 from Sys and Crypto module operations, got %v", val)
	}
}

func TestRegexGroupsAndTimeIsoCombinators(t *testing.T) {
	src := `
let grpOpt = Regex.groups "([a-z]+)-([0-9]+)" "release: ver-42!"
let grps = Option.defaultValue [] grpOpt
let noGrp = Regex.groups "([0-9]+)" "abc"

let ms = Time.nowMs ()
let nowIso = Time.nowIso ()
let parsedMs = Time.parseIso "2025-01-01T00:00:00Z" |> Result.defaultValue 0
let formatted = Time.formatIso parsedMs
let badIso = Time.parseIso "not-a-timestamp"

if (List.length grps == 2) && (List.head grps == "ver") && (Option.isNone noGrp) && (ms > 0) && (String.length nowIso >= 20) && (formatted == "2025-01-01T00:00:00Z") && (Result.isError badIso) then
    parsedMs
else
    0
`
	val := evalFML(t, src)
	if iv, ok := val.(runtime.IntVal); !ok || iv.V != 1735689600000 {
		t.Fatalf("expected 1735689600000 from Regex.groups and Time ISO operations, got %v", val)
	}
}

func TestListAndArrayExtendedCombinators(t *testing.T) {
	src := `
let nums = [1; 2; 3; 4; 5]
let found = List.tryFind (fun x -> x > 3) nums |> Option.defaultValue 0
let missing = List.tryFind (fun x -> x > 10) nums
let hasEven = List.exists (fun x -> x == 4) nums
let allPos = List.forall (fun x -> x > 0) nums
let zipped = List.zip [1; 2] [10; 20]
let (evens, odds) = List.partition (fun x -> (x % 2) == 0) [1; 2; 3; 4]
let chosen = List.choose (fun x -> if x > 2 then Some (x * 10) else None) [1; 2; 3; 4]
let collected = List.collect (fun x -> [x; x * 10]) [1; 2]

let arr = [| 10; 20; 30; 40 |]
let arrFiltered = Array.filter (fun x -> x >= 20) arr
let arrSum = Array.fold (fun acc x -> acc + x) 0 arrFiltered

if (Option.isNone missing) && hasEven && allPos && (List.length zipped == 2) then
    found + List.sum evens + List.sum odds + List.sum chosen + List.sum collected + arrSum
else
    0
`
	val := evalFML(t, src)
	// found = 4
	// evens = [2; 4] -> 6; odds = [1; 3] -> 4
	// chosen = [30; 40] -> 70
	// collected = [1; 10; 2; 20] -> 33
	// arrSum = 20 + 30 + 40 = 90
	// Total = 4 + 6 + 4 + 70 + 33 + 90 = 207
	if iv, ok := val.(runtime.IntVal); !ok || iv.V != 207 {
		t.Fatalf("expected 207 from extended List/Array combinators, got %v", val)
	}
}

func TestStringAndCryptoExtendedCombinators(t *testing.T) {
	src := `
let raw = "fml_compiler_engine"
let replaced = raw |> String.replace "compiler" "native"
let sub = replaced |> String.substring 4 6
let idx = replaced |> String.indexOf "engine"
let hasPre = replaced |> String.startsWith "fml_"
let hasSuf = replaced |> String.endsWith "_engine"

let randHex = Crypto.randomBytesHex 16
let randLen = String.length randHex
let eqOk = Crypto.constantTimeEq "secret_token" "secret_token"
let eqBad = Crypto.constantTimeEq "secret_token" "wrong_token"

if hasPre && hasSuf && sub == "native" && eqOk && (not eqBad) then
    idx + randLen
else
    -1
`
	val := evalFML(t, src)
	// idx = 11 ("fml_native_engine" -> "engine" at index 11), randLen = 32 (16 bytes hex) -> 43
	if iv, ok := val.(runtime.IntVal); !ok || iv.V != 43 {
		t.Fatalf("expected 43 from extended String/Crypto combinators, got %v", val)
	}
}


