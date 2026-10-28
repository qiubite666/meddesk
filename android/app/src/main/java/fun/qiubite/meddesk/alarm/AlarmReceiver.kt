package fun.qiubite.meddesk.alarm

import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.os.Build
import androidx.core.app.NotificationCompat
import androidx.core.app.NotificationManagerCompat

/**
 * 服药提醒的广播接收器。
 *
 * 【为什么只用这一个 Receiver 承担两类 Intent】
 * 少一个类就少一个 Manifest 条目，也少一处 exported 需要审查的面。
 * 两类 Intent 用不同的 action + data 区分，互不干扰。
 *
 * 【为什么不用 exported=true】
 * 本 Receiver 只接受本进程自己通过 PendingIntent 发出的广播；
 * 对外暴露会让任意第三方 App 伪造 occKey 写脏操作记录。
 * 因此 Manifest 里必须写成 android:exported="false"。
 *
 * 【生命周期】
 * onReceive 跑在 Binder 线程，不入 UI 主线程。这里的工作是「写 prefs + 弹通知」，
 * 都在毫秒级且全同步，不需要 goAsync()。
 */
class AlarmReceiver : BroadcastReceiver() {

    override fun onReceive(context: Context, intent: Intent) {
        // Receiver 里抛异常会导致进程崩溃（RuntimeInit logs "Unable to start receiver"），
        // 而闹钟场景往往无人值守，绝不能因为一条脏数据就让 App 崩掉。
        try {
            // 必须用 applicationContext：Receiver 的 Context 生命周期到此方法返回为止。
            val appCtx = context.applicationContext
            when (intent.action) {
                DoseStore.ACTION_FIRE -> onFire(appCtx, intent)
                DoseStore.ACTION_DOSE_ACTION -> onDoseAction(appCtx, intent)
                else -> return
            }
        } catch (t: Throwable) {
            // 无用户界面的后台路径，吞掉异常是最安全的选择：写不了就下次再说。
        }
    }

    /* ------------------------------------------------------------ 闹钟到点 */

