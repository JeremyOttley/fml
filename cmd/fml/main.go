// Package main is the command-line entry point for the FML compiler and runtime.
//
// Jaime says: A compiler shouldn't require a cathedral of configuration files
// or a 500MB dependency tree. Read the bytes, build the flat arena, unify the
// types, lower to 10 IR instructions, bind the Go runtime primitives, and run
// it. Data dominates.
package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"fml/internal/ast"
	"fml/internal/codegen"
	"fml/internal/ir"
	"fml/internal/parser"
	"fml/internal/repl"
	"fml/internal/runtime"
	"fml/internal/stdlib"
	"fml/internal/token"
	"fml/internal/types"
)

// ExecuteSource compiles, type-checks, lowers, and evaluates an FML source buffer.
func ExecuteSource(src []byte, stdout io.Writer) (runtime.Value, error) {
	p, root, err := parser.ParseSource(src, nil)
	if err != nil {
		return nil, err
	}
	chk := types.NewChecker(p.Arena, nil)
	chk.AttachSource(p.Source(), p.Tokens())
	if _, err := chk.Infer(root); err != nil {
		return nil, err
	}
	prog := ir.Lower(p.Arena, chk.Types, root)
	ir.Optimize(prog)
	vm := runtime.NewVM(prog)
	if stdout != nil {
		vm.Stdout = stdout
	}
	stdlib.RegisterAll(vm)
	return vm.Run()
}

// ExecuteFile compiles, type-checks (with multi-file open and relative schema resolution),
// lowers, and evaluates a `.fml` file from disk.
func ExecuteFile(path string, stdout io.Writer) (runtime.Value, error) {
	prog, err := codegen.CompileFile(path)
	if err != nil {
		return nil, err
	}
	vm := runtime.NewVM(prog)
	if stdout != nil {
		vm.Stdout = stdout
	}
	stdlib.RegisterAll(vm)
	return vm.Run()
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: fml [run|build|emit-go|check|fmt|doc|lsp|test|repl] [file.fml] [-o <binary>]")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "test":
		target := "."
		if len(os.Args) >= 3 {
			target = os.Args[2]
		}
		if err := runTestSuite(target, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "fml test failed: %v\n", err)
			os.Exit(1)
		}

	case "lsp":
		if err := repl.RunLSP(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "fml lsp error: %v\n", err)
			os.Exit(1)
		}

	case "repl":
		if err := repl.RunInteractive(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "fml repl error: %v\n", err)
			os.Exit(1)
		}

	case "fmt":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: fml fmt [-w] <file_or_dir>")
			os.Exit(1)
		}
		if err := runFmt(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "fml fmt error: %v\n", err)
			os.Exit(1)
		}

	case "doc":
		query := ""
		if len(os.Args) >= 3 {
			query = os.Args[2]
		}
		if err := runDoc(query, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "fml doc error: %v\n", err)
			os.Exit(1)
		}

	case "check":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: fml check <file.fml>")
			os.Exit(1)
		}
		summary, err := repl.CheckFile(os.Args[2])
		if err != nil {
			fmt.Fprintf(os.Stderr, "fml check error: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprint(os.Stdout, summary)

	case "build":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: fml build <file.fml> [-o <binary>]")
			os.Exit(1)
		}
		srcPath := os.Args[2]
		outBin := strings.TrimSuffix(filepath.Base(srcPath), filepath.Ext(srcPath))
		for i := 3; i < len(os.Args); i++ {
			if os.Args[i] == "-o" && i+1 < len(os.Args) {
				outBin = os.Args[i+1]
				i++
			}
		}
		if err := codegen.BuildBinary(srcPath, outBin); err != nil {
			fmt.Fprintf(os.Stderr, "fml build error: %v\n", err)
			os.Exit(1)
		}

	case "emit-go":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: fml emit-go <file.fml>")
			os.Exit(1)
		}
		prog, err := codegen.CompileFile(os.Args[2])
		if err != nil {
			fmt.Fprintf(os.Stderr, "fml error: %v\n", err)
			os.Exit(1)
		}
		goSrc, err := codegen.EmitGoSource(prog)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fml emit-go error: %v\n", err)
			os.Exit(1)
		}
		os.Stdout.Write(goSrc)

	case "run":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: fml run <file.fml>")
			os.Exit(1)
		}
		runCLIFile(os.Args[2])

	default:
		runCLIFile(os.Args[1])
	}
}

