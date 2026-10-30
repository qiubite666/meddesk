package ledger

// WebDAV 凭据的加密持久化（记账本每日备份）。
//
// 【为什么需要落盘】契约 0 节：备份由**服务端定时**执行（默认 02:00 Asia/Shanghai），
// 那一刻没有用户请求、没有会话，凭据只能从数据库里取。这是用户明确批准的取舍
// （见 ../PROJECT-SUMMARY.md「备份与调度」——该文档位于工作区根，不在本仓库；
// 原契约已归档为 docs/archive/CONTRACTS-LEDGER.md）。
//
// 【怎么降低风险】
//  1. 只存密文：AES-256-GCM，密钥来自环境变量 MEDDESK_SECRETBOX_KEY（base64 的 32 字节），
//     密钥不进数据库、不进代码、不进日志；
//  2. 每个字段独立 12 字节 nonce（GCM 禁止在同密钥下复用 nonce），四个 nonce 拼接成 48 字节的 iv 列；
//  3. AAD 绑定 user_id + 字段名：把密文复制到别的账号或别的字段都会解密失败，
//     防止"用 A 账号的密文顶替 B 账号"这类挪窝攻击；
//  4. 明文只出现在调用栈里，不写日志、不进错误、不回传前端。

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/qiubite666/meddesk/internal/apierr"
	"github.com/qiubite666/meddesk/internal/store"
)

// secretKeyEnv 是凭据加密密钥的环境变量名（base64 编码的 32 字节）。
const secretKeyEnv = "MEDDESK_SECRETBOX_KEY"

const (
	// nonceSize 是 AES-GCM 的标准 nonce 长度（12 字节）。
	nonceSize = 12
	// credentialFields 是加密字段的个数（base_url / username / password / path_prefix）。
	credentialFields = 4
	// ivSize 是 iv 列的字节数：4 个 nonce 顺序拼接。
	ivSize = nonceSize * credentialFields

	// 各字段在 iv 列中的偏移（顺序必须与加密时一致）。
	ivOffsetURL      = 0
	ivOffsetUsername = nonceSize
	ivOffsetPassword = nonceSize * 2
	ivOffsetPrefix   = nonceSize * 3
)

// StoredCredentials 是解密后的用户 WebDAV 凭据，只存在于内存中、用完即弃。
type StoredCredentials struct {
	BaseURL    string
	Username   string
	Password   string
	PathPrefix string // 相对根目录的子目录，可为空
}

// credentialStore 负责凭据的加密写入与解密读取。
type credentialStore struct{ repo store.LedgerBackupRepo }

func newCredentialStore(repo store.LedgerBackupRepo) *credentialStore {
	return &credentialStore{repo: repo}
}

// load 读取并解密该账号的凭据；未配置返回 store.ErrNotFound。
func (c *credentialStore) load(ctx context.Context, userID string) (*StoredCredentials, error) {
	key, err := secretBoxKey()
	if err != nil {
		return nil, err
	}
	row, err := c.repo.GetCredentials(ctx, userID)
	if err != nil {
		// 未配置 → store.ErrNotFound，由上层转成 "未配置" 而不是 500。
		return nil, err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	baseURL, err := openField(gcm, row.IV, ivOffsetURL, row.BaseURL, userID, "base_url")
	if err != nil {
		return nil, err
	}
	username, err := openField(gcm, row.IV, ivOffsetUsername, row.Username, userID, "username")
	if err != nil {
		return nil, err
	}
	password, err := openField(gcm, row.IV, ivOffsetPassword, row.Password, userID, "password")
	if err != nil {
		return nil, err
	}
	prefix, err := openField(gcm, row.IV, ivOffsetPrefix, row.PathPrefix, userID, "path_prefix")
	if err != nil {
		return nil, err
	}
	return &StoredCredentials{BaseURL: baseURL, Username: username, Password: password, PathPrefix: prefix}, nil
}

// save 加密并写入（覆盖）该账号的凭据。
func (c *credentialStore) save(ctx context.Context, userID string, creds StoredCredentials) error {
	key, err := secretBoxKey()
	if err != nil {
		return err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return err
	}
	iv := make([]byte, ivSize)
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return apierr.Wrap(apierr.CodeInternal, err, "生成加密随机数失败")
	}
	row := &store.WebdavCredentialRow{
		UserID:     userID,
		BaseURL:    sealField(gcm, iv, ivOffsetURL, creds.BaseURL, userID, "base_url"),
		Username:   sealField(gcm, iv, ivOffsetUsername, creds.Username, userID, "username"),
		Password:   sealField(gcm, iv, ivOffsetPassword, creds.Password, userID, "password"),
		PathPrefix: sealField(gcm, iv, ivOffsetPrefix, creds.PathPrefix, userID, "path_prefix"),
		IV:         iv,
	}
	if err := c.repo.SaveCredentials(ctx, row); err != nil {
		return apierr.Wrap(apierr.CodeInternal, err, "保存 WebDAV 凭据失败")
	}
	return nil
}

