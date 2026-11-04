package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fml/internal/runtime"
)

func TestEndToEndFMLProgram(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "integration.fml")

	program := `
[<Measure>] type kg
[<Measure>] type m
[<Measure>] type s

type Telemetry = Circle of int | Box of int * int

let (|High|Low|) v = if v >= 100 then High else Low

let rec sumRange lo hi =
    if lo > hi then 0 else lo + sumRange (lo + 1) hi

let mass = 10.0<kg>
let accel = 9.8<m/s^2>
let force = mass * accel

let m0 = Map.empty
let m1 = Map.add "base" (sumRange 1 10) m0
let baseVal = Map.find "base" m1

let seqTotal =
    Seq.init 5 (fun i -> i * 10)
    |> Seq.filter (fun x -> x >= 20)
    |> Seq.sum

let worker = Mailbox.Start (fun inbox ->
    let req = Mailbox.Receive inbox
    let v = fst req
    let reply = snd req
    reply (v + 5)
)

let actorResult = Mailbox.PostAndAsyncReply worker (baseVal + seqTotal)

let finalScore =
    match actorResult with
    | High -> actorResult * 2
    | Low -> actorResult

printfn "FML_OK"
finalScore
`
	if err := os.WriteFile(scriptPath, []byte(program), 0600); err != nil {
		t.Fatalf("failed to write test script: %v", err)
	}

	var outBuf bytes.Buffer
	val, err := ExecuteFile(scriptPath, &outBuf)
	if err != nil {
		t.Fatalf("ExecuteFile failed: %v", err)
	}

	// sumRange 1 10 = 55
	// seqTotal (20 + 30 + 40) = 90
	// actorResult = 55 + 90 + 5 = 150 (High -> 150 * 2 = 300)
	if iv, ok := val.(runtime.IntVal); !ok || iv.V != 300 {
		t.Fatalf("expected finalScore 300, got %v", val)
	}
	if !strings.Contains(outBuf.String(), "FML_OK") {
		t.Fatalf("expected stdout to contain FML_OK, got %q", outBuf.String())
	}
}

func TestCustomComputationExpressions(t *testing.T) {
	src := []byte(`
let findPositive x = if x > 0 then Some x else None

let optOk = option {
    let! a = findPositive 10
    let! b = findPositive 25
    return a + b
}

let optShortCircuit = option {
    let! a = findPositive 10
    let! b = findPositive (0 - 5)
    return a + b
}

type Builder = { Bind: int -> (int -> int) -> int; Return: int -> int; ReturnFrom: int -> int; Zero: unit -> int }
let doubled = { Bind = fun m f -> f (m * 2); Return = fun x -> x; ReturnFrom = fun x -> x; Zero = fun () -> 0 }

let customVal = doubled {
    let! x = 3
    let! y = 4
    return x + y
}

let base =
    match optOk with
    | Some v -> v
    | None -> 0

let sc =
    match optShortCircuit with
    | Some v -> v
    | None -> 100

base + sc + customVal
`)
	val, err := ExecuteSource(src, nil)
	if err != nil {
		t.Fatalf("ExecuteSource failed: %v", err)
	}
	// base = 35, sc = 100, customVal = (3*2) + (4*2) = 14 => total = 149
	if iv, ok := val.(runtime.IntVal); !ok || iv.V != 149 {
		t.Fatalf("expected 149, got %v", val)
	}
}

func TestMultiFileOpenModules(t *testing.T) {
	tmpDir := t.TempDir()
	libPath := filepath.Join(tmpDir, "math_utils.fml")
	mainPath := filepath.Join(tmpDir, "main.fml")

	libSrc := `
let square x = x * x
let cube x = x * x * x
`
	mainSrc := `
open MathUtils

let res = square 6 + cube 3
res
`
	if err := os.WriteFile(libPath, []byte(libSrc), 0600); err != nil {
		t.Fatalf("write lib failed: %v", err)
	}
	if err := os.WriteFile(mainPath, []byte(mainSrc), 0600); err != nil {
		t.Fatalf("write main failed: %v", err)
	}

	val, err := ExecuteFile(mainPath, nil)
	if err != nil {
		t.Fatalf("ExecuteFile multi-file open failed: %v", err)
	}
	// 36 + 27 = 63
	if iv, ok := val.(runtime.IntVal); !ok || iv.V != 63 {
		t.Fatalf("expected 63, got %v", val)
	}
}

