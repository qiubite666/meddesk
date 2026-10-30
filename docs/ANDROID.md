# MedDesk 安卓壳（Capacitor）操作手册

> 面向开发者：照着本文档执行即可把现有 `web/` 前端打包成一个安卓 App。
> 本文不改动 `web/` 下任何一行代码，也不引入任何前端构建步骤。
>
> 相关契约：`docs/archive/CONTRACTS-ANDROID.md` 第 5 节（Contract E，本文档即其产出；原件已归档）、
> 第 1 节（Contract A，原生闹钟插件）、第 4 节（Contract D，后端 CORS）。

---

## 1. 这份壳工程包含什么

| 文件 | 归属 | 说明 |
|---|---|---|
| `capacitor.config.json` | 本文 | Capacitor 配置，**唯一**需要手写的配置 |
| `package.json` | 本文 | 只装 Capacitor 三件套，**没有** webpack / vite / 任何前端框架 |
| `docs/ANDROID.md` | 本文 | 本文档 |
| `.gitignore` | 本文 | 追加了安卓相关的忽略规则 |
| `android/app/src/main/java/fun/qiubite/meddesk/alarm/*.kt` | Contract A | 手写插件源码，**唯一**需要入库的安卓代码 |
| `android/` 下其余部分 | 生成物 | 由 `npx cap add android` 生成，见第 4 节 |

关键设计：**App 里跑的前端就是 `web/` 原样拷贝过去的静态文件**，零构建。
改了 `web/` 里的 JS，`npm run sync:android` 一次就能在 App 里看到效果。

---

## 2. 环境准备

### 2.1 版本要求

| 组件 | 要求 | 说明 |
|---|---|---|
| JDK | **17**（必须） | Capacitor 6 使用 Android Gradle Plugin 8.x，AGP 8.x 强制要求 JDK 17；JDK 11/21 都会报错 |
| Android Studio | Koala Feature Drop（2024.1.2）或更新 | 自带 AGP 8.x 兼容的 Gradle 插件与 SDK Manager |
| Android SDK Platform | **34**（Android 14） | 对应 `compileSdk 34`，必须与 `targetSdk` 一致 |
| Android SDK Build-Tools | 34.x | SDK Manager 里勾选 "Android SDK Build-Tools 34" |
| Android SDK Platform-Tools | 最新 | `adb` 所在 |
| Android SDK Command-line Tools | 最新 | `sdkmanager` / `avdmanager` 所在 |
| Gradle | 不装 | Android 工程用自带的 Gradle Wrapper（`./gradlew`），版本由 AGP 决定 |
| Node.js | >= 18 | 只用于跑 `npm` / `npx cap`，**不参与前端构建** |
| npm | >= 9 | Node 18 自带 |

与契约对齐的安卓参数（Contract 0 / 1.1）：

```
minSdk    26     （Android 8.0，Capacitor 6 默认 22，需要手动改，见 3.3）
targetSdk 34     （Android 14，Capacitor 6 默认即 34）
compileSdk 34
语言：Kotlin + AndroidX
```

### 2.2 环境变量

```bash
# Linux / macOS（写进 ~/.bashrc 或 ~/.zshrc）
export JAVA_HOME=/path/to/jdk-17
export ANDROID_SDK_ROOT=$HOME/Android/Sdk     # 旧写法 ANDROID_HOME 也认，二者留其一即可
export PATH=$PATH:$ANDROID_SDK_ROOT/platform-tools:$ANDROID_SDK_ROOT/cmdline-tools/latest/bin

# Windows（PowerShell，管理员）
setx JAVA_HOME "C:\Program Files\Java\jdk-17"
setx ANDROID_SDK_ROOT "%LOCALAPPDATA%\Android\Sdk"
```

自检：

```bash
java -version          # 应含 17
echo $ANDROID_SDK_ROOT # 路径存在且非空
adb --version          # 有输出
```

### 2.3 通过命令行安装 SDK（无 Android Studio 时）

```bash
sdkmanager --install "platforms;android-34" "build-tools;34.0.0" "platform-tools"
sdkmanager --licenses
```

---

## 3. 首次构建

在项目根目录（`capacitor.config.json` 所在目录）执行。

### 3.1 安装依赖

```bash
npm install
```

