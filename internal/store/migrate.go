package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationDir 是内嵌迁移文件所在的目录（相对本包目录）。
const migrationDir = "migrations"

// migrateLockKey 是迁移使用的 advisory lock 键，避免多实例同时启动时的迁移竞争。
// 必须显式声明为 int64：pgx 无法把 Go 的 int 编码成 bigint（int8）参数。
// pg_advisory_xact_lock 是事务级锁，事务结束即释放。
const migrateLockKey int64 = 0x6D65646465736B01 // "meddesk" 前缀 + 1

// Migrate 执行内嵌的 SQL 迁移，可重复执行（幂等）。
//
// 实现要点：
//  1. 自建 schema_migrations(version, applied_at) 记录已执行版本；
//  2. 只执行 *.sql，跳过 *.down.sql（回滚脚本不自动执行）；
//  3. 按文件名升序逐个执行，整体包在一个事务里，任一步失败即整体回滚；
//  4. 同一份文件内容不含参数，交由 pgx 走简单协议，因此支持多语句脚本。
func (s *Store) Migrate(ctx context.Context) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("执行迁移失败: %w", errNoPool)
	}

	files, err := listMigrations()
	if err != nil {
		return fmt.Errorf("读取迁移脚本失败: %w", err)
	}
	if len(files) == 0 {
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("开启迁移事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// 事务级咨询锁：并发启动的多个实例串行化迁移。
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", migrateLockKey); err != nil {
		return fmt.Errorf("获取迁移锁失败: %w", err)
	}

	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
    version    text        PRIMARY KEY,
    applied_at timestamptz NOT NULL DEFAULT now()
)`); err != nil {
		return fmt.Errorf("创建迁移记录表失败: %w", err)
	}

	for _, name := range files {
		version := strings.TrimSuffix(name, ".sql")

		var applied bool
		err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", version).Scan(&applied)
		if err != nil {
			return fmt.Errorf("查询迁移状态失败[%s]: %w", version, err)
		}
		if applied {
			continue
		}

		sqlBytes, err := migrationsFS.ReadFile(migrationDir + "/" + name)
		if err != nil {
			return fmt.Errorf("读取迁移脚本失败[%s]: %w", version, err)
		}
		if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
			return fmt.Errorf("执行迁移失败[%s]: %w", version, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", version); err != nil {
			return fmt.Errorf("记录迁移版本失败[%s]: %w", version, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("提交迁移事务失败: %w", err)
	}
	return nil
}

// listMigrations 返回按文件名升序排列的迁移脚本名（已排除 *.down.sql）。
func listMigrations() ([]string, error) {
	entries, err := migrationsFS.ReadDir(migrationDir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		if strings.HasSuffix(e.Name(), ".down.sql") {
			continue // 回滚脚本不参与自动迁移
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}

// errNoPool 是连接池未初始化时的内部错误（不对外暴露，仅用于包装）。
var errNoPool = errors.New("连接池未初始化")
