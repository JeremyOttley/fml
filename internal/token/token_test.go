package token

import (
	"bytes"
	"testing"
	"unsafe"
)

func TestTokenSizeAndSlice(t *testing.T) {
	if sz := unsafe.Sizeof(Token{}); sz > 16 {
		t.Fatalf("Token struct size %d exceeds 16-byte cache packing limit", sz)
	}

	src := []byte("let mutable x = 42")
	tok := Token{
		Kind:  TokenMutable,
		Line:  1,
		Col:   5,
		Start: 4,
		End:   11,
	}
	if !bytes.Equal(tok.Slice(src), []byte("mutable")) {
		t.Fatalf("expected slice %q, got %q", "mutable", tok.Slice(src))
	}
	if tok.Kind.String() != "mutable" {
		t.Fatalf("expected kind string %q, got %q", "mutable", tok.Kind.String())
	}
}
