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

  function watchParentTheme() {
    var root = parentRoot();
    if (!root || !window.MutationObserver) return;
    new MutationObserver(function () { applyTheme(); renderChart(); })
      .observe(root, { attributes: true, attributeFilter: ["data-theme"] });
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
          askForKey("管理密钥无效，请重新输入。");
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
    if (ms <= 0) return "已到";
    var minutes = Math.round(ms / 60000);
    if (minutes < 60) return minutes + " 分钟";
    var hours = Math.floor(minutes / 60);
    if (hours < 48) return hours + " 小时 " + (minutes % 60) + " 分";
    return Math.round(hours / 24) + " 天";
  }

  function fmtIn(t) {
    if (!t) return "";
    var ms = t.getTime() - Date.now();
    if (ms < 60000) return "即将";
    return fmtDuration(ms) + "后";
  }

  function fmtAgo(t) {
    if (!t) return "";
    var ms = Date.now() - t.getTime();
    if (ms < 60000) return "刚刚";
    return fmtDuration(ms) + "前";
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
    if (ms <= 0) return "已重置";
    var minutes = Math.round(ms / 60000);
    if (minutes < 60) return minutes + "m";
    var hours = Math.floor(minutes / 60);
    if (hours < 48) return hours + "h " + pad(minutes % 60) + "m";
    return Math.floor(hours / 24) + "d " + (hours % 24) + "h";
  }

  function windowName(w) {
    if (w.short === "5h") return "5 小时";
    if (w.short === "7d") return "7 天";
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
    if (!status.enabled) state.appendChild(badge("muted", "已停用"));
    else if (!status.running) state.appendChild(badge("warning", "启动中"));
    else state.appendChild(badge("success", "运行中"));
    var parts = ["v" + status.version];
    var last = parseTime(status.last_poll), next = parseTime(status.next_poll);
    if (last) parts.push("上次查询 " + fmtClock(last) + "（" + fmtAgo(last) + "）");
    if (next) parts.push("下次 " + fmtClock(next) + "（" + fmtIn(next) + "）");
    $("meta").textContent = parts.join(" · ");
  }

  function notice(text) {
    return el("div", { class: "notice" }, [el("span", { class: "notice-icon", text: "!" }), el("span", { text: text })]);
  }

  function renderNotices() {
    var box = $("notices");
    clear(box);
    if (status.config_error) box.appendChild(notice("配置无法解析，仍在使用上一份配置：" + status.config_error));
    if (status.lock_error) box.appendChild(notice(status.lock_error));
    if (status.list_error) box.appendChild(notice(status.list_error));
    if (status.models_error) box.appendChild(notice("模型列表不可用：" + status.models_error));
    if (status.config && !status.config.bark_url) box.appendChild(notice("未设置 Bark 推送地址，通知不会发送。"));
    if (status.config && status.config.ignition && status.config.ignition.enabled && !status.config.models_api_key) {
      box.appendChild(notice("未设置模型列表 API key，自动点火无法选择模型。"));
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
        "aria-valuenow": String(Math.round(remaining)), "aria-label": group.label + " " + windowName(w) + " 剩余" }, [
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
      el("span", { class: "source source-" + source, text: source === "passive" ? "被动" : "主动" }),
      el("span", { text: "更新于 " + fmtAgo(latest) })
    ]);
  }

  function renderAccounts() {
    var box = $("accounts");
    clear(box);
    if (!status.accounts.length) {
      box.appendChild(el("p", { class: "empty", text: status.running ? "没有可监控的 ChatGPT、Claude 或 Antigravity 凭证。" : "CPA 启动约 20 秒后开始第一次查询。" }));
      return;
    }
    var meterIndex = 0;
    status.accounts.forEach(function (account) {
      var card = el("div", { class: "card account" });
      var refresh = el("button", { type: "button", class: "btn btn-ghost btn-sm", text: "刷新", onclick: function () {
        runAction(refresh, API + "/refresh", { auth_index: account.auth_index }, "已刷新 " + account.title);
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
        card.appendChild(el("div", { class: "error-text", text: "额度已恢复，但 CPA 仍冷却到 " + fmtDateTime(cooldown) +
          "，期间的请求和点火都会被拒绝；在 CPA 管理中心清除这个账号的冷却。" }));
      } else if (cooldown) {
        card.appendChild(el("div", { class: "cell-note", text: "CPA 冷却到 " + fmtDateTime(cooldown) + "（" + fmtIn(cooldown) + "）" }));
      }
      if (account.error) card.appendChild(el("div", { class: "error-text", text: account.error }));
      box.appendChild(card);
    });
  }

  function targetStatus(t) {
    if (t.blocked_window) {
      var until = parseTime(t.blocked_until);
      return { kind: "muted", text: windowName({ short: t.blocked_window, label: t.blocked_window }) + "额度已用完",
        title: until ? fmtDateTime(until) + " 重置后恢复点火" : "额度恢复后恢复点火" };
    }
    var circuit = parseTime(t.circuit_until);
    if (circuit) return { kind: "failure", text: "暂停至 " + fmtDateTime(circuit), title: t.circuit_reason };
    if (t.consecutive_failures > 0) return { kind: "warning", text: "失败 " + t.consecutive_failures + " 次", title: t.last_error };
    if (t.rolling) return { kind: "muted", text: "窗口未开始" };
    return { kind: "success", text: "正常" };
  }

  function renderTargets() {
    var body = $("targets");
    clear(body);
    var ignition = (status.config && status.config.ignition) || {};
    var endMinutes = (ignition.end_hour || 0) * 60 + (ignition.end_grace_minutes || 0);
    $("ignition-note").textContent = ignition.enabled
      ? "每天 " + pad(ignition.start_hour) + ":00 开始，之后在每次重置后 " + ignition.grace_seconds + " 秒点火，最晚到 " +
        pad(Math.floor(endMinutes / 60) % 24) + ":" + pad(endMinutes % 60) + "。"
      : "自动点火已关闭。";
    if (!status.targets.length) {
      body.appendChild(el("tr", {}, [el("td", { colspan: "7", class: "empty", text: "没有启用点火的额度组。" })]));
      return;
    }
    status.targets.forEach(function (t) {
      var st = targetStatus(t);
      var button = el("button", { type: "button", class: "btn btn-secondary btn-sm", text: "立即点火", onclick: function () {
        if (!confirm("立即向 " + t.label + " 发送一次点火请求？这会开始一个新的 5 小时窗口。")) return;
        runAction(button, API + "/ignite", { target: t.key }, t.label + " 点火成功");
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

  var EVENT_TYPES = {
    ignite: ["success", "点火成功"],
    ignite_manual: ["success", "手动点火"],
    ignite_failed: ["warning", "点火失败"],
    ignite_paused: ["failure", "点火暂停"],
    cooldown_stale: ["warning", "冷却未解除"],
    notify: ["muted", "已推送"],
    notify_failed: ["warning", "推送失败"],
    codex_reset: ["muted", "重置信号"],
    error: ["failure", "错误"]
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

  // Events written before label and detail existed only have a message that
  // starts with the group label.
  function eventParts(ev) {
    var label = ev.label || (ev.group ? groupLabel(ev.group) : "");
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
      list.appendChild(el("p", { class: "empty", text: "暂无事件" }));
      more.hidden = true;
      return;
    }
    var events = eventsExpanded ? status.events.slice(0, 100) : status.events.slice(0, EVENTS_COLLAPSED);
    events.forEach(function (ev) {
      var type = EVENT_TYPES[ev.kind] || [ev.level === "error" ? "failure" : ev.level === "warn" ? "warning" : "muted", ev.kind];
      var parts = eventParts(ev);
      list.appendChild(el("div", { class: "event-row", role: "row" }, [
        el("span", { class: "event-time", role: "cell", text: fmtDateTime(parseTime(ev.time)) }),
        el("span", { class: "event-type", role: "cell" }, [badge(type[0], type[1])]),
        el("span", { class: "event-label", role: "cell", text: parts.label }),
        el("span", { class: "event-detail", role: "cell", title: parts.detail, text: parts.detail })
      ]));
    });
    more.hidden = status.events.length <= EVENTS_COLLAPSED;
    more.textContent = eventsExpanded ? "收起" : "显示更多（共 " + Math.min(status.events.length, 100) + " 条）";
  }

  function renderStatus() {
    renderHeader();
    renderNotices();
    renderAccounts();
    renderTargets();
    renderEvents();
  }

  function loadStatus() {
    return api("GET", API + "/status").then(function (data) {
      status = data;
      renderStatus();
    });
  }

  function runAction(button, path, body, success) {
    button.disabled = true;
    return api("POST", path, body).then(function (data) {
      if (data.status) { status = data.status; renderStatus(); }
      toast(data.ok ? success : ("失败：" + data.error));
      loadHistory();
    }).catch(function (err) {
      if (err.message !== "unauthorized") toast("失败：" + err.message);
    }).then(function () { button.disabled = false; });
  }

  // ---- Chart ----

  // Time ranges follow the quota window: a 5-hour quota is read over hours,
  // a 7-day quota over days. Keys are the history endpoint's range values.
  var RANGES = {
    "5h": [["1h", "1 小时"], ["3h", "3 小时"], ["6h", "6 小时"], ["12h", "12 小时"], ["24h", "24 小时"], ["26h", "26 小时"]],
    "7d": [["24h", "1 天"], ["4d", "4 天"], ["8d", "8 天"], ["15d", "15 天"], ["1mo", "1 个月"], ["36d", "36 天"]]
  };
  // The default ranges are the shortest that cover one whole window.
  var DEFAULT_RANGE = { "5h": "6h", "7d": "8d" };
  // Default row order by quota name; other quotas follow in API order.
  var SERVICE_ORDER = ["Claude", "ChatGPT", "Gemini", "Fable", "Claude / GPT"];
  var LANE = 22, LANE_GAP = 6, AXIS_H = 22, EVENT_H = 24;
  // A rise of this many points between two samples is a reset.
  var RESET_JUMP = 5;
  // Quotas report whole percents; steps this small are a steady decline.
  var QUANTUM = 2;

  var chartView = null;
  var hoverSource = "";
  var hoverListeners = [];

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
  // drawn as missing data rather than as a held value.
  function gapLimit() {
    var poll = status && status.config ? Number(status.config.poll_interval_seconds) || 300 : 300;
    return Math.max(history.bucket_seconds || 0, poll) * 3.5;
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
      return { t: new Date(ev.time).getTime() / 1000, failed: ev.kind === "ignite_failed" || ev.kind === "ignite_paused", message: ev.message, group: ev.group };
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
      el("span", { class: "value", text: value != null ? pct(value) : "无数据" })
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
    var root = svg("svg", { viewBox: "0 0 " + g.width + " " + height, role: "img", "aria-label": "各额度剩余百分比总览" });
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
    root.appendChild(svgText({ x: 4, y: ey + 13, "font-size": 12, fill: cssVar("--text-tertiary"), "class": "row-name" }, "点火"));
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
      (events.length - failed) + " 成功" + (failed ? " · " + failed + " 失败" : "")));
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
        tipRow(tip, r.name + (r.children ? " 合计" : ""), at(r), r.id === rowId);
        if (r.children && !chartExpanded[r.id]) {
          r.children.slice(0, 5).forEach(function (c) { tipRow(tip, "　" + c.sub, at(c), false); });
          if (r.children.length > 5) tip.appendChild(el("div", { class: "row muted", text: "　… 另 " + (r.children.length - 5) + " 个账号" }));
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
    return { parts: parts, resets: resets };
  }

  // Detail: the selected row as a smoothed line that breaks at resets. A
  // pooled row adds a band from its lowest to its highest account.
  function renderDetail(row, head, box) {
    var g = chartView, limit = gapLimit();
    var group = !!row.children;
    var lowest = Math.min.apply(null, row.points.map(function (p) { return group ? p[3] : p[1]; }));
    head.appendChild(el("span", { class: "detail-name", text: row.name + " · " + (chartWindow === "5h" ? "5 小时额度" : "7 天额度") }));
    head.appendChild(el("span", { class: "muted", text: group ? row.children.length + " 个账号合计，阴影为账号间的最低到最高" : row.sub }));
    [[group ? "合计" : "当前", pct(row.last)], [group ? "单账号最低" : "最低", pct(lowest)],
      ["距重置", row.reset ? fmtCountdown(row.reset * 1000 - Date.now()) : "—"]].forEach(function (stat) {
      head.appendChild(el("span", { class: "detail-stat" }, [document.createTextNode(stat[0]), el("strong", { text: stat[1] })]));
    });

    var H = 206, top = 22, bottom = 24, plotH = H - top - bottom;
    function y(v) { return top + (1 - v / 100) * plotH; }
    var root = svg("svg", { viewBox: "0 0 " + g.width + " " + H, role: "img", "aria-label": row.name + " 剩余百分比" });
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
        fill: cssVar("--text-tertiary"), "class": "row-name" }, "无数据"));
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
      "aria-label": "按左右方向键查看各时刻的数值" });
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
        tipRow(tip, "合计", p ? p[1] : null, true);
        row.children.slice(0, 6).forEach(function (c) {
          var q = valueAt(c.points, t);
          tipRow(tip, "　" + c.sub, q && t - q[0] <= limit ? q[1] : null, false);
        });
        if (row.children.length > 6) tip.appendChild(el("div", { class: "row muted", text: "　… 另 " + (row.children.length - 6) + " 个账号" }));
      } else {
        tipRow(tip, row.name + (p ? (p[2] === 1 ? "（被动）" : "（主动）") : ""), p ? p[1] : null, true);
      }
      var near = (g.to - g.from) / g.plotW * 8;
      resets.forEach(function (r) { if (Math.abs(r - t) <= near) tip.appendChild(el("div", { class: "note", text: fmtClock(unixDate(r)) + " 额度重置" })); });
      tip.hidden = false;
      placeTip(tip, box, g, t, 6);
    });
  }

  function hoverAt(e, root, rowId, source) {
    var rect = root.getBoundingClientRect();
    var t = chartView.t((e.clientX - rect.left) / rect.width * chartView.width);
    if (t < chartView.from || t > chartView.to) { setHover(null); return; }
    hoverSource = source;
    setHover(t, rowId);
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
    head.hidden = true;
    if (!history || !status) {
      overview.appendChild(el("div", { class: "empty", text: "加载中…" }));
      return;
    }
    var rows = chartRows();
    if (!rows.length) {
      overview.appendChild(el("div", { class: "empty", text: "这个范围内还没有额度数据。" }));
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
      box.appendChild(el("button", { type: "button", class: "seg", "aria-pressed": r[0] === range ? "true" : "false", text: r[1], onclick: function () {
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
      if (err.message !== "unauthorized") toast("读取历史失败：" + err.message);
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
    var source = rawConfig.timezone || rawConfig.timezone_offset_hours !== undefined ? "配置中指定" : "跟随 CPA 服务器";
    return "通知和页面中的时间使用 " + (name ? name + "（" + utc + "）" : utc) + "，" + source + "。";
  }

  // Effective values come from the status config (defaults applied); saved
  // secrets come from the raw plugin config.
  function fillSettings() {
    var form = $("settings");
    var effective = JSON.parse(JSON.stringify(status.config || {}));
    effective.bark_url = rawConfig.bark_url || "";
    effective.models_api_key = rawConfig.models_api_key || "";
    Array.prototype.forEach.call(form.elements, function (input) {
      if (!input.name) return;
      var value = getPath(effective, input.name);
      if (input.type === "checkbox") input.checked = !!value;
      else input.value = value === undefined || value === null ? "" : value;
    });
    renderGroupChecks(getPath(effective, "providers.antigravity.groups"));
    $("timezone-note").textContent = timezoneText();
  }

  function saveSettings(event) {
    event.preventDefault();
    var form = $("settings");
    var patch = {
      ignition: JSON.parse(JSON.stringify(rawConfig.ignition || (status.config && status.config.ignition) || {})),
      providers: JSON.parse(JSON.stringify(rawConfig.providers || {})),
      codex_reset_updates: JSON.parse(JSON.stringify(rawConfig.codex_reset_updates || {}))
    };
    PROVIDERS.forEach(function (p) { if (!patch.providers[p]) patch.providers[p] = {}; });
    Array.prototype.forEach.call(form.elements, function (input) {
      if (!input.name) return;
      var value;
      if (input.type === "checkbox") value = input.checked;
      else if (input.type === "number") {
        if (input.value === "") return;
        value = Number(input.value);
      } else value = input.value.trim();
      setPath(patch, input.name, value);
    });
    patch.providers.antigravity.groups = Array.prototype.filter.call(
      document.querySelectorAll("[data-ag-group]"), function (input) { return input.checked; }
    ).map(function (input) { return input.getAttribute("data-ag-group"); });
    // A fixed offset from older versions is removed so the time zone follows CPA.
    if (rawConfig.timezone_offset_hours !== undefined) patch.timezone_offset_hours = null;
    var button = form.querySelector("button[type=submit]");
    button.disabled = true;
    api("PATCH", CONFIG_API, patch).then(function () {
      toast("已保存，插件会在几秒内应用新设置");
      return loadConfig();
    }).then(function () {
      setTimeout(function () { loadStatus().then(fillSettings); }, 3000);
    }).catch(function (err) {
      if (err.message !== "unauthorized") toast("保存失败：" + err.message);
    }).then(function () { button.disabled = false; });
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
      if (err.message !== "unauthorized") toast("加载失败：" + err.message);
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
    runAction(e.currentTarget, API + "/refresh", {}, "已刷新全部额度");
  });
  $("test-bark").addEventListener("click", function (e) {
    runAction(e.currentTarget, API + "/test-bark", {}, "测试通知已发送");
  });
  $("settings").addEventListener("submit", saveSettings);
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
  window.addEventListener("storage", function (e) { if (e.key === "cli-proxy-theme") { applyTheme(); renderChart(); } });
  if (window.matchMedia) {
    var media = window.matchMedia("(prefers-color-scheme: dark)");
    if (media.addEventListener) media.addEventListener("change", function () { applyTheme(); renderChart(); });
  }

  applyTheme();
  watchParentTheme();
  key = sessionStorage.getItem(KEY_STORE) || centerKey();
  if (key) start(); else askForKey("");
})();
