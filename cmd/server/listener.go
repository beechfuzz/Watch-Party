package main

import (
	"net"
	"sync"
)

// connBroker owns the one real net.Listener for a given listen address for
// as long as runLoop keeps reusing that address across incarnations. A
// single goroutine (acceptLoop) is the only thing that ever calls Accept on
// the real listener; every incarnation's *http.Server instead calls Serve
// on a thin per-incarnation handoff (below) that reads from this broker's
// shared conns channel.
//
// This is what closes the race documented in ARCHITECTURE.md §16.25: the
// naive approach (each incarnation calls net.Listen itself, and the
// previous one's http.Server.Shutdown really closes its listener) leaves a
// real window, between the old listener closing and the new one binding,
// where the kernel has nothing listening on that port and refuses new
// connections outright -- which is exactly what let the confirmation
// page's own CSS request fail. Here the real listener is never closed
// until realClose is called (process shutdown, or the listen address
// itself changes), so a connection arriving while no incarnation is
// actively reading from conns simply waits there (bounded by the channel's
// buffer) instead of being refused.
type connBroker struct {
	real net.Listener

	conns chan net.Conn

	closed    chan struct{}
	closeOnce sync.Once
}

// connBrokerBacklog bounds how many accepted-but-not-yet-claimed
// connections this broker will hold while no incarnation's Serve loop is
// reading from conns -- comfortably more than a browser's own per-origin
// connection limit for the handful of requests (stylesheet, a couple of
// scripts, a few fonts) a single page load like the confirmation page
// issues, so it isn't sized against any real production request volume.
const connBrokerBacklog = 32

// newConnBroker takes ownership of l (an already-bound listener) and starts
// forwarding its Accept results onto conns immediately.
func newConnBroker(l net.Listener) *connBroker {
	b := &connBroker{
		real:   l,
		conns:  make(chan net.Conn, connBrokerBacklog),
		closed: make(chan struct{}),
	}
	go b.acceptLoop()
	return b
}

func (b *connBroker) acceptLoop() {
	for {
		c, err := b.real.Accept()
		if err != nil {
			// The real listener is gone (realClose was called, or a
			// genuine accept error) -- nothing more will ever arrive.
			close(b.conns)
			return
		}
		select {
		case b.conns <- c:
		case <-b.closed:
			c.Close()
			return
		}
	}
}

// Addr returns the real listener's bound address -- stable for this
// broker's whole lifetime, since the address a broker was constructed for
// never changes (a change in the desired listen address always means a new
// broker; see runLoop's acquireListener).
func (b *connBroker) Addr() net.Addr { return b.real.Addr() }

// realClose actually tears down the underlying OS socket -- only ever
// called by runLoop itself, either on final process shutdown or when the
// next incarnation needs a different listen address than this broker is
// bound to. Never called as a side effect of any individual incarnation's
// http.Server.Shutdown (see handoff.Close, which deliberately does not
// reach this).
func (b *connBroker) realClose() error {
	var err error
	b.closeOnce.Do(func() {
		close(b.closed)
		err = b.real.Close()
	})
	return err
}

// newHandoff returns a fresh net.Listener-shaped adapter for exactly one
// incarnation's http.Server.Serve call. Each incarnation gets its own so
// that its Shutdown-triggered Close only stops *that* incarnation from
// reading further connections -- it never touches the broker's real
// listener, which is the whole point.
func (b *connBroker) newHandoff() *handoff {
	return &handoff{broker: b, done: make(chan struct{})}
}

// handoff is what actually gets passed to http.Server.Serve. Accept blocks
// on the broker's shared conns channel (so it sees exactly the same
// connections the real listener produces, just relayed) until either a
// connection arrives or this specific handoff is closed. Close does not
// close the broker -- a later incarnation's own handoff keeps working
// against the same still-open real socket.
type handoff struct {
	broker *connBroker

	done     chan struct{}
	doneOnce sync.Once
}

func (h *handoff) Accept() (net.Conn, error) {
	select {
	case c, ok := <-h.broker.conns:
		if !ok {
			return nil, net.ErrClosed
		}
		return c, nil
	case <-h.done:
		return nil, net.ErrClosed
	}
}

func (h *handoff) Close() error {
	h.doneOnce.Do(func() { close(h.done) })
	return nil
}

func (h *handoff) Addr() net.Addr { return h.broker.Addr() }

// isEphemeralAddr reports whether addr asks the OS to pick an arbitrary
// free port (":0", "127.0.0.1:0", ...) -- a request that always means "give
// me a new one," the opposite of what listener reuse is for. Tests rely on
// this: LISTEN_ADDR=127.0.0.1:0 is how existing cmd/server tests get an
// independent ephemeral port per incarnation (see run_test.go), and that
// behavior must keep working unchanged, so an ephemeral address always
// forces a fresh real bind rather than being (incorrectly) matched against
// a previous literal ":0" and reused.
func isEphemeralAddr(addr string) bool {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	return port == "0"
}
