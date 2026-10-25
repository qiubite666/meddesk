package attachments

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/qiubite666/meddesk/internal/config"
	"github.com/qiubite666/meddesk/internal/model"
)

// TestHTTPRoundTrip 临时冒烟：上传 → 下载 → 列表 → 删除。
func TestHTTPRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir, _ := os.MkdirTemp("", "md-http-")
	defer os.RemoveAll(dir)
	svc := NewService(&fakeRepo{items: map[string]*model.Attachment{}}, config.UploadConfig{Dir: dir, MaxBytes: 1 << 20})
	r := gin.New()
	Mount(r.Group("/api/v1"), svc, func(c *gin.Context) { c.Set(ctxUserIDKey, "u1"); c.Next() })

	data := []byte("cipher-bytes-123")
	iv := []byte("0123456789ab")
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "a.bin")
	fw.Write(data)
	_ = mw.WriteField("iv", base64.StdEncoding.EncodeToString(iv))
	_ = mw.WriteField("mime", "image/png")
	_ = mw.WriteField("recordId", "rec-1")
	mw.Close()

	req := httptest.NewRequest("POST", "/api/v1/attachments", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	t.Logf("upload: %d %s", rec.Code, rec.Body.String())
	if rec.Code != 200 {
		t.Fatalf("upload code=%d", rec.Code)
	}
	var dto model.AttachmentDTO
	json.Unmarshal(rec.Body.Bytes(), &dto)

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/attachments/"+dto.ID, nil))
	t.Logf("download: %d iv=%s len=%d ct=%s cc=%s", rec.Code, rec.Header().Get("X-MedDesk-IV"),
		rec.Body.Len(), rec.Header().Get("Content-Type"), rec.Header().Get("Cache-Control"))
	if !bytes.Equal(rec.Body.Bytes(), data) {
		t.Fatalf("下载内容不符")
	}

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/attachments?recordId=rec-1", nil))
	t.Logf("list: %d %s", rec.Code, rec.Body.String())

	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("DELETE", "/api/v1/attachments/"+dto.ID, nil))
	t.Logf("delete: %d", rec.Code)

	// 非法 mime 应被白名单拦下
	var b2 bytes.Buffer
	mw2 := multipart.NewWriter(&b2)
	fw2, _ := mw2.CreateFormFile("file", "a.bin")
	fw2.Write(data)
	_ = mw2.WriteField("iv", base64.StdEncoding.EncodeToString(iv))
	_ = mw2.WriteField("mime", "text/html")
	mw2.Close()
	req = httptest.NewRequest("POST", "/api/v1/attachments", &b2)
	req.Header.Set("Content-Type", mw2.FormDataContentType())
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	t.Logf("bad mime: %d %s", rec.Code, rec.Body.String())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 mime 应返回 400，实际 %d", rec.Code)
	}
}
