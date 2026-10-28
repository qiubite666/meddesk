package fun.qiubite.meddesk.alarm

import android.app.AlarmManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.os.Build
import org.json.JSONArray
import org.json.JSONObject

/**
 * 闹钟/通知数据的持久化与走 Intent 的编解码。
 *
 * 【为什么要自己写序列化】
 * 1. 这里用了 org.json（Android 框架自带），不引入 Gson/Moshi 依赖，
 *    避免插件源码被塞进未加 synthetic scope 的工程里时编译不过。
 * 2. 数据只含 ASCII 之外的中文，SharedPreferences 存 String 本身是 UTF-16，
 *    不需要 Base64 包装；一旦做 Base64 就引入「换行/填充字符」的坑
 *    （必须用 Base64.NO_WRAP），本项目索性不做这个转换。
 *
 * 【为什么每个方法都 synchronized】
 * 写入方有两个线程：WebView 主线程（插件方法）与 Binder 线程（BroadcastReceiver.onReceive）。
 * SharedPreferences 自身线程安全，但「读出 -> 改 -> 写回」是复合操作，
 * 不加锁会出现两个 Receiver 同时 append 动作时互相覆盖（经典的 read-modify-write 竞态）。
 * 这里统一在一把 monitor 锁里完成。
 *
 * 【为什么用 commit() 而不是 apply()】
 * apply() 先把改动提交到内存，再由 QueuedWork 异步落盘；若在落盘前进程被杀
 * （App 退后台时系统回收是很常见的场景），这条记录在复查日志时会丢。
 * 契约要求「App 被杀后也不丢」，因此选同步的 commit()。
 * 代价：commit() 不能在主线程做大批量写。这里的数据量极小（每天几十条），
 * 且 Receiver 里本来就在非主线程，成本可接受。
 * 插件方法在主线程调用 schedule()，单次提交几十条记录耗时在毫秒级，可接受。
 */
internal class DoseStore private constructor(private val prefs: android.content.SharedPreferences) {

    private val lock = Any()

    /* ------------------------------------------------------ 已注册闹钟台账 */

    /**
     * 记录一条已注册的闹钟。key 是 occKey，value 是 JSON：{atMs, title, body, rc}。
     *
     * rc（requestCode）必须持久化：
     * PendingIntent 的复用判定不比较 extras，只比较 action/data/class 等，
     * cancelAll() 要精确取消某一条闹钟，就必须拿到当初分配的那个 requestCode。
     * 用 occKey.hashCode() 代替是不可靠的 —— 不同 occKey 可能碰撞，
     * 一旦碰撞，取消的是另一条闹钟，用户会收到莫名的漏提醒。
     */
    fun putScheduled(occKey: String, atMs: Long, title: String, body: String, rc: Int) {
        synchronized(lock) {
            val idx = scheduledIndex()
            idx.put(occKey, JSONObject()
                .put("atMs", atMs)
                .put("title", title)
                .put("body", body)
                .put("rc", rc))
            prefs.edit().putString(KEY_SCHEDULED, idx.toString()).commit()
        }
    }

    /** 某条闹钟触发或取消后从台账移除，避免 cancelAll() 把已经消失的闹钟也算进 cancelled。 */
    fun removeScheduled(occKey: String) {
        synchronized(lock) {
            val idx = scheduledIndex()
            if (!idx.has(occKey)) return
            idx.remove(occKey)
            prefs.edit().putString(KEY_SCHEDULED, idx.toString()).commit()
        }
    }

    /** 返回当前台账里所有未被消费的 occKey（乱序，不保证与注册顺序一致）。 */
    fun scheduledKeys(): List<String> {
        synchronized(lock) {
            val idx = scheduledIndex()
            val out = ArrayList<String>(idx.length())
            val it = idx.keys()
            while (it.hasNext()) out.add(it.next())
            return out
        }
    }

