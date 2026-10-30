# 记账本模块契约（已冻结）

> 冻结日期：2026-10-29
> 所有子任务必须以此为准；发现缺陷写入「建议修改点」交主 agent 裁决，不得擅自改动他人在写的文件。

## 0. 架构决策（用户已确认）

| 决策项 | 结论 | 影响 |
|---|---|---|
| 加密模型 | **账本数据明文建表**（该模块放弃端到端加密） | 服务端可做聚合、统计、定时备份；账本金额/分类/备注明文落库 |
| 健康数据 | 仍保持端到端加密，不受影响 | 两者分属不同表，互不影响 |
| 每日备份 | **服务端定时**（默认 02:00 Asia/Shanghai），需持久化 WebDAV 凭据 | 需新增凭据加密存储与调度器 |

**必须向用户明示**：账本不是端到端加密的。设置页与文档须写明「记账数据以明文存储于服务器，便于统计与自动备份；健康数据仍为端到端加密」。

## 1. 数据模型

### ledger_categories（分类）
| 字段 | 类型 | 说明 |
|---|---|---|
| id | uuid PK | |
| user_id | uuid FK→users ON DELETE CASCADE | 所属账号 |
| kind | text CHECK in ('income','expense') | |
| name | text | 分类名（同账号同 kind 下唯一） |
| sort_order | int default 0 | |
| archived | bool default false | 停用；**停用不影响历史账单**（账单存快照名） |
| created_at / updated_at | timestamptz | |

### ledger_records（账单）
| 字段 | 类型 | 说明 |
|---|---|---|
| id | uuid PK | |
| user_id | uuid FK→users ON DELETE CASCADE | |
| type | text CHECK in ('income','expense') | |
| amount | **numeric(14,2) CHECK (amount > 0)** | 定点数，**禁止浮点** |
| currency | text default 'CNY' | 默认人民币 |
| category_id | uuid NULL | 分类被删后为 NULL |
| category_name_snapshot | text NOT NULL | 快照，分类停用/删除不丢分类信息 |
| transaction_at | timestamptz NOT NULL | 发生时间 |
| occurred_on | date NOT NULL | Asia/Shanghai 下的自然日，用于按日聚合 |
| note | text default '' | 标题/备注 |
| reimbursement_status | text CHECK in ('none','pending','done') default 'none' | 收入恒为 'none' |
| created_at / updated_at | timestamptz | |

### ledger_attachments（凭证）
| 字段 | 类型 | 说明 |
|---|---|---|
| id / user_id / record_id | uuid | record_id FK→ledger_records ON DELETE CASCADE |
| rel_path / mime / bytes / width / height | | 私有存储，仅所属账号可访问 |

### 索引
- `idx_ledger_records_user_time (user_id, transaction_at DESC, id DESC)`
- `idx_ledger_records_user_type (user_id, type, transaction_at DESC)`
- `idx_ledger_records_user_cat (user_id, category_id)`
- `idx_ledger_records_user_reimb (user_id, reimbursement_status) WHERE reimbursement_status <> 'none'`
- `idx_ledger_categories_user_kind (user_id, kind, sort_order)`

### 金额口径（重要）
- 数据库：`numeric(14,2)`，写入前校验 `^\d{1,12}(\.\d{1,2})?$` 且 > 0。
- API 同时返回 `amount: "12.34"`（字符串，精确）与 `amountCents: 1234`（整数，供前端计算/图表）。
- 聚合一律在 SQL 里 `SUM(amount)`（精确），再转 `(SUM(amount)*100)::bigint` 得到 cents。
- **Go 侧全程不出现 float：金额以 string 传递，cents 以 int64 传递。**

