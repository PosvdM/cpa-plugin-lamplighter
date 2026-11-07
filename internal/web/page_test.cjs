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

test("applied settings use redacted values, empty URLs and the MCP presence flag", () => {
  const ctx = context(["sameConfig", "checkApplied"], {
    status: { config: { bark_url: "已设置", webhook: { url: "", method: "POST" }, mcp_key_set: false } },
    unapplied: { bark_url: "https://api.day.app/test", webhook: { url: "", method: "post" }, mcp_key: "llmcp_test" },
  });
  ctx.checkApplied();
  assert.deepEqual(Object.keys(ctx.unapplied), ["mcp_key"]);
  ctx.status.config.mcp_key_set = true;
  ctx.checkApplied();
  assert.deepEqual(Object.keys(ctx.unapplied), []);
  assert.equal(ctx.sameConfig("", "已设置"), false);
  assert.equal(ctx.sameConfig("", ""), true);
  assert.equal(ctx.sameConfig("key", undefined), true);
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
