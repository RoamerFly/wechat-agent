package stationbrowser

import (
	"context"
	"errors"
)

type CreatorPublicationRequest struct {
	TaskID, ManifestSHA256  string
	Title, Body, Visibility string
	MediaPath               string
	MediaBytes              int64
	Submitted               bool
	RemoteID                string
	Query                   *CreatorPublicationQuery
	BeforeSubmit            func(context.Context) error
	Progress                func(context.Context, string) error
}

type CreatorPublicationResult struct {
	Stage                string
	RemoteID             string
	Submitted, Published bool
	RejectionCode        int
}

type CreatorPublicationQuery struct {
	Title, Body, Visibility, CreatedAt string
}

var (
	ErrCreatorPublicationInput      = errors.New("抖音作品参数或原件无效")
	ErrCreatorPublicationControls   = errors.New("创作者中心暂未提供可核对的上传、文案或发布控件")
	ErrCreatorPublicationUpload     = errors.New("创作者中心上传尚未完成")
	ErrCreatorPublicationVisibility = errors.New("创作者中心未确认作品可见范围")
	ErrCreatorPublicationCaption    = errors.New("创作者中心未确认填写的标题或简介")
	ErrCreatorPublicationUnknown    = errors.New("作品已进入提交核对，尚未取得可核对的作品编号")
	ErrCreatorPublicationRejected   = errors.New("创作者中心拒绝提交作品")
)
