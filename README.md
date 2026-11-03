# FML — Functional ML Compiler & Runtime in Go

**FML** is a statically typed, functional programming language combining **F# syntax** (significant whitespace / off-side rule, pipelines, active patterns, computation expressions, units of measure), **OCaml semantics** (flat Lambda-style IR, Wadler pretty-printing, persistent AVL collections), and **Standard ML's parametric module functors**, implemented in **zero-dependency Go** using strict data-oriented design.

---

## Key Architectural Principles

1. **Flat Indexed Arenas (`internal/token`, `internal/ast`):**
   * Zero pointer trees or dynamic interface dispatch in the syntax tree.
   * `token.Token` is packed to **16 bytes** (4 tokens per 64-byte CPU cache line).
   * `ast.Node` is packed to **24 bytes** in a contiguous `[]ast.Node` slice indexed by 32-bit `NodeID`s.
   * Identifiers and string literals are interned into a single contiguous `[]byte` arena (`StringTable`) using open-addressed FNV-1a hashing (`0 allocs/op` on existing symbols).
2. **Zero-Allocation Lexer & Off-Side Indentation Engine (`internal/lexer`):**
   * Scans raw `[]byte` buffers directly into `[]token.Token` with `0 B/op` and `0 allocs/op`.
   * Resolves F# significant whitespace via a fixed `[64]uint16` column indentation stack emitting synthetic `INDENT`, `DEDENT`, and `SEMI` tokens.
3. **Hindley-Milner Type Inference via Union-Find (`internal/types`):**
   * Implements Algorithm W over a flat `[]TypeNode` disjoint-set forest (20 bytes per node) with iterative path compression (`0 allocs/op`) and an iterative worklist-based `OccursIn` check.
   * Enforces the **ML Value Restriction** on `let mutable` bindings.
   * Performs compile-time **Units of Measure (`[<Measure>]`)** dimensional analysis (abelian group exponent normalization) and erases all unit metadata prior to IR lowering for zero runtime overhead.
   * Performs Maranget-style **pattern match exhaustiveness and redundancy verification** and static **SML Parametric Module Functor** instantiation.
4. **10-Opcode Flat IR & Decision-Tree Compiler (`internal/ir`, `internal/runtime`):**
   * Distills the entire language into **10 flat IR instructions** (`IRConst`, `IRVar`, `IRLet`, `IRFn`, `IRApp`, `IRSwitch`, `IRPrim`, `IRAlloc`, `IRLoad`, `IRStore`), each 24 bytes.
   * Compiles `match ... with` expressions into grouped `IRSwitch` decision trees and performs free-variable analysis for flat closure conversion.
   * Executes on a tail-call-optimized Virtual Machine (`runtime.VM`) using closed Go sum-type interfaces (`isValue()`).
5. **"Batteries-Included" Standard Library (`internal/stdlib`):**
   * Ships natively with persistent AVL `Map` and `Set` (`sync.Pool`-backed), lazy pull-based `Seq` generators, Wadler's `Format` pretty-printer, Erlang/F#-style `Mailbox` actors over Go goroutines and channels, and native `File`, `Json`, and `Http` bindings.
   * See [**STDLIB.md**](./STDLIB.md) for the complete Standard Library Reference Manual.

---

## Quick Start

### Prerequisites
* Go `1.24+` (zero external dependencies)

### Build the Standalone Binary

```bash
go build -o fml ./cmd/fml
```

### Run an FML Program

Create a file `hello.fml`:

```fsharp
[<Measure>] type kg
[<Measure>] type m
[<Measure>] type s

let (|Even|Odd|) n = if n % 2 == 0 then Even else Odd

let mass = 12.5<kg>
let accel = 9.8<m/s^2>
let force = mass * accel

let summary =
    Seq.init 6 (fun i -> i + 1)
    |> Seq.filter (fun n ->
        match n with
        | Even -> true
        | Odd -> false)
    |> Seq.map (fun n -> n * 10)
    |> Seq.sum

printfn "Force (N):"
printfn force
summary
```

Execute it with `./fml`:

```bash
./fml hello.fml
```

Output:
```text
Force (N):
122.5
120
```

---

## Language Tour

### 1. Bindings, Mutability & Scoped Disposal (`use`)
```fsharp
let x = 10                        // Immutable binding
let mutable counter = 0           // Mutable binding (monomorphic via Value Restriction)
counter <- counter + 1            // In-place mutation

let rec fact n =                  // Recursive function
    if n <= 1 then 1 else n * fact (n - 1)

let use content = File.openReader "config.json" // Automatically disposed at scope exit
```

### 2. Curried Functions, Pipelines & Composition
```fsharp
let add x y = x + y
let double x = x * 2

let forward = 10 |> add 5 |> double     // 30
let backward = double <| add 5 10       // 30
let transform = add 5 >> double         // Left-to-right composition
let result = transform 10               // 30
```

