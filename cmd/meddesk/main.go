// Command meddesk 是 MedDesk 服务端入口：加载配置 → 连接数据库与缓存 → 迁移 → 启动 HTTP 服务 → 优雅退出。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/qiubite666/meddesk/internal/cache"
	"github.com/qiubite666/meddesk/internal/config"
	"github.com/qiubite666/meddesk/internal/server"
	"github.com/qiubite666/meddesk/internal/store"
)

func main() {
	var (
		showVersion = flag.Bool("version", false, "打印版本号并退出")
		migrateOnly = flag.Bool("migrate", false, "仅执行数据库迁移后退出")
		checkOnly   = flag.Bool("check", false, "仅校验配置与依赖连通性后退出")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("meddesk", server.Version)
		return
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置错误：%v\n", err)
		os.Exit(2)
	}
	log := newLogger(cfg)
	slog.SetDefault(log)

	// 管理后台配置缺失只告警、不阻断：主功能（健康档案/记账本）应先保证可用。
	for _, w := range cfg.AdminWarnings() {
		log.Warn("管理后台配置告警", "detail", w)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 数据库：建立连接池并按需迁移。
	st, err := store.Open(ctx, cfg.DB)
	if err != nil {
		log.Error("数据库连接失败", "error", err)
		os.Exit(1)
	}
	defer st.Close()

	if cfg.DB.MigrateOnBoot || *migrateOnly {
		if err := st.Migrate(ctx); err != nil {
			log.Error("数据库迁移失败", "error", err)
			os.Exit(1)
		}
		log.Info("数据库迁移完成")
	}
	if *migrateOnly {
		return
	}

	// 缓存：Redis 不可用时自动降级为进程内实现，不阻断启动。
	c, err := cache.Open(ctx, cfg.Redis)
	if err != nil {
		log.Error("缓存初始化失败", "error", err)
		os.Exit(1)
	}
	defer c.Close()

	if *checkOnly {
		if err := st.Ping(ctx); err != nil {
			log.Error("数据库连通性检查失败", "error", err)
			os.Exit(1)
		}
		log.Info("配置与依赖检查通过")
		return
	}

	// 过期刷新令牌清理：每小时一次，失败只告警不影响服务。
	go cleanupRefreshTokens(ctx, st, log)

	engine, err := server.New(server.Deps{Cfg: cfg, Log: log, Store: st, Cache: c})
	if err != nil {
		log.Error("路由初始化失败", "error", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           engine,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      180 * time.Second, // 导出大文件需要较长写超时
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		log.Info("MedDesk 启动", "addr", cfg.HTTPAddr, "env", cfg.Env, "version", server.Version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("HTTP 服务异常退出", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("收到退出信号，开始优雅关闭")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("优雅关闭超时，强制结束", "error", err)
	}
	log.Info("已退出")
}

// newLogger 按配置构造结构化日志器。
func newLogger(cfg *config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.Log.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if cfg.Log.Format == "text" {
		h = slog.NewTextHandler(os.Stdout, opts)
	} else {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(h).With("service", "meddesk")
}

// cleanupRefreshTokens 定期清理过期刷新令牌。
func cleanupRefreshTokens(ctx context.Context, st *store.Store, log *slog.Logger) {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := st.Refresh().DeleteExpired(ctx, time.Now())
			if err != nil {
				log.Warn("清理过期刷新令牌失败", "error", err)
				continue
			}
			if n > 0 {
				log.Info("已清理过期刷新令牌", "count", n)
			}
		}
	}
}
