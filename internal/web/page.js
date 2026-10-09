(function () {
  "use strict";

  var API = "/v0/management/lamplighter";
  var CONFIG_API = "/v0/management/plugins/lamplighter/config";
  var KEY_STORE = "lamplighter-management-key";
  var PROVIDERS = ["codex", "claude", "antigravity"];

  var key = "";
  var status = null;
  var rawConfig = {};
  var chartWindow = "5h";
  var range = "6h";
  var chartSort = "default";
  var chartExpanded = {};
  var chartSelected = null;
  var history = null;
  var refreshTimer = null;

  function $(id) { return document.getElementById(id); }

  function el(tag, attrs, children) {
    var node = document.createElement(tag);
    if (attrs) {
      Object.keys(attrs).forEach(function (name) {
        var value = attrs[name];
        if (value === undefined || value === null || value === false) return;
        if (name === "text") node.textContent = value;
        else if (name === "class") node.className = value;
        else if (name === "style") node.setAttribute("style", value);
        else if (name.indexOf("on") === 0) node.addEventListener(name.slice(2), value);
        else node.setAttribute(name, value === true ? "" : value);
      });
    }
    (children || []).forEach(function (child) {
      if (child === null || child === undefined) return;
      node.appendChild(typeof child === "string" ? document.createTextNode(child) : child);
    });
    return node;
  }

  function clear(node) { while (node.firstChild) node.removeChild(node.firstChild); }

  // The page runs in a same-origin iframe of the Management Center, so it
  // reads the theme from the parent document: data-theme is "dark", "white"
  // or absent for the default light theme. Opened on its own, it falls back
  // to the theme the Management Center stored in localStorage.
  function parentRoot() {
    try {
      if (window.parent !== window) return window.parent.document.documentElement;
    } catch (e) { /* cross-origin */ }
    return null;
  }

  function applyTheme() {
    var theme = "light";
    var root = parentRoot();
    if (root) {
      var value = root.getAttribute("data-theme");
      if (value === "dark" || value === "white") theme = value;
    } else {
      var stored = "auto";
      try {
        var parsed = JSON.parse(localStorage.getItem("cli-proxy-theme") || "null");
        stored = (parsed && parsed.state && parsed.state.theme) || (parsed && parsed.theme) || stored;
      } catch (e) { /* use auto */ }
      if (stored === "dark" || stored === "white") theme = stored;
      if (stored === "auto" && window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches) theme = "dark";
    }
    if (theme === "light") document.documentElement.removeAttribute("data-theme");
    else document.documentElement.setAttribute("data-theme", theme);
  }

  function watchParent() {
    var root = parentRoot();
    if (!root || !window.MutationObserver) return;
    new MutationObserver(function () { applyTheme(); applyLanguage(); renderChart(); })
      .observe(root, { attributes: true, attributeFilter: ["data-theme", "lang"] });
  }

  // ---- Language ----

  // Every visible text, per language. Keys are used by data-i18n attributes
  // in page.html and by translate() below; {0}, {1} are arguments. A new language
  // adds a dictionary here and a column in internal/notify/text.go.
  var I18N = {
    zh: {
      app_sub: "点灯人", repo_hint: "在 GitHub 上查看",
      key_title: "需要管理密钥",
      key_hint: "在管理中心登录时勾选“记住密码”，此页面会直接使用该密钥；否则请在这里输入，密钥只保存在当前标签页。",
      key_placeholder: "CPA 管理密钥",
      key_submit: "确定",
      key_invalid: "管理密钥无效，请重新输入。",
      refresh_all: "立即刷新全部",
      quota: "额度",
      ignition: "自动点火",
      col_group: "额度组", col_next: "下次点火", col_reset: "当前重置", col_last: "最近成功", col_model: "模型", col_status: "状态",
      chart_title: "额度变化", chart_window: "额度窗口", chart_range: "时间范围", chart_sort: "排序",
      quota_5h: "5 小时额度", quota_7d: "7 天额度",
      sort_default: "默认排序", sort_low: "最低优先",
      no_data: "无数据",
      legend_ignited: "● 点火成功，×n 为同一时刻的多个目标",
      legend_failed: "✕ 含点火失败",
      legend_reset: "↻ 重置",
      legend_pooled: "多账号的服务显示合计额度，点击行查看详情，再点一次展开各账号",
      data_table: "数据表",
      col_quota: "额度", col_account: "账号", col_latest: "最新", col_min: "最低", col_max: "最高", col_samples: "样本", col_to_reset: "距重置",
      events: "事件",
      settings: "设置",
      notifications: "通知",
      bark_url: "Bark 推送地址", bark_group: "Bark 分组",
      test_bark: "测试推送", test_bark_hint: "按插件已应用的设置发送，修改地址后等几秒再测试",
      thresholds: "提醒阈值", remaining_le: "剩余 ≤",
      thresholds_hint: "额度降到每一档时推送一次，耗尽时再推送一次",
      window_5h: "5 小时窗口", window_7d: "7 天窗口",
      recovery_notify: "额度恢复时通知", reset_reminder: "重置前提醒",
      recovery_notify_5h: "额度恢复时通知 · 5 小时窗口", recovery_notify_7d: "额度恢复时通知 · 7 天窗口",
      reset_reminder_5h: "重置前提醒 · 5 小时窗口", reset_reminder_7d: "重置前提醒 · 7 天窗口",
      mode_off: "关闭", recovery_all: "每次", recovery_after_exhausted: "用完后",
      reminder_all: "每次", reminder_has_remaining: "有余量",
      mode_off_title: "关闭", recovery_all_title: "每次重置都通知", recovery_after_exhausted_title: "窗口用完后，在下一次重置时通知一次",
      reminder_all_title: "每次重置前都提醒", reminder_has_remaining_title: "剩余高于第三档阈值时才提醒",
      notify_modes_hint: "“用完后”：窗口用完后，在下一次重置时通知一次。“有余量”：剩余高于第三档阈值时才提醒。重置前提醒在 5 小时窗口重置前 1 小时、7 天窗口重置前 1 天发送。恢复通知附带上一周期的剩余额度，取自每次重置前 30 秒的查询。",
      quota_queries: "额度查询",
      models_api_key: "模型列表 API key",
      models_api_key_hint: "在 CPA 的 access.api-keys 中新建一个专用 key，用于读取模型列表、选择点火模型",
      poll_interval: "查询间隔（秒）", request_timeout: "请求超时（秒）", history_retention: "历史保留（天）",
      passive_skip: "被动数据跳过窗口（秒）", passive_skip_max: "最多连续跳过（分钟）",
      passive_skip_hint: "整点前这段时间内已有被动数据的账号跳过本轮主动查询；设为 0 则每轮都主动查询。",
      ignition_enabled: "启用自动点火",
      start_hour: "每日开始（时）", end_hour: "每日结束（时）", end_grace: "结束后宽限（分钟）",
      grace: "重置后延迟（秒）", failure_retry: "失败重试间隔（秒）", max_transient: "临时错误上限（次）",
      services: "服务", col_service: "服务", col_monitor: "监控", col_ignition_model: "点火模型",
      monitor_codex: "监控 ChatGPT", ignite_codex: "点火 ChatGPT",
      monitor_claude: "监控 Claude", ignite_claude: "点火 Claude",
      monitor_antigravity: "监控 Antigravity", ignite_antigravity: "点火 Antigravity",
      model_auto_codex: "自动：最新的 Luna", model_auto_claude: "自动：最新的 Haiku", model_auto_antigravity: "自动：最新的 Flash",
      ag_groups: "Antigravity 点火额度组",
      ag_groups_hint: "Antigravity 按模型分成多个独立的 5 小时额度组。只有勾选的额度组会自动点火；Claude / GPT 额度组较小，默认不点。",
      codex_reset_enabled: "转发 Did Codex Reset 发布的 Codex 重置信号",
      codex_reset_pending: "首次开启时推送当前待生效的排期",
      save_hint: "修改后自动保存到 CPA 的 config.yaml，插件几秒内应用。文本和数字在离开输入框或按回车时保存。",
      language_note: "通知语言跟随最近打开此页面时管理中心的语言，当前为{0}。",
      lang_zh: "中文", lang_en: "English",
      due: "已到", minutes: "{0} 分钟", hours_minutes: "{0} 小时 {1} 分", days: "{0} 天",
      soon: "即将", in_time: "{0}后", just_now: "刚刚", ago: "{0}前",
      reset_now: "已重置",
      window_5h: "5 小时", window_7d: "7 天",
      state_disabled: "已停用", state_starting: "启动中", state_running: "运行中",
      last_poll: "上次查询 {0}（{1}）", next_poll: "下次 {0}（{1}）",
      config_error: "配置无法解析，仍在使用上一份配置：{0}",
      models_error: "模型列表不可用：{0}",
      no_bark: "未设置 Bark 推送地址，通知不会发送。",
      no_models_key: "未设置模型列表 API key，自动点火无法选择模型。",
      remaining_aria: "{0} {1} 剩余",
      source_passive: "被动", source_active: "主动", updated: "更新于 {0}",
      no_accounts: "没有可监控的 ChatGPT、Claude 或 Antigravity 凭证。",
      first_query: "CPA 启动约 20 秒后开始第一次查询。",
      refresh: "刷新", refreshed: "已刷新 {0}",
      cooldown_stale: "额度已恢复，但 CPA 仍冷却到 {0}，期间的请求和点火都会被拒绝；在 CPA 管理中心清除这个账号的冷却。",
      cooldown: "CPA 冷却到 {0}（{1}）",
      blocked_5h: "5 小时额度已用完", blocked_7d: "7 天额度已用完", blocked_other: "{0}额度已用完",
      blocked_until: "{0} 重置后恢复点火", blocked_wait: "额度恢复后恢复点火",
      paused_until: "暂停至 {0}", failures: "失败 {0} 次", rolling: "窗口未开始", normal: "正常",
      ignition_note: "每天 {0}:00 开始，之后在每次重置后 {1} 秒点火，最晚到 {2}。",
      ignition_off: "自动点火已关闭。",
      no_targets: "没有启用点火的额度组。",
      ignite_now: "立即点火",
      ignite_confirm: "立即向 {0} 发送一次点火请求？这会开始一个新的 5 小时窗口。",
      ignited: "{0} 点火成功",
      ev_ignite: "点火成功", ev_ignite_manual: "手动点火", ev_ignite_failed: "点火失败", ev_ignite_paused: "点火暂停", ev_model_refused: "模型被拒",
      ev_cooldown_stale: "冷却未解除", ev_notify: "已推送", ev_notify_failed: "推送失败", ev_codex_reset: "重置信号", ev_error: "错误",
      ev_ignited: "{0} · 下次重置 {1}",
      ev_retry: "{0}后重试：{1}", ev_retry_cooldown: "CPA 冷却结束，{0}后重试：{1}",
      ev_seconds: "{0} 秒", ev_minutes: "{0} 分钟",
      ev_paused: "暂停到 {0}：{1}",
      ev_refused: "{0} 被上游拒绝，改用 {1}，{2} 小时内不再尝试",
      ev_notify_failed_detail: "{0}：{1}",
      ev_cooldown: "额度已恢复，CPA 仍冷却到 {0}",
      ev_codex_sent: "已发送 {0} 条通知",
      no_events: "暂无事件", collapse: "收起", show_more: "显示更多（共 {0} 条）",
      failed: "失败：{0}",
      range_h: "{0} 小时", range_d: "{0} 天", range_mo: "{0} 个月",
      overview_aria: "各额度剩余百分比总览",
      ignition_lane: "点火", lane_ok: "{0} 成功", lane_failed: " · {0} 失败",
      total: "合计", total_suffix: " 合计", more_accounts: "　… 另 {0} 个账号",
      pooled_note: "{0} 个账号合计，阴影为账号间的最低到最高",
      stat_total: "合计", stat_current: "当前", stat_min_account: "单账号最低", stat_min: "最低", stat_to_reset: "距重置",
      detail_aria: "{0} 剩余百分比",
      detail_keys: "按左右方向键查看各时刻的数值",
      passive_sample: "（被动）", active_sample: "（主动）",
      quota_reset: "额度重置",
      loading: "加载中…", no_history: "这个范围内还没有额度数据。",
      history_failed: "读取历史失败：{0}",
      tz_config: "配置中指定", tz_cpa: "跟随 CPA 服务器",
      tz_note: "通知和页面中的时间使用 {0}，{1}。",
      saved: "已保存，插件会在几秒内应用新设置", save_failed: "保存失败：{0}",
      load_failed: "加载失败：{0}",
      refreshed_all: "已刷新全部额度", test_sent: "测试通知已发送"
    },
    en: {
      app_sub: "", repo_hint: "View on GitHub",
      key_title: "Management key required",
      key_hint: "If you checked \"remember password\" when signing in to the Management Center, this page uses that key. Otherwise enter it here; it is kept in this tab only.",
      key_placeholder: "CPA management key",
      key_submit: "OK",
      key_invalid: "The management key is not valid. Enter it again.",
      refresh_all: "Refresh all",
      quota: "Quota",
      ignition: "Ignition",
      col_group: "Quota group", col_next: "Next ignition", col_reset: "Current reset", col_last: "Last success", col_model: "Model", col_status: "Status",
      chart_title: "Quota history", chart_window: "Quota window", chart_range: "Time range", chart_sort: "Order",
      quota_5h: "5-hour quota", quota_7d: "7-day quota",
      sort_default: "Default order", sort_low: "Lowest first",
      no_data: "No data",
      legend_ignited: "● ignition succeeded; ×n marks several targets at the same time",
      legend_failed: "✕ an ignition failed",
      legend_reset: "↻ reset",
      legend_pooled: "Services with several accounts show their total; click a row for details, click again to list the accounts",
      data_table: "Data table",
      col_quota: "Quota", col_account: "Account", col_latest: "Latest", col_min: "Lowest", col_max: "Highest", col_samples: "Samples", col_to_reset: "Until reset",
      events: "Events",
      settings: "Settings",
      notifications: "Notifications",
      bark_url: "Bark push URL", bark_group: "Bark group",
      test_bark: "Send test", test_bark_hint: "Uses the settings the plugin has applied; after changing the URL, wait a few seconds",
      thresholds: "Alert thresholds", remaining_le: "Remaining ≤",
      thresholds_hint: "One notification when quota drops to each threshold, and one more when it runs out",
      window_5h: "5-hour window", window_7d: "7-day window",
      recovery_notify: "Notify when quota recovers", reset_reminder: "Remind before resets",
      recovery_notify_5h: "Notify when quota recovers · 5-hour window", recovery_notify_7d: "Notify when quota recovers · 7-day window",
      reset_reminder_5h: "Remind before resets · 5-hour window", reset_reminder_7d: "Remind before resets · 7-day window",
      mode_off: "Off", recovery_all: "Always", recovery_after_exhausted: "After out",
      reminder_all: "Always", reminder_has_remaining: "If left",
      mode_off_title: "Off", recovery_all_title: "Notify at every reset", recovery_after_exhausted_title: "After the window runs out, notify once at its next reset",
      reminder_all_title: "Remind before every reset", reminder_has_remaining_title: "Remind only when more than the third threshold is left",
      notify_modes_hint: "\"After out\": after the window runs out, notify once at its next reset. \"If left\": remind only when more than the third threshold is left. Reminders are sent 1 hour before a 5-hour window resets and 1 day before a 7-day window resets. Recovery notifications include what was left of the ended cycle, from the query 30 seconds before every reset.",
      quota_queries: "Quota queries",
      models_api_key: "Model list API key",
      models_api_key_hint: "Create a dedicated key in CPA's access.api-keys; the plugin reads the model list with it to pick ignition models",
      poll_interval: "Query interval (s)", request_timeout: "Request timeout (s)", history_retention: "History retention (days)",
      passive_skip: "Passive data window (s)", passive_skip_max: "Longest skip (min)",
      passive_skip_hint: "Accounts with passive data within this time before a slot skip that active query; 0 queries every time.",
      ignition_enabled: "Enable ignition",
      start_hour: "Daily start (hour)", end_hour: "Daily end (hour)", end_grace: "Grace after end (min)",
      grace: "Delay after reset (s)", failure_retry: "Retry after failure (s)", max_transient: "Transient error limit",
      services: "Services", col_service: "Service", col_monitor: "Monitor", col_ignition_model: "Ignition model",
      monitor_codex: "Monitor ChatGPT", ignite_codex: "Ignite ChatGPT",
      monitor_claude: "Monitor Claude", ignite_claude: "Ignite Claude",
      monitor_antigravity: "Monitor Antigravity", ignite_antigravity: "Ignite Antigravity",
      model_auto_codex: "Auto: latest Luna", model_auto_claude: "Auto: latest Haiku", model_auto_antigravity: "Auto: latest Flash",
      ag_groups: "Antigravity ignition groups",
      ag_groups_hint: "Antigravity splits quota into separate 5-hour groups by model. Only checked groups are ignited; the Claude / GPT group is small and off by default.",
      codex_reset_enabled: "Forward Codex reset signals published by Did Codex Reset",
      codex_reset_pending: "When first turned on, send the pending schedule",
      save_hint: "Changes save to CPA's config.yaml automatically, and the plugin applies them within a few seconds. Text and numbers save when you leave the field or press Enter.",
      language_note: "Notifications use the Management Center language from the last time this page was opened, now {0}.",
      lang_zh: "中文", lang_en: "English",
      due: "due", minutes: "{0} min", hours_minutes: "{0} h {1} min", days: "{0} days",
      soon: "soon", in_time: "in {0}", just_now: "just now", ago: "{0} ago",
      reset_now: "reset",
      window_5h: "5 hours", window_7d: "7 days",
      state_disabled: "Disabled", state_starting: "Starting", state_running: "Running",
      last_poll: "last query {0} ({1})", next_poll: "next {0} ({1})",
      config_error: "The config cannot be parsed; the previous config stays in use: {0}",
      models_error: "Model list unavailable: {0}",
      no_bark: "No Bark push URL is set, so no notifications are sent.",
      no_models_key: "No model list API key is set, so ignition cannot pick a model.",
      remaining_aria: "{0} {1} remaining",
      source_passive: "passive", source_active: "active", updated: "updated {0}",
      no_accounts: "No ChatGPT, Claude or Antigravity credentials to monitor.",
      first_query: "The first query runs about 20 seconds after CPA starts.",
      refresh: "Refresh", refreshed: "Refreshed {0}",
      cooldown_stale: "Quota is back, but CPA cools this account down until {0} and rejects requests and ignition until then. Clear the cooldown in the CPA Management Center.",
      cooldown: "CPA cooldown until {0} ({1})",
      blocked_5h: "5-hour quota used up", blocked_7d: "7-day quota used up", blocked_other: "{0} quota used up",
      blocked_until: "Ignition resumes after the reset at {0}", blocked_wait: "Ignition resumes when the quota recovers",
      paused_until: "Paused until {0}", failures: "{0} failures", rolling: "Window not started", normal: "Normal",
      ignition_note: "Starts at {0}:00 every day, then ignites {1} seconds after each reset, until {2}.",
      ignition_off: "Ignition is off.",
      no_targets: "No quota group has ignition enabled.",
      ignite_now: "Ignite now",
      ignite_confirm: "Send an ignition request to {0} now? This starts a new 5-hour window.",
      ignited: "{0} ignited",
      ev_ignite: "Ignited", ev_ignite_manual: "Manual ignition", ev_ignite_failed: "Ignition failed", ev_ignite_paused: "Ignition paused", ev_model_refused: "Model refused",
      ev_cooldown_stale: "Cooldown not cleared", ev_notify: "Notified", ev_notify_failed: "Notification failed", ev_codex_reset: "Reset signal", ev_error: "Error",
      ev_ignited: "{0} · next reset {1}",
      ev_retry: "Retrying in {0}: {1}", ev_retry_cooldown: "Retrying when the CPA cooldown ends, in {0}: {1}",
      ev_seconds: "{0} s", ev_minutes: "{0} min",
      ev_paused: "Paused until {0}: {1}",
      ev_refused: "{0} was refused upstream; used {1}, not tried again for {2} hours",
      ev_notify_failed_detail: "{0}: {1}",
      ev_cooldown: "Quota is back, but CPA cools down until {0}",
      ev_codex_sent: "Sent {0} notifications",
      no_events: "No events yet", collapse: "Show less", show_more: "Show more ({0} in total)",
      failed: "Failed: {0}",
      range_h: "{0}h", range_d: "{0}d", range_mo: "{0} month",
      overview_aria: "Remaining quota overview",
      ignition_lane: "Ignition", lane_ok: "{0} succeeded", lane_failed: " · {0} failed",
      total: "Total", total_suffix: " total", more_accounts: "　… {0} more accounts",
      pooled_note: "Total of {0} accounts; the shade spans the lowest to the highest account",
      stat_total: "Total", stat_current: "Now", stat_min_account: "Lowest account", stat_min: "Lowest", stat_to_reset: "Until reset",
      detail_aria: "{0} remaining percent",
      detail_keys: "Use the left and right arrow keys to step through the values",
      passive_sample: " (passive)", active_sample: " (active)",
      quota_reset: "quota reset",
      loading: "Loading…", no_history: "No quota data in this range yet.",
      history_failed: "Could not read the history: {0}",
      tz_config: "set in the config", tz_cpa: "following the CPA server",
      tz_note: "Notifications and this page show times in {0}, {1}.",
      saved: "Saved; the plugin applies the new settings within a few seconds", save_failed: "Could not save: {0}",
      load_failed: "Could not load: {0}",
      refreshed_all: "Refreshed all quota", test_sent: "Test notification sent"
    }
  };

  var lang = "zh";

  function translate(key) {
    var text = (I18N[lang] && I18N[lang][key] !== undefined) ? I18N[lang][key] : (I18N.zh[key] !== undefined ? I18N.zh[key] : key);
    for (var i = 1; i < arguments.length; i++) text = text.split("{" + (i - 1) + "}").join(String(arguments[i]));
    return text;
  }

  // The language follows the Management Center: the lang attribute of the
  // parent page, which it updates on every switch, or the language it stored
  // when the page is opened on its own. Chinese variants show Chinese and
  // every other language English.
  function centerLanguage() {
    var root = parentRoot();
    var value = root ? root.getAttribute("lang") || "" : "";
    if (!value) {
      try {
        var raw = localStorage.getItem("cli-proxy-language") || "";
        try {
          var parsed = JSON.parse(raw);
          value = (parsed && parsed.state && parsed.state.language) || (parsed && parsed.language) || (typeof parsed === "string" ? parsed : "");
        } catch (e) { value = raw; }
      } catch (e) { /* no storage */ }
    }
    if (!value) value = navigator.language || "";
    return /^zh/i.test(value) ? "zh" : "en";
  }

  function applyStaticText() {
    document.documentElement.lang = lang === "zh" ? "zh-CN" : "en";
    [["data-i18n", null], ["data-i18n-placeholder", "placeholder"], ["data-i18n-title", "title"], ["data-i18n-aria-label", "aria-label"]].forEach(function (pair) {
      Array.prototype.forEach.call(document.querySelectorAll("[" + pair[0] + "]"), function (node) {
        var text = translate(node.getAttribute(pair[0]));
        if (pair[1]) node.setAttribute(pair[1], text);
        else node.textContent = text;
      });
    });
  }

  var languageApplied = false;
  function applyLanguage() {
    var next = centerLanguage();
    if (languageApplied && next === lang) return;
    lang = next;
    languageApplied = true;
    applyStaticText();
    renderRangeButtons();
    if (status) {
      renderStatus();
      $("timezone-note").textContent = timezoneText();
      reportLanguage();
    }
    if (status && history) renderChart();
  }

  // Notifications are sent without a page open, so the page tells the
  // plugin which language to use; it keeps the last one reported.
  var reportingLanguage = false;
  function reportLanguage() {
    if (!status || status.language === lang || reportingLanguage) return;
    reportingLanguage = true;
    api("POST", API + "/language", { language: lang }).then(function (data) {
      if (data.status) { status = data.status; renderLanguageNote(); }
    }).catch(function () { /* retried on the next status refresh */ }).then(function () { reportingLanguage = false; });
  }

  function renderLanguageNote() {
    $("language-note").textContent = translate("language_note", translate("lang_" + (status.language || "zh")));
  }

  // The Management Center keeps the key in localStorage, XOR-obfuscated with
  // a key derived from the host and user agent, when "remember password" is on.
  function centerKey() {
    try {
      var raw = localStorage.getItem("cli-proxy-auth");
      if (!raw) return "";
      var prefix = "enc::v1::";
      if (raw.indexOf(prefix) === 0) {
        var data = atob(raw.slice(prefix.length));
        var mask = new TextEncoder().encode("cli-proxy-api-webui::secure-storage|" + location.host + "|" + navigator.userAgent);
        var bytes = new Uint8Array(data.length);
        for (var i = 0; i < data.length; i++) bytes[i] = data.charCodeAt(i) ^ mask[i % mask.length];
        raw = new TextDecoder().decode(bytes);
      }
      var parsed = JSON.parse(raw);
      var stored = parsed && parsed.state ? parsed.state : parsed;
      return stored && typeof stored.managementKey === "string" ? stored.managementKey : "";
    } catch (e) {
      return "";
    }
  }

  function api(method, path, body) {
    var opts = { method: method, headers: { "Authorization": "Bearer " + key, "Accept": "application/json" } };
    if (body !== undefined) {
      opts.headers["Content-Type"] = "application/json";
      opts.body = JSON.stringify(body);
    }
    return fetch(path, opts).then(function (res) {
      return res.json().catch(function () { return {}; }).then(function (data) {
        if (res.status === 401) {
          askForKey(translate("key_invalid"));
          throw new Error("unauthorized");
        }
        if (!res.ok) throw new Error(data.error || data.message || ("HTTP " + res.status));
        return data;
      });
    });
  }

  function askForKey(message) {
    key = "";
    sessionStorage.removeItem(KEY_STORE);
    $("app").hidden = true;
    $("key-section").hidden = false;
    $("key-error").hidden = !message;
    $("key-error").textContent = message || "";
  }

  var toastTimer = null;
  function toast(message) {
    var node = $("toast");
    node.textContent = message;
    node.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { node.hidden = true; }, 5000);
  }

  // Times are shown in the plugin's time zone, like notifications.
  function offsetSeconds() {
    var value = status ? Number(status.utc_offset_seconds) : NaN;
    return isFinite(value) ? value : -new Date().getTimezoneOffset() * 60;
  }

  function parseTime(value) {
    if (!value) return null;
    var t = new Date(value);
    if (isNaN(t.getTime()) || t.getUTCFullYear() < 2000) return null;
    return t;
  }

  function pad(n) { return (n < 10 ? "0" : "") + n; }

  function shifted(t) { return new Date(t.getTime() + offsetSeconds() * 1000); }

  function fmtDateTime(t) {
    if (!t) return "—";
    var s = shifted(t);
    return pad(s.getUTCMonth() + 1) + "/" + pad(s.getUTCDate()) + " " + pad(s.getUTCHours()) + ":" + pad(s.getUTCMinutes());
  }

  function fmtClock(t) {
    var s = shifted(t);
    return pad(s.getUTCHours()) + ":" + pad(s.getUTCMinutes());
  }

  function fmtDay(t) {
    var s = shifted(t);
    return pad(s.getUTCMonth() + 1) + "/" + pad(s.getUTCDate());
  }

  function fmtDuration(ms) {
    if (ms <= 0) return translate("due");
    var minutes = Math.round(ms / 60000);
    if (minutes < 60) return translate("minutes", minutes);
    var hours = Math.floor(minutes / 60);
    if (hours < 48) return translate("hours_minutes", hours, minutes % 60);
    return translate("days", Math.round(hours / 24));
  }

  function fmtIn(t) {
    if (!t) return "";
    var ms = t.getTime() - Date.now();
    if (ms < 60000) return translate("soon");
    return translate("in_time", fmtDuration(ms));
  }

  function fmtAgo(t) {
    if (!t) return "";
    var ms = Date.now() - t.getTime();
    if (ms < 60000) return translate("just_now");
    return translate("ago", fmtDuration(ms));
  }

  // Bars and the chart color quota by the first two notification
  // thresholds, the same split as the 🟡 and 🔴 notifications.
  function quotaThresholds() {
    var cfg = status && status.config ? status.config : {};
    var notice = Number(cfg.notice_threshold), low = Number(cfg.low_threshold);
    return { notice: isFinite(notice) ? notice : 50, low: isFinite(low) ? low : 20 };
  }

  function quotaLevel(remaining) {
    var t = quotaThresholds();
    if (remaining <= t.low) return "low";
    if (remaining <= t.notice) return "medium";
    return "high";
  }

  function meterClass(remaining) { return "fill-" + quotaLevel(remaining); }

  function fmtCountdown(ms) {
    if (ms <= 0) return translate("reset_now");
    var minutes = Math.round(ms / 60000);
    if (minutes < 60) return minutes + "m";
    var hours = Math.floor(minutes / 60);
    if (hours < 48) return hours + "h " + pad(minutes % 60) + "m";
    return Math.floor(hours / 24) + "d " + (hours % 24) + "h";
  }

  function windowName(w) {
    if (w.short === "5h") return translate("window_5h");
    if (w.short === "7d") return translate("window_7d");
    return w.label;
  }

  function providerTitle(provider) {
    return { codex: "ChatGPT", claude: "Claude", antigravity: "Antigravity" }[provider] || provider;
  }

  function badge(kind, text) {
    return el("span", { class: "badge badge-" + kind }, [el("span", { class: "badge-dot" }), document.createTextNode(text)]);
  }

  // ---- Status rendering ----

  function renderHeader() {
    var state = $("state");
    clear(state);
    if (!status.enabled) state.appendChild(badge("muted", translate("state_disabled")));
    else if (!status.running) state.appendChild(badge("warning", translate("state_starting")));
    else state.appendChild(badge("success", translate("state_running")));
    var parts = ["v" + status.version];
    var last = parseTime(status.last_poll), next = parseTime(status.next_poll);
    if (last) parts.push(translate("last_poll", fmtClock(last), fmtAgo(last)));
    if (next) parts.push(translate("next_poll", fmtClock(next), fmtIn(next)));
    $("meta").textContent = parts.join(" · ");
  }

  function notice(text) {
    return el("div", { class: "notice" }, [el("span", { class: "notice-icon", text: "!" }), el("span", { text: text })]);
  }

  function renderNotices() {
    var box = $("notices");
    clear(box);
    if (status.config_error) box.appendChild(notice(translate("config_error", status.config_error)));
    if (status.lock_error) box.appendChild(notice(status.lock_error));
    if (status.list_error) box.appendChild(notice(status.list_error));
    if (status.models_error) box.appendChild(notice(translate("models_error", status.models_error)));
    if (status.config && !status.config.bark_url) box.appendChild(notice(translate("no_bark")));
    if (status.config && status.config.ignition && status.config.ignition.enabled && !status.config.models_api_key) {
      box.appendChild(notice(translate("no_models_key")));
    }
  }

  function quotaRow(group, w, index) {
    var remaining = Math.max(0, Math.min(100, w.remaining));
    var reset = parseTime(w.reset);
    var meta = [el("span", { class: "quota-percent", text: Math.round(remaining) + "%" })];
    if (reset) {
      var left = reset.getTime() - Date.now();
      meta.push(el("span", { class: "quota-reset", text: fmtDateTime(reset) }));
      meta.push(el("span", { class: "quota-countdown" + (left > 0 && left <= 3600000 ? " soon" : ""), text: fmtCountdown(left) }));
    }
    return el("div", { class: "quota-row" }, [
      el("div", { class: "quota-row-head" }, [
        el("span", { class: "quota-name", text: windowName(w) }),
        el("span", { class: "quota-meta" }, meta)
      ]),
      el("div", { class: "quota-bar", role: "meter", "aria-valuemin": "0", "aria-valuemax": "100",
        "aria-valuenow": String(Math.round(remaining)), "aria-label": translate("remaining_aria", group.label, windowName(w)) }, [
        el("span", { class: "quota-fill " + meterClass(remaining), style: "width:" + remaining + "%;--meter-index:" + index })
      ])
    ]);
  }

  function freshness(group) {
    var latest = null, source = "";
    (group.windows || []).forEach(function (w) {
      var t = parseTime(w.observed_at);
      if (t && (!latest || t > latest)) { latest = t; source = w.source; }
    });
    if (!latest) return null;
    return el("div", { class: "group-foot" }, [
      el("span", { class: "source source-" + source, text: source === "passive" ? translate("source_passive") : translate("source_active") }),
      el("span", { text: translate("updated", fmtAgo(latest)) })
    ]);
  }

  function renderAccounts() {
    var box = $("accounts");
    clear(box);
    if (!status.accounts.length) {
      box.appendChild(el("p", { class: "empty", text: status.running ? translate("no_accounts") : translate("first_query") }));
      return;
    }
    var meterIndex = 0;
    status.accounts.forEach(function (account) {
      var card = el("div", { class: "card account" });
      var refresh = el("button", { type: "button", class: "btn btn-ghost btn-sm", text: translate("refresh"), onclick: function () {
        runAction(refresh, API + "/refresh", { auth_index: account.auth_index }, translate("refreshed", account.title));
      } });
      card.appendChild(el("div", { class: "account-head" }, [
        el("span", { class: "type-badge type-" + account.provider, text: account.title }),
        el("span", { class: "account-email", title: account.email || account.name, text: account.email || account.name }),
        refresh
      ]));
      (account.groups || []).forEach(function (group) {
        var block = el("div", { class: "quota-group" });
        if (group.source_label && group.source_label !== providerTitle(account.provider)) {
          block.appendChild(el("div", { class: "group-title", text: group.source_label }));
        }
        (group.windows || []).forEach(function (w) { block.appendChild(quotaRow(group, w, meterIndex++)); });
        var foot = freshness(group);
        if (foot) block.appendChild(foot);
        card.appendChild(block);
      });
      var cooldown = parseTime(account.cooldown_until);
      if (cooldown && account.stale_cooldown) {
        card.appendChild(el("div", { class: "error-text", text: translate("cooldown_stale", fmtDateTime(cooldown)) }));
      } else if (cooldown) {
        card.appendChild(el("div", { class: "cell-note", text: translate("cooldown", fmtDateTime(cooldown), fmtIn(cooldown)) }));
      }
      if (account.error) card.appendChild(el("div", { class: "error-text", text: account.error }));
      box.appendChild(card);
    });
  }

  function targetStatus(t) {
    var tr = translate;
    if (t.blocked_window) {
      var until = parseTime(t.blocked_until);
      var blocked = t.blocked_window === "5h" || t.blocked_window === "7d" ? tr("blocked_" + t.blocked_window) : tr("blocked_other", t.blocked_window);
      return { kind: "muted", text: blocked, title: until ? tr("blocked_until", fmtDateTime(until)) : tr("blocked_wait") };
    }
    var circuit = parseTime(t.circuit_until);
    if (circuit) return { kind: "failure", text: tr("paused_until", fmtDateTime(circuit)), title: t.circuit_reason };
    if (t.consecutive_failures > 0) return { kind: "warning", text: tr("failures", t.consecutive_failures), title: t.last_error };
    if (t.rolling) return { kind: "muted", text: tr("rolling") };
    return { kind: "success", text: tr("normal") };
  }

  function renderTargets() {
    var body = $("targets");
    clear(body);
    var ignition = (status.config && status.config.ignition) || {};
    var endMinutes = (ignition.end_hour || 0) * 60 + (ignition.end_grace_minutes || 0);
    $("ignition-note").textContent = ignition.enabled
      ? translate("ignition_note", pad(ignition.start_hour), ignition.grace_seconds, pad(Math.floor(endMinutes / 60) % 24) + ":" + pad(endMinutes % 60))
      : translate("ignition_off");
    if (!status.targets.length) {
      body.appendChild(el("tr", {}, [el("td", { colspan: "7", class: "empty", text: translate("no_targets") })]));
      return;
    }
    status.targets.forEach(function (t) {
      var st = targetStatus(t);
      var button = el("button", { type: "button", class: "btn btn-secondary btn-sm", text: translate("ignite_now"), onclick: function () {
        if (!confirm(translate("ignite_confirm", t.label))) return;
        runAction(button, API + "/ignite", { target: t.key }, translate("ignited", t.label));
      } });
      var next = parseTime(t.next_due), reset = parseTime(t.reset), last = parseTime(t.last_success);
      var statusCell = el("td", {}, [badge(st.kind, st.text)]);
      if (st.title) statusCell.appendChild(el("div", { class: "cell-note", text: st.title }));
      body.appendChild(el("tr", {}, [
        el("td", { class: "strong", text: t.label }),
        el("td", { class: "mono", text: ignition.enabled && next ? fmtDateTime(next) : "—" }),
        el("td", { class: "mono", text: reset ? fmtDateTime(reset) : "—" }),
        el("td", { class: "mono", text: last ? fmtDateTime(last) : "—" }),
        el("td", { class: "mono", text: t.last_model || "—" }),
        statusCell,
        el("td", { class: "actions" }, [button])
      ]));
    });
  }

  // Badge style per event type; the label is ev_<kind> in the dictionary.
  var EVENT_TYPES = {
    ignite: "success", ignite_manual: "success", ignite_failed: "warning", ignite_paused: "failure", model_refused: "warning",
    cooldown_stale: "warning", notify: "muted", notify_failed: "warning", codex_reset: "muted", error: "failure"
  };
  var EVENTS_COLLAPSED = 12;
  var eventsExpanded = false;

  function groupLabel(key) {
    var label = "";
    (status.accounts || []).forEach(function (a) {
      (a.groups || []).forEach(function (g) { if (g.key === key) label = g.label; });
    });
    return label;
  }

  function retryText(seconds) {
    return seconds < 60 ? translate("ev_seconds", seconds) : translate("ev_minutes", Math.round(seconds / 60));
  }

  // The event text in the page language, built from the event params.
  // Events written before params existed keep their stored Chinese text.
  function eventDetail(ev) {
    var p = ev.params;
    if (!p) return null;
    var time = function (value) { return fmtDateTime(parseTime(value)); };
    switch (ev.kind) {
      case "ignite": case "ignite_manual": return p.reset ? translate("ev_ignited", p.model, time(p.reset)) : p.model;
      case "ignite_failed": return translate(p.cooldown ? "ev_retry_cooldown" : "ev_retry", retryText(Number(p.retry_seconds) || 0), p.error);
      case "ignite_paused": return translate("ev_paused", time(p.until), p.error);
      case "model_refused": return translate("ev_refused", p.model, p.fallback, p.hours);
      case "notify": return p.title;
      case "notify_failed": return translate("ev_notify_failed_detail", p.title, p.error);
      case "cooldown_stale": return translate("ev_cooldown", time(p.until));
      case "codex_reset": return translate("ev_codex_sent", p.count);
    }
    return null;
  }

  // Events written before label and detail existed only have a message that
  // starts with the group label.
  function eventParts(ev) {
    var label = ev.label || (ev.group ? groupLabel(ev.group) : "");
    var translated = eventDetail(ev);
    if (translated !== null) return { label: label || "—", detail: translated };
    var detail = ev.detail || ev.message || "";
    if (!ev.detail && label && detail.indexOf(label + " ") === 0) detail = detail.slice(label.length + 1);
    if (!ev.detail) detail = detail.replace(/^点火成功（(.+?)），下次重置 /, "$1 · 下次重置 ");
    return { label: label || "—", detail: detail };
  }

  function renderEvents() {
    var list = $("events");
    clear(list);
    var more = $("events-more");
    if (!status.events.length) {
      list.appendChild(el("p", { class: "empty", text: translate("no_events") }));
      more.hidden = true;
      return;
    }
    var events = eventsExpanded ? status.events.slice(0, 100) : status.events.slice(0, EVENTS_COLLAPSED);
    events.forEach(function (ev) {
      var known = EVENT_TYPES[ev.kind];
      var type = [known || (ev.level === "error" ? "failure" : ev.level === "warn" ? "warning" : "muted"), known ? translate("ev_" + ev.kind) : ev.kind];
      var parts = eventParts(ev);
      list.appendChild(el("div", { class: "event-row", role: "row" }, [
        el("span", { class: "event-time", role: "cell", text: fmtDateTime(parseTime(ev.time)) }),
        el("span", { class: "event-type", role: "cell" }, [badge(type[0], type[1])]),
        el("span", { class: "event-label", role: "cell", text: parts.label }),
        el("span", { class: "event-detail", role: "cell", title: parts.detail, text: parts.detail })
      ]));
    });
    more.hidden = status.events.length <= EVENTS_COLLAPSED;
    more.textContent = eventsExpanded ? translate("collapse") : translate("show_more", Math.min(status.events.length, 100));
  }

  // The model inputs show, after the automatic rule, the model the next
  // ignition starts with.
  function renderModelHints() {
    var next = status.next_models || {};
    var form = $("settings");
    ["codex", "claude", "antigravity"].forEach(function (provider) {
      var input = form.elements["providers." + provider + ".model"];
      if (!input) return;
      var text = translate("model_auto_" + provider);
      if (next[provider]) text += " - " + next[provider];
      input.setAttribute("placeholder", text);
    });
  }

  function renderStatus() {
    renderHeader();
    renderNotices();
    renderModelHints();
    renderAccounts();
    renderTargets();
    renderEvents();
    renderLanguageNote();
  }

  function loadStatus() {
    return api("GET", API + "/status").then(function (data) {
      status = data;
      renderStatus();
      reportLanguage();
    });
  }

  function runAction(button, path, body, success) {
    button.disabled = true;
    return api("POST", path, body).then(function (data) {
      if (data.status) { status = data.status; renderStatus(); }
      toast(data.ok ? success : translate("failed", data.error));
      loadHistory();
    }).catch(function (err) {
      if (err.message !== "unauthorized") toast(translate("failed", err.message));
    }).then(function () { button.disabled = false; });
  }

  // ---- Chart ----

  // Time ranges follow the quota window, in hours for the 5-hour quota and
  // days for the 7-day quota: one unit; half of a window plus one (3h, 4d);
  // a window plus one (6h, 8d); half a day or month, which fits two whole
  // windows (12h, 15d); a day or a month; five windows plus one (26h, 36d).
  // Keys are the history endpoint's range values.
  // Each entry is the range value, the count and the unit of its label.
  var RANGES = {
    "5h": [["1h", 1, "h"], ["3h", 3, "h"], ["6h", 6, "h"], ["12h", 12, "h"], ["24h", 24, "h"], ["26h", 26, "h"]],
    "7d": [["24h", 1, "d"], ["4d", 4, "d"], ["8d", 8, "d"], ["15d", 15, "d"], ["1mo", 1, "mo"], ["36d", 36, "d"]]
  };
  // The default ranges are the shortest that cover one whole window.
  var DEFAULT_RANGE = { "5h": "6h", "7d": "8d" };
  // Default row order by quota name; other quotas follow in API order. The
  // status API orders the quota groups of an account the same way (groupOrder
  // in internal/engine/api.go).
  var SERVICE_ORDER = ["Claude", "ChatGPT", "Gemini", "Fable", "Claude / GPT"];
  var LANE = 22, LANE_GAP = 6, AXIS_H = 22, EVENT_H = 24;
  // A rise of this many points between two samples is a reset.
  var RESET_JUMP = 5;
  // Quotas report whole percents; steps this small are a steady decline.
  var QUANTUM = 2;

  var chartView = null;
  var hoverSource = "";
  var hoverListeners = [];
  // Samples of the row in the detail chart; hovering either chart snaps to them.
  var snapPoints = [];

  function cssVar(name) {
    return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  }

  function levelVar(remaining) {
    return { high: "--viz-success", medium: "--quota-medium-color", low: "--viz-failure" }[quotaLevel(remaining)];
  }

  function pct(v) { return v == null ? "—" : Math.round(v) + "%"; }

  function unixDate(t) { return new Date(t * 1000); }

  function windowKind(series) {
    if (series.window === "five-hour" || series.window_label === "5h") return "5h";
    if (series.window === "seven-day") return "7d";
    return series.window_label || series.window;
  }

  function valueAt(points, t) {
    var value = null;
    for (var i = 0; i < points.length; i++) {
      if (points[i][0] <= t) value = points[i];
      else break;
    }
    return value;
  }

  // Samples further apart than a few poll intervals leave a gap, which is
  // drawn as missing data rather than as a held value. Active queries are
  // skipped for up to passive_skip_max_minutes while response headers keep
  // the main windows fresh; windows without header data, such as Fable, then
  // wait that long plus one poll, which is not a gap.
  function gapLimit() {
    var cfg = status && status.config ? status.config : {};
    var poll = Number(cfg.poll_interval_seconds) || 300;
    var limit = Math.max(history.bucket_seconds || 0, poll) * 3.5;
    if (Number(cfg.passive_skip_seconds) > 0) {
      limit = Math.max(limit, (Number(cfg.passive_skip_max_minutes) || 0) * 60 + poll * 1.5);
    }
    return limit;
  }

  // Pooled remaining of several accounts: the mean percentage, which is the
  // share of the total quota left when the accounts have the same plan. Each
  // point also carries the lowest and highest account for the range band.
  function pooled(rows, limit) {
    var times = [];
    rows.forEach(function (r) { r.points.forEach(function (p) { times.push(p[0]); }); });
    times.sort(function (a, b) { return a - b; });
    var out = [];
    times.forEach(function (t, i) {
      if (i && t === times[i - 1]) return;
      var sum = 0, n = 0, lo = 101, hi = -1;
      rows.forEach(function (r) {
        var p = valueAt(r.points, t);
        if (!p || t - p[0] > limit) return;
        sum += p[1]; n++; lo = Math.min(lo, p[1]); hi = Math.max(hi, p[1]);
      });
      if (n) out.push([t, sum / n, 0, lo, hi]);
    });
    return out;
  }

  // One row per quota group label. A label with several accounts becomes a
  // pooled row that expands into one row per account.
  function chartRows() {
    var info = {};
    status.accounts.forEach(function (a) {
      (a.groups || []).forEach(function (g) {
        (g.windows || []).forEach(function (w) {
          var reset = parseTime(w.reset);
          info[g.key + "|" + w.id] = { email: a.email || a.name, reset: reset ? reset.getTime() / 1000 : null };
        });
      });
    });
    var limit = gapLimit();
    var byLabel = {}, order = [];
    history.series.forEach(function (s) {
      if (!s.points || !s.points.length || windowKind(s) !== chartWindow) return;
      var meta = info[s.group + "|" + s.window] || {};
      var id = s.provider + "|" + (s.source_label || s.label);
      if (!byLabel[id]) { byLabel[id] = []; order.push(id); }
      byLabel[id].push({ id: s.group + "|" + s.window, name: s.label, source: s.source_label || s.label, sub: meta.email || s.auth_index || "", group: s.group,
        points: s.points, reset: meta.reset, last: s.points[s.points.length - 1][1] });
    });
    var rows = order.map(function (id, index) {
      var kids = byLabel[id];
      var rank = SERVICE_ORDER.indexOf(kids[0].source);
      if (rank < 0) rank = SERVICE_ORDER.length + index;
      if (kids.length === 1) { kids[0].order = rank; return kids[0]; }
      var resets = kids.map(function (k) { return k.reset; }).filter(Boolean);
      return { id: "group:" + id, name: kids[0].source, children: kids, points: pooled(kids, limit), order: rank,
        last: kids.reduce(function (sum, k) { return sum + k.last; }, 0) / kids.length,
        reset: resets.length ? Math.min.apply(null, resets) : null, groups: kids.map(function (k) { return k.group; }) };
    });
    var lowFirst = chartSort === "low";
    rows.sort(function (a, b) { return lowFirst ? (a.last - b.last) || (a.order - b.order) : a.order - b.order; });
    rows.forEach(function (r) {
      if (r.children && lowFirst) r.children.sort(function (a, b) { return a.last - b.last; });
    });
    return rows;
  }

  function findRow(rows, id) {
    var hit = null;
    rows.forEach(function (r) {
      if (r.id === id) hit = r;
      (r.children || []).forEach(function (c) { if (c.id === id) hit = c; });
    });
    return hit;
  }

  function geometry(width) {
    var from = new Date(history.from).getTime() / 1000;
    var to = new Date(history.to).getTime() / 1000;
    var labelW = width < 640 ? 120 : 190, rightW = width < 640 ? 112 : 136;
    var plotW = width - labelW - rightW;
    return { from: from, to: to, width: width, labelW: labelW, rightW: rightW, plotW: plotW,
      x: function (t) { return labelW + (t - from) / (to - from) * plotW; },
      t: function (px) { return from + (px - labelW) / plotW * (to - from); } };
  }

  function chartTicks(g) {
    var span = g.to - g.from;
    var step = span <= 3600 ? 600 : span <= 3 * 3600 ? 1800 : span <= 6 * 3600 ? 3600 : span <= 12 * 3600 ? 2 * 3600 : span <= 2 * 86400 ? 4 * 3600 : span <= 8 * 86400 ? 86400 : span <= 15 * 86400 ? 2 * 86400 : 5 * 86400;
    var offset = offsetSeconds(), list = [];
    for (var t = Math.ceil((g.from + offset) / step) * step - offset; t <= g.to; t += step) list.push(t);
    return { list: list, label: function (t) { return span <= 2 * 86400 ? fmtClock(unixDate(t)) : fmtDay(unixDate(t)); } };
  }

  // Contiguous stretches of samples; each ends at its last sample plus one
  // gap limit, or at "now" when that is sooner.
  function runs(points, limit, end) {
    var out = [], cur = [];
    points.forEach(function (p) {
      if (cur.length && p[0] - cur[cur.length - 1][0] > limit) { out.push(cur); cur = []; }
      cur.push(p);
    });
    if (cur.length) out.push(cur);
    return out.map(function (r, i) {
      var last = r[r.length - 1][0];
      var stop = i === out.length - 1 ? Math.min(end, last + limit) : last + Math.min(limit, 300);
      return { pts: r, start: r[0][0], end: Math.max(stop, last) };
    });
  }

  function hatchPattern(root) {
    var defs = svg("defs", {});
    var pattern = svg("pattern", { id: "chart-hatch", width: 6, height: 6, patternUnits: "userSpaceOnUse", patternTransform: "rotate(45)" });
    pattern.appendChild(svg("rect", { width: 2, height: 6, fill: cssVar("--chart-hatch") }));
    defs.appendChild(pattern);
    root.appendChild(defs);
    return defs;
  }

  var SVG_NS = "http://www.w3.org/2000/svg";
  function svg(tag, attrs) {
    var node = document.createElementNS(SVG_NS, tag);
    Object.keys(attrs || {}).forEach(function (name) { node.setAttribute(name, attrs[name]); });
    return node;
  }

  function svgText(attrs, text) {
    var node = svg("text", attrs);
    node.textContent = text;
    return node;
  }

  // A band of solid color per quota level. Samples merge while the level
  // stays the same, and edges snap to whole pixels so neighbours leave no seam.
  function drawBand(root, g, points, y0, limit) {
    var cursor = g.from;
    function hatch(a, b) {
      if (g.x(b) - g.x(a) > 1) root.appendChild(svg("rect", { x: g.x(a), y: y0, width: g.x(b) - g.x(a), height: LANE, fill: "url(#chart-hatch)" }));
    }
    runs(points, limit, g.to).forEach(function (run) {
      if (run.start > cursor + limit) hatch(cursor, run.start);
      cursor = run.end;
      var segments = [];
      run.pts.forEach(function (p, i) {
        var end = i + 1 < run.pts.length ? run.pts[i + 1][0] : run.end;
        var level = levelVar(p[1]), last = segments[segments.length - 1];
        if (last && last.level === level) last.end = end;
        else segments.push({ start: Math.max(g.from, p[0]), end: end, level: level });
      });
      segments.forEach(function (seg) {
        var x0 = Math.round(g.x(seg.start)), x1 = Math.max(x0 + 1, Math.round(g.x(seg.end)));
        root.appendChild(svg("rect", { x: x0, y: y0, width: x1 - x0, height: LANE, fill: cssVar(seg.level), "fill-opacity": 0.8, "shape-rendering": "crispEdges" }));
      });
    });
    if (cursor < g.to - limit) hatch(cursor, g.to);
  }

  function ignitionEvents() {
    var from = new Date(history.from).getTime() / 1000;
    return (history.events || []).map(function (ev) {
      var parts = eventParts(ev);
      return { t: new Date(ev.time).getTime() / 1000, failed: ev.kind === "ignite_failed" || ev.kind === "ignite_paused",
        message: (parts.label !== "—" ? parts.label + " " : "") + parts.detail, group: ev.group };
    }).filter(function (ev) { return ev.t >= from; });
  }

  function placeTip(tip, box, g, t, top) {
    var scale = box.clientWidth / g.width, left = g.x(t) * scale + 14;
    if (left + 280 > box.clientWidth) left = g.x(t) * scale - 294;
    tip.style.left = Math.max(0, left) + "px";
    tip.style.top = top + "px";
  }

  function tipRow(tip, text, value, strong) {
    tip.appendChild(el("div", { class: "row" + (strong ? " strong" : "") }, [
      el("span", { class: "key", style: "background:" + (value != null ? cssVar(levelVar(value)) : "transparent") }),
      el("span", { class: "name", text: text }),
      el("span", { class: "value", text: value != null ? pct(value) : translate("no_data") })
    ]));
  }

  // Overview: one band per row on a shared time axis, the ignition lane
  // below, and a crosshair shared with the detail chart.
  function renderOverview(rows, box) {
    var g = chartView = geometry(Math.max(480, box.clientWidth || 800));
    var limit = gapLimit();
    var flat = [];
    rows.forEach(function (r) {
      flat.push({ row: r, depth: 0 });
      if (r.children && chartExpanded[r.id]) r.children.forEach(function (c) { flat.push({ row: c, depth: 1 }); });
    });
    var height = AXIS_H + flat.length * (LANE + LANE_GAP) + EVENT_H + 6;
    var root = svg("svg", { viewBox: "0 0 " + g.width + " " + height, role: "img", "aria-label": translate("overview_aria") });
    hatchPattern(root);
    var ticks = chartTicks(g);
    ticks.list.forEach(function (t) {
      root.appendChild(svgText({ x: g.x(t), y: 13, "text-anchor": "middle", "font-size": 11, fill: cssVar("--text-tertiary") }, ticks.label(t)));
      root.appendChild(svg("line", { x1: g.x(t), x2: g.x(t), y1: AXIS_H - 4, y2: height - 4, stroke: cssVar("--border-color"), "stroke-dasharray": "2 4" }));
    });
    var rowY = {};
    flat.forEach(function (f, i) {
      var r = f.row, y0 = AXIS_H + i * (LANE + LANE_GAP);
      rowY[r.id] = y0;
      var selected = chartSelected === r.id;
      if (selected) root.appendChild(svg("rect", { x: 0, y: y0 - 3, width: g.width, height: LANE + 6, rx: 6, fill: cssVar("--chart-select") }));
      var lx = 4 + f.depth * 18;
      if (r.children) root.appendChild(svgText({ x: lx, y: y0 + 15, "font-size": 10, fill: cssVar("--text-tertiary") }, chartExpanded[r.id] ? "▾" : "▸"));
      var nameX = lx + (r.children ? 14 : 0);
      var name = f.depth ? r.sub : r.name;
      var maxChars = Math.floor((g.labelW - nameX - (r.children ? 30 : 8)) / 7);
      if (name.length > maxChars) name = name.slice(0, Math.max(1, maxChars - 1)) + "…";
      root.appendChild(svgText({ x: nameX, y: y0 + 15, "font-size": f.depth ? 12 : 13, "font-weight": f.depth ? 400 : 600,
        fill: cssVar(f.depth ? "--text-secondary" : "--text-primary"), "class": "row-name" }, name));
      if (r.children) root.appendChild(svgText({ x: nameX + name.length * 8 + 8, y: y0 + 15, "font-size": 11, fill: cssVar("--text-tertiary") }, "×" + r.children.length));
      root.appendChild(svg("rect", { x: g.labelW, y: y0, width: g.plotW, height: LANE, rx: 4, fill: cssVar("--chart-lane") }));
      drawBand(root, g, r.points, y0, limit);
      var rx = g.width - g.rightW + 14;
      root.appendChild(svg("rect", { x: rx, y: y0 + 7, width: 8, height: 8, rx: 2, fill: cssVar(levelVar(r.last)) }));
      root.appendChild(svgText({ x: rx + 14, y: y0 + 15, "font-size": 13, "font-weight": 650, fill: cssVar("--text-primary") }, pct(r.last)));
      if (r.reset) root.appendChild(svgText({ x: g.width - 2, y: y0 + 15, "font-size": 11, "text-anchor": "end", fill: cssVar("--text-tertiary") },
        "↻" + fmtCountdown(r.reset * 1000 - Date.now())));
      var hit = svg("rect", { x: 0, y: y0 - 3, width: g.width, height: LANE + 6, fill: "transparent", style: "cursor:pointer" });
      hit.addEventListener("click", function () {
        if (r.children && chartSelected === r.id) chartExpanded[r.id] = !chartExpanded[r.id];
        chartSelected = r.id;
        renderChart();
      });
      hit.addEventListener("pointermove", function (e) { hoverAt(e, root, r.id, "overview"); });
      root.appendChild(hit);
    });

    // Ignitions at the same moment merge into one mark with a count.
    var ey = AXIS_H + flat.length * (LANE + LANE_GAP) + 4, cy = ey + 9;
    root.appendChild(svgText({ x: 4, y: ey + 13, "font-size": 12, fill: cssVar("--text-tertiary"), "class": "row-name" }, translate("ignition_lane")));
    root.appendChild(svg("line", { x1: g.labelW, x2: g.labelW + g.plotW, y1: cy, y2: cy, stroke: cssVar("--border-color") }));
    var events = ignitionEvents(), clusters = [];
    events.slice().sort(function (a, b) { return a.t - b.t; }).forEach(function (ev) {
      var last = clusters[clusters.length - 1];
      if (last && g.x(ev.t) - g.x(last.t) < 26) { last.n++; last.failed = last.failed || ev.failed; }
      else clusters.push({ t: ev.t, n: 1, failed: ev.failed });
    });
    clusters.forEach(function (c) {
      var x = g.x(c.t);
      if (c.n > 1) root.appendChild(svgText({ x: x + 6, y: cy + 4, "font-size": 10, fill: cssVar("--text-tertiary") }, "×" + c.n));
      if (c.failed) root.appendChild(svg("path", { d: "M" + (x - 4) + " " + (cy - 4) + "L" + (x + 4) + " " + (cy + 4) + "M" + (x + 4) + " " + (cy - 4) + "L" + (x - 4) + " " + (cy + 4),
        stroke: cssVar("--viz-failure"), "stroke-width": 2.2, "stroke-linecap": "round" }));
      else root.appendChild(svg("circle", { cx: x, cy: cy, r: 3.5, fill: cssVar("--viz-success"), stroke: cssVar("--bg-primary"), "stroke-width": 1.5 }));
    });
    var failed = events.filter(function (ev) { return ev.failed; }).length;
    root.appendChild(svgText({ x: g.width - 2, y: ey + 13, "font-size": 11, "text-anchor": "end", fill: cssVar(failed ? "--viz-failure" : "--text-tertiary") },
      translate("lane_ok", events.length - failed) + (failed ? translate("lane_failed", failed) : "")));
    var eventHit = svg("rect", { x: g.labelW, y: ey - 2, width: g.plotW, height: 22, fill: "transparent" });
    eventHit.addEventListener("pointermove", function (e) { hoverAt(e, root, null, "overview"); });
    root.appendChild(eventHit);

    var cross = svg("line", { y1: AXIS_H - 4, y2: height - 4, stroke: cssVar("--text-tertiary"), "stroke-width": 1, visibility: "hidden", "pointer-events": "none" });
    root.appendChild(cross);
    root.addEventListener("pointerleave", function () { setHover(null); });
    box.appendChild(root);
    var tip = el("div", { class: "tooltip", hidden: true });
    box.appendChild(tip);

    hoverListeners.push(function (t, rowId) {
      if (t == null) { cross.setAttribute("visibility", "hidden"); tip.hidden = true; return; }
      cross.setAttribute("x1", g.x(t)); cross.setAttribute("x2", g.x(t)); cross.setAttribute("visibility", "visible");
      if (hoverSource !== "overview") { tip.hidden = true; return; }
      clear(tip);
      tip.appendChild(el("div", { class: "time", text: fmtDateTime(unixDate(t)) }));
      function at(row) {
        var p = valueAt(row.points, t);
        return p && t - p[0] <= limit ? p[1] : null;
      }
      flat.forEach(function (f) {
        var r = f.row;
        if (f.depth) { tipRow(tip, "　" + r.sub, at(r), r.id === rowId); return; }
        tipRow(tip, r.name + (r.children ? translate("total_suffix") : ""), at(r), r.id === rowId);
        if (r.children && !chartExpanded[r.id]) {
          r.children.slice(0, 5).forEach(function (c) { tipRow(tip, "　" + c.sub, at(c), false); });
          if (r.children.length > 5) tip.appendChild(el("div", { class: "row muted", text: translate("more_accounts", r.children.length - 5) }));
        }
      });
      var near = (g.to - g.from) / g.plotW * 8;
      events.forEach(function (ev) {
        if (Math.abs(ev.t - t) <= near) tip.appendChild(el("div", { class: "note" + (ev.failed ? " failed" : ""), text: fmtClock(unixDate(ev.t)) + " " + ev.message }));
      });
      tip.hidden = false;
      var scale = box.clientWidth / g.width;
      placeTip(tip, box, g, t, ((rowY[rowId] != null ? rowY[rowId] : ey) + 28) * scale);
    });
  }

  // Monotone cubic (Fritsch-Carlson): smooth, but never overshoots the
  // samples, so a quota that only falls is never drawn rising.
  function monotonePath(xy) {
    var n = xy.length;
    if (n < 3) return xy.map(function (p, i) { return (i ? "L" : "M") + p[0] + " " + p[1]; }).join("");
    var dx = [], m = [], tg = [], i;
    for (i = 0; i < n - 1; i++) { dx[i] = xy[i + 1][0] - xy[i][0]; m[i] = dx[i] ? (xy[i + 1][1] - xy[i][1]) / dx[i] : 0; }
    tg[0] = m[0]; tg[n - 1] = m[n - 2];
    for (i = 1; i < n - 1; i++) tg[i] = m[i - 1] * m[i] <= 0 ? 0 : (m[i - 1] + m[i]) / 2;
    for (i = 0; i < n - 1; i++) {
      if (!m[i]) { tg[i] = tg[i + 1] = 0; continue; }
      var a = tg[i] / m[i], b = tg[i + 1] / m[i], h = a * a + b * b;
      if (h > 9) { var k = 3 / Math.sqrt(h); tg[i] = k * a * m[i]; tg[i + 1] = k * b * m[i]; }
    }
    var d = "M" + xy[0][0] + " " + xy[0][1];
    for (i = 0; i < n - 1; i++) {
      var third = dx[i] / 3;
      d += "C" + (xy[i][0] + third) + " " + (xy[i][1] + tg[i] * third) + " " + (xy[i + 1][0] - third) + " " +
        (xy[i + 1][1] - tg[i + 1] * third) + " " + xy[i + 1][0] + " " + xy[i + 1][1];
    }
    return d;
  }

  // Within one window a quota only falls, but active queries and response
  // headers can disagree by a point and round differently, so samples step
  // back up for a moment. Pool adjacent violators to the closest
  // non-increasing sequence; a curve through the raw samples would show
  // each reversal as a notch.
  function declining(pts, key) {
    var blocks = [];
    pts.forEach(function (p) {
      blocks.push({ sum: p[key], n: 1 });
      while (blocks.length > 1) {
        var top = blocks[blocks.length - 1], prev = blocks[blocks.length - 2];
        if (top.sum / top.n <= prev.sum / prev.n) break;
        prev.sum += top.sum; prev.n += top.n;
        blocks.pop();
      }
    });
    var out = [], i = 0;
    blocks.forEach(function (b) {
      for (var j = 0; j < b.n; j++, i++) {
        var p = pts[i].slice();
        p[key] = b.sum / b.n;
        out.push(p);
      }
    });
    return out;
  }

  // Keep the first sample of every value, and the last sample of a plateau
  // only when a real drop follows it: 62, 61, 60 is a steady decline, while
  // 100 held for an hour and then 80 is a burst of use.
  function curvePoints(pts, key) {
    return pts.filter(function (p, i) {
      if (i === 0 || i === pts.length - 1 || p[key] !== pts[i - 1][key]) return true;
      return Math.abs(pts[i + 1][key] - p[key]) > QUANTUM;
    });
  }

  // Split samples where any of the keys rises by a reset. Resets one sample
  // apart (two accounts, or a reset seen late) count as one.
  function splitAtResets(pts, keys) {
    var parts = [[pts[0]]], resets = [];
    for (var i = 1; i < pts.length; i++) {
      var up = keys.some(function (k) { return pts[i][k] - pts[i - 1][k] >= RESET_JUMP; });
      if (up && parts.length > 1 && parts[parts.length - 1].length === 1) {
        parts[parts.length - 1] = [];
        resets[resets.length - 1] = pts[i][0];
      } else if (up) {
        parts.push([]);
        resets.push(pts[i][0]);
      }
      parts[parts.length - 1].push(pts[i]);
    }
    // Hold each piece's last value until the reset sample, so the line
    // reaches the reset instead of stopping a poll interval before it.
    for (var k = 0; k + 1 < parts.length; k++) {
      var tail = parts[k][parts[k].length - 1];
      parts[k].push([parts[k + 1][0][0]].concat(tail.slice(1)));
    }
    return { parts: parts, resets: resets };
  }

  // Detail: the selected row as a smoothed line that breaks at resets. A
  // pooled row adds a band from its lowest to its highest account.
  function renderDetail(row, head, box) {
    var g = chartView, limit = gapLimit();
    snapPoints = row.points;
    var group = !!row.children;
    var lowest = Math.min.apply(null, row.points.map(function (p) { return group ? p[3] : p[1]; }));
    head.appendChild(el("span", { class: "detail-name", text: row.name + " · " + translate(chartWindow === "5h" ? "quota_5h" : "quota_7d") }));
    head.appendChild(el("span", { class: "muted", text: group ? translate("pooled_note", row.children.length) : row.sub }));
    [[translate(group ? "stat_total" : "stat_current"), pct(row.last)], [translate(group ? "stat_min_account" : "stat_min"), pct(lowest)],
      [translate("stat_to_reset"), row.reset ? fmtCountdown(row.reset * 1000 - Date.now()) : "—"]].forEach(function (stat) {
      head.appendChild(el("span", { class: "detail-stat" }, [document.createTextNode(stat[0]), el("strong", { text: stat[1] })]));
    });

    var H = 206, top = 22, bottom = 24, plotH = H - top - bottom;
    function y(v) { return top + (1 - v / 100) * plotH; }
    var root = svg("svg", { viewBox: "0 0 " + g.width + " " + H, role: "img", "aria-label": translate("detail_aria", row.name) });
    var defs = hatchPattern(root);
    var fade = svg("linearGradient", { id: "chart-fade", x1: 0, x2: 0, y1: 0, y2: 1 });
    fade.appendChild(svg("stop", { offset: "0%", "stop-color": cssVar("--text-secondary"), "stop-opacity": 0.18 }));
    fade.appendChild(svg("stop", { offset: "100%", "stop-color": cssVar("--text-secondary"), "stop-opacity": 0 }));
    defs.appendChild(fade);
    [0, 50, 100].forEach(function (v) {
      root.appendChild(svg("line", { x1: g.labelW, x2: g.labelW + g.plotW, y1: y(v), y2: y(v), stroke: cssVar(v ? "--border-color" : "--border-primary"), "stroke-dasharray": v ? "2 4" : "none" }));
      root.appendChild(svgText({ x: g.labelW - 8, y: y(v) + 4, "text-anchor": "end", "font-size": 11, fill: cssVar("--text-tertiary") }, v + "%"));
    });
    var low = status.config ? Number(status.config.low_threshold) : 0;
    if (low > 0 && low < 100) {
      root.appendChild(svg("line", { x1: g.labelW, x2: g.labelW + g.plotW, y1: y(low), y2: y(low), stroke: cssVar("--viz-failure"), "stroke-opacity": 0.5, "stroke-dasharray": "4 4" }));
      root.appendChild(svgText({ x: g.labelW - 8, y: y(low) + 4, "text-anchor": "end", "font-size": 11, fill: cssVar("--viz-failure") }, low + "%"));
    }
    var ticks = chartTicks(g);
    ticks.list.forEach(function (t) {
      root.appendChild(svg("line", { x1: g.x(t), x2: g.x(t), y1: top, y2: top + plotH, stroke: cssVar("--border-color"), "stroke-dasharray": "2 4" }));
      root.appendChild(svgText({ x: g.x(t), y: H - 6, "text-anchor": "middle", "font-size": 11, fill: cssVar("--text-tertiary") }, ticks.label(t)));
    });

    var stretches = runs(row.points, limit, g.to), cursor = g.from;
    stretches.forEach(function (run) {
      if (run.start > cursor + limit) root.appendChild(svg("rect", { x: g.x(cursor), y: top, width: g.x(run.start) - g.x(cursor), height: plotH, fill: "url(#chart-hatch)" }));
      cursor = run.end;
    });
    if (cursor < g.to - limit) root.appendChild(svg("rect", { x: g.x(cursor), y: top, width: g.x(g.to) - g.x(cursor), height: plotH, fill: "url(#chart-hatch)" }));
    if (stretches.length && stretches[0].start > g.from + limit) {
      root.appendChild(svgText({ x: (g.x(g.from) + g.x(stretches[0].start)) / 2, y: top + plotH / 2 + 4, "text-anchor": "middle", "font-size": 12,
        fill: cssVar("--text-tertiary"), "class": "row-name" }, translate("no_data")));
    }

    var resets = [];
    stretches.forEach(function (run) {
      // Hold the last value to the end of the stretch so the line reaches it.
      var pts = run.pts.slice(), tail = pts[pts.length - 1];
      if (run.end > tail[0]) pts.push([run.end].concat(tail.slice(1)));
      // One account resetting raises a pooled total by its share; the total
      // line breaks only when that share is a visible jump.
      var split = splitAtResets(pts, [1]);
      resets = resets.concat(split.resets);
      if (group) splitAtResets(pts, [3, 4]).parts.forEach(function (part) {
        if (part.length < 2) return;
        var upper = curvePoints(declining(part, 4), 4).map(function (p) { return [g.x(p[0]), y(p[4])]; });
        var lower = curvePoints(declining(part, 3), 3).map(function (p) { return [g.x(p[0]), y(p[3])]; }).reverse();
        root.appendChild(svg("path", { d: monotonePath(upper) + monotonePath(lower).replace(/^M/, "L") + "Z", fill: cssVar("--text-secondary"), "fill-opacity": 0.13 }));
      });
      // Each piece starts at its reset sample, so the return to full is a
      // break in the line rather than a slope that never happened.
      split.parts.forEach(function (part) {
        var xy = curvePoints(declining(part, 1), 1).map(function (p) { return [g.x(p[0]), y(p[1])]; });
        if (xy.length === 1) xy.push([xy[0][0] + 1, xy[0][1]]);
        var d = monotonePath(xy);
        if (!group) root.appendChild(svg("path", { d: d + "L" + xy[xy.length - 1][0] + " " + y(0) + "L" + xy[0][0] + " " + y(0) + "Z", fill: "url(#chart-fade)" }));
        root.appendChild(svg("path", { d: d, fill: "none", stroke: cssVar("--text-primary"), "stroke-width": 2, "stroke-linejoin": "round", "stroke-linecap": "round" }));
      });
    });
    var lastLabel = -99;
    resets.forEach(function (t) {
      var x = g.x(t);
      root.appendChild(svg("line", { x1: x, x2: x, y1: top - 4, y2: top + plotH, stroke: cssVar("--text-tertiary"), "stroke-opacity": 0.7, "stroke-dasharray": "3 3" }));
      if (x - lastLabel > 52) {
        root.appendChild(svgText({ x: x, y: top - 8, "text-anchor": "middle", "font-size": 11, fill: cssVar("--text-tertiary") }, "↻ " + fmtClock(unixDate(t))));
        lastLabel = x;
      }
    });
    var groups = group ? row.groups : [row.group];
    ignitionEvents().forEach(function (ev) {
      if (groups.indexOf(ev.group) < 0) return;
      var x = g.x(ev.t), base = top + plotH;
      if (ev.failed) root.appendChild(svg("path", { d: "M" + (x - 4) + " " + (base - 8) + "L" + (x + 4) + " " + base + "M" + (x + 4) + " " + (base - 8) + "L" + (x - 4) + " " + base,
        stroke: cssVar("--viz-failure"), "stroke-width": 2.2, "stroke-linecap": "round" }));
      else root.appendChild(svg("path", { d: "M" + x + " " + (base - 7) + "L" + (x + 4) + " " + base + "L" + (x - 4) + " " + base + "Z", fill: cssVar("--viz-success") }));
    });
    var last = row.points[row.points.length - 1];
    var lastRun = stretches[stretches.length - 1];
    if (last && lastRun.end >= g.to - limit) {
      root.appendChild(svg("circle", { cx: g.x(lastRun.end), cy: y(last[1]), r: 4, fill: cssVar("--text-primary"), stroke: cssVar("--bg-primary"), "stroke-width": 2 }));
    }

    var cross = svg("line", { y1: top, y2: top + plotH, stroke: cssVar("--text-tertiary"), visibility: "hidden", "pointer-events": "none" });
    root.appendChild(cross);
    var dot = svg("circle", { r: 4, fill: cssVar("--text-primary"), stroke: cssVar("--bg-primary"), "stroke-width": 2, visibility: "hidden", "pointer-events": "none" });
    root.appendChild(dot);
    var hit = svg("rect", { x: g.labelW, y: top, width: g.plotW, height: plotH, fill: "transparent", tabindex: "0",
      "aria-label": translate("detail_keys") });
    hit.addEventListener("pointermove", function (e) { hoverAt(e, root, row.id, "detail"); });
    root.appendChild(hit);
    root.addEventListener("pointerleave", function () { setHover(null); });
    box.appendChild(root);
    var tip = el("div", { class: "tooltip", hidden: true });
    box.appendChild(tip);

    // Keyboard: step through this row's samples.
    var keyIndex = -1;
    function showSample(i) {
      if (i < 0 || i >= row.points.length) return;
      keyIndex = i;
      hoverSource = "detail";
      setHover(row.points[i][0], row.id);
    }
    hit.addEventListener("focus", function () { showSample(keyIndex < 0 ? row.points.length - 1 : keyIndex); });
    hit.addEventListener("blur", function () { setHover(null); });
    hit.addEventListener("keydown", function (e) {
      if (e.key === "ArrowLeft") { showSample(Math.max(0, keyIndex - 1)); e.preventDefault(); }
      if (e.key === "ArrowRight") { showSample(Math.min(row.points.length - 1, keyIndex + 1)); e.preventDefault(); }
    });

    hoverListeners.push(function (t) {
      if (t == null) { cross.setAttribute("visibility", "hidden"); dot.setAttribute("visibility", "hidden"); tip.hidden = true; return; }
      cross.setAttribute("x1", g.x(t)); cross.setAttribute("x2", g.x(t)); cross.setAttribute("visibility", "visible");
      var p = valueAt(row.points, t);
      if (p && t - p[0] > limit) p = null;
      if (p) { dot.setAttribute("cx", g.x(p[0])); dot.setAttribute("cy", y(p[1])); dot.setAttribute("visibility", "visible"); }
      else dot.setAttribute("visibility", "hidden");
      if (hoverSource !== "detail") { tip.hidden = true; return; }
      clear(tip);
      tip.appendChild(el("div", { class: "time", text: fmtDateTime(unixDate(t)) }));
      if (group) {
        tipRow(tip, translate("total"), p ? p[1] : null, true);
        row.children.slice(0, 6).forEach(function (c) {
          var q = valueAt(c.points, t);
          tipRow(tip, "　" + c.sub, q && t - q[0] <= limit ? q[1] : null, false);
        });
        if (row.children.length > 6) tip.appendChild(el("div", { class: "row muted", text: translate("more_accounts", row.children.length - 6) }));
      } else {
        tipRow(tip, row.name + (p ? translate(p[2] === 1 ? "passive_sample" : "active_sample") : ""), p ? p[1] : null, true);
      }
      var near = (g.to - g.from) / g.plotW * 8;
      resets.forEach(function (r) { if (Math.abs(r - t) <= near) tip.appendChild(el("div", { class: "note", text: fmtClock(unixDate(r)) + " " + translate("quota_reset") })); });
      tip.hidden = false;
      placeTip(tip, box, g, t, 6);
    });
  }

  function hoverAt(e, root, rowId, source) {
    var rect = root.getBoundingClientRect();
    var t = chartView.t((e.clientX - rect.left) / rect.width * chartView.width);
    if (t < chartView.from || t > chartView.to) { setHover(null); return; }
    hoverSource = source;
    setHover(snapTime(t), rowId);
  }

  // The nearest sample of the detail row, so the crosshair and tooltip show a
  // measured time. Inside a data gap the pointer's own time is kept.
  function snapTime(t) {
    var best = null;
    snapPoints.forEach(function (p) {
      if (p[0] >= chartView.from && p[0] <= chartView.to && (best == null || Math.abs(p[0] - t) < Math.abs(best - t))) best = p[0];
    });
    return best != null && Math.abs(best - t) <= gapLimit() ? best : t;
  }

  function setHover(t, rowId) {
    hoverListeners.forEach(function (fn) { fn(t, rowId); });
  }

  function renderChartTable(rows) {
    var table = $("chart-table");
    clear(table);
    rows.forEach(function (r) {
      (r.children || [r]).forEach(function (c) {
        var values = c.points.map(function (p) { return p[1]; });
        table.appendChild(el("tr", {}, [
          el("td", { text: c.name }),
          el("td", { text: c.sub }),
          el("td", { class: "num", text: pct(c.last) }),
          el("td", { class: "num", text: pct(Math.min.apply(null, values)) }),
          el("td", { class: "num", text: pct(Math.max.apply(null, values)) }),
          el("td", { class: "num", text: String(values.length) }),
          el("td", { class: "num", text: c.reset ? fmtCountdown(c.reset * 1000 - Date.now()) : "—" })
        ]));
      });
    });
  }

  function renderChart() {
    var overview = $("chart"), head = $("chart-detail-head"), detail = $("chart-detail");
    overview.classList.remove("loading");
    clear(overview); clear(head); clear(detail);
    clear($("chart-table"));
    hoverListeners = [];
    snapPoints = [];
    head.hidden = true;
    if (!history || !status) {
      overview.appendChild(el("div", { class: "empty", text: translate("loading") }));
      return;
    }
    var rows = chartRows();
    if (!rows.length) {
      overview.appendChild(el("div", { class: "empty", text: translate("no_history") }));
      return;
    }
    if (!findRow(rows, chartSelected)) chartSelected = rows[0].id;
    renderOverview(rows, overview);
    head.hidden = false;
    renderDetail(findRow(rows, chartSelected), head, detail);
    renderChartTable(rows);
    var t = quotaThresholds();
    $("legend-high").textContent = "> " + t.notice + "%";
    $("legend-medium").textContent = t.low + "–" + t.notice + "%";
    $("legend-low").textContent = "≤ " + t.low + "%";
  }

  function renderRangeButtons() {
    var box = $("range-buttons");
    clear(box);
    RANGES[chartWindow].forEach(function (r) {
      box.appendChild(el("button", { type: "button", class: "seg", "aria-pressed": r[0] === range ? "true" : "false", text: translate("range_" + r[2], r[1]), onclick: function () {
        range = r[0];
        renderRangeButtons();
        loadHistory();
      } }));
    });
  }

  function loadHistory() {
    $("chart").classList.add("loading");
    return api("GET", API + "/history?range=" + range).then(function (data) {
      history = data;
      renderChart();
    }).catch(function (err) {
      if (err.message !== "unauthorized") toast(translate("history_failed", err.message));
      $("chart").classList.remove("loading");
    });
  }

  // ---- Settings ----

  function getPath(obj, path) {
    return path.split(".").reduce(function (value, part) { return value && typeof value === "object" ? value[part] : undefined; }, obj);
  }

  function setPath(obj, path, value) {
    var parts = path.split(".");
    var target = obj;
    for (var i = 0; i < parts.length - 1; i++) {
      if (!target[parts[i]] || typeof target[parts[i]] !== "object") target[parts[i]] = {};
      target = target[parts[i]];
    }
    target[parts[parts.length - 1]] = value;
  }

  // Antigravity quota groups offered as checkboxes: the groups seen on the
  // accounts plus any configured ones, in a stable order.
  function antigravityGroups(selected) {
    var names = ["Gemini", "Claude / GPT"];
    (status.accounts || []).forEach(function (a) {
      if (a.provider !== "antigravity") return;
      (a.groups || []).forEach(function (g) { if (names.indexOf(g.source_label) < 0) names.push(g.source_label); });
    });
    (selected || []).forEach(function (name) { if (names.indexOf(name) < 0) names.push(name); });
    return names;
  }

  function renderGroupChecks(selected) {
    var box = $("ag-groups");
    clear(box);
    antigravityGroups(selected).forEach(function (name) {
      var input = el("input", { type: "checkbox", "data-ag-group": name });
      input.checked = (selected || []).some(function (s) { return s.toLowerCase() === name.toLowerCase(); });
      box.appendChild(el("label", { class: "inline" }, [input, document.createTextNode(name)]));
    });
  }

  function timezoneText() {
    var offset = offsetSeconds() / 3600;
    var name = status && status.timezone && status.timezone !== "Local" ? status.timezone : "";
    var sign = offset >= 0 ? "+" : "-";
    var utc = "UTC" + sign + Math.abs(offset);
    var source = rawConfig.timezone || rawConfig.timezone_offset_hours !== undefined ? translate("tz_config") : translate("tz_cpa");
    return translate("tz_note", name ? name + " (" + utc + ")" : utc, source);
  }

  function topKey(name) { return name.split(".")[0]; }

  // Effective values come from the status config (defaults applied); saved
  // secrets come from the raw plugin config.
  function effectiveConfig() {
    var effective = JSON.parse(JSON.stringify(status.config || {}));
    effective.bark_url = rawConfig.bark_url || "";
    effective.models_api_key = rawConfig.models_api_key || "";
    return effective;
  }

  function fillInput(input, effective) {
    var value = getPath(effective, input.name);
    if (input.type === "checkbox") input.checked = !!value;
    else if (input.type === "radio") input.checked = input.value === value;
    else input.value = value === undefined || value === null ? "" : value;
  }

  // keys, when given, limits the refill to those top-level keys and skips the
  // focused field, so a refill after a save leaves the field being edited alone.
  function fillSettings(keys) {
    var effective = effectiveConfig();
    var active = document.activeElement;
    Array.prototype.forEach.call($("settings").elements, function (input) {
      if (!input.name) return;
      if (keys && (!keys[topKey(input.name)] || input === active)) return;
      fillInput(input, effective);
    });
    Array.prototype.forEach.call(document.querySelectorAll(".mode-switch"), function (box) { placeThumb(box, false); });
    if (!keys || (keys.providers && !$("ag-groups").contains(active))) {
      renderGroupChecks(getPath(effective, "providers.antigravity.groups"));
    }
    $("timezone-note").textContent = timezoneText();
  }

  // Mode switches: the thumb slides under the checked option. bounce plays
  // the squash animation; filling the form places it without one.
  function placeThumb(box, bounce) {
    var options = box.querySelectorAll("input");
    var index = 0;
    Array.prototype.forEach.call(options, function (input, i) { if (input.checked) index = i; });
    box.style.setProperty("--index", index);
    if (!bounce) return;
    var thumb = box.querySelector(".mode-thumb");
    thumb.classList.remove("bounce");
    void thumb.offsetWidth; // restart the animation
    thumb.classList.add("bounce");
  }

  Array.prototype.forEach.call(document.querySelectorAll(".mode-switch"), function (box) {
    box.addEventListener("change", function () { placeThumb(box, true); });
  });

  // The patch for one top-level key. CPA merges top-level keys only, so
  // objects are sent whole, starting from the saved object to keep keys the
  // page does not show.
  function buildPatch(top) {
    var patch = {};
    var base = rawConfig[top];
    if (top === "ignition" && !base) base = status.config && status.config.ignition;
    if (base && typeof base === "object") patch[top] = JSON.parse(JSON.stringify(base));
    Array.prototype.forEach.call($("settings").elements, function (input) {
      if (!input.name || topKey(input.name) !== top) return;
      var value;
      if (input.type === "checkbox") value = input.checked;
      else if (input.type === "radio") {
        if (!input.checked) return;
        value = input.value;
      } else if (input.type === "number") {
        if (input.value === "") return;
        value = Number(input.value);
      } else value = input.value.trim();
      setPath(patch, input.name, value);
    });
    if (top === "providers") {
      PROVIDERS.forEach(function (p) { if (!patch.providers[p]) patch.providers[p] = {}; });
      patch.providers.antigravity.groups = Array.prototype.filter.call(
        document.querySelectorAll("[data-ag-group]"), function (input) { return input.checked; }
      ).map(function (input) { return input.getAttribute("data-ag-group"); });
    }
    // A fixed offset from older versions is removed so the time zone follows CPA.
    if (rawConfig.timezone_offset_hours !== undefined) patch.timezone_offset_hours = null;
    // The single switches are replaced by recovery_notify and reset_reminder.
    if (top === "recovery_notify" && rawConfig.notify_recovery !== undefined) patch.notify_recovery = null;
    if (top === "reset_reminder" && rawConfig.notify_reset_reminders !== undefined) patch.notify_reset_reminders = null;
    return patch;
  }

  // Settings save as they change: switches and checkboxes when clicked, text
  // and number fields when they lose focus or on Enter. Saves run one at a
  // time, so each patch starts from the config the previous one wrote. A few
  // seconds after the last save, when the plugin has applied it, the saved
  // fields show the effective values again.
  var saveQueue = Promise.resolve();
  var refillKeys = {};
  var refillTimer = null;

  function saveField(event) {
    var input = event.target;
    var top = input.hasAttribute("data-ag-group") ? "providers" : input.name && topKey(input.name);
    if (!top) return;
    if (input.type === "number") {
      if (!input.checkValidity()) { input.reportValidity(); return; }
      if (input.value === "") { fillInput(input, effectiveConfig()); return; }
    }
    saveQueue = saveQueue.then(function () {
      return api("PATCH", CONFIG_API, buildPatch(top)).then(function () {
        toast(translate("saved"));
        refillKeys[top] = true;
        clearTimeout(refillTimer);
        refillTimer = setTimeout(refillSaved, 3000);
        return loadConfig();
      }).catch(function (err) {
        if (err.message !== "unauthorized") toast(translate("save_failed", err.message));
        var keys = {};
        keys[top] = true;
        fillSettings(keys);
      });
    });
  }

  function refillSaved() {
    saveQueue = saveQueue.then(function () {
      var keys = refillKeys;
      refillKeys = {};
      return loadStatus().then(function () { fillSettings(keys); }).catch(function () {});
    });
  }

  function loadConfig() {
    return api("GET", CONFIG_API).then(function (data) { rawConfig = data || {}; });
  }

  // ---- Startup ----

  function start() {
    $("key-section").hidden = true;
    $("app").hidden = false;
    Promise.all([loadStatus(), loadConfig()]).then(function () {
      fillSettings();
      return loadHistory();
    }).catch(function (err) {
      if (err.message !== "unauthorized") toast(translate("load_failed", err.message));
    });
    clearInterval(refreshTimer);
    refreshTimer = setInterval(function () {
      if (document.hidden) return;
      loadStatus().catch(function () {});
    }, 30000);
  }

  $("key-form").addEventListener("submit", function (e) {
    e.preventDefault();
    key = $("key-input").value.trim();
    if (!key) return;
    sessionStorage.setItem(KEY_STORE, key);
    start();
  });
  $("refresh-all").addEventListener("click", function (e) {
    runAction(e.currentTarget, API + "/refresh", {}, translate("refreshed_all"));
  });
  $("test-bark").addEventListener("click", function (e) {
    var button = e.currentTarget;
    saveQueue.then(function () { runAction(button, API + "/test-bark", {}, translate("test_sent")); });
  });
  $("settings").addEventListener("change", saveField);
  $("settings").addEventListener("submit", function (e) { e.preventDefault(); });
  // Enter commits a text or number field; leaving it fires the change that saves it.
  $("settings").addEventListener("keydown", function (e) {
    if (e.key === "Enter" && e.target.tagName === "INPUT" && /^(text|number|password)$/.test(e.target.type)) {
      e.preventDefault();
      e.target.blur();
    }
  });
  $("events-more").addEventListener("click", function () { eventsExpanded = !eventsExpanded; renderEvents(); });
  function bindSegmented(id, onPick) {
    Array.prototype.forEach.call($(id).querySelectorAll("button"), function (button) {
      button.addEventListener("click", function () {
        Array.prototype.forEach.call($(id).querySelectorAll("button"), function (b) {
          b.setAttribute("aria-pressed", b === button ? "true" : "false");
        });
        onPick(button.getAttribute("data-value"));
      });
    });
  }
  bindSegmented("window-buttons", function (value) {
    chartWindow = value;
    range = DEFAULT_RANGE[value];
    renderRangeButtons();
    loadHistory();
  });
  bindSegmented("sort-buttons", function (value) { chartSort = value; renderChart(); });
  renderRangeButtons();
  window.addEventListener("resize", function () { if (history) renderChart(); });
  window.addEventListener("storage", function (e) {
    if (e.key === "cli-proxy-theme") { applyTheme(); renderChart(); }
    if (e.key === "cli-proxy-language") applyLanguage();
  });
  if (window.matchMedia) {
    var media = window.matchMedia("(prefers-color-scheme: dark)");
    if (media.addEventListener) media.addEventListener("change", function () { applyTheme(); renderChart(); });
  }

  applyTheme();
  applyLanguage();
  watchParent();
  key = sessionStorage.getItem(KEY_STORE) || centerKey();
  if (key) start(); else askForKey("");
})();