// delete 删除该账号的凭据密文（幂等）。
func (c *credentialStore) delete(ctx context.Context, userID string) error {
	if err := c.repo.DeleteCredentials(ctx, userID); err != nil {
		return apierr.Wrap(apierr.CodeInternal, err, "删除 WebDAV 凭据失败")
	}
	return nil
}

// configured 判断该账号是否已配置凭据（**不解密、不需要密钥**）。
//
// 为什么单独一个方法：未配置 MEDDESK_SECRETBOX_KEY 时，备份接口仍应能返回
// "已配置 / 未配置"以及备份功能是否启用，而不是统统报"密钥缺失"。
func (c *credentialStore) configured(ctx context.Context, userID string) (bool, error) {
	if _, err := c.repo.GetCredentials(ctx, userID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, apierr.Wrap(apierr.CodeInternal, err, "读取 WebDAV 凭据状态失败")
	}
	return true, nil
}

// ------------------------------------------------------------------ 加解密原语

// secretBoxKey 读取环境变量中的密钥。未配置或格式错误时返回 400，
// 错误信息里**绝不包含密钥内容**。
func secretBoxKey() ([]byte, error) {
	raw := strings.TrimSpace(os.Getenv(secretKeyEnv))
	if raw == "" {
		return nil, apierr.New(apierr.CodeBadRequest,
			"服务端未配置 MEDDESK_SECRETBOX_KEY，备份功能不可用（需 base64 编码的 32 字节密钥）")
	}
	key, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(key) != 32 {
		return nil, apierr.New(apierr.CodeBadRequest,
			"MEDDESK_SECRETBOX_KEY 无效：必须是 base64 编码的 32 字节密钥")
	}
	return key, nil
}

// newGCM 由密钥构造 AES-256-GCM。
func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeInternal, err, "初始化凭据加密器失败")
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, apierr.Wrap(apierr.CodeInternal, err, "初始化凭据加密器失败")
	}
	return gcm, nil
}

// aad 生成附加认证数据：把密文与"哪个账号的哪个字段"绑定。
func aad(userID, field string) []byte { return []byte(userID + "|" + field) }

// sealField 加密单个字段。
func sealField(gcm cipher.AEAD, iv []byte, offset int, plain, userID, field string) []byte {
	return gcm.Seal(nil, iv[offset:offset+nonceSize], []byte(plain), aad(userID, field))
}

// openField 解密单个字段；密文被挪到别的账号/字段时会因 AAD 不匹配而失败。
func openField(gcm cipher.AEAD, iv []byte, offset int, ct []byte, userID, field string) (string, error) {
	if len(iv) < offset+nonceSize {
		return "", apierr.New(apierr.CodeInternal, "凭据密文的 iv 长度异常")
	}
	plain, err := gcm.Open(nil, iv[offset:offset+nonceSize], ct, aad(userID, field))
	if err != nil {
		// 密钥被轮换或密文被篡改/挪用时走到这里：提示用户重新填写，不回显技术细节。
		return "", apierr.New(apierr.CodeInternal, "已保存的 WebDAV 凭据无法解密，请重新填写")
	}
	return string(plain), nil
}
