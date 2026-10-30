# MedDesk 安卓化契约（单一真相源）

> 本文件是本次并行开发的**唯一接口真相源**。所有子 agent 必须以此为准，
> **子任务之间只通过本文定义的接口交互，不得调用对方未在此声明的内部实现。**
>
> 冲突仲裁原则：发现需要修改**不属于自己**的文件时，禁止自行修改，
> 在产出中列出「建议改动 + 理由」，由集成方统一处理。

---

## 0. 通用约束（所有子任务必须遵守）

| 项 | 要求 |
|---|---|
| 注释语言 | 简体中文，解释「为什么」而非复述代码 |
| 禁用 emoji | 代码、注释、文档中一律不用 |
| 前端 JS 风格 | 与 `core.js`/`export.js` 一致：IIFE 包裹、`var` 声明、挂载到 `MD.*` 命名空间、**不**引入构建步骤 |
| 前端兼容性 | 必须能在无 Capacitor 的普通浏览器里加载且不报错（原生能力用 `isNative()` 守卫，降级不抛异常） |
| 后端 Go 风格 | 现有 `internal/*` 分层约定，改动必须带单元测试 |
| Android 目标 | `minSdk 26` / `targetSdk 34`，Kotlin + AndroidX |
| 新增文件原则 | 尽量只**新增**文件；确需修改现有文件时，改动面必须最小且保持向后兼容 |

### 0.1 代码所有权矩阵（修改不属于自己的文件即视为违约）

| 路径 | 归属 | 说明 |
|---|---|---|
| `capacitor.config.json`、`package.json`、`.gitignore` | **E 工程脚手架** | |
| `android/app/src/main/java/fun/qiubite/meddesk/alarm/*.kt` | **A 原生插件** | 插件与广播接收器 |
| `capacitor-plugins/meddesk-alarm/*.ts` | **A 原生插件** |  Bridge 类型契约 |
| `web/assets/js/native-alarm.js` | **B Web 桥接** | JS 侧封装，**不得改 schedule.js / med.js** |
| `web/assets/js/export-share.js` | **C 导出分享** | |
| `web/assets/js/export.js` | **C 导出分享** | 仅允许改 `download()` 一处 |
| `internal/config/config.go` | **D CORS 配置** | |
| `internal/middleware/cors.go` | **D CORS 配置** | |
| `internal/middleware/cors_test.go` | **D CORS 配置** | 新增 |
| `docs/ANDROID.md` | **E 工程脚手架** | |
| `web/index.html`、`tmp/*`、部署 | **集成方保留** | 任何 agent 不得改动 |

---

## 1. Contract A：原生闹钟插件 `MeddeskAlarm`（A 负责）

### 1.1 标识

```
插件名（Capacitor plugin id）: MeddeskAlarm
Kotlin 包:                      fun.qiubite.meddesk.alarm
applicationId:                  fun.qiubite.meddesk
Android:                        minSdk 26 / targetSdk 34
```

### 1.2 数据结构

```ts
// 一条待提醒的服药剂量
interface DoseAlarm {
  occKey: string;   // 必填。全局唯一键，来自 web 端 MD.sched.buildToday() 的 occKey 字段
  atMs:   number;   // 必填。Unix 毫秒时间戳（绝对时间），必须 > 0
  title:  string;   // 必填。通知标题，例：「布洛芬 该服药了」
  body:   string;   // 必填。通知正文，例：「1 片 · 08:00」
}

// 用户在通知上的操作结果（离线暂存，供 Web 侧拉取）
interface DoseAction {
  occKey: string;
  action: 'taken' | 'skipped' | 'snoozed' | 'opened';
  atMs:   number;   // 操作发生时刻，Unix 毫秒
}

// 权限与系统状态
interface AlarmStatus {
  notifications: boolean;                    // 通知权限是否已授予（Android 13+）
  exactAlarm:    boolean;                    // 是否可注册精确闹钟 SCHEDULE_EXACT_ALARM
  ignoringBattery: boolean;                  // 是否已加入电池优化白名单
}
```

### 1.3 插件方法（JS 可调用）

```ts
interface MeddeskAlarmPlugin {
  /** 批量注册闹钟。同 occKey 覆盖旧值；幂等，重复调用结果一致。 */
  schedule(opts: { doses: DoseAlarm[] }): Promise<{ scheduled: number; failed: string[] }>;

  /** 取消全部已注册闹钟（不清空暂存的 DoseAction）。 */
  cancelAll(): Promise<{ cancelled: number }>;

  /** 拉取并清空离线期间暂存的服药操作。用于在 App 恢复前台后补写 */
  drainActions(): Promise<{ actions: DoseAction[] }>;

  /** 查询权限与系统状态，不做任何请求动作。 */
  status(): Promise<AlarmStatus>;

  /** 跳转到系统通知设置页（引导用户授权）。 */
  openNotificationSettings(): Promise<void>;

  /** 跳转到电池优化豁免设置页（国产 ROM 保活的关键一步）。 */
  openBatterySettings(): Promise<void>;
}
```

### 1.4 行为约定（A 必须满足）

