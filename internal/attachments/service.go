// Package attachments 实现加密附件（如药物照片）的落盘、读取、删除与列表。
//
// 【安全前提】MedDesk 采用端到端加密（E2EE）：客户端在本地把文件压缩并用
// AES-GCM 加密后再上传，服务端收到并落盘的**永远是密文**（iv + ct）。
// 服务端没有数据密钥，因此本包不做任何解码、转码、缩略图生成或内容嗅探，
// 也不可能对上传内容做真实性校验——mime 只能信任客户端声明（见 http.go）。
package attachments

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/config"
	"github.com/qiubite666/meddesk/internal/model"
	"github.com/qiubite666/meddesk/internal/store"
)

// ivLen 是 AES-GCM 初始向量的长度（字节）。
const ivLen = 12

// dirPerm / filePerm 分别是密文目录与密文文件的权限：仅属主可读写。
const (
	dirPerm  = 0o700
	filePerm = 0o600
)

// Service 是附件服务，负责密文落盘与元数据读写。
type Service struct {
	repo store.AttachmentRepo
	cfg  config.UploadConfig
}

// NewService 构造附件服务。cfg.Dir 为密文根目录，cfg.MaxBytes 为单附件上限。
func NewService(repo store.AttachmentRepo, cfg config.UploadConfig) *Service {
	return &Service{repo: repo, cfg: cfg}
}

