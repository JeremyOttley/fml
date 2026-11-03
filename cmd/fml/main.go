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
	"os"
	"path/filepath"
	"strings"

	"fml/internal/codegen"
	"fml/internal/ir"
	"fml/internal/parser"
	"fml/internal/repl"
	"fml/internal/runtime"
	"fml/internal/stdlib"
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
		fmt.Fprintln(os.Stderr, "usage: fml [run|build|emit-go|check|fmt|lsp|test|repl] [file.fml] [-o <binary>]")
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
			fmt.Fprintln(os.Stderr, "usage: fml fmt <file.fml>")
			os.Exit(1)
		}
		formatted, err := repl.FormatFile(os.Args[2], 80)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fml fmt error: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprint(os.Stdout, formatted)

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
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	var files []string
	if !info.IsDir() {
		files = append(files, target)
	} else {
		entries, err := os.ReadDir(target)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), "_test.fml") {
				files = append(files, filepath.Join(target, e.Name()))
			}
		}
	}
	for _, f := range files {
		if _, err := ExecuteFile(f, out); err != nil {
			fmt.Fprintf(out, "FAIL\t%s: %v\n", f, err)
			return err
		}
		fmt.Fprintf(out, "PASS\t%s\n", f)
	}
	return nil
}
