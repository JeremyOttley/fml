package stdlib

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"fml/internal/ir"
	"fml/internal/parser"
	"fml/internal/runtime"
	"fml/internal/types"
)

func TestFileJSONAndHTTP(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "sample.json")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"status":"created","code":201}`))
			return
		}
		_, _ = w.Write([]byte(`{"service":"fml-node","ok":true}`))
	}))
	defer srv.Close()

	src := fmt.Sprintf(`
type Config = { Host: string; Port: int }
let cfg = { Host = "localhost"; Port = 8080 }
let encoded = Json.encode cfg
File.writeText %q encoded
let exists = File.exists %q
let use readerContent = File.openReader %q
let decoded = Json.decode readerContent
let port = decoded.Port

let getResp = Http.get %q
let getObj = Json.decode getResp
let postResp = Http.post %q "application/json" encoded
let postObj = Json.decode postResp

if exists && getObj.ok then port + postObj.code else 0
`, filePath, filePath, filePath, srv.URL, srv.URL)

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
	RegisterAll(vm)
	res, err := vm.Run()
	if err != nil {
		t.Fatalf("runtime error: %v", err)
	}

	// port (8080) + postObj.code (201) = 8281
	if iv, ok := res.(runtime.IntVal); !ok || iv.V != 8281 {
		t.Fatalf("expected 8281 from File/Json/Http integration, got %v", res)
	}
}

func TestHttpServeOnceGetResultAndJsonTryDecode(t *testing.T) {
	src := `
let baseUrl = Http.serveOnce "127.0.0.1:0" (fun req ->
    let payload = Json.encode { Method = req.Method; Path = req.Path; Value = 321 }
    { Status = 200; Body = payload }
)

let respRes = Http.getResult (baseUrl + "/api/v1/health")
let bodyText = Result.defaultValue "{}" respRes
let parsedRes = Json.tryDecode bodyText
let badJsonRes = Json.tryDecode "{invalid_json"

let score =
    match parsedRes with
    | Ok obj ->
        if (obj.Method == "GET") && (obj.Path == "/api/v1/health") then obj.Value else 0
    | Error _ -> 0

if (Result.isOk respRes) && (Result.isError badJsonRes) then
    score
else
    0
`
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
	RegisterAll(vm)
	res, err := vm.Run()
	if err != nil {
		t.Fatalf("runtime error: %v", err)
	}
	if iv, ok := res.(runtime.IntVal); !ok || iv.V != 321 {
		t.Fatalf("expected 321 from Http.serveOnce + Http.getResult + Json.tryDecode, got %v", res)
	}
}

