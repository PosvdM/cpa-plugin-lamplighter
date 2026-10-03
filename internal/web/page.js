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

  // Theme follows the Management Center setting stored in localStorage.
  function applyTheme() {
    var theme = "auto";
    try {
      var raw = localStorage.getItem("cli-proxy-theme");
      if (raw) {
        var parsed = JSON.parse(raw);
        theme = (parsed && parsed.state && parsed.state.theme) || parsed.theme || theme;
      }
    } catch (e) { /* use auto */ }
    var dark = theme === "dark" || (theme === "auto" && window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches);
    document.documentElement.setAttribute("data-theme", dark ? "dark" : "light");
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

  // Times are shown in the plugin's configured time zone, like notifications.
  function offsetHours() {
    var value = status && status.config ? Number(status.config.timezone_offset_hours) : 8;
    return isFinite(value) ? value : 8;
  }

  function parseTime(value) {
    if (!value) return null;
    var t = new Date(value);
    if (isNaN(t.getTime()) || t.getUTCFullYear() < 2000) return null;
    return t;
  }

  function pad(n) { return (n < 10 ? "0" : "") + n; }

  function shifted(t) { return new Date(t.getTime() + offsetHours() * 3600 * 1000); }

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

  function fmtAgo(t) {
    if (!t) return "";
    var ms = Date.now() - t.getTime();
    if (ms < 60000) return "刚刚";
    return fmtDuration(ms) + "前";
  }

  function severityColor(remaining) {
    var cfg = (status && status.config) || {};
    if (remaining <= (cfg.critical_threshold || 10)) return "var(--critical)";
    if (remaining <= (cfg.low_threshold || 20)) return "var(--serious)";
    if (remaining <= (cfg.notice_threshold || 50)) return "var(--warning)";
    return "var(--accent)";
  }

  function providerTitle(provider) {
    return { codex: "ChatGPT", claude: "Claude", antigravity: "Antigravity" }[provider] || provider;
  }

  // ---- Status rendering ----

  function renderHeader() {
    var state = $("state");
    clear(state);
    var color = "var(--good)", text = "运行中";
    if (!status.enabled) { color = "var(--text-muted)"; text = "已停用"; }
    else if (!status.running) { color = "var(--warning)"; text = "启动中"; }
    state.appendChild(el("span", { class: "dot", style: "background:" + color }));
    state.appendChild(document.createTextNode(text));
    var parts = ["v" + status.version];
    var last = parseTime(status.last_poll), next = parseTime(status.next_poll);
    if (last) parts.push("上次查询 " + fmtDateTime(last));
    if (next) parts.push("下次 " + fmtDateTime(next));
    $("meta").textContent = parts.join(" · ");
  }

  function notice(icon, color, text) {
    return el("div", { class: "notice" }, [
      el("span", { class: "icon", style: "color:" + color, text: icon }),
      el("span", { text: text })
    ]);
  }

  function renderNotices() {
    var box = $("notices");
    clear(box);
    if (status.config_error) box.appendChild(notice("✕", "var(--critical)", "配置无法解析，仍在使用上一份配置：" + status.config_error));
    if (status.lock_error) box.appendChild(notice("!", "var(--warning)", status.lock_error));
    if (status.list_error) box.appendChild(notice("✕", "var(--critical)", status.list_error));
    if (status.models_error) box.appendChild(notice("!", "var(--warning)", "模型列表不可用：" + status.models_error));
    if (status.config && !status.config.bark_url) box.appendChild(notice("!", "var(--warning)", "未设置 Bark 推送地址，通知不会发送。"));
    if (status.config && status.config.ignition && status.config.ignition.enabled && !status.config.models_api_key) {
      box.appendChild(notice("!", "var(--warning)", "未设置模型列表 API key，自动点火无法选择模型。"));
    }
  }

  function renderAccounts() {
    var box = $("accounts");
    clear(box);
    if (!status.accounts.length) {
      box.appendChild(el("p", { class: "muted", text: status.running ? "没有可监控的 ChatGPT、Claude 或 Antigravity 凭证。" : "CPA 启动约 20 秒后开始第一次查询。" }));
      return;
    }
    status.accounts.forEach(function (account) {
      var card = el("div", { class: "account" });
      var refresh = el("button", { type: "button", class: "chip", text: "刷新", onclick: function () {
        runAction(refresh, API + "/refresh", { auth_index: account.auth_index }, "已刷新 " + account.title);
      } });
      card.appendChild(el("div", { class: "account-head" }, [
        el("span", { class: "title", text: account.title }),
        el("span", { class: "muted", text: account.email || account.name }),
        refresh
      ]));
      (account.groups || []).forEach(function (group) {
        if (group.source_label && group.source_label !== providerTitle(account.provider)) {
          card.appendChild(el("div", { class: "group-label", text: group.source_label }));
        }
        (group.windows || []).forEach(function (w) {
          var remaining = Math.max(0, Math.min(100, w.remaining));
          var reset = parseTime(w.reset);
          var observed = parseTime(w.observed_at);
          var meta = [];
          if (reset) meta.push(el("span", { text: "重置 " + fmtDuration(reset.getTime() - Date.now()) + " · " + fmtDateTime(reset) }));
          meta.push(el("span", { text: (w.source === "passive" ? "被动" : "主动") + (observed ? " · " + fmtAgo(observed) : "") }));
          card.appendChild(el("div", { class: "window" }, [
            el("span", { class: "label", text: w.short }),
            el("div", { class: "meter", role: "meter", "aria-valuemin": "0", "aria-valuemax": "100", "aria-valuenow": String(Math.round(remaining)), "aria-label": group.label + " " + w.short + " 剩余" }, [
              el("span", { style: "width:" + remaining + "%;background:" + severityColor(remaining) })
            ]),
            el("span", { class: "value", text: Math.round(remaining) + "%" }),
            el("div", { class: "window-meta" }, meta)
          ]));
        });
      });
      if (account.error) card.appendChild(el("div", { class: "error-text", text: account.error }));
      box.appendChild(card);
    });
  }

  function targetStatus(t) {
    var circuit = parseTime(t.circuit_until);
    if (circuit) return { color: "var(--critical)", icon: "■", text: "已暂停至 " + fmtDateTime(circuit), title: t.circuit_reason };
    if (t.consecutive_failures > 0) return { color: "var(--serious)", icon: "▲", text: "失败 " + t.consecutive_failures + " 次", title: t.last_error };
    if (t.rolling) return { color: "var(--warning)", icon: "○", text: "窗口未启动" };
    return { color: "var(--good)", icon: "●", text: "正常" };
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
      body.appendChild(el("tr", {}, [el("td", { colspan: "7", class: "muted", text: "没有启用点火的额度组。" })]));
      return;
    }
    status.targets.forEach(function (t) {
      var st = targetStatus(t);
      var button = el("button", { type: "button", class: "chip", text: "立即点火", onclick: function () {
        if (!confirm("立即向 " + t.label + " 发送一次点火请求？这会开始一个新的 5 小时窗口。")) return;
        runAction(button, API + "/ignite", { target: t.key }, t.label + " 点火成功");
      } });
      var next = parseTime(t.next_due), reset = parseTime(t.reset), last = parseTime(t.last_success);
      body.appendChild(el("tr", {}, [
        el("td", { text: t.label }),
        el("td", { class: "tnum", text: ignition.enabled && next ? fmtDateTime(next) : "—" }),
        el("td", { class: "tnum", text: reset ? fmtDateTime(reset) : "—" }),
        el("td", { class: "tnum", text: last ? fmtDateTime(last) : "—" }),
        el("td", { text: t.last_model || "—" }),
        el("td", { title: st.title || "" }, [el("span", { class: "status" }, [
          el("span", { style: "color:" + st.color, text: st.icon }), document.createTextNode(st.text)
        ])]),
        el("td", {}, [button])
      ]));
    });
  }

  function renderEvents() {
    var list = $("events");
    clear(list);
    if (!status.events.length) {
      list.appendChild(el("li", { class: "empty-row muted", text: "暂无事件" }));
      return;
    }
    status.events.slice(0, 100).forEach(function (ev) {
      var icon = ev.level === "error" ? "✕" : ev.level === "warn" ? "!" : "·";
      var color = ev.level === "error" ? "var(--critical)" : ev.level === "warn" ? "var(--serious)" : "var(--text-muted)";
      list.appendChild(el("li", {}, [
        el("span", { class: "when", text: fmtDateTime(parseTime(ev.time)) }),
        el("span", {}, [el("span", { style: "color:" + color + ";margin-right:6px", text: icon }), document.createTextNode(ev.message)])
      ]));
    });
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
      return all.filter(function (s) { return windowKind(s) === kind; }).map(function (s) {
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
        var name = s.window_label;
        if (s.label && account && s.label !== account.title) name = s.label + " · " + s.window_label;
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
    var height = 280;
    var directLabels = series.length <= 4;
    var margin = { top: 12, right: directLabels ? 96 : 16, bottom: 28, left: 44 };
    var plotW = width - margin.left - margin.right;
    var plotH = height - margin.top - margin.bottom;
    var from = new Date(history.from).getTime() / 1000;
    var to = new Date(history.to).getTime() / 1000;
    function x(t) { return margin.left + (t - from) / (to - from) * plotW; }
    function y(v) { return margin.top + (1 - v / 100) * plotH; }

    var root = svg("svg", { viewBox: "0 0 " + width + " " + height, role: "img", "aria-label": "额度剩余百分比随时间变化" });
    // Gridlines and y ticks.
    [0, 25, 50, 75, 100].forEach(function (v) {
      root.appendChild(svg("line", { x1: margin.left, x2: margin.left + plotW, y1: y(v), y2: y(v), stroke: v === 0 ? cssVar("--axis") : cssVar("--grid"), "stroke-width": 1 }));
      var label = svg("text", { x: margin.left - 8, y: y(v) + 4, "text-anchor": "end", "font-size": 11, fill: cssVar("--text-muted") });
      label.textContent = v + "%";
      root.appendChild(label);
    });
    // X ticks.
    var span = to - from;
    var step = span <= 2 * 86400 ? 4 * 3600 : span <= 8 * 86400 ? 86400 : 5 * 86400;
    var offset = offsetHours() * 3600;
    for (var t = Math.ceil((from + offset) / step) * step - offset; t <= to; t += step) {
      var tickLabel = svg("text", { x: x(t), y: height - 8, "text-anchor": "middle", "font-size": 11, fill: cssVar("--text-muted") });
      tickLabel.textContent = span <= 2 * 86400 ? fmtClock(new Date(t * 1000)) : fmtDay(new Date(t * 1000));
      root.appendChild(tickLabel);
    }
    // Ignition events along the baseline.
    (history.events || []).forEach(function (ev) {
      var et = new Date(ev.time).getTime() / 1000;
      if (et < from || et > to) return;
      var failed = ev.kind === "ignite_failed" || ev.kind === "ignite_paused";
      root.appendChild(svg("line", { x1: x(et), x2: x(et), y1: y(0) - 6, y2: y(0), stroke: failed ? cssVar("--critical") : cssVar("--text-muted"), "stroke-width": 2, "stroke-linecap": "round" }));
    });
    // Lines: values hold until the next sample, so the path is a step.
    var ends = [];
    series.forEach(function (s) {
      var color = cssVar(s.color);
      var d = "";
      s.points.forEach(function (p, i) {
        var px = x(p[0]), py = y(p[1]);
        d += i === 0 ? "M" + px + " " + py : "H" + px + "V" + py;
      });
      root.appendChild(svg("path", { d: d, fill: "none", stroke: color, "stroke-width": 2, "stroke-linejoin": "round", "stroke-linecap": "round" }));
      if (range === "24h") {
        s.points.forEach(function (p) {
          if (p[2] !== 1) return;
          root.appendChild(svg("circle", { cx: x(p[0]), cy: y(p[1]), r: 4, fill: color, stroke: cssVar("--surface"), "stroke-width": 2 }));
        });
      }
      var last = s.points[s.points.length - 1];
      root.appendChild(svg("circle", { cx: x(last[0]), cy: y(last[1]), r: 4, fill: color, stroke: cssVar("--surface"), "stroke-width": 2 }));
      ends.push({ x: x(last[0]), y: y(last[1]), name: s.name, value: last[1] });
    });
    // Direct end labels, only when they do not collide.
    if (directLabels) {
      var sorted = ends.slice().sort(function (a, b) { return a.y - b.y; });
      var collide = sorted.some(function (e, i) { return i > 0 && e.y - sorted[i - 1].y < 14; });
      if (!collide) {
        ends.forEach(function (e) {
          var label = svg("text", { x: margin.left + plotW + 8, y: e.y + 4, "font-size": 11, fill: cssVar("--text-secondary") });
          label.textContent = e.name + " " + Math.round(e.value) + "%";
          root.appendChild(label);
        });
      }
    }
    // Crosshair and tooltip.
    var cross = svg("line", { y1: margin.top, y2: margin.top + plotH, stroke: cssVar("--axis"), "stroke-width": 1, visibility: "hidden" });
    root.appendChild(cross);
    var overlay = svg("rect", { x: margin.left, y: margin.top, width: plotW, height: plotH, fill: "transparent", tabindex: "0", "aria-label": "按左右方向键查看各时刻的数值" });
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
      (history.events || []).forEach(function (ev) {
        var et = new Date(ev.time).getTime() / 1000;
        if (Math.abs(et - t) <= Math.max(300, (to - from) / plotW * 6)) {
          tip.appendChild(el("div", { class: "row muted", text: ev.message }));
        }
      });
      tip.hidden = false;
      var scale = box.clientWidth / width;
      var left = px * scale + 12;
      if (left + 220 > box.clientWidth) left = px * scale - 232;
      tip.style.left = Math.max(0, left) + "px";
      tip.style.top = (margin.top * scale) + "px";
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

    // Legend, always present for two or more series.
    if (series.length > 1) {
      series.forEach(function (s) {
        legend.appendChild(el("span", {}, [el("span", { class: "key", style: "background:" + cssVar(s.color) }), document.createTextNode(s.name)]));
      });
    }
    legend.appendChild(el("span", { class: "muted", text: (range === "24h" ? "圆点为被动数据；" : "") + "底部刻线为点火，红色为点火失败" }));
    // Table view.
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

  // Effective values come from the status config (defaults applied); saved
  // values come from the raw plugin config.
  function fillSettings() {
    var form = $("settings");
    var effective = JSON.parse(JSON.stringify(status.config || {}));
    effective.bark_url = rawConfig.bark_url || "";
    effective.models_api_key = rawConfig.models_api_key || "";
    Array.prototype.forEach.call(form.elements, function (input) {
      if (!input.name) return;
      var value = getPath(effective, input.name);
      if (input.type === "checkbox") input.checked = !!value;
      else if (input.name === "providers.antigravity.groups") input.value = (value || []).join(", ");
      else input.value = value === undefined || value === null ? "" : value;
    });
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
      } else if (input.name === "providers.antigravity.groups") {
        value = input.value.split(",").map(function (s) { return s.trim(); }).filter(Boolean);
      } else value = input.value.trim();
      setPath(patch, input.name, value);
    });
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
  key = sessionStorage.getItem(KEY_STORE) || centerKey();
  if (key) start(); else askForKey("");
})();