func TestASTQuotationsAndJsonTypeProvider(t *testing.T) {
	tmpDir := t.TempDir()
	schemaPath := filepath.Join(tmpDir, "service.json")
	mainPath := filepath.Join(tmpDir, "provider_quote.fml")

	schemaJSON := `{"host": "localhost", "port": 8080, "workers": 4}`
	if err := os.WriteFile(schemaPath, []byte(schemaJSON), 0600); err != nil {
		t.Fatalf("write schema failed: %v", err)
	}

	mainSrc := `
type ServiceConfig = JsonProvider<"service.json">

let cfg = ServiceConfig.Sample
let live = ServiceConfig.Parse "{\"host\": \"prod\", \"port\": 9000, \"workers\": 8}"

let expr = <@ 20 + 22 @>
let str = Quote.toString expr
let evaluated = Quote.eval expr

let topOp =
    match expr with
    | QInfix (op, l, r) -> op
    | _ -> "?"

if topOp == "+" && str == "(20 + 22)" then
    cfg.port + live.workers + evaluated
else
    0
`
	if err := os.WriteFile(mainPath, []byte(mainSrc), 0600); err != nil {
		t.Fatalf("write main failed: %v", err)
	}

	val, err := ExecuteFile(mainPath, nil)
	if err != nil {
		t.Fatalf("ExecuteFile failed: %v", err)
	}
	// 8080 + 8 + 42 = 8130
	if iv, ok := val.(runtime.IntVal); !ok || iv.V != 8130 {
		t.Fatalf("expected 8130, got %v", val)
	}

	// Verify compile-time type safety on JsonProvider schema fields:
	// `cfg.host` is inferred as `string` from `service.json`, so `cfg.host + 10` must fail HM unification!
	badPath := filepath.Join(tmpDir, "bad_provider.fml")
	badSrc := `
type ServiceConfig = JsonProvider<"service.json">
let cfg = ServiceConfig.Sample
let bad = cfg.host + 10
`
	if err := os.WriteFile(badPath, []byte(badSrc), 0600); err != nil {
		t.Fatalf("write bad failed: %v", err)
	}
	if _, err := ExecuteFile(badPath, nil); err == nil {
		t.Fatalf("expected compile-time type error when adding int to string JSON schema field cfg.host")
	}
}

func TestRecordsRowPolyMutualRecAndPatterns(t *testing.T) {
	src := []byte(`
type Account = { Name: string; Balance: int }

let deposit amt acc = { acc with Balance = acc.Balance + amt }
let readBalance r = r.Balance

let a0 = { Name = "Alice"; Balance = 100 }
let a1 = deposit 50 a0

// Anonymous structural record with extra fields passed to row-polymorphic readBalance
let anonVault = { Balance = 250; Currency = "USD"; Tier = 3 }
let totalBal = readBalance a1 + readBalance anonVault

// Mutual recursion: let rec ... and ...
let rec isEven n =
    if n == 0 then true else isOdd (n - 1)
and isOdd n =
    if n == 0 then false else isEven (n - 1)

// Or-patterns and tuple parameter destructuring
let classify x =
    match x with
    | 1 | 2 | 3 -> 10
    | 4 | 5 -> 20
    | _ -> 0

let manhattan (x1, y1) (x2, y2) =
    (x2 - x1) + (y2 - y1)

let dist = manhattan (1, 2) (4, 6)
let c = classify 2 + classify 5
if isEven 10 && isOdd 7 then
    totalBal + dist + c
else
    0
`)
	val, err := ExecuteSource(src, nil)
	if err != nil {
		t.Fatalf("ExecuteSource failed: %v", err)
	}
	// totalBal (150 + 250 = 400) + dist (3 + 4 = 7) + c (10 + 20 = 30) = 437
	if iv, ok := val.(runtime.IntVal); !ok || iv.V != 437 {
		t.Fatalf("expected 437, got %v", val)
	}

	// Verify compile-time row-polymorphism error when required field is missing
	badRowSrc := []byte(`
let getBal r = r.Balance
let bad = getBal { Name = "NoBalance"; Age = 42 }
`)
	if _, err := ExecuteSource(badRowSrc, nil); err == nil {
		t.Fatalf("expected row-polymorphism compile error for missing field Balance")
	}
}

