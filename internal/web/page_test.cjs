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
