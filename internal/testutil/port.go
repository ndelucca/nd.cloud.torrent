// Package testutil holds fixture helpers shared by more than one package's
// tests. It is imported only from _test.go files.
package testutil

import (
	"fmt"
	"net"
	"testing"
)

// FreePort returns a port free on both TCP and UDP.
//
// Both, because anacrolix binds TCP *and* UDP on the listen port: a TCP-only
// check hands out ports whose UDP half is taken, and Configure then fails with
// "subsequent listen: bind: address already in use". That surfaces as an
// intermittent failure in whichever test happened to draw the port, blaming the
// code under test for a collision in the fixture.
//
// Still a TOCTOU — the listeners are closed before the caller binds — but
// checking both halves removes the systematic collisions, which are the common
// case. A caller that binds through the engine should retry on top of this; a
// genuine race with another process is what the retry covers.
//
// Callers wanting a plain HTTP port get the UDP probe too. It is unnecessary
// there but harmless, and one helper that is always right beats two that differ
// in a way nobody remembers.
func FreePort(t *testing.T) int {
	t.Helper()
	for attempt := 0; attempt < 20; attempt++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := l.Addr().(*net.TCPAddr).Port
		// Probe the UDP half of the same port before releasing the TCP one, so
		// nothing else can claim the pair in between.
		pc, uerr := net.ListenPacket("udp", fmt.Sprintf("127.0.0.1:%d", port))
		l.Close()
		if uerr != nil {
			continue // UDP half taken; draw another
		}
		pc.Close()
		return port
	}
	t.Fatal("no port free on both TCP and UDP after 20 attempts")
	return 0
}
