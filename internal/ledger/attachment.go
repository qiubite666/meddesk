package ledger

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/model"
	"github.com/qiubite666/meddesk/internal/store"
)

// maxAttachmentBytes 是单张凭证的大小上限（2 MiB），与迁移
// 0004_ledger.sql 里 ledger_attachments.bytes 的 CHECK 约束保持一致。
const maxAttachmentBytes = 2 << 20

// ledgerAttachmentDir 是凭证在上传根目录下的子目录名。
//
// 为什么要单独一层目录：与加密附件（密文）物理隔离，便于备份脚本按目录打包、
// 也避免误把明文凭证当成 E2EE 密文同步出去。
const ledgerAttachmentDir = "ledger"

// 磁盘权限：目录与文件都只对属主开放（凭证是明文，不能被同机其他用户读到）。
const (
	attachmentDirPerm  = 0o700
	attachmentFilePerm = 0o600
)

// RecordOwner 是"账单归属判定"的最小接口。
//
// 为什么只依赖这一个方法而不是整个 store.LedgerRepo：附件模块只需要确认
// recordId 确实属于当前账号，依赖越窄越好——store.LedgerRepo 天然满足它，
// 单元测试里也能用几行的替身替代。
type RecordOwner interface {
	GetRecord(ctx context.Context, userID, id string) (*model.LedgerRecord, error)
}

// AttachmentService 是账单凭证附件服务：落盘 + 元数据读写。
type AttachmentService struct {
	dir     string // 上传根目录（config.Upload.Dir）
	repo    store.LedgerAttachmentRepo
	records RecordOwner
}

// NewAttachmentService 构造凭证附件服务。
//
// uploadDir 是上传根目录（cfg.Upload.Dir），凭证落在它的 ledger/ 子目录下；
// records 用于校验账单归属（传 store.LedgerRepo 即可，例如 store.Ledger()）。
func NewAttachmentService(uploadDir string, repo store.LedgerAttachmentRepo, records RecordOwner) *AttachmentService {
	return &AttachmentService{dir: uploadDir, repo: repo, records: records}
}

// 编译期断言：凭证服务必须能满足导出模块（B6）依赖的附件来源契约。
var _ AttachmentSource = (*AttachmentService)(nil)

// Save 落盘一张凭证并登记元数据。
//
// data 是**完整的文件字节**（HTTP 层已按上限截断），本方法负责二次校验：
// 大小 → 真实类型（魔数嗅探）→ 账单归属 → 落盘 → 写库。
// 顺序上把"归属校验"放在落盘之前：账单不属于当前账号时不产生任何临时文件。
func (s *AttachmentService) Save(ctx context.Context, userID, recordID string, data []byte) (*model.LedgerAttachment, error) {
	userID = strings.TrimSpace(userID)
	recordID = strings.TrimSpace(recordID)
	if userID == "" {
		return nil, apierr.New(apierr.CodeUnauthorized, "未登录或令牌已过期")
	}
	if !safePathSegment(userID) {
		return nil, apierr.New(apierr.CodeBadRequest, "账号标识非法")
	}
	if recordID == "" {
		return nil, apierr.New(apierr.CodeBadRequest, "缺少 recordId")
	}
	if len(data) == 0 {
		return nil, apierr.New(apierr.CodeBadRequest, "文件内容为空")
	}
	if int64(len(data)) > maxAttachmentBytes {
		return nil, apierr.Newf(apierr.CodePayloadTooLarge, "单张凭证不能超过 %d 字节", maxAttachmentBytes)
	}
	// 只认魔数：客户端声明的 Content-Type/mime 字段可以随便伪造，
	// 拿它做白名单等于没有校验（攻击者照样能传 HTML 或可执行脚本）。
	mime, ext, ok := sniffImage(data)
	if !ok {
		return nil, apierr.New(apierr.CodeBadRequest, "只支持 JPG / PNG / WebP 图片")
	}
	if err := s.mustOwnRecord(ctx, userID, recordID); err != nil {
		return nil, err
	}
	if strings.TrimSpace(s.dir) == "" {
		return nil, apierr.New(apierr.CodeInternal, "上传目录未配置")
	}

	// id 由服务端生成并直接用于文件名：用户输入完全不参与路径拼装，
	// 这是防路径穿越的根本手段（而不是事后过滤 ../）。
	id := uuid.NewString()
	rel := path.Join(ledgerAttachmentDir, userID, id+"."+ext)
	abs := filepath.Join(s.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), attachmentDirPerm); err != nil {
		return nil, apierr.Wrap(apierr.CodeInternal, err, "创建凭证目录失败")
	}
	if err := os.WriteFile(abs, data, attachmentFilePerm); err != nil {
		return nil, apierr.Wrap(apierr.CodeInternal, err, "保存凭证文件失败")
	}
	w, h := imageSize(data, mime)
	a := &model.LedgerAttachment{
		ID: id, UserID: userID, RecordID: recordID, RelPath: rel,
		Mime: mime, Bytes: int64(len(data)), Width: w, Height: h,
	}
	if err := s.repo.Create(ctx, a); err != nil {
		// 元数据写库失败 → 回收已落盘的文件，避免留下没有主人、永远删不掉的孤儿。
		_ = os.Remove(abs)
		return nil, wrapRepoErr(err, "保存凭证失败")
	}
	return a, nil
}

