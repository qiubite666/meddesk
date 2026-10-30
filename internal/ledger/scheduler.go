package ledger

// 记账本每日备份的进程内调度器。
//
// 【已知限制（务必阅读）】
// 这是**进程内 ticker**：每个运行中的实例都会各自触发一次备份。
// 多实例（多副本）部署时，同一份备份会被上传多次——内容相同、路径相同，
// 最终是同名覆盖，不会产生重复文件，但会浪费带宽与 WebDAV 配额，
// 且运行记录会被后写入的实例覆盖。
// 这里**没有**做跨实例选主（比如 PostgreSQL advisory lock 或 Redis 锁），
// 因此不能声称已解决。后续若要严格"每天只跑一次"，应在 RunAll 之前
// 取一把跨进程锁（推荐 pg_advisory_xact_lock，与 migrate.go 的做法一致）。
//
// 单实例内的并发由两道防线挡住：BackupService.runMu（内存互斥） +
// ledger_backup_runs 的 UNIQUE(user_id, run_date)（数据库幂等）。

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// 备份相关的环境变量（不改动 config.go，按任务要求直接在此读取）。
const (
	// envBackupTime 是每日触发时刻，格式 "HH:MM"，默认 "02:00"。
	envBackupTime = "MEDDESK_LEDGER_BACKUP_TIME"
	// envBackupTZ 是时区名，默认 "Asia/Shanghai"。
	envBackupTZ = "MEDDESK_LEDGER_BACKUP_TZ"
	// envBackupEnabled 是总开关，默认 "1"。
	envBackupEnabled = "MEDDESK_LEDGER_BACKUP_ENABLED"

	defaultBackupTime = "02:00"
	defaultBackupTZ   = "Asia/Shanghai"
)

// backupConfig 是备份调度配置。
type backupConfig struct {
	Enabled bool
	Hour    int
	Minute  int
	TZ      string
	loc     *time.Location
}

// scheduleText 返回 "HH:MM" 形式的配置时刻。
func (c backupConfig) scheduleText() string {
	return pad2i(c.Hour) + ":" + pad2i(c.Minute)
}

func pad2i(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// loadBackupConfig 读取环境变量构造配置；任一取值非法时回落到默认值（不阻断启动）。
func loadBackupConfig() backupConfig {
	cfg := backupConfig{Enabled: envFlag(envBackupEnabled, true)}

	hour, minute := parseHHMM(os.Getenv(envBackupTime))
	cfg.Hour, cfg.Minute = hour, minute

	cfg.TZ = strings.TrimSpace(os.Getenv(envBackupTZ))
	if cfg.TZ == "" {
		cfg.TZ = defaultBackupTZ
	}
	// 镜像常常不带 tzdata，LoadLocation 会失败；此时退化为固定 +08:00，
	// 中国目前不使用夏令时，固定偏移与 Asia/Shanghai 等价（service.go 的 shanghai 同理）。
	loc, err := time.LoadLocation(cfg.TZ)
	if err != nil {
		// 容器镜像常常不带 tzdata：退化为固定 +08:00 偏移（中国不使用夏令时，
		// 与 Asia/Shanghai 等价），保证"每天 02:00"仍然落在预期的自然日上。
		loc = time.FixedZone(cfg.TZ, 8*60*60)
	}
	cfg.loc = loc
	return cfg
}

// parseHHMM 解析 "HH:MM"；非法时返回默认的 02:00。
func parseHHMM(raw string) (int, int) {
	s := strings.TrimSpace(raw)
	if s == "" {
		s = defaultBackupTime
	}
	h, m, ok := strings.Cut(s, ":")
	if !ok {
		h, m = s, "00"
	}
	hour, err := strconv.Atoi(strings.TrimSpace(h))
	if err != nil || hour < 0 || hour > 23 {
		hour = 2
	}
	minute, err := strconv.Atoi(strings.TrimSpace(m))
	if err != nil || minute < 0 || minute > 59 {
		minute = 0
	}
	return hour, minute
}

// envFlag 读取布尔型环境变量。
func envFlag(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// Scheduler 是每日备份调度器。
type Scheduler struct {
	svc *BackupService
	cfg backupConfig
	log *slog.Logger
}

// NewScheduler 构造调度器（配置在构造时读取一次；改环境变量需重启进程）。
func NewScheduler(svc *BackupService, log *slog.Logger) *Scheduler {
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{svc: svc, cfg: svc.cfg, log: log}
}

// Start 启动后台 goroutine，按配置的时区与时刻每天触发一次全量备份。
//
// 传人的 ctx 被取消（进程退出）时 goroutine 结束。
func (s *Scheduler) Start(ctx context.Context) {
	if s == nil || s.svc == nil {
		return
	}
	if !s.cfg.Enabled {
		s.log.Info("记账本自动备份已关闭", "env", envBackupEnabled)
		return
	}
	go s.loop(ctx)
}

// loop 计算下一次触发时刻并等待；触发后为所有已配置凭据的账号各跑一次。
func (s *Scheduler) loop(ctx context.Context) {
	for {
		next := s.svc.NextRun()
		wait := time.Until(next)
		s.log.Info("记账本自动备份已排程", "tz", s.cfg.TZ, "time", s.cfg.scheduleText(),
			"nextRunAt", next.Format(time.RFC3339))
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			s.log.Info("记账本自动备份调度已停止")
			return
		case <-timer.C:
			s.runAll(ctx)
		}
	}
}

// runAll 执行一次全量备份并记录结果（失败只告警，不影响下一次排程）。
func (s *Scheduler) runAll(ctx context.Context) {
	ok, failed, err := s.svc.RunAll(ctx)
	if err != nil {
		s.log.Warn("记账本自动备份未执行", "error", err)
		return
	}
	s.log.Info("记账本自动备份已执行", "success", ok, "failed", failed)
}
