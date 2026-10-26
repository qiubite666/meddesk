// Package server 负责组装 HTTP 路由：中间件链、各业务模块挂载与前端静态资源服务。
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	meddesk "github.com/qiubite666/meddesk"
	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/attachments"
	"github.com/qiubite666/meddesk/internal/auth"
	"github.com/qiubite666/meddesk/internal/cache"
	"github.com/qiubite666/meddesk/internal/config"
	"github.com/qiubite666/meddesk/internal/export"
	"github.com/qiubite666/meddesk/internal/middleware"
	"github.com/qiubite666/meddesk/internal/model"
	"github.com/qiubite666/meddesk/internal/store"
	"github.com/qiubite666/meddesk/internal/syncsvc"
)

// Version 服务版本号，可通过 -ldflags "-X ...server.Version=v1.2.3" 覆盖。
var Version = "0.1.0"

// Repos 是路由组装所需的**最小仓储依赖**。
//
// 之所以用接口而非具体类型 *store.Store：生产环境传入 *store.Store（天然满足本接口），
// 集成测试则传入 internal/fake 的内存实现，从而在没有 PostgreSQL/Redis 的机器上
// 也能验证 HTTP 全链路契约。本接口只暴露组装所需的方法，不引入任何额外功能。
type Repos interface {
	Users() store.UserRepo
	Refresh() store.RefreshRepo
	Sync() store.SyncRepo
	Attachments() store.AttachmentRepo
	Ping(ctx context.Context) error
}

// Deps 组装路由所需依赖。
type Deps struct {
	Cfg   *config.Config
	Log   *slog.Logger
	Store Repos
	Cache cache.Cache
}

// New 构建完整的 gin 引擎。
func New(d Deps) (*gin.Engine, error) {
	cfg, log := d.Cfg, d.Log

	if !cfg.IsDev() {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.New()
	// 反向代理后置：是否信任 Nginx 传来的 X-Forwarded-* 头。
	if cfg.TrustProxy {
		_ = r.SetTrustedProxies([]string{"0.0.0.0/0", "::/0"})
	} else {
		_ = r.SetTrustedProxies(nil)
	}

	// 中间件顺序：请求 ID → 恢复 → 日志 → 安全头 → CORS → 体积限制。
	r.Use(middleware.RequestID())
	r.Use(middleware.Recovery(log))
	r.Use(middleware.Logger(log))
	r.Use(middleware.SecurityHeaders())
	r.Use(middleware.CORS(cfg.CORS.AllowedOrigins))
	r.Use(middleware.BodyLimit(cfg.Upload.MaxBytes))

	// 存活与就绪探针（不鉴权、不计入业务限流）。
	r.GET("/api/v1/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, model.HealthResponse{
			Status:     "ok",
			Version:    Version,
			DB:         probe(d.Store.Ping(c.Request.Context())),
			Cache:      probe(d.Cache.Ping(c.Request.Context())),
			ServerTime: time.Now().UnixMilli(),
		})
	})
	r.GET("/api/v1/ready", func(c *gin.Context) {
		dbOK := d.Store.Ping(c.Request.Context()) == nil
		cacheOK := d.Cache.Ping(c.Request.Context()) == nil
		status := "ok"
		code := http.StatusOK
		// 缓存不可用可降级运行，数据库不可用则视为未就绪。
		if !dbOK {
			status, code = "down", http.StatusServiceUnavailable
		} else if !cacheOK {
			status = "degraded"
		}
		c.JSON(code, model.HealthResponse{
			Status:     status,
			Version:    Version,
			DB:         probeBool(dbOK),
			Cache:      probeBool(cacheOK),
			ServerTime: time.Now().UnixMilli(),
		})
	})

	v1 := r.Group("/api/v1")
	v1.Use(middleware.RateLimit(d.Cache, cfg.RateLimit.RPS, cfg.RateLimit.Burst))

	// 认证模块：自行挂载 /auth/*，并导出鉴权中间件给其余模块复用。
	authSvc := auth.NewService(d.Store.Users(), d.Store.Refresh(), d.Cache, cfg.Auth)
	auth.Mount(v1, authSvc)
	requireAuth := auth.RequireAuth(authSvc)

	// 加密同步、附件、导出。
	syncsvc.Mount(v1, syncsvc.NewService(d.Store.Sync(), cfg.Sync), requireAuth)
	attachments.Mount(v1, attachments.NewService(d.Store.Attachments(), cfg.Upload), requireAuth)
	export.Mount(v1, export.NewService(cfg.PDF), requireAuth)

	// 前端静态资源：优先内嵌 FS，开发模式读磁盘。
	mountSPA(r, cfg, log)

	return r, nil
}