// Get 读取凭证元数据；越权与不存在一律 404（不透露资源是否存在）。
func (s *AttachmentService) Get(ctx context.Context, userID, id string) (*model.LedgerAttachment, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, apierr.New(apierr.CodeUnauthorized, "未登录或令牌已过期")
	}
	a, err := s.repo.Get(ctx, userID, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, apierr.New(apierr.CodeNotFound, "凭证不存在")
	}
	if err != nil {
		return nil, wrapRepoErr(err, "读取凭证失败")
	}
	return a, nil
}

// OpenMeta 打开凭证文件并返回元数据；调用方负责关闭 rc。
func (s *AttachmentService) OpenMeta(ctx context.Context, userID, id string) (*model.LedgerAttachment, io.ReadCloser, error) {
	a, err := s.Get(ctx, userID, id)
	if err != nil {
		return nil, nil, err
	}
	abs, err := s.absPath(userID, a.RelPath)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.Open(abs)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, apierr.New(apierr.CodeNotFound, "凭证文件已丢失")
	}
	if err != nil {
		return nil, nil, apierr.Wrap(apierr.CodeInternal, err, "打开凭证失败")
	}
	return a, f, nil
}

// Open 实现 AttachmentSource：导出模块（B6）按此接口读取凭证字节。
func (s *AttachmentService) Open(ctx context.Context, userID, attachmentID string) (io.ReadCloser, error) {
	_, rc, err := s.OpenMeta(ctx, userID, attachmentID)
	if err != nil {
		return nil, err
	}
	return rc, nil
}

// List 列出某条账单下的凭证（完整元数据，供 HTTP 输出）。
func (s *AttachmentService) List(ctx context.Context, userID, recordID string) ([]model.LedgerAttachment, error) {
	userID = strings.TrimSpace(userID)
	recordID = strings.TrimSpace(recordID)
	if userID == "" {
		return nil, apierr.New(apierr.CodeUnauthorized, "未登录或令牌已过期")
	}
	if recordID == "" {
		return nil, apierr.New(apierr.CodeBadRequest, "缺少 recordId")
	}
	items, err := s.repo.ListByRecord(ctx, userID, recordID)
	if err != nil {
		return nil, wrapRepoErr(err, "读取凭证列表失败")
	}
	return items, nil
}

