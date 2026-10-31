package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/qiubite666/meddesk/internal/apierr"
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
	key, err := argon2InSlot(func() []byte {
		return argon2.IDKey([]byte(clientHash), salt, iterations, memory, threads, argon2KeyLen)
	})
	if err != nil {
		return "", err
	}
	return formatArgon2(memory, iterations, threads, salt, key), nil
}

// VerifyClientHash 校验客户端派生值是否与已存储的 argon2id 串匹配，使用常量时间比对。
func VerifyClientHash(encoded, clientHash string) (bool, error) {
	memory, iterations, threads, salt, want, err := parseArgon2(encoded)
	if err != nil {
		return false, err
	}
	got, err := argon2InSlot(func() []byte {
		return argon2.IDKey([]byte(clientHash), salt, iterations, memory, threads, uint32(len(want)))
	})
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// argon2MaxConcurrent 是同时进行的 argon2 计算上限。
//
// 【为什么必须有全局闸门】argon2 的代价是**内存**：默认 m=64MiB，10 个并发就是
// 640MiB。攻击者不需要爆破成功，只要并发打 /auth/login 或 /auth/register 就能把
// 进程顶到 OOM —— 这是"用防御成本攻击防御方"的典型放大器，且它绕过了所有
// 基于"失败次数"的限流（每次请求都在正常计算）。
//
// 8 的取值：2C4G 部署下 8×64MiB=512MiB 峰值可控，且 8 路并发足够撑住正常登录
// 峰值（单次约 30–50ms，8 路 ≈ 160–260 req/s）。超出部分排队而非拒绝——
// 排队放大延迟但不会放大内存，这正是我们要的权衡。
//
// 注意：闸门只限制**并发度**，不替代限流；两者是不同层次的防护。
const argon2MaxConcurrent = 8

// argon2QueueTimeout 是排队等闸门的上限。
//
// 【为什么不能无限排队】闸门只限制并发，不限制排队长度：攻击者持续打登录接口时，
// 每个请求都会持有一个 goroutine 在 channel 上等待，内存与 fd 仍然线性增长，
// 只是从"内存爆炸"变成"goroutine/fd 泄漏"。超出该时长直接拒绝（503），
// 让客户端退避，把压力挡在进程之外。
//
// 声明为 var 而非 const：测试需要把它缩短到毫秒级来验证「排队超时返回错误」，
// 否则每条用例都要真等 2 秒。生产代码不得改写它。
var argon2QueueTimeout = 2 * time.Second

// ErrArgon2Busy 表示排队超时。导出是为了让 admin 登录等调用方能区分
// 「过载」与「口令错误」——否则过载会被当成一次失败尝试累计进锁定计数。
var ErrArgon2Busy = errors.New("argon2 计算排队超时")

var argon2Slots = make(chan struct{}, argon2MaxConcurrent)

// argon2InSlot 在闸门内执行一次 argon2 计算；排队超时返回 errArgon2Busy。
func argon2InSlot(fn func() []byte) ([]byte, error) {
	select {
	case argon2Slots <- struct{}{}:
		defer func() { <-argon2Slots }()
		return fn(), nil
	case <-time.After(argon2QueueTimeout):
		return nil, ErrArgon2Busy
	}
}

// argon2Err 把 argon2 相关错误统一映射为对外错误。
//
// 闸门排队超时不是「服务端 bug」，而是过载信号：必须回 503 让客户端退避，
// 不能混进 500（那会让调用方以为是自己请求有问题而立即重试，反而加剧排队）。
func argon2Err(err error) error {
	if errors.Is(err, ErrArgon2Busy) {
		return apierr.New(apierr.CodeUnavailable, "服务繁忙，请稍后再试")
	}
	return apierr.Wrap(apierr.CodeInternal, err, msgInternal)
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
