const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { test } = require("node:test");

const source = fs.readFileSync(path.join(__dirname, "page.js"), "utf8");
// Load the actual pure functions without starting the DOM or polling timers.
function context(names, globals) {
  const code = names.map((name) => {
    const match = source.match(new RegExp("  function " + name + "\\([^]*?\\n  }"));
    assert.ok(match, name);
    return match[0];
  }).join("\n");
  const ctx = vm.createContext(globals || {});
  vm.runInContext(code, ctx);
  return ctx;
}

test("a save counts as applied only by its own config token or a later one", () => {
  const ctx = context(["tokenApplied", "checkApplied"], { status: { config: { bark_url: "已设置" } } });
  const tokens = ["a1", "b2", "c3"];
  assert.equal(ctx.tokenApplied(tokens, "b2", "a1"), false);
  assert.equal(ctx.tokenApplied(tokens, "b2", "b2"), true);
  assert.equal(ctx.tokenApplied(tokens, "b2", "c3"), true);
  assert.equal(ctx.tokenApplied(tokens, "b2", ""), false);
  assert.equal(ctx.tokenApplied(tokens, "b2", undefined), false);
  // A token saved elsewhere, or a redacted value in the status, proves nothing.
  assert.equal(ctx.tokenApplied(tokens, "b2", "other"), false);
  assert.equal(ctx.tokenApplied(tokens, "zz", "c3"), false);

  const settled = [];
  ctx.savedTokens = tokens;
  const wait = (token) => {
    const w = { token, done: (v) => { settled.push([token, v]); ctx.applyWaits = ctx.applyWaits.filter((x) => x !== w); } };
    return w;
  };
  ctx.applyWaits = [wait("b2"), wait("c3")];
  ctx.status.config_applied_token = "b2";
  ctx.checkApplied();
  assert.deepEqual(settled, [["b2", true]]);
  assert.equal(ctx.applyWaits.length, 1);
  ctx.status.config_applied_token = "c3";
  ctx.checkApplied();
  assert.deepEqual(settled.at(-1), ["c3", true]);
  assert.equal(ctx.applyWaits.length, 0);
  assert.doesNotMatch(source, /function sameConfig|mcp_key_set/);
});

test("config tokens are 16 hexadecimal digits", () => {
  const ctx = context(["newConfigToken"], { crypto: require("node:crypto").webcrypto, Uint8Array });
  const a = ctx.newConfigToken(), b = ctx.newConfigToken();
  assert.match(a, /^[0-9a-f]{16}$/);
  assert.notEqual(a, b);
});

test("account names preserve trailing note lines and clearing removes the whole note", () => {
  const ctx = context(["accountNote"]);
  assert.equal(ctx.accountNote("old\r\nsecond\r\nthird", "new"), "new\nsecond\nthird");
  assert.equal(ctx.accountNote("old\nsecond", ""), "");
  assert.equal(ctx.accountNote("", "new"), "new");
});

test("drag targets use layout midpoints rather than animated rectangles", () => {
  const ctx = context(["accountDropIndex"]);
  const rows = [0, 100, 200].map((offsetTop) => ({
    offsetTop, offsetHeight: 80,
    getBoundingClientRect: () => { throw new Error("animated geometry must not be read"); },
  }));
  assert.equal(ctx.accountDropIndex(rows, 49, 10), 0);
  assert.equal(ctx.accountDropIndex(rows, 51, 10), 1);
  assert.equal(ctx.accountDropIndex(rows, 150, 10), 1);
  assert.equal(ctx.accountDropIndex(rows, 151, 10), 2);
  assert.equal(ctx.accountDropIndex(rows, 999, 10), 3);
});

