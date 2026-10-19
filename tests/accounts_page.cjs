// Optional browser regression test; see README for the Playwright invocation.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const http = require("node:http");
const path = require("node:path");
const {chromium} = require("playwright");

const html = fs.readFileSync(path.join(__dirname, "../resources/quota_page.html"), "utf8");
const usage = {credits_left: 9, plan: "Go", five_hour: {status: "ok", percent: 10, used: 1, cap: 10}, weekly: {status: "ok", percent: 5, used: 1, cap: 20}};
let cards = [
  {key_id: "a", label: "a@example.test", email: "a@example.test", file_name: "personal.json", disabled: false, status: "active"},
  {key_id: "b", label: "b@example.test", email: "b@example.test", file_name: "commandcode-key-" + "f".repeat(64) + ".json", disabled: true, status: "disabled"},
];
let quotaCalls = 0;
let saveCalls = 0;
let releaseSave;
let releaseQuota;
let saveStarted, cStarted, aStarted;
const savedRequest = new Promise(resolve => {saveStarted = resolve;});
const cRequest = new Promise(resolve => {cStarted = resolve;});
const aRequest = new Promise(resolve => {aStarted = resolve;});
let holdQuota = false;
let failList = false;
let lastSavedKey;
const server = http.createServer(async (req, res) => {
  if (req.method === "GET") {res.setHeader("Content-Type", "text/html; charset=utf-8"); res.end(html); return;}
  let raw = ""; for await (const chunk of req) raw += chunk;
  const body = JSON.parse(raw || "{}");
  const reply = (data, status = 200) => {res.writeHead(status, {"Content-Type": "application/json"}); res.end(JSON.stringify(data));};
  if (req.headers.authorization !== "Bearer browser-test-management") {reply({error: "unauthorized"}, 401); return;}
  if (req.url.endsWith("/accounts")) {
    saveCalls++;
    lastSavedKey = body.api_key;
    if (body.api_key === "duplicate-key") {reply({error: "此 API key 已存在"}, 409); return;}
    await new Promise(resolve => {releaseSave = resolve; saveStarted();});
    cards.push({key_id: "c", label: "new.json", file_name: "new.json", disabled: false, status: "active"});
    reply({file_name: "new.json"}, 201); return;
  }
  if (!body.key_id) {
    if (failList) {reply({error: "测试：无法读取认证文件"}, 502); return;}
    reply({cards, legacy_api_keys_ignored: true}); return;
  }
  quotaCalls++;
  const card = {...cards.find(card => card.key_id === body.key_id)};
  if (holdQuota || body.key_id === "c") await new Promise(resolve => {
    releaseQuota = resolve;
    if (body.key_id === "c") cStarted(); else aStarted();
  });
  reply({...card, email: card.email || "new@example.test", usage});
});

