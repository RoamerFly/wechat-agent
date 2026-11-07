package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// These are the documented user-authorized publishing endpoints.  Keep them
// separate from the public-search endpoints: publishing must never acquire a
// browser cookie or follow an interactive redirect.
const (
	douyinUploadVideoEndpoint        = "https://open.douyin.com/api/douyin/v1/video/upload_video/"
	douyinInitPartUploadEndpoint     = "https://open.douyin.com/api/douyin/v1/video/init_video_part_upload/"
	douyinUploadPartEndpoint         = "https://open.douyin.com/api/douyin/v1/video/upload_video_part/"
	douyinCompletePartUploadEndpoint = "https://open.douyin.com/api/douyin/v1/video/complete_video_part_upload/"
	douyinCreateVideoEndpoint        = "https://open.douyin.com/api/douyin/v1/video/create_video/"

	douyinPublicationMaxBytes = int64(4 << 30)
	douyinPublicationTimeout  = 3 * time.Minute
)

var (
	// errDouyinPublishRejected is deliberately reserved for a documented,
	// explicit create-video refusal for which the provider guarantees no item
	// was created.  Transport failures and provider busy responses are
	// ambiguous and must remain retry/query candidates.
	errDouyinPublishRejected      = errors.New("抖音拒绝创建视频")
	errDouyinPublishInvalid       = errors.New("抖音发布参数无效")
	errDouyinPublishAuthorization = errors.New("抖音发布授权不可用")
	errDouyinPublishUnavailable   = errors.New("抖音发布服务暂不可用")
	errDouyinPublicationBodySize  = errors.New("抖音上传内容长度与声明不一致")
)

// douyinPublishReceipt identifies an accepted create request.  ItemID is an
// opaque provider identifier; receiving it does not claim that the video has
// become public or passed later platform review.
type douyinPublishReceipt struct {
	VideoID string
	ItemID  string
}

type douyinPublicationResponse struct {
	Data *struct {
		ErrorCode   *int            `json:"error_code"`
		Description string          `json:"description"`
		UploadID    string          `json:"upload_id"`
		VideoID     json.RawMessage `json:"video_id"`
		ItemID      string          `json:"item_id"`
		Video       *struct {
			VideoID string `json:"video_id"`
		} `json:"video"`
	} `json:"data"`
	Extra *struct {
		ErrorCode *int `json:"error_code"`
	} `json:"extra"`
}

// uploadPublicationVideo performs the documented one-shot multipart upload.
// It is intentionally not a create fallback: callers must separately decide
// whether and when an uploaded video should be published.
func (p *SearchApp) uploadPublicationVideo(ctx context.Context, account accountAuthorization, name string, reader io.Reader, size int64) (string, error) {
	if !validPublicationAccount(p, account) || !safePublicationFileName(name) || reader == nil || size < 1 || size > douyinPublicationMaxBytes {
		return "", errDouyinPublishInvalid
	}
	body, contentType, contentLength, err := publicationMultipartBody(reader, name, size)
	if err != nil {
		return "", errDouyinPublishInvalid
	}
	decoded, err := p.callPublication(ctx, douyinUploadVideoEndpoint, account, nil, body, contentType, contentLength, true, "upload")
	if err != nil {
		return "", err
	}
	videoID, ok := publicationUploadVideoID(decoded)
	if !ok {
		return "", errDouyinPublishUnavailable
	}
	return videoID, nil
}

func (p *SearchApp) initializePublicationUpload(ctx context.Context, account accountAuthorization) (string, error) {
	if !validPublicationAccount(p, account) {
		return "", errDouyinPublishInvalid
	}
	decoded, err := p.callPublication(ctx, douyinInitPartUploadEndpoint, account, nil, nil, "application/json", 0, false, "init")
	if err != nil {
		return "", err
	}
	if decoded.Data == nil || !safeOpaque(decoded.Data.UploadID, 1, 4096) {
		return "", errDouyinPublishUnavailable
	}
	return decoded.Data.UploadID, nil
}

func (p *SearchApp) uploadPublicationPart(ctx context.Context, account accountAuthorization, uploadID string, part int64, data []byte) error {
	if !validPublicationAccount(p, account) || !validDouyinCheckpointID(uploadID) || part < 1 || len(data) == 0 || int64(len(data)) > 100<<20 {
		return errDouyinPublishInvalid
	}
	body, contentType, contentLength, err := publicationMultipartBody(bytes.NewReader(data), "part.mp4", int64(len(data)))
	if err != nil {
		return errDouyinPublishInvalid
	}
	query := url.Values{"upload_id": {uploadID}, "part_number": {strconv.FormatInt(part, 10)}}
	_, err = p.callPublication(ctx, douyinUploadPartEndpoint, account, query, body, contentType, contentLength, true, "part")
	return err
}

