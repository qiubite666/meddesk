package ledger

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/fake"
	"github.com/qiubite666/meddesk/internal/model"
	"github.com/qiubite666/meddesk/internal/store"
)

// ------------------------------------------------------------------ 测试替身与脚手架

// ownerStub 是 RecordOwner 的最小替身：只认"账号 + 账单 id"都匹配的记录。
// 不匹配时返回 store.ErrNotFound（与真实仓储一致，服务层据此映射成 404）。
type ownerStub map[string]string // recordID -> userID

func (o ownerStub) GetRecord(ctx context.Context, userID, id string) (*model.LedgerRecord, error) {
	if o[id] != userID {
		return nil, store.ErrNotFound
	}
	return &model.LedgerRecord{ID: id, UserID: userID}, nil
}

// newHarness 组装一套测试环境：内存仓储 + 临时上传目录 + 一个已归属给 userA 的账单。
func newHarness(t *testing.T) (*AttachmentService, *fake.Repos, string, string, string) {
	t.Helper()
	userID := uuid.NewString()
	recordID := uuid.NewString()

	repos := fake.New()
	// 账单必须先存在：上传时要做归属校验。
	if err := repos.Ledger().CreateRecord(context.Background(), &model.LedgerRecord{
		ID: recordID, UserID: userID, Type: model.LedgerTypeExpense,
		Amount: "12.34", AmountCents: 1234, Currency: "CNY",
		CategoryName: "餐饮", TransactionAt: time.Now(), OccurredOn: "2026-10-29",
		ReimbursementStatus: model.LedgerReimbNone,
	}); err != nil {
		t.Fatalf("准备账单失败: %v", err)
	}

	dir := t.TempDir()
	svc := NewAttachmentService(dir, repos.LedgerAttachments(), ownerStub{recordID: userID})
	return svc, repos, dir, userID, recordID
}

// pngBytes 生成一张真实 PNG（尺寸 w×h），魔数与尺寸都可被解析。
func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewGray(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.Gray{Y: 128})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("生成 PNG 失败: %v", err)
	}
	return buf.Bytes()
}

func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatalf("生成 JPEG 失败: %v", err)
	}
	return buf.Bytes()
}

// webpBytes 拼一个 VP8X 容器头的 WebP（尺寸 w×h），足以通过魔数嗅探与尺寸解析。
// 结构：RIFF + 文件长度 + WEBP + "VP8X" + 块长度 + 10 字节画布头。
func webpBytes(w, h int) []byte {
	canvas := make([]byte, 10) // 1 标志 + 3 保留 + 3 宽 + 3 高
	canvas[4] = byte(w - 1)
	canvas[7] = byte(h - 1)

	chunk := append([]byte("VP8X"), make([]byte, 4)...) // 块长度占位
	chunk = append(chunk, canvas...)
	binary.LittleEndian.PutUint32(chunk[4:8], uint32(len(canvas)))

	out := []byte("RIFF")
	out = append(out, 0, 0, 0, 0) // 文件长度占位
	out = append(out, []byte("WEBP")...)
	out = append(out, chunk...)
	binary.LittleEndian.PutUint32(out[4:8], uint32(len(out)-8))
	return out
}

// ------------------------------------------------------------------ 服务层

