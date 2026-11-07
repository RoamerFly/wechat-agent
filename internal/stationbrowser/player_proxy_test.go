package stationbrowser

import (
	"bufio"
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPlayerProxyCreatorCapacityDoesNotChangeDefault(t *testing.T) {
	for _, capacity := range []int{playerProxyMaxClients, 128} {
		p, err := openPlayerProxyWithCapacity(t.Context(), func(string) bool { return false }, capacity)
		if err != nil {
			t.Fatal(err)
		}
		connections := make([]net.Conn, 0, capacity)
		for i := 0; i < capacity; i++ {
			left, right := net.Pipe()
			right.Close()
			if !p.track(left) {
				t.Fatalf("capacity %d rejected descriptor %d", capacity, i)
			}
			connections = append(connections, left)
		}
		left, right := net.Pipe()
		right.Close()
		if p.track(left) {
			t.Fatal("capacity not bounded")
		}
		left.Close()
		for _, c := range connections {
			p.untrack(c)
			c.Close()
		}
		p.Close()
	}
}

func TestPlayerProxyRejectsNonConnectAndUnapprovedHost(t *testing.T) {
	p := newPlayerProxyForTest(t, func(host string) bool { return host == "allowed.example" })
	var resolves atomic.Int32
	p.resolver = func(context.Context, string) ([]net.IPAddr, error) { resolves.Add(1); return nil, nil }
	for _, request := range []string{
		"GET http://allowed.example/ HTTP/1.1\r\nHost: allowed.example\r\n\r\n",
		"CONNECT denied.example:443 HTTP/1.1\r\nHost: denied.example:443\r\n\r\n",
	} {
		response := playerProxyRequest(t, p, request)
		if !strings.HasPrefix(response, "HTTP/1.1 ") || strings.HasPrefix(response, "HTTP/1.1 200") {
			t.Fatalf("unexpected proxy response %q", response)
		}
	}
	if got := resolves.Load(); got != 0 {
		t.Fatalf("denied request resolved %d times", got)
	}
}

func TestPlayerProxyAcceptsAllowedByteDanceHostAndBoundsHeaders(t *testing.T) {
	p := newPlayerProxyForTest(t, func(host string) bool { return host == "lf-c-flwb.bytetos.com" })
	p.resolver = func(context.Context, string) ([]net.IPAddr, error) { return nil, nil }
	response := playerProxyRequest(t, p, "CONNECT lf-c-flwb.bytetos.com:443 HTTP/1.1\r\nHost: lf-c-flwb.bytetos.com:443\r\n\r\n")
	if !strings.HasPrefix(response, "HTTP/1.1 502") {
		t.Fatalf("allowed hostname was not resolved: %q", response)
	}
	conn, err := net.Dial("tcp", p.address())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Fill exactly one parser buffer without a newline. Extra unread TCP
	// bytes can turn the rejection into a reset on Windows and hide its status.
	if _, err := conn.Write([]byte("CONNECT " + strings.Repeat("a", playerProxyHeaderLimit-len("CONNECT ")))); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	status, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || !strings.HasPrefix(status, "HTTP/1.1 405") {
		t.Fatalf("oversized line response = %q, %v", status, err)
	}
}

func TestPlayerProxyBoundsIncompleteRequestLine(t *testing.T) {
	for _, size := range []int{playerProxyHeaderLimit + 1, playerProxyHeaderLimit * 2} {
		input := strings.NewReader(strings.Repeat("a", size))
		reader := bufio.NewReaderSize(input, playerProxyHeaderLimit)
		if _, err := readConnectRequest(reader); err == nil {
			t.Fatalf("accepted an incomplete request of %d bytes", size)
		}
		if read := size - input.Len(); read > playerProxyHeaderLimit {
			t.Fatalf("read %d bytes before rejecting the request", read)
		}
	}
}

func TestPlayerProxyNeverDialsPrivateOrUnknownResolution(t *testing.T) {
	p := newPlayerProxyForTest(t, func(host string) bool { return host == "allowed.example" })
	var dials atomic.Int32
	p.dial = func(context.Context, string, string) (net.Conn, error) { dials.Add(1); return nil, nil }
	for _, ips := range [][]net.IPAddr{
		{{IP: net.ParseIP("127.0.0.1")}},
		{{IP: net.ParseIP("10.2.3.4")}},
		{{IP: net.ParseIP("192.0.2.5")}},
		{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("127.0.0.1")}},
		nil,
	} {
		p.resolver = func(context.Context, string) ([]net.IPAddr, error) { return ips, nil }
		response := playerProxyRequest(t, p, "CONNECT allowed.example:443 HTTP/1.1\r\nHost: allowed.example:443\r\n\r\n")
		if !strings.HasPrefix(response, "HTTP/1.1 403") && !strings.HasPrefix(response, "HTTP/1.1 502") {
			t.Fatalf("unexpected resolution response %q", response)
		}
	}
	if got := dials.Load(); got != 0 {
		t.Fatalf("unsafe resolution dialled %d times", got)
	}
}

func TestPlayerProxyRejectsSpecialIPv6(t *testing.T) {
	for _, raw := range []string{"2001:db8::1", "2002::1", "2001::1", "64:ff9b::808:808"} {
		if _, ok := playerProxyPublicAddress(net.ParseIP(raw)); ok {
			t.Fatalf("special IPv6 %s was accepted", raw)
		}
	}
	if _, ok := playerProxyPublicAddress(net.ParseIP("2606:4700:4700::1111")); !ok {
		t.Fatal("public IPv6 was rejected")
	}
}

func TestPlayerProxyPinsPublicResolvedAddressAndClosesTunnel(t *testing.T) {
	p := newPlayerProxyForTest(t, func(host string) bool { return host == "allowed.example" })
	p.resolver = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}}, nil
	}
	dialled := make(chan string, 1)
	upstreamClosed := make(chan struct{})
	p.dial = func(_ context.Context, network, address string) (net.Conn, error) {
		dialled <- address
		client, server := net.Pipe()
		go func() {
			defer close(upstreamClosed)
			buf := make([]byte, 1)
			_, _ = server.Read(buf)
			_ = server.Close()
		}()
		return client, nil
	}
	conn, err := net.Dial("tcp", p.address())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.Write([]byte("CONNECT allowed.example:443 HTTP/1.1\r\nHost: allowed.example:443\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(status, "HTTP/1.1 200") {
		t.Fatalf("CONNECT not established: %q (%v)", status, err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil || line == "\r\n" {
			break
		}
	}
	select {
	case got := <-dialled:
		if got != "8.8.8.8:443" {
			t.Fatalf("dial address = %q, want pinned public IP", got)
		}
	case <-time.After(time.Second):
		t.Fatal("no upstream dial")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-upstreamClosed:
	case <-time.After(time.Second):
		t.Fatal("proxy Close did not close tunnel")
	}
}

func newPlayerProxyForTest(t *testing.T, allow func(string) bool) *playerProxy {
	t.Helper()
	p, err := openPlayerProxy(context.Background(), allow)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func playerProxyRequest(t *testing.T, p *playerProxy, request string) string {
	t.Helper()
	conn, err := net.Dial("tcp", p.address())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err = conn.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	response, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	return response
}
