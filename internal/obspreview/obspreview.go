// Package obspreview provides read-only OBS program preview and a separate,
// ownership-checked protocol for controlling a dedicated local OBS encoder.
package obspreview

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image/jpeg"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	previewWidth   = 640
	previewHeight  = 360
	maxMessage     = 6 << 20
	maxJPEGBytes   = 4 << 20
	defaultTimeout = 15 * time.Second
	jpegDataPrefix = "data:image/jpeg;base64,"
)

var (
	// ErrInvalidPort means the caller did not provide a usable numeric TCP port.
	ErrInvalidPort = errors.New("invalid OBS preview port")
	// ErrUnavailable means the local OBS WebSocket endpoint could not be reached.
	ErrUnavailable = errors.New("OBS preview endpoint unavailable")
	// ErrProtocol means the endpoint did not follow the expected OBS v5 read protocol.
	ErrProtocol = errors.New("invalid OBS preview protocol response")
	// ErrAuthentication means the endpoint required authentication which could not succeed.
	ErrAuthentication = errors.New("OBS preview authentication failed")
	// ErrRequestRejected means OBS explicitly rejected one of the two read requests.
	ErrRequestRejected = errors.New("OBS preview request rejected")
	// ErrInvalidFrame means the returned screenshot was not the requested bounded JPEG.
	ErrInvalidFrame = errors.New("invalid OBS preview frame")
)

// Frame is a verified current-program-scene preview. ImageData is always a
// canonical data:image/jpeg;base64 URL for a decoded JPEG no larger than
// 640x360. OBS may aspect-fit a source on older server versions.
type Frame struct {
	Scene     string `json:"scene"`
	ImageData string `json:"imageData"`
}

// Capture connects only to a numeric loopback OBS WebSocket port and returns a
// verified screenshot of the current program scene. password is never retained
// or included in returned errors.
func Capture(ctx context.Context, port int, password string) (Frame, error) {
	if port < 1 || port > 65535 {
		return Frame{}, ErrInvalidPort
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultTimeout)
		defer cancel()
	}
	return capture(ctx, port, password, websocket.Dialer{
		Proxy:            nil,
		HandshakeTimeout: defaultTimeout,
	})
}

func capture(ctx context.Context, port int, password string, dialer websocket.Dialer) (frame Frame, err error) {
	err = withOBS(ctx, port, password, dialer, func(ctx context.Context, conn *websocket.Conn) error {
		scene, err := requestScene(ctx, conn)
		if err != nil {
			return err
		}
		imageData, err := requestScreenshot(ctx, conn, scene)
		if err != nil {
			return err
		}
		frame = Frame{Scene: scene, ImageData: imageData}
		return nil
	})
	return frame, err
}

func withOBS(ctx context.Context, port int, password string, dialer websocket.Dialer, use func(context.Context, *websocket.Conn) error) error {
	endpoint := url.URL{Scheme: "ws", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}
	conn, response, err := dialer.DialContext(ctx, endpoint.String(), nil)
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		if errors.Is(err, ErrStreamEndpoint) {
			return ErrStreamEndpoint
		}
		return connectionError(ctx)
	}
	defer conn.Close()
	// Closing this private connection wakes Gorilla's blocking reads on cancel.
	stopCancelWatch := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopCancelWatch()
	conn.SetReadLimit(maxMessage)

	hello, err := readHello(ctx, conn)
	if err != nil {
		return err
	}
	if err := identify(ctx, conn, hello, password); err != nil {
		return err
	}
	return use(ctx, conn)
}

type envelope struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
}

type helloData struct {
	RPCVersion     int                  `json:"rpcVersion"`
	Authentication *helloAuthentication `json:"authentication"`
}

type helloAuthentication struct {
	Challenge string `json:"challenge"`
	Salt      string `json:"salt"`
}

func readHello(ctx context.Context, conn *websocket.Conn) (helloData, error) {
	env, err := readEnvelope(ctx, conn)
	if err != nil {
		return helloData{}, err
	}
	if env.Op != 0 {
		return helloData{}, ErrProtocol
	}
	var hello helloData
	if err := json.Unmarshal(env.D, &hello); err != nil || hello.RPCVersion != 1 {
		return helloData{}, ErrProtocol
	}
	if hello.Authentication == nil || !validChallenge(hello.Authentication.Challenge) || !validChallenge(hello.Authentication.Salt) {
		return helloData{}, ErrProtocol
	}
	return hello, nil
}

func identify(ctx context.Context, conn *websocket.Conn, hello helloData, password string) error {
	payload := struct {
		RPCVersion         int    `json:"rpcVersion"`
		Authentication     string `json:"authentication"`
		EventSubscriptions int    `json:"eventSubscriptions"`
	}{RPCVersion: 1, EventSubscriptions: 0}
	if password == "" {
		return ErrAuthentication
	}
	authentication, err := challengeResponse(password, hello.Authentication.Salt, hello.Authentication.Challenge)
	if err != nil {
		return ErrProtocol
	}
	payload.Authentication = authentication
	if err := writeEnvelope(ctx, conn, 1, payload); err != nil {
		return err
	}
	env, err := readEnvelope(ctx, conn)
	if err != nil {
		return err
	}
	if env.Op != 2 {
		return ErrProtocol
	}
	var identified struct {
		NegotiatedRPCVersion int `json:"negotiatedRpcVersion"`
	}
	if err := json.Unmarshal(env.D, &identified); err != nil || identified.NegotiatedRPCVersion != 1 {
		return ErrProtocol
	}
	return nil
}

