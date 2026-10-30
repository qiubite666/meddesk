# 记账本任务看板

> 契约：docs/CONTRACTS-LEDGER.md（已冻结）
> 架构决策：账本明文建表（放弃该模块 E2E）+ 服务端定时备份（持久化 WebDAV 凭据）
> 最后更新：2026-10-30 01:15

## 一、子 agent 分工（职责 / 上下文 / 约束 / 状态）

| ID | 职责 | 所需上下文 | 约束（独占文件 + 禁止触碰） | 状态 | 完成证据 |
|---|---|---|---|---|---|
| **lead**（主） | 冻结契约、路由接线、迁移合并、冲突裁决、集成 | 契约文档、router.go、迁移目录 | 独占 router.go / config.go / model/ledger.go / store/ledger_repo.go / ledger/service.go / ledger/http.go / ledger/attachment_contract.go / docs/** | 🔄 进行中 | 契约+看板已冻结；三个子模块已接线；B4 建议修改点已处理 |
| **B1 数据层** | 建表、索引、迁移、仓储 | 0001/0002 迁移写法、store 辅助函数 | 独占 migrations/0004*、store/ledger_repo.go、model/ledger.go | ✅ 完成 | 真实 PG 冒烟通过（播种 16 分类、numeric 精确、generate_series 趋势） |
| **B2 记账 CRUD** | 账单与分类增删改查 | B1 契约、apierr、会话取号 | 独占 ledger/service.go、ledger/http.go | ✅ 完成 | server/ledger_integration_test.go：两账号越权、统计口径、金额校验 |
| **B4 附件** | 凭证上传/预览/删除/权限 | 契约、attachment_contract.go、attachments 模块写法 | 独占 model/ledger_attachment.go、store/ledger_attachment_repo.go、ledger/attachment*.go、fake/ledger_attachment.go | ✅ 完成 | 11 个用例：2MB 超限、魔数嗅探、越权 404、磁盘清理、AttachmentSource |
| **B6 导出恢复** | HTML / XLSX / ZIP / JSON 导出与导入 | 契约、B2 仓储、AttachmentSource | 独占 ledger/export*.go、ledger/import.go、ledger/export_test.go | ✅ 完成 | 4 个接口齐备；merge/overwrite+confirm+快照回滚；XSS 转义、空结果 400 |
| **B7 每日备份** | 定时任务、打包、WebDAV、状态与日志 | webdav 包（Client 方法未导出，需在同包新文件加导出包装） | 独占 migrations/0005*、store/ledger_backup_repo.go、ledger/backup*.go、ledger/credstore.go、ledger/scheduler.go、webdav/backup_api.go | ✅ 完成 | httptest 假 WebDAV：上传+远端校验、同日幂等（1 文件）、凭据 AES-GCM 往返、跨账号挪密文解密失败 |
| **B8 前端页面** | 导航替换、工作台、报销页、导出/备份入口、响应式 | med.js 渲染与事件委托风格、core.js UI 助手、CSS 变量 | 独占 web/assets/js/ledger.js、ledger-page-test.js；仅可改 app.js 的占位路由片段、index.html 加脚本、sw.js 预缓存 | ✅ 完成 | 38 条 jsdom 断言全过；browser-check 104/104、wipe-flow 12/12 未受影响 |
| **B9 文档测试** | 文档齐备、测试补充、隐私扫描 | 全部模块产出 | 独占 docs/**（与 lead 共用需协调） | ⏳ 待开始 | — |

## 二、进度时间线（可见）

| 时间 | 事件 |
|---|---|
| 23:00 | lead 侦察：发现「家庭账本/日程待办」是占位路由；识别 E2E 与「服务端统计/备份」的架构冲突 |
| 23:10 | 用户裁决：账本明文建表 + 服务端定时备份 |
| 23:15 | lead 冻结契约 CONTRACTS-LEDGER.md，建立看板 |
| 23:30 | B1+B2 完成（迁移 0004、仓储、14 个接口），集成测试含两账号越权 |
| 23:45 | 部署 v0.3.49 → 真实 PG 冒烟：发现趋势接口 500（generate_series 与筛选占位符冲突） |
| 23:50 | lead 修复占位符冲突并复测通过（7 天，10-24 → 10-30） |
| 00:05 | 并行分派 B4 / B6 / B7 / B8 |
| 00:20 | B4（附件）、B7（备份）完成 |
| 00:35 | B6 产出落盘（其后 agent 因 429 中断，产出已由 lead 核验完整） |
| 00:50 | lead 集成接线：附件/导出/备份挂进 router；删除账单前先清磁盘凭证 |
| 01:00 | lead 处理 B4 建议修改点：fake Repos 补 `ledgerAttachments` 字段，去掉包级 map 双锁 |
| 01:10 | B8 完成（前端 1920 行），修掉 4 个真 bug（分页丢失前几页、导出不下载、凭证不能放大、点击区不足 44px） |
| 01:15 | lead 补 `categoryIds` 多选；Go 全量测试 + 前端三套回归通过 |

## 三、已完成功能清单

- 分类：内置 16 条自动播种、增删改、停用（archived，不影响历史快照）
- 账单：增删改查、分页筛选（类型 / 分类多选 / 关键词 / 时间范围 / 报销状态 / 指定 id）
- 金额：`numeric(14,2)`，Go 侧字符串 + 整数分，全程无浮点；拒绝 0、负数、三位小数
- 统计：月度汇总（收入/支出/结余）、7 日趋势（补 0）、分类金额（收支分组不混算）
- 报销：待报销列表与合计、批量标记、批量导出（不改状态）、批量操作校验归属
- 附件：JPG/PNG/WebP 魔数校验、≤2MB、私有存储、越权 404、删账单先清磁盘
- 导出：HTML（自包含 + 转义）、ZIP（xlsx + images + manifest）、JSON 备份（含附件 base64）
- 导入：merge（跳过已存在）/ overwrite（confirm + 快照回滚）
- 备份：凭据 AES-GCM 加密落库（AAD 绑定账号）、每日 02:00 打包上传、远端校验、同日幂等、状态可查
- 前端：导航「记账本」位于「日程待办」之上、三张手写 SVG 图（含空/加载/失败态）、响应式三档、暗色模式

## 四、待办与阻断

| 项 | 状态 | 说明 |
|---|---|---|
| B9 文档与隐私扫描 | 待做 | 功能说明、接口文档、数据模型与迁移、部署环境变量（只写变量名） |
| 浏览器逐页实测（360/768/1440） | 待做 | 需部署后在真实浏览器逐页走查 |
| 独立分支 + Conventional Commits + PR | 待做 | 推送前须隐私扫描 |
| Excel 生成 | 已实现（B6 自写最小 xlsx，未引入依赖） | 复杂格式不支持 |
| 多实例备份重复 | **已知限制** | 进程内 ticker 无选主；建议 `pg_advisory_xact_lock` 或 Redis 锁 |
| 分页 size 上限 200 | 已知限制 | 深翻页建议改游标分页 |
| 设置页未提示「账本明文存储」 | 待补 | 契约 §0 要求在设置页明示（导出页已有） |
| ZIP 的 images/ 无法被导入还原 | 待补 | 备份可下载，但导入侧暂不能还原附件 |

## 五、B8 提出的契约问题（已裁决）

1. 缺少按 recordId 列举附件的接口 → **已解决**：B4 的 `GET /ledger/attachments?recordId=` 已实现
2. 账单列表不支持分类多选 → **已解决**：lead 补 `categoryIds=a,b,c`
3. HTML 导出是否支持 ids → **已解决**：B6 的 `ExportFilter.IDs` 已实现
4. 待报销默认口径 → **已解决**：服务层硬编码 `reimbursement_status='pending'`

## 六、新增环境变量（只列名称）

`MEDDESK_SECRETBOX_KEY`、`MEDDESK_LEDGER_BACKUP_TIME`、`MEDDESK_LEDGER_BACKUP_TZ`、`MEDDESK_LEDGER_BACKUP_ENABLED`