test("queued inline weights merge changes and an empty field removes only its own override", async () => {
  const states = [], writes = [];
  const ctx = context(["saveAccountField", "accountFieldHost", "accountNote", "fmtWeight"], {
    rawConfig: {}, saveQueue: Promise.resolve(), history: null,
    saveHost: () => ({}),
    translate: (key) => key, setSaveState: (...args) => states.push(args),
    claimState: () => () => {}, showApplied: () => {},
    patchConfig: async (patch) => { writes.push(plain(patch)); Object.assign(ctx.rawConfig, patch); return "token"; },
    loadConfig: async () => {}, renderAccounts: () => {},
  });
  const input = (value) => ({ value, parentNode: { firstChild: {} }, accountSaved: "", accountEdit: 1, accountDirty: true, validity: { badInput: false }, hasAttribute: () => false, getAttribute: () => "weight" });
  const a = input("2.5"), b = input("7");
  ctx.saveAccountField(a, { account: { name: "a" } });
  ctx.saveAccountField(b, { account: { name: "b" } });
  await ctx.saveQueue;
  assert.deepEqual(plain(ctx.rawConfig.account_weights), { a: 2.5, b: 7 });
  a.value = ""; a.accountEdit++;
  ctx.saveAccountField(a, { account: { name: "a" } });
  await ctx.saveQueue;
  assert.deepEqual(plain(ctx.rawConfig.account_weights), { b: 7 });
  for (const bad of ["0", "-1", "Infinity", "NaN"]) {
    ctx.saveAccountField(input(bad), { account: { name: "a" } });
  }
  const invalidEmpty = input(""); invalidEmpty.validity.badInput = true;
  ctx.saveAccountField(invalidEmpty, { account: { name: "a" } });
  await ctx.saveQueue;
  assert.equal(writes.length, 3);
  assert.equal(states.length, 5);
});

test("an earlier account name save does not overwrite a newer unsaved edit", async () => {
  const ctx = context(["saveAccountField", "accountFieldHost", "accountNote", "fmtWeight"], {
    saveQueue: Promise.resolve(), history: null, saveHost: () => ({}),
    claimState: () => () => {}, translate: (key) => key, renderAccounts: () => {},
    saveAccountName: async () => {},
  });
  const input = { value: "first", parentNode: { firstChild: {} }, accountSaved: "old", accountEdit: 1, accountDirty: true, getAttribute: () => "name" };
  ctx.saveAccountField(input, { account: { name: "a", note: "old\nrest" } });
  input.value = "new unsaved draft"; input.accountEdit++;
  await ctx.saveQueue;
  assert.equal(input.value, "new unsaved draft");
  assert.equal(input.accountDirty, true);
  assert.equal(input.accountPending, 0);
});

// Minimal DOM doubles exercise the actual state aggregation and save queue.
function saveNode(tagName, attrs = {}) {
  return {
    tagName, id: attrs.id || "", name: attrs.name || "", children: [], attrs: { ...attrs },
    value: "", validity: { badInput: false }, accountSaved: "", accountEdit: 1, accountDirty: true,
    get textContent() { return (this.text || "") + this.children.map(n => n.textContent).join(""); },
    set textContent(value) { this.text = value; this.children = []; },
    getAttribute(name) { return this.attrs[name] ?? null; },
    hasAttribute(name) { return name in this.attrs; },
    setAttribute(name, value) { this.attrs[name] = value; },
    removeAttribute(name) { delete this.attrs[name]; },
    appendChild(node) { this.children.push(node); return node; },
    querySelector(selector) { return selector === ".save-state" ? this.children[0] : null; },
    closest() { return null; },
  };
}

const saveFunctions = ["saveHost", "saveControls", "saveItemKey", "saveEntry", "renderSaveState",
  "updateSaveEntry", "setSaveState", "claimState", "editSaveState"];
function stateContext(names = [], globals = {}) {
  return context([...saveFunctions, ...names], {
    saveSerial: 0,
    translate: (key, ...args) => {
      if (key === "save_field_failed") return "✕ " + args[0] + " save failed: " + args[1];
      if (key === "save_more_failed") return " (" + args[0] + " more failed)";
      return key;
    },
    saveText: text => text,
    saveItemLabel: input => input.id === "account-order-state" ? "Order" : input.getAttribute("data-account-field") || input.name,
    el: (tag, attrs) => saveNode(tag.toUpperCase(), attrs),
    clear: node => { node.textContent = ""; },
    maskedNodes: text => [{ textContent: text }],
    ...globals,
  });
}