func TestAttachmentSaveAcceptsImages(t *testing.T) {
	svc, _, dir, userID, recordID := newHarness(t)
	cases := []struct {
		name       string
		data       []byte
		wantMime   string
		wantWidth  int
		wantHeight int
	}{
		{"png", pngBytes(t, 3, 5), model.MimePNG, 3, 5},
		{"jpeg", jpegBytes(t, 8, 6), model.MimeJPEG, 8, 6},
		{"webp", webpBytes(12, 7), model.MimeWebP, 12, 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := svc.Save(context.Background(), userID, recordID, tc.data)
			if err != nil {
				t.Fatalf("保存 %s 失败: %v", tc.name, err)
			}
			if a.Mime != tc.wantMime {
				t.Errorf("mime = %s, 期望 %s", a.Mime, tc.wantMime)
			}
			if a.Bytes != int64(len(tc.data)) {
				t.Errorf("bytes = %d, 期望 %d", a.Bytes, len(tc.data))
			}
			if a.Width != tc.wantWidth || a.Height != tc.wantHeight {
				t.Errorf("尺寸 = %dx%d, 期望 %dx%d", a.Width, a.Height, tc.wantWidth, tc.wantHeight)
			}
			// 落盘路径必须是服务端生成、且位于该账号目录下。
			want := filepath.Join(dir, "ledger", userID, a.ID+"."+extOfMime(a.Mime))
			if _, err := os.Stat(want); err != nil {
				t.Fatalf("文件未落到期望位置 %s: %v", want, err)
			}
			if !strings.HasPrefix(a.RelPath, "ledger/"+userID+"/") {
				t.Errorf("rel_path 前缀异常: %s", a.RelPath)
			}
		})
	}
}

func TestAttachmentSaveRejectsOversize(t *testing.T) {
	svc, _, _, userID, recordID := newHarness(t)
	// 超过 2 MiB 的"图片"：即便魔数合法也必须被大小闸挡住。
	big := append(pngBytes(t, 1, 1), bytes.Repeat([]byte{0x00}, maxAttachmentBytes)...)
	_, err := svc.Save(context.Background(), userID, recordID, big)
	if !apierr.Is(err, apierr.CodePayloadTooLarge) {
		t.Fatalf("超限应返回 payload_too_large，实际: %v", err)
	}
}

func TestAttachmentSaveRejectsNonImage(t *testing.T) {
	svc, _, _, userID, recordID := newHarness(t)
	// 一段纯文本：客户端即便声明 image/png 也不该被接受（以魔数为准）。
	_, err := svc.Save(context.Background(), userID, recordID, []byte("这不是图片，只是一段文本"))
	if !apierr.Is(err, apierr.CodeBadRequest) {
		t.Fatalf("非图片应返回 bad_request，实际: %v", err)
	}
	if !strings.Contains(err.Error(), "只支持") {
		t.Errorf("错误文案应提示支持的类型，实际: %v", err)
	}
}

func TestAttachmentSaveRejectsForeignRecord(t *testing.T) {
	svc, _, _, _, _ := newHarness(t)
	// 别的账号 + 一条本替身里不存在的账单 → 必须 404，且不产生任何文件。
	_, err := svc.Save(context.Background(), uuid.NewString(), uuid.NewString(), pngBytes(t, 1, 1))
	if !apierr.Is(err, apierr.CodeNotFound) {
		t.Fatalf("账单不属于当前账号应返回 not_found，实际: %v", err)
	}
}

func TestAttachmentDeleteRemovesFile(t *testing.T) {
	svc, _, dir, userID, recordID := newHarness(t)
	a, err := svc.Save(context.Background(), userID, recordID, pngBytes(t, 2, 2))
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	abs := filepath.Join(dir, filepath.FromSlash(a.RelPath))
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("落盘文件不存在: %v", err)
	}
	if err := svc.Delete(context.Background(), userID, a.ID); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if _, err := os.Stat(abs); !os.IsNotExist(err) {
		t.Fatalf("删除后磁盘文件仍存在: %v", err)
	}
	// 越权删除同样 404。
	if err := svc.Delete(context.Background(), uuid.NewString(), a.ID); !apierr.Is(err, apierr.CodeNotFound) {
		t.Fatalf("他人删除应返回 not_found，实际: %v", err)
	}
}

func TestAttachmentDeleteByRecordCleansFiles(t *testing.T) {
	svc, _, dir, userID, recordID := newHarness(t)
	var abs []string
	for i := 0; i < 3; i++ {
		a, err := svc.Save(context.Background(), userID, recordID, pngBytes(t, 1+i, 1))
		if err != nil {
			t.Fatalf("保存第 %d 张失败: %v", i+1, err)
		}
		abs = append(abs, filepath.Join(dir, filepath.FromSlash(a.RelPath)))
	}
	if err := svc.DeleteByRecord(context.Background(), userID, recordID); err != nil {
		t.Fatalf("按账单清理失败: %v", err)
	}
	for _, p := range abs {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("DeleteByRecord 后文件仍存在: %s", p)
		}
	}
	items, err := svc.List(context.Background(), userID, recordID)
	if err != nil {
		t.Fatalf("列表失败: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("清理后仍残留 %d 条元数据", len(items))
	}
}

