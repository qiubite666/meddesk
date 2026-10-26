// Package model 定义服务端共享的数据传输对象（DTO）。
//
// 重要：MedDesk 采用端到端加密（E2EE）。服务端只保存密文（iv + ct），
// 永远看不到用户明文数据；因此这里的同步类结构体只承载字节与元数据，
// 不含任何业务字段（药物名、剂量、成员姓名等）。
package model

import "time"

// 同步逻辑表名（客户端局部数据库的表名，服务端原样存储不做解析）。
const (
	TableProfile    = "profile"    // 家庭成员档案
	TableMedication = "medication" // 药物
	TableSchedule   = "schedule"   // 用药排程
	TableIntake     = "intake"     // 服药记录（事件型）
	TableSetting    = "setting"    // 跨设备同步的设置
)

// KnownTables 返回允许同步的表名白名单。
func KnownTables() []string {
	return []string{TableProfile, TableMedication, TableSchedule, TableIntake, TableSetting}
}

// IsKnownTable 判断表名是否在白名单内。
func IsKnownTable(t string) bool {
	for _, k := range KnownTables() {
		if k == t {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- 同步契约

// SyncItem 是一条同步记录（密文信封 + 冲突元数据）。
// 服务端按 (user_id, table, id) 唯一，按 (ver, updated_at) 做 LWW 冲突裁决。
type SyncItem struct {
	Table     string `json:"table"`              // 逻辑表名，见 KnownTables
	ID        string `json:"id"`                 // 客户端生成的 UUIDv4
	Ver       int64  `json:"ver"`                // 客户端版本号，每次本地修改自增
	UpdatedAt int64  `json:"updatedAt"`          // 客户端修改时刻（Unix 毫秒）
	Deleted   bool   `json:"deleted"`            // 墓碑标记
	IV        string `json:"iv"`                 // base64，AES-GCM 初始向量（12 字节）
	CT        string `json:"ct"`                 // base64，AES-GCM 密文（含 16 字节 tag）
	DeviceID  string `json:"deviceId,omitempty"` // 产生该修改的设备标识（可选）
	Seq       int64  `json:"seq,omitempty"`      // 服务端单调递增序列，仅出现在响应中
}

// SyncPushRequest 是上传本地变更的请求体。
type SyncPushRequest struct {
	Changes []SyncItem `json:"changes"`
}

// SyncPushResponse 返回已接受的变更、冲突项与最新游标。
// 冲突项 conflicts 为服务端当前胜出版本，客户端据此合并并提示用户。
type SyncPushResponse struct {
	Applied   []string   `json:"applied"`   // 已应用的 "table:id"
	Conflicts []SyncItem `json:"conflicts"` // 服务端版本（客户端需合并）
	Cursor    int64      `json:"cursor"`    // 应用后服务端最大 seq
	SyncedAt  int64      `json:"syncedAt"`  // 服务端时间（Unix 毫秒）
}

// SyncPullResponse 是增量拉取响应。
type SyncPullResponse struct {
	Items   []SyncItem `json:"items"`
	Cursor  int64      `json:"cursor"`  // 本次返回的最大 seq，客户端应持久化
	HasMore bool       `json:"hasMore"` // 是否还有更多（需继续翻页）
}

// SyncStatsResponse 用于设置页展示同步概况。
type SyncStatsResponse struct {
	Cursor      int64            `json:"cursor"`
	RecordCount int64            `json:"recordCount"`
	ByTable     map[string]int64 `json:"byTable"`
	ServerTime  int64            `json:"serverTime"`
}

// ---------------------------------------------------------------- 鉴权契约

// DeviceInfo 用于刷新令牌绑定与异常登录提示。
type DeviceInfo struct {
	DeviceID  string `json:"deviceId,omitempty"`
	UserAgent string `json:"userAgent,omitempty"`
	IP        string `json:"ip,omitempty"`
}

// RegisterRequest 注册请求。passwordHash 是客户端 PBKDF2 派生的结果（非明文口令）。
type RegisterRequest struct {
	Email       string `json:"email" binding:"required,email,max=160"`
	PasswordHash string `json:"passwordHash" binding:"required,min=32,max=128"`
	KDFSalt     string `json:"kdfSalt" binding:"required"`     // base64，客户端 PBKDF2 salt
	WrappedDEK  string `json:"wrappedDek" binding:"required"`  // base64，被 KEK 包裹的数据密钥
	DEKIV       string `json:"dekIv" binding:"required"`       // base64，包裹 DEK 所用的 IV
	Device      DeviceInfo `json:"device,omitempty"`
}

// LoginRequest 登录请求。
type LoginRequest struct {
	Email        string     `json:"email" binding:"required,email,max=160"`
	PasswordHash string     `json:"passwordHash" binding:"required,min=32,max=128"`
	Device       DeviceInfo `json:"device,omitempty"`
}

// SaltRequest 账户盐值查询请求（新设备首次登录的握手步骤）。
type SaltRequest struct {
	Email string `json:"email" binding:"required,email,max=160"`
}

// SaltResponse 账户盐值响应。
// 账户不存在时返回随机盐（防账户枚举）：用假盐派生的口令哈希登录必然失败，
// 与「账户存在但口令错误」的外部表现完全一致。
type SaltResponse struct {
	KDFSalt string `json:"kdfSalt"`
}

// RefreshRequest 刷新令牌请求。
type RefreshRequest struct {
	RefreshToken string     `json:"refreshToken" binding:"required"`
	Device       DeviceInfo `json:"device,omitempty"`
}

// LogoutRequest 登出请求（撤销单个刷新令牌）。
type LogoutRequest struct {
	RefreshToken string `json:"refreshToken" binding:"required"`
}

// ChangePasswordRequest 修改口令：需同时提交新的 KDF 参数与重新包裹的 DEK。
type ChangePasswordRequest struct {
	OldPasswordHash string `json:"oldPasswordHash" binding:"required,min=32,max=128"`
	NewPasswordHash string `json:"newPasswordHash" binding:"required,min=32,max=128"`
	KDFSalt         string `json:"kdfSalt" binding:"required"`
	WrappedDEK      string `json:"wrappedDek" binding:"required"`
	DEKIV           string `json:"dekIv" binding:"required"`
}

// TokenResponse 令牌响应。kdf* 字段供客户端本地解出数据密钥（DEK）。
type TokenResponse struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	TokenType    string `json:"tokenType"` // 固定 "Bearer"
	ExpiresIn    int64  `json:"expiresIn"` // access token 剩余秒数
	UserID       string `json:"userId"`
	KDFSalt      string `json:"kdfSalt"`
	WrappedDEK   string `json:"wrappedDek"`
	DEKIV        string `json:"dekIv"`
}

// UserDTO 对外暴露的用户信息。
type UserDTO struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"createdAt"`
}

// MeResponse /api/v1/auth/me 响应。
type MeResponse struct {
	User     UserDTO `json:"user"`
	ServerTime int64 `json:"serverTime"`
}

// ---------------------------------------------------------------- 存储实体

// User 是 users 表的行映射。
type User struct {
	ID           string
	Email        string
	PasswordHash string // argon2id(passwordHash 客户端派生值)
	KDFSalt      []byte
	WrappedDEK   []byte
	DEKIV        []byte
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// RefreshToken 是 refresh_tokens 表的行映射。
type RefreshToken struct {
	ID        string
	UserID    string
	TokenHash []byte // sha256(refreshToken)
	DeviceID  string
	UserAgent string
	IP        string
	ExpiresAt time.Time
	CreatedAt time.Time
	RevokedAt *time.Time
	ReplacedBy *string
}

// Attachment 是附件元数据（内容本身以密文落盘，服务端不可读）。
type Attachment struct {
	ID        string
	UserID    string
	RecordID  string // 关联的 medication id（可为空）
	IV        []byte
	Size      int64
	Mime      string
	Path      string // 相对 UploadDir 的存储路径
	CreatedAt time.Time
	Seq       int64
}

// AttachmentDTO 附件对外结构。
type AttachmentDTO struct {
	ID       string `json:"id"`
	RecordID string `json:"recordId"`
	IV       string `json:"iv"`
	Size     int64  `json:"size"`
	Mime     string `json:"mime"`
	CreatedAt time.Time `json:"createdAt"`
}

// ---------------------------------------------------------------- 导出契约

// ExportRow 是一行导出数据（由客户端解密后提交，服务端仅在内存中渲染，不落盘）。
type ExportRow struct {
	Date       string `json:"date"`       // 2026-10-25
	Time       string `json:"time"`       // 08:00
	Profile    string `json:"profile"`    // 成员名
	Medication string `json:"medication"`
	Dosage     string `json:"dosage"`     // 0.5 片 / 5 ml
	Status     string `json:"status"`     // taken|skipped|snoozed|pending|missed
	Note       string `json:"note"`
}

// ExportRequest 导出请求（PDF/CSV）。
type ExportRequest struct {
	Format     string      `json:"format" binding:"required,oneof=pdf csv"`
	Title      string      `json:"title"`
	Profile    string      `json:"profile"`
	RangeFrom  string      `json:"rangeFrom"`
	RangeTo    string      `json:"rangeTo"`
	Timezone   string      `json:"timezone"`
	Rows       []ExportRow `json:"rows"`
	Stats      ExportStats `json:"stats"`
}

// ExportStats 导出文件顶部汇总。
type ExportStats struct {
	Total    int     `json:"total"`
	Taken    int     `json:"taken"`
	Skipped  int     `json:"skipped"`
	Missed   int     `json:"missed"`
	Adherence float64 `json:"adherence"` // 0~1
}

// ---------------------------------------------------------------- 其他

// HealthResponse 健康检查响应。
type HealthResponse struct {
	Status    string `json:"status"` // ok | degraded | down
	Version   string `json:"version"`
	DB        string `json:"db"`
	Cache     string `json:"cache"`
	ServerTime int64 `json:"serverTime"`
}