function accountSaveContext(fail) {
  const heading = saveNode("H2", { id: "account-order-state" });
  const section = { tagName: "SECTION", querySelector: selector => { assert.equal(selector, ".card-title"); return heading; } };
  const row = { account: { name: "a", note: "old" }, getAttribute: () => "a" };
  const ctx = stateContext(["saveAccountField", "accountFieldHost", "accountNote", "fmtWeight", "saveAccountOrder"], {
    fail, rawConfig: {}, saveQueue: Promise.resolve(), history: null, pendingOrder: null,
    $: () => heading, toast: () => {},
    showApplied: say => { say("waiting", "waiting"); say("saved", "applied"); },
    saveAccountName: async () => { if (ctx.fail === "name" || ctx.fail === "unauthorized") throw new Error(ctx.fail); },
    patchConfig: async patch => {
      if (ctx.fail === "weight" && patch.account_weights || ctx.fail === "order" && patch.account_order) throw new Error("failed");
      Object.assign(ctx.rawConfig, patch);
      return "token";
    },
    loadConfig: async () => {}, sortAccounts: () => {}, renderAccounts: () => {},
  });
  const field = (kind, value) => {
    const input = saveNode("INPUT", { "data-account-field": kind });
    input.value = value;
    input.closest = selector => selector === ".account-setting" ? row : selector === ".settings-section" ? section : null;
    return input;
  };
  return { ctx, heading, row, name: field("name", "unsaved name"), weight: field("weight", "2.5") };
}

test("a failed name save stays in the heading after successful weight and order saves", async () => {
  const { ctx, heading, row, name, weight } = accountSaveContext("name");
  ctx.saveAccountField(name, row);
  ctx.saveAccountField(weight, row);
  ctx.saveAccountOrder(["a"]);
  await ctx.saveQueue;
  assert.equal(heading.children.length, 1);
  assert.equal(heading.children[0].getAttribute("data-state"), "failed");
  assert.match(heading.textContent, /name save failed/);
  assert.equal(name.getAttribute("aria-invalid"), "true");
  assert.equal(weight.hasAttribute("aria-invalid"), false);
  assert.equal(name.value, "unsaved name");
  assert.equal(name.accountDirty, true);
  assert.equal(name.accountPending, 0);
  assert.equal(weight.accountDirty, false);
  ctx.fail = null;
  ctx.saveAccountField(name, row);
  await ctx.saveQueue;
  assert.equal(name.hasAttribute("aria-invalid"), false);
  assert.equal(heading.children[0].getAttribute("data-state"), "saved");
  assert.doesNotMatch(heading.textContent, /failed/);
});

test("weight and order failures survive a successful name save", async () => {
  for (const fail of ["weight", "order"]) {
    const { ctx, heading, row, name, weight } = accountSaveContext(fail);
    if (fail === "weight") ctx.saveAccountField(weight, row);
    else ctx.saveAccountOrder(["a"]);
    ctx.saveAccountField(name, row);
    await ctx.saveQueue;
    assert.equal(heading.children[0].getAttribute("data-state"), "failed");
    assert.match(heading.textContent, fail === "weight" ? /weight save failed/ : /Order save failed/);
    if (fail === "weight") {
      assert.equal(weight.getAttribute("aria-invalid"), "true");
      assert.equal(weight.value, "2.5");
      assert.equal(weight.accountDirty, true);
    }
  }
});

test("reverting a failed account field to its saved value still retries and clears the failure", async () => {
  const { ctx, heading, row, weight } = accountSaveContext("weight");
  ctx.saveAccountField(weight, row);
  await ctx.saveQueue;
  assert.equal(weight.getAttribute("aria-invalid"), "true");
  ctx.fail = null;
  weight.value = weight.accountSaved;
  ctx.saveAccountField(weight, row);
  await ctx.saveQueue;
  assert.equal(weight.hasAttribute("aria-invalid"), false);
  assert.equal(heading.children[0].getAttribute("data-state"), "saved");
  assert.equal(ctx.rawConfig.account_weights, null);
});

