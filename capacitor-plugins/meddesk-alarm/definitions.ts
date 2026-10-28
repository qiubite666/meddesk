/*
 * MedDesk · MeddeskAlarm 插件的 Bridge 类型契约
 * 对应文档：docs/CONTRACTS-ANDROID.md 第 1 节（Contract A）
 * 对应实现：android/app/src/main/java/fun/qiubite/meddesk/alarm/*.kt
 *
 * 【本文件的定位】
 * 这是纯类型声明，**不含运行时代码、不 import 任何包**：
 * Web 侧（Contract B 的 MD.nativeAlarm）只允许通过 window.Capacitor 访问插件，
 * 不做构建步骤，因此这里不调用 registerPlugin()，也不引用 @capacitor/core 的类型。
 *
 * 【字段对齐约定】
 * 下列每个 interface 的字段名、类型都必须与 Kotlin 侧序列化到 JSObject 的键逐一对齐：
 *   DoseAlarm    <- MeddeskAlarmPlugin.schedule() 的入参，由 Web 侧构造
 *   DoseAction   <- AlarmReceiver 写入 SharedPreferences，drainActions() 输出
 *   AlarmStatus  <- MeddeskAlarmPlugin.status() 输出
 * 修改任一侧都必须同步改另一侧，否则会在运行时静默拿到 undefined。
 */

/** 一条待提醒的服药剂量。 */
export interface DoseAlarm {
  /** 全局唯一键，来自 MD.sched.buildToday() 的 occKey 字段。必填且非空。 */
  occKey: string;
  /** Unix 毫秒时间戳（绝对时间）。必须大于 0；小于等于当前时间的会被丢弃并计入 failed。 */
  atMs: number;
  /** 通知标题，例：「布洛芬 该服药了」。必填且非空。 */
  title: string;
  /** 通知正文，例：「1 片 · 08:00」。必填且非空。 */
  body: string;
}

/** 用户在通知上的操作结果（离线暂存，供 Web 侧拉取）。 */
export interface DoseAction {
  /** 与 DoseAlarm.occKey 同源，用于把操作对应回具体剂量。 */
  occKey: string;
  /** taken/skipped 来自通知按钮；opened 来自点击通知本体；snoozed 来自「稍后」按钮。
   *  注：opened 表示用户只是点开了通知、未在三个按钮中做选择，不含任何决策，
   *  Web 侧写入服药记录时应跳过它（原生侧从不因 opened 修改任何闹钟）。 */
  action: 'taken' | 'skipped' | 'snoozed' | 'opened';
  /** 操作发生时刻，Unix 毫秒。由原生侧生成，不受 Web 侧时钟影响。 */
  atMs: number;
}

/** 权限与系统状态。status() 只查询，不做任何授权请求。 */
export interface AlarmStatus {
  /** 通知是否已允许（Android 13 起对应 POST_NOTIFICATIONS；低版本由系统 AppOps 判定）。 */
  notifications: boolean;
  /** 是否可以注册精确闹钟 SCHEDULE_EXACT_ALARM。为 false 时原生侧降级为 setWindow。 */
  exactAlarm: boolean;
  /** 是否已加入电池优化白名单。国产 ROM 上后台闹钟能否按时响的关键前提。 */
  ignoringBattery: boolean;
}

/** MeddeskAlarm 插件对外暴露的方法集合。 */
export interface MeddeskAlarmPlugin {
  /**
   * 批量注册闹钟。幂等：同一 occKey 覆盖旧的触发时间与文案。
   * failed 里放的是**注册失败的 occKey 字符串**（字段缺失、atMs 非法、已过期、
   * 或系统注册时抛异常）；无法取出 occKey 时用序号占位，形如 '#0'。
   */
  schedule(opts: { doses: DoseAlarm[] }): Promise<{ scheduled: number; failed: string[] }>;

  /**
   * 取消全部已注册闹钟。不会清空已暂存的 DoseAction。
   * 「稍后」由原生侧重排的那条闹钟也记在同一个台账里，会被一并取消。
   */
  cancelAll(): Promise<{ cancelled: number }>;

  /** 拉取并清空离线期间暂存的操作（取走即清空）。 */
  drainActions(): Promise<{ actions: DoseAction[] }>;

  /** 查询权限与系统状态，不做任何授权请求动作。 */
  status(): Promise<AlarmStatus>;

  /** 跳转到本 App 的系统通知设置页。 */
  openNotificationSettings(): Promise<void>;

  /** 跳转到电池优化豁免设置页。 */
  openBatterySettings(): Promise<void>;
}
