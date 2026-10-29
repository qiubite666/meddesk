package auth

import (
	"context"
	crand "crypto/rand"
	"crypto/subtle"
	"fmt"
	"math/big"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/qiubite666/meddesk/internal/cache"
)

// 图形验证码——针对注册接口的自动化滥用防护。
// 只防「脚本批量注册」，不承担人机对抗的强安全职责：
// 因此字符集只去掉肉眼易混的字符，而不是加噪到人眼也难辨认。
const (
	// captchaTTL 是单张验证码的有效期。给足用户填写表单的时间，同时不让条目久留。
	captchaTTL = 5 * time.Minute
	// captchaLength 是验证码字符数。
	//
	// 4 位字母数字（31^4≈92 万）改成 6 位纯数字（8^6≈26 万）是刻意的取舍：
	// 点阵字体只覆盖 8 个数字，字符越少点阵表越可控；熵略降但配合
	// 「5 次/分/IP + 5 次容错」的限流，在线爆破依旧不可行。
	// 纯数字还有个副作用好处：手机上直接唤起数字键盘。
	captchaLength = 6
	// captchaMaxFailures 是同一个 captchaId 的容错次数，超过即整体作废，
	// 防止攻击者用一张图暴力穷举（4 位字符集约 30 万种组合，必须靠次数上限兜底）。
	captchaMaxFailures = 5
	// captchaAlphabet 只用 2-9：0/1 在点阵字体里与 O/I 极易混淆，
	// 且纯数字在手机端能直接唤起数字键盘。
	captchaAlphabet = "23456789"
	// msgCaptchaInvalid 是验证码失败时对外统一的消息：不区分「不存在」「已过期」
	// 「已用过」「输错了」，避免调用方据此推断某个 id 是否真实存在。
	msgCaptchaInvalid = "验证码错误或已过期，请点击图片刷新后重试"
)

// CaptchaResponse 是 GET /auth/captcha 的响应。
// svg 直接是完整 SVG 源码（非 base64），前端 innerHTML 插入即可，少一次解码。
type CaptchaResponse struct {
	CaptchaID string `json:"captchaId"`
	SVG       string `json:"svg"`
	ExpiresIn int    `json:"expiresIn"`
}

// captchaStore 负责验证码的生成、暂存与校验。
// 存储复用 internal/cache：Redis 可用时多实例共享，不可用时自动降级为进程内实现，
// 过期与清理由缓存层统一保障（TTL + 后台清扫），这里不重复造轮子。
type captchaStore struct {
	c       cache.Cache
	ttl     time.Duration
	maxFail int
}

// newCaptchaStore 构造验证码存储。
func newCaptchaStore(c cache.Cache) *captchaStore {
	return &captchaStore{c: c, ttl: captchaTTL, maxFail: captchaMaxFailures}
}

// codeKey / failKey 是两类条目的键：正确答案与失败计数分开存，
// 失败计数独立过期，避免「答对就把计数键删掉」的写法在并发下丢失计数。
func captchaCodeKey(id string) string { return "captcha:code:" + id }
func captchaFailKey(id string) string { return "captcha:fail:" + id }

// issue 生成一张新验证码并返回 id 与 SVG 源码。
func (s *captchaStore) issue(ctx context.Context) (string, string, error) {
	code, err := randomCaptchaCode()
	if err != nil {
		return "", "", err
	}
	id := uuid.NewString()
	if err := s.c.Set(ctx, captchaCodeKey(id), code, s.ttl); err != nil {
		return "", "", err
	}
	notifyCaptchaIssued(id, code)
	return id, renderCaptchaSVG(code), nil
}

// captchaObserver 仅供测试注入。
//
// 【为什么需要它】
// 验证码的正确答案只存在于服务端，HTTP 层拿不到。而 internal/server 的集成测试
// 要通过真实 HTTP 完成注册（后续所有用例都依赖登录态），没有这个钩子就只能
// 把注册断言降级成「一定会 400」——那等于让端到端链路失去覆盖。
// 生产路径下 observer 恒为 nil，不发散任何信息，也不进日志或响应。
var (
	captchaObserverMu sync.Mutex
	captchaObserver   func(id, code string)
)

// SetCaptchaAnswerObserver 注册签发观察者（仅测试使用，传 nil 可注销）。
func SetCaptchaAnswerObserver(fn func(id, code string)) {
	captchaObserverMu.Lock()
	defer captchaObserverMu.Unlock()
	captchaObserver = fn
}

func notifyCaptchaIssued(id, code string) {
	captchaObserverMu.Lock()
	obs := captchaObserver
	captchaObserverMu.Unlock()
	if obs != nil {
		obs(id, code)
	}
}

// verify 校验并消耗验证码。返回 false 时调用方一律回「错误或已过期」。
// 校验通过即删除条目：验证码是一次性凭证，不能重放。
func (s *captchaStore) verify(ctx context.Context, id, code string) bool {
	id = strings.TrimSpace(id)
	code = strings.TrimSpace(code)
	if id == "" || code == "" || len(id) > 64 || len(code) > 16 {
		return false
	}
	// 先看失败次数：达到上限的 id 已作废，即使这一次输对也不放行。
	if n, err := s.c.GetInt(ctx, captchaFailKey(id)); err == nil && n >= int64(s.maxFail) {
		_ = s.c.Del(ctx, captchaCodeKey(id))
		return false
	}
	want, err := s.c.Get(ctx, captchaCodeKey(id))
	if err != nil {
		// 未命中或缓存故障都按失败处理（失败闭合），不因此放行注册。
		return false
	}
	if subtle.ConstantTimeCompare([]byte(strings.ToUpper(want)), []byte(strings.ToUpper(code))) == 1 {
		_ = s.c.Del(ctx, captchaCodeKey(id), captchaFailKey(id))
		return true
	}
	n, err := s.c.Incr(ctx, captchaFailKey(id), s.ttl)
	if err != nil {
		return false
	}
	if n >= int64(s.maxFail) {
		_ = s.c.Del(ctx, captchaCodeKey(id))
	}
	return false
}