func requestScene(ctx context.Context, conn *websocket.Conn) (string, error) {
	var response struct {
		CurrentProgramSceneName string `json:"currentProgramSceneName"`
	}
	if err := request(ctx, conn, "GetCurrentProgramScene", struct{}{}, &response); err != nil {
		return "", err
	}
	if !validScene(response.CurrentProgramSceneName) {
		return "", ErrProtocol
	}
	return response.CurrentProgramSceneName, nil
}

func requestScreenshot(ctx context.Context, conn *websocket.Conn, scene string) (string, error) {
	var response struct {
		ImageData string `json:"imageData"`
	}
	requestData := struct {
		SourceName              string `json:"sourceName"`
		ImageFormat             string `json:"imageFormat"`
		ImageWidth              int    `json:"imageWidth"`
		ImageHeight             int    `json:"imageHeight"`
		ImageCompressionQuality int    `json:"imageCompressionQuality"`
	}{scene, "jpeg", previewWidth, previewHeight, 80}
	if err := request(ctx, conn, "GetSourceScreenshot", requestData, &response); err != nil {
		return "", err
	}
	return normalizeJPEG(response.ImageData)
}

func request(ctx context.Context, conn *websocket.Conn, requestType string, requestData any, responseData any) error {
	id := uuid.NewString()
	payload := struct {
		RequestType string `json:"requestType"`
		RequestID   string `json:"requestId"`
		RequestData any    `json:"requestData"`
	}{requestType, id, requestData}
	if err := writeEnvelope(ctx, conn, 6, payload); err != nil {
		return err
	}
	env, err := readEnvelope(ctx, conn)
	if err != nil {
		return err
	}
	if env.Op != 7 {
		return ErrProtocol
	}
	var response struct {
		RequestType   string `json:"requestType"`
		RequestID     string `json:"requestId"`
		RequestStatus *struct {
			Result bool `json:"result"`
			Code   int  `json:"code"`
		} `json:"requestStatus"`
		ResponseData json.RawMessage `json:"responseData"`
	}
	if err := json.Unmarshal(env.D, &response); err != nil || response.RequestType != requestType || response.RequestID != id || response.RequestStatus == nil {
		return ErrProtocol
	}
	if !response.RequestStatus.Result || response.RequestStatus.Code != 100 {
		return ErrRequestRejected
	}
	if responseData == nil {
		return nil
	}
	if len(response.ResponseData) == 0 || json.Unmarshal(response.ResponseData, responseData) != nil {
		return ErrProtocol
	}
	return nil
}

func writeEnvelope(ctx context.Context, conn *websocket.Conn, op int, data any) error {
	if err := applyDeadline(ctx, conn.SetWriteDeadline); err != nil {
		return err
	}
	if err := conn.WriteJSON(struct {
		Op int `json:"op"`
		D  any `json:"d"`
	}{op, data}); err != nil {
		return connectionError(ctx)
	}
	return nil
}

func readEnvelope(ctx context.Context, conn *websocket.Conn) (envelope, error) {
	if err := applyDeadline(ctx, conn.SetReadDeadline); err != nil {
		return envelope{}, err
	}
	messageType, body, err := conn.ReadMessage()
	if err != nil {
		var closeError *websocket.CloseError
		if errors.As(err, &closeError) && closeError.Code == 4005 {
			return envelope{}, ErrAuthentication
		}
		return envelope{}, connectionError(ctx)
	}
	if messageType != websocket.TextMessage || len(body) == 0 || len(body) > maxMessage {
		return envelope{}, ErrProtocol
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil || env.D == nil {
		return envelope{}, ErrProtocol
	}
	return env, nil
}

func applyDeadline(ctx context.Context, set func(time.Time) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(defaultTimeout)
	}
	if err := set(deadline); err != nil {
		return ErrUnavailable
	}
	return nil
}

func connectionError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrUnavailable
}

func validChallenge(value string) bool {
	if value == "" || len(value) > 4096 {
		return false
	}
	_, err := base64.StdEncoding.DecodeString(value)
	return err == nil
}

func challengeResponse(password, salt, challenge string) (string, error) {
	if !validChallenge(salt) || !validChallenge(challenge) {
		return "", ErrProtocol
	}
	secret := sha256.Sum256([]byte(password + salt))
	secretBase64 := base64.StdEncoding.EncodeToString(secret[:])
	auth := sha256.Sum256([]byte(secretBase64 + challenge))
	return base64.StdEncoding.EncodeToString(auth[:]), nil
}

func validScene(scene string) bool {
	if strings.TrimSpace(scene) == "" || len(scene) > 512 || !utf8.ValidString(scene) {
		return false
	}
	return !strings.ContainsAny(scene, "\x00\r\n")
}

func normalizeJPEG(data string) (string, error) {
	prefix := ""
	switch {
	case strings.HasPrefix(data, jpegDataPrefix):
		prefix = jpegDataPrefix
	case strings.HasPrefix(data, "data:image/jpg;base64,"):
		prefix = "data:image/jpg;base64,"
	default:
		return "", ErrInvalidFrame
	}
	encoded := strings.TrimPrefix(data, prefix)
	if encoded == "" || len(encoded) > base64.StdEncoding.EncodedLen(maxJPEGBytes) {
		return "", ErrInvalidFrame
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(raw) == 0 || len(raw) > maxJPEGBytes {
		return "", ErrInvalidFrame
	}
	decoded, err := jpeg.Decode(bytes.NewReader(raw))
	width, height := decoded.Bounds().Dx(), decoded.Bounds().Dy()
	if err != nil || width < 1 || height < 1 || width > previewWidth || height > previewHeight {
		return "", ErrInvalidFrame
	}
	return jpegDataPrefix + base64.StdEncoding.EncodeToString(raw), nil
}
