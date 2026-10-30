package ledger

// 记账本备份的 HTTP 接口（前缀 /api/v1/ledger/backup）。
//
// 账号一律取自会话（requireAuth 写入 gin 上下文的 "user_id"），
// 不接受请求体里的 user_id——越权在入口就被挡住。

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/qiubite666/meddesk/internal/apierr"
)

// BackupHandler 承载备份接口的 HTTP 处理函数。
type BackupHandler struct{ svc *BackupService }

// MountBackup 把备份路由挂到 rg 之下（rg 是 /api/v1 组，与 Mount 的约定一致），
// 全部要求登录态；svc 为 nil 时跳过挂载（便于调用方在未启用备份时不传）。
func MountBackup(rg *gin.RouterGroup, svc *BackupService, requireAuth gin.HandlerFunc) {
	if rg == nil || svc == nil {
		return
	}
	h := &BackupHandler{svc: svc}
	g := rg.Group("/ledger")
	if requireAuth != nil {
		g.Use(requireAuth)
	}
	g.PUT("/backup/credentials", h.saveCredentials)
	g.GET("/backup/status", h.status)
	g.POST("/backup/run", h.run)
	g.DELETE("/backup/credentials", h.deleteCredentials)
}

// saveCredentials 保存/更新凭据（先做连通性校验）。
func (h *BackupHandler) saveCredentials(c *gin.Context) {
	var in CredentialsInput
	if err := c.ShouldBindJSON(&in); err != nil {
		writeErr(c, apierr.Wrap(apierr.CodeBadRequest, err, "请求参数有误"))
		return
	}
	if err := h.svc.SaveCredentials(c.Request.Context(), userIDOf(c), in); err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "configured": true})
}

// deleteCredentials 删除已保存的凭据（幂等）。
func (h *BackupHandler) deleteCredentials(c *gin.Context) {
	if err := h.svc.DeleteCredentials(c.Request.Context(), userIDOf(c)); err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "configured": false})
}

// status 返回备份状态与最近运行记录（不含凭据内容）。
func (h *BackupHandler) status(c *gin.Context) {
	out, err := h.svc.Status(c.Request.Context(), userIDOf(c), backupStatusLimit)
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, out)
}

// run 手动触发一次备份（用户触发自己的，服务端不做管理员特权区分：
// 账号取自会话，只能备份自己的数据）。
func (h *BackupHandler) run(c *gin.Context) {
	run, err := h.svc.RunForUser(c.Request.Context(), userIDOf(c))
	if err != nil {
		writeErr(c, err)
		return
	}
	c.JSON(http.StatusOK, BackupRunView{
		Date:        run.RunDate,
		Status:      run.Status,
		RemotePath:  run.RemotePath,
		SizeBytes:   run.SizeBytes,
		RecordCount: run.RecordCount,
		Message:     run.Message,
		UpdatedAt:   run.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	})
}
