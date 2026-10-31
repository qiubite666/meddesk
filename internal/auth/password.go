package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id 参数：salt 16 字节、派生密钥 32 字节，编码串共 6 段。
const (
	argon2SaltLen = 16
	argon2KeyLen  = 32
	argon2Version = 19
)

// 解析已存哈希时允许的参数上界。
//
// 【为什么必须校验】PHC 串来自数据库，而 m/t/p 会被原样喂给 argon2.IDKey：
// 一条被篡改（或迁移时误写）的 `m=4294967295` 会让单次登录申请 4TiB 内存，
// 进程直接 OOM —— 攻击者只要能让自己的 password_hash 变成任意串就构成 DoS。
// 上界取「合法配置的合理上限」：256MiB / 10 轮 / 16 线程，远高于实际部署
// （默认 64MiB / 3 / 2），不会误伤任何正常凭据。
const (
	maxArgon2MemoryKiB = 256 * 1024
	maxArgon2Time      = 10
	maxArgon2Threads   = 16
)

// HashClientHash 用 argon2id 对客户端送来的 PBKDF2 派生值做二次哈希。
// 编码格式：$argon2id$v=19$m=<mem>,t=<time>,p=<par>$<b64salt>$<b64hash>
func HashClientHash(clientHash string, memory, iterations uint32, threads uint8) (string, error) {
	if memory == 0 || iterations == 0 || threads == 0 {
		return "", errors.New("argon2 参数非法")
	}
	salt := make([]byte, argon2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("生成随机 salt 失败: %w", err)
	}
	key := argon2.IDKey([]byte(clientHash), salt, iterations, memory, threads, argon2KeyLen)
	return formatArgon2(memory, iterations, threads, salt, key), nil
}

// VerifyClientHash 校验客户端派生值是否与已存储的 argon2id 串匹配，使用常量时间比对。
func VerifyClientHash(encoded, clientHash string) (bool, error) {
	memory, iterations, threads, salt, want, err := parseArgon2(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(clientHash), salt, iterations, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// formatArgon2 组装 PHC 风格的 argon2id 串。
func formatArgon2(memory, iterations uint32, threads uint8, salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2Version, memory, iterations, threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
}

// parseArgon2 解析 argon2id 串，返回参数、salt 与期望密钥。
func parseArgon2(encoded string) (memory, iterations uint32, threads uint8, salt, key []byte, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return 0, 0, 0, nil, nil, errors.New("口令哈希格式非法")
	}
	var version int
	if _, err = fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return 0, 0, 0, nil, nil, errors.New("口令哈希版本非法")
	}
	if version != argon2Version {
		// 只认 v=19（argon2id 现行版本）。其他版本的参数语义不同，
		// 按未知格式处理比「尽力解析」安全。
		return 0, 0, 0, nil, nil, errors.New("口令哈希版本不受支持")
	}
	for _, kv := range strings.Split(parts[3], ",") {
		if len(kv) < 3 {
			return 0, 0, 0, nil, nil, errors.New("口令哈希参数非法")
		}
		v, convErr := strconv.ParseUint(kv[2:], 10, 32)
		if convErr != nil {
			return 0, 0, 0, nil, nil, errors.New("口令哈希参数非法")
		}
		switch kv[0] {
		case 'm':
			memory = uint32(v)
		case 't':
			iterations = uint32(v)
		case 'p':
			threads = uint8(v)
		default:
			return 0, 0, 0, nil, nil, errors.New("口令哈希参数非法")
		}
	}
	if salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil {
		return 0, 0, 0, nil, nil, errors.New("口令哈希 salt 非法")
	}
	if key, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil {
		return 0, 0, 0, nil, nil, errors.New("口令哈希值非法")
	}
	if memory == 0 || iterations == 0 || threads == 0 || len(key) == 0 {
		return 0, 0, 0, nil, nil, errors.New("口令哈希参数非法")
	}
	// 上界校验必须在调用 argon2.IDKey 之前完成：那里会按 m 直接分配内存。
	// 同时 memory < 8*threads 时 argon2 会 panic（"memory too little"），
	// 也必须在这里拦住，否则一条坏哈希就能打挂进程。
	if memory > maxArgon2MemoryKiB || iterations > maxArgon2Time || threads > maxArgon2Threads ||
		memory < 8*uint32(threads) {
		return 0, 0, 0, nil, nil, errors.New("口令哈希参数超出允许范围")
	}
	return memory, iterations, threads, salt, key, nil
}
