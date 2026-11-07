package obspreview

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

var testUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

func TestCaptureAuthenticatedReadOnlyPreview(t *testing.T) {
	const password = "not-logged-test-password"
	salt := base64.StdEncoding.EncodeToString([]byte("salt"))
	challenge := base64.StdEncoding.EncodeToString([]byte("challenge"))
	wantAuth, err := challengeResponse(password, salt, challenge)
	if err != nil {
		t.Fatal(err)
	}
	imageData := testJPEGData(t, previewWidth, previewHeight, "data:image/jpg;base64,")

	server := testServer(t, func(conn *websocket.Conn) {
		writeWire(t, conn, 0, map[string]any{"rpcVersion": 1, "authentication": map[string]string{"salt": salt, "challenge": challenge}})
		identify := readWire(t, conn)
		var identifyData struct {
			Authentication     string `json:"authentication"`
			EventSubscriptions int    `json:"eventSubscriptions"`
		}
		if err := json.Unmarshal(identify.D, &identifyData); err != nil {
			t.Error(err)
		}
		if identify.Op != 1 || identifyData.Authentication != wantAuth || identifyData.EventSubscriptions != 0 {
			t.Errorf("unexpected Identify: %#v", identify)
			return
		}
		writeWire(t, conn, 2, map[string]any{"negotiatedRpcVersion": 1})

		first := readWire(t, conn)
		assertRequest(t, first, "GetCurrentProgramScene")
		writeResponse(t, conn, first.D, "GetCurrentProgramScene", map[string]any{"currentProgramSceneName": "Program Scene"})

		second := readWire(t, conn)
		assertRequest(t, second, "GetSourceScreenshot")
		var request struct {
			RequestData struct {
				SourceName  string `json:"sourceName"`
				ImageFormat string `json:"imageFormat"`
				ImageWidth  int    `json:"imageWidth"`
				ImageHeight int    `json:"imageHeight"`
			} `json:"requestData"`
		}
		if err := json.Unmarshal(second.D, &request); err != nil {
			t.Error(err)
		} else if request.RequestData.SourceName != "Program Scene" || request.RequestData.ImageFormat != "jpeg" || request.RequestData.ImageWidth != previewWidth || request.RequestData.ImageHeight != previewHeight {
			t.Errorf("unexpected screenshot request: %+v", request.RequestData)
		}
		writeResponse(t, conn, second.D, "GetSourceScreenshot", map[string]any{"imageData": imageData})
	})
	defer server.Close()

	frame, err := Capture(context.Background(), serverPort(t, server), password)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if frame.Scene != "Program Scene" {
		t.Errorf("scene = %q", frame.Scene)
	}
	if want := jpegDataPrefix + imageData[len("data:image/jpg;base64,"):]; frame.ImageData != want {
		t.Error("image data was not safely canonicalized")
	}
}

func TestCaptureRejectsMismatchedResponse(t *testing.T) {
	server := testServer(t, func(conn *websocket.Conn) {
		writeDefaultHello(t, conn)
		_ = readWire(t, conn)
		writeWire(t, conn, 2, map[string]any{"negotiatedRpcVersion": 1})
		_ = readWire(t, conn)
		writeWire(t, conn, 7, map[string]any{
			"requestType": "GetCurrentProgramScene", "requestId": "another-request",
			"requestStatus": map[string]any{"result": true, "code": 100},
			"responseData":  map[string]any{"currentProgramSceneName": "ignored"},
		})
	})
	defer server.Close()

	_, err := Capture(context.Background(), serverPort(t, server), "test-password")
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("error = %v, want ErrProtocol", err)
	}
}

func TestCaptureRejectsUnauthenticatedHello(t *testing.T) {
	server := testServer(t, func(conn *websocket.Conn) {
		writeWire(t, conn, 0, map[string]any{"rpcVersion": 1})
	})
	defer server.Close()

	_, err := Capture(context.Background(), serverPort(t, server), "test-password")
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("error = %v, want ErrProtocol", err)
	}
}