1. `DoseAction` 必须**持久化**（SharedPreferences），保证 App 被杀后也不丢。
   `drainActions()` 语义为「取走即清空」。
2. `atMs` 早于当前时间的剂量**直接丢弃**并计入 `failed`，不得立刻弹通知。
3. 通知点击「已服用」「跳过」「稍后」均写入 `DoseAction` 并**关闭该条通知**。
4. 注册闹钟使用 `AlarmManager.setExactAndAllowWhileIdle`；
   精确闹钟不可用时**降级为 `setWindow()`**，不得静默失败。
5. **禁止**申请 `USE_EXACT_ALARM`（Play 政策限制），只用 `SCHEDULE_EXACT_ALARM`。
6. 所有异常必须 `call.reject(msg)`，不得让插件调用悬挂。

### 1.5 A 的产出文件

```
android/app/src/main/java/fun/qiubite/meddesk/alarm/MeddeskAlarmPlugin.kt
android/app/src/main/java/fun/qiubite/meddesk/alarm/AlarmReceiver.kt
android/app/src/main/java/fun/qiubite/meddesk/alarm/DoseStore.kt
capacitor-plugins/meddesk-alarm/definitions.ts
```

### 1.6 A 的验收标准

- 上述 5 个文件存在且**相互类型一致**（TS 定义与 Kotlin 字段逐一对齐）
- TS 定义的 `interface MeddeskAlarmPlugin` 导出，字段名/类型与本文件 1.3 完全一致
- 不在 `android/` 之外改动任何文件
- 产出中列出：需要在 `AndroidManifest.xml` 中声明的权限与 receiver 清单（**不要自己改 Manifest**，由 E 负责）

---

## 2. Contract B：Web 侧桥接模块 `MD.nativeAlarm`（B 负责）

### 2.1 依赖的上游契约（B 只读ublik调用，不得修改）

来自 `web/assets/js/schedule.js`（现有，零改动）：

```js
MD.sched.buildToday({ meds, schedules, intakes, profileId, nowMs })
// 返回条目字段：
// { occKey, medId, schedId, profileId, medName, dosageText,
//   timeMs, status, intakeId }
// status ∈ 'taken' | 'skipped' | 'snoozed' | 'pending' | 'missed'
```

### 2.2 对外接口（B 必须实现）

```js
MD.nativeAlarm.isNative()                 // -> boolean  Capacitor 环境才为 true
MD.nativeAlarm.syncAlarms()               // -> Promise<{scheduled, skipped, failed, reason}>
MD.nativeAlarm.drainAndApply()            // -> Promise<number>  应用离线操作到 MD.data，返回处理条数
MD.nativeAlarm.statusAsync()              // -> Promise<AlarmStatus | null>
MD.nativeAlarm.plugins()                  // -> object | null   原始 Capacitor 插件对象，供 E/集成方使用
```

### 2.3 行为约定（B 必须满足）

1. **非原生环境必须完全静默**：`isNative()` 为 false 时所有方法返回零值 Promise，不 throw、不 console.error。
2. 只对 `status === 'pending'` 或 `'missed'` 的剂量注册闹钟；`taken`/`skipped`/`snoozed` 一律跳过。
3. `occKey → DoseAlarm.body` 的文案格式：`'<dosageText> · <HH:mm>'`，例：`'1 片 · 08:00'`。
4. `drainAndApply()` 需要把离线操作写回服药记录，但 `MD.data` 的写入接口**不在本契范围内**，
   B **不得臆造或直接使用**。按下面的约定解耦：
   ```js
   MD.record.markStatus({ occKey, status, atMs })   // 由集成方实现并注入
   ```
   B 的实现中先判断 `MD.record && MD.record.markStatus` 是否存在，
   存在才调用，不存在则跳过并返回 0（不抛异常，不报错）。
5. 只读 `window.Capacitor`，不得 `require`/`import` 任何 npm 包（无构建步骤）。

### 2.4 B 的产出文件

```
web/assets/js/native-alarm.js        （唯一所有权）
```

### 2.5 B 的验收标准

- `node --check web/assets/js/native-alarm.js` 通过
- 纯浏览器环境下加载不报错：提供 `tmp/test-native-alarm.js` 断言脚本并自行跑通
   （断言：`isNative()===false` 时 `syncAlarms()` resolve 且 `scheduled===0`）
- 不改动 `schedule.js` / `med.js` / `index.html`

---

## 3. Contract C：导出降级 `MD.exportShare`（C 负责）

### 3.1 对外接口

```js
MD.exportShare.isNative()                          // -> boolean
MD.exportShare.saveBlob(blob, filename)            // -> Promise<{ok:boolean, via:'native'|'browser', path?:string}>
MD.exportShare.toBase64(blob)                      // -> Promise<string>  不含 data: 前缀
```

### 3.2 行为约定

1. `saveBlob` 在非原生环境必须**完全等价于现有浏览器下载**，不得改变用户可见行为。
2. 原生路径实现：`Filesystem.writeFile`（`Directory.Documents`，`MedDesk/` 子目录）
   → `Filesystem.getUri` → `Share.share({ files: [uri] })`。
