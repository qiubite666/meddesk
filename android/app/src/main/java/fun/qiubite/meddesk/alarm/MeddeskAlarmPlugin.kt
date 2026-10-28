package fun.qiubite.meddesk.alarm

import android.app.AlarmManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.os.Build
import android.os.PowerManager
import android.provider.Settings
import androidx.core.app.NotificationManagerCompat
import com.getcapacitor.JSArray
import com.getcapacitor.JSObject
import com.getcapacitor.Plugin
import com.getcapacitor.PluginCall
import com.getcapacitor.PluginMethod
import com.getcapacitor.annotation.CapacitorPlugin
import org.json.JSONObject

/**
 * Contract A：MeddeskAlarm 插件（详细定义见 docs/CONTRACTS-ANDROID.md 第 1 节）。
 *
 * 【设计前提】
 * 服药提醒必须是「真闹钟」，不能靠 WebView 里的 setInterval：
 * 页面被系统回收、App 退到后台后 JS 定时器会被节流乃至停止。
 * 因此这里用 AlarmManager 把时间点交给系统托管 —— 只要进程被杀的时间点之前
 * 已经 register 过，系统仍会按时发出广播（Receiver 在 AndroidManifest 静态注册）。
 *
 * 【为什么不用 JobScheduler / WorkManager】
 * 两者的最小执行粒度与厂商对齐策略会引入不可控延迟（可能十几分钟），
 * 服药场景容忍不了。精确闹钟虽然也有 Doze 节流，但相对可控，且可降级到 setWindow。
 *
 * 【为什么拒绝 USE_EXACT_ALARM】
 * 该权限是为闹钟/日历类 App 保留的，普通用途会被 Play 判定违反政策。
 * 这里只用普通权限 SCHEDULE_EXACT_ALARM，并且接受它可能被用户关掉 ——
 * 关掉时降级到 setWindow，宁可延迟几分钟也不静默失败。
 *
 * 【Promise 不悬挂原则】
 * 每个 @PluginMethod 都是「try { call.resolve(...) } catch { call.reject(...) }」结构：
 * 一次调用恰好 resolve 或 reject 一次，绝不存在两边都不走的路径。
 */
@CapacitorPlugin(name = "MeddeskAlarm")
class MeddeskAlarmPlugin : Plugin() {

    private val alarmManager: AlarmManager
        get() = context.getSystemService(Context.ALARM_SERVICE) as AlarmManager

    private val doseStore: DoseStore
        get() = DoseStore.get(context.applicationContext)

    /* ------------------------------------------------------------ 插件方法 */

    /**
     * 批量注册闹钟。幂等：同一 occKey 重复调用会覆盖旧的时间与文案。
     * 返回 { scheduled: number, failed: string[] }，failed 里放的是无法注册的 occKey。
     */
    @PluginMethod
    fun schedule(call: PluginCall) {
        try {
            call.resolve(doSchedule(call))
        } catch (t: Throwable) {
            call.reject("ERR_SCHEDULE: ${t.message ?: t.javaClass.simpleName}")
        }
    }

    /** 取消全部已注册的闹钟。注意：不清空暂存的 DoseAction（契约 1.3 明确要求）。 */
    @PluginMethod
    fun cancelAll(call: PluginCall) {
        try {
            call.resolve(doCancelAll())
        } catch (t: Throwable) {
            call.reject("ERR_CANCEL_ALL: ${t.message ?: t.javaClass.simpleName}")
        }
    }

    /** 取出并清空离线期间暂存的用户操作（「取走即清空」语义见契约 1.4-1）。 */
    @PluginMethod
    fun drainActions(call: PluginCall) {
        try {
            call.resolve(doDrainActions())
        } catch (t: Throwable) {
            call.reject("ERR_DRAIN: ${t.message ?: t.javaClass.simpleName}")
        }
    }

    /** 只读地查询三项系统状态。不做任何权限请求动作，避免用户体验被打断。 */
    @PluginMethod
    fun status(call: PluginCall) {
        try {
            call.resolve(doStatus())
        } catch (t: Throwable) {
            call.reject("ERR_STATUS: ${t.message ?: t.javaClass.simpleName}")
        }
    }

    /** 跳转到本 App 的通知设置页，引导用户授予 POST_NOTIFICATIONS。 */
    @PluginMethod
    fun openNotificationSettings(call: PluginCall) {
        try {
            openNotificationSettings()
            call.resolve()
        } catch (t: Throwable) {
            call.reject("ERR_OPEN_NOTIFICATION_SETTINGS: ${t.message ?: t.javaClass.simpleName}")
        }
    }

    /** 跳转到电池优化豁免页。国产 ROM 上是保证后台闹钟能响的关键一步。 */
    @PluginMethod
    fun openBatterySettings(call: PluginCall) {
        try {
            openBatterySettings()
            call.resolve()
        } catch (t: Throwable) {
            call.reject("ERR_OPEN_BATTERY_SETTINGS: ${t.message ?: t.javaClass.simpleName}")
        }
    }