只装五个包：`@capacitor/core`、`@capacitor/android`、`@capacitor/filesystem`、
`@capacitor/share`、以及 `@capacitor/cli`（在 `devDependencies`，`npm install` 默认会装）。
后两个给 Contract C 的原生导出分享用。

版本一律写**主版本宽松范围** `^6.0.0`，不锁小版本：五个包必须同属 Capacitor 6
（插件与 core 主版本不一致会直接崩），`^6.0.0` 保证解析到最新的 6.x。
**不要**改成 7.x / 8.x——那会把 `targetSdk` 顶到 35+，与契约的 34 冲突（见第 9 节）。

> **本机未执行安装**：开发环境 npm 拉取返回 403，因此本工程的 `npm install`
> 与 Gradle 构建**均未经实机验证**。依赖版本与配置是静态推导的结果，
> 首次构建请按第 3 节步骤执行，遇到装不上或编译报错以实际输出为准。
> 没有任何前端框架与构建工具。

### 3.2 生成安卓工程

```bash
npx cap add android
```

这一步生成 `android/` 下约 200 个文件的 Gradle 树，**只在第一次执行**。
如果 `android/` 已存在，不要再跑（`cap add` 会提示已存在）。

### 3.3 对齐 minSdk / targetSdk

`npx cap add android` 生成的 `android/variables.gradle` 里 `minSdkVersion` 默认是 22，
与契约的 26 不符，必须改：

```gradle
// android/variables.gradle
ext {
    minSdkVersion = 26          // 改这里：22 -> 26
    compileSdkVersion = 34
    targetSdkVersion = 34
    ...
}
```

> 注意：这一步改的是**生成物**。如果哪天删掉 `android/` 重跑 `cap add android`，
> 这个改动会丢失，需要再改一次。

**26 是硬性下限，不是建议值**（Contract A 明确要求）：插件调用的
`AlarmManager.setExactAndAllowWhileIdle()` 是 API 23 才有的方法，且**没有**加版本守卫
（正因为契约已定 minSdk 26）。若按模板留着 22，在 API 21/22 的机器上执行 `schedule()`
会直接抛 `NoSuchMethodError`。插件其余的高版本 API（`NotificationChannel`、
`isIgnoringBatteryOptimizations()`、`canScheduleExactAlarms()`）都已有
`SDK_INT` 守卫，因此**不需要** 27/28/31 及以上的 minSdk。

### 3.4 同步前端资源

```bash
npx cap sync
```

等价于 `cap copy`（把 `web/` 拷到 `android/app/src/main/assets/public/`）
+ `cap update`（同步插件与依赖版本）。

### 3.5 用 Android Studio 打开

```bash
npx cap open android
```

首次打开会下载 Gradle 依赖（需要网络，可能几分钟）。
之后点 Run 即可装到模拟器或真机。

> **这一步必须由使用者实际执行才算数。** 本仓库只交付配置与文档，
> `npm install` / `cap sync` / Gradle 构建都**没有**在 CI 或本机跑过（见 3.1 的说明）。
> 首次集成请完整走一遍 `npm install` → `npx cap add android` → `npx cap sync` →
> `npx cap open android`，并把实际输出作为验收依据，不要假定文档里的命令一定零报错。

### 3.6 日常开发循环

```bash
# 改了 web/ 下的 JS/CSS 之后
npm run copy:android     # 只拷贝前端，比 sync 快

# 改了插件 Kotlin 源码或 npm 依赖之后
npm run sync:android     # 完整同步
```

便捷脚本汇总（`package.json`）：

| 命令 | 等价于 | 用途 |
|---|---|---|
| `npm run add:android` | `npx cap add android` | 首次生成安卓工程 |
| `npm run sync` | `npx cap sync` | 全平台同步 |
| `npm run sync:android` | `npx cap sync android` | 安卓完整同步 |
| `npm run copy:android` | `npx cap copy android` | 只同步前端资源 |
| `npm run open:android` | `npx cap open android` | 打开 Android Studio |

### 3.7 出包

Android Studio 菜单 **Build → Generate Signed Bundle / APK**：
- 调试包：`*.apk`（已被 `.gitignore` 忽略）
- 上架包：`*.aab`（同上）

签名用的 keystore **不要**放进仓库。

---

## 4. 关于 `android/` 目录

