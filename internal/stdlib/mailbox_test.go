package stdlib

import (
	"testing"

	"fml/internal/runtime"
)

func TestMailboxProcessor(t *testing.T) {
	// Spawn a stateful accumulator actor that processes 3 Post messages
	// and replies to a final PostAndAsyncReply request.
	src := `
let agent = Mailbox.Start (fun inbox ->
    let rec loop state =
        let m1 = Mailbox.Receive inbox
        let m2 = Mailbox.Receive inbox
        let req = Mailbox.Receive inbox
        let msg = fst req
        let reply = snd req
        reply (state + m1 + m2 + msg)
    in loop 100
)
Mailbox.Post agent 10
Mailbox.Post agent 20
let finalTotal = Mailbox.PostAndAsyncReply agent 30
finalTotal
`
	res := runWithStdlib(t, src)
	if iv, ok := res.(runtime.IntVal); !ok || iv.V != 160 {
		t.Fatalf("expected actor reply 160, got %v", res)
	}
}
