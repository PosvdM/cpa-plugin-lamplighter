(function () {
  "use strict";

  var API = "/v0/management/lamplighter";
  var CONFIG_API = "/v0/management/plugins/lamplighter/config";
  var KEY_STORE = "lamplighter-management-key";
  var PROVIDERS = ["codex", "claude", "antigravity"];
  var SERIES_VARS = ["--series-1", "--series-2", "--series-3", "--series-4", "--series-5", "--series-6", "--series-7", "--series-8"];

  var key = "";
  var status = null;
  var rawConfig = {};
  var range = "24h";
  var view = "all:5h";
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

  // Bars use the same three tiers as the Management Center quota page.
  function meterClass(remaining) {
    if (remaining >= 70) return "fill-high";
    if (remaining >= 30) return "fill-medium";
    return "fill-low";
  }

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
      if (account.error) card.appendChild(el("div", { class: "error-text", text: account.error }));
      box.appendChild(card);
    });
  }

  function targetStatus(t) {
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

  function renderViewOptions() {
    var select = $("view-select");
    var current = view;
    clear(select);
    select.appendChild(el("option", { value: "all:5h", text: "全部账号 · 5 小时" }));
    select.appendChild(el("option", { value: "all:7d", text: "全部账号 · 7 天" }));
    status.accounts.forEach(function (account) {
      select.appendChild(el("option", { value: "account:" + account.auth_index, text: account.title + (account.email ? "（" + account.email + "）" : "") }));
    });
    var exists = Array.prototype.some.call(select.options, function (o) { return o.value === current; });
    view = exists ? current : "all:5h";
    select.value = view;
  }

  function renderStatus() {
    renderHeader();
    renderNotices();
    renderAccounts();
    renderTargets();
    renderEvents();
    renderViewOptions();
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

  // Colors follow the entity: every group or window keeps its slot no matter
  // which view or range is shown.
  function colorSlot(entity, entities) {
    var index = entities.indexOf(entity);
    return SERIES_VARS[(index < 0 ? 0 : index) % SERIES_VARS.length];
  }

  function cssVar(name) {
    return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
  }

  function windowKind(series) {
    if (series.window === "five-hour" || series.window_label === "5h") return "5h";
    if (series.window === "seven-day") return "7d";
    return series.window_label || series.window;
  }

  function currentSeries() {
    if (!history) return [];
    var all = history.series.filter(function (s) { return s.points && s.points.length; });
    if (view.indexOf("all:") === 0) {
      var kind = view.slice(4);
      var groups = [];
      status.accounts.forEach(function (a) { (a.groups || []).forEach(function (g) { groups.push(g.key); }); });
      all.forEach(function (s) { if (groups.indexOf(s.group) < 0) groups.push(s.group); });
      return all.filter(function (s) { return windowKind(s) === kind; })
        .sort(function (a, b) { return groups.indexOf(a.group) - groups.indexOf(b.group); })
        .map(function (s) {
          return { name: s.label, color: colorSlot(s.group, groups), points: s.points, group: s.group };
        });
    }
    var authIndex = view.slice(8);
    var account = status.accounts.filter(function (a) { return a.auth_index === authIndex; })[0];
    var windows = [];
    if (account) {
      (account.groups || []).forEach(function (g) { (g.windows || []).forEach(function (w) { windows.push(g.key + "|" + w.id); }); });
    }
    return all.filter(function (s) { return s.auth_index === authIndex || (account && windows.indexOf(s.group + "|" + s.window) >= 0); })
      .map(function (s) {
        var id = s.group + "|" + s.window;
        if (windows.indexOf(id) < 0) windows.push(id);
        var name = windowName({ short: s.window_label, label: s.window_label });
        if (s.label && account && s.label !== account.title) name = s.label + " · " + name;
        return { name: name, color: colorSlot(id, windows), points: s.points, group: s.group };
      });
  }

  function valueAt(points, t) {
    var value = null;
    for (var i = 0; i < points.length; i++) {
      if (points[i][0] <= t) value = points[i];
      else break;
    }
    return value;
  }

  var SVG_NS = "http://www.w3.org/2000/svg";
  function svg(tag, attrs) {
    var node = document.createElementNS(SVG_NS, tag);
    Object.keys(attrs || {}).forEach(function (name) { node.setAttribute(name, attrs[name]); });
    return node;
  }

  // Small multiples: one row per series on a shared time axis, so lines never
  // overlap. Each row is labeled with its series and current value, which
  // also makes a legend unnecessary.
  function renderChart() {
    var box = $("chart");
    box.classList.remove("loading");
    clear(box);
    var legend = $("legend");
    clear(legend);
    var table = $("chart-table");
    clear(table);
    var series = currentSeries();
    if (!history || !series.length) {
      box.appendChild(el("div", { class: "empty", text: history ? "这个范围内还没有额度数据。" : "加载中…" }));
      return;
    }

    var width = Math.max(320, box.clientWidth || 800);
    var labelW = width < 560 ? 104 : 168;
    var rowH = 58, rowGap = 14, axisH = 24, top = 4;
    var margin = { left: labelW, right: 12 };
    var plotW = width - margin.left - margin.right;
    var height = top + series.length * (rowH + rowGap) - rowGap + axisH;
    var from = new Date(history.from).getTime() / 1000;
    var to = new Date(history.to).getTime() / 1000;
    function x(t) { return margin.left + (t - from) / (to - from) * plotW; }
    function rowTop(i) { return top + i * (rowH + rowGap); }
    function y(i, v) { return rowTop(i) + (1 - v / 100) * rowH; }

    var root = svg("svg", { viewBox: "0 0 " + width + " " + height, role: "img", "aria-label": "各额度窗口剩余百分比随时间变化" });
    var span = to - from;
    var step = span <= 2 * 86400 ? 4 * 3600 : span <= 8 * 86400 ? 86400 : 5 * 86400;
    var offset = offsetSeconds();
    var ticks = [];
    for (var tk = Math.ceil((from + offset) / step) * step - offset; tk <= to; tk += step) ticks.push(tk);

    var eventsByGroup = {};
    (history.events || []).forEach(function (ev) {
      var et = new Date(ev.time).getTime() / 1000;
      if (et < from || et > to || !ev.group) return;
      (eventsByGroup[ev.group] = eventsByGroup[ev.group] || []).push({ t: et, failed: ev.kind === "ignite_failed" || ev.kind === "ignite_paused" });
    });

    series.forEach(function (s, i) {
      var color = cssVar(s.color);
      var base = rowTop(i) + rowH;
      // Row frame: 0% baseline, 50% and 100% hairlines, time gridlines.
      [0, 50, 100].forEach(function (v) {
        root.appendChild(svg("line", { x1: margin.left, x2: margin.left + plotW, y1: y(i, v), y2: y(i, v),
          stroke: cssVar(v === 0 ? "--axis" : "--grid"), "stroke-width": 1 }));
      });
      ticks.forEach(function (tk) {
        root.appendChild(svg("line", { x1: x(tk), x2: x(tk), y1: rowTop(i), y2: base, stroke: cssVar("--grid"), "stroke-width": 1 }));
      });
      // Row label and current value.
      var last = s.points[s.points.length - 1];
      var name = svg("text", { x: 0, y: rowTop(i) + 16, "font-size": 12.5, fill: cssVar("--text-secondary"), "class": "row-name" });
      name.textContent = s.name;
      root.appendChild(name);
      var value = svg("text", { x: 0, y: rowTop(i) + 40, "font-size": 18, "font-weight": 650, fill: cssVar("--text-primary"), "class": "row-value" });
      value.textContent = Math.round(last[1]) + "%";
      root.appendChild(value);
      var scale = svg("text", { x: margin.left - 6, y: rowTop(i) + 9, "font-size": 10, "text-anchor": "end", fill: cssVar("--text-muted") });
      scale.textContent = "100";
      root.appendChild(scale);
      // Area wash and step line: a value holds until the next sample.
      var d = "";
      s.points.forEach(function (p, j) {
        var px = x(p[0]), py = y(i, p[1]);
        d += j === 0 ? "M" + px + " " + py : "H" + px + "V" + py;
      });
      var endX = x(last[0]);
      root.appendChild(svg("path", { d: d + "V" + base + "H" + x(s.points[0][0]) + "Z", fill: color, "fill-opacity": 0.1, stroke: "none" }));
      root.appendChild(svg("path", { d: d, fill: "none", stroke: color, "stroke-width": 2, "stroke-linejoin": "round", "stroke-linecap": "round" }));
      root.appendChild(svg("circle", { cx: endX, cy: y(i, last[1]), r: 4, fill: color, stroke: cssVar("--surface"), "stroke-width": 2 }));
      // Ignition marks of this group on the row baseline.
      (eventsByGroup[s.group] || []).forEach(function (ev) {
        root.appendChild(svg("line", { x1: x(ev.t), x2: x(ev.t), y1: base - 6, y2: base,
          stroke: cssVar(ev.failed ? "--critical" : "--text-muted"), "stroke-width": 2, "stroke-linecap": "round" }));
      });
    });
    ticks.forEach(function (tk) {
      var label = svg("text", { x: x(tk), y: height - 6, "text-anchor": "middle", "font-size": 11, fill: cssVar("--text-muted") });
      label.textContent = span <= 2 * 86400 ? fmtClock(new Date(tk * 1000)) : fmtDay(new Date(tk * 1000));
      root.appendChild(label);
    });

    var plotBottom = height - axisH;
    var cross = svg("line", { y1: top, y2: plotBottom, stroke: cssVar("--axis"), "stroke-width": 1, visibility: "hidden" });
    root.appendChild(cross);
    var overlay = svg("rect", { x: margin.left, y: top, width: plotW, height: plotBottom - top, fill: "transparent", tabindex: "0",
      "aria-label": "按左右方向键查看各时刻的数值" });
    root.appendChild(overlay);
    box.appendChild(root);
    var tip = el("div", { class: "tooltip", hidden: true });
    box.appendChild(tip);

    var times = [];
    series.forEach(function (s) { s.points.forEach(function (p) { times.push(p[0]); }); });
    times.sort(function (a, b) { return a - b; });
    times = times.filter(function (t, i) { return i === 0 || t !== times[i - 1]; });
    var cursor = -1;

    function show(index) {
      if (index < 0 || index >= times.length) return;
      cursor = index;
      var t = times[index];
      var px = x(t);
      cross.setAttribute("x1", px);
      cross.setAttribute("x2", px);
      cross.setAttribute("visibility", "visible");
      clear(tip);
      tip.appendChild(el("div", { class: "time", text: fmtDateTime(new Date(t * 1000)) }));
      series.forEach(function (s) {
        var p = valueAt(s.points, t);
        if (!p) return;
        tip.appendChild(el("div", { class: "row" }, [
          el("span", { class: "key", style: "background:" + cssVar(s.color) }),
          el("strong", { text: Math.round(p[1]) + "%" }),
          el("span", { text: s.name }),
          el("span", { class: "src", text: p[2] === 1 ? "被动" : "主动" })
        ]));
      });
      var near = Math.max(300, (to - from) / plotW * 6);
      (history.events || []).forEach(function (ev) {
        var et = new Date(ev.time).getTime() / 1000;
        if (Math.abs(et - t) <= near) tip.appendChild(el("div", { class: "row muted", text: ev.message }));
      });
      tip.hidden = false;
      var scale = box.clientWidth / width;
      var left = px * scale + 12;
      if (left + 240 > box.clientWidth) left = px * scale - 252;
      tip.style.left = Math.max(0, left) + "px";
      tip.style.top = "0px";
    }
    function hide() { cross.setAttribute("visibility", "hidden"); tip.hidden = true; }
    function nearest(clientX) {
      var rect = root.getBoundingClientRect();
      var t = from + ((clientX - rect.left) / rect.width * width - margin.left) / plotW * (to - from);
      var best = 0;
      for (var i = 1; i < times.length; i++) if (Math.abs(times[i] - t) < Math.abs(times[best] - t)) best = i;
      return best;
    }
    overlay.addEventListener("pointermove", function (e) { show(nearest(e.clientX)); });
    overlay.addEventListener("pointerleave", hide);
    overlay.addEventListener("blur", hide);
    overlay.addEventListener("focus", function () { show(cursor < 0 ? times.length - 1 : cursor); });
    overlay.addEventListener("keydown", function (e) {
      if (e.key === "ArrowLeft") { show(Math.max(0, cursor - 1)); e.preventDefault(); }
      if (e.key === "ArrowRight") { show(Math.min(times.length - 1, cursor + 1)); e.preventDefault(); }
    });

    legend.textContent = "每行一个额度窗口，纵轴 0–100%。底部刻线为点火，红色为点火失败；悬停查看各时刻数值和数据来源。";
    series.forEach(function (s) {
      var values = s.points.map(function (p) { return p[1]; });
      table.appendChild(el("tr", {}, [
        el("td", { text: s.name }),
        el("td", { class: "num", text: Math.round(values[values.length - 1]) + "%" }),
        el("td", { class: "num", text: Math.round(Math.min.apply(null, values)) + "%" }),
        el("td", { class: "num", text: Math.round(Math.max.apply(null, values)) + "%" }),
        el("td", { class: "num", text: String(values.length) })
      ]));
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
  Array.prototype.forEach.call(document.querySelectorAll("#range-buttons button"), function (button) {
    button.addEventListener("click", function () {
      range = button.getAttribute("data-range");
      Array.prototype.forEach.call(document.querySelectorAll("#range-buttons button"), function (b) {
        b.setAttribute("aria-pressed", b === button ? "true" : "false");
      });
      loadHistory();
    });
  });
  $("view-select").addEventListener("change", function (e) { view = e.target.value; renderChart(); });
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
