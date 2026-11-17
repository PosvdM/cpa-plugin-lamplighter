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
    saveHost: () => ({}), saveEntry: (_, input) => ({ input }),
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
    saveQueue: Promise.resolve(), history: null, saveHost: () => ({}), saveEntry: (_, input) => ({ input }),
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
      if (key === "save_field_failed") return "✕ " + args[0] + " save failed";
      if (key === "save_failure_reason") return ": " + args[0];
      if (key === "save_failed_compact") return "✕ Save failed";
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
  return { ctx, heading, row, field, name: field("name", "unsaved name"), weight: field("weight", "2.5") };
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

test("account cards keep only identity in the heading and place actions after the footer plan", () => {
  const cards = saveNode("DIV");
  const element = (tag, attrs = {}, children = []) => {
    const node = saveNode(tag.toUpperCase(), attrs);
    if (attrs.text) node.textContent = attrs.text;
    children.filter(Boolean).forEach(child => node.appendChild(child));
    return node;
  };
  const ctx = context(["renderAccounts"], {
    accountDrag: null, accountsBehind: false, SENSITIVE: "account-sensitive",
    status: { accounts: [
      { name: "a", provider: "codex", title: "GPT#first", email: "first@example.com", plan: "Pro", groups: [] },
      { name: "b", provider: "codex", title: "GPT#second", email: "second@example.com", groups: [] },
    ] },
    rawConfig: {}, masked: false, renderAccountSettings: () => {}, accountWeight: () => 2,
    fmtWeight: String, parseTime: () => null, $: () => cards,
    clear: node => { node.textContent = ""; }, el: element,
    translate: (key, value) => value === undefined ? key : key + ": " + value,
    maskedNodes: text => [element("span", { class: "account-sensitive", text })],
    snapshotButton: label => element("button", { class: "snapshot no-snapshot", "aria-label": label }),
  });
  ctx.renderAccounts();
  assert.equal(cards.children.length, 2);
  cards.children.forEach((card, i) => {
    const heading = card.children[0], footer = card.children.at(-1);
    assert.equal(heading.children.length, 2);
    assert.ok(heading.children.every(node => node.tagName === "SPAN"));
    assert.match(heading.children[0].attrs.class, /type-badge/);
    assert.match(heading.children[1].attrs.class, /account-email account-sensitive/);
    assert.match(heading.children[0].children.at(-1).attrs.class, /account-weight/);
    assert.equal(footer.attrs.class, "account-head cell-note");
    assert.equal(footer.children[0].textContent, i ? "" : "plan: Pro");
    assert.equal(footer.children[1].textContent, "refresh");
    assert.equal(footer.children[2].attrs["aria-label"], "snapshot_card");
    footer.children.slice(1).forEach(button => {
      assert.equal(button.tagName, "BUTTON");
      assert.match(button.attrs.class, /no-snapshot/);
    });
  });
  assert.match(source, /copy.querySelectorAll\("\.no-snapshot,/);
});

function settingsSaveContext(radio = false) {
  const a = saveNode("INPUT", { name: radio ? "recovery_notify.five_hour" : "ignition.start_hour" });
  const b = saveNode("INPUT", { name: radio ? "recovery_notify.five_hour" : "ignition.end_hour" });
  const inputs = [a, b], applied = [], refilled = [];
  inputs.forEach((input, i) => { input.type = "number"; input.value = String(i ? 22 : 7); input.checkValidity = () => true; });
  if (radio) inputs.forEach((input, i) => {
    input.type = "radio"; input.value = i ? "all" : "off"; input.checked = !i;
    input.closest = selector => selector === ".thresholds, .mode-switch" ? { querySelectorAll: () => inputs } : null;
  });
  const ctx = context(["saveField", "captureIntent", "buildPatch", "refillSaved", "noteEdit", "inputTop", "topKey", "setPath", "saveControls"], {
    $: () => ({ elements: inputs }), THRESHOLDS: [], saveQueue: Promise.resolve(),
    rawConfig: { ignition: {} }, status: { config: {} },
    saveHost: () => ({}), claimState: () => () => true,
    patchConfig: async patch => { Object.assign(ctx.rawConfig, patch); return "token"; },
    loadConfig: async () => {},
    showApplied: () => new Promise(resolve => applied.push(resolve)),
    fillSettings: keys => refilled.push(plain(keys)), renderIgnitionOff: () => {},
  });
  const edit = (input, value) => { input.value = value; input.settingsDirty = true; ctx.noteEdit(input); };
  return { ctx, a, b, applied, refilled, edit };
}

test("consecutive saves within one top-level key clear both submitted drafts independently", async () => {
  const { ctx, a, b, applied, refilled, edit } = settingsSaveContext();
  edit(a, "8"); ctx.saveField({ target: a });
  edit(b, "23"); ctx.saveField({ target: b });
  await ctx.saveQueue;
  assert.equal(applied.length, 2);
  applied[0](true); await Promise.resolve();
  assert.equal(a.settingsDirty, false);
  assert.equal(b.settingsDirty, true);
  applied[1](true); await Promise.resolve();
  assert.equal(a.settingsDirty, false);
  assert.equal(b.settingsDirty, false);
  assert.equal(refilled.length, 2);
});

test("confirmation preserves newer drafts and independently failed controls of the same key", async () => {
  const { ctx, a, b, applied, edit } = settingsSaveContext();
  edit(a, "8"); ctx.saveField({ target: a });
  edit(b, "23"); ctx.saveField({ target: b });
  await ctx.saveQueue;
  edit(a, "9");
  applied[0](true); applied[1](true); await Promise.resolve();
  assert.equal(a.settingsDirty, true);
  assert.equal(a.value, "9");
  assert.equal(b.settingsDirty, false);
  b.settingsDirty = true;
  b.setAttribute("aria-invalid", "true");
  edit(a, "10"); ctx.saveField({ target: a });
  await ctx.saveQueue;
  applied[2](true); await Promise.resolve();
  assert.equal(a.settingsDirty, false);
  assert.equal(b.settingsDirty, true);
  assert.equal(b.getAttribute("aria-invalid"), "true");
});

test("a radio selection owns the generations and submitted state of the whole group", async () => {
  const { ctx, a, b, applied } = settingsSaveContext(true);
  a.settingsDirty = true; ctx.noteEdit(a); ctx.saveField({ target: a });
  a.checked = false; b.checked = true;
  b.settingsDirty = true; ctx.noteEdit(b); ctx.saveField({ target: b });
  await ctx.saveQueue;
  applied[0](true); await Promise.resolve();
  assert.equal(a.settingsDirty, true);
  assert.equal(b.settingsDirty, true);
  applied[1](true); await Promise.resolve();
  assert.equal(a.settingsDirty, false);
  assert.equal(b.settingsDirty, false);
  assert.equal(a.checked, false);
  assert.equal(b.checked, true);
});

test("fields skipped by patch construction keep their drafts after confirmation", () => {
  const { ctx, a, b, edit } = settingsSaveContext();
  edit(a, ""); edit(b, "23");
  const intent = ctx.captureIntent(["ignition"]);
  ctx.buildPatch("ignition", intent);
  assert.equal(intent.submitted.includes(a), false);
  ctx.refillSaved(intent);
  assert.equal(a.settingsDirty, true);
  assert.equal(b.settingsDirty, false);
});

test("recreated account inputs inherit drafts and failures and receive later save results", async () => {
  const { ctx, heading, row, field, name } = accountSaveContext("name");
  ctx.saveAccountField(name, row); await ctx.saveQueue;
  const replacement = field("name", "server value");
  ctx.saveEntry(heading, replacement);
  const entry = heading.saveEntries.get("a:name");
  assert.equal(entry.input, replacement);
  assert.equal(entry.controls[0], replacement);
  assert.equal(replacement.value, "unsaved name");
  assert.equal(replacement.getAttribute("aria-invalid"), "true");
  ctx.saveAccountField(replacement, row); await ctx.saveQueue;
  assert.equal(replacement.getAttribute("aria-invalid"), "true");
  ctx.fail = null;
  ctx.saveAccountField(replacement, row); await ctx.saveQueue;
  assert.equal(replacement.hasAttribute("aria-invalid"), false);
  assert.equal(replacement.accountDirty, false);
});

test("pending account saves settle against the current binding after the row is recreated", async () => {
  const { ctx, heading, row, field, name } = accountSaveContext();
  let finish;
  ctx.saveAccountName = () => new Promise(resolve => { finish = resolve; });
  ctx.saveAccountField(name, row);
  await Promise.resolve();
  const replacement = field("name", "server value");
  ctx.saveEntry(heading, replacement);
  assert.equal(replacement.accountPending, 1);
  finish(); await ctx.saveQueue;
  assert.equal(replacement.value, "unsaved name");
  assert.equal(replacement.accountPending, 0);
  assert.equal(replacement.accountDirty, false);
});

test("failed states separate the compact summary, field summary, reason and count", () => {
  const ctx = stateContext(), heading = saveNode("H2");
  ctx.setSaveState(heading, "failed", "first reason", saveNode("INPUT", { name: "first" }));
  ctx.setSaveState(heading, "failed", "second reason", saveNode("INPUT", { name: "long field name" }));
  const parts = Object.fromEntries(heading.children[0].children.map(n => [n.attrs["data-save-part"], n.textContent]));
  assert.equal(parts.compact, "✕ Save failed");
  assert.equal(parts.summary, "✕ long field name save failed");
  assert.equal(parts.reason, ": second reason");
  assert.equal(parts.count, " (1 more failed)");
  const css = fs.readFileSync(path.join(__dirname, "page.css"), "utf8");
  assert.match(css, /@media \(max-width: 640px\)/);
  assert.match(css, /\.save-state\[data-state="failed"\] > \[data-save-part="summary"\], \.save-state > \[data-save-part="reason"\] { display: none; }/);
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

// ---- Events ----

// A var statement of page.js, such as the NOTIFY_TRIGGERS table.
function pageVar(name) {
  const match = source.match(new RegExp("\\n  var " + name + " = [^]*?;\\n"));
  assert.ok(match, name);
  return match[0];
}

const EVENT_FUNCTIONS = ["groupLabel", "retryText", "cutText", "errorSummary", "notifyKind", "eventWindow", "sameSubject",
  "eventSeconds", "eventRows", "notifyTitle", "eventView", "eventParts", "eventMasked", "pushMarks", "parseTime"];

// Elements and text nodes as plain objects; translate shows the key and
// its arguments.
function eventContext(globals = {}) {
  const ctx = context(EVENT_FUNCTIONS, {
    status: { accounts: [] }, masked: false, SENSITIVE: "account-sensitive",
    translate: (key, ...args) => args.length ? key + "(" + args.join("|") + ")" : key,
    fmtDateTime: (t) => t ? t.toISOString().slice(5, 16) : "—",
    pct: (v) => v == null || isNaN(v) ? "—" : Math.round(v) + "%",
    el: (tag, attrs = {}, children = []) => ({ tag, ...attrs, children }),
    document: { createTextNode: (text) => ({ text }) },
    maskedNodes: (text, extra) => {
      // Like page.js: each listed name in its own sensitive span.
      let rest = String(text), out = [];
      const names = (extra || []).filter((n) => n && n.trim()).sort((a, b) => b.length - a.length);
      while (rest) {
        let at = -1, hit = "";
        for (const n of names) { const i = rest.indexOf(n); if (i >= 0 && (at < 0 || i < at)) { at = i; hit = n; } }
        if (at < 0) { out.push({ text: rest }); break; }
        if (at) out.push({ text: rest.slice(0, at) });
        out.push({ tag: "span", class: "account-sensitive", text: hit });
        rest = rest.slice(at + hit.length);
      }
      return out;
    },
    ...globals,
  });
  vm.runInContext(["EVENT_TYPES", "NOTIFY_TRIGGERS", "NOTIFY_LAG", "PAUSE_PAIR", "ERROR_CHARS"].map(pageVar).join("\n"), ctx);
  return ctx;
}

const T0 = Date.UTC(2026, 10, 16, 0, 0, 0) / 1000;
function event(kind, at, fields = {}) {
  return { time: new Date((T0 + at) * 1000).toISOString(), kind, level: "info", ...fields };
}

test("a notification joins the row of the event it reports", () => {
  const ctx = eventContext();
  const g = { group: "codex:a", label: "GPT" };
  const events = [
    event("notify", 70, { ...g, params: { title: "✅ GPT · 7d 已恢复", notify_kind: "recovery", windows: "seven-day" } }),
    event("reset", 60, { ...g, params: { window: "seven-day", kind: "7d", label: "7d", remaining: "100.00" } }),
    event("notify_failed", 40, { ...g, level: "warn", params: { title: "⚠️ GPT 点火已暂停", notify_kind: "circuit", error: "bark: 500" } }),
    event("notify", 39, { ...g, params: { title: "⚠️ GPT 点火已暂停", notify_kind: "circuit" } }),
    event("ignite_paused", 30, { ...g, level: "error", params: { until: "2026-11-17T00:00:00Z", error: "boom", summary: "boom" } }),
    // No event before it: a quota alert stays a row of its own.
    event("notify", 20, { ...g, params: { title: "🟡 GPT · 5h 45%", notify_kind: "quota" } }),
    // Too late for the reset of another group.
    event("notify", 10, { group: "codex:b", label: "GPT 2", params: { title: "✅ GPT 2 · 7d 已恢复", notify_kind: "recovery" } }),
  ];
  const built = ctx.eventRows(events, false);
  assert.deepEqual(plain(built.rows.map((r) => r.event.kind + "@" + (r.event.params.title || ""))), [
    "reset@", "ignite_paused@", "notify@🟡 GPT · 5h 45%", "notify@✅ GPT 2 · 7d 已恢复"]);
  assert.deepEqual(plain(built.rows[0].pushes.map((ev) => ev.kind)), ["notify"]);
  assert.deepEqual(plain(built.rows[1].pushes.map((ev) => ev.kind).sort()), ["notify", "notify_failed"]);
  const marks = ctx.pushMarks(built.rows[1].pushes);
  assert.deepEqual(plain(marks.map((m) => m.text)), ["ev_notify", "ev_notify_failed"]);
  assert.match(marks[1].title, /bark: 500/);
  assert.equal(marks[1].class, "event-mark failed");
  // A standalone quota alert has its own badge, without the repeated label.
  const view = ctx.eventView(built.rows[2].event, built.rows[2].notifyKind);
  assert.equal(view.badge, "ev_notify_quota");
  assert.equal(view.detail, "🟡 5h 45%");
  // An unmatched recovery falls back to the plain badge.
  assert.equal(ctx.eventView(built.rows[3].event, built.rows[3].notifyKind).badge, "ev_notify");
});

test("matching needs the same group, the order, the time limit and the windows", () => {
  const ctx = eventContext();
  const g = { group: "k", label: "Gemini" };
  const reset = (at, window) => event("reset", at, { ...g, params: { window, remaining: "100.00" } });
  const recovery = (at, windows) => event("notify", at, { ...g, params: { title: "✅ Gemini · 7d 已恢复", notify_kind: "recovery", windows } });
  // Sent before the event, or more than 70 minutes after it, as retries
  // can be: no match.
  assert.equal(ctx.eventRows([reset(100, "gemini-weekly"), recovery(99, "gemini-weekly")], true).rows.length, 2);
  assert.equal(ctx.eventRows([recovery(4301, "gemini-weekly"), reset(100, "gemini-weekly")], true).rows.length, 2);
  assert.equal(ctx.eventRows([recovery(4300, "gemini-weekly"), reset(100, "gemini-weekly")], true).rows.length, 1);
  // The window must be one the notification names.
  let built = ctx.eventRows([recovery(110, "gemini-weekly"), reset(100, "gemini-5h")], true);
  assert.equal(built.rows.length, 2);
  // A notification for two windows marks both resets.
  built = ctx.eventRows([recovery(110, "gemini-5h,gemini-weekly"), reset(100, "gemini-5h"), reset(100, "gemini-weekly")], true);
  assert.equal(built.rows.length, 2);
  assert.ok(built.rows.every((r) => r.pushes.length === 1));
  // Hidden 5-hour resets take their notifications with them.
  built = ctx.eventRows([recovery(110, "gemini-5h"), reset(100, "gemini-5h"), reset(50, "gemini-weekly")], false);
  assert.deepEqual(plain(built.rows.map((r) => r.event.params.window)), ["gemini-weekly"]);
  assert.equal(built.hidden, 1);
  assert.equal(built.fiveHour, 1);
  // Older notifications have no group: their label decides.
  built = ctx.eventRows([
    event("notify", 20, { label: "Gemini", params: { title: "⚠️ Gemini 额度已恢复，CPA 仍在冷却" } }),
    event("cooldown_stale", 10, { label: "Gemini", params: { until: "2026-11-17T00:00:00Z" } }),
  ], false);
  assert.equal(built.rows.length, 1);
  assert.equal(built.rows[0].event.kind, "cooldown_stale");
});

test("older notifications are told by their titles", () => {
  const ctx = eventContext();
  const kind = (title, kindName = "notify") => ctx.notifyKind({ kind: kindName, params: { title } });
  assert.equal(kind("✅ GPT · 7d 已恢复"), "recovery");
  assert.equal(kind("✅ GPT · 7d recovered"), "recovery");
  assert.equal(kind("⚠️ Gemini 点火已暂停"), "circuit");
  assert.equal(kind("⚠️ Gemini ignition paused"), "circuit");
  assert.equal(kind("🎫 GPT · 套餐 plus → free"), "plan");
  assert.equal(kind("⚠️ GPT 额度已恢复，CPA 仍在冷却"), "cooldown");
  assert.equal(kind("⚠️ GPT quota is back, CPA still cooling down"), "cooldown");
  assert.equal(kind("🟡 GPT · 5h 45%"), "quota");
  assert.equal(kind("🔴 GPT · 5h 5%"), "quota");
  assert.equal(kind("⏰ GPT · 5h 重置提醒"), "reminder");
  // Codex reset signals also start with ✅ but report no reset of an account.
  assert.equal(kind("✅ Codex 全局重置已完成"), "codex_reset");
  assert.equal(kind("🕯️ Lamplighter 测试通知"), "");
  // Failed notifications recorded without params.
  assert.equal(ctx.notifyKind({ kind: "notify_failed", detail: "✅ GPT · 7d 已恢复：timeout" }), "recovery");
  assert.equal(ctx.notifyKind({ kind: "notify", params: { title: "x", notify_kind: "suspended" } }), "suspended");
});

test("resets and exhaustion name the window by its short label", () => {
  const ctx = eventContext();
  assert.deepEqual(plain(ctx.eventWindow({ window: "five-hour" })), { kind: "5h", label: "5h" });
  assert.deepEqual(plain(ctx.eventWindow({ window: "gemini-5h" })), { kind: "5h", label: "5h" });
  assert.deepEqual(plain(ctx.eventWindow({ window: "seven-day" })), { kind: "7d", label: "7d" });
  assert.deepEqual(plain(ctx.eventWindow({ window: "3p-weekly" })), { kind: "7d", label: "7d" });
  assert.deepEqual(plain(ctx.eventWindow({ window: "seven-day-opus" })), { kind: "", label: "seven-day-opus" });
  assert.deepEqual(plain(ctx.eventWindow({ window: "seven-day-opus", kind: "", label: "Opus 7d" })), { kind: "", label: "Opus 7d" });
  let view = ctx.eventView(event("reset", 0, { label: "GPT", params: { window: "seven-day", remaining: "100.00", reset: "2026-11-23T00:00:00Z" } }));
  assert.equal(view.badge, "ev_reset");
  assert.equal(view.style, "success");
  assert.equal(view.detail, "ev_window_next(7d|100%|11-23T00:00)");
  view = ctx.eventView(event("exhausted", 0, { label: "GPT", params: { window: "five-hour", kind: "5h", label: "5h", remaining: "0.00", reset: "2026-11-16T05:00:00Z" } }));
  assert.equal(view.badge, "ev_exhausted");
  assert.equal(view.detail, "ev_window_due(5h|0%|11-16T05:00)");
  view = ctx.eventView(event("reset", 0, { label: "GPT", params: { window: "seven-day", remaining: "100.00" } }));
  assert.equal(view.detail, "ev_window_left(7d|100%)");
  // Exhaustion of a 5-hour window always shows.
  const built = ctx.eventRows([event("exhausted", 0, { params: { window: "five-hour", remaining: "0" } })], false);
  assert.equal(built.rows.length, 1);
});

test("error texts shrink to one line and stay masked", () => {
  const ctx = eventContext();
  const google = 'host_call_failed: {\n  "error": {\n    "code": 403,\n    "message": "The caller does not have permission for alice@example.com",\n' +
    '    "status": "PERMISSION_DENIED",\n    "details": [{ "@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": "TOS_VIOLATION" }]\n  }\n} (HTTP 403)';
  assert.equal(ctx.errorSummary(google), "host_call_failed: HTTP 403 · TOS_VIOLATION · The caller does not have permission for alice@example.com");
  assert.equal(ctx.errorSummary('{"error":{"message":"Rate limit reached","type":"rate_limit_error","code":"rate_limit_exceeded"}} (HTTP 429)'),
    "HTTP 429 · rate_limit_error · Rate limit reached");
  assert.equal(ctx.errorSummary('刷新失败：{"error":"invalid_grant","error_description":"Token has been expired"}'), "刷新失败：invalid_grant · Token has been expired");
  assert.equal(ctx.errorSummary("dial tcp: timeout\nmore lines"), "dial tcp: timeout");
  assert.equal(ctx.errorSummary("{not json\nsecond"), "{not json");
  const long = ctx.errorSummary("x".repeat(400));
  assert.equal(Array.from(long).length, 160);
  assert.ok(long.endsWith("…"));

  const ev = event("ignite_failed", 0, { group: "g", label: "alice", params: { retry_seconds: "300", error: google } });
  const view = ctx.eventView(ev);
  assert.ok(!view.detail.includes("\n"));
  assert.ok(view.detail.includes("TOS_VIOLATION"));
  assert.ok(view.tip.includes('"code": 403'));
  // The summary from the backend wins over the page's own.
  assert.ok(ctx.eventView({ ...ev, params: { ...ev.params, summary: "账号被封禁" } }).detail.includes("账号被封禁"));
  // Masked, neither the label nor the error leaves a plain text node.
  const nodes = ctx.eventMasked(ev, view);
  const plainText = (list) => list.filter((n) => n.class !== "account-sensitive").map((n) => n.text || "").join("");
  assert.equal(plainText(nodes.label), "");
  assert.ok(!plainText(nodes.detail).includes("alice"), plainText(nodes.detail));
  assert.ok(!plainText(nodes.detail).includes("TOS_VIOLATION"));
  // The tooltips of notification marks are left out while masked.
  ctx.masked = true;
  const marks = ctx.pushMarks([event("notify_failed", 0, { params: { title: "⚠️ alice 点火已暂停", error: "x" } })]);
  assert.equal(marks[0].title, null);
});

test("credential events show the reason, a summary and an appeal link", () => {
  const ctx = eventContext();
  const invalid = (params) => ctx.eventView(event("credential_invalid", 0, { level: "error", label: "Gemini", params }));
  let view = invalid({ reason: "suspended", status: "403", code: "TOS_VIOLATION", summary: "账号因违反服务条款被停用", appeal_url: "https://support.example.com/appeal", source: "ignite" });
  assert.equal(view.badge, "ev_credential_suspended");
  assert.equal(view.style, "failure");
  // Found by ignition: the row says ignition is paused.
  assert.equal(view.detail, "ev_ignite_paused_note(账号因违反服务条款被停用)");
  assert.equal(view.link, "https://support.example.com/appeal");
  // The tooltip shows the status, code and summary, and the appeal address.
  assert.equal(view.tip, "HTTP 403 · TOS_VIOLATION · 账号因违反服务条款被停用\nhttps://support.example.com/appeal");
  assert.ok(view.secrets.includes("账号因违反服务条款被停用"));
  view = invalid({ reason: "auth", summary: "", status: "401", appeal_url: "javascript:alert(1)" });
  assert.equal(view.badge, "ev_credential_invalid");
  assert.equal(view.detail, "HTTP 401");
  assert.equal(view.link, "");
  view = invalid({ reason: "rate_limited", summary: "请求过多", until: "2026-11-16T08:00:00Z" });
  assert.equal(view.badge, "ev_credential_rate_limited");
  assert.equal(view.detail, "ev_limited_until(请求过多|11-16T08:00)");
  view = ctx.eventView(event("credential_restored", 0, { label: "Gemini", params: { source: "query" } }));
  assert.equal(view.badge, "ev_credential_restored");
  assert.equal(view.style, "success");
  assert.equal(view.detail, "ev_source_query");
  // A credential notification joins its event.
  const built = ctx.eventRows([
    event("notify", 5, { group: "k", label: "Gemini", params: { title: "⚠️ Gemini", notify_kind: "suspended" } }),
    event("credential_invalid", 0, { group: "k", label: "Gemini", params: { reason: "suspended" } }),
    event("notify", 5, { group: "j", label: "Claude", params: { title: "✅ Claude", notify_kind: "credential_ok" } }),
    event("credential_restored", 0, { group: "j", label: "Claude", params: { source: "query" } }),
  ], false);
  assert.deepEqual(plain(built.rows.map((r) => [r.event.kind, r.pushes.length])), [["credential_invalid", 1], ["credential_restored", 1]]);
});

// ---- Even pace extended back to the cycle start ----

function extension() {
  return context(["paceSegments", "paceAt", "pooledPace", "paceExtensions", "extendedPace"], { RESET_DRIFT: 60, RESET_JUMP: 5 });
}

// Synthetic samples shaped like a 7-day window whose quota came back to
// 100% at rise and whose scheduled reset moved back to an earlier time at
// move: [time, remaining].
function movedReset() {
  const rise = 6000 * DAY, x1 = rise + WEEK + 4 * 60 + 14, x2 = rise + 6 * DAY - 52 * 60, move = rise + 16.5 * HOUR;
  const points = [[rise - 2 * HOUR, 22], [rise - HOUR, 21], [rise, 100], [rise + 6 * HOUR, 91], [move - 2 * HOUR, 65], [move, 21], [move + HOUR, 20]];
  // The history records a change of the reset only beyond 60 s; the
  // reset wavered between x1 and x1 + 1 meanwhile.
  const resets = [[rise - 3 * DAY, x2], [rise, rise + WEEK], [rise + 4 * 60, x1], [move, x2]];
  return { rise, x1, x2, move, points, resets };
}

test("a reset that moves within its cycle extends the new line back to the cycle start", () => {
  const ctx = extension();
  const d = movedReset();
  const segs = ctx.paceSegments(d.resets, WEEK, d.x2, d.move + 2 * HOUR);
  const ext = ctx.paceExtensions(segs, d.points, WEEK);
  assert.deepEqual(plain(ext), [[d.rise, d.move, d.x2]]);
  // The old line stays: just before the move it follows the reset x1.
  assert.ok(Math.abs(ctx.paceAt(segs, WEEK, d.move - 1) - 100 * (d.x1 - d.move + 1) / WEEK) < 1e-6);
  // The extension runs the line of x2 back to the rise.
  const part = { weight: 1, segments: segs, extensions: ext };
  assert.ok(Math.abs(ctx.extendedPace([part], WEEK, d.rise) - 100 * (d.x2 - d.rise) / WEEK) < 1e-9);
  assert.ok(Math.abs(ctx.extendedPace([part], WEEK, d.move) - ctx.paceAt(segs, WEEK, d.move)) < 1e-9);
  // Outside the extension there is none.
  assert.equal(ctx.extendedPace([part], WEEK, d.rise - 1), null);
  assert.equal(ctx.extendedPace([part], WEEK, d.move + 1), null);
});

test("resets that waver by a second extend nothing", () => {
  const ctx = extension();
  const d = movedReset();
  // The same cycle recorded with every one-second change, as a backend
  // without the 60 s tolerance would: x1, x1 + 1, x1, x1 + 1.
  const resets = [[d.rise, d.rise + WEEK], [d.rise + 4 * 60, d.x1], [d.rise + 6 * HOUR, d.x1 + 1], [d.rise + 8 * HOUR, d.x1], [d.rise + 9 * HOUR, d.x1 + 1]];
  const segs = ctx.paceSegments(resets, WEEK, d.x1, d.rise + 10 * HOUR);
  assert.deepEqual(plain(ctx.paceExtensions(segs, d.points, WEEK)), []);
  // The status reset that wavers adds no segment either.
  assert.equal(ctx.paceSegments(resets, WEEK, d.x1 + 1, d.rise + 10 * HOUR).length, segs.length);
  // A line that starts with its cycle, or an early reset, extends nothing.
  const early = d.rise + 2 * DAY;
  assert.deepEqual(plain(ctx.paceExtensions(ctx.paceSegments([[d.rise, d.x1], [early, early + WEEK]], WEEK, null, early + HOUR),
    [[d.rise, 100], [early - HOUR, 40], [early, 100]], WEEK)), []);
  // Without a rise in the samples, the start of the cycle is not known.
  assert.deepEqual(plain(ctx.paceExtensions(ctx.paceSegments([[d.move, d.x2]], WEEK, null, d.move + HOUR), [[d.move, 21]], WEEK)), []);
});

test("a total extends where an account does, with the other accounts' lines", () => {
  const ctx = extension();
  const d = movedReset();
  const segsA = ctx.paceSegments(d.resets, WEEK, d.x2, d.move + HOUR);
  const a = { weight: 1, segments: segsA, extensions: ctx.paceExtensions(segsA, d.points, WEEK) };
  const xb = d.rise + 3 * DAY;
  const segsB = ctx.paceSegments([[xb - WEEK, xb]], WEEK, xb, d.move + HOUR);
  const b = { weight: 3, segments: segsB, extensions: ctx.paceExtensions(segsB, [], WEEK) };
  const t = d.rise + HOUR;
  const want = (100 * (d.x2 - t) / WEEK + 3 * 100 * (xb - t) / WEEK) / 4;
  assert.ok(Math.abs(ctx.extendedPace([a, b], WEEK, t) - want) < 1e-9);
  assert.equal(ctx.extendedPace([b], WEEK, t), null);
});

test("the detail heading moves left of the plot only where it fits", () => {
  const ctx = context(["placeDetailHead"], { SIDE_GAP: 46 });
  const wrap = () => {
    const classes = new Set(), style = {};
    return { classList: { add: (c) => classes.add(c), remove: (c) => classes.delete(c) }, style: { setProperty: (k, v) => { style[k] = v; } }, classes, css: style };
  };
  const box = { clientWidth: 1000, offsetHeight: 206 };
  let w = wrap();
  ctx.placeDetailHead(w, { offsetHeight: 180 }, box, { labelW: 190, width: 1000 });
  assert.ok(w.classes.has("side"));
  assert.equal(w.css["--detail-side"], "144px");
  // Too narrow: the blank of a chart below 640 pixels.
  w = wrap();
  ctx.placeDetailHead(w, { offsetHeight: 100 }, { clientWidth: 600, offsetHeight: 206 }, { labelW: 120, width: 600 });
  assert.ok(!w.classes.has("side"));
  // Too tall, such as a long name over several lines.
  w = wrap();
  ctx.placeDetailHead(w, { offsetHeight: 240 }, box, { labelW: 190, width: 1000 });
  assert.ok(!w.classes.has("side"));
});

test("manual ignition failures show no retry, and credential pauses no end time", () => {
  const ctx = eventContext();
  const json = '{"error":{"code":401,"message":"token expired","status":"UNAUTHENTICATED"}}';
  let view = ctx.eventView(event("ignite_failed", 0, { label: "GPT", params: { manual: "1", error: json, summary: "凭证已失效" } }));
  assert.equal(view.detail, "凭证已失效");
  assert.equal(view.tip, json);
  view = ctx.eventView(event("ignite_failed", 0, { label: "GPT", params: { manual: "1", error: json } }));
  assert.equal(view.detail, "HTTP 401 · UNAUTHENTICATED · token expired");
  assert.ok(view.secrets.includes(json));
  // Automatic failures keep the retry; older manual failures without params
  // keep their stored text.
  assert.match(ctx.eventView(event("ignite_failed", 0, { label: "GPT", params: { retry_seconds: "300", error: "x" } })).detail, /^ev_retry\(/);
  assert.equal(ctx.eventView(event("ignite_failed", 0, { label: "GPT", detail: "点火失败：x" })).detail, "点火失败：x");

  const paused = (params) => ctx.eventView(event("ignite_paused", 0, { level: "error", label: "Gemini", params }));
  assert.equal(paused({ reason: "auth", error: json, summary: "凭证已失效" }).detail, "ev_paused_auth(凭证已失效)");
  view = paused({ reason: "suspended", error: json });
  assert.equal(view.detail, "ev_paused_suspended(HTTP 401 · UNAUTHENTICATED · token expired)");
  assert.equal(view.tip, "ev_paused_suspended(" + json + ")");
  assert.equal(paused({ reason: "rate_limited", until: "2026-11-17T00:00:00Z", error: "x", summary: "请求过多" }).detail, "ev_paused(11-17T00:00|请求过多)");
  assert.equal(paused({ until: "2026-11-17T00:00:00Z", error: "boom" }).detail, "ev_paused(11-17T00:00|boom)");
  // Without an end time or a known reason, no "until —".
  assert.equal(paused({ error: "boom" }).detail, "boom");
});

test("a credential problem shares its row with the ignition pause and the notifications", () => {
  const ctx = eventContext();
  const g = { group: "claude:k", label: "Claude" };
  const until = "2026-11-16T23:00:00Z";
  const events = [
    event("notify", 1800, { ...g, params: { title: "⏳ Claude 被限流", notify_kind: "rate_limited" } }),
    event("notify_failed", 30, { ...g, level: "warn", params: { title: "⚠️ Claude 点火已暂停", notify_kind: "circuit", error: "Bark: 500" } }),
    event("credential_invalid", 0, { ...g, level: "error", params: { reason: "rate_limited", status: "429", summary: "请求过多", until, source: "ignite" } }),
    event("ignite_paused", 0, { ...g, level: "error", params: { until, error: '{"error":{"code":429,"message":"Number of requests has exceeded your rate limit"}}', summary: "请求过多" } }),
    // Another group's pause stays its own row.
    event("ignite_paused", 10, { group: "gemini:k", label: "Gemini", level: "error", params: { until, error: "x" } }),
  ];
  const built = ctx.eventRows(events, false);
  assert.deepEqual(plain(built.rows.map((r) => r.event.kind + ":" + r.event.label)), ["credential_invalid:Claude", "ignite_paused:Gemini"]);
  const row = built.rows[0];
  assert.equal(row.paused.kind, "ignite_paused");
  // The rate limit notification half an hour later and the circuit
  // notification of the pause both join it.
  assert.deepEqual(plain(row.pushes.map((ev) => ev.params.notify_kind).sort()), ["circuit", "rate_limited"]);
  const view = ctx.eventView(row.event, row.notifyKind, row.paused);
  assert.equal(view.badge, "ev_credential_rate_limited");
  assert.equal(view.detail, "ev_ignite_paused_until(请求过多|11-16T23:00)");
  // The tooltip adds the full error of the pause on a line of its own.
  assert.equal(view.tip, 'HTTP 429 · 请求过多\n{"error":{"code":429,"message":"Number of requests has exceeded your rate limit"}}');
  // A pause more than PAUSE_PAIR seconds away is a row of its own.
  assert.equal(ctx.eventRows([events[2], { ...events[3], time: new Date((T0 - 61) * 1000).toISOString() }], false).rows.length, 2);
  // Found by a query, without a pause: the rate limit alone.
  assert.equal(ctx.eventView(event("credential_invalid", 0, { ...g, params: { reason: "rate_limited", summary: "请求过多", until, source: "query" } })).detail,
    "ev_limited_until(请求过多|11-16T23:00)");
  // An account suspended at ignition: one row too.
  const suspended = ctx.eventRows([
    event("credential_invalid", 0, { ...g, params: { reason: "suspended", summary: "停用", source: "ignite" } }),
    event("ignite_paused", 0, { ...g, params: { reason: "suspended", error: "x", summary: "停用" } }),
  ], false);
  assert.equal(suspended.rows.length, 1);
  const merged = ctx.eventView(suspended.rows[0].event, undefined, suspended.rows[0].paused);
  assert.equal(merged.detail, "ev_ignite_paused_note(停用)");
  // With an appeal address, the pause's error follows it; a pause without
  // an error adds nothing.
  const appeal = { reason: "suspended", status: "403", summary: "停用", appeal_url: "https://appeal.example.com/", source: "ignite" };
  const withPause = (error) => ctx.eventView(event("credential_invalid", 0, { ...g, params: appeal }), undefined,
    event("ignite_paused", 0, { ...g, params: error ? { reason: "suspended", error, summary: "停用" } : { reason: "suspended", summary: "停用" } }));
  assert.equal(withPause("raw").tip, "HTTP 403 · 停用\nhttps://appeal.example.com/\nraw");
  assert.equal(withPause("").tip, "HTTP 403 · 停用\nhttps://appeal.example.com/");
});

test("Did Codex Reset notifications join the event recorded after them", () => {
  const ctx = eventContext();
  const signal = (at) => event("codex_reset", at, { label: "Did Codex Reset", params: { count: "1" } });
  const push = (at, fields = {}) => event("notify", at, { params: { title: "✅ Codex 全局重置已完成", notify_kind: "codex_reset" }, ...fields });
  let built = ctx.eventRows([signal(10), push(5), push(0, { params: { title: "📅 Codex 全局重置已排期" } })], false);
  assert.equal(built.rows.length, 1);
  assert.equal(built.rows[0].event.kind, "codex_reset");
  assert.equal(built.rows[0].pushes.length, 2);
  // More than five minutes apart: the notification stays a row, under the
  // label of Did Codex Reset.
  built = ctx.eventRows([signal(400), push(0)], false);
  assert.equal(built.rows.length, 2);
  const view = ctx.eventView(built.rows[1].event, built.rows[1].notifyKind);
  assert.equal(view.label, "Did Codex Reset");
  assert.equal(view.detail, "✅ Codex 全局重置已完成");
});

test("older failure events without params hide their whole detail when masked", () => {
  const ctx = eventContext();
  const plainText = (list) => list.filter((n) => n.class !== "account-sensitive").map((n) => n.text || "").join("");
  // A manual ignition failure from before params, naming an account that
  // has since been removed: no name list can catch it.
  const old = [
    event("ignite_failed", 0, { group: "claude:k", label: "Claude", level: "warn", detail: "手动点火失败：permission denied for gone@example.com" }),
    event("ignite_paused", 0, { group: "claude:k", label: "Claude", level: "error", detail: "暂停到 11/17：gone@example.com" }),
    event("notify_failed", 0, { group: "claude:k", label: "Claude", level: "warn", detail: "Bark：gone@example.com" }),
    event("error", 0, { level: "error", label: "Claude", detail: "额度查询失败：gone@example.com" }),
  ];
  for (const ev of old) {
    const view = ctx.eventView(ev);
    assert.ok(view.whole, ev.kind);
    const nodes = ctx.eventMasked(ev, view);
    assert.equal(plainText(nodes.detail), "", ev.kind);
    assert.ok(nodes.detail.some((n) => n.text.includes("gone@example.com")), ev.kind);
  }
  // An older success keeps its detail, with only the names hidden.
  const ok = event("ignite", 0, { group: "claude:k", label: "Claude", detail: "claude-haiku · 下次重置 11/17" });
  assert.equal(ctx.eventView(ok).whole, false);
  assert.equal(plainText(ctx.eventMasked(ok).detail), "claude-haiku · 下次重置 11/17");
  // An older success without a stored label hides whole too, even when its
  // group gives a label now: the account may have been renamed since.
  const ctx2 = eventContext({ status: { accounts: [{ groups: [{ key: "claude:k", label: "Claude Now" }] }] } });
  const unlabeled = event("ignite", 0, { group: "claude:k", message: "Claude Old 点火成功（claude-haiku），下次重置 11/17" });
  const view = ctx2.eventView(unlabeled);
  assert.equal(view.label, "Claude Now");
  assert.ok(view.whole);
  const nodes = ctx2.eventMasked(unlabeled, view);
  assert.equal(plainText(nodes.detail), "");
  assert.ok(nodes.detail.some((n) => n.text.includes("Claude Old")));
  // Events with params keep the masking by names and error texts.
  assert.equal(ctx.eventView(event("ignite_failed", 0, { label: "Claude", params: { manual: "1", error: "x", summary: "y" } })).whole, false);
});