func (p *SearchApp) completePublicationUpload(ctx context.Context, account accountAuthorization, uploadID string) (string, error) {
	if !validPublicationAccount(p, account) || !validDouyinCheckpointID(uploadID) {
		return "", errDouyinPublishInvalid
	}
	decoded, err := p.callPublication(ctx, douyinCompletePartUploadEndpoint, account, url.Values{"upload_id": {uploadID}}, nil, "application/json", 0, false, "complete")
	if err != nil {
		return "", err
	}
	videoID, ok := publicationUploadVideoID(decoded)
	if !ok {
		return "", errDouyinPublishUnavailable
	}
	return videoID, nil
}

func (p *SearchApp) createPublicationVideo(ctx context.Context, account accountAuthorization, uploadVideoID, text string, visibility int) (douyinPublishReceipt, error) {
	if !validPublicationAccount(p, account) || !validDouyinCheckpointID(uploadVideoID) || !utf8.ValidString(text) || utf8.RuneCountInString(text) > 1000 || visibility < 0 || visibility > 2 {
		return douyinPublishReceipt{}, errDouyinPublishInvalid
	}
	payload, err := json.Marshal(struct {
		VideoID       string `json:"video_id"`
		Text          string `json:"text"`
		PrivateStatus int    `json:"private_status"`
	}{VideoID: uploadVideoID, Text: text, PrivateStatus: visibility})
	if err != nil {
		return douyinPublishReceipt{}, errDouyinPublishInvalid
	}
	decoded, err := p.callPublication(ctx, douyinCreateVideoEndpoint, account, nil, bytes.NewReader(payload), "application/json", int64(len(payload)), false, "create")
	if err != nil {
		return douyinPublishReceipt{}, err
	}
	if decoded.Data == nil {
		return douyinPublishReceipt{}, errDouyinPublishUnavailable
	}
	var videoID string
	if len(decoded.Data.VideoID) > 0 {
		var ok bool
		videoID, ok = canonicalItemID(decoded.Data.VideoID)
		if !ok {
			return douyinPublishReceipt{}, errDouyinPublishUnavailable
		}
	}
	itemID := decoded.Data.ItemID
	if itemID != "" && !validDouyinCheckpointID(itemID) {
		return douyinPublishReceipt{}, errDouyinPublishUnavailable
	}
	if videoID == "" && itemID == "" {
		return douyinPublishReceipt{}, errDouyinPublishUnavailable
	}
	return douyinPublishReceipt{VideoID: videoID, ItemID: itemID}, nil
}

func validPublicationAccount(p *SearchApp, account accountAuthorization) bool {
	return p != nil && p.client != nil && account.PublishVideo && openID.MatchString(account.OpenID) && accessToken.MatchString(account.AccessToken) && p.nowUTC().Before(account.ExpiresAt)
}

func safePublicationFileName(name string) bool {
	return name != "" && len(name) <= 255 && filepath.Base(name) == name && !strings.ContainsAny(name, `/\\`+string([]byte{0, 13, 10}))
}

func publicationUploadVideoID(decoded douyinPublicationResponse) (string, bool) {
	if decoded.Data == nil {
		return "", false
	}
	if decoded.Data.Video != nil && validDouyinCheckpointID(decoded.Data.Video.VideoID) {
		return decoded.Data.Video.VideoID, true
	}
	var value string
	if json.Unmarshal(decoded.Data.VideoID, &value) != nil || !validDouyinCheckpointID(value) {
		return "", false
	}
	return value, true
}

// publicationMultipartBody streams one bounded file between a small prefix
// and suffix. It avoids both whole-video buffering and a producer goroutine:
// an early transport exit cannot leave a pipe writer blocked in the background.
func publicationMultipartBody(reader io.Reader, name string, size int64) (io.ReadCloser, string, int64, error) {
	var prefix bytes.Buffer
	writer := multipart.NewWriter(&prefix)
	if _, err := writer.CreateFormFile("video", name); err != nil {
		return nil, "", 0, err
	}
	boundary := writer.Boundary()
	var suffix bytes.Buffer
	closer := multipart.NewWriter(&suffix)
	if err := closer.SetBoundary(boundary); err != nil {
		return nil, "", 0, err
	}
	if err := closer.Close(); err != nil {
		return nil, "", 0, err
	}
	contentLength := int64(prefix.Len()+suffix.Len()) + size
	stream := io.MultiReader(bytes.NewReader(prefix.Bytes()), &publicationSizedReader{reader: reader, remaining: size}, bytes.NewReader(suffix.Bytes()))
	return io.NopCloser(stream), writer.FormDataContentType(), contentLength, nil
}