**`android/` 是 `npx cap add android` 的生成物，不是手写源码。**

本仓库只维护**手写的插件源码**：

```
android/app/src/main/java/fun/qiubite/meddesk/alarm/MeddeskAlarmPlugin.kt
android/app/src/main/java/fun/qiubite/meddesk/alarm/AlarmReceiver.kt
android/app/src/main/java/fun/qiubite/meddesk/alarm/DoseStore.kt
```

由 Contract A 提供，包名为 `fun.qiubite.meddesk.alarm`，与 `applicationId`
`fun.qiubite.meddesk` 对齐。

`.gitignore` 里只忽略了**构建产物**（`android/app/build/`、`android/.gradle/`、
`*.apk`、`*.aab`、`local.properties`、`node_modules/`），并没有整体忽略 `android/`。
因此：

- 插件源码会正常入库，这是期望行为；
- Gradle 树的其余生成文件**也会**被 git 看到。如果希望严格遵守契约假设 1
  （`android/` 不入库），需要集成方额外决定是否整体忽略 `android/` 再用
  `git add -f` 单独跟踪插件源码——这超出了本次「只准追加」的范围，故未擅自处理，
  已在产出中作为待确认项列出。

`local.properties` 记录本机 SDK 路径，**绝不入库**。

---

## 5. `AndroidManifest.xml` 需要声明的内容

> 本节内容由 **Contract A（原生闹钟插件）提供**，E 只做汇总，未改动任何 Manifest 文件。
> 清单来源：`MeddeskAlarmPlugin.kt` / `AlarmReceiver.kt` / `DoseStore.kt`。

填充位置：`android/app/src/main/AndroidManifest.xml`
（由 `cap add android` 生成，属于生成物，**每次重建 `android/` 工程后都要重新填充**）。

### 5.1 权限（放在 `<manifest>` 根标签内、`<application>` 之外）

```xml
<!-- Android 13(API 33)+ 通知权限；不声明则通知会被系统静默丢弃，status().notifications 恒为 false -->
<uses-permission android:name="android.permission.POST_NOTIFICATIONS" />

<!-- Android 12(API 31)+ 精确闹钟；只用 SCHEDULE_EXACT_ALARM，严禁 USE_EXACT_ALARM（Play 政策限制） -->
<uses-permission android:name="android.permission.SCHEDULE_EXACT_ALARM" />

<!-- openBatterySettings() 需要；缺失时该方法只能降级到电池优化列表页 -->
<uses-permission android:name="android.permission.REQUEST_IGNORE_BATTERY_OPTIMIZATIONS" />

<!-- 可选：通知渠道开了 enableVibration(true)，保留震动则建议声明；不要震动可去掉该权限，
     并同步去掉渠道的 enableVibration -->
<uses-permission android:name="android.permission.VIBRATE" />
```

### 5.2 接收器（放在 `<application>` 标签内）

```xml
<!-- 不需要 intent-filter：插件用显式 Component 构造 Intent，收窄暴露面 -->
<receiver
    android:name=".alarm.AlarmReceiver"
    android:exported="false" />
```

### 5.3 配套注意事项（来自 Contract A）

1. `android:exported="false"` 是**硬性要求**。置 `true` 会让任意第三方 App
   伪造 `occKey` 写脏服药记录。
2. `<application android:icon="...">` 必须存在（`npx cap add android` 默认就带）：
   通知的小图标与三个按钮图标都复用 `applicationInfo.icon`，插件没有、也不允许
   新增 drawable 资源。
3. **不需要** `USE_EXACT_ALARM`，也**没有**实现 `BOOT_COMPLETED` 开机恢复闹钟：
   设备重启后依赖 Web 侧调用 `MD.nativeAlarm.syncAlarms()` 重建。
4. **没有** `<uses-feature>`、`<service>` 或其他组件：插件不用前台服务、不用
   JobService，只靠 `AlarmManager` + 广播接收器。