func TestCaptureReportsAuthenticationWithoutLeakingPassword(t *testing.T) {
	const password = "top-secret-test-value"
	server := testServer(t, func(conn *websocket.Conn) {
		writeWire(t, conn, 0, map[string]any{"rpcVersion": 1, "authentication": map[string]string{
			"salt": base64.StdEncoding.EncodeToString([]byte("salt")), "challenge": base64.StdEncoding.EncodeToString([]byte("challenge")),
		}})
		_ = readWire(t, conn)
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4005, ""), time.Now().Add(time.Second))
	})
	defer server.Close()

	_, err := Capture(context.Background(), serverPort(t, server), password)
	if !errors.Is(err, ErrAuthentication) {
		t.Fatalf("error = %v, want ErrAuthentication", err)
	}
	if err != nil && bytes.Contains([]byte(err.Error()), []byte(password)) {
		t.Fatal("password leaked through error")
	}
}

func TestCaptureHonorsCancellation(t *testing.T) {
	entered := make(chan struct{})
	server := testServer(t, func(conn *websocket.Conn) {
		writeDefaultHello(t, conn)
		_ = readWire(t, conn)
		writeWire(t, conn, 2, map[string]any{"negotiatedRpcVersion": 1})
		_ = readWire(t, conn)
		close(entered)
		time.Sleep(time.Second)
	})
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := Capture(ctx, serverPort(t, server), "test-password"); result <- err }()
	select {
	case <-entered:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("request did not reach server")
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Capture did not return after cancellation")
	}
}

func TestNormalizeJPEGAcceptsBoundedAspectFitAndRejectsOversize(t *testing.T) {
	bounded := testJPEGData(t, 320, 180, jpegDataPrefix)
	if _, err := normalizeJPEG(bounded); err != nil {
		t.Fatalf("bounded JPEG rejected: %v", err)
	}
	overwide := testJPEGData(t, previewWidth+1, previewHeight, jpegDataPrefix)
	if _, err := normalizeJPEG(overwide); !errors.Is(err, ErrInvalidFrame) {
		t.Fatalf("oversize JPEG error = %v", err)
	}
}

func testServer(t *testing.T, handler func(*websocket.Conn)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := testUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		handler(conn)
	}))
}

func writeDefaultHello(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	writeWire(t, conn, 0, map[string]any{"rpcVersion": 1, "authentication": map[string]string{
		"salt": base64.StdEncoding.EncodeToString([]byte("salt")), "challenge": base64.StdEncoding.EncodeToString([]byte("challenge")),
	}})
}

func serverPort(t *testing.T, server *httptest.Server) int {
	t.Helper()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, portText, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func readWire(t *testing.T, conn *websocket.Conn) envelope {
	t.Helper()
	messageType, body, err := conn.ReadMessage()
	if err != nil {
		t.Error(err)
		return envelope{}
	}
	if messageType != websocket.TextMessage {
		t.Errorf("message type = %d", messageType)
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Error(err)
	}
	return env
}

func writeWire(t *testing.T, conn *websocket.Conn, op int, data any) {
	t.Helper()
	if err := conn.WriteJSON(map[string]any{"op": op, "d": data}); err != nil {
		t.Error(err)
	}
}

func fieldString(t *testing.T, raw json.RawMessage, name string) string {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Error(err)
	}
	text, _ := value[name].(string)
	return text
}

func assertRequest(t *testing.T, env envelope, wantType string) {
	t.Helper()
	if env.Op != 6 || fieldString(t, env.D, "requestType") != wantType {
		t.Errorf("unexpected request: %#v", env)
	}
}

func writeResponse(t *testing.T, conn *websocket.Conn, request json.RawMessage, requestType string, responseData any) {
	t.Helper()
	writeWire(t, conn, 7, map[string]any{
		"requestType": requestType, "requestId": fieldString(t, request, "requestId"),
		"requestStatus": map[string]any{"result": true, "code": 100}, "responseData": responseData,
	})
}

func testJPEGData(t *testing.T, width, height int, prefix string) string {
	t.Helper()
	imageValue := image.NewRGBA(image.Rect(0, 0, width, height))
	imageValue.Set(0, 0, color.RGBA{R: 1, G: 2, B: 3, A: 255})
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, imageValue, &jpeg.Options{Quality: 75}); err != nil {
		t.Fatal(err)
	}
	return prefix + base64.StdEncoding.EncodeToString(encoded.Bytes())
}
