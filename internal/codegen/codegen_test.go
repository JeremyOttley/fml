package codegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAOTBuildAndRunNativeBinary(t *testing.T) {
	tmpDir := t.TempDir()
	srcPath := filepath.Join(tmpDir, "aot_app.fml")
	binPath := filepath.Join(tmpDir, "aot_app_bin")

	src := `
[<Measure>] type km
[<Measure>] type hr

let speed (d: float<km>) (t: float<hr>) = d / t

let safeDiv a b =
    if b == 0 then Error "div_by_zero" else Ok (a / b)

let calc = result {
    let! x = safeDiv 100 5
    let! y = safeDiv x 2
    return y + 7
}

let q = <@ 10 + 32 @>
let qVal = Quote.eval q

match calc with
| Ok v -> printfn (v + qVal)
| Error e -> printfn e
`
	if err := os.WriteFile(srcPath, []byte(src), 0600); err != nil {
		t.Fatalf("write source failed: %v", err)
	}

	if err := BuildBinary(srcPath, binPath); err != nil {
		t.Fatalf("BuildBinary failed: %v", err)
	}

	info, err := os.Stat(binPath)
	if err != nil || info.Size() == 0 {
		t.Fatalf("expected non-empty compiled binary at %s", binPath)
	}

	out, err := exec.Command(binPath).CombinedOutput()
	if err != nil {
		t.Fatalf("running compiled binary failed: %v\noutput: %s", err, string(out))
	}

	// calc = Ok ((100 / 5) / 2 + 7) = Ok 17; qVal = 42; 17 + 42 = 59
	got := strings.TrimSpace(string(out))
	if got != "59" {
		t.Fatalf("expected native binary output '59', got %q", got)
	}
}

func TestAOTZeroVMEvalFallbackAndFullOpcodeCoverage(t *testing.T) {
	tmpDir := t.TempDir()
	srcPath := filepath.Join(tmpDir, "full_aot.fml")
	binPath := filepath.Join(tmpDir, "full_aot_bin")

	src := `
type Shape = Circle of int | Rect of int * int | Point

let area s =
    match s with
    | Circle r when r > 0 -> r * r * 3
    | Circle _ -> 0
    | Rect (w, h) -> w * h
    | Point -> 1

let rec sumDown n acc =
    if n <= 0 then acc
    else sumDown (n - 1) (acc + n)

let makeAdder base =
    fun x -> x + base

let add10 = makeAdder 10

let mutable mutTotal = 0
for x in [Circle 2; Rect (4, 5); Point] do
    mutTotal <- mutTotal + (area x)

type Config = { Base: int; Scale: int }
let c0 = { Base = mutTotal; Scale = 2 }
let c1 = { c0 with Base = c0.Base + 7 }

let mapped = List.map add10 [1; 2; 3]
let listSum = List.sum mapped

let actor = Mailbox.Start (fun inbox ->
    let req = Mailbox.Receive inbox
    let v = fst req
    let reply = snd req
    reply (v + c1.Base)
)
let actorRes = Mailbox.PostAndAsyncReply actor listSum

let tcoRes = sumDown 1000 0
printfn (actorRes + tcoRes)
`
	if err := os.WriteFile(srcPath, []byte(src), 0600); err != nil {
		t.Fatalf("write source failed: %v", err)
	}

	prog, err := CompileFile(srcPath)
	if err != nil {
		t.Fatalf("CompileFile failed: %v", err)
	}
	goSrcBytes, err := EmitGoSource(prog)
	if err != nil {
		t.Fatalf("EmitGoSource failed: %v", err)
	}
	goSrc := string(goSrcBytes)

	if strings.Contains(goSrc, "vm.Eval(") {
		t.Fatalf("expected zero vm.Eval fallback calls in AOT Go source, got:\n%s", goSrc)
	}
	if strings.Contains(goSrc, "[]ir.IRInstr") {
		t.Fatalf("expected zero interpreted IRInstr slice in AOT Go source, got:\n%s", goSrc)
	}

	if err := BuildBinary(srcPath, binPath); err != nil {
		t.Fatalf("BuildBinary failed: %v", err)
	}

	out, err := exec.Command(binPath).CombinedOutput()
	if err != nil {
		t.Fatalf("running compiled binary failed: %v\noutput: %s", err, string(out))
	}

	// mutTotal = (2*2*3) + (4*5) + 1 = 12 + 20 + 1 = 33
	// c1.Base = 33 + 7 = 40
	// listSum = 11 + 12 + 13 = 36
	// actorRes = 36 + 40 = 76
	// tcoRes = 1000 * 1001 / 2 = 500500
	// total = 500576
	got := strings.TrimSpace(string(out))
	if got != "500576" {
		t.Fatalf("expected native binary output '500576', got %q", got)
	}
}