test("multiple failures show the latest reason and count, with every failed control marked", async () => {
  const { ctx, heading, row, name, weight } = accountSaveContext("unauthorized");
  ctx.saveAccountField(name, row);
  await ctx.saveQueue;
  weight.value = "0";
  ctx.saveAccountField(weight, row);
  assert.match(heading.textContent, /weight save failed: weight_invalid \(1 more failed\)/);
  assert.equal(name.getAttribute("aria-invalid"), "true");
  assert.equal(weight.getAttribute("aria-invalid"), "true");
  ctx.editSaveState(heading, name);
  assert.match(heading.textContent, /1 more failed/);
  ctx.fail = null;
  ctx.saveAccountField(name, row);
  await ctx.saveQueue;
  assert.equal(name.hasAttribute("aria-invalid"), false);
  assert.equal(weight.getAttribute("aria-invalid"), "true");
  assert.doesNotMatch(heading.textContent, /more failed/);
  weight.value = "3";
  ctx.saveAccountField(weight, row);
  await ctx.saveQueue;
  assert.equal(weight.hasAttribute("aria-invalid"), false);
  assert.equal(heading.children[0].getAttribute("data-state"), "saved");
});

test("all settings resolve to section headings or their own advanced legends", () => {
  const ctx = stateContext();
  for (const advanced of [false, true]) {
    for (const name of ["bark_url", "ignition.start_hour", "providers.codex.monitor", "remember_chart_filters", "mcp_enabled"]) {
      const heading = saveNode(advanced ? "LEGEND" : "H2");
      const group = { tagName: advanced ? "FIELDSET" : "SECTION", querySelector: selector => {
        assert.equal(selector, advanced ? "legend" : ".card-title"); return heading;
      } };
      const input = saveNode("INPUT", { name });
      input.closest = selector => selector === (advanced ? "fieldset" : ".settings-section") ? group : null;
      assert.equal(ctx.saveHost(input), heading);
      ctx.setSaveState(heading, "failed", "reason", input);
      assert.equal(heading.children.length, 1);
      assert.equal(heading.children[0].attrs.class, "save-state");
      assert.equal(input.children.length, 0);
      assert.equal(input.getAttribute("aria-invalid"), "true");
    }
  }
  const html = fs.readFileSync(path.join(__dirname, "page.html"), "utf8");
  assert.match(html, /<form id="settings">\s*<section[^>]+>\s*<h2[^>]+id="account-order-state"/);
  assert.doesNotMatch(html, /data-save-in/);
});

test("ordinary settings keep A's failure through B's save and reject stale replies after editing", () => {
  const ctx = stateContext(), heading = saveNode("H2");
  const a = saveNode("INPUT", { name: "a" }), b = saveNode("INPUT", { name: "b" });
  const sayA = ctx.claimState(heading, a);
  sayA("failed", "A failed");
  const sayB = ctx.claimState(heading, b);
  sayB("saved", "B saved");
  assert.match(heading.textContent, /a save failed: A failed/);
  assert.equal(a.getAttribute("aria-invalid"), "true");
  ctx.editSaveState(heading, a);
  assert.equal(sayA("saved", "late reply"), false);
  assert.match(heading.textContent, /A failed/);
  const retry = ctx.claimState(heading, a);
  assert.match(heading.textContent, /A failed/);
  retry("waiting", "saved, waiting");
  assert.equal(a.hasAttribute("aria-invalid"), false);
  retry("saved", "applied");
  assert.equal(heading.textContent, "applied");
});

test("an older confirmation cannot replace a more recent save's progress", () => {
  const ctx = stateContext(), heading = saveNode("H2");
  const a = saveNode("INPUT", { name: "a" }), b = saveNode("INPUT", { name: "b" });
  const sayA = ctx.claimState(heading, a), sayB = ctx.claimState(heading, b);
  sayB("waiting", "B waiting");
  sayA("saved", "A applied");
  assert.equal(heading.textContent, "B waiting");
});

