// Command meddesk-admin 是 MedDesk 的运维管理工具。
//
// 子命令 reset-password：为指定邮箱重置登录口令。
//
// 【端到端加密的硬约束】
// 口令派生 KEK，KEK 解包 DEK（数据密钥）。口令重置后旧 KEK 永远解不开旧
// wrapped_dek，服务器上的旧密文（sync_records）无法再用——必须一并清空，
// 账号从空状态开始。旧设备本地 IndexedDB 里的明文数据不受影响（可先导出备份）。
//
// 用法：
//
//	meddesk-admin reset-password -email <邮箱> [-new-password <口令>] [-env-file <路径>] [-apply]
//
// 默认 dry-run：只打印账号现状。带 -apply 才真正执行。
// 执行内容：更新口令三件套（kdf_salt / wrapped_dek / dek_iv）、撤销全部会话、
// 清空该用户的 sync_records 与同步游标。
package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/crypto/hkdf"
	"crypto/sha256"
	"golang.org/x/crypto/pbkdf2"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/qiubite666/meddesk/internal/auth"
	"github.com/qiubite666/meddesk/internal/config"
)

// 与 web/assets/js/crypto.js 严格一致的客户端派生参数。
const (
	pbkdf2Iterations = 210000
	pbkdf2KeyLen     = 32 // 256 bit
	hkdfInfo         = "meddesk-kek-v1"
	dekLen           = 32
	kekIVLen         = 12
	kdfSaltLen       = 16
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "用法:\n  meddesk-admin reset-password -email <邮箱> [-new-password <口令>] [-env-file <路径>] [-apply]\n  meddesk-admin admin-credential -password <口令> -env-file <路径>\n  meddesk-admin list-records -email <邮箱> [-table <表名>] [-limit 200] -env-file <路径>")
		os.Exit(2)
	}
	if os.Args[1] == "admin-credential" {
		adminCredential(os.Args[2:])
		return
	}
	if os.Args[1] == "recovery-keygen" {
		recoveryKeygen()
		return
	}
	if os.Args[1] == "list-records" {
		listRecords(os.Args[2:])
		return
	}
	if os.Args[1] != "reset-password" {
		fmt.Fprintln(os.Stderr, "未知子命令:", os.Args[1])
		os.Exit(2)
	}
	fs := flag.NewFlagSet("reset-password", flag.ExitOnError)
	var (
		email       = fs.String("email", "", "目标账号邮箱（必填）")
		newPassword = fs.String("new-password", "", "新口令（留空则自动生成强口令）")
		envFile     = fs.String("env-file", "", "服务端 env 文件路径（读取 MEDDESK_DB_URL）")
		apply       = fs.Bool("apply", false, "真正执行（默认 dry-run 只打印现状）")
	)
	_ = fs.Parse(os.Args[2:])
	if *email == "" {
		fmt.Fprintln(os.Stderr, "-email 必填")
		os.Exit(2)
	}

	// DSN：优先 env 文件，其次环境变量。
	dsn := os.Getenv("MEDDESK_DB_URL")
	if *envFile != "" {
		raw, err := os.ReadFile(*envFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "读 env 文件失败:", err)
			os.Exit(1)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "MEDDESK_DB_URL=") {
				dsn = strings.TrimPrefix(line, "MEDDESK_DB_URL=")
				dsn = strings.Trim(dsn, `"'`)
			}
		}
	}
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "缺少 MEDDESK_DB_URL（-env-file 或环境变量）")
		os.Exit(1)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "连接数据库失败:", err)
		os.Exit(1)
	}
	defer pool.Close()

	// 1) 账号现状
	var (
		userID      string
		createdAt   interface{}
		refreshCnt  int
		syncCnt     int
		grantOwner  int
		grantViewer int
	)
	err = pool.QueryRow(ctx,
		`SELECT id, created_at,
		       (SELECT count(*) FROM refresh_tokens WHERE user_id = u.id),
		       (SELECT count(*) FROM sync_records WHERE user_id = u.id),
		       (SELECT count(*) FROM family_grants WHERE owner_id = u.id AND status = 'active'),
		       (SELECT count(*) FROM family_grants WHERE viewer_id = u.id AND status = 'active')
		 FROM users u WHERE lower(email::text) = lower($1)`, *email).
		Scan(&userID, &createdAt, &refreshCnt, &syncCnt, &grantOwner, &grantViewer)
	if err == pgx.ErrNoRows {
		fmt.Fprintln(os.Stderr, "账号不存在:", *email)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "查询账号失败:", err)
		os.Exit(1)
	}

	fmt.Printf("账号       %s\n用户 ID    %s\n创建时间   %v\n活跃会话   %d 条 refresh_token\n服务器密文 %d 条 sync_records\n作为owner的授权  %d 条（重置后失效，查看方需重新关联）\n作为viewer的授权 %d 条\n",
		*email, userID, createdAt, refreshCnt, syncCnt, grantOwner, grantViewer)

	if !*apply {
		fmt.Println("\n[dry-run] 未做任何修改。确认无误后追加 -apply 执行。")
		fmt.Println("[dry-run] 注意：执行后该账号服务器密文全部清空（端到端加密约束），账号从空状态开始；")
		fmt.Println("[dry-run] 旧设备本地数据仍在，可先在旧设备导出备份。")
		return
	}

	// 2) 新口令与密钥材料
	password := *newPassword
	if password == "" {
		password = generatePassword(20)
	}
	if len(password) < 8 {
		fmt.Fprintln(os.Stderr, "新口令至少 8 位")
		os.Exit(1)
	}

	salt := make([]byte, kdfSaltLen)
	dek := make([]byte, dekLen)
	if _, err = io.ReadFull(rand.Reader, salt); err != nil {
		fatal("生成盐失败", err)
	}
	if _, err = io.ReadFull(rand.Reader, dek); err != nil {
		fatal("生成 DEK 失败", err)
	}

	// PBKDF2-SHA256（客户端第一步），再 argon2id 二次哈希（服务端口径）
	clientHash := b64(pbkdf2.Key([]byte(password), salt, pbkdf2Iterations, pbkdf2KeyLen, sha256.New))
	cfg, err := config.Load()
	if err != nil {
		fatal("读取配置失败", err)
	}
	passwordHash, err := auth.HashClientHash(clientHash, cfg.Auth.Argon2Memory, cfg.Auth.Argon2Time, cfg.Auth.Argon2Threads)
	if err != nil {
		fatal("argon2id 失败", err)
	}

	// HKDF(authHash, salt, 'meddesk-kek-v1') → AES-GCM 包裹 DEK
	wrapped, iv, err := wrapDEK(dek, clientHash, salt)
	if err != nil {
		fatal("包裹 DEK 失败", err)
	}

	// 3) 事务写入
	tx, err := pool.Begin(ctx)
	if err != nil {
		fatal("开启事务失败", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err = tx.Exec(ctx,
		`UPDATE users SET kdf_salt=$1, wrapped_dek=$2, dek_iv=$3, password_hash=$4 WHERE id=$5`,
		salt, wrapped, iv, passwordHash, userID); err != nil {
		fatal("更新口令失败", err)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM refresh_tokens WHERE user_id=$1`, userID); err != nil {
		fatal("撤销会话失败", err)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM sync_records WHERE user_id=$1`, userID); err != nil {
		fatal("清空同步记录失败", err)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM sync_state WHERE user_id=$1`, userID); err != nil {
		fatal("清空同步游标失败", err)
	}
	if err = tx.Commit(ctx); err != nil {
		fatal("提交失败", err)
	}

	fmt.Println("\n✅ 口令已重置，全部会话已撤销，服务器密文已清空。")
	fmt.Println("新口令:", password)
	fmt.Println("（请立即保存。旧设备的登录已全部失效；旧设备本地数据仍在，可导出备份后导入新会话。）")
}

// wrapDEK 复刻客户端 wrapDEK：KEK = HKDF-SHA256(ikm=PBKDF2 结果, salt, info=meddesk-kek-v1, 256bit)
func wrapDEK(dek []byte, clientHash string, salt []byte) (wrappedB64, ivB64 string, err error) {
	ikm := []byte(clientHash)
	reader := hkdf.New(sha256.New, ikm, salt, []byte(hkdfInfo))
	kekBytes := make([]byte, 32)
	if _, err = io.ReadFull(reader, kekBytes); err != nil {
		return "", "", err
	}
	block, err := aes.NewCipher(kekBytes)
	if err != nil {
		return "", "", err
	}
	iv := make([]byte, kekIVLen)
	if _, err = io.ReadFull(rand.Reader, iv); err != nil {
		return "", "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", "", err
	}
	sealed := gcm.Seal(nil, iv, dek, nil)
	return base64.StdEncoding.EncodeToString(sealed), base64.StdEncoding.EncodeToString(iv), nil
}

func generatePassword(n int) string {
	const alphabet = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKMNPQRSTUVWXYZ23456789!@#$%&*"
	buf := make([]byte, n)
	_, _ = io.ReadFull(rand.Reader, buf)
	out := make([]byte, n)
	for i, b := range buf {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out)
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func fatal(msg string, err error) {
	fmt.Fprintln(os.Stderr, msg+":", err)
	os.Exit(1)
}

// adminCredential 生成管理后台的独立凭据，并写入 env 文件。
//
// 复用 auth.GenerateCredentials：它按客户端口径算出
// salt（16B）+ argon2id(PBKDF2(口令, salt))，与用户表同格式，
// 因此后台登录可以用同一套 VerifyClientHash 校验，不需要另外的密码学路径。
func adminCredential(args []string) {
	fs := flag.NewFlagSet("admin-credential", flag.ExitOnError)
	password := fs.String("password", "", "管理员后台口令（至少 8 位）")
	envFile := fs.String("env-file", "", "要写入的 env 文件路径")
	_ = fs.Parse(args)
	if len(*password) < 8 {
		fmt.Fprintln(os.Stderr, "口令至少 8 位")
		os.Exit(1)
	}
	cfg, err := config.Load()
	if err != nil {
		// 该子命令不依赖数据库，配置校验失败（如缺少 JWT_SECRET）不应阻断生成。
		cfg = &config.Config{}
	}
	mem := cfg.Auth.Argon2Memory
	tm := cfg.Auth.Argon2Time
	th := cfg.Auth.Argon2Threads
	if mem == 0 {
		mem = 65536
	}
	if tm == 0 {
		tm = 3
	}
	if th == 0 {
		th = 2
	}
	mat, err := auth.GenerateCredentials(*password, mem, tm, th)
	if err != nil {
		fmt.Fprintln(os.Stderr, "生成凭据失败:", err)
		os.Exit(1)
	}
	saltB64 := base64.StdEncoding.EncodeToString(mat.KDFSalt)
	fmt.Println("MEDDESK_ADMIN_SALT=" + saltB64)
	fmt.Println("MEDDESK_ADMIN_PASSWORD_HASH=" + mat.PasswordHash)

	if *envFile == "" {
		fmt.Println("\n未指定 -env-file，以上两行请手动写入服务端 env 文件后重启服务。")
		return
	}
	raw, err := os.ReadFile(*envFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "读 env 文件失败:", err)
		os.Exit(1)
	}
	lines := strings.Split(string(raw), "\n")
	out := make([]string, 0, len(lines)+2)
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "MEDDESK_ADMIN_SALT=") ||
			strings.HasPrefix(strings.TrimSpace(line), "MEDDESK_ADMIN_PASSWORD_HASH=") {
			continue
		}
		out = append(out, line)
	}
	out = append(out, "MEDDESK_ADMIN_SALT="+saltB64)
	out = append(out, "MEDDESK_ADMIN_PASSWORD_HASH="+mat.PasswordHash)
	if err := os.WriteFile(*envFile, []byte(strings.Join(out, "\n")), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "写 env 文件失败:", err)
		os.Exit(1)
	}
	fmt.Println("\n已写入", *envFile, "（重启服务后生效）")
}


// loadDSN 从 env 文件或环境变量里取数据库连接串。
func loadDSN(envFile string) string {
	dsn := os.Getenv("MEDDESK_DB_URL")
	if envFile == "" {
		return dsn
	}
	raw, err := os.ReadFile(envFile)
	if err != nil {
		return dsn
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "MEDDESK_DB_URL=") {
			return strings.Trim(strings.TrimPrefix(line, "MEDDESK_DB_URL="), `"'`)
		}
	}
	return dsn
}