// ListByRecord 实现 AttachmentSource：导出模块按账单取附件清单。
//
// 只暴露导出必需的最小信息（id/recordId/mime/ext/bytes），不给出存储路径。
func (s *AttachmentService) ListByRecord(ctx context.Context, userID, recordID string) ([]AttachmentRef, error) {
	items, err := s.List(ctx, userID, recordID)
	if err != nil {
		return nil, err
	}
	out := make([]AttachmentRef, 0, len(items))
	for _, a := range items {
		out = append(out, AttachmentRef{
			ID: a.ID, RecordID: a.RecordID, Mime: a.Mime,
			Ext: extOfMime(a.Mime), Bytes: a.Bytes,
		})
	}
	return out, nil
}

// Delete 删除单条凭证：先删元数据（成功才拿到路径），再删磁盘文件。
func (s *AttachmentService) Delete(ctx context.Context, userID, id string) error {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return apierr.New(apierr.CodeUnauthorized, "未登录或令牌已过期")
	}
	rel, err := s.repo.Delete(ctx, userID, id)
	if errors.Is(err, store.ErrNotFound) {
		return apierr.New(apierr.CodeNotFound, "凭证不存在")
	}
	if err != nil {
		return wrapRepoErr(err, "删除凭证失败")
	}
	s.removeFiles(userID, []string{rel})
	return nil
}

// DeleteByRecord 删除某条账单下的全部凭证（元数据 + 磁盘文件）。
//
// 由删除账单的地方在**删账单之前**调用：外键 CASCADE 只会删元数据行，
// 磁盘文件必须靠这里回收，否则会积累大量孤儿图片。
func (s *AttachmentService) DeleteByRecord(ctx context.Context, userID, recordID string) error {
	userID = strings.TrimSpace(userID)
	recordID = strings.TrimSpace(recordID)
	if userID == "" {
		return apierr.New(apierr.CodeUnauthorized, "未登录或令牌已过期")
	}
	if recordID == "" {
		return apierr.New(apierr.CodeBadRequest, "缺少 recordId")
	}
	rels, err := s.repo.DeleteByRecord(ctx, userID, recordID)
	if err != nil {
		return wrapRepoErr(err, "清理账单凭证失败")
	}
	s.removeFiles(userID, rels)
	return nil
}

// mustOwnRecord 确认账单属于当前账号；越权与不存在都按 404 返回。
func (s *AttachmentService) mustOwnRecord(ctx context.Context, userID, recordID string) error {
	if s.records == nil {
		return apierr.New(apierr.CodeInternal, "凭证服务缺少账单归属校验")
	}
	if _, err := s.records.GetRecord(ctx, userID, recordID); err != nil {
		return wrapRepoErr(err, "账单不存在")
	}
	return nil
}

// absPath 把库中的相对路径还原成绝对路径，并阻断路径穿越。
//
// 双重防御：即便将来有人手工改库写入异常 rel_path，也必须满足
// "ledger/<userID>/" 前缀，才能被拼成路径——读不到他人目录。
func (s *AttachmentService) absPath(userID, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) || strings.Contains(rel, "..") {
		return "", apierr.New(apierr.CodeInternal, "凭证路径非法")
	}
	if !strings.HasPrefix(rel, path.Join(ledgerAttachmentDir, strings.TrimSpace(userID))+"/") {
		return "", apierr.New(apierr.CodeInternal, "凭证路径非法")
	}
	return filepath.Join(s.dir, filepath.FromSlash(rel)), nil
}

// removeFiles 删除磁盘文件。失败只记日志：元数据已删，残留文件不会对外可见，
// 不该因为它让整个删除操作失败（用户侧的语义是"已删除"）。
func (s *AttachmentService) removeFiles(userID string, rels []string) {
	for _, rel := range rels {
		abs, err := s.absPath(userID, rel)
		if err != nil {
			slog.Default().Warn("凭证路径非法，跳过磁盘清理",
				slog.String("user_id", userID), slog.String("err", err.Error()))
			continue
		}
		if err := os.Remove(abs); err != nil && !errors.Is(err, os.ErrNotExist) {
			// 只记相对路径：绝对路径属于部署信息，不进日志。
			slog.Default().Warn("凭证文件删除失败（元数据已删，残留孤儿文件）",
				slog.String("user_id", userID), slog.String("rel_path", rel), slog.String("err", err.Error()))
		}
	}
}