test("saved messages and dynamic failure reasons follow language changes", () => {
  const code = source.slice(source.indexOf("  var I18N ="), source.indexOf("\n  function translate("));
  assert.match(code, /save_field_failed/);
  const globals = vm.createContext({});
  vm.runInContext(code, globals);
  globals.translate = (key, value) => globals.I18N.en[key].replace("{0}", value);
  const ctx = context(["saveText"], globals);
  assert.equal(ctx.saveText(globals.I18N.zh.save_applied), globals.I18N.en.save_applied);
  assert.equal(ctx.saveText(globals.I18N.zh.weight_invalid), globals.I18N.en.weight_invalid);
  assert.equal(ctx.saveText(globals.I18N.zh.mcp_error.replace("{0}", "bind failed")), globals.I18N.en.mcp_error.replace("{0}", "bind failed"));
  assert.equal(ctx.saveText("HTTP 500 upstream error"), "HTTP 500 upstream error");
});

const HOUR = 3600, DAY = 86400, WEEK = 7 * DAY;
function pace() { return context(["paceSegments", "paceAt", "pooledPace"], { RESET_DRIFT: 60 }); }
// Arrays from the VM context have another prototype.
const plain = (value) => JSON.parse(JSON.stringify(value));

test("an early reset cuts the even pace where the new reset appears", () => {
  const ctx = pace();
  const t0 = 1000 * DAY, x1 = t0 + 5 * DAY, early = t0 + 3 * DAY, x2 = early + WEEK;
  const segs = ctx.paceSegments([[t0 - 3 * DAY, x1], [early, x2]], WEEK, x2, early + DAY);
  assert.equal(segs.length, 2);
  // The old line starts where its window began and stops at the early reset
  // at the share an even pace leaves there, not at 0%.
  assert.deepEqual(plain(segs[0]), [x1 - WEEK, early, x1, false]);
  assert.ok(Math.abs(ctx.paceAt(segs, WEEK, early - 1) - 100 * 2 / 7) < 0.01);
  // From the early reset on, the new window runs from 100% over a full week.
  assert.ok(Math.abs(ctx.paceAt(segs, WEEK, early) - 100) < 1e-9);
  assert.ok(Math.abs(ctx.paceAt(segs, WEEK, early + DAY) - 100 * 6 / 7) < 1e-9);
  assert.equal(ctx.paceAt(segs, WEEK, x2 + 1), null);
});

test("reset times that drift by seconds keep one continuous line", () => {
  const ctx = pace();
  const x = 2000 * DAY, t0 = x - 4 * HOUR, t1 = x - 3 * HOUR;
  const segs = ctx.paceSegments([[t0, x], [t1, x + 2]], 5 * HOUR, x + 30, t1 + 600);
  // The status reset within RESET_DRIFT of the history adds no segment.
  assert.equal(segs.length, 2);
  const before = ctx.paceAt(segs, 5 * HOUR, t1 - 1e-6), after = ctx.paceAt(segs, 5 * HOUR, t1);
  assert.ok(Math.abs(before - after) < 0.05, before + " " + after);
  assert.ok(Math.abs(after - 60) < 0.05);
});