3. 插件通过 `window.Capacitor.Plugins.Filesystem` / `.Share` 访问，**不得 import**。
4. base64 必须剥离 `data:*;base64,` 前缀后再交给 Filesystem。
5. 任何失败都必须 fallback 到浏览器下载路径并向 `MD.toast.show` 提示（若 toast 不存在则忽略）。

### 3.3 对 `export.js` 的唯一允许改动

```js
function download(blob, filename) {
  // 新增：原生环境下改走 MD.exportShare.saveBlob
  if (window.MD && MD.exportShare && MD.exportShare.isNative()) {
    MD.exportShare.saveBlob(blob, filename).catch(function () { legacyDownload(blob, filename); });
    return;
  }
  legacyDownload(blob, filename);
}
// legacyDownload = 现有 download 实现，原样保留
```

### 3.4 C 的验收标准

- `node --check` 通过两个文件
- 提供 `tmp/test-export-share.js` 断言脚本并跑通（断言：非原生时走浏览器路径且行为不变）
- 除 `download()` 外不改动 `export.js` 任何其他函数

---

## 4. Contract D：后端 CORS（D 负责）

### 4.1 现状（已核查）

- `config.go:145` 使用 `splitList(envString("MEDDESK_CORS_ALLOWED_ORIGINS",""))`，**已支持逗号分隔多源**
- `splitList` 实现在 `config.go:247`，会 trim 并丢弃空串
- `cors.go:25` `allowAny := len(allow) == 0`：白名单为空时**只放行同源**
- `cors.go:44` 已正确输出 `Vary: Origin`

### 4.2 D 的任务

1. 确认并把「App 容器源」加入可接受清单，至少支持：
   `capacitor://localhost`、`https://localhost`、`http://localhost`
2. 增加**配置项校验**：单个 origin 含空格或逗号以外的非法字符时告警（不中断启动）
3. 在 `docs/SECURITY.md` 或新建说明段落中写清：放行这些源的安全含义
4. **不要**改成 `*` 通配
5. 补充单元测试 `internal/middleware/cors_test.go`，覆盖：
   - 白名单为空 → 跨域源不放行、同源放行
   - 白名单含 `capacitor://localhost` → 该源放行且带 `Vary: Origin`
   - `OPTIONS` 预检返回 204 且带正确 `Allow-Methods`
   - 大小写不敏感 + 末尾斜杠容错

### 4.3 D 的验收标准

- `go test ./internal/middleware/ ./internal/config/` 全部通过
- 不修改 `internal/router.go`、`internal/server/*`
- 产出中给出**必须写入 `.meddesk.env` 的确切一行**

---

## 5. Contract E：工程脚手架（E 负责）

### 5.1 产出文件

```
capacitor.config.json
package.json                 （仅含 @capacitor/core、@capacitor/cli、@capacitor/android 依赖）
docs/ANDROID.md              （环境准备与首次构建步骤）
.gitignore 追加条目          （node_modules/、android/app/build/、*.apk、*.aab）
```

### 5.2 `capacitor.config.json` 必须包含的键

```json
{
  "appId": "fun.qiubite.meddesk",
  "appName": "MedDesk",
  "webDir": "web",
  "androidScheme": "https",
  "plugins": {}
}
```

`androidScheme` **必须显式写死**，使 API 源可预测——这是 Contract D 能提前配白名单的前提。

### 5.3 `AndroidManifest.xml` 清单由 E 汇总 A 的需求后写入 `docs/ANDROID.md`（不生成 android/ 工程）

原因：`npx cap add android` 会生成约 200 个文件的 Gradle 树，属于生成物而非源码，
由开发者本地执行；本仓库只维护手写的插件源码。

### 5.4 E 的验收标准

- JSON 文件可被 `python -m json.tool` 解析通过
- `docs/ANDROID.md` 包含：JDK/Android SDK 版本要求、`npx cap add android` 步骤、
  A 提供的 Manifest 权限清单、以及对 Contract D 那行 env 配置的引用

---

## 6. 集成点（集成方保留，任何 agent 不得改动）

集成方将在收到全部产出后统一执行：

1. `web/index.html` 在 `tools.js` 之前插入两个 `<script defer>`：
   `/assets/js/export-share.js` 与 `/assets/js/native-alarm.js`
2. 提供 `MD.record.markStatus({occKey, status, atMs})` 适配（Contract B 2.3-4）
3. 写入 `.meddesk.env` 的 CORS 行并重启验证

---

## 7. 全局假设（各 agent 如认为有误，在产出中指出但不得自行更正）

- 假设 1：`android/` 目录由 `npx cap add android` 生成，不入库。
- 假设 2：`meddesk.qiubite.fun` 是唯一 API 源，App 通过公网访问它（不做本地内置后端）。
- 假设 3：暂不做 iOS，故不涉及 `AppDelegate` / APNs。
- 假设 4：本机无 Android SDK，所有 Kotlin 代码**只能做静态审查**，无法编译验证。
- 假设 5：服药提醒是本地定时事件，不需要任何远程推送通道。
