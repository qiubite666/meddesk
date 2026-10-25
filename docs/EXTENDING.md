# MedDesk · 扩展指南（新增模块）

> MedDesk 的架构目标：**加一个功能模块 = 一个前端文件 + 一个后端包 + 一行表名白名单**，不动现有代码。
> 本文档以「健康记录（health）」为例走一遍标准步骤。

## 目录

1. [架构约定](#1-架构约定)
2. [新增一个模块的 6 步](#2-新增一个模块的-6-步)
3. [同步层为什么"免费"](#3-同步层为什么免费)
4. [二期候选模块与接口预留点](#4-二期候选模块与接口预留点)

---

## 1. 架构约定

```
web/assets/js/         前端：core/crypto/db/api/data/sync 是平台层（共享）；
                       各业务模块一个独立文件（med.js 是一期的示范）
internal/<module>/     后端：一个模块一个包，对外只暴露 NewService + Mount
internal/store/        数据层：所有 SQL 集中于此，其他包零 SQL
```

三条铁律（新增模块必须遵守）：

1. **模块之间不互相 import**——跨模块交互只走 `MD.bus`（前端）或显式接口（后端）。
2. **后端模块包不写 SQL**——需要新表时在 `internal/store` 加 repo + 迁移文件。
3. **同步的业务数据一律密文信封**——服务端不解析、不索引业务字段。

## 2. 新增一个模块的 6 步

### ① 表名加入白名单（后端，1 行）

```go
// internal/model/model.go
func KnownTables() []string {
    return []string{TableProfile, TableMedication, TableSchedule, TableIntake, TableSetting,
        TableVital /* 新增：vital = 体征记录 */}
}
```

同步表 `sync_records` **无需任何迁移**——它本来就按 `(user_id, table_name, record_id)` 存任意表名。

### ② 前端领域访问器（web/assets/js/data.js 中加一组）

```js
// 与 meds/schedules/intakes 同构：list 过滤 deleted、save 自动 ver++ 与置脏
MD.data.vitals = { list, get, save, remove };
```

### ③ 前端模块文件（新建 `web/assets/js/health.js`）

```js
(function () {
  'use strict';
  MD.health = {
    register: function () {
      MD.router.register({
        id: 'health', title: '健康记录', icon: 'heart', order: 100,
        render: function (el) { /* 数据 → 计算 → 渲染，参照 med.js 结构 */ }
      });
    }
  };
})();
```

在 `index.html` 的脚本链中插入（**必须在 `app.js` 之前**）：

```html
<script src="assets/js/health.js"></script>
```

在 `app.js` 启动链中注册（与 `MD.med.register()` 并列）：`if (MD.health && MD.health.register) MD.health.register();`

### ④ 后端模块包（新建 `internal/health/`，仅当需要服务端逻辑时）

```go
// internal/health/health.go
package health

func NewService(deps Deps) *Service {...}
func Mount(rg *gin.RouterGroup, svc *Service, requireAuth gin.HandlerFunc) {
    g := rg.Group("/health")
    g.Use(requireAuth)
    _ = g // POST /api/v1/health/metrics 等
}
```

挂载点只有一处：`internal/server/router.go` 的 `registerAPI`。错误处理复用 `apierr`（`writeError` 模式）。

### ⑤ 需要服务端真实表时才做

- 新增 `internal/store/migrations/0002_vital.sql`（**只增不改**，编号递增）；
- 在 `internal/store` 加 repo 与接口，fake 同步补一份（`internal/fake/`）；
- 集成测试（`internal/server/integration_test.go`）追加一组用例。

### ⑥ 侧栏入口

前端 `app.js` 的占位数组里已有入口（`health`/`todo`/`ledger`，`comingSoon: true`）——
模块实现后把对应项删掉或把 `comingSoon` 去掉即可，侧栏与移动端底部 Tab 会自动按 `order` 重排。

## 3. 同步层为什么"免费"

因为 `sync_records` 是 **K/V 密文表**：新模块的记录只是新的 `table_name` 取值，
push/pull/冲突裁决/游标逻辑全部原样复用——后端一行不改，就能跨设备同步。

前提只有两条：

- 记录是「自包含 JSON 对象」（信封模式），而不是需要服务端 JOIN 的关系行；
- 记录体积 ≤ `MEDDESK_SYNC_MAX_RECORD_BYTES`（默认 1MiB），照片走附件通道而不是塞进信封。

若模块确实需要**服务端可查询**的数据（如全局药品字典、公开的用药知识库），才建独立表——
这属于"服务端数据"，不进 E2EE 信封，并在 SECURITY.md 威胁模型中注明其明文属性。

## 4. 二期候选模块与接口预留点

| 候选 | 数据形态 | 主要工作 |
|---|---|---|
| 健康记录 | 体征测量（血压/血糖/体重），信封同步 | 前端视图 + 折线图（内联 SVG） |
| 日程待办 | 任务列表，信封同步 | 前端视图；提醒复用 `MD.remind` 调度器 |
| 家庭账本 | 收支记录，信封同步 | 前端视图 + 分类汇总 |

| 接口预留点 | 位置 | 说明 |
|---|---|---|
| Web Push（真·勿扰穿透） | `MD.remind.registerPush(vapidPublicKey)`（现为 `not_implemented` 占位） | 服务端需新增 VAPID 密钥管理与 push 网关表；`urgency=high` 可穿透 Android 勿扰 |
| 附件转 S3 兼容对象存储 | `internal/attachments` 的落盘层 | 抽象为 `BlobStore` 接口后接 MinIO/OSS，元数据表结构不变 |
| 照片 OCR 识别药名 | 客户端 | 密文照片服务端不可读，OCR 只能在前端（如 onnxruntime-web）或"导出解密后手动送 OCR" |
| 多用户共享档案 | `profile` 加成员关系表 | E2EE 下共享=共享 DEK 的密钥协商（服务端只见密文信封），设计成本高，暂缓 |