test("no even pace while the reset is unknown, and a new status reset applies from now", () => {
  const ctx = pace();
  const L = 5 * HOUR, x = 3000 * DAY, now = x + 2 * HOUR;
  const segs = ctx.paceSegments([[x - 4 * HOUR, x], [x - HOUR, 0]], L, x + 4 * HOUR, now);
  assert.equal(ctx.paceAt(segs, L, x - 2 * HOUR) > 0, true);
  assert.equal(ctx.paceAt(segs, L, x - 30 * 60), null);
  // From the moment the reset is unknown, not only after it.
  assert.equal(ctx.paceAt(segs, L, x - HOUR), null);
  assert.ok(ctx.paceAt(segs, L, x - HOUR - 1) > 0);
  assert.equal(ctx.paceAt(segs, L, now - 60), null);
  assert.ok(Math.abs(ctx.paceAt(segs, L, now) - 40) < 1e-9);
  assert.ok(Math.abs(ctx.paceAt(segs, L, x + 3 * HOUR) - 20) < 1e-9);
  assert.deepEqual(plain(ctx.paceSegments([], L, 0, now)), []);
  assert.deepEqual(plain(ctx.paceSegments(undefined, L, null, now)), []);
  assert.deepEqual(plain(ctx.paceSegments([[now - HOUR, now + HOUR]], 0, null, now)), []);
});

test("a total has an even pace only while every account has one", () => {
  const ctx = pace();
  const L = WEEK, now = 4000 * DAY;
  const a = { weight: 1, segments: ctx.paceSegments([[now - 2 * DAY, now + 5 * DAY]], L, null, now) };
  const b = { weight: 3, segments: ctx.paceSegments([[now - DAY, now + 6 * DAY]], L, null, now) };
  assert.ok(Math.abs(ctx.pooledPace([a, b], L, now) - (100 * 5 / 7 + 3 * 100 * 6 / 7) / 4) < 1e-9);
  // Before b's line starts, the total is left empty rather than a's alone.
  assert.equal(ctx.pooledPace([a, b], L, now - 1.5 * DAY), null);
  assert.equal(ctx.pooledPace([], L, now), null);
});

test("normal saves use the backend URL defaults and generate a key when enabling MCP", () => {
  let inputs = [];
  const ctx = context(["topKey", "setPath", "buildPatch", "mcpKey"], {
    rawConfig: {}, status: { config: {} },
    $: () => ({ elements: inputs }),
    newMcpKey: () => "llmcp_generated",
  });
  function patch(name, value, type) {
    inputs = [{ name, value, type: type || "text", checked: value, getAttribute: () => null }];
    return JSON.parse(JSON.stringify(ctx.buildPatch(name)));
  }
  assert.deepEqual(patch("cpa_base_url", " http://127.0.0.1:8317/// "), { cpa_base_url: "http://127.0.0.1:8317" });
  assert.deepEqual(patch("cpa_base_url", ""), { cpa_base_url: "http://127.0.0.1:8317" });
  assert.deepEqual(patch("mcp_listen", ""), { mcp_listen: "127.0.0.1:8318" });
  assert.deepEqual(patch("bark_url", "https://api.day.app/test/"), { bark_url: "https://api.day.app/test" });
  ctx.rawConfig.mcp_key = "  ";
  assert.deepEqual(patch("mcp_enabled", true, "checkbox"), { mcp_enabled: true, mcp_key: "llmcp_generated" });
});

test("threshold validation accepts equal levels and rejects reversed levels", () => {
  const elements = {};
  const ctx = context(["thresholdsInOrder"], {
    THRESHOLDS: ["notice_threshold", "low_threshold", "critical_threshold"],
    $: () => ({ elements }),
  });
  function valid(values) {
    ctx.THRESHOLDS.forEach((name, i) => { elements[name] = { value: String(values[i]) }; });
    return ctx.thresholdsInOrder();
  }
  assert.equal(valid([20, 20, 10]), true);
  assert.equal(valid([0, 0, 0]), true);
  assert.equal(valid([10, 20, 10]), false);
});

test("five color levels use raw percentages before display rounding", () => {
  const ctx = context(["quotaThresholds", "quotaLevel"], { LEVEL_EPSILON: 1e-6, status: { config: { notice_threshold: 50, low_threshold: 20 } } });
  assert.equal(ctx.quotaLevel(100), "full");
  assert.equal(ctx.quotaLevel(99.6), "high");
  assert.equal(ctx.quotaLevel(50), "medium");
  assert.equal(ctx.quotaLevel(20), "low");
  assert.equal(ctx.quotaLevel(0), "empty");
});

