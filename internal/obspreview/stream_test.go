package obspreview

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const testRelayTicket = "synthetic-relay-ticket"

func testOwnedStream(t *testing.T, handler func(*websocket.Conn)) *OwnedStream {
	t.Helper()
	server := testServer(t, func(conn *websocket.Conn) {
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		writeDefaultHello(t, conn)
		identify := readWire(t, conn)
		if identify.Op != 1 || fieldString(t, identify.D, "authentication") == "" {
			t.Error("missing authentication")
			return
		}
		writeWire(t, conn, 2, map[string]int{"negotiatedRpcVersion": 1})
		handler(conn)
	})
	t.Cleanup(server.Close)
	port := serverPort(t, server)
	output, err := NewOwnedStream(port, "synthetic-password", func(_ context.Context, tcp *net.TCPConn) error {
		if peer := tcp.RemoteAddr().(*net.TCPAddr); !peer.IP.IsLoopback() || peer.Port != port {
			return errors.New("wrong synthetic peer")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func respondToStatus(t *testing.T, conn *websocket.Conn, active, reconnecting bool) {
	t.Helper()
	env := readWire(t, conn)
	assertRequest(t, env, "GetStreamStatus")
	writeResponse(t, conn, env.D, "GetStreamStatus", map[string]bool{"outputActive": active, "outputReconnecting": reconnecting})
}

func acknowledgeCommand(t *testing.T, conn *websocket.Conn, env envelope, kind string) {
	t.Helper()
	assertRequest(t, env, kind)
	writeWire(t, conn, 7, map[string]any{
		"requestType": kind, "requestId": fieldString(t, env.D, "requestId"),
		"requestStatus": map[string]any{"result": true, "code": 100},
	})
}

func TestOwnedStreamStartStatusStop(t *testing.T) {
	var sessions atomic.Int32
	var outputActive atomic.Bool
	output := testOwnedStream(t, func(conn *websocket.Conn) {
		switch sessions.Add(1) {
		case 1:
			respondToStatus(t, conn, false, false)
			config := readWire(t, conn)
			var decoded struct {
				RequestData struct {
					Type     string            `json:"streamServiceType"`
					Settings map[string]string `json:"streamServiceSettings"`
				} `json:"requestData"`
			}
			if err := json.Unmarshal(config.D, &decoded); err != nil ||
				decoded.RequestData.Type != "rtmp_custom" || decoded.RequestData.Settings["server"] != "rtmp://127.0.0.1:19350/live" ||
				decoded.RequestData.Settings["key"] != testRelayTicket {
				t.Error("unexpected relay settings")
			}
			acknowledgeCommand(t, conn, config, "SetStreamServiceSettings")
			acknowledgeCommand(t, conn, readWire(t, conn), "StartStream")
		case 2, 4:
			respondToStatus(t, conn, outputActive.Load(), false)
		case 3:
			acknowledgeCommand(t, conn, readWire(t, conn), "StopStream")
		default:
			t.Error("unexpected connection")
		}
	})
	ctx := context.Background()
	if err := output.Start(ctx, 19350, testRelayTicket); err != nil {
		t.Fatal(err)
	}
	outputActive.Store(true)
	if status, err := output.Status(ctx); err != nil || !status.Active {
		t.Fatalf("active status: %+v, %v", status, err)
	}
	if err := output.RequestStop(ctx); err != nil {
		t.Fatal(err)
	}
	// OBS's command acknowledgment does not make the output inactive.
	if status, err := output.Status(ctx); err != nil || !status.Active {
		t.Fatalf("stop acknowledgement changed observed status: %+v, %v", status, err)
	}
	if err := output.Start(ctx, 19350, testRelayTicket); !errors.Is(err, ErrStreamStopped) {
		t.Fatalf("restart after stop: %v", err)
	}
}

func TestOwnedStreamRejectsPeerBeforeWebSocketUpgrade(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	port := serverPort(t, server)
	output, err := NewOwnedStream(port, "synthetic-password", func(context.Context, *net.TCPConn) error {
		return errors.New("private process verification detail")
	})
	if err != nil {
		t.Fatal(err)
	}
	err = output.Start(context.Background(), 19350, testRelayTicket)
	if err != ErrStreamEndpoint || requests.Load() != 0 {
		t.Fatalf("unowned endpoint was contacted or leaked details: %v, %d HTTP requests", err, requests.Load())
	}
}

func TestOwnedStreamNeverRepeatsUnconfirmedStart(t *testing.T) {
	var sessions atomic.Int32
	output := testOwnedStream(t, func(conn *websocket.Conn) {
		if sessions.Add(1) > 1 {
			t.Error("start reconnected after its response was lost")
			return
		}
		respondToStatus(t, conn, false, false)
		acknowledgeCommand(t, conn, readWire(t, conn), "SetStreamServiceSettings")
		assertRequest(t, readWire(t, conn), "StartStream")
		// Connection closes after receiving the command.
	})
	ctx := context.Background()
	if err := output.Start(ctx, 19350, testRelayTicket); !errors.Is(err, ErrStreamStartUnknown) || strings.Contains(err.Error(), testRelayTicket) {
		t.Fatalf("lost start response: %v", err)
	}
	if err := output.Start(ctx, 19350, testRelayTicket); !errors.Is(err, ErrStreamAlreadyRequested) {
		t.Fatalf("repeat start: %v", err)
	}
}

func TestOwnedStreamStopDuringSetupFencesStart(t *testing.T) {
	configReceived := make(chan struct{})
	resume := make(chan struct{})
	drained := make(chan struct{})
	var connections atomic.Int32
	output := testOwnedStream(t, func(conn *websocket.Conn) {
		if connections.Add(1) != 1 {
			t.Error("stop before start claim must not send a stop command")
			return
		}
		defer close(drained)
		respondToStatus(t, conn, false, false)
		env := readWire(t, conn)
		assertRequest(t, env, "SetStreamServiceSettings")
		close(configReceived)
		<-resume
		if _, _, err := conn.ReadMessage(); err == nil {
			t.Error("received StartStream after canceling setup")
		}
	})
	defer func() {
		close(resume)
		select {
		case <-drained:
		case <-time.After(time.Second):
			t.Error("encoder fixture did not drain")
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := make(chan error, 1)
	go func() { started <- output.Start(ctx, 19350, testRelayTicket) }()
	select {
	case <-configReceived:
	case <-ctx.Done():
		t.Fatal("no configuration request")
	}
	if err := output.RequestStop(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-started:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("start was not interrupted: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("stop did not unblock start")
	}
	if err := output.Start(ctx, 19350, testRelayTicket); !errors.Is(err, ErrStreamStopped) {
		t.Fatalf("stop did not fence new start: %v", err)
	}
}

func TestOwnedStreamRejectsActiveOrReconnecting(t *testing.T) {
	output := testOwnedStream(t, func(conn *websocket.Conn) {
		respondToStatus(t, conn, false, true)
		if _, _, err := conn.ReadMessage(); err == nil {
			t.Error("setup attempted to change a reconnecting encoder")
		}
	})
	if err := output.Start(context.Background(), 19350, testRelayTicket); !errors.Is(err, ErrStreamBusy) {
		t.Fatalf("reconnecting: %v", err)
	}
}