### 3. Algebraic Data Types (Discriminated Unions), Records & Options
```fsharp
type Person = { Name: string; Age: int }
type Shape = Circle of float | Rect of float * float | Point

let describe s =
    match s with
    | Circle 0.0 -> "degenerate circle"
    | Circle r when r > 100.0 -> "large circle"
    | Circle r -> "circle"
    | Rect (w, h) -> "rectangle"
    | Point -> "point"
```

### 4. Units of Measure (Zero Runtime Cost)
```fsharp
[<Measure>] type kg
[<Measure>] type m
[<Measure>] type s

let m1 = 5.0<kg>
let d = 20.0<m>
let t = 2.0<s>
let f = m1 * (d / (t * t))        // Inferred as float<kg*m*s^-2>
// let bad = m1 + d               // Compile-time error: unit-of-measure mismatch <kg> vs <m>
```

### 5. SML Parametric Module Functors & Multi-File `open`
```fsharp
// Automatically loads `math_utils.fml` from the same directory if present
open MathUtils

module IntOrd =
    let eq a b = a == b

module MakeSetChecker (Ord: COMPARABLE) =
    let isSame x y = Ord.eq x y

module IntChecker = MakeSetChecker(IntOrd)
let ok = IntChecker.isSame 42 42  // true
```

### 6. Custom Computation Expressions (`option`, `result` & Custom Monads)
```fsharp
let safeDiv a b = if b == 0 then Error "div_by_zero" else Ok (a / b)

let calc = result {
    let! x = safeDiv 100 5
    let! y = safeDiv x 2
    return y + 7
}
```

### 7. First-Class AST Quotations & Compile-Time Schema Type Providers (`JsonProvider`, `CsvProvider`, `EnvProvider`)
```fsharp
// Statically synthesizes record fields and types from JSON, CSV, and .env files at compile time
type ServiceConfig = JsonProvider<"service.json">
type Metrics       = CsvProvider<"metrics.csv">
type AppEnv        = EnvProvider<".env">

let cfg  = ServiceConfig.Sample
let rows = Metrics.Sample
let env  = AppEnv.Sample

// Reifies FML syntax into the inspectable `Expr` ADT (`QInfix`, `QInt`, `QVar`, ...)
let expr = <@ 20 + 22 @>
let code = Quote.toString expr    // "(20 + 22)"
let ans  = Quote.eval expr        // 42
```

### 8. Ahead-of-Time (AOT) Native Binary Compilation, Formatter, LSP, Test Runner & REPL
```bash
# Compile an FML program into a standalone native executable binary (zero VM.Eval fallback)
./fml build program.fml -o program_bin
./program_bin

# Statically type-check and print inferred top-level signatures without executing
./fml check program.fml

# Format an FML source file using the Wadler algebraic layout engine
./fml fmt program.fml

# Run all *_test.fml unit & property test suites in a directory
./fml test .

# Start the JSON-RPC 2.0 Language Server Protocol (hover & real-time diagnostics)
./fml lsp

# Launch the interactive REPL (:type, :ir, :quote, :fmt, :help)
./fml repl
```

---

## Testing & Benchmarks

Run the full test suite with the Go race detector enabled:

```bash
go vet ./...
go test -race -v ./...
```

Run zero-allocation lexer and string interner benchmarks:

```bash
go test -bench=. -benchmem ./internal/token ./internal/lexer
```

---

## Repository Structure

```text
.
├── cmd/fml/                 # Standalone CLI entry point (`fml run`, `fml build`, `fml emit-go`, `fml check`, `fml repl`)
├── internal/
│   ├── token/               # 16-byte Token definitions and contiguous []byte StringTable interner
│   ├── ast/                 # 24-byte indexed AST Node and flat Arena allocator
│   ├── lexer/               # Zero-allocation state-machine lexer and off-side indentation stack
│   ├── parser/              # Recursive descent, Pratt expression parser & multi-file module loader
│   ├── types/               # Union-Find TypeArena, Algorithm W, Units of Measure, Functors, JsonProvider, Diagnostics
│   ├── ir/                  # 10-opcode flat IR, AST lowering, IRSwitch decision trees, closure conversion
│   ├── codegen/             # Ahead-of-Time (AOT) Go jump-table emitter, disassembler, and binary builder
│   ├── repl/                # Interactive REPL session, :type/:ir/:quote inspectors, and static file checker
│   ├── runtime/             # Closed Value sum-type interface and tail-call-optimized VM
│   └── stdlib/              # Map, Set, List, Array, String, Option, Result, Seq, Format, Mailbox, Async, File, Json, Http, Quote
├── STDLIB.md                # User-facing Standard Library Reference Manual
├── f_language_elements.md   # F# Core Language Elements Reference
├── PLAN.md                  # BABOK Requirements Synthesis & PMBOK Work Breakdown Structure
└── AGENTS.md                # Repository engineering and token-economy constraints
```