    /* -------------------------------------------------------------- 实现 */

    private fun doSchedule(call: PluginCall): JSObject {
        val doses = call.getArray("doses")
            ?: throw IllegalArgumentException("doses 必填且必须是数组")

        val nowMs = System.currentTimeMillis()
        val failed = JSArray()
        var scheduled = 0

        for (i in 0 until doses.length()) {
            val item = doses.optJSONObject(i)
            if (item == null) {
                failed.put("#$i")
                continue
            }
            val keyLabel = DoseStore.optTrimmedString(item, "occKey") ?: "#$i"

            // 校验失败一律计入 failed 而非 reject 整批：
            // 一条脏数据不该让同批次几十条有效闹钟一起注册失败。
            if (!isValidDose(item, nowMs)) {
                failed.put(keyLabel)
                continue
            }
            val occKey = DoseStore.optTrimmedString(item, "occKey")!!
            val title = DoseStore.optTrimmedString(item, "title")!!
            val body = DoseStore.optTrimmedString(item, "body")!!
            val atMs = item.opt("atMs") as Number

            try {
                scheduleOne(occKey, atMs.toLong(), title, body)
                scheduled++
            } catch (t: Throwable) {
                // 单条注册失败也要留下痕迹，避免 Web 侧以为注册成功了。
                failed.put(keyLabel)
                doseStore.removeScheduled(occKey)
            }
        }

        // 拆成单条语句而非链式调用：不同版本的 Capacitor 对 put(String, Object)
        // 重载的返回类型签名不完全一致，链式写法可能把整条表达式的类型推成 JSONObject，
        // 从而和 JSObject 返回类型对不上。写成独立语句即可绕过这个签名差异。
        val ret = JSObject()
        ret.put("scheduled", scheduled)
        ret.put("failed", failed)
        return ret
    }

    /**
     * 字段合法性校验。
     *
     * atMs 必须是数字（不收字符串：JS 侧 Date.now() 一律给 number，
     * 收到字符串说明上游出错了，宁塞进 failed 也不要猜）。
     * atMs <= 当前时间的直接丢弃：契约 1.4-2 要求不得补弹过期提醒，
     * 否则用户一打开 App 就会被一堆历史通知轰炸。
     */
    private fun isValidDose(item: JSONObject, nowMs: Long): Boolean {
        if (DoseStore.optTrimmedString(item, "occKey") == null) return false
        if (DoseStore.optTrimmedString(item, "title") == null) return false
        if (DoseStore.optTrimmedString(item, "body") == null) return false
        if (item.isNull("atMs")) return false
        val raw = item.opt("atMs")
        if (raw !is Number) return false
        val atMs = raw.toLong()
        return atMs > 0 && atMs > nowMs
    }

    /**
     * 注册单条闹钟。
     *
     * 幂等的关键：同一 occKey 复用同一个 requestCode（存在 SharedPreferences 里），
     * 且 Intent 的 action + data 完全一致 —— 系统判定为同一个 PendingIntent，
     * 新注册的触发时间会顶掉旧的，天然实现「同 occKey 覆盖」。
     */
    private fun scheduleOne(occKey: String, atMs: Long, title: String, body: String) {
        val rc = doseStore.requestCodeOf(occKey) ?: doseStore.nextRequestCode()
        doseStore.putScheduled(occKey, atMs, title, body, rc)

        val appCtx = context.applicationContext
        val intent = DoseStore.fireIntentFor(appCtx, occKey, atMs, title, body)
        val flags = PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE
        val pending = PendingIntent.getBroadcast(appCtx, rc, intent, flags)

        registerWithAlarmManager(pending, atMs)
    }

    /**
     * 下发到 AlarmManager。
     *
     * 降级策略本身实现在 DoseStore.registerAlarm()，与 Receiver 重排「稍后」共用一个入口，
     * 保证同一 occKey 在两条路径上精度一致。
     */
    private fun registerWithAlarmManager(pending: PendingIntent, atMs: Long) {
        DoseStore.registerAlarm(context.applicationContext, pending, atMs)
    }

