// Opt-in native-host integration. All config/auth data live in a NEW temp dir.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const http = require("node:http");
const {spawn} = require("node:child_process");
const {createHash} = require("node:crypto");
const binary=process.env.CPA_HOST_BINARY,library=process.env.CPA_PLUGIN_LIBRARY;
if(!binary||!library) throw new Error("Set CPA_HOST_BINARY and CPA_PLUGIN_LIBRARY to native binaries");
const root=fs.mkdtempSync(path.join(os.tmpdir(),"commandcode-oauth-host-"));
const id="commandcode-go-cliproxyapi", mgmt="/v0/management", plugin=mgmt+"/plugins/"+id;
let child,log="",lastKey="";
const upstream=http.createServer(async(req,res)=>{
  for await(const _ of req) {}
  const key=(req.headers.authorization||"").replace(/^Bearer /,"");
  const reply=(data,status=200)=>{res.writeHead(status,{"Content-Type":"application/json"});res.end(JSON.stringify(data));};
  if(req.url.startsWith("/alpha/whoami")){
    if(key==="native-invalid")return reply({error:"invalid"},401);
    return reply({success:true,user:{email:"native@example.test"}});
  }
  if(req.url.endsWith("/subscriptions")){
    const plan=key==="native-go"?"individual-go":key==="native-pro"?"individual-pro-v1":key==="native-max"?"individual-max":"individual-goat";
    return reply({success:true,data:{planId:plan,status:"active",currentPeriodEnd:"2099-01-01T00:00:00Z"}});
  }
  if(req.url.endsWith("/models"))return reply({data:[{id:"native-model"}]});
  if(req.url.endsWith("/chat/completions")){
    lastKey=key;
    return reply({id:"chatcmpl-native",object:"chat.completion",model:"native-model",choices:[{index:0,message:{role:"assistant",content:"ok"},finish_reason:"stop"}],usage:{prompt_tokens:1,completion_tokens:1,total_tokens:2}});
  }
  reply({error:"not found"},404);
});
const delay=ms=>new Promise(resolve=>setTimeout(resolve,ms));
async function until(fn,message){
  for(let i=0;i<100;i++){const v=await fn().catch(()=>false);if(v)return v;await delay(100);}
  throw new Error(message);
}
(async()=>{
  let browser;
  await new Promise(resolve=>upstream.listen(0,"127.0.0.1",resolve));
  const portProbe=http.createServer();
  await new Promise(resolve=>portProbe.listen(0,"127.0.0.1",resolve));
  const port=portProbe.address().port;
  await new Promise(resolve=>portProbe.close(resolve));
  const base="http://127.0.0.1:"+port;
  const authDir=path.join(root,"auths"),pluginDir=path.join(root,"plugins",process.platform,process.arch==="x64"?"amd64":process.arch);
  fs.mkdirSync(authDir,{recursive:true});fs.mkdirSync(pluginDir,{recursive:true});fs.mkdirSync(path.join(root,"home"));
  fs.copyFileSync(library,path.join(pluginDir,id+path.extname(library)));
  const config=path.join(root,"config.yaml");
  fs.writeFileSync(config,[
    "host: 127.0.0.1","port: "+port,
    "remote-management:","  allow-remote: false","  secret-key: test-management-key","  disable-control-panel: true",
    "auth-dir: "+JSON.stringify(authDir),"api-keys: [test-client-key]",
    "plugins:","  enabled: true","  dir: "+JSON.stringify(path.join(root,"plugins")),"  configs:","    "+id+":",
    "      enabled: true","      allow-http: true","      base-url: http://127.0.0.1:"+upstream.address().port+"/provider/v1",
    "      api-keys: [{value: retired-key-never-import}]",""
  ].join("\n"),{mode:0o600});
  async function api(url,method="GET",data,authorized=true){
    const response=await fetch(base+url,{method,headers:{"Content-Type":"application/json",...(authorized?{Authorization:"Bearer test-management-key"}:{})},body:data===undefined?undefined:JSON.stringify(data),signal:AbortSignal.timeout(20000)});
    return {status:response.status,data:await response.json().catch(()=>({}))};
  }
  async function begin(){
    const r=await api(mgmt+"/commandcode-auth-url?is_webui=true");
    assert.equal(r.status,200,JSON.stringify(r));
    assert.equal(r.data.status,"ok");
    assert.match(r.data.state,/^[a-f0-9]{64}$/);
    return r.data;
  }
  async function complete(start,key){
    const r=await api(plugin+"/oauth/complete","POST",{state:start.state,api_key:key});
    assert.equal(r.status,202,JSON.stringify(r));
    return r;
  }
  async function finish(start){
    return until(async()=>{
      const r=await api(mgmt+"/get-auth-status?state="+start.state);
      assert.notEqual(r.data.status,"error",JSON.stringify(r));
      return r.data.status==="ok";
    },"native host did not confirm save");
  }
  function authFiles(){return fs.readdirSync(authDir).filter(f=>f.endsWith(".json"));}
  function startHost(){
    child=spawn(binary,["--config",config,"--local-model"],{cwd:root,env:{PATH:process.env.PATH,HOME:path.join(root,"home"),TMPDIR:process.env.TMPDIR||"/tmp",LANG:"en_US.UTF-8"},stdio:["ignore","pipe","pipe"]});
    child.stdout.on("data",data=>{log+=data});child.stderr.on("data",data=>{log+=data});
  }
  async function stopHost(){
    if(!child||child.exitCode!==null)return;
    const done=new Promise(resolve=>child.once("exit",resolve));
    child.kill("SIGTERM");
    await Promise.race([done,delay(8000).then(()=>{throw new Error("isolated host did not exit")})]);
  }
  try{
    startHost();
    await until(async()=>(await api(plugin+"/quota-usage","POST",{})).status===200,"host/plugin startup failed");
    assert.equal(authFiles().length,0);
    let start=await begin();
    const publicPage=await fetch(new URL(start.url,base+"/management.html"));
    assert.equal(publicPage.status,200);
    assert.match(await publicPage.text(),/CommandCode 浏览器授权/);
    assert.equal((await api(plugin+"/oauth/complete","POST",{state:start.state,api_key:"native-first"},false)).status,401);
    assert.equal((await api(plugin+"/accounts","POST",{api_key:"native-go"})).status,422,"Go admitted");
    assert.equal((await api(plugin+"/accounts","POST",{api_key:"native-invalid"})).status,401,"invalid key admitted");
    await complete(start,"native-first");
    assert.equal(authFiles().length,0,"completion saved instead of staging");
    assert.equal((await api(plugin+"/accounts","POST",{api_key:"native-first"})).status,409,"cross-entry duplicate accepted");
    const poll=await Promise.all(Array.from({length:12},()=>api(mgmt+"/get-auth-status?state="+start.state)));
    assert(poll.every(r=>r.data.status==="ok"||r.data.status==="wait"),"concurrent status poisoned session");
    await finish(start);
    assert.equal(authFiles().length,1);
    let saved=JSON.parse(fs.readFileSync(path.join(authDir,authFiles()[0]),"utf8"));
    assert.equal(saved.api_key,"native-first");assert.equal(saved.credential_source,"browser_authorization");
    assert.equal(saved.email,"native@example.test");assert.equal(saved.plan_id,"individual-goat");
    const fileName=authFiles()[0];
    assert.equal(fileName,"commandcode-key-"+createHash("sha256").update("native-first").digest("hex")+".json");
    const present=await api(plugin+"/oauth/session","POST",{state:start.state});
    assert.equal(present.data.status,"present",JSON.stringify(present));
    await until(async()=>{const r=await fetch(base+"/v1/models",{headers:{Authorization:"Bearer test-client-key"}});const data=await r.json();return data.data?.some(m=>m.id==="commandcode/native-model");},"new auth did not expose models without restart");
    const completion=await fetch(base+"/v1/chat/completions",{method:"POST",headers:{Authorization:"Bearer test-client-key","Content-Type":"application/json"},body:JSON.stringify({model:"commandcode/native-model",messages:[{role:"user",content:"test fixture"}]})});
    assert.equal(completion.status,200);assert.equal(lastKey,"native-first");
    assert.equal((await api(mgmt+"/auth-files/status","PATCH",{name:fileName,disabled:true})).status,200);
    const before=fs.readFileSync(path.join(authDir,fileName));
    start=await begin();
    assert.equal((await api(plugin+"/oauth/complete","POST",{state:start.state,api_key:"native-first"})).status,409);
    assert.deepEqual(fs.readFileSync(path.join(authDir,fileName)),before,"duplicate changed disabled file");
    start=await begin();await complete(start,"native-cancel");
    assert.equal((await api(plugin+"/oauth/cancel","POST",{state:start.state})).status,200);
    assert.equal((await api(mgmt+"/get-auth-status?state="+start.state)).data.status,"error");
    assert.equal(authFiles().length,1,"cancelled session wrote file");
    for(const key of ["native-pro","native-max"]){
      const r=await api(plugin+"/accounts","POST",{api_key:key});
      assert.equal(r.status,201,JSON.stringify(r));
    }
    if(process.env.CPA_BROWSER==="1"){
      const {chromium}=require("playwright");
      browser=await chromium.launch({headless:true,executablePath:process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH||undefined});
      const page=await browser.newPage();
      await page.addInitScript(()=>localStorage.setItem("cli-proxy-auth",JSON.stringify({state:{managementKey:"test-management-key"}})));
      start=await begin();
      await page.goto(new URL(start.url,base+"/management.html").href);
      await page.waitForFunction(()=>!document.querySelector("#complete").disabled);
      await page.locator("#authorization-link").waitFor({state:"visible"});
      const official=new URL(await page.locator("#authorization-link").getAttribute("href"));
      assert.equal(official.origin,"https://commandcode.ai");
      assert.equal(official.pathname,"/studio/auth/cli");
      assert.equal(official.searchParams.get("callback"),"http://localhost:6000/callback");
      assert.equal(official.searchParams.get("state"),start.state,"native host escaped or lost official login state");
      assert.equal(official.searchParams.has("mode"),false);
      await page.locator("#api-key").fill("native-browser");
      await page.locator("#complete").click();
      await page.waitForFunction(()=>document.querySelector("#status").textContent.includes("已确认完成"));
      assert.equal(await page.locator("#api-key").inputValue(),"");
      assert.equal(await page.evaluate(()=>JSON.stringify(localStorage).includes("native-browser")||JSON.stringify(sessionStorage).includes("native-browser")),false);
      await browser.close();browser=null;
    }
    const count=authFiles().length;
    await stopHost();startHost();
    await until(async()=>{const r=await api(plugin+"/quota-usage","POST",{});return r.data.cards?.length===count;},"restart lost/resurrected auth");
    assert.equal(authFiles().length,count);
    console.log("PASS: real native CPA OAuth + plugin: protected completion, stage-only, concurrent host polls, verified file/source/identity, cross-entry duplicate, disabled preservation, cancellation, GOAT/Pro/Max admission, Go/invalid rejection, immediate model call, browser guide="+(process.env.CPA_BROWSER==="1")+", restart. Artifacts: "+root);
  }catch(error){
    console.error("Isolated test artifacts: "+root+"; host log bytes="+log.length);
    throw error;
  }finally{
    if(browser)await browser.close();
    await stopHost();await new Promise(resolve=>upstream.close(resolve));
  }
})().catch(error=>{console.error(error.stack);process.exitCode=1});