// safePathSegment 判断 s 是否可以安全地作为路径的一段。
func safePathSegment(s string) bool {
	return s != "" && !strings.Contains(s, "..") && !strings.ContainsAny(s, `/\`) && !filepath.IsAbs(s)
}

// sniffImage 按魔数判断真实图片类型，返回 (mime, 扩展名, 是否识别)。
//
// 不引入任何第三方依赖：JPEG/PNG 只需要比前几个字节，WebP 比 RIFF....WEBP 容器标识。
func sniffImage(b []byte) (string, string, bool) {
	switch {
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return model.MimeJPEG, model.ExtJPEG, true
	case len(b) >= 8 && bytes.Equal(b[:8], []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}):
		return model.MimePNG, model.ExtPNG, true
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return model.MimeWebP, model.ExtWebP, true
	}
	return "", "", false
}

// imageSize 解析图片像素尺寸（导出排版用）；解析不出来时返回 0，不影响业务。
func imageSize(b []byte, mime string) (int, int) {
	switch mime {
	case model.MimeJPEG:
		if cfg, err := jpeg.DecodeConfig(bytes.NewReader(b)); err == nil {
			return cfg.Width, cfg.Height
		}
	case model.MimePNG:
		if cfg, err := png.DecodeConfig(bytes.NewReader(b)); err == nil {
			return cfg.Width, cfg.Height
		}
	case model.MimeWebP:
		if w, h, ok := webpSize(b); ok {
			return w, h
		}
	}
	return 0, 0
}

// webpSize 从 WebP 容器头里读画布尺寸（标准库不认识 WebP，只能自己解析）。
//
// 覆盖三种子格式：VP8（有损）、VP8L（无损）、VP8X（扩展）。解析不出就返回 false，
// 尺寸只用于排版，不是安全边界。
func webpSize(b []byte) (int, int, bool) {
	if len(b) < 20 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WEBP" {
		return 0, 0, false
	}
	chunk := string(b[12:16])
	data := b[20:]
	switch chunk {
	case "VP8 ":
		// 3 字节帧头 + 3 字节同步码(9d 01 2a) + 各 2 字节宽高（低 14 位，存的是减 1 后的值）
		if len(data) < 10 {
			return 0, 0, false
		}
		w := int(binary.LittleEndian.Uint16(data[6:8])) & 0x3FFF
		h := int(binary.LittleEndian.Uint16(data[8:10])) & 0x3FFF
		return w + 1, h + 1, true
	case "VP8L":
		// 1 字节签名(0x2f) + 14 位宽 + 14 位高（均为减 1 后的值）
		if len(data) < 5 || data[0] != 0x2F {
			return 0, 0, false
		}
		v := binary.LittleEndian.Uint32(data[1:5])
		return int(v&0x3FFF) + 1, int((v>>14)&0x3FFF) + 1, true
	case "VP8X":
		// 1 字节标志 + 3 字节保留 + 3 字节宽 + 3 字节高（24 位小端，减 1 后的值）
		if len(data) < 10 {
			return 0, 0, false
		}
		w := int(data[4]) | int(data[5])<<8 | int(data[6])<<16
		h := int(data[7]) | int(data[8])<<8 | int(data[9])<<16
		return w + 1, h + 1, true
	}
	return 0, 0, false
}

// extOfMime 返回凭证的扩展名（不含点）；未知类型退回 bin。
func extOfMime(mime string) string {
	switch mime {
	case model.MimeJPEG:
		return model.ExtJPEG
	case model.MimePNG:
		return model.ExtPNG
	case model.MimeWebP:
		return model.ExtWebP
	}
	return "bin"
}