    private fun doCancelAll(): JSObject {
        val appCtx = context.applicationContext
        var cancelled = 0

        // 先取快照再逐个取消：避免在遍历过程中修改台账引发的不确定性。
        val keys = doseStore.scheduledKeys()
        for (occKey in keys) {
            val rc = doseStore.requestCodeOf(occKey) ?: continue
            // FLAG_NO_CREATE：只用于「找到同一个 PendingIntent 然后取消」，
            // 不会被用于发送，因此不需要 FLAG_UPDATE_CURRENT。
            // 注意这里的 Intent 必须与注册时 action + data 完全一致
            // （extras 不参与 PendingIntent 相等判定，所以不传也没关系）。
            val intent = DoseStore.fireIntentFor(appCtx, occKey, 0L, "", "")
            val pending = PendingIntent.getBroadcast(
                appCtx, rc, intent,
                PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_NO_CREATE
            ) ?: continue
            alarmManager.cancel(pending)
            // 顺带把 PendingIntent 自身注销，防止某些 ROM 上仍残留可送达的句柄。
            pending.cancel()
            cancelled++
        }
        doseStore.clearScheduled()

        // 已经弹出、仍在通知抽屉里的通知不在清理范围内：
        // 那些闹钟已经消耗掉了，cancelAll() 语义上是对「未触发闹钟」的操作，
        // 顺手清掉会造成「用户正看着的提醒突然消失」。
        val ret = JSObject()
        ret.put("cancelled", cancelled)
        return ret
    }

    private fun doDrainActions(): JSObject {
        val actions = JSArray()
        for (a in doseStore.drainActions()) {
            val item = JSObject()
            item.put("occKey", a.occKey)
            item.put("action", a.action)
            item.put("atMs", a.atMs)
            actions.put(item)
        }
        // 同 doSchedule()：用独立语句组装，规避 put 重载签名差异。
        val ret = JSObject()
        ret.put("actions", actions)
        return ret
    }

    private fun doStatus(): JSObject {
        // 同 doSchedule()：用独立语句组装，规避 put 重载签名差异。
        val ret = JSObject()
        ret.put("notifications", NotificationManagerCompat.from(context).areNotificationsEnabled())
        ret.put("exactAlarm", canScheduleExactAlarms())
        ret.put("ignoringBattery", isIgnoringBatteryOptimizations())
        return ret
    }

    /* -------------------------------------------------------- 系统能力查询 */

    /**
     * 是否允许注册精确闹钟。
     *
     * Android 12（API 31）之前无需任何权限；
     * API 31 起 SCHEDULE_EXACT_ALARM 默认授予，但用户可在「闹钟和提醒」里关掉，
     * 因此每次注册前都要现场查询，不能只查一次缓存。
     */
    private fun canScheduleExactAlarms(): Boolean {
        return if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
            alarmManager.canScheduleExactAlarms()
        } else {
            true
        }
    }

    /**
     * 是否已加入电池优化白名单。
     *
     * API 23 之前不存在电池优化机制，视为已豁免，避免给出无意义的 false。
     */
    private fun isIgnoringBatteryOptimizations(): Boolean {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.M) return true
        val pm = context.getSystemService(Context.POWER_SERVICE) as? PowerManager ?: return false
        return pm.isIgnoringBatteryOptimizations(context.packageName)
    }

    /* ---------------------------------------------------------- 设置页跳转 */

    /**
     * 通知设置页。
     *
     * Android 8（API 26）起才有带 App 参数的 ACTION_APP_NOTIFICATION_SETTINGS；
     * 老版本只能用隐藏格式的 extras（字符串常量而非 SDK 常量），
     * 这里做了版本分流，并统一在打不开时降级到应用详情页。
     */
    private fun openNotificationSettings() {
        val pkg = context.packageName
        val intent = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            Intent(Settings.ACTION_APP_NOTIFICATION_SETTINGS)
                .putExtra(Settings.EXTRA_APP_PACKAGE, pkg)
        } else {
            Intent(Settings.ACTION_APP_NOTIFICATION_SETTINGS)
                .putExtra("app_package", pkg)
                .putExtra("app_uid", context.applicationInfo.uid)
        }
        intent.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
        startActivitySafely(intent)
    }

    /**
     * 电池优化豁免页。
     *
     * 首选 ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS：直接弹确认框，转化路径最短；
     * 其次降级到 ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS（列表页）。
     *
     * 合规提示：前者属于 Play 政策里的敏感权限意图，
     * 上架时需要在政策清单里声明「服药提醒」这一核心用途。若审核受阻，
     * 把这里改成只走列表页即可，其余逻辑无需改动。
     */
    private fun openBatterySettings() {
        val pkg = context.packageName
        val request = Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS)
            .setData(Uri.parse("package:$pkg"))
            .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
        if (request.resolveActivity(context.packageManager) != null) {
            context.startActivity(request)
            return
        }
        val list = Intent(Settings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS)
            .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
        startActivitySafely(list)
    }

    /** 统一兜底：目标页不存在时落到应用详情页，确保总有一个设置页能打开。 */
    private fun startActivitySafely(intent: Intent) {
        val pm = context.packageManager
        val fallback = Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS)
            .setData(Uri.parse("package:${context.packageName}"))
            .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)

        if (intent.resolveActivity(pm) != null) {
            context.startActivity(intent)
        } else if (fallback.resolveActivity(pm) != null) {
            context.startActivity(fallback)
        } else {
            throw IllegalStateException("无法打开系统设置页")
        }
    }

}