// listRecords 只读排查：按表列出某个账号的同步记录元数据。
//
// 【为什么只输出元数据】
// sync_records 里的内容是端到端加密的（iv + ct），服务端与运维工具都解不开，
// 也无需解开：排查「数据被覆盖还是被删除」只需要 id / ver / deleted / updated_at。
// 输出里刻意不含 ct/iv，避免把密文倒进日志。
func listRecords(args []string) {
	fs := flag.NewFlagSet("list-records", flag.ExitOnError)
	email := fs.String("email", "", "账号邮箱")
	table := fs.String("table", "", "只看某张表（profile/medication/schedule/intake/family），留空为全部")
	limit := fs.Int("limit", 200, "最多输出多少行")
	envFile := fs.String("env-file", "", "服务端 env 文件路径（读取 MEDDESK_DB_URL）")
	_ = fs.Parse(args)

	if strings.TrimSpace(*email) == "" {
		fmt.Fprintln(os.Stderr, "缺少 -email")
		os.Exit(2)
	}
	dsn := loadDSN(*envFile)
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "缺少 MEDDESK_DB_URL（-env-file 或环境变量）")
		os.Exit(1)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "连接数据库失败:", err)
		os.Exit(1)
	}
	defer pool.Close()

	var userID string
	if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE lower(email) = lower($1)`, *email).Scan(&userID); err != nil {
		fmt.Fprintln(os.Stderr, "找不到账号:", err)
		os.Exit(1)
	}

	fmt.Printf("账号 %s  id=%s\n", *email, userID)
	if *table != "" {
		fmt.Printf("表：%s\n", *table)
	}
	fmt.Printf("%-12s %-38s %8s %8s  %s\n", "table", "record_id", "ver", "deleted", "updated_at")

	rows, err := pool.Query(ctx, `
		SELECT table_name, record_id, ver, deleted, updated_at
		FROM sync_records
		WHERE user_id = $1 AND ($2 = '' OR table_name = $2)
		ORDER BY updated_at DESC
		LIMIT $3`, userID, *table, *limit)
	if err != nil {
		fmt.Fprintln(os.Stderr, "查询失败:", err)
		os.Exit(1)
	}
	defer rows.Close()

	total, dead := 0, 0
	for rows.Next() {
		var (
			tbl     string
			rid     string
			ver     int64
			deleted bool
			updated interface{}
		)
		if err := rows.Scan(&tbl, &rid, &ver, &deleted, &updated); err != nil {
			fmt.Fprintln(os.Stderr, "读取行失败:", err)
			os.Exit(1)
		}
		total++
		if deleted {
			dead++
		}
		fmt.Printf("%-12s %-38s %8d %8v  %v\n", tbl, rid, ver, deleted, updated)
	}
	fmt.Printf("\n合计 %d 行（其中删除墓碑 %d 行）\n", total, dead)
}

// recoveryKeygen 生成「托管恢复」用的 RSA 密钥对，并打印可直接写入 env 的那一行。
//
// 私钥只出现在这一次输出里，之后无法从数据库反推 —— 请立刻存进密码管理器/离线保管。
// 更换私钥会让已开启托管的账号无法再用托管路径改口令（仍可走设备自愈）。
func recoveryKeygen() {
	_, keyB64, err := auth.GenerateRecoveryKey()
	if err != nil {
		fmt.Fprintln(os.Stderr, "生成失败:", err)
		os.Exit(1)
	}
	fmt.Println("把下面这一行写入服务端 env 文件（.meddesk.env），然后重启服务：")
	fmt.Println()
	fmt.Println("MEDDESK_RECOVERY_KEY=" + keyB64)
	fmt.Println()
	fmt.Println("注意：")
	fmt.Println("  1) 这串私钥等同于「所有开启托管账号的数据解密能力」，请离线保管；")
	fmt.Println("  2) 不要提交到 git，不要贴到聊天工具；")
	fmt.Println("  3) 丢失后已开启托管的账号仍可用「直接改口令 + 设备自愈」路径，但无法完全恢复。")
}