## 2. 接口（全部 `requireAuth`，账号一律取自会话，不信任请求体 user_id）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/ledger/categories?kind=` | 分类列表（首次访问自动播种内置分类） |
| POST | `/api/v1/ledger/categories` | 新建分类 |
| PATCH | `/api/v1/ledger/categories/:id` | 改名/排序/停用 |
| DELETE | `/api/v1/ledger/categories/:id` | 删除（被引用则 409） |
| GET | `/api/v1/ledger/records` | 分页筛选列表 |
| POST | `/api/v1/ledger/records` | 新增 |
| GET | `/api/v1/ledger/records/:id` | 详情 |
| PATCH | `/api/v1/ledger/records/:id` | 编辑 |
| DELETE | `/api/v1/ledger/records/:id` | 删除（级联删凭证） |
| GET | `/api/v1/ledger/summary` | 月度汇总 |
| GET | `/api/v1/ledger/trend` | 近 7 日趋势 |
| GET | `/api/v1/ledger/breakdown` | 分类金额 |
| GET | `/api/v1/ledger/reimbursements` | 待报销列表 |
| POST | `/api/v1/ledger/reimbursements/mark` | 批量改状态 |
| POST | `/api/v1/ledger/attachments` | 上传凭证（B4） |
| GET | `/api/v1/ledger/attachments/:id` | 预览（B4） |
| DELETE | `/api/v1/ledger/attachments/:id` | 删除（B4） |
| POST | `/api/v1/ledger/export/html` | HTML 报告（B6） |
| POST | `/api/v1/ledger/export/zip` | ZIP 包（B6） |
| GET | `/api/v1/ledger/export/json` | JSON 备份（B6） |
| POST | `/api/v1/ledger/import` | 导入恢复：合并/覆盖（B6） |
| POST | `/api/v1/ledger/backup/run` | 手动触发备份（B7） |
| GET | `/api/v1/ledger/backup/status` | 备份状态（B7） |

## 3. 返回结构

```jsonc
// 账单
{ "id":"...", "type":"expense", "amount":"12.34", "amountCents":1234,
  "currency":"CNY", "categoryId":"...", "categoryName":"餐饮",
  "transactionAt":"2026-10-29T12:00:00+08:00", "occurredOn":"2026-10-29",
  "note":"午饭", "reimbursementStatus":"none",
  "attachmentCount":1, "createdAt":"...", "updatedAt":"..." }

// 月度汇总
{ "month":"2026-10", "income":{"value":"1000.00","cents":100000},
  "expense":{"value":"300.00","cents":30000},
  "balance":{"value":"700.00","cents":70000} }

// 趋势（近 7 个自然日含今天，无数据补 0）
{ "days":[ {"date":"2026-10-29","income":{"value":"0.00","cents":0},"expense":{"value":"12.00","cents":1200}} ] }

// 分类金额（收入与支出分组，不得混算）
{ "income":[{"categoryId":"...","name":"工资","value":"1000.00","cents":100000}],
  "expense":[{"categoryId":"...","name":"餐饮","value":"12.00","cents":1200}] }

// 分页
{ "items":[...], "total":128, "page":1, "size":20 }
```

## 4. 内置分类（首次访问播种，按用户隔离）

- 收入：工资、奖金、投资收益、兼职、其他收入
- 支出：餐饮、交通、购物、住房、水电、医疗、教育、娱乐、数码、日用品、其他支出

## 5. 错误码

统一走 `apierr`：400 参数/校验错误、401 未登录、403 越权（不做区分时统一 404）、404 不存在、409 冲突（分类被引用）。
**越权一律返回 404**（不告诉攻击者资源是否存在）。

## 6. 时间与时区

- 存储用 `timestamptz`；自然日一律以 `Asia/Shanghai` 计算。
- 月度汇总、7 日趋势均按 `occurred_on`（上海时区自然日）统计。

## 7. 备份文件结构（B7）

```
ledger-backup-YYYY-MM-DD.zip
├── ledger.json          # 账单与分类（含附件清单）
├── ledger.xlsx          # 表格
├── images/<recordId>/<attachmentId>.<ext>
└── manifest.json        # {schema, version, generatedAt, recordCount, attachmentCount, checksum}
```

`manifest.json` 含格式版本、生成时间、条数、校验和；重复执行幂等（同名覆盖，不产生不可控重复文件）。

## 8. 待裁决事项

- Excel 生成是否需要引入第三方库（当前 go.mod 未包含 excelize）。候选：引入 `github.com/xuri/excelize` 或退化为 CSV。
- 调度器实现方式：进程内 ticker（简单，但多实例会重复）vs 数据库锁选主。
