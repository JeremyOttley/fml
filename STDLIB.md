# FML Standard Library Manual (`stdlib`)

FML follows a **"batteries-included"** standard library philosophy inspired by OCaml's `Stdlib`, Standard ML's Basis Library, and `FSharp.Core`. Every module documented below is pre-registered in the global environment of the `fml` binary with zero external dependencies.

---

## Table of Contents

1. [Core Prelude Functions](#1-core-prelude-functions)
2. [`Map` — Persistent AVL Key-Value Maps](#2-map--persistent-avl-key-value-maps)
3. [`Set` — Persistent AVL Ordered Sets](#3-set--persistent-avl-ordered-sets)
4. [`Seq` — Lazy Pull-Based Sequences](#4-seq--lazy-pull-based-sequences)
5. [`Format` — Wadler Pretty-Printing Engine](#5-format--wadler-pretty-printing-engine)
6. [`Mailbox` — Erlang/F# Actor Concurrency](#6-mailbox--erlangf-actor-concurrency)
7. [`File` — File System I/O & Scoped Resources](#7-file--file-system-io--scoped-resources)
8. [`Json` — Native JSON Serialization](#8-json--native-json-serialization)
9. [`Http` — Native HTTP Client](#9-http--native-http-client)

---

## 1. Core Prelude Functions

The following functions and constructors are available unqualified in every FML program:

| Function / Constructor | Type Signature | Description |
| :--- | :--- | :--- |
| `id` | `'a -> 'a` | Identity function; returns its argument unchanged. |
| `ignore` | `'a -> unit` | Discards the passed value and returns `()`. |
| `fst` | `'a * 'b -> 'a` | Extracts the first element of a 2-tuple. |
| `snd` | `'a * 'b -> 'b` | Extracts the second element of a 2-tuple. |
| `print` | `'a -> unit` | Formats and writes a value to standard output without a trailing newline. |
| `printfn` | `'a -> unit` | Writes a value (or format string `"%d"`/`"%s"`/`"%v"` + argument) to standard output with a newline. |
| `Some` / `None` | `'a -> 'a option` / `'a option` | Built-in `Option` constructors. |
| `Ok` / `Error` | `'a -> Result<'a, 'e>` / `'e -> Result<'a, 'e>` | Built-in `Result` constructors. |

### Example
```fsharp
let pair = (42, "answer")
let n = fst pair
let label = snd pair
printfn label
printfn "%d" n
```

---

## 2. `Map` — Persistent AVL Key-Value Maps

**Implementation:** [`internal/stdlib/collections.go`](file:///home/jottley/.projects/go/fml/internal/stdlib/collections.go)

`Map` is an immutable, height-balanced AVL binary search tree backed by Go's `sync.Pool` for internal node recycling. Inserting a key returns a new map root sharing unmodified subtrees with previous versions in $O(\log N)$ time while keeping all prior map bindings intact.

### Functions

| Symbol | Signature | Description |
| :--- | :--- | :--- |
| `Map.empty` | `Map<'k, 'v>` | The empty persistent map. |
| `Map.add` | `'k -> 'v -> Map<'k, 'v> -> Map<'k, 'v>` | Returns a new map with `(key, value)` inserted or updated. |
| `Map.remove` | `'k -> Map<'k, 'v> -> Map<'k, 'v>` | Returns a new map with `key` removed in $O(\log N)$ time. |
| `Map.find` | `'k -> Map<'k, 'v> -> 'v` | Looks up `key` in $O(\log N)$ time; raises a runtime error if absent. |
| `Map.tryFind` | `'k -> Map<'k, 'v> -> 'v option` | Looks up `key` and returns `Some value` if present, or `None` if absent. |
| `Map.containsKey` | `'k -> Map<'k, 'v> -> bool` | Returns `true` if `key` exists in the map, `false` otherwise. |
| `Map.count` | `Map<'k, 'v> -> int` | Returns the total number of key-value pairs in the map. |
| `Map.keys` | `Map<'k, 'v> -> 'k list` | Returns all keys in ascending order as a list. |
| `Map.values` | `Map<'k, 'v> -> 'v list` | Returns all values ordered by ascending key as a list. |

### Example
```fsharp
let m0 = Map.empty
let m1 = Map.add "alice" 100 m0
let m2 = Map.add "bob" 200 m1
let m1Updated = Map.add "alice" 999 m2

// m1 is completely unaffected by m1Updated (persistence invariant)
let originalAlice = Map.find "alice" m1          // 100
let updatedAlice  = Map.find "alice" m1Updated   // 999

let lookupUnknown =
    match Map.tryFind "charlie" m2 with
    | Some score -> score
    | None -> 0
```

---

## 3. `Set` — Persistent AVL Ordered Sets

**Implementation:** [`internal/stdlib/collections.go`](file:///home/jottley/.projects/go/fml/internal/stdlib/collections.go)

`Set` is an immutable, height-balanced AVL tree storing unique ordered elements in $O(\log N)$ time.

### Functions

| Symbol | Signature | Description |
| :--- | :--- | :--- |
| `Set.empty` | `Set<'a>` | The empty persistent set. |
| `Set.add` | `'a -> Set<'a> -> Set<'a>` | Returns a new set containing the element. |
| `Set.remove` | `'a -> Set<'a> -> Set<'a>` | Returns a new set with the element removed in $O(\log N)$ time. |
| `Set.contains` | `'a -> Set<'a> -> bool` | Returns `true` if the element is a member of the set. |
| `Set.count` | `Set<'a> -> int` | Returns the cardinality of the set. |
| `Set.union` | `Set<'a> -> Set<'a> -> Set<'a>` | Returns the set union of two sets. |
| `Set.intersect` | `Set<'a> -> Set<'a> -> Set<'a>` | Returns the set intersection of two sets. |
| `Set.diff` | `Set<'a> -> Set<'a> -> Set<'a>` | Returns elements in the first set that are not in the second set. |
| `Set.isSubset` | `Set<'a> -> Set<'a> -> bool` | Returns `true` if every element of the first set is in the second set. |
| `Set.toList` | `Set<'a> -> 'a list` | Materializes the set into a sorted ascending list. |
| `Set.ofList` | `'a list -> Set<'a>` | Constructs a persistent ordered set from a list. |

### Example
```fsharp
let s =
    Set.empty
    |> Set.add 10
    |> Set.add 20
    |> Set.add 10   // Duplicate ignored

let has20 = Set.contains 20 s   // true
let size  = Set.count s         // 2
```

---

## 4. `Seq` — Lazy Pull-Based Sequences

**Implementation:** [`internal/stdlib/seq.go`](file:///home/jottley/.projects/go/fml/internal/stdlib/seq.go)

`Seq` implements on-demand, pull-based sequence iterators (`SeqIter` state machines). Transformations such as `Seq.map`, `Seq.filter`, and `Seq.take` do not allocate intermediate lists and can operate over infinite streams (`Seq.initInfinite`).

### Functions

| Symbol | Signature | Description |
| :--- | :--- | :--- |
| `Seq.init` | `int -> (int -> 'a) -> 'a seq` | Creates a lazy sequence of `count` elements generated by `fn index`. |
| `Seq.initInfinite` | `(int -> 'a) -> 'a seq` | Creates an unbounded lazy sequence `fn 0, fn 1, fn 2, ...`. |
| `Seq.map` | `('a -> 'b) -> 'a seq -> 'b seq` | Lazily transforms each element of the sequence using `fn`. |
| `Seq.filter` | `('a -> bool) -> 'a seq -> 'a seq` | Lazily yields only elements satisfying `predicate`. |
| `Seq.take` | `int -> 'a seq -> 'a seq` | Lazily truncates a sequence to at most `n` elements. |
| `Seq.toList` | `'a seq -> 'a list` | Materializes a lazy sequence into an immutable singly-linked list. |
| `Seq.sum` | `int seq -> int` | Consumes the sequence and computes the sum of its integer elements. |

### Example
```fsharp
// Sum the squares of the first 5 even natural numbers from an infinite stream
let total =
    Seq.initInfinite (fun i -> i)
    |> Seq.filter (fun x -> x % 2 == 0)
    |> Seq.map (fun x -> x * x)
    |> Seq.take 5
    |> Seq.sum      // 0 + 4 + 16 + 36 + 64 = 120
```

---

## 5. `Format` — Wadler Pretty-Printing Engine

**Implementation:** [`internal/stdlib/format.go`](file:///home/jottley/.projects/go/fml/internal/stdlib/format.go)

Inspired by OCaml's `Format` module and Philip Wadler's *"A Prettier Printer"*, `Format` builds algebraic layout documents (`Doc`). A `Format.group` node automatically renders all inner `Format.line` breaks as single spaces if the group fits within the target column width, or breaks them into indented newlines if it exceeds the width.

### Functions

| Symbol | Signature | Description |
| :--- | :--- | :--- |
| `Format.empty` | `Doc` | Empty document node (`DocNil`). |
| `Format.line` | `Doc` | Breakable newline (renders as `' '` in flat mode, or `'\n'` + indentation in break mode). |
| `Format.text` / `Format.doc` | `string -> Doc` | Atomic literal text node. |
| `Format.cat` | `Doc -> Doc -> Doc` | Concatenates two documents sequentially. |
| `Format.nest` | `int -> Doc -> Doc` | Increases the indentation level of breaks inside `Doc` by `n` columns. |
| `Format.group` | `Doc -> Doc` | Marks a sub-document that flattens onto one line if it fits the remaining page width. |
| `Format.pretty` | `int -> Doc -> string` | Renders `Doc` formatted for a maximum column width of `maxWidth`. |

### Example
```fsharp
let header = Format.text "begin"
let body   = Format.nest 4 (Format.cat Format.line (Format.text "let x = 42"))
let footer = Format.cat Format.line (Format.text "end")
let doc    = Format.group (Format.cat (Format.cat header body) footer)

let oneLine   = Format.pretty 80 doc   // "begin let x = 42 end"
let multiLine = Format.pretty 12 doc   // "begin\n    let x = 42\nend"
```

---

## 6. `Mailbox` — Erlang/F# Actor Concurrency

**Implementation:** [`internal/stdlib/mailbox.go`](file:///home/jottley/.projects/go/fml/internal/stdlib/mailbox.go)

`Mailbox` implements F#'s `MailboxProcessor` actor model directly on top of Go goroutines and buffered channels, with `sync.Pool`-recycled message envelopes and isolated per-actor VM state snapshots for race-free execution. Actors also implement `DisposableVal`, so binding an actor with `use` / `let use` automatically stops its mailbox when the scope exits.

### Functions

| Symbol | Signature | Description |
| :--- | :--- | :--- |
| `Mailbox.Start` | `(Mailbox<'msg> -> unit) -> Mailbox<'msg>` | Spawns a background actor goroutine running the message-loop function. |
| `Mailbox.Post` | `Mailbox<'msg> -> 'msg -> unit` | Asynchronously enqueues a fire-and-forget message to the actor's inbox. |
| `Mailbox.Receive` | `Mailbox<'msg> -> 'msg` | Blocks the actor until the next message arrives in its inbox. If sent via `PostAndAsyncReply`, returns a 2-tuple `(msg, replyFn)`. |
| `Mailbox.PostAndAsyncReply` | `Mailbox<'msg> -> 'msg -> 'reply` | Sends a message with a one-shot reply channel and awaits the actor's response. |

### Example
```fsharp
let counterActor = Mailbox.Start (fun inbox ->
    let d1 = Mailbox.Receive inbox
    let d2 = Mailbox.Receive inbox
    let req = Mailbox.Receive inbox
    let finalDelta = fst req
    let reply = snd req
    reply (d1 + d2 + finalDelta)
)

Mailbox.Post counterActor 10
Mailbox.Post counterActor 25
let total = Mailbox.PostAndAsyncReply counterActor 7   // 42
```

---

## 7. `File` — File System I/O & Scoped Resources

**Implementation:** [`internal/stdlib/io_net_json.go`](file:///home/jottley/.projects/go/fml/internal/stdlib/io_net_json.go)

`File` binds Go's `os` and `io` file operations into FML, including deterministic resource cleanup when paired with `use` or `let use`.

### Functions

| Symbol | Signature | Description |
| :--- | :--- | :--- |
| `File.readText` | `string -> string` | Reads the entire file at `path` into a UTF-8 string. |
| `File.writeText` | `string -> string -> unit` | Writes `content` to `path` (permissions `0600`). |
| `File.exists` | `string -> bool` | Returns `true` if `path` exists on disk. |
| `File.openReader` | `string -> string` | Opens `path` as a `DisposableVal` resource that automatically closes its underlying file descriptor when bound with `use` / `let use`. |

### Example
```fsharp
File.writeText "/tmp/notes.txt" "hello from FML"

if File.exists "/tmp/notes.txt" then
    let use text = File.openReader "/tmp/notes.txt"
    printfn text
else
    ()
```

---

## 8. `Json` — Native JSON Serialization

**Implementation:** [`internal/stdlib/io_net_json.go`](file:///home/jottley/.projects/go/fml/internal/stdlib/io_net_json.go)

`Json` bridges FML structural values (`RecordVal`, `ListVal`, `ArrayVal`, `TupleVal`, `IntVal`, `FloatVal`, `BoolVal`, `StringVal`, `UnitVal`) with Go's `encoding/json`.

* **Encoding (`Json.encode`):** FML records serialize to JSON objects (`{...}`), lists/arrays/tuples serialize to JSON arrays (`[...]`), and primitives serialize to their JSON equivalents.
* **Decoding (`Json.decode`):** JSON objects deserialize directly into FML records (accessible via dot notation `obj.Field`), JSON arrays deserialize into FML lists, and integer/float numbers preserve exact 64-bit precision via `json.Number`.

### Functions

| Symbol | Signature | Description |
| :--- | :--- | :--- |
| `Json.encode` | `'a -> string` | Serializes any FML value to a compact JSON string. |
| `Json.decode` | `string -> 'b` | Parses a JSON string into native FML records, lists, and primitives. |

### Example
```fsharp
type ServerConfig = { Host: string; Port: int }

let cfg = { Host = "127.0.0.1"; Port = 9000 }
let raw = Json.encode cfg              // "{\"Host\":\"127.0.0.1\",\"Port\":9000}"
let parsed = Json.decode raw
let port = parsed.Port                 // 9000
```

---

## 9. `Http` — Native HTTP Client

**Implementation:** [`internal/stdlib/io_net_json.go`](file:///home/jottley/.projects/go/fml/internal/stdlib/io_net_json.go)

`Http` exposes Go's `net/http` client for synchronous REST/HTTP requests.

### Functions

| Symbol | Signature | Description |
| :--- | :--- | :--- |
| `Http.get` | `string -> string` | Performs an HTTP `GET` request to `url` and returns the response body as a `string`. |
| `Http.post` | `string -> string -> string -> string` | Performs an HTTP `POST` to `url` with `contentType` and `body`, returning the response body as a `string`. |

### Example
```fsharp
let payload = Json.encode { Service = "worker"; Ready = true }
let responseText = Http.post "http://localhost:8080/status" "application/json" payload
let responseObj = Json.decode responseText
printfn responseObj.status
```

---

## 10. `option`, `result` & `Quote` — Computation Expression Builders & AST Quotations

**Implementation:** [`internal/stdlib/comp_quote.go`](file:///home/jottley/.projects/go/fml/internal/stdlib/comp_quote.go)

FML desugars `builder { let! x = m; return expr }` into calls to `builder.Bind`, `builder.Return`, `builder.ReturnFrom`, and `builder.Zero`. Both built-in (`option`, `result`) and user-defined builder records are supported.

First-class code quotations `<@ expr @>` reify FML expressions into the built-in `Expr` algebraic data type (`QInt`, `QFloat`, `QBool`, `QString`, `QVar`, `QInfix`, `QApp`, `QLambda`, `QLet`, `QUnit`).

### Functions

| Symbol | Signature | Description |
| :--- | :--- | :--- |
| `option.Bind` | `'a option -> ('a -> 'b option) -> 'b option` | Short-circuiting monadic bind over `Some` / `None`. |
| `option.Return` | `'a -> 'a option` | Wraps a value in `Some`. |
| `result.Bind` | `Result<'a, 'e> -> ('a -> Result<'b, 'e>) -> Result<'b, 'e>` | Short-circuiting monadic bind over `Ok` / `Error`. |
| `result.Return` | `'a -> Result<'a, 'e>` | Wraps a value in `Ok`. |
| `Quote.toString` | `Expr -> string` | Formats a quoted `Expr` AST back into canonical FML syntax. |
| `Quote.eval` | `Expr -> 'a` | Evaluates a reified `Expr` AST at runtime. |

---

## 11. `List`, `Array`, `String`, `Option` & `Result` — Core Combinators

**Implementation:** [`internal/stdlib/combinators.go`](file:///home/jottley/.projects/go/fml/internal/stdlib/combinators.go)

| Module | Functions |
| :--- | :--- |
| `List` | `List.empty`, `List.isEmpty`, `List.length`, `List.head`, `List.tail`, `List.rev`, `List.map`, `List.filter`, `List.fold`, `List.sortBy`, `List.sum` |
| `Array` | `Array.length`, `Array.get`, `Array.set`, `Array.map`, `Array.ofList`, `Array.toList` |
| `String` | `String.length`, `String.trim`, `String.split`, `String.join`, `String.contains`, `String.toUpper`, `String.toLower` |
| `Option` | `Option.isSome`, `Option.isNone`, `Option.defaultValue`, `Option.map`, `Option.bind` |
| `Result` | `Result.isOk`, `Result.isError`, `Result.defaultValue`, `Result.map`, `Result.mapError`, `Result.bind` |
| `Regex` | `Regex.isMatch`, `Regex.find`, `Regex.findAll`, `Regex.replace`, `Regex.split`, `Regex.groups` |
| `Math` | `Math.pi`, `Math.abs`, `Math.min`, `Math.max`, `Math.sqrt`, `Math.pow`, `Math.sin`, `Math.cos`, `Math.floor`, `Math.ceil`, `Math.round` |
| `Time` | `Time.nowUnixMs`, `Time.nowMs`, `Time.nowIso`, `Time.sleepMs`, `Time.measureMs`, `Time.parseIso`, `Time.formatIso` |
| `Sys` | `Sys.args`, `Sys.getenv`, `Sys.setenv`, `Sys.cwd`, `Sys.fileExists` |
| `Crypto` | `Crypto.sha256`, `Crypto.hmacSha256`, `Crypto.base64Encode`, `Crypto.base64Decode`, `Crypto.uuid4` |
| `Test` | `Test.assert`, `Test.assertEq`, `Test.forall` (QuickCheck-style property verification) |
| `Csv` / `Env` | `Csv.decode`, `Env.decode` (backing runtime parsers for `CsvProvider` & `EnvProvider`) |

---

## 12. `Async` — Concurrent Goroutine Workflows (`async { ... }`)

**Implementation:** [`internal/stdlib/async.go`](file:///home/jottley/.projects/go/fml/internal/stdlib/async.go)

| Symbol | Signature | Description |
| :--- | :--- | :--- |
| `Async.RunSynchronously` | `Async<'a> -> 'a` | Executes an `async { ... }` workflow and awaits its final value. |
| `Async.Parallel` | `Async<'a> list -> Async<'a list>` | Fans out a list of async workflows across isolated Go goroutines and collects their results in order. |
| `Async.Sleep` | `int -> Async<unit>` | Suspends the current workflow for `ms` milliseconds. |
| `Async.Start` | `Async<unit> -> unit` | Spawns a workflow in the background on a Go goroutine and returns `()` immediately. |
| `Async.Return` | `'a -> Async<'a>` | Wraps a pure value in an `Async<'a>` workflow. |