    private fun onFire(context: Context, intent: Intent) {
        val occKey = intent.getStringExtra(DoseStore.EXTRA_OCC_KEY)
        if (occKey.isNullOrEmpty()) return

        val store = DoseStore.get(context)

        // notifyId 必须全局唯一且在本次通知存活期内稳定：
        // 通知取消、以及四个 PendingIntent 的 requestCode 都依赖它。
        // 正常路径下直接用台账里已分配好的 requestCode（按 occKey 单调递增且持久化）；
        // 只有在台账被清空的异常情况下才退化为哈希值 —— 宁可承担极小的碰撞概率
        // （最坏是取消错另一条通知），也好过整条服药提醒都不弹。
        val notifyId = store.requestCodeOf(occKey)
            ?: (occKey.hashCode() and Int.MAX_VALUE).let { if (it == 0) 1 else it }

        // 闹钟已经消费掉了：从台账移除，保证 cancelAll() 返回的 cancelled 是真实数量。
        // 放在写动作记录之前，避免出现「闹钟不在了但计数还在」的偏差。
        store.removeScheduled(occKey)

        val title = intent.getStringExtra(DoseStore.EXTRA_TITLE) ?: DEFAULT_TITLE
        val body = intent.getStringExtra(DoseStore.EXTRA_BODY) ?: ""

        val nm = NotificationManagerCompat.from(context)
        // Android 13（API 33）起没有 POST_NOTIFICATIONS 权限，通知会被系统静默丢弃。
        // 这里显式早退：与其发一条永远看不到的通知，不如什么都不做，
        // 由 status() 把权限状态暴露给 Web 侧去引导用户授权。
        if (!nm.areNotificationsEnabled()) return

        ensureChannel(context)

        // 通知图标：本插件不允许新增 res/ 资源，因此复用 App 自身的应用图标。
        // applicationInfo.icon 由 Merge Manifest 的 <application android:icon> 决定，必然存在。
        // 极端情况下取不到（0），构造出的通知会让 SystemUI 崩溃，宁可放弃这条提醒。
        val smallIcon = context.applicationInfo.icon
        if (smallIcon == 0) return

        val builder = NotificationCompat.Builder(context, DoseStore.CHANNEL_ID)
            .setSmallIcon(smallIcon)
            .setContentTitle(title)
            .setContentText(body)
            .setStyle(NotificationCompat.BigTextStyle().bigText(body))
            // priority 在 Android 8 起会被渠道的 importance 覆盖，这里保留是为了照顾 API 26 以下。
            .setPriority(NotificationCompat.PRIORITY_MAX)
            .setCategory(NotificationCompat.CATEGORY_REMINDER)
            // VISIBILITY_PRIVATE：锁屏上隐藏正文，只留标题。
            // 药品名属于健康隐私，锁屏场景下不应该裸奔。
            // 想在锁屏/勿扰下完整露出内容需要用户手动加白名单，本插件不做任何伪装绕过。
            .setVisibility(NotificationCompat.VISIBILITY_PRIVATE)
            .setAutoCancel(true)
            // 点击通知本体记为 opened；三个按钮依次记为 taken / skipped / snoozed。
            // addAction 一律传合法的 drawable（这里复用 App 图标）：传 0 这类无效资源
            // 在系统折叠通知时有触发 RemoteServiceException 的历史风险。
            .setContentIntent(actionPendingIntent(context, occKey, notifyId, DoseStore.ACTION_OPENED, title, body))
            .addAction(smallIcon, LABEL_TAKEN, actionPendingIntent(context, occKey, notifyId, DoseStore.ACTION_TAKEN, title, body))
            .addAction(smallIcon, LABEL_SKIPPED, actionPendingIntent(context, occKey, notifyId, DoseStore.ACTION_SKIPPED, title, body))
            .addAction(smallIcon, LABEL_SNOOZED, actionPendingIntent(context, occKey, notifyId, DoseStore.ACTION_SNOOZED, title, body))

        nm.notify(occKey, notifyId, builder.build())
    }

    /* ------------------------------------------------------ 用户点击通知 */

    private fun onDoseAction(context: Context, intent: Intent) {
        val occKey = intent.getStringExtra(DoseStore.EXTRA_OCC_KEY)
        if (occKey.isNullOrEmpty()) return
        val action = intent.getStringExtra(DoseStore.EXTRA_ACTION) ?: DoseStore.ACTION_OPENED
        val notifyId = intent.getIntExtra(DoseStore.EXTRA_NOTIFY_ID, 0)

        // 先落盘再关通知：就算后面 UI 操作失败，用户的操作也不会丢。
        DoseStore.get(context).appendAction(occKey, action, System.currentTimeMillis())

        if (notifyId != 0) {
            NotificationManagerCompat.from(context).cancel(occKey, notifyId)
        }

        // 复用 notifyId 作为 requestCode 重排闹钟：它就是当初分配给这个 occKey 的那个号码，
        // 复用后 PendingIntent 与原闹钟完全同身份，cancelAll() 依旧能精确取消它。
        if (action == DoseStore.ACTION_SNOOZED && notifyId != 0) {
            val title = intent.getStringExtra(DoseStore.EXTRA_TITLE) ?: DEFAULT_TITLE
            val body = intent.getStringExtra(DoseStore.EXTRA_BODY) ?: ""
            rescheduleSnooze(context, occKey, notifyId, title, body)
        }

        if (action == DoseStore.ACTION_OPENED) {
            launchApp(context)
        }
    }