    /**
     * 返回台账里全部可恢复的记录，供开机重建使用。
     *
     * 这里会顺手过滤掉残废条目（缺 rc、atMs 非正）：
     * 开机路径要把这些记录重新注册到系统，带着坏数据去注册只会让 a PendingIntent
     * 落在错误的 requestCode 上，进而连累 cancelAll()。
     */
    fun scheduledEntries(): List<ScheduledEntry> {
        synchronized(lock) {
            val idx = scheduledIndex()
            val out = ArrayList<ScheduledEntry>(idx.length())
            val it = idx.keys()
            while (it.hasNext()) {
                val key = it.next()
                val o = idx.optJSONObject(key) ?: continue
                val rc = o.optInt("rc", 0)
                val atMs = o.optLong("atMs", 0L)
                if (rc == 0 || atMs <= 0L) continue
                out.add(ScheduledEntry(
                    occKey = key,
                    atMs = atMs,
                    title = o.optString("title", ""),
                    body = o.optString("body", ""),
                    requestCode = rc
                ))
            }
            return out
        }
    }

    fun requestCodeOf(occKey: String): Int? {
        synchronized(lock) {
            val idx = scheduledIndex()
            if (!idx.has(occKey)) return null
            val rc = idx.getJSONObject(occKey).optInt("rc", 0)
            return if (rc == 0) null else rc
        }
    }

    /** 清空台账（cancelAll），不影响已暂存的操作记录。 */
    fun clearScheduled() {
        synchronized(lock) {
            prefs.edit().putString(KEY_SCHEDULED, JSONObject().toString()).commit()
        }
    }

    /* ---------------------------------------------------- requestCode 分配 */

    /**
     * 单调递增的 requestCode。
     *
     * 为什么从 1000 起步：
     * 留一段低位空间给将来可能的固定语义 Intent（如「全部已服用」这类聚合动作），
     * 避免与动态分配的号码撞车。
     *
     * 为什么要把计数器持久化：
     * 进程重启后若从 1000 重新开始，会把号码重新分配给不同的 occKey，
     * 导致 PendingIntent 复用错旧闹钟（extras 会被 FLAG_UPDATE_CURRENT 覆盖到别人身上）。
     */
    fun nextRequestCode(): Int {
        synchronized(lock) {
            val next = prefs.getInt(KEY_RC_SEQ, RC_BASE) + 1
            prefs.edit().putInt(KEY_RC_SEQ, next).commit()
            return next
        }
    }

    /* ---------------------------------------------------------- 操作暂存 */

    /** 追加一条用户操作。顺序按追加时间保证，drainActions() 依此顺序返回。 */
    fun appendAction(occKey: String, action: String, atMs: Long) {
        synchronized(lock) {
            val arr = actionArray()
            arr.put(JSONObject()
                .put("occKey", occKey)
                .put("action", action)
                .put("atMs", atMs))
            // 上限保护：极端情况下（用户长期不打开 App）避免预文件无限膨胀。
            // 超限时丢弃最旧的记录 —— 越旧的数据对业务补偿的意义越小。
            val trimmed = trimToCap(arr)
            prefs.edit().putString(KEY_ACTIONS, trimmed.toString()).commit()
        }
    }

    /** 「取走即清空」：返回当前全部暂存操作，并一次性清空。 */
    fun drainActions(): List<DoseAction> {
        synchronized(lock) {
            val arr = actionArray()
            val out = ArrayList<DoseAction>(arr.length())
            for (i in 0 until arr.length()) {
                val o = arr.optJSONObject(i) ?: continue
                out.add(DoseAction(
                    occKey = o.optString("occKey", ""),
                    action = o.optString("action", ""),
                    atMs = o.optLong("atMs", 0L)
                ))
            }
            prefs.edit().putString(KEY_ACTIONS, JSONArray().toString()).commit()
            return out
        }
    }

    /* ------------------------------------------------------------- 内部工具 */

    private fun scheduledRaw(): String = prefs.getString(KEY_SCHEDULED, null) ?: "{}"

    private fun actionsRaw(): String = prefs.getString(KEY_ACTIONS, null) ?: "[]"

    /**
     * 解析台账。坏数据一律当作空台账处理：
     * prefs 里的这份 JSON 只可能因为跨版本结构变更或磁盘损坏而损坏，
     * 此时比起让 Receiver 抛异常（进而丢失用户刚点的那一次操作记录），
     * 宁可从空台账重新开始 —— 最坏结果是漏取消几条闹钟，而不是崩进程。
     */
    private fun scheduledIndex(): JSONObject = try {
        JSONObject(scheduledRaw())
    } catch (t: Throwable) {
        JSONObject()
    }

    /** 同上：损坏的操作记录按空列表处理。 */
    private fun actionArray(): JSONArray = try {
        JSONArray(actionsRaw())
    } catch (t: Throwable) {
        JSONArray()
    }

