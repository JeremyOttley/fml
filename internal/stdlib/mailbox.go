package stdlib

import (
	"fmt"
	"sync"

	"fml/internal/runtime"
	"fml/internal/token"
)

type mailboxEnvelope struct {
	msg     runtime.Value
	replyCh chan runtime.Value
}

var envelopePool = sync.Pool{
	New: func() any {
		return &mailboxEnvelope{}
	},
}

// MailboxActor implements F#'s Erlang-style MailboxProcessor actor backed by a
// dedicated Go goroutine and a buffered channel.
//
// Jaime says: F# calls it MailboxProcessor; Erlang calls it an actor; Go calls
// it a goroutine reading from a channel. Give the actor its own VM state
// snapshot so it never contends on shared locks, and pool the message envelopes.
type MailboxActor struct {
	ch       chan *mailboxEnvelope
	stopOnce sync.Once
	stopCh   chan struct{}
}

func newMailboxActor(parentVM *runtime.VM, bodyFn runtime.Value) *MailboxActor {
	actor := &MailboxActor{
		ch:     make(chan *mailboxEnvelope, 256),
		stopCh: make(chan struct{}),
	}

	// Snapshot Globals for the actor goroutine so concurrent execution is race-free
	actorVM := &runtime.VM{
		Prog:       parentVM.Prog,
		Globals:    make(map[token.StringID]runtime.Value, len(parentVM.Globals)),
		Stdout:     parentVM.Stdout,
		CustomEval: parentVM.CustomEval,
	}
	for k, v := range parentVM.Globals {
		actorVM.Globals[k] = v
	}

	inboxVal := wrapActor(actor)
	go func() {
		_, _ = actorVM.Apply(bodyFn, inboxVal)
	}()
	return actor
}

func (a *MailboxActor) Stop() {
	a.stopOnce.Do(func() {
		close(a.stopCh)
	})
}

func wrapActor(a *MailboxActor) *runtime.DisposableVal {
	return &runtime.DisposableVal{
		Inner: &runtime.NativeVal{
			Kind:    "MailboxProcessor",
			Payload: a,
			Format:  func() string { return "<MailboxProcessor>" },
		},
		OnDispose: a.Stop,
	}
}

func unwrapActor(v runtime.Value) (*MailboxActor, error) {
	v = runtime.Deref(v)
	nv, ok := v.(*runtime.NativeVal)
	if !ok || nv.Payload == nil {
		return nil, fmt.Errorf("expected MailboxProcessor actor, got %v", v)
	}
	actor, ok := nv.Payload.(*MailboxActor)
	if !ok {
		return nil, fmt.Errorf("invalid MailboxProcessor payload")
	}
	return actor, nil
}

// RegisterMailbox binds `Mailbox` (`Start`, `Post`, `Receive`, `PostAndAsyncReply`, `Reply`) into vm.
func RegisterMailbox(vm *runtime.VM) {
	mbStart := &runtime.BuiltinVal{
		Name:  "Mailbox.Start",
		Arity: 1,
		Fn: func(v *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			fn := runtime.Deref(args[0])
			actor := newMailboxActor(v, fn)
			return wrapActor(actor), nil
		},
	}

	mbPost := &runtime.BuiltinVal{
		Name:  "Mailbox.Post",
		Arity: 2,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			actor, err := unwrapActor(args[0])
			if err != nil {
				return nil, err
			}
			env := envelopePool.Get().(*mailboxEnvelope)
			env.msg = runtime.Deref(args[1])
			env.replyCh = nil
			select {
			case actor.ch <- env:
			case <-actor.stopCh:
			}
			return runtime.Unit, nil
		},
	}

	mbReceive := &runtime.BuiltinVal{
		Name:  "Mailbox.Receive",
		Arity: 1,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			actor, err := unwrapActor(args[0])
			if err != nil {
				return nil, err
			}
			select {
			case env := <-actor.ch:
				msg := env.msg
				replyCh := env.replyCh
				env.msg = nil
				env.replyCh = nil
				envelopePool.Put(env)
				if replyCh != nil {
					replyFn := &runtime.BuiltinVal{
						Name:  "ReplyChannel.Reply",
						Arity: 1,
						Fn: func(_ *runtime.VM, rArgs []runtime.Value) (runtime.Value, error) {
							replyCh <- runtime.Deref(rArgs[0])
							return runtime.Unit, nil
						},
					}
					return runtime.TupleVal{Elems: []runtime.Value{msg, replyFn}}, nil
				}
				return msg, nil
			case <-actor.stopCh:
				return runtime.Unit, nil
			}
		},
	}

	mbPostAndAsyncReply := &runtime.BuiltinVal{
		Name:  "Mailbox.PostAndAsyncReply",
		Arity: 2,
		Fn: func(_ *runtime.VM, args []runtime.Value) (runtime.Value, error) {
			actor, err := unwrapActor(args[0])
			if err != nil {
				return nil, err
			}
			msgVal := runtime.Deref(args[1])
			replyCh := make(chan runtime.Value, 1)
			env := envelopePool.Get().(*mailboxEnvelope)
			env.msg = msgVal
			env.replyCh = replyCh
			select {
			case actor.ch <- env:
			case <-actor.stopCh:
				return runtime.Unit, nil
			}
			select {
			case reply := <-replyCh:
				return reply, nil
			case <-actor.stopCh:
				return runtime.Unit, nil
			}
		},
	}

	defineModuleRecord(vm, "Mailbox", map[string]runtime.Value{
		"Start":             mbStart,
		"Post":              mbPost,
		"Receive":           mbReceive,
		"PostAndAsyncReply": mbPostAndAsyncReply,
	})
}