(async () => {
  let browser;
  await new Promise(resolve => server.listen(0, "127.0.0.1", resolve));
  try {
    browser = await chromium.launch({headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH || undefined});
    const page = await browser.newPage({viewport: {width: 1100, height: 850}});
    const errors = [];
    page.on("pageerror", error => errors.push(error.message));
    await page.addInitScript(() => localStorage.setItem("cli-proxy-auth", JSON.stringify({state: {managementKey: "browser-test-management"}})));
    // Mark consumption on the next task, after the page's JSON/Promise handlers.
    await page.addInitScript(() => {
      window.__processedQuotaIDs = [];
      const originalFetch = window.fetch;
      window.fetch = async (...args) => {
        const response = await originalFetch(...args);
        const body = JSON.parse(args[1]?.body || "{}");
        if (body.key_id) {
          const json = response.json.bind(response);
          response.json = async () => {
            const result = await json();
            setTimeout(() => window.__processedQuotaIDs.push(body.key_id), 0);
            return result;
          };
        }
        return response;
      };
    });
    await page.goto("http://127.0.0.1:" + server.address().port);
    await page.waitForFunction(() => document.querySelectorAll(".quota-card").length === 2);
    assert.equal(await page.title(), "CommandCode 账户与额度");
    assert.equal(await page.locator("#api-key").getAttribute("type"), "password");
    assert.match(await page.locator("#app").innerText(), /personal\.json/);
    assert.match(await page.locator("#app").innerText(), /已禁用/);
    assert.match(await page.locator("#legacy-status").innerText(), /不会导入/);
    assert.equal(quotaCalls, 0, "already-known accounts auto-refreshed");
    await page.getByRole("button", {name: "保存", exact: true}).click();
    assert.match(await page.locator("#save-status").innerText(), /请输入/);
    assert.equal(saveCalls, 0);

    await page.locator("#api-key").fill("duplicate-key");
    await page.locator("#save-account").click();
    await page.waitForFunction(() => document.querySelector("#save-status").textContent.includes("已存在"));
    assert.equal(await page.locator(".quota-card").count(), 2);

    await page.locator("#api-key").fill("  browser-new-secret  ");
    await page.locator("#save-account").click();
    await page.waitForFunction(() => document.querySelector("#save-account").disabled);
    assert.equal(await page.locator("#api-key").isDisabled(), true);
    await savedRequest;
    assert.equal(lastSavedKey, "browser-new-secret");
    releaseSave();
    await page.waitForFunction(() => document.querySelectorAll(".quota-card").length === 3);
    await page.waitForFunction(() => document.querySelector("#api-key").value === "");
    assert.match(await page.locator("#save-status").innerText(), /失败不会影响/);
    assert.equal(saveCalls, 2);
    const storage = await page.evaluate(() => JSON.stringify({local: {...localStorage}, session: {...sessionStorage}}));
    assert.equal(storage.includes("browser-new-secret"), false, "upstream API key persisted in browser storage");

    // Delete an account while its automatic quota request is still in flight.
    await cRequest;
    cards = cards.filter(card => card.key_id !== "c");
    await page.locator("#reload-accounts").click();
    await page.waitForFunction(() => document.querySelectorAll(".quota-card").length === 2);
    releaseQuota();
    await page.waitForFunction(() => window.__processedQuotaIDs.includes("c"));
    await page.waitForFunction(() => !document.querySelector("#app").textContent.includes("new.json"));
    assert.equal(await page.evaluate(() => sessionStorage.getItem("commandcode-go-cliproxyapi:quota")?.includes('"c"') || false), false);

    // A slow quota response must not undo a newer enabled/disabled list snapshot.
    holdQuota = true;
    await page.getByRole("button", {name: "Refresh a@example.test", exact: true}).click();
    await page.waitForFunction(() => document.querySelector("#app").textContent.includes("Refreshing..."));
    await aRequest;
    cards = cards.map(card => card.key_id === "a" ? {...card, disabled: true, status: "disabled"} : card);
    await page.locator("#reload-accounts").click();
    await page.waitForFunction(() => document.querySelectorAll(".quota-account .quota-warning").length === 2);
    releaseQuota();
    await page.waitForFunction(() => !document.querySelector("#app").textContent.includes("Refreshing..."));
    assert.equal(await page.locator(".quota-account .quota-warning").count(), 2, "stale quota response reverted disabled state");

    // Narrow layouts must not require horizontal scrolling, including filenames.
    await page.setViewportSize({width: 375, height: 812});
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth), true, "mobile layout overflows");
    await page.evaluate(() => document.documentElement.setAttribute("data-theme", "dark"));
    assert.equal(await page.evaluate(() => getComputedStyle(document.body).color), "rgb(240, 238, 232)");

    // Errors remain visible even when the new list would be empty.
    failList = true;
    await page.locator("#reload-accounts").click();
    await page.waitForFunction(() => document.querySelector("#list-status").textContent.includes("无法读取"));
    assert.match(await page.locator("#list-status").innerText(), /无法读取/);
    assert.deepEqual(errors, []);
    console.log("PASS: browser form, password masking, duplicate/error states, no key storage, no automatic refresh for known accounts, deleted/stale response protection, disabled status, mobile/dark theme.");
  } finally {
    if (browser) await browser.close();
    server.closeAllConnections();
    await new Promise(resolve => server.close(resolve));
  }
})().catch(error => {console.error(error); process.exitCode = 1;});