    private fun trimToCap(arr: JSONArray): JSONArray {
        if (arr.length() <= ACTION_CAP) return arr
        val out = JSONArray()
        for (i in (arr.length() - ACTION_CAP) until arr.length()) {
            out.put(arr.opt(i))
        }
        return out
    }

    data class DoseAction(val occKey: String, val action: String, val atMs: Long)

    companion object {

        /* ------------------------------------------------------------ 常量 */

        const val PREFS_NAME = "meddesk_alarm"

        private const val KEY_SCHEDULED = "scheduled"   // occKey -> {atMs,title,body,rc}
        private const val KEY_ACTIONS = "actions"       // DoseAction 列表（JSON 数组）
        private const val KEY_RC_SEQ = "rc_seq"         // requestCode 分配游标

        private const val RC_BASE = 1000
        private const val ACTION_CAP = 500

        /** 精确闹钟不可用时的降级窗口：系统在该窗口内自行挑触发点，最坏延迟这么多。 */
        const val WINDOW_MS = 5 * 60 * 1000L

        /**
         * 点击「稍后」后的推迟时长。
         *
         * 放在原生侧而不是 Web 侧的理由：推迟多久是产品决策，而 Web 桥接层拿到的是
         * 只有 {occKey, action, atMs} 三项的 DoseAction，没有任何字段能承载业务时长，
         * 让桥接层自己编一个常量会造成「同一决策散落在两端」。
         * 契约 1.2 没给 snoozeUntil 字段，与其改契约，不如由原生在自己重排闹钟时消化这个常量。
         */
        const val SNOOZE_MS = 10 * 60 * 1000L

        /* --------------------------------------------- Intent / 通知 协议 */

        /** 闹钟到点，需要弹通知。 */
        const val ACTION_FIRE = "fun.qiubite.meddesk.alarm.FIRE"
        /** 用户点击通知本体或某个动作按钮。 */
        const val ACTION_DOSE_ACTION = "fun.qiubite.meddesk.alarm.DOSE_ACTION"

        const val EXTRA_OCC_KEY = "occKey"
        const val EXTRA_TITLE = "title"
        const val EXTRA_BODY = "body"
        const val EXTRA_AT_MS = "atMs"
        const val EXTRA_ACTION = "action"
        const val EXTRA_NOTIFY_ID = "notifyId"

        const val ACTION_TAKEN = "taken"
        const val ACTION_SKIPPED = "skipped"
        const val ACTION_SNOOZED = "snoozed"
        const val ACTION_OPENED = "opened"

        const val CHANNEL_ID = "meddesk_dose"
        const val CHANNEL_NAME = "服药提醒"

        /**
         * Uri scheme，只为让不同的 Intent 在 filterEquals 意义上互相区分。
         *
         * 为什么需要它：AlarmManager.cancel() 依赖 PendingIntent 相等判定，
         * 而 Intent 相等只看 action/data/type/class/categories/identity，
         * **不看 extras**。所以同一个 AlarmReceiver 上挂 N 条闹钟时，
         * 若只靠 extras 区分，这 N 个 Intent 在系统眼里是完全相同的，
         * 后注册的会顶掉先注册的。data 与 requestCode 双保险可以杜绝这个问题。
         *
         * 为什么用 appendPath 而不是字符串拼接：
         * occKey 里可能带 '/'、'#' 等字符，拼接会破坏 URI 结构。
         * 注意：这里 **只依赖它的唯一性**，接收端一律从 extras 读 occKey，
         * 绝不用 lastPathSegment 反推，规避一切编解码边界问题。
         */
        fun dataUriFor(occKey: String): Uri =
            Uri.Builder().scheme("meddesk").authority("dose").appendPath(occKey).build()

        /** 通知按钮专用：路径再带一段 action，使同一 occKey 的四个 Intent 互相可区分。 */
        fun actionDataUriFor(occKey: String, action: String): Uri =
            Uri.Builder().scheme("meddesk").authority("dose")
                .appendPath(occKey).appendPath(action).build()

        @Volatile
        private var instance: DoseStore? = null

        /** 进程级单例：prefs 本身线程安全，这里缓存只是为了省掉重复读取 XML 的开销。 */
        fun get(context: Context): DoseStore {
            val cached = instance
            if (cached != null) return cached
            synchronized(this) {
                val again = instance
                if (again != null) return again
                val prefs = context.applicationContext
                    .getSharedPreferences(PREFS_NAME, Context.MODE_PRIVATE)
                val created = DoseStore(prefs)
                instance = created
                return created
            }
        }

        /** 构造「闹钟到点」用的 Intent。务必与 [actionIntentFor] 使用同一套 action + data。 */
        fun fireIntentFor(context: Context, occKey: String, atMs: Long, title: String, body: String): Intent =
            Intent(context, AlarmReceiver::class.java)
                .setAction(ACTION_FIRE)
                .setData(dataUriFor(occKey))
                .putExtra(EXTRA_OCC_KEY, occKey)
                .putExtra(EXTRA_AT_MS, atMs)
                .putExtra(EXTRA_TITLE, title)
                .putExtra(EXTRA_BODY, body)

        /**
         * 构造「用户点了通知」用的 Intent。
         *
         * data 在 [fireIntentFor] 的基础上再追加一段 action 路径：
         * 「已服用」「跳过」「稍后」以及点击通知本体（记为 opened）四个 PendingIntent
         * 复用同一个 requestCode（= notifyId），靠这一段路径在 filterEquals 意义上互相区分。
         * 这样就不必为四个 Intent 各申请一个 requestCode，也不会互相顶掉。
         *
         * 顺带把 title/body 一起带上：点「稍后」时原生要按原文案重排闹钟并再次弹通知，
         * 而那个时刻既有的一条通知马上就要关掉，没有其他途径再取回文案。
         * extras 不影响 PendingIntent 相等判定，加字段是安全的。
         */
        fun actionIntentFor(
            context: Context,
            occKey: String,
            notifyId: Int,
            action: String,
            title: String,
            body: String
        ): Intent =
            Intent(context, AlarmReceiver::class.java)
                .setAction(ACTION_DOSE_ACTION)
                .setData(actionDataUriFor(occKey, action))
                .putExtra(EXTRA_OCC_KEY, occKey)
                .putExtra(EXTRA_NOTIFY_ID, notifyId)
                .putExtra(EXTRA_ACTION, action)
                .putExtra(EXTRA_TITLE, title)
                .putExtra(EXTRA_BODY, body)

        /**
         * 把 PendingIntent 交给 AlarmManager，并按需降级。
         *
         * RTC_WAKEUP 而非 RTC：只有 WAKEUP 才会在息屏休眠时唤醒 CPU 来跑 Receiver。
         *
         * 降级链路（契约 1.4-4）：setExactAndAllowWhileIdle -> setWindow。
         * Android 12 起 SCHEDULE_EXACT_ALARM 默认授予但用户可以关掉，
         * 一旦关掉继续调 setExactAndAllowWhileIdle 会抛 SecurityException
         * （而不是安静地不响），所以每次都要现场查询。
         *
         * 为什么放在 DoseStore 而不是插件类里：
         * 「Web 调 schedule()」与「用户点稍后后 Receiver 自己重排」两条入口
         * 必须共用同一套注册策略，否则同一个 occKey 会出现两种精度的闹钟。
         * 我的授权文件只有三个，不新增第四个，因此把这点共享逻辑收在这里。
         */
        fun registerAlarm(context: Context, pending: PendingIntent, atMs: Long) {
            val am = context.getSystemService(Context.ALARM_SERVICE) as? AlarmManager ?: return
            val exactAllowed = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
                am.canScheduleExactAlarms()
            } else {
                true
            }
            if (exactAllowed) {
                // 注：Doze 模式下精确闹钟有相邻最小间隔的隐性节流，属平台限制，无法绕过。
                am.setExactAndAllowWhileIdle(AlarmManager.RTC_WAKEUP, atMs, pending)
            } else {
                // 窗口起点必须落在未来（否则立即触发，等于变相补弹过期提醒），
                // 由调用方保证 atMs > now。
                am.setWindow(AlarmManager.RTC_WAKEUP, atMs, WINDOW_MS, pending)
            }
        }

        /** 把 JSONObject 里可能缺失/为 null 的字段转成 String，缺失返回 null（交由调用方判空）。 */
        fun optTrimmedString(obj: JSONObject, key: String): String? {
            if (!obj.has(key) || obj.isNull(key)) return null
            val v = obj.optString(key, "")
            return if (v.isEmpty()) null else v
        }
    }
}