func TestAttachmentSourceContract(t *testing.T) {
	svc, _, _, userID, recordID := newHarness(t)
	a, err := svc.Save(context.Background(), userID, recordID, pngBytes(t, 4, 4))
	if err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	var src AttachmentSource = svc

	rc, err := src.Open(context.Background(), userID, a.ID)
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	rc.Close()
	// 越权 Open 必须 404（不泄露存在性）。
	if _, err := src.Open(context.Background(), uuid.NewString(), a.ID); !apierr.Is(err, apierr.CodeNotFound) {
		t.Fatalf("越权 Open 应返回 not_found，实际: %v", err)
	}

	refs, err := src.ListByRecord(context.Background(), userID, recordID)
	if err != nil {
		t.Fatalf("ListByRecord 失败: %v", err)
	}
	if len(refs) != 1 || refs[0].ID != a.ID || refs[0].Ext != model.ExtPNG || refs[0].Bytes != a.Bytes {
		t.Fatalf("附件清单不符合预期: %+v", refs)
	}
}

// ------------------------------------------------------------------ HTTP 层

// newRouter 搭一个只带凭证路由的 gin：中间件直接把 user_id 写进上下文，
// 模拟鉴权中间件（本模块不关心令牌怎么来的）。
func newRouter(t *testing.T, svc *AttachmentService, userID string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if userID != "" {
			c.Set(ctxUserIDKey, userID)
		}
		c.Next()
	})
	MountAttachments(r.Group("/api/v1"), svc, nil)
	return r
}

