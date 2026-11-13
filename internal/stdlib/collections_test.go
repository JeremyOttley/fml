package stdlib

import (
	"testing"

	"fml/internal/ir"
	"fml/internal/parser"
	"fml/internal/runtime"
	"fml/internal/types"
)

func runWithStdlib(t *testing.T, src string) runtime.Value {
	t.Helper()
	p, root, err := parser.ParseSource([]byte(src), nil)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	chk := types.NewChecker(p.Arena, nil)
	if _, err := chk.Infer(root); err != nil {
		t.Fatalf("type error: %v", err)
	}
	prog := ir.Lower(p.Arena, chk.Types, root)
	vm := runtime.NewVM(prog)
	RegisterCollections(vm)
	RegisterSeq(vm)
	RegisterFormat(vm)
	RegisterMailbox(vm)
	val, err := vm.Run()
	if err != nil {
		t.Fatalf("runtime error: %v", err)
	}
	return val
}

func TestCollections(t *testing.T) {
	// Test sync.Pool node reuse directly
	n := AcquireAVLNode(runtime.IntVal{V: 1}, runtime.StringVal{V: "one"}, nil, nil)
	ReleaseAVLNode(n)
	n2 := AcquireAVLNode(runtime.IntVal{V: 2}, runtime.StringVal{V: "two"}, nil, nil)
	if n2.Height != 1 {
		t.Fatalf("expected height 1 on pooled AVLNode, got %d", n2.Height)
	}

	// Test persistent immutability across branches in FML
	src := `
let m0 = Map.empty
let m1 = Map.add "a" 10 m0
let m2 = Map.add "b" 20 m1
let m3 = Map.add "a" 99 m2

let v1 = Map.find "a" m1
let v3 = Map.find "a" m3
let hasBInM1 = Map.containsKey "b" m1

let s0 = Set.empty
let s1 = Set.add 42 (Set.add 7 s0)
let has42 = Set.contains 42 s1
let has99 = Set.contains 99 s1

if (not hasBInM1) && has42 && (not has99) then v1 + v3 else 0
`
	res := runWithStdlib(t, src)
	// v1 (10) + v3 (99) = 109
	if iv, ok := res.(runtime.IntVal); !ok || iv.V != 109 {
		t.Fatalf("expected 109 from persistent Map/Set operations, got %v", res)
	}
}

func TestSetAlgebraAndMapRemoveKeysValues(t *testing.T) {
	src := `
let s1 = Set.ofList [10; 20; 30; 40]
let s2 = Set.ofList [30; 40; 50]
let u = Set.union s1 s2
let inter = Set.intersect s1 s2
let d = Set.diff s1 s2
let dRemoved = Set.remove 10 d
let isSub = Set.isSubset inter u

let m = Map.add "x" 5 (Map.add "y" 15 (Map.add "z" 25 Map.empty))
let m2 = Map.remove "y" m
let kCount = Map.count m2

if isSub && (Set.count u == 5) && (Set.count inter == 2) && (Set.count dRemoved == 1) then
    kCount + (Map.find "x" m2) + (Map.find "z" m2)
else
    0
`
	res := runWithStdlib(t, src)
	// kCount (2) + 5 + 25 = 32
	if iv, ok := res.(runtime.IntVal); !ok || iv.V != 32 {
		t.Fatalf("expected 32 from Set algebra and Map.remove, got %v", res)
	}
}

func TestMapAndSetHigherOrderCombinators(t *testing.T) {
	src := `
let m = Map.ofList [("a", 10); ("b", 20); ("c", 30)]
let mDoubled = Map.map (fun _ v -> v * 2) m
let mFiltered = Map.filter (fun _ v -> v > 20) mDoubled
let mSum = Map.fold (fun acc _ v -> acc + v) 0 mFiltered
let pairsLen =
    match Map.toList mFiltered with
    | [_; _] -> 2
    | _ -> 0

let s = Set.ofList [1; 2; 3; 4]
let sMapped = Set.map (fun x -> x * 10) s
let sFiltered = Set.filter (fun x -> x >= 20) sMapped
let sSum = Set.fold (fun acc x -> acc + x) 0 sFiltered

mSum + pairsLen + sSum
`
	res := runWithStdlib(t, src)
	// mDoubled: a=20, b=40, c=60 -> mFiltered: b=40, c=60 -> mSum=100, pairsLen=2
	// sMapped: 10, 20, 30, 40 -> sFiltered: 20, 30, 40 -> sSum=90
	// Total = 100 + 2 + 90 = 192
	if iv, ok := res.(runtime.IntVal); !ok || iv.V != 192 {
		t.Fatalf("expected 192 from Map/Set higher-order combinators, got %v", res)
	}
}

func TestMapAndSetPredicatesPartitionAndExtrema(t *testing.T) {
	src := `
let m = Map.ofList [(1, 10); (2, 20); (3, 30); (4, 40)]
let mEmpty = Map.isEmpty Map.empty
let mNonEmpty = not (Map.isEmpty m)
let mHas30 = Map.exists (fun _ v -> v == 30) m
let mAllPos = Map.forall (fun k v -> k > 0 && v > 0) m
let kFound = Map.findKey (fun _ v -> v >= 25) m
let kTryFound =
    match Map.tryFindKey (fun k _ -> k == 4) m with
    | Some k -> k
    | None -> 0
let kTryMissing =
    match Map.tryFindKey (fun k _ -> k == 99) m with
    | Some _ -> 1
    | None -> 0
let mParts = Map.partition (fun _ v -> v >= 25) m
let mYes = fst mParts
let mNo = snd mParts

let s1 = Set.ofList [10; 20; 30; 40]
let s2 = Set.ofList [20; 30]
let sEmpty = Set.isEmpty Set.empty
let sSup = Set.isSuperset s1 s2
let sHas20 = Set.exists (fun x -> x == 20) s1
let sAllTens = Set.forall (fun x -> x % 10 == 0) s1
let sParts = Set.partition (fun x -> x > 20) s1
let sYes = fst sParts
let sNo = snd sParts
let sMin = Set.minElement s1
let sMax = Set.maxElement s1

if mEmpty && mNonEmpty && mHas30 && mAllPos && sEmpty && sSup && sHas20 && sAllTens && (kTryMissing == 0) then
    kFound + kTryFound + (Map.count mYes) + (Map.count mNo) + (Set.count sYes) + (Set.count sNo) + sMin + sMax
else
    0
`
	res := runWithStdlib(t, src)
	// kFound (3) + kTryFound (4) + Map.count mYes (2) + Map.count mNo (2) + Set.count sYes (2) + Set.count sNo (2) + sMin (10) + sMax (40) = 65
	if iv, ok := res.(runtime.IntVal); !ok || iv.V != 65 {
		t.Fatalf("expected 65 from Map/Set predicates, partition, and extrema, got %v", res)
	}
}