func TestIncrementalCompilationQueryCache(t *testing.T) {
	InvalidateCache()
	tmpDir := t.TempDir()
	srcPath := filepath.Join(tmpDir, "cached_mod.fml")

	if err := os.WriteFile(srcPath, []byte("let x = 20 + 22\nx\n"), 0600); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	if _, err := CompileFile(srcPath); err != nil {
		t.Fatalf("first CompileFile failed: %v", err)
	}
	if hits := CacheHits(srcPath); hits != 0 {
		t.Fatalf("expected 0 cache hits on initial compile, got %d", hits)
	}

	// Second compilation of unmodified file must hit DefaultCache
	if _, err := CompileFile(srcPath); err != nil {
		t.Fatalf("second CompileFile failed: %v", err)
	}
	if hits := CacheHits(srcPath); hits != 1 {
		t.Fatalf("expected 1 cache hit on unchanged file, got %d", hits)
	}

	// Modifying source bytes must invalidate digest and recompile cleanly
	if err := os.WriteFile(srcPath, []byte("let x = 100\nx\n"), 0600); err != nil {
		t.Fatalf("write update failed: %v", err)
	}
	if _, err := CompileFile(srcPath); err != nil {
		t.Fatalf("re-compile after edit failed: %v", err)
	}
	if hits := CacheHits(srcPath); hits != 0 {
		t.Fatalf("expected cache hit counter reset to 0 after content change, got %d", hits)
	}
}

func TestAOTSelfRecursiveMultiArgTailCallLoopLowering(t *testing.T) {
	src := `
let rec sumRange (lo: int) (hi: int) (acc: int) =
    if lo > hi then acc
    else sumRange (lo + 1) hi (acc + lo)

let rec gcd (a: int) (b: int) =
    if b == 0 then a
    else gcd b (a % b)

let s = sumRange 1 10000 0
let g = gcd 1071 462
s + g
`
	tmpDir := t.TempDir()
	srcPath := filepath.Join(tmpDir, "tco_multi.fml")
	binPath := filepath.Join(tmpDir, "tco_multi_bin")
	if err := os.WriteFile(srcPath, []byte(src), 0600); err != nil {
		t.Fatalf("write source failed: %v", err)
	}

	prog, err := CompileFile(srcPath)
	if err != nil {
		t.Fatalf("CompileFile failed: %v", err)
	}
	goSrcBytes, err := EmitGoSource(prog)
	if err != nil {
		t.Fatalf("EmitGoSource failed: %v", err)
	}
	goSrc := string(goSrcBytes)
	if !strings.Contains(goSrc, "Direct self-recursive tail-call loop lowering") {
		t.Fatalf("expected AOT self-recursive tail-call loop lowering in generated Go source, got:\n%s", goSrc)
	}

	if err := BuildBinary(srcPath, binPath); err != nil {
		t.Fatalf("BuildBinary failed: %v", err)
	}
	out, err := exec.Command(binPath).CombinedOutput()
	if err != nil {
		t.Fatalf("running compiled binary failed: %v\noutput: %s", err, string(out))
	}
	// sumRange 1 10000 0 = 50005000, gcd 1071 462 = 21 -> 50005021
	got := strings.TrimSpace(string(out))
	if got != "50005021" {
		t.Fatalf("expected '50005021', got %q", got)
	}
}

func TestAOTUnboxedScalarTypeSpecialization(t *testing.T) {
	src := `
[<Measure>] type kg
[<Measure>] type m
[<Measure>] type s

let kineticEnergy (mass: float<kg>) (vel: float<m/s>) =
    0.5 * mass * (vel * vel)

let avgForce (energy: float<kg*m^2/s^2>) (dist: float<m>) =
    energy / dist

let rec fibIter (n: int) (a: int) (b: int) =
    if n <= 0 then a
    else fibIter (n - 1) b (a + b)

let mutable countdown = 5
let mutable accum = 0
while countdown > 0 do
    accum <- accum + (countdown * 2)
    countdown <- countdown - 1

let ke = kineticEnergy 4.0<kg> 10.0<m/s>
let f = avgForce ke 8.0<m>
let fib10 = fibIter 10 0 1
printfn f
printfn (fib10 + accum)
`
	tmpDir := t.TempDir()
	srcPath := filepath.Join(tmpDir, "scalar_spec.fml")
	binPath := filepath.Join(tmpDir, "scalar_spec_bin")
	if err := os.WriteFile(srcPath, []byte(src), 0600); err != nil {
		t.Fatalf("write source failed: %v", err)
	}

	prog, err := CompileFile(srcPath)
	if err != nil {
		t.Fatalf("CompileFile failed: %v", err)
	}
	goSrcBytes, err := EmitGoSource(prog)
	if err != nil {
		t.Fatalf("EmitGoSource failed: %v", err)
	}
	goSrc := string(goSrcBytes)

	requiredMarkers := []string{
		"Unboxed int64 specialization",
		"Unboxed float64 specialization",
		"Unboxed float64 division specialization",
		"Unboxed boolean branch specialization",
		"Unboxed while-loop condition specialization",
		"aotLoadInt(vm, env,",
		"aotLoadFloat(vm, env,",
	}
	for _, marker := range requiredMarkers {
		if !strings.Contains(goSrc, marker) {
			t.Fatalf("expected %q in AOT Go source, got:\n%s", marker, goSrc)
		}
	}

	if err := BuildBinary(srcPath, binPath); err != nil {
		t.Fatalf("BuildBinary failed: %v", err)
	}
	out, err := exec.Command(binPath).CombinedOutput()
	if err != nil {
		t.Fatalf("running compiled binary failed: %v\noutput: %s", err, string(out))
	}
	// ke = 0.5 * 4 * 100 = 200.0; f = 200.0 / 8.0 = 25
	// fib10 = 55; accum = 10 + 8 + 6 + 4 + 2 = 30 -> 55 + 30 = 85
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 || strings.TrimSpace(lines[0]) != "25" || strings.TrimSpace(lines[1]) != "85" {
		t.Fatalf("expected output lines ['25', '85'], got %v", lines)
	}
}