5. `<application>` 属性（Contract A 确认）：
   - `android:icon` 必须存在（`npx cap add android` 默认就带）——见上面第 2 条；
   - `android:allowBackup` 保持默认 `true` 即可。闹钟台账存在 SharedPreferences 里，
     换机恢复时会连带还原成「旧设备的待触发闹钟」，但 App 首次启动 Web 侧会调
     `MD.nativeAlarm.syncAlarms()` 重新对齐，不会出问题。想彻底避免脏数据可设 `false`，
     但这不是默认建议；
   - `android:usesCleartextTraffic` **不需要**配置：`androidScheme` 是 `https`，
     页面源 `https://localhost`，后端走公网 HTTPS，不存在明文流量；
   - `MainActivity` 的 `MAIN` / `LAUNCHER` intent-filter **必须保留**：通知上的
     `opened` 操作走 `getLaunchIntentForPackage()`，没有 launcher Activity 就拉不起
     前台（该路径在 try/catch 里不会崩，但表现为点了通知没反应）。
4. 插件源码在 `cap sync` 后必须出现在这三个路径，否则编译不过：
   ```
   android/app/src/main/java/fun/qiubite/meddesk/alarm/MeddeskAlarmPlugin.kt
   android/app/src/main/java/fun/qiubite/meddesk/alarm/AlarmReceiver.kt
   android/app/src/main/java/fun/qiubite/meddesk/alarm/DoseStore.kt
   ```
   TS 类型契约在 `capacitor-plugins/meddesk-alarm/definitions.ts`
   （纯类型、无 import、无构建步骤）。

### 5.4 填充步骤

1. 打开 `android/app/src/main/AndroidManifest.xml`；
2. 把 5.1 的 `<uses-permission .../>` 插到 `<manifest>` 根标签内、`<application>` 之前；
3. 把 5.2 的 `<receiver .../>` 插到 `<application>` 标签内；
4. `npm run sync:android` 后在 Android Studio 重新 Build。

### 5.5 插件注册（首次 `cap add android` 后手工添加一次）

Capacitor 自定义插件必须在 `MainActivity.onCreate()` 里注册，否则
`window.Capacitor.Plugins.MeddeskAlarm` 根本不存在——
**这一步漏掉不会报错，表现是闹钟全部静默失效**（`MD.nativeAlarm` 走降级分支，界面无任何提示）。

**归属决策**：`MainActivity.java` 由 `npx cap add android` 生成，属于生成物，
**不纳入源码跟踪**（用户重新生成工程时仓库里的版本会被覆盖，反而造成版本漂移）。
因此下面是**一次性手工步骤**，每次重建 `android/` 工程后都要重做一遍。

文件：`android/app/src/main/java/fun/qiubite/meddesk/MainActivity.java`

```java
package fun.qiubite.meddesk;

import com.getcapacitor.BridgeActivity;   // cap add 已生成
import android.os.Bundle;                 // 新增
import fun.qiubite.meddesk.alarm.MeddeskAlarmPlugin;   // 新增

public class MainActivity extends BridgeActivity {
    @Override
    public void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);   // 必须保留且排在注册之前
        registerPlugin(MeddeskAlarmPlugin.class);   // 新增：紧跟 super.onCreate 之后
    }
}
```

要点：

1. 包名必须是 `fun.qiubite.meddesk.alarm.MeddeskAlarmPlugin`（与插件源码的
   `package fun.qiubite.meddesk.alarm` 一致，写错则编译不过）。
2. `registerPlugin(...)` 必须放在 `super.onCreate(savedInstanceState)` **之后**，
   放在之前会导致 Bridge 尚未初始化、注册被丢弃。
3. 只加这一行；除 `onCreate` 外**不要动其他生命周期方法**，尤其不要改写 `load()`
   （那是 Capacitor 2 的旧写法，Capacitor 6 已不支持）。
3. 改完执行 `npm run sync:android` 再重新 Build。
4. 验证：App 起来后在 WebView 控制台执行
   `!!window.Capacitor.Plugins.MeddeskAlarm`，返回 `true` 才算注册成功。

---

## 6. 服务端必须补的 CORS 白名单（否则 App 调不通 API）

### 6.1 为什么必须配

App 里的页面由**设备本地**的 WebView 加载（`web/` 被拷贝进 App 内部），
其页面源是 `https://localhost`；而 API 在公网 `https://meddesk.qiubite.fun`。
**两者不同源**，浏览器同源策略会拦掉所有 `/api/v1/*` 请求，控制台表现为
`CORS policy: No 'Access-Control-Allow-Origin' header`。