func TestRecordPatternsPartialActivePatternsAndSafePrim(t *testing.T) {
	src := []byte(`
type User = { Name: string; Score: int }

let (|HighScore|_|) n =
    if n >= 100 then Some (n * 2) else None

let evalUser u =
    match u with
    | { Name = "VIP"; Score = s } -> s + 1000
    | { Name = _; Score = s } ->
        match s with
        | HighScore bonus -> bonus
        | _ -> s

let u1 = { Name = "VIP"; Score = 50 }
let u2 = { Name = "Bob"; Score = 120 }
let u3 = { Name = "Dan"; Score = 30 }

// Verify dynamic Json.decode type mismatch raises a catchable error instead of panicking host
let recovered =
    try
        let dyn = Json.decode "\"not_an_int\""
        dyn + 10
    with
    | _ -> 99

// Verify pure lambda has zero unnecessary lexical capture overhead
let pureFn =
    let unusedLocal = 12345
    fun x -> x + 1

evalUser u1 + evalUser u2 + evalUser u3 + recovered + pureFn 0
`)
	val, err := ExecuteSource(src, nil)
	if err != nil {
		t.Fatalf("ExecuteSource failed: %v", err)
	}
	// evalUser u1 = 1050
	// evalUser u2 = 240
	// evalUser u3 = 30
	// recovered = 99
	// pureFn 0 = 1
	// total = 1420
	if iv, ok := val.(runtime.IntVal); !ok || iv.V != 1420 {
		t.Fatalf("expected 1420, got %v", val)
	}
}

func TestCsvProviderEnvProviderAndTestRunner(t *testing.T) {
	tmpDir := t.TempDir()
	csvPath := filepath.Join(tmpDir, "metrics.csv")
	envPath := filepath.Join(tmpDir, ".env")
	testFilePath := filepath.Join(tmpDir, "providers_test.fml")

	csvData := "service,latency,healthy\napi,12,true\ndb,30,true\n"
	envData := "# Server settings\nBASE_LATENCY=10\nREGION=\"us-east\"\n"

	if err := os.WriteFile(csvPath, []byte(csvData), 0600); err != nil {
		t.Fatalf("write csv failed: %v", err)
	}
	if err := os.WriteFile(envPath, []byte(envData), 0600); err != nil {
		t.Fatalf("write env failed: %v", err)
	}

	testSrc := `
type Metrics = CsvProvider<"metrics.csv">
type AppEnv = EnvProvider<".env">

let rows = Metrics.Sample
let env = AppEnv.Sample

let firstRow = List.head rows
let totalLatency =
    rows
    |> List.map (fun r -> r.latency)
    |> List.sum

Test.assert "first row is healthy" firstRow.healthy
Test.assertEq "region matches" "us-east" env.REGION
Test.assertEq "latency sum + base" 52 (totalLatency + env.BASE_LATENCY)
Test.forall "all latencies positive" rows (fun r -> r.latency > 0)
`
	if err := os.WriteFile(testFilePath, []byte(testSrc), 0600); err != nil {
		t.Fatalf("write test file failed: %v", err)
	}

	var out bytes.Buffer
	if err := runTestSuite(tmpDir, &out); err != nil {
		t.Fatalf("runTestSuite failed: %v\noutput: %s", err, out.String())
	}
	if !strings.Contains(out.String(), "PASS") {
		t.Fatalf("expected PASS in test runner output, got %q", out.String())
	}
}

