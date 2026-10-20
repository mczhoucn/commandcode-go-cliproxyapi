// Browser-only fixtures: no real account, key, official navigation, or CPA auth dir.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const http = require("node:http");
const path = require("node:path");
const {randomBytes} = require("node:crypto");
const {chromium} = require("playwright");
const html = fs.readFileSync(path.join(__dirname,"../resources/oauth_page.html"),"utf8");
const records = new Map(), requests = [], errors = [];
const server = http.createServer(async (req,res) => {
  const url = new URL(req.url,"http://localhost");
  if (req.method === "GET" && url.pathname.endsWith("/oauth")) {
    res.setHeader("Content-Type","text/html; charset=utf-8");res.end(html);return;
  }
  let raw="";for await(const chunk of req) raw+=chunk;
  const data=JSON.parse(raw||"{}");
  requests.push({url:req.url,method:req.method,body:data,auth:req.headers.authorization});
  const reply=(data,status=200)=>{res.writeHead(status,{"Content-Type":"application/json"});res.end(JSON.stringify(data))};
  if(req.headers.authorization!=="Bearer mock-management") return reply({error:"unauthorized"},401);
  const state = data.state || url.searchParams.get("state");
  const record = records.get(state);
  if(!record) return reply({error:"会话无效或已过期"},410);
  if(url.pathname.endsWith("/get-auth-status")) {
    if(record.failSave) return reply({status:"error",error:"Failed to save authentication tokens"});
    if(record.phase==="ready") record.phase="delivered";
    return reply({status:record.phase==="present"?"ok":"wait"});
  }
  if(url.pathname.endsWith("/oauth-session")) {record.hostCancelled=true;return reply({status:"ok",cancelled:true});}
  if(url.pathname.endsWith("/complete")) {
    record.submits++;
    if(record.mode==="invalid") return reply({error:"仅支持有效 GOAT / Pro / Max 订阅"},422);
    if(record.mode==="lost") {req.socket.destroy();return;}
    record.phase="ready";return reply({status:"ready",file_name:"mock-auth.json"},202);
  }
  if(url.pathname.endsWith("/cancel")) {
    record.phase="cancelled";return reply({status:"cancelled"});
  }
  if(url.pathname.endsWith("/session")) {
    const authorization = new URL("https://commandcode.ai/studio/auth/cli");
    authorization.searchParams.set("callback","http://localhost:6000/callback");
    authorization.searchParams.set("state",state);
    return reply({status:record.phase,expires_at:new Date(Date.now()+600000).toISOString(),authorization_url:record.mode==="escaped"?authorization.href.replaceAll("&","&amp;"):authorization.href,file_name:record.phase!=="pending"?"mock-auth.json":undefined});
  }
  reply({error:"unhandled"},404);
});
(async()=>{
  let browser;
  await new Promise(resolve=>server.listen(0,"127.0.0.1",resolve));
  try {
    browser=await chromium.launch({headless:true,executablePath:process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH||undefined});
    const origin="http://127.0.0.1:"+server.address().port;
    async function pageFor(mode="success",prefix="",encrypted=false,remember=true){
      const context=await browser.newContext();
      const page=await context.newPage();
      page.on("pageerror",error=>errors.push(error.message));
      await page.addInitScript(({encrypted,remember})=>{
        window.__credentialSubmissions = 0;
        const fetch = window.fetch;
        window.fetch = (...args) => {
          if (String(args[0]).endsWith("/oauth/complete")) window.__credentialSubmissions++;
          return fetch(...args);
        };
        if(!remember) return;
        const raw=JSON.stringify({state:{managementKey:"mock-management"}});
        let stored=raw;
        if(encrypted){
          const bytes=new TextEncoder().encode(raw),seed=new TextEncoder().encode("cli-proxy-api-webui::secure-storage|"+location.host+"|"+navigator.userAgent);
          for(let i=0;i<bytes.length;i++) bytes[i]^=seed[i%seed.length];
          stored="enc::v1::"+btoa(String.fromCharCode(...bytes));
        }
        localStorage.setItem("cli-proxy-auth",stored);
      },{encrypted,remember});
      const state=randomBytes(32).toString("hex");
      const record={phase:"pending",mode,submits:0};
      records.set(state,record);
      await page.goto(origin+prefix+"/v0/resource/plugins/commandcode-go-cliproxyapi/oauth#state="+state);
      if(remember) await page.waitForFunction(()=>!document.querySelector("#complete").disabled);
      return {page,context,state,record};
    }
    async function noSecretPersistence(page,key){
      const values=await page.evaluate(()=>({local:JSON.stringify(localStorage),session:JSON.stringify(sessionStorage),url:location.href}));
      for(const v of Object.values(values)) assert(!v.includes(key),"credential persisted or entered URL");
      assert(!requests.some(r=>r.url.includes(key)),"credential entered request URL");
    }
    for(const mode of ["success","escaped"]) for(const [prefix,encrypted] of [["",false],["/cpa",true]]){
      const {page,context,state,record}=await pageFor(mode,prefix,encrypted);
      assert.equal(await page.locator("#api-key").getAttribute("type"),"password");
      const official=new URL(await page.locator("#authorization-link").getAttribute("href"));
      assert.equal(official.origin,"https://commandcode.ai");
      assert.equal(official.searchParams.get("callback"),"http://localhost:6000/callback");
      assert.equal(official.searchParams.has("mode"),false);
      assert.equal(official.searchParams.get("state"),state,"official authorization link lost login state");
      const key="fake-browser-only-secret";
      await page.locator("#api-key").fill(key);
      await page.locator("#complete").click();
      await page.waitForFunction(()=>document.querySelector("#status").textContent.includes("等待"));
      assert.equal(record.submits,1);
      assert(!(await page.locator("#status").innerText()).includes("已确认完成"),"premature save success");
      await page.waitForFunction(()=>document.querySelector("#api-key").value==="");
      await noSecretPersistence(page,key);
      record.phase="present";
      await page.waitForFunction(()=>document.querySelector("#status").textContent.includes("已确认完成"));
      assert.equal(await page.locator("#complete").isDisabled(),true);
      assert.equal(record.submits,1);
      if(prefix){
        assert(requests.filter(r=>r.body.state===state || r.url.includes(state)).every(r=>r.url.startsWith("/cpa/")),"proxy prefix lost");
      }
      await context.close();
    }
    {
      const {page,context,record}=await pageFor("invalid");
      await page.locator("#api-key").fill("unsupported-plan-key");
      await page.locator("#complete").click();
      await page.waitForFunction(()=>document.querySelector("#status").textContent.includes("仅支持"));
      assert.equal(await page.locator("#api-key").inputValue(),"");
      await page.locator("#check").click();
      await page.waitForFunction(()=>!document.querySelector("#complete").disabled);
      assert.equal(record.submits,1);
      await context.close();
    }
    {
      const {page,context,record}=await pageFor("lost");
      await page.locator("#api-key").fill("lost-request-key");
      await page.locator("#complete").click();
      await page.waitForFunction(()=>document.querySelector("#status").textContent.includes("重新确认状态"));
      await page.locator("#check").click();
      await page.waitForFunction(()=>document.querySelector("#status").textContent.includes("上次提交结果仍不明确"));
      assert.equal(await page.locator("#complete").isDisabled(),true,"empty state incorrectly allowed resend");
      assert.equal(await page.evaluate(()=>window.__credentialSubmissions),1,"page automatically resubmitted after a lost response");
      await page.locator("#cancel").click();
      await page.waitForFunction(()=>document.querySelector("#status").textContent.includes("已取消"));
      assert.equal(record.phase,"cancelled");
      await context.close();
    }
    {
      const {page,context,record}=await pageFor();
      record.failSave=true;
      await page.locator("#check").click();
      await page.waitForFunction(()=>document.querySelector("#status").textContent.includes("Failed to save"));
      assert.equal(await page.locator("#complete").isDisabled(),true);
      assert.equal(record.submits,0);
      await context.close();
    }
    {
      const {page,context}=await pageFor("success","",false,false);
      assert.match(await page.locator("#status").innerText(),/记住密码/);
      assert.equal(await page.locator("#complete").isDisabled(),true);
      await context.close();
    }
    {
      const {page,context}=await pageFor();
      await page.setViewportSize({width:390,height:844});
      await page.emulateMedia({colorScheme:"dark"});
      assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),"mobile page overflows");
      if(process.env.OAUTH_SCREENSHOT) await page.screenshot({path:process.env.OAUTH_SCREENSHOT,fullPage:true});
      await context.close();
    }
    assert.deepEqual(errors,[]);
    console.log("PASS: OAuth browser fixtures: normal/encrypted management login, proxy prefix, manual credential completion, no early success/no persistence, invalid plan, uncertain request/no resend, cancel, save failure, narrow/dark UI.");
  } finally {if(browser)await browser.close();await new Promise(resolve=>server.close(resolve));}
})().catch(error=>{console.error(error.stack);process.exitCode=1});