// doUpload 发一个 multipart 上传请求。
func doUpload(t *testing.T, r *gin.Engine, recordID, filename string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	if err := w.WriteField("recordId", recordID); err != nil {
		t.Fatalf("写字段失败: %v", err)
	}
	fw, err := w.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("建文件字段失败: %v", err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatalf("写文件内容失败: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("关闭表单失败: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ledger/attachments", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestHTTPUploadAndPreview(t *testing.T) {
	svc, _, _, userID, recordID := newHarness(t)
	r := newRouter(t, svc, userID)

	rec := doUpload(t, r, recordID, "receipt.png", pngBytes(t, 6, 6))
	if rec.Code != http.StatusCreated {
		t.Fatalf("上传应返回 201，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var got model.LedgerAttachment
	decodeJSON(t, rec.Body.Bytes(), &got)
	if got.Mime != model.MimePNG || got.RecordID != recordID {
		t.Fatalf("返回结构异常: %+v", got)
	}

	// 预览：Content-Type 与缓存头都要正确。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/ledger/attachments/"+got.ID, nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("预览应返回 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != model.MimePNG {
		t.Errorf("Content-Type = %s, 期望 %s", ct, model.MimePNG)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "private, no-store" {
		t.Errorf("Cache-Control = %q, 期望 private, no-store", cc)
	}
	if !bytes.Equal(rec.Body.Bytes(), pngBytes(t, 6, 6)) {
		t.Error("预览内容与上传内容不一致")
	}

	// 列表
	req = httptest.NewRequest(http.MethodGet, "/api/v1/ledger/attachments?recordId="+recordID, nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), got.ID) {
		t.Fatalf("列表异常: %d %s", rec.Code, rec.Body.String())
	}
}

func TestHTTPUploadRejectsOversizeAndNonImage(t *testing.T) {
	svc, _, _, userID, recordID := newHarness(t)
	r := newRouter(t, svc, userID)

	// 超过 2 MiB：请求体上限有余量，因此由服务层按文件字节数拒绝（413）。
	big := append(pngBytes(t, 1, 1), bytes.Repeat([]byte{0x00}, maxAttachmentBytes)...)
	rec := doUpload(t, r, recordID, "big.png", big)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限应返回 413，实际 %d: %s", rec.Code, rec.Body.String())
	}
	// 远超请求体上限时，在读阶段就被 MaxBytesReader 截断（同样是 413）。
	rec = doUpload(t, r, recordID, "huge.png", bytes.Repeat([]byte{0x41}, maxAttachmentBytes+1024*1024))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("超大请求体应返回 413，实际 %d: %s", rec.Code, rec.Body.String())
	}
	// 非图片：魔数不匹配 → 400。
	rec = doUpload(t, r, recordID, "note.png", []byte("这是一段文本，不是图片"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非图片应返回 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
	// 缺少 recordId / 账单不属于当前账号 → 400 / 404。
	if rec := doUpload(t, r, "", "x.png", pngBytes(t, 1, 1)); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺少 recordId 应返回 400，实际 %d", rec.Code)
	}
	if rec := doUpload(t, r, uuid.NewString(), "x.png", pngBytes(t, 1, 1)); rec.Code != http.StatusNotFound {
		t.Fatalf("账单不属于当前账号应返回 404，实际 %d", rec.Code)
	}
}

func TestHTTPPreviewCrossUserReturns404(t *testing.T) {
	svc, _, _, userID, recordID := newHarness(t)
	owner := newRouter(t, svc, userID)
	rec := doUpload(t, owner, recordID, "receipt.png", pngBytes(t, 2, 2))
	if rec.Code != http.StatusCreated {
		t.Fatalf("上传失败: %d", rec.Code)
	}
	var got model.LedgerAttachment
	decodeJSON(t, rec.Body.Bytes(), &got)

	// 另一个账号预览 / 删除他人凭证：一律 404（不区分"不存在"与"无权"）。
	other := newRouter(t, svc, uuid.NewString())
	req := httptest.NewRequest(http.MethodGet, "/api/v1/ledger/attachments/"+got.ID, nil)
	rec = httptest.NewRecorder()
	other.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("越权预览应返回 404，实际 %d: %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodDelete, "/api/v1/ledger/attachments/"+got.ID, nil)
	rec = httptest.NewRecorder()
	other.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("越权删除应返回 404，实际 %d: %s", rec.Code, rec.Body.String())
	}

	// 未登录（上下文里没有 user_id）时也不该给出资源。
	anon := newRouter(t, svc, "")
	req = httptest.NewRequest(http.MethodGet, "/api/v1/ledger/attachments/"+got.ID, nil)
	rec = httptest.NewRecorder()
	anon.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("未登录应返回 401，实际 %d", rec.Code)
	}
}

func TestHTTPDeleteRemovesFile(t *testing.T) {
	svc, _, dir, userID, recordID := newHarness(t)
	r := newRouter(t, svc, userID)
	rec := doUpload(t, r, recordID, "receipt.png", pngBytes(t, 2, 2))
	if rec.Code != http.StatusCreated {
		t.Fatalf("上传失败: %d", rec.Code)
	}
	var got model.LedgerAttachment
	decodeJSON(t, rec.Body.Bytes(), &got)

	// rel_path 不对外输出（避免暴露存储布局），因此从服务层取真实路径。
	stored, err := svc.Get(context.Background(), userID, got.ID)
	if err != nil {
		t.Fatalf("读取凭证失败: %v", err)
	}
	abs := filepath.Join(dir, filepath.FromSlash(stored.RelPath))
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("落盘失败: %v", err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/ledger/attachments/"+got.ID, nil)
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("删除应返回 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(abs); !os.IsNotExist(err) {
		t.Fatalf("删除后磁盘文件仍存在: %v", err)
	}
}

// decodeJSON 解析响应体（失败即视为测试失败）。
func decodeJSON(t *testing.T, body []byte, out any) {
	t.Helper()
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("解析响应失败: %v（body=%s）", err, body)
	}
}