type publicationSizedReader struct {
	reader    io.Reader
	remaining int64
	checked   bool
}

func (r *publicationSizedReader) Read(buffer []byte) (int, error) {
	if r.remaining > 0 {
		if int64(len(buffer)) > r.remaining {
			buffer = buffer[:r.remaining]
		}
		n, err := r.reader.Read(buffer)
		r.remaining -= int64(n)
		if err == io.EOF && r.remaining != 0 {
			return n, errDouyinPublicationBodySize
		}
		return n, err
	}
	if r.checked {
		return 0, io.EOF
	}
	r.checked = true
	var extra [1]byte
	n, err := r.reader.Read(extra[:])
	if n > 0 {
		return 0, errDouyinPublicationBodySize
	}
	if err == nil {
		return 0, errDouyinPublicationBodySize
	}
	if err == io.EOF {
		return 0, io.EOF
	}
	return 0, err
}

func (p *SearchApp) callPublication(ctx context.Context, rawEndpoint string, account accountAuthorization, additional url.Values, body io.Reader, contentType string, contentLength int64, longUpload bool, operation string) (douyinPublicationResponse, error) {
	endpoint, err := url.Parse(rawEndpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host != "open.douyin.com" || endpoint.User != nil {
		return douyinPublicationResponse{}, errDouyinPublishUnavailable
	}
	query := endpoint.Query()
	query.Set("open_id", account.OpenID)
	for key, values := range additional {
		for _, value := range values {
			query.Add(key, value)
		}
	}
	endpoint.RawQuery = query.Encode()

	timeout := requestTimeout
	if longUpload {
		timeout = douyinPublicationTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, endpoint.String(), body)
	if err != nil {
		return douyinPublicationResponse{}, errDouyinPublishUnavailable
	}
	req.ContentLength = contentLength
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", "Agent-home-station/1.0 douyin-publication")
	req.Header.Set("access-token", account.AccessToken)
	client := *p.client // do not mutate the shared client used by observations.
	client.Timeout = timeout
	response, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return douyinPublicationResponse{}, ctx.Err()
		}
		return douyinPublicationResponse{}, errDouyinPublishUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusPreconditionFailed {
		return douyinPublicationResponse{}, errDouyinPublishUnavailable
	}
	if response.StatusCode != http.StatusOK {
		return douyinPublicationResponse{}, errDouyinPublishUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(raw) > maxResponseBytes {
		return douyinPublicationResponse{}, errDouyinPublishUnavailable
	}
	var decoded douyinPublicationResponse
	if json.Unmarshal(raw, &decoded) != nil {
		return douyinPublicationResponse{}, errDouyinPublishUnavailable
	}
	if decoded.Data != nil && decoded.Data.ErrorCode != nil && *decoded.Data.ErrorCode != 0 {
		return douyinPublicationResponse{}, publicationResponseError(operation, *decoded.Data.ErrorCode)
	}
	if decoded.Extra != nil && decoded.Extra.ErrorCode != nil && *decoded.Extra.ErrorCode != 0 {
		return douyinPublicationResponse{}, publicationResponseError(operation, *decoded.Extra.ErrorCode)
	}
	if decoded.Data == nil || decoded.Data.ErrorCode == nil || decoded.Extra == nil || decoded.Extra.ErrorCode == nil {
		return douyinPublicationResponse{}, errDouyinPublishUnavailable
	}
	return decoded, nil
}

func publicationResponseError(operation string, code int) error {
	switch code {
	case 2190002, 2190008, 28001003, 28001008, 28001014, 28001016, 28001018, 28001019:
		return errDouyinPublishAuthorization
	case 2114007, 2190007, 2114006, 2100005, 28001007:
		if operation == "create" {
			return errDouyinPublishRejected
		}
		return errDouyinPublishInvalid
	case 2190005, 2114020, 2114021, 2114022, 2114026:
		return errDouyinPublishInvalid
	default:
		return errDouyinPublishUnavailable
	}
}
