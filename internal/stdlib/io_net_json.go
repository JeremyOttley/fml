package stdlib

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"fml/internal/runtime"
	"fml/internal/token"
)

// RegisterAll binds the complete "batteries-included" standard library
// (Map, Set, Seq, Format, Mailbox, File, Json, Http) into vm.
//
// Jaime says: One call wires the entire runtime library before evaluation starts.
// No dynamic plugin loaders, no package managers, no external dependencies.
func RegisterAll(vm *runtime.VM) {
	RegisterCollections(vm)
	RegisterCombinators(vm)
	RegisterSeq(vm)
	RegisterFormat(vm)
	RegisterMailbox(vm)
	RegisterIONetJSON(vm)
	RegisterCompAndQuotes(vm)
	RegisterAsync(vm)
}

// RegisterIONetJSON binds Go's os/io, encoding/json, and net/http primitives
// into the FML runtime under the `File`, `Json`, and `Http` modules.
func RegisterIONetJSON(vm *runtime.VM) {
	fileReadText := &runtime.BuiltinVal{
		Name:  "File.readText",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			path := runtime.Deref(args[0]).(runtime.StringVal).V
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("File.readText: %w", err)
			}
			return runtime.StringVal{V: string(data)}, nil
		},
	}

	fileWriteText := &runtime.BuiltinVal{
		Name:  "File.writeText",
		Arity: 2,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			path := runtime.Deref(args[0]).(runtime.StringVal).V
			content := runtime.Deref(args[1]).String()
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				return nil, fmt.Errorf("File.writeText: %w", err)
			}
			return runtime.Unit, nil
		},
	}

	fileExists := &runtime.BuiltinVal{
		Name:  "File.exists",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			path := runtime.Deref(args[0]).(runtime.StringVal).V
			_, err := os.Stat(path)
			return runtime.BoolVal{V: err == nil}, nil
		},
	}

	fileOpenReader := &runtime.BuiltinVal{
		Name:  "File.openReader",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			path := runtime.Deref(args[0]).(runtime.StringVal).V
			f, err := os.Open(path)
			if err != nil {
				return nil, fmt.Errorf("File.openReader: %w", err)
			}
			data, readErr := io.ReadAll(f)
			if readErr != nil {
				_ = f.Close()
				return nil, readErr
			}
			return &runtime.DisposableVal{
				Inner: runtime.StringVal{V: string(data)},
				OnDispose: func() {
					_ = f.Close()
				},
			}, nil
		},
	}

	defineModuleRecord(vm, "File", map[string]runtime.Value{
		"readText":   fileReadText,
		"writeText":  fileWriteText,
		"exists":     fileExists,
		"openReader": fileOpenReader,
	})

	jsonEncode := &runtime.BuiltinVal{
		Name:  "Json.encode",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			goVal := fmlValueToGoJSON(runtime.Deref(args[0]))
			raw, err := json.Marshal(goVal)
			if err != nil {
				return nil, fmt.Errorf("Json.encode: %w", err)
			}
			return runtime.StringVal{V: string(raw)}, nil
		},
	}

	jsonDecode := &runtime.BuiltinVal{
		Name:  "Json.decode",
		Arity: 1,
		Fn: func(v *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			str := runtime.Deref(args[0]).(runtime.StringVal).V
			dec := json.NewDecoder(strings.NewReader(str))
			dec.UseNumber()
			var raw any
			if err := dec.Decode(&raw); err != nil {
				return nil, fmt.Errorf("Json.decode: %w", err)
			}
			return goJSONToFMLValue(v.Prog.Strings, raw), nil
		},
	}

	defineModuleRecord(vm, "Json", map[string]runtime.Value{
		"encode": jsonEncode,
		"decode": jsonDecode,
	})

	httpGet := &runtime.BuiltinVal{
		Name:  "Http.get",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			url := runtime.Deref(args[0]).(runtime.StringVal).V
			resp, err := http.Get(url)
			if err != nil {
				return nil, fmt.Errorf("Http.get: %w", err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return nil, fmt.Errorf("Http.get read: %w", err)
			}
			return runtime.StringVal{V: string(body)}, nil
		},
	}

	httpPost := &runtime.BuiltinVal{
		Name:  "Http.post",
		Arity: 3,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			url := runtime.Deref(args[0]).(runtime.StringVal).V
			contentType := runtime.Deref(args[1]).(runtime.StringVal).V
			payload := runtime.Deref(args[2]).String()
			resp, err := http.Post(url, contentType, bytes.NewReader([]byte(payload)))
			if err != nil {
				return nil, fmt.Errorf("Http.post: %w", err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return nil, fmt.Errorf("Http.post read: %w", err)
			}
			return runtime.StringVal{V: string(body)}, nil
		},
	}

	defineModuleRecord(vm, "Http", map[string]runtime.Value{
		"get":  httpGet,
		"post": httpPost,
	})

	csvDecode := &runtime.BuiltinVal{
		Name:  "Csv.decode",
		Arity: 1,
		Fn: func(v *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			raw := runtime.Deref(args[0]).(runtime.StringVal).V
			r := csv.NewReader(strings.NewReader(strings.TrimSpace(raw)))
			records, err := r.ReadAll()
			if err != nil {
				return nil, fmt.Errorf("Csv.decode: %w", err)
			}
			if len(records) == 0 {
				return runtime.EmptyList, nil
			}
			headers := records[0]
			var rows []runtime.Value
			for _, row := range records[1:] {
				fields := make(map[token.StringID]runtime.Value, len(headers))
				byName := make(map[string]runtime.Value, len(headers))
				for i, h := range headers {
					colName := strings.TrimSpace(h)
					var cellStr string
					if i < len(row) {
						cellStr = strings.TrimSpace(row[i])
					}
					val := parseScalarCell(cellStr)
					fid := v.Prog.Strings.InternString(colName)
					fields[fid] = val
					byName[colName] = val
				}
				rows = append(rows, &runtime.RecordVal{Fields: fields, ByName: byName})
			}
			return runtime.SliceToList(rows), nil
		},
	}
	defineModuleRecord(vm, "Csv", map[string]runtime.Value{
		"decode": csvDecode,
	})

	envDecode := &runtime.BuiltinVal{
		Name:  "Env.decode",
		Arity: 1,
		Fn: func(v *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			raw := runtime.Deref(args[0]).(runtime.StringVal).V
			fields := make(map[token.StringID]runtime.Value)
			byName := make(map[string]runtime.Value)
			for _, line := range strings.Split(raw, "\n") {
				trimmed := strings.TrimSpace(line)
				if trimmed == "" || strings.HasPrefix(trimmed, "#") {
					continue
				}
				idx := strings.IndexByte(trimmed, '=')
				if idx <= 0 {
					continue
				}
				key := strings.TrimSpace(trimmed[:idx])
				valStr := strings.Trim(strings.TrimSpace(trimmed[idx+1:]), `"'`)
				val := parseScalarCell(valStr)
				fid := v.Prog.Strings.InternString(key)
				fields[fid] = val
				byName[key] = val
			}
			return &runtime.RecordVal{Fields: fields, ByName: byName}, nil
		},
	}
	defineModuleRecord(vm, "Env", map[string]runtime.Value{
		"decode": envDecode,
	})
}

