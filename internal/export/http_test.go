package export

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/qiubite666/meddesk/internal/config"
)

// TestMountCSV 冒烟：CSV 路由的响应头与文件名（ASCII + RFC 5987 中文名）。
func TestMountCSV(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	Mount(r.Group("/api/v1"), NewService(config.PDFConfig{}), func(c *gin.Context) {
		c.Set(ctxUserIDKey, "u1")
		c.Next()
	})

	payload, _ := json.Marshal(sampleRequest())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/export/csv", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, `attachment; filename="meddesk-`) || !strings.Contains(cd, `filename*=UTF-8''`) {
		t.Fatalf("Content-Disposition 不符: %s", cd)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("缺少 no-store")
	}
	if !bytes.HasPrefix(rec.Body.Bytes(), []byte{0xEF, 0xBB, 0xBF}) {
		t.Fatalf("响应体缺少 BOM")
	}
	t.Logf("csv: ct=%s cd=%s", rec.Header().Get("Content-Type"), cd)
}
