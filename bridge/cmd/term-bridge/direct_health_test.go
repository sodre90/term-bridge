package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"testing"
	"time"
)

// directTLSListener stands in for serveDirect's Tailscale-certificate
// listener: same countingListener wrapping and same TLS shape, on loopback
// with a throwaway cert, so the health reporting can be driven end to end
// without a live Tailscale daemon.
func directTLSListener(t *testing.T, health *directHealth) net.Listener {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}

	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return tls.NewListener(countingListener{Listener: tcpLn, health: health}, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})
}

func serveForTest(t *testing.T, health *directHealth) (addr string) {
	t.Helper()
	ln := directTLSListener(t, health)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = serveDirectListener(ctx, ln, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}), health)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	waitFor(t, "listener to report bound", func() bool {
		bound, _, _ := health.snapshot()
		return bound
	})
	return ln.Addr().String()
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// cmux-app-0no: this is the state the old status output called plainly "up".
// A listener nothing ever reaches has to be distinguishable from a working
// one, and the only thing that separates them is that nothing arrived.
func TestDirectHealthBoundButNothingConnects(t *testing.T) {
	health := &directHealth{}
	serveForTest(t, health)

	bound, accepted, lastServed := health.snapshot()
	if !bound {
		t.Fatal("listener should report bound")
	}
	if accepted != 0 {
		t.Fatalf("nothing dialed it, want 0 accepted, got %d", accepted)
	}
	if !lastServed.IsZero() {
		t.Fatal("nothing was served, so there is no last-served time")
	}
}

// The other way a bound listener is useless: connections arrive and die
// before they ever become a request. Accepted must move even though nothing
// is ever served, which is exactly what tells the two failures apart.
func TestDirectHealthCountsConnectionsThatNeverBecomeRequests(t *testing.T) {
	health := &directHealth{}
	addr := serveForTest(t, health)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	// Not a ClientHello: the handshake fails and the connection is dropped
	// long before any HTTP request exists.
	if _, err := conn.Write([]byte("this is not TLS\n")); err != nil {
		t.Fatal(err)
	}
	conn.Close()

	waitFor(t, "the connection to be counted as accepted", func() bool {
		_, accepted, _ := health.snapshot()
		return accepted > 0
	})
	if _, _, lastServed := health.snapshot(); !lastServed.IsZero() {
		t.Fatal("a failed handshake must not count as the listener serving anything")
	}
}

func directTestClient() *http.Client {
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // throwaway self-signed cert
	}}
}

func TestDirectHealthRecordsAServedRequest(t *testing.T) {
	health := &directHealth{}
	addr := serveForTest(t, health)

	resp, err := directTestClient().Get("https://" + addr + "/anything")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	bound, accepted, lastServed := health.snapshot()
	if !bound || accepted != 1 {
		t.Fatalf("want bound with 1 accepted, got bound=%v accepted=%d", bound, accepted)
	}
	if lastServed.IsZero() {
		t.Fatal("a completed request must record a last-served time")
	}
}

// cmux-app-8d3: the hourly reaper calls GET /agent/devices against every
// configured server, and the direct listener is one of them. Counting that
// made the field advance once an hour on the agent's own behalf, so it could
// not report whether the standby transport actually works -- it read healthy
// on a day when every phone request to it 401'd.
func TestDirectHealthIgnoresTheAgentsOwnAdminProbe(t *testing.T) {
	health := &directHealth{}
	addr := serveForTest(t, health)

	resp, err := directTestClient().Get("https://" + addr + "/agent/devices")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	_, accepted, lastServed := health.snapshot()
	if accepted != 1 {
		t.Fatalf("the probe still arrives, so it is still an accepted connection; got %d", accepted)
	}
	if !lastServed.IsZero() {
		t.Fatal("the agent's own admin probe must not report the direct listener as serving devices")
	}
}

// The exclusion is by caller, not by blanket silence: a device request still
// records, including one that will go on to fail auth -- it reached the
// transport, which is what this field answers.
func TestDirectHealthStillRecordsADeviceRequestAfterAnAdminProbe(t *testing.T) {
	health := &directHealth{}
	addr := serveForTest(t, health)
	client := directTestClient()

	probe, err := client.Get("https://" + addr + "/agent/devices")
	if err != nil {
		t.Fatal(err)
	}
	probe.Body.Close()

	resp, err := client.Get("https://" + addr + "/sessions")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if _, _, lastServed := health.snapshot(); lastServed.IsZero() {
		t.Fatal("a device request must record a last-served time even after an admin probe")
	}
}

// serveDirect passes health straight through, so a nil one must be inert
// rather than a panic waiting on the first connection.
func TestDirectHealthNilIsInert(t *testing.T) {
	var health *directHealth
	health.markBound()
	health.markAccepted()
	health.markServed()
	health.markUnbound()
	if bound, accepted, lastServed := health.snapshot(); bound || accepted != 0 || !lastServed.IsZero() {
		t.Fatal("a nil directHealth must report nothing")
	}
}
