package stationbrowser

// The player proxy is deliberately a very small CONNECT proxy rather than a
// general HTTP proxy.  A DevTools-controlled page is not allowed to use the
// desktop's ordinary network stack: every destination must first be approved,
// resolved, and then dialled by the pinned IP address.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	playerProxyHeaderLimit = 16 << 10
	playerProxyMaxClients  = 16
	playerProxyTimeout     = 8 * time.Second
)

type playerProxy struct {
	listener  net.Listener
	allowHost func(string) bool
	resolver  func(context.Context, string) ([]net.IPAddr, error)
	dial      func(context.Context, string, string) (net.Conn, error)
	ctx       context.Context
	cancel    context.CancelFunc

	mu             sync.Mutex
	conns          map[net.Conn]struct{}
	maxConnections int
	wg             sync.WaitGroup
	once           sync.Once
}

func openPlayerProxy(ctx context.Context, allowHost func(string) bool) (*playerProxy, error) {
	return openPlayerProxyWithCapacity(ctx, allowHost, playerProxyMaxClients)
}

func openPlayerProxyWithCapacity(ctx context.Context, allowHost func(string) bool, capacity int) (*playerProxy, error) {
	if ctx == nil || allowHost == nil {
		return nil, fmt.Errorf("%w: player proxy requires context and allowlist", errCDPProtocol)
	}
	if capacity < 2 || capacity > 256 {
		return nil, errCDPProtocol
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	proxyCtx, cancel := context.WithCancel(ctx)
	p := &playerProxy{
		listener: listener, allowHost: allowHost,
		resolver: net.DefaultResolver.LookupIPAddr,
		dial:     (&net.Dialer{Timeout: playerProxyTimeout, KeepAlive: -1}).DialContext,
		ctx:      proxyCtx, cancel: cancel, conns: map[net.Conn]struct{}{}, maxConnections: capacity,
	}
	p.wg.Add(1)
	go p.acceptLoop()
	return p, nil
}

func (p *playerProxy) address() string { return p.listener.Addr().String() }

func (p *playerProxy) acceptLoop() {
	defer p.wg.Done()
	for {
		conn, err := p.listener.Accept()
		if err != nil {
			if p.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		if !p.track(conn) {
			_ = writeProxyFailure(conn, 503, "proxy busy")
			_ = conn.Close()
			continue
		}
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			defer p.untrack(conn)
			defer conn.Close()
			p.handle(conn)
		}()
	}
}

func (p *playerProxy) track(conn net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.conns) >= p.maxConnections || p.ctx.Err() != nil {
		return false
	}
	p.conns[conn] = struct{}{}
	return true
}
func (p *playerProxy) untrack(conn net.Conn) {
	p.mu.Lock()
	delete(p.conns, conn)
	p.mu.Unlock()
}

func (p *playerProxy) handle(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(playerProxyTimeout))
	reader := bufio.NewReaderSize(conn, playerProxyHeaderLimit)
	authority, err := readConnectRequest(reader)
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		_ = writeProxyFailure(conn, 405, "CONNECT HTTPS only")
		return
	}
	host, _, err := permittedConnectHost(authority, p.allowHost)
	if err != nil {
		_ = writeProxyFailure(conn, 403, "destination denied")
		return
	}
	resolveCtx, cancel := context.WithTimeout(p.ctx, playerProxyTimeout)
	ips, err := p.resolver(resolveCtx, host)
	cancel()
	if err != nil || len(ips) == 0 {
		_ = writeProxyFailure(conn, 502, "destination unavailable")
		return
	}
	// A mixed answer is not a harmless fallback: accepting it turns DNS into a
	// policy oracle and makes later resolver behaviour difficult to reason
	// about. Require the entire answer set to be public before pinning a dial.
	for _, item := range ips {
		if _, ok := playerProxyPublicAddress(item.IP); !ok {
			_ = writeProxyFailure(conn, 403, "destination denied")
			return
		}
	}
	var upstream net.Conn
	for _, item := range ips {
		address, ok := playerProxyPublicAddress(item.IP)
		if !ok {
			continue
		}
		dialCtx, cancel := context.WithTimeout(p.ctx, playerProxyTimeout)
		upstream, err = p.dial(dialCtx, "tcp", net.JoinHostPort(address.String(), "443"))
		cancel()
		if err == nil {
			break
		}
	}
	if upstream == nil {
		_ = writeProxyFailure(conn, 403, "destination denied")
		return
	}
	if !p.track(upstream) {
		_ = upstream.Close()
		_ = writeProxyFailure(conn, 503, "proxy busy")
		return
	}
	defer p.untrack(upstream)
	defer upstream.Close()
	if _, err = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	// reader owns any bytes buffered beyond the CONNECT headers.
	done := make(chan struct{})
	go func() { _, _ = io.Copy(upstream, reader); _ = upstream.Close(); close(done) }()
	_, _ = io.Copy(conn, upstream)
	_ = conn.Close()
	<-done
}