// probe 把 error 转成健康状态字符串。
func probe(err error) string { return probeBool(err == nil) }

// probeBool 布尔转健康状态字符串。
func probeBool(ok bool) string {
	if ok {
		return "ok"
	}
	return "down"
}

// mountSPA 挂载单页应用：静态文件优先，未命中回退 index.html（支持前端路由刷新）。
//
// 缓存策略（解决「发版后页面仍用旧 JS」）：
//  1. 启动时对内嵌前端全部文件计算内容哈希 buildID；
//  2. index.html 的所有 /assets/ 引用被注入 ?v=<buildID> 指纹参数，指纹变化 → URL 变化 →
//     浏览器/CDN 必然重新拉取（配合 /assets/ 的 immutable 长缓存是安全的）；
//  3. sw.js 注入同样的 buildID 作为 Service Worker 缓存名后缀，activate 时自动清理旧缓存；
//  4. index.html / sw.js / manifest 响应 no-cache（每次校验），静态资源附 ETag 支持 304 协商。
func mountSPA(r *gin.Engine, cfg *config.Config, log *slog.Logger) {
	var fsys fs.FS
	if cfg.WebDir != "" {
		log.Info("静态资源使用磁盘目录（开发模式）", "dir", cfg.WebDir)
		fsys = os.DirFS(cfg.WebDir)
	} else {
		sub, err := fs.Sub(meddesk.WebFS, "web")
		if err != nil {
			// 不会发生：//go:embed all:web 已保证目录存在；仅作兜底，避免启动即崩。
			log.Error("内嵌前端资源不可用，回退到根目录", "error", err)
			fsys = meddesk.WebFS
		} else {
			fsys = sub
		}
	}
	root := http.FS(fsys)
	fileServer := http.FileServer(root)

	// ---- 构建哈希与注入 ----
	buildID := computeBuildID(fsys)
	var reRef = regexp.MustCompile(`(?i)(src|href)="(/assets/[^"]+)"`)
	injectRef := func(data []byte) []byte {
		return reRef.ReplaceAll(data, []byte(`$1="`+"$2?v="+buildID+`"`))
	}
	var indexHTML, swJS []byte
	if f, err := root.Open("index.html"); err == nil {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(f)
		_ = f.Close()
		indexHTML = injectRef(buf.Bytes())
		// 同时把构建指纹暴露给前端（设置页/侧栏显示，便于用户确认已加载新版）
		indexHTML = bytes.Replace(indexHTML, []byte("<head>"),
			// 用 meta 传递构建信息：nginx 的 CSP `script-src 'self'` 会拦截内联 <script>，
			// 导致 window.MD_BUILD_ID 永远 undefined（版本显示 dev + 自愈逻辑空转无限刷新的根因）。
			// meta 标签不受 script-src 约束，前端 core.js 从 meta 读取。
			[]byte("<head>\n  <meta name=\"md-build-id\" content=\""+buildID+"\">\n  <meta name=\"md-version\" content=\""+Version+"\">"), 1)
		// 把版本号写进 <title>：不依赖任何 JS，浏览器标签页即可看到实际加载的前端版本，
		// 用于快速区分「拿到新 HTML」还是「旧 Service Worker 缓存」。
		indexHTML = bytes.Replace(indexHTML, []byte("<title>丘比特的工作台</title>"),
			[]byte("<title>丘比特的工作台 · "+Version+"</title>"), 1)
	}
	if f, err := root.Open("sw.js"); err == nil {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(f)
		_ = f.Close()
		swJS = bytes.ReplaceAll(buf.Bytes(), []byte("__BUILD_ID__"), []byte(buildID))
	}
	log.Info("前端静态资源构建指纹就绪", "buildID", buildID)

	// writeBytes：直接输出注入后的字节并附加 no-cache 头。
	writeBytes := func(w http.ResponseWriter, r *http.Request, data []byte, ctype string) {
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		w.Header().Set("ETag", `"`+buildID+`"`)
		if match := r.Header.Get("If-None-Match"); match != "" && match == `"`+buildID+`"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write(data)
	}
	serveIndex := func(w http.ResponseWriter, r *http.Request) {
		if len(indexHTML) == 0 {
			fileServer.ServeHTTP(w, r)
			return
		}
		writeBytes(w, r, indexHTML, "text/html; charset=utf-8")
	}
	serveSW := func(w http.ResponseWriter, r *http.Request) {
		if len(swJS) == 0 {
			fileServer.ServeHTTP(w, r)
			return
		}
		writeBytes(w, r, swJS, "application/javascript; charset=utf-8")
	}
	r.GET("/", gin.WrapH(http.HandlerFunc(serveIndex)))
	r.GET("/index.html", gin.WrapH(http.HandlerFunc(serveIndex)))
	r.GET("/sw.js", gin.WrapH(http.HandlerFunc(serveSW)))
	// manifest：no-cache + ETag 协商（内容小，304 开销可忽略）。
	r.GET("/manifest.webmanifest", func(c *gin.Context) {
		etag := `"` + buildID + `"`
		c.Header("Cache-Control", "no-cache, must-revalidate")
		c.Header("ETag", etag)
		if c.GetHeader("If-None-Match") == etag {
			c.AbortWithStatus(http.StatusNotModified)
			return
		}
		fileServer.ServeHTTP(c.Writer, c.Request)
	})
	// 静态资源：URL 已由 index.html 注入 ?v=<buildID> 指纹，指纹变化 → URL 变化 →
	// 浏览器/CDN 必然重新拉取，因此这里的长缓存 + immutable 是安全的；ETag 兜底 304 协商。
	r.GET("/assets/*filepath", func(c *gin.Context) {
		c.Header("Cache-Control", "public, max-age=31536000, immutable")
		c.Header("ETag", `"`+buildID+`"`)
		if c.GetHeader("If-None-Match") == `"`+buildID+`"` {
			c.AbortWithStatus(http.StatusNotModified)
			return
		}
		fileServer.ServeHTTP(c.Writer, c.Request)
	})

	r.NoRoute(func(c *gin.Context) {
		path := c.Request.URL.Path
		if strings.HasPrefix(path, "/api/") {
			writeError(c, apierr.New(apierr.CodeNotFound, "接口不存在"))
			return
		}
		// 静态文件命中则直接返回，否则回退到 index.html 交由前端路由处理。
		if f, err := root.Open(strings.TrimPrefix(path, "/")); err == nil {
			_ = f.Close()
			fileServer.ServeHTTP(c.Writer, c.Request)
			return
		}
		if strings.HasSuffix(path, "/") {
			writeError(c, apierr.New(apierr.CodeNotFound, "页面不存在"))
			return
		}
		c.Request.URL.Path = "/"
		fileServer.ServeHTTP(c.Writer, c.Request)
	})
}

// computeBuildID 对前端静态目录全部文件内容计算 sha256，取前 12 位 hex 作为构建指纹。
// 任一前端文件变化都会改变该值，从而驱动浏览器与 Service Worker 更新缓存。
func computeBuildID(fsys fs.FS) string {
	h := sha256.New()
	_ = fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return nil
		}
		h.Write([]byte(path))
		h.Write([]byte{0})
		h.Write(data)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// writeError 统一错误输出：状态码由错误码映射，避免暴露内部细节。
func writeError(c *gin.Context, err error) {
	e := apierr.FromError(err)
	c.AbortWithStatusJSON(apierr.HTTPStatus(e.Code), e)
}