func runCLIFile(path string) {
	val, err := ExecuteFile(path, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fml error: %v\n", err)
		os.Exit(1)
	}
	if val != nil && val != runtime.Unit {
		fmt.Fprintln(os.Stdout, val.String())
	}
}

func runTestSuite(target string, out io.Writer) error {
	recursive := false
	cleanTarget := target
	if cleanTarget == "./..." || cleanTarget == "..." {
		recursive = true
		cleanTarget = "."
	} else if strings.HasSuffix(cleanTarget, "/...") {
		recursive = true
		cleanTarget = strings.TrimSuffix(cleanTarget, "/...")
		if cleanTarget == "" {
			cleanTarget = "."
		}
	}

	info, err := os.Stat(cleanTarget)
	if err != nil {
		return err
	}
	var files []string
	if !info.IsDir() {
		files = append(files, cleanTarget)
	} else if recursive {
		err = filepath.WalkDir(cleanTarget, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				if d.Name() != "." && strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(d.Name(), "_test.fml") {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			return err
		}
	} else {
		entries, err := os.ReadDir(cleanTarget)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), "_test.fml") {
				files = append(files, filepath.Join(cleanTarget, e.Name()))
			}
		}
	}
	sort.Strings(files)
	for _, f := range files {
		start := time.Now()
		if _, err := ExecuteFile(f, out); err != nil {
			fmt.Fprintf(out, "FAIL\t%s: %v\n", f, err)
			return err
		}
		fmt.Fprintf(out, "PASS\t%s\t(%dms)\n", f, time.Since(start).Milliseconds())
	}
	if len(files) > 0 {
		fmt.Fprintf(out, "ok\t%d test file(s) passed\n", len(files))
	}
	return nil
}

func runFmt(args []string, out io.Writer) error {
	writeInPlace := false
	var targets []string
	for _, a := range args {
		if a == "-w" {
			writeInPlace = true
		} else {
			targets = append(targets, a)
		}
	}
	if len(targets) == 0 {
		return fmt.Errorf("missing file or directory path")
	}

	var files []string
	for _, t := range targets {
		clean := strings.TrimSuffix(t, "/...")
		if clean == "" || clean == "." && strings.HasSuffix(t, "...") {
			clean = "."
		}
		info, err := os.Stat(clean)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			files = append(files, clean)
			continue
		}
		err = filepath.WalkDir(clean, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				if d.Name() != "." && strings.HasPrefix(d.Name(), ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(d.Name(), ".fml") {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}

	for _, f := range files {
		formatted, err := repl.FormatFile(f, 80)
		if err != nil {
			return err
		}
		if writeInPlace {
			if err := os.WriteFile(f, []byte(formatted), 0600); err != nil {
				return err
			}
		} else {
			fmt.Fprint(out, formatted)
		}
	}
	return nil
}

func runDoc(query string, out io.Writer) error {
	if strings.HasSuffix(query, ".fml") {
		if _, err := os.Stat(query); err == nil {
			summary, err := repl.CheckFile(query)
			if err != nil {
				return err
			}
			fmt.Fprint(out, summary)
			return nil
		}
	}

	arena := ast.NewArena(32, 32, nil)
	chk := types.NewChecker(arena, nil)
	scope := chk.Scopes[0]

	type docEntry struct {
		name string
		sig  string
	}
	var matches []docEntry
	prefix := query + "."

	for symID, b := range scope {
		name := arena.Strings.String(symID)
		if name == "" {
			continue
		}
		if query == "" {
			matches = append(matches, docEntry{name: name, sig: chk.Types.Format(b.Scheme.Body)})
		} else if name == query || strings.HasPrefix(name, prefix) {
			if name == query && strings.Contains(chk.Types.Format(b.Scheme.Body), "{ ") && !strings.Contains(query, ".") {
				// Module record entry; individual "Module.fn" entries are more readable
				continue
			}
			matches = append(matches, docEntry{name: name, sig: chk.Types.Format(b.Scheme.Body)})
		}
	}

	if len(matches) == 0 && query != "" {
		symID := token.StringID(arena.Strings.InternString(query))
		if b, ok := scope[symID]; ok {
			matches = append(matches, docEntry{name: query, sig: chk.Types.Format(b.Scheme.Body)})
		}
	}

	if len(matches) == 0 {
		return fmt.Errorf("no documentation found for %q", query)
	}

	sort.Slice(matches, func(i, j int) bool {
		return matches[i].name < matches[j].name
	})
	for _, m := range matches {
		fmt.Fprintf(out, "val %s : %s\n", m.name, m.sig)
	}
	return nil
}