func (p *playerProxy) Close() error {
	p.once.Do(func() {
		p.cancel()
		_ = p.listener.Close()
		p.mu.Lock()
		connections := make([]net.Conn, 0, len(p.conns))
		for conn := range p.conns {
			connections = append(connections, conn)
		}
		p.mu.Unlock()
		for _, conn := range connections {
			_ = conn.Close()
		}
		p.wg.Wait()
	})
	return nil
}

func readConnectRequest(reader *bufio.Reader) (string, error) {
	var used int
	line, n, err := readProxyLine(reader)
	used += n
	if err != nil {
		return "", err
	}
	parts := strings.Split(line, " ")
	if len(parts) != 3 || parts[0] != "CONNECT" || (parts[2] != "HTTP/1.1" && parts[2] != "HTTP/1.0") {
		return "", errors.New("not a CONNECT request")
	}
	for {
		line, n, err = readProxyLine(reader)
		used += n
		if err != nil || used > playerProxyHeaderLimit {
			return "", errors.New("invalid proxy headers")
		}
		if line == "" {
			return parts[1], nil
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || name == "" || strings.EqualFold(strings.TrimSpace(name), "Proxy-Authorization") || strings.TrimSpace(value) == "" {
			return "", errors.New("invalid proxy header")
		}
	}
}
func readProxyLine(reader *bufio.Reader) (string, int, error) {
	// ReadSlice observes Reader.Size and returns ErrBufferFull rather than
	// allocating an arbitrary attacker-controlled first line.
	line, err := reader.ReadSlice('\n')
	if err != nil || len(line) > playerProxyHeaderLimit || !strings.HasSuffix(string(line), "\r\n") {
		return "", len(line), errors.New("invalid proxy line")
	}
	return strings.TrimSuffix(string(line), "\r\n"), len(line), nil
}

func permittedConnectHost(authority string, allowHost func(string) bool) (string, int, error) {
	host, port, err := net.SplitHostPort(authority)
	if err != nil || port != "443" || host == "" {
		return "", 0, errors.New("invalid CONNECT authority")
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" || net.ParseIP(host) != nil || !playerProxyHostSyntax(host) || !allowHost(host) {
		return "", 0, errors.New("CONNECT host is not allowed")
	}
	return host, 443, nil
}

func playerProxyHostSyntax(host string) bool {
	if len(host) > 253 || host == "localhost" || strings.HasSuffix(host, ".localhost") || !strings.Contains(host, ".") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

func playerProxyPublicAddress(ip net.IP) (netip.Addr, bool) {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return netip.Addr{}, false
	}
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
		return netip.Addr{}, false
	}
	if addr.Is6() {
		// Global IPv6 allocations are within 2000::/3.  The explicitly
		// excluded ranges are documentation and transition mechanisms: they
		// are not stable public origins for an allowlisted browser session.
		if addr.As16()[0]&0xe0 != 0x20 || playerProxySpecialIPv6(addr) {
			return netip.Addr{}, false
		}
		return addr, true
	}
	b := addr.As4()
	if b[0] == 0 || b[0] >= 224 || b[0] == 100 && b[1]&0xc0 == 0x40 || b[0] == 192 && (b[1] == 0 || b[1] == 2 || b[1] == 88) || b[0] == 198 && (b[1] == 18 || b[1] == 19 || b[1] == 51) || b[0] == 203 && b[1] == 0 {
		return netip.Addr{}, false
	}
	return addr, true
}

func playerProxySpecialIPv6(addr netip.Addr) bool {
	for _, prefix := range []netip.Prefix{
		netip.MustParsePrefix("2001:db8::/32"), // documentation
		netip.MustParsePrefix("2002::/16"),     // 6to4
		netip.MustParsePrefix("2001::/32"),     // Teredo
		netip.MustParsePrefix("64:ff9b::/96"),  // well-known NAT64
	} {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func writeProxyFailure(conn net.Conn, status int, text string) error {
	if status < 400 || status > 599 {
		status, text = 502, "proxy failure"
	}
	_, err := io.WriteString(conn, "HTTP/1.1 "+strconv.Itoa(status)+" "+text+"\r\nConnection: close\r\nContent-Length: 0\r\n\r\n")
	return err
}