    /**
     * 「稍后」的重排。
     *
     * 契约 1.4-3 只要求写 DoseAction 并关通知，没有定义是否再提醒；
     * 但「稍后」在服药场景里的语义就是「过会儿再叫我」，只关不排等于让用户永久漏服。
     * 因此原生侧自己补这一步：记录照写（Web 侧仍会拿到 snoozed），闹钟延迟 SNOOZE_MS 后再响一次。
     *
     * 为什么是原生做而不是 Web 做：DoseAction 只有 {occKey, action, atMs}，
     * 没有能承载推迟时长（或推迟目标时刻）的字段，Web 侧拿不到依据，
     * 硬编码到桥接层会把同一个产品常量拆散在两个地方。这里写死 SNOOZE_MS 并集中在一处，
     * 将来要改成可配置（例如加到 DoseAlarm 或新增插件方法），只动这一处即可。
     */
    private fun rescheduleSnooze(
        context: Context,
        occKey: String,
        requestCode: Int,
        title: String,
        body: String
    ) {
        val atMs = System.currentTimeMillis() + DoseStore.SNOOZE_MS
        DoseStore.get(context).putScheduled(occKey, atMs, title, body, requestCode)
        val intent = DoseStore.fireIntentFor(context, occKey, atMs, title, body)
        val flags = PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE
        val pending = PendingIntent.getBroadcast(context, requestCode, intent, flags)
        DoseStore.registerAlarm(context, pending, atMs)
    }

    /**
     * 把 App 拉回前台。
     *
     * Android 10（API 29）起后台启动 Activity 受限。这里仍在 try/catch 中调用：
     * 由用户点击通知触发的路径属于「系统通过 PendingIntent 唤起的场景」，
     * 是受豁免的一类；即便个别 ROM 收紧策略导致 startActivity 抛 SecurityException，
     * 也已经有 DoseAction 落库，用户下次打开 App 时由 drainActions() 补偿，不会丢数据。
     */
    private fun launchApp(context: Context) {
        try {
            val launch = context.packageManager.getLaunchIntentForPackage(context.packageName)
                ?: return
            launch.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_SINGLE_TOP)
            context.startActivity(launch)
        } catch (t: Throwable) {
            // 忽略：前台拉不起 App 不影响数据完整性
        }
    }

    /* ------------------------------------------------------------ 基础设施 */

    /**
     * 通知渠道。
     *
     * IMPORTANCE_HIGH 而非 MAX：
     * MAX 会强行弹出悬浮抬头通知（heads-up），对服药提醒过于打扰；
     * HIGH 已经是「出声 + 横幅」，足够用。
     *
     * createNotificationChannel 本身是幂等的：重复调用只会被系统忽略，
     * 但 channelId 一旦创建就无法再改 importance/声音，因此这里的取值不要轻易变动。
     */
    private fun ensureChannel(context: Context) {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.O) return
        val mgr = context.getSystemService(Context.NOTIFICATION_SERVICE) as? NotificationManager
            ?: return
        if (mgr.getNotificationChannel(DoseStore.CHANNEL_ID) != null) return
        val channel = NotificationChannel(
            DoseStore.CHANNEL_ID,
            DoseStore.CHANNEL_NAME,
            NotificationManager.IMPORTANCE_HIGH
        ).apply {
            description = "按时服药提醒"
            enableVibration(true)
        }
        mgr.createNotificationChannel(channel)
    }

    /**
     * 构造通知按钮的 PendingIntent。
     *
     * FLAG_IMMUTABLE：targetSdk 34 强制要求显式声明可变性。
     * 我们不希望接收方（系统通知栏）往 Intent 里塞任何东西，因此锁死为不可变。
     *
     * FLAG_UPDATE_CURRENT：同一 occKey 的同一 action 复用同一个 requestCode，
     * 但 title/body 可能已经变化（重新 schedule 过），需要让 extras 刷新。
     */
    private fun actionPendingIntent(
        context: Context,
        occKey: String,
        notifyId: Int,
        action: String,
        title: String,
        body: String
    ): PendingIntent {
        val intent = DoseStore.actionIntentFor(context, occKey, notifyId, action, title, body)
        val flags = PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE
        return PendingIntent.getBroadcast(context, notifyId, intent, flags)
    }

    companion object {
        private const val DEFAULT_TITLE = "用药提醒"
        private const val LABEL_TAKEN = "已服用"
        private const val LABEL_SKIPPED = "跳过"
        private const val LABEL_SNOOZED = "稍后"
    }
}