func TestExamplesSuiteAndRunner(t *testing.T) {
	examplesDir := filepath.Join("..", "..", "examples")
	cases := []struct {
		file string
		want int64
	}{
		{file: "physics_units.fml", want: 500},
		{file: "actor_pipeline.fml", want: 430},
		{file: "type_providers_and_quotations.fml", want: 546},
	}

	for _, tc := range cases {
		var out bytes.Buffer
		path := filepath.Join(examplesDir, tc.file)
		val, err := ExecuteFile(path, &out)
		if err != nil {
			t.Fatalf("ExecuteFile(%s) failed: %v", tc.file, err)
		}
		iv, ok := val.(runtime.IntVal)
		if !ok || iv.V != tc.want {
			t.Fatalf("%s: expected IntVal(%d), got %v", tc.file, tc.want, val)
		}
	}

	var suiteOut bytes.Buffer
	if err := runTestSuite(examplesDir, &suiteOut); err != nil {
		t.Fatalf("runTestSuite(examples) failed: %v\noutput: %s", err, suiteOut.String())
	}
	if !strings.Contains(suiteOut.String(), "PASS") {
		t.Fatalf("expected PASS in examples test suite output, got %q", suiteOut.String())
	}
}

func BenchmarkExecutePhysicsUnitsExample(b *testing.B) {
	path := filepath.Join("..", "..", "examples", "physics_units.fml")
	var sink bytes.Buffer
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sink.Reset()
		if _, err := ExecuteFile(path, &sink); err != nil {
			b.Fatalf("ExecuteFile failed: %v", err)
		}
	}
}

func TestCLIFmtWriteRecursiveTestAndDoc(t *testing.T) {
	tmpDir := t.TempDir()
	nestedDir := filepath.Join(tmpDir, "sub", "pkg")
	if err := os.MkdirAll(nestedDir, 0755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}

	unformattedPath := filepath.Join(nestedDir, "math_test.fml")
	unformattedSrc := "let   x   =   10   +   32\nTest.assertEq \"forty-two\" 42 x\n"
	if err := os.WriteFile(unformattedPath, []byte(unformattedSrc), 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	var fmtOut bytes.Buffer
	if err := runFmt([]string{"-w", tmpDir}, &fmtOut); err != nil {
		t.Fatalf("runFmt -w failed: %v", err)
	}
	formattedBytes, err := os.ReadFile(unformattedPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if !strings.Contains(string(formattedBytes), "let x = 10 + 32\n") {
		t.Fatalf("expected in-place formatted file to normalize spacing, got:\n%s", string(formattedBytes))
	}

	var testOut bytes.Buffer
	if err := runTestSuite(tmpDir+"/...", &testOut); err != nil {
		t.Fatalf("recursive runTestSuite failed: %v", err)
	}
	if !strings.Contains(testOut.String(), "PASS") || !strings.Contains(testOut.String(), "ok\t1 test file(s) passed") {
		t.Fatalf("expected recursive PASS and summary line, got:\n%s", testOut.String())
	}

	var docOut bytes.Buffer
	if err := runDoc("Time", &docOut); err != nil {
		t.Fatalf("runDoc(Time) failed: %v", err)
	}
	for _, want := range []string{"val Time.nowMs :", "val Time.nowIso :", "val Time.parseIso :", "val Time.formatIso :"} {
		if !strings.Contains(docOut.String(), want) {
			t.Fatalf("expected %q in runDoc(Time) output, got:\n%s", want, docOut.String())
		}
	}
}