test("window translations have no duplicate definitions", () => {
  assert.equal((source.match(/window_5h:/g) || []).length, 2);
  assert.equal((source.match(/window_7d:/g) || []).length, 2);
  assert.match(source, /window_5h: "5-hour window"/);
});

test("a queued save keeps the value the user chose, whatever the field shows later", () => {
  const input = { name: "mcp_enabled", value: "on", type: "checkbox", checked: false, getAttribute: () => null };
  const ctx = context(["topKey", "setPath", "buildPatch", "mcpKey"], {
    rawConfig: { mcp_key: "llmcp_x" }, status: { config: {} },
    $: () => ({ elements: [input] }),
    newMcpKey: () => "llmcp_generated",
  });
  // The user turned MCP off; a late confirmation of an earlier save then
  // refilled the switch as on.
  const intent = { inputs: [input], states: [{ value: "on", checked: false }] };
  input.checked = true;
  assert.deepEqual(plain(ctx.buildPatch("mcp_enabled", intent)), { mcp_enabled: false });
  assert.deepEqual(plain(ctx.buildPatch("mcp_enabled")), { mcp_enabled: true });
});

test("the confirmation deadline ends a wait even when no status arrives", async () => {
  const ctx = context(["tokenApplied", "whenApplied", "checkApplied"], {
    APPLY_TIMEOUT: 40, setTimeout, clearTimeout, Promise,
    savedTokens: ["a1", "b2"], applyWaits: [], status: { config_applied_token: "old" },
    // The status requests hang: nothing calls checkApplied.
    pollApplied: () => {},
  });
  const started = Date.now();
  assert.equal(await ctx.whenApplied("a1"), false);
  assert.ok(Date.now() - started < 1000);
  assert.equal(ctx.applyWaits.length, 0);
  // The token reported after the deadline settles nothing.
  ctx.status.config_applied_token = "a1";
  ctx.checkApplied();
  assert.equal(ctx.applyWaits.length, 0);

  ctx.status.config_applied_token = "old";
  const pending = ctx.whenApplied("b2");
  assert.equal(ctx.applyWaits.length, 1);
  ctx.status.config_applied_token = "b2";
  ctx.checkApplied();
  assert.equal(await pending, true);
  assert.equal(ctx.applyWaits.length, 0);
  assert.equal(await ctx.whenApplied("a1"), true);
});

test("a current window whose reset became unknown ends the even pace now", () => {
  const ctx = pace();
  const L = WEEK, now = 5000 * DAY, x = now + 3 * DAY;
  const history = [[now - 2 * DAY, x]];
  // Reset unknown in the status: the line stops now, including the future.
  let segs = ctx.paceSegments(history, L, 0, now);
  assert.ok(ctx.paceAt(segs, L, now - 1) > 0);
  assert.equal(ctx.paceAt(segs, L, now), null);
  assert.equal(ctx.paceAt(segs, L, now + HOUR), null);
  assert.equal(ctx.paceAt(segs, L, x - 1), null);
  // Without the window in the status, the history holds.
  segs = ctx.paceSegments(history, L, null, now);
  assert.ok(ctx.paceAt(segs, L, now + HOUR) > 0);
  segs = ctx.paceSegments(history, L, undefined, now);
  assert.ok(ctx.paceAt(segs, L, now + HOUR) > 0);
  // Already unknown in the history: nothing more to add.
  assert.deepEqual(plain(ctx.paceSegments([[now - DAY, 0]], L, 0, now)), []);
  // A total breaks with the account whose reset became unknown.
  const a = { weight: 1, segments: ctx.paceSegments(history, L, 0, now) };
  const b = { weight: 1, segments: ctx.paceSegments([[now - DAY, now + 6 * DAY]], L, now + 6 * DAY, now) };
  assert.ok(ctx.pooledPace([a, b], L, now - 1) > 0);
  assert.equal(ctx.pooledPace([a, b], L, now), null);
  assert.equal(ctx.pooledPace([a, b], L, now + HOUR), null);
});