后端现状（Contract D 4.1 已核查）：`config.go` 的
`MEDDESK_CORS_ALLOWED_ORIGINS` 支持逗号分隔多源，白名单为空时**只放行同源**，
所以不配就是调不通。

前端已经在自动切绝对地址，不需要再改 `web/`：采用**统一参数化替换**——
在 `index.html` 里、`api.js` **之前**插一段 inline script 设置 `window.MD_API_BASE`，
由集成方注入，**不需要 Webpack 或任何构建步骤**。`web/assets/js/api.js` 的
`apiBase()`（第 22 行起）在 `Capacitor.isNativePlatform()` 为真时优先取该值，
否则回落到内置默认地址；非原生环境仍返回相对路径 `/api/v1`，浏览器与 PWA 行为不变。
也就是说 **App 里跨域一定会发生，本节的白名单是必需项，不是保险项**。

```html
<!-- index.html：必须放在 api.js 之前 -->
<script>
  window.MD_API_BASE = 'https://你的域名/api/v1';
</script>
```

取值注意：

- 默认占位是 `https://example.com`，部署时**必须换成自己的域名**，否则所有请求会打到占位域名；
- 本地联调可用 `http://localhost` 或局域网 IP，但 `apiBase()` 的校验正则是
  `/^https:\/\/.+/i`，**只接受 https 绝对地址**，http 值会被忽略并回落到内置默认地址。
  要支持 http 联调需放宽该正则（属 `web/`，由集成方决定，本文档不代改）。

### 6.2 必写的一行

在服务器上的 `.meddesk.env`（部署目录的环境文件，与 `MEDDESK_JWT_SECRET` 同一个文件）里加：

```bash
MEDDESK_CORS_ALLOWED_ORIGINS=capacitor://localhost,https://localhost,http://localhost
```

说明：

- 逗号分隔，**值内不要有空格**（`config.go` 的 `splitList` 会 trim，但脚本里容易踩引号坑）；
- **不要**写成 `*`（Contract D 4.2 第 4 条明确禁止）；
- 三个源的分工：`https://localhost` 是 `androidScheme: "https"` 下 Android 的**实际页面源**（必需）；
  `capacitor://localhost` 是 iOS 默认 scheme（本次不做 iOS，留作零成本兼容位）；
  `http://localhost` 是自定义 scheme / 本地联调兜底。
  **若改用 Capacitor 6 默认 scheme 之外的方案（例如改 `androidScheme`、
  或改用 `server.url` 加载远端站点），需同步增删这个列表**，否则会出现
  「源不匹配 → 请求被 CORS 拦掉」或「白名单留了用不到的源」。
- **不要追加本站源** `https://meddesk.qiubite.fun`（集成方裁决）：同源请求本就不受
  CORS 约束，写进去只会无谓放大放行面。副作用是白名单非空后中间件走**纯精确匹配**，
  同源响应里不会回显 `Access-Control-Allow-Origin`——这是**正常现象，不是故障**，
  不要拿「响应头里没有 ACAO」当判断依据。

### 6.3 生效与验证

```bash
# systemd 部署
sudo systemctl restart meddesk

# docker compose 部署
docker compose -f deploy/docker-compose.yml restart meddesk
```

验证（在任意机器上执行，看响应头）：

```bash
curl -i -s -X OPTIONS https://meddesk.qiubite.fun/api/v1/health \
  -H 'Origin: https://localhost' \
  -H 'Access-Control-Request-Method: GET' | head -20
```

期望看到：

```
HTTP/2 204
Access-Control-Allow-Origin: https://localhost
Vary: Origin
```

放行这三个源的安全含义见 Contract D 第 4.2 条与 `docs/SECURITY.md`
（由 Contract D 负责补充说明）。

---

## 7. 为什么 `androidScheme` 写死为 `https`

`capacitor.config.json` 里：

```json
"androidScheme": "https"
```

原因：

1. **让 App 的源可预测。** Capacitor 默认的安卓 scheme 在不同版本间变过
   （`http` → `https` → `capacitor://`）。一旦依赖默认值，App 的页面源就可能随
   Capacitor 升级而漂移，后端 CORS 白名单、OAuth 回调、Cookie 域全部要跟着改。