func parseScalarCell(s string) runtime.Value {
	if s == "true" {
		return runtime.BoolVal{V: true}
	}
	if s == "false" {
		return runtime.BoolVal{V: false}
	}
	if iv, err := strconv.ParseInt(s, 10, 64); err == nil {
		return runtime.IntVal{V: iv}
	}
	if strings.Contains(s, ".") {
		if fv, err := strconv.ParseFloat(s, 64); err == nil {
			return runtime.FloatVal{V: fv}
		}
	}
	return runtime.StringVal{V: s}
}

func fmlValueToGoJSON(v runtime.Value) any {
	v = runtime.Deref(v)
	switch tv := v.(type) {
	case runtime.UnitVal:
		return nil
	case runtime.BoolVal:
		return tv.V
	case runtime.IntVal:
		return tv.V
	case runtime.FloatVal:
		return tv.V
	case runtime.StringVal:
		return tv.V
	case runtime.TupleVal:
		out := make([]any, len(tv.Elems))
		for i, e := range tv.Elems {
			out[i] = fmlValueToGoJSON(e)
		}
		return out
	case *runtime.ArrayVal:
		out := make([]any, len(tv.Elems))
		for i, e := range tv.Elems {
			out[i] = fmlValueToGoJSON(e)
		}
		return out
	case *runtime.ListVal:
		out := make([]any, 0)
		curr := tv
		for curr != nil && !curr.Empty {
			out = append(out, fmlValueToGoJSON(curr.Head))
			curr = curr.Tail
		}
		return out
	case *runtime.RecordVal:
		m := make(map[string]any, len(tv.ByName))
		for k, fv := range tv.ByName {
			m[k] = fmlValueToGoJSON(fv)
		}
		return m
	default:
		return tv.String()
	}
}

func goJSONToFMLValue(strings *token.StringTable, raw any) runtime.Value {
	switch v := raw.(type) {
	case nil:
		return runtime.Unit
	case bool:
		return runtime.BoolVal{V: v}
	case json.Number:
		if iv, err := v.Int64(); err == nil {
			return runtime.IntVal{V: iv}
		}
		if fv, err := v.Float64(); err == nil {
			return runtime.FloatVal{V: fv}
		}
		return runtime.StringVal{V: v.String()}
	case float64:
		return runtime.FloatVal{V: v}
	case string:
		return runtime.StringVal{V: v}
	case []any:
		items := make([]runtime.Value, len(v))
		for i, elem := range v {
			items[i] = goJSONToFMLValue(strings, elem)
		}
		return runtime.SliceToList(items)
	case map[string]any:
		fields := make(map[token.StringID]runtime.Value, len(v))
		byName := make(map[string]runtime.Value, len(v))
		for k, elem := range v {
			fv := goJSONToFMLValue(strings, elem)
			fid := strings.InternString(k)
			fields[fid] = fv
			byName[k] = fv
		}
		return &runtime.RecordVal{Fields: fields, ByName: byName}
	default:
		return runtime.StringVal{V: fmt.Sprintf("%v", v)}
	}
}
