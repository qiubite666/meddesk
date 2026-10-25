// Package server 负责组装 HTTP 路由：中间件链、各业务模块挂载与前端静态资源服务。
package server

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
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
func mountSPA(r *gin.Engine, cfg *config.Config, log *slog.Logger) {
	var root http.FileSystem
	if cfg.WebDir != "" {
		log.Info("静态资源使用磁盘目录（开发模式）", "dir", cfg.WebDir)
		root = http.Dir(cfg.WebDir)
	} else {
		sub, err := fs.Sub(meddesk.WebFS, "web")
		if err != nil {
			// 不会发生：//go:embed all:web 已保证目录存在；仅作兜底，避免启动即崩。
			log.Error("内嵌前端资源不可用，回退到根目录", "error", err)
			root = http.FS(meddesk.WebFS)
		} else {
			root = http.FS(sub)
		}
	}
	fileServer := http.FileServer(root)

	// 外壳文件禁止强缓存，避免发版后浏览器仍用旧索引。
	noCache := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		fileServer.ServeHTTP(w, r)
	}
	r.GET("/", gin.WrapH(http.HandlerFunc(noCache)))
	r.GET("/index.html", gin.WrapH(http.HandlerFunc(noCache)))
	r.GET("/sw.js", gin.WrapH(http.HandlerFunc(noCache)))
	r.GET("/manifest.webmanifest", gin.WrapH(fileServer))
	// 带指纹的静态资源可长期缓存。
	r.GET("/assets/*filepath", func(c *gin.Context) {
		c.Header("Cache-Control", "public, max-age=604800, immutable")
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

// writeError 统一错误输出：状态码由错误码映射，避免暴露内部细节。
func writeError(c *gin.Context, err error) {
	e := apierr.FromError(err)
	c.AbortWithStatusJSON(apierr.HTTPStatus(e.Code), e)
}