2. **给 Contract D 一个确定的输入。** 写死之后，App 源恒为 `https://localhost`，
   D 才能在拿到代码之前就把白名单写好（第 6.2 节那一行），不必等 App 跑起来再抓包猜源。
3. **`https` 比 `http` 更容易被后端当作可信源。** 部分中间件对 `http://` 源会更严格，
   用 `https://localhost` 走得通的路更宽。
4. **不引入证书问题。** 这是 WebView 内部的私有 scheme，不走真实 TLS，
   `https://localhost` 只在 App 内部有效，不影响真实证书与公网访问。

JSON 不支持注释，所以把理由写在这里而不是配置文件里。

---

## 8. 排错表

| 现象 | 原因 | 处理 |
|---|---|---|
| `cap` 命令不存在 | 依赖没装 | `npm install` |
| Gradle 报 JDK 版本错 | 用了非 17 的 JDK | `JAVA_HOME` 指向 JDK 17 |
| 编译报 `uses-sdk:minSdkVersion 22` | 没改 `variables.gradle` | 按 3.3 改成 26 |
| App 白屏 / 404 | 没同步前端 | `npm run copy:android` |
| 所有 API 请求被 CORS 拦 | 服务端没配白名单 | 按第 6 节配 `.meddesk.env` 并重启 |
| 通知不弹 | Android 13+ 未授权通知 | 用插件的 `openNotificationSettings()` 引导；真机需在系统设置里手动开 |
| 闹钟全都不响、界面无报错 | 忘了注册插件 | 按 5.5 在 `MainActivity.onCreate()` 里 `registerPlugin` |
| 国产 ROM 闹钟不准 | 电池优化杀后台 | 引导用户走 `openBatterySettings()` 加入白名单 |
| 模拟器能跑、真机装不上 | 签名 / ABI 问题 | Android Studio Build → Generate Signed Bundle / APK |

---

## 9. 已裁决与遗留事项

1. **API 基址：已解决（无需再改前端）。** `web/assets/js/api.js` 的 `apiBase()`
   在 `Capacitor.isNativePlatform()` 为真时自动切到 `https://meddesk.qiubite.fun/api/v1`，
   浏览器/PWA 仍走相对路径。换域名用 `index.html` 里的 `window.MD_API_BASE` 覆盖
   （只接受 https 绝对地址）。
   备选方案若哪天要改用 Capacitor `server.url` 直接加载远端站点：页面源变成
   `https://meddesk.qiubite.fun`，与 API 同源，第 6 节的白名单**不是必需项**
   （但照常写上也没有副作用）；代价是失去离线可用、`webDir` 不再生效。
   已知观感问题（已裁决**不修复**）：白名单非空后中间件走**纯精确匹配**，
   本站自身源不在名单里时响应不会回显 `Access-Control-Allow-Origin`。
   同源请求不受 CORS 约束、功能正常，**不要把它误判成故障**；
   也不追加 `https://meddesk.qiubite.fun`，避免无谓放大放行面。
   安全含义：`localhost` 是整机共享的源，放行后**用户本机上的任意网页**
   都能带凭据读 API（详见 `docs/SECURITY.md`，由 Contract D 补充说明）。
   只发 Android 的话，除 `https://localhost` 之外两个源是零成本兼容位，
   等 iOS 真正上之前可考虑收窄。
2. **`MainActivity` 插件注册：已裁决。** 作为「首次 `cap add android` 后的一次性手动步骤」
   写在 5.5，`MainActivity.java` 属生成物、**不纳入源码跟踪**（否则重建工程时会被覆盖、
   造成版本漂移）。文档已给出可直接复制的完整片段与验证方法。
3. **Contract C 依赖的官方插件：已解决。** `@capacitor/filesystem`、`@capacitor/share`
   已由集成方补入 `package.json`，版本锁在 `^6.0.4` 与 `@capacitor/core ^6.2.2` 主版本对齐
   （插件与 core 主版本不一致会直接崩）。`npm install` 后执行 `npm run sync:android`
   即可让原生分享生效；不装也不会报错，C 会 fallback 到浏览器下载。
4. **Capacitor 版本与 targetSdk 的绑定**：当前锁在 6.x（对应 targetSdk 34）。
   升级到 Capacitor 7/8 会把 targetSdk 顶到 35+，与契约的 34 冲突，
   升级前需先改契约。