// randomCaptchaCode 生成定长验证码。用 crypto/rand 而不是 math/rand：
// 后者可预测，攻击者拿到若干张图即可推断后续验证码（注册接口正是要防脚本）。
func randomCaptchaCode() (string, error) {
	buf := make([]byte, captchaLength)
	limit := big.NewInt(int64(len(captchaAlphabet)))
	for i := range buf {
		n, err := crand.Int(crand.Reader, limit)
		if err != nil {
			return "", err
		}
		buf[i] = captchaAlphabet[n.Int64()]
	}
	return string(buf), nil
}

// 文字与干扰线的取色：深色系保证对比度，浅色干扰线不遮挡笔画。
var (
	captchaTextColors = []string{"#2f3542", "#1e3799", "#3d3d3d", "#40407a", "#227093"}
	captchaLineColors = []string{"#b2bec3", "#74b9ff", "#a4b0be", "#81ecec"}
)

// captchaGlyphs 是 5x7 点阵字体，只覆盖 2-9。
//
// 【为什么不用 <text> 渲染】
// 早期版本把字符直接写进 <text> 节点，任何脚本一条正则
// （<text[^>]*>(.)</text>）就能把答案全部提出来 —— 验证码形同虚设。
// 改成点阵后 SVG 里只有一堆 <rect>，通用脚本读不出答案；要绕过必须
// 针对这套点阵写专用解码器，成本远高于直接放弃刷注册。
var captchaGlyphs = map[byte][7]string{
	'2': {"01110", "10001", "00001", "00010", "00100", "01000", "11111"},
	'3': {"11111", "00010", "00100", "00010", "00001", "10001", "01110"},
	'4': {"00010", "00110", "01010", "10010", "11111", "00010", "00010"},
	'5': {"11111", "10000", "11110", "00001", "00001", "10001", "01110"},
	'6': {"00110", "01000", "10000", "11110", "10001", "10001", "01110"},
	'7': {"11111", "00001", "00010", "00100", "01000", "01000", "01000"},
	'8': {"01110", "10001", "10001", "01110", "10001", "10001", "01110"},
	'9': {"01110", "10001", "10001", "01111", "00001", "00010", "01100"},
}

// renderCaptchaSVG 把验证码画成一张 SVG，字符以点阵方块呈现（不含任何文本节点）。
func renderCaptchaSVG(code string) string {
	const (
		cell = 4 // 单个点的边长
		glyphW = 5 * cell
		glyphH = 7 * cell
		gapX  = 7 // 字符之间的间距
	)
	width := 12 + len(code)*(glyphW+gapX)
	height := glyphH + 16

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-label="图形验证码">`,
		width, height, width, height)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#f4f6f8"/>`, width, height)

	// 干扰线先画，字符压在上面，保证仍然可读。
	for i := 0; i < 4; i++ {
		fmt.Fprintf(&b, `<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="%s" stroke-width="%d" opacity="0.7"/>`,
			rand.Intn(20), rand.Intn(height), width-20+rand.Intn(20), rand.Intn(height),
			captchaLineColors[rand.Intn(len(captchaLineColors))], 1+rand.Intn(2))
	}

	for i := 0; i < len(code); i++ {
		glyph, ok := captchaGlyphs[code[i]]
		if !ok {
			continue // 字符集与点阵表不同步时跳过而不是画出空白：宁可少一位也不画错位
		}
		baseX := 8 + i*(glyphW+gapX) + rand.Intn(4)
		baseY := 8 + rand.Intn(4)
		rotate := rand.Intn(25) - 12
		color := captchaTextColors[rand.Intn(len(captchaTextColors))]

		var cells strings.Builder
		for row := 0; row < 7; row++ {
			for col := 0; col < 5; col++ {
				if glyph[row][col] != '1' {
					continue
				}
				fmt.Fprintf(&cells, `<rect x="%d" y="%d" width="%d" height="%d"/>`,
					col*cell, row*cell, cell, cell)
			}
		}
		// 绕字符中心旋转：translate 之后的局部坐标里中心就是 (glyphW/2, glyphH/2)
		fmt.Fprintf(&b, `<g fill="%s" transform="translate(%d %d) rotate(%d %d %d)">%s</g>`,
			color, baseX, baseY, rotate, glyphW/2, glyphH/2, cells.String())
	}

	// 噪点：打断纯色背景，提高二值化脚本的识别成本。
	for i := 0; i < 24; i++ {
		fmt.Fprintf(&b, `<rect x="%d" y="%d" width="2" height="2" fill="%s" opacity="0.55"/>`,
			rand.Intn(width), rand.Intn(height),
			captchaLineColors[rand.Intn(len(captchaLineColors))])
	}

	b.WriteString(`</svg>`)
	return b.String()
}