// Save 把密文流写入 {UploadDir}/{userID}/{yy}/{id}.bin 并登记元数据。
// 先写同目录临时文件再 os.Rename，保证落盘原子性（不会出现半截文件）。
// size 为声明大小；实际读取超过 size 时按超限处理。
func (s *Service) Save(ctx context.Context, userID, recordID string, iv []byte, mime string, r io.Reader, size int64) (*model.AttachmentDTO, error) {
	if userID == "" {
		return nil, apierr.New(apierr.CodeUnauthorized, "未登录")
	}
	if strings.Contains(userID, "..") || strings.ContainsAny(userID, `/\`) {
		return nil, apierr.New(apierr.CodeBadRequest, "用户标识非法")
	}
	if len(iv) != ivLen {
		return nil, apierr.Newf(apierr.CodeBadRequest, "iv 必须为 %d 字节", ivLen)
	}
	if size < 0 {
		return nil, apierr.New(apierr.CodeBadRequest, "附件大小非法")
	}
	if max := s.maxBytes(); max > 0 && size > max {
		return nil, apierr.Newf(apierr.CodePayloadTooLarge, "附件大小超过上限 %d 字节", max)
	}

	now := time.Now()
	id := uuid.NewString()
	// rel 是存库的相对路径，统一使用 '/' 分隔，与操作系统无关。
	rel := path.Join(userID, now.Format("06"), id+".bin") // "06" = 年份后两位
	abs := filepath.Join(s.cfg.Dir, filepath.FromSlash(rel))

	if err := os.MkdirAll(filepath.Dir(abs), dirPerm); err != nil {
		return nil, apierr.Wrap(apierr.CodeInternal, err, "创建附件目录失败")
	}

	if err := writeFileAtomic(abs, r, size); err != nil {
		return nil, err
	}

	a := &model.Attachment{
		ID:        id,
		UserID:    userID,
		RecordID:  recordID,
		IV:        iv,
		Size:      size,
		Mime:      mime,
		Path:      rel,
		CreatedAt: now,
	}
	if err := s.repo.Create(ctx, a); err != nil {
		// 元数据登记失败：回收已落盘的密文，避免产生孤儿文件。
		_ = os.Remove(abs)
		return nil, apierr.Wrap(apierr.CodeInternal, err, "保存附件元数据失败")
	}
	dto := toDTO(*a)
	return &dto, nil
}

// Open 取出附件元数据并打开密文文件。调用方负责关闭返回的 ReadCloser。
// 越权或不存在一律返回 CodeNotFound，避免通过错误码差异枚举他人资源。
func (s *Service) Open(ctx context.Context, userID, id string) (*model.Attachment, io.ReadCloser, error) {
	if userID == "" {
		return nil, nil, apierr.New(apierr.CodeUnauthorized, "未登录")
	}
	a, err := s.repo.Get(ctx, userID, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, apierr.New(apierr.CodeNotFound, "附件不存在")
	}
	if err != nil {
		return nil, nil, apierr.Wrap(apierr.CodeInternal, err, "读取附件元数据失败")
	}
	abs, err := s.absPath(a.Path)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.Open(abs)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, apierr.New(apierr.CodeNotFound, "附件文件已丢失")
	}
	if err != nil {
		return nil, nil, apierr.Wrap(apierr.CodeInternal, err, "打开附件失败")
	}
	return a, f, nil
}

// Delete 先删数据库记录再删密文文件；文件删除失败只记录 warn，不影响返回。
func (s *Service) Delete(ctx context.Context, userID, id string) error {
	if userID == "" {
		return apierr.New(apierr.CodeUnauthorized, "未登录")
	}
	// 先读一次拿到落盘路径（删库后就拿不到了）。
	a, err := s.repo.Get(ctx, userID, id)
	if errors.Is(err, store.ErrNotFound) {
		return apierr.New(apierr.CodeNotFound, "附件不存在")
	}
	if err != nil {
		return apierr.Wrap(apierr.CodeInternal, err, "读取附件元数据失败")
	}
	if err := s.repo.Delete(ctx, userID, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return apierr.New(apierr.CodeNotFound, "附件不存在")
		}
		return apierr.Wrap(apierr.CodeInternal, err, "删除附件记录失败")
	}
	abs, err := s.absPath(a.Path)
	if err != nil {
		slog.Default().Warn("附件记录已删除但路径非法，跳过文件清理",
			slog.String("user_id", userID), slog.String("id", id), slog.String("path", a.Path))
		return nil
	}
	if err := os.Remove(abs); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Default().Warn("附件文件删除失败（记录已删除，残留孤儿文件）",
			slog.String("user_id", userID), slog.String("id", id), slog.String("path", abs), slog.String("err", err.Error()))
	}
	return nil
}

// List 列出指定 recordID 下的附件；recordID 为空表示列出该用户全部附件。
func (s *Service) List(ctx context.Context, userID, recordID string) ([]model.AttachmentDTO, error) {
	if userID == "" {
		return nil, apierr.New(apierr.CodeUnauthorized, "未登录")
	}
	items, err := s.repo.List(ctx, userID, recordID)
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeInternal, err, "读取附件列表失败")
	}
	out := make([]model.AttachmentDTO, 0, len(items))
	for i := range items {
		out = append(out, toDTO(items[i]))
	}
	return out, nil
}

// maxBytes 返回单附件上限；<=0 表示不限制（由调用方自行兜底）。
func (s *Service) maxBytes() int64 { return s.cfg.MaxBytes }

// absPath 把库中的相对路径还原为绝对路径，并阻断路径穿越。
func (s *Service) absPath(rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) || strings.Contains(rel, "..") {
		return "", apierr.New(apierr.CodeInternal, "附件路径非法")
	}
	return filepath.Join(s.cfg.Dir, filepath.FromSlash(rel)), nil
}

// writeFileAtomic 先写临时文件再 rename，保证并发读到的一定是完整文件。
func writeFileAtomic(abs string, r io.Reader, size int64) error {
	dir := filepath.Dir(abs)
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		return apierr.Wrap(apierr.CodeInternal, err, "创建临时文件失败")
	}
	tmpName := tmp.Name()
	// 多读 1 字节用于发现「实际大小 > 声明大小」。
	n, err := io.Copy(tmp, &io.LimitedReader{R: r, N: size + 1})
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmpName)
		return apierr.Wrap(apierr.CodeBadRequest, err, "读取上传内容失败")
	}
	if n > size {
		_ = os.Remove(tmpName)
		return apierr.Newf(apierr.CodePayloadTooLarge, "附件实际大小超过声明的 %d 字节", size)
	}
	if err := os.Chmod(tmpName, filePerm); err != nil {
		_ = os.Remove(tmpName)
		return apierr.Wrap(apierr.CodeInternal, err, "设置附件权限失败")
	}
	if err := os.Rename(tmpName, abs); err != nil {
		_ = os.Remove(tmpName)
		return apierr.Wrap(apierr.CodeInternal, err, "保存附件失败")
	}
	return nil
}

// toDTO 把存储实体转成对外结构（iv 以标准 base64 输出）。
func toDTO(a model.Attachment) model.AttachmentDTO {
	return model.AttachmentDTO{
		ID:        a.ID,
		RecordID:  a.RecordID,
		IV:        base64.StdEncoding.EncodeToString(a.IV),
		Size:      a.Size,
		Mime:      a.Mime,
		CreatedAt: a.CreatedAt,
	}
}
