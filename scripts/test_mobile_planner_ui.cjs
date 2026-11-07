/* Synthetic browser verification of the actual console DOM and package renderer. */
'use strict';
const {chromium}=require(process.env.PLAYWRIGHT_MODULE||'playwright');
const fs=require('node:fs/promises'),fsSync=require('node:fs'),os=require('node:os'),path=require('node:path'),assert=require('node:assert/strict');
const flow=require('../internal/admin/static/flow-model.js');

// The legacy live-device configuration must be unavailable until status is
// known, then remain absent only on the Windows retirement path. This is kept
// outside the browser fixture so the transition stays covered even when the
// package renderer evolves independently.
assert.equal(flow.isDeviceControlRetired(),true);
assert.equal(flow.catalog.tools.pages.includes('mobile'),false);
flow.setDeviceControlRetired(false);
assert.equal(flow.catalog.tools.pages.includes('mobile'),true);
flow.setDeviceControlRetired(true);
assert.equal(flow.catalog.tools.pages.includes('mobile'),false);

(async()=>{
 const edge='C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe';
 const executablePath=process.env.PLAYWRIGHT_EXECUTABLE_PATH||(fsSync.existsSync(edge)?edge:'');
 const browser=await chromium.launch({headless:true,...(executablePath?{executablePath}:{})});
 const output=await fs.mkdtemp(path.join(os.tmpdir(),'wechat-agent-plugin-mobile-ui-'));
 let page;
 try{
  page=await browser.newPage({reducedMotion:'reduce'});page.setDefaultTimeout(6000);const errors=[],consoleErrors=[];
  page.on('pageerror',error=>errors.push(error.message));page.on('console',m=>{if(m.type()==='error')consoleErrors.push(m.text())});
  const root=path.resolve('internal/admin/static'),mime={'.html':'text/html; charset=utf-8','.js':'text/javascript; charset=utf-8','.css':'text/css; charset=utf-8','.png':'image/png','.svg':'image/svg+xml'};
  await page.route('**/*',async route=>{
   const url=new URL(route.request().url());assert.equal(url.origin,'http://plugins.synthetic');
   if(/^\/api\/mobile\/tasks\/[^/]+\/screenshot\/[^/]+$/.test(url.pathname))return route.fulfill({body:'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 360 640"><defs><linearGradient id="a" x1="0" y1="0" x2="1" y2="1"><stop stop-color="#172554"/><stop offset="1" stop-color="#6d28d9"/></linearGradient></defs><rect width="360" height="640" fill="url(#a)"/><text x="28" y="54" fill="white" font-size="18" font-family="sans-serif">Reader</text><rect x="24" y="92" width="312" height="118" rx="16" fill="#ffffff" fill-opacity=".16"/><rect x="24" y="232" width="312" height="188" rx="16" fill="#ffffff" fill-opacity=".10"/><text x="48" y="130" fill="white" font-size="24" font-family="sans-serif">合成设备画面</text><text x="48" y="165" fill="#dbeafe" font-size="15" font-family="sans-serif">只用于离线视觉验收</text></svg>',contentType:'image/svg+xml'});
   const name=url.pathname==='/'?'index.html':path.basename(url.pathname),file=path.join(root,name);
   if(!fsSync.existsSync(file))return route.fulfill({status:404,body:'not found',contentType:'text/plain'});
   let body=await fs.readFile(file);if(name==='index.html')body=Buffer.from(body.toString().replace(/<script\b[^>]*>[\s\S]*?<\/script>/gi,''));
   await route.fulfill({body,contentType:mime[path.extname(file)]||'application/octet-stream'});
  });
  await page.goto('http://plugins.synthetic');
  await page.evaluate(()=>{
   document.querySelector('#login-view').hidden=true;document.querySelector('#app-view').hidden=false;
   document.querySelectorAll('.page').forEach(node=>node.classList.toggle('active-page',node.id==='plugins'));
   document.querySelectorAll('[data-nav-group]').forEach(node=>node.hidden=node.dataset.navGroup!=='wechat');
   document.querySelector('#page-title').textContent='扩展与连接';document.querySelector('#account-select').replaceChildren(new Option('合成验收账号','one'));document.querySelector('#status-label').textContent='微信已登录';
    window.WechatAccountContext={id:'one',url:path=>path};window.__calls=[];window.__defer='';window.__pending=null;window.__createFailure='';window.__observeFailure='';window.__unbindWarning='';window.__observationAppFailure='';
   window.__schema=[{id:'configuration',title:'执行设置',component:'configuration',binding:'host_data.settings',action:'settings.save'},{id:'tasks',title:'设备任务',component:'tasks',binding:'host_data.tasks',action:'tasks.create'},{id:'resources',title:'任务素材',component:'resources',binding:'host_data.resources'},{id:'diagnostics',title:'状态与说明',component:'diagnostics',binding:'host_data.diagnostics',action:'devices.list'}];
    const fixture=()=>({enabled:false,data:{state:'active',settings:{revision:1,enabled:true,devices:['lab'],actions:['tap'],max_steps:8,max_seconds:120,connection_profile_status:'已登记'},tasks:[{id:'queued',goal:'查看合成页面',state:'queued',device_id:'lab',artifacts:[]}],resources:{},diagnostics:{planner_available:false,planner_reason:'手机规划工具未安装，暂不能新建任务。'},overview:{fish_id:'fish-one',checked_at:'2026-11-05T06:00:00Z',devices:[{device_id:'lab',connection_state:'disconnected',checked_at:'2026-11-05T06:00:00Z',current_app:'com.example.reader',observed_at:'2026-11-05T05:58:00Z',task_id:'queued',observation_id:'frame-1',screenshot_uri:'/api/mobile/tasks/queued/screenshot/frame-1',authorization_state:'ready',authorized_apps:['com.example.reader'],authorized_actions:['observe','tap'],observation_apps_state:'ready',observation_apps:[]}]}}});
   window.__accounts={one:fixture(),two:fixture()};window.__accounts.two.data.settings.devices=['account-two'];
   window.WechatAdmin={isAuthenticated:()=>true,api:async(url,opt={})=>{
    const owner=window.WechatAccountContext.id,fixture=window.__accounts[owner],body=structuredClone(opt.json);window.__calls.push({url,method:opt.method||'GET',body,account:owner});
    if(window.__defer===url){window.__defer='';await new Promise((resolve,reject)=>{window.__pending={resolve,reject}})}
    if(url==='/api/plugins')return {installed:[],templates:[]};
    if(url==='/api/plugin-packages')return {catalog:[],installed:fixture.data.state==='archive'?[]:[{package:{id:'mobile-control',name:'设备控制'},enabled:fixture.enabled}],runtimes:{},retained_ids:fixture.data.state==='archive'?['mobile-control']:[]};
    if(url==='/api/plugin-packages/mobile-control')return {installed:fixture.data.state==='archive'?undefined:{package:{id:'mobile-control',name:'设备控制'},enabled:fixture.enabled,revision:1},runtime:{state:fixture.enabled?'running':'stopped'},runtime_connected:true,ui_schema:{version:1,sections:structuredClone(window.__schema)},host_data:structuredClone(fixture.data)};
    if(url.endsWith('/runtime')){fixture.enabled=body.action==='start';return {}}
    if(url.endsWith('/actions/settings.save')){fixture.data.settings={...body,revision:body.revision+1,connection_profile_status:'已登记'};return structuredClone(fixture.data.settings)}
    if(url.endsWith('/actions/tasks.create')){if(window.__createFailure){const kind=window.__createFailure;window.__createFailure='';if(kind==='unknown')return {};throw new Error('合成网络中断，提交状态未知')}const task={id:'created-'+window.__calls.length,goal:body.goal,state:'queued',device_id:body.device_id,artifacts:[]};fixture.data.tasks.push(task);return task}
    if(url.endsWith('/actions/tasks.cancel')){fixture.data.tasks.find(t=>t.id===body.id).state='cancelled';return {cancelled:true}}
    if(url.endsWith('/actions/tasks.stop')){fixture.data.tasks.forEach(t=>{if(['queued','running'].includes(t.state))t.state='stopped'});return {stopped:true}}
    if(url.endsWith('/actions/overview.observe')){const device=fixture.data.overview.devices.find(d=>d.device_id===body.device_id);if(window.__observeFailure){device.connection_state='disconnected';const failure=window.__observeFailure;window.__observeFailure='';throw new Error(failure)}device.connection_state='connected';device.checked_at='2026-11-05T06:01:00Z';return {observed:true}}
    if(url.endsWith('/actions/overview.pause')){const device=fixture.data.overview.devices.find(d=>d.device_id===body.device_id);device.connection_state='paused';device.paused=true;return {paused:true}}
    if(url.endsWith('/actions/overview.takeover')){const device=fixture.data.overview.devices.find(d=>d.device_id===body.device_id);device.connection_state='paused';device.paused=true;device.takeover=true;return {taken_over:true}}
    if(url.endsWith('/actions/overview.resume')){const device=fixture.data.overview.devices.find(d=>d.device_id===body.device_id);device.connection_state='unknown';device.paused=false;device.takeover=false;return {resumed:true}}
    if(url.endsWith('/actions/overview.bind')){const device=fixture.data.overview.devices.find(d=>d.device_id===body.device_id);device.fish_id=fixture.data.overview.fish_id;device.binding_id='binding-1';return {bound:true}}
    if(url.endsWith('/actions/overview.unbind')){const device=fixture.data.overview.devices.find(d=>d.device_id===body.device_id);if(body.binding_id!==device.binding_id)throw new Error('binding mismatch');device.fish_id='';device.binding_id='';device.paused=true;device.connection_state='paused';const warning=window.__unbindWarning;window.__unbindWarning='';return warning?{unbound:true,overview:structuredClone(fixture.data.overview),warning}:{unbound:true,overview:structuredClone(fixture.data.overview)}}
    if(url.endsWith('/actions/overview.observation-apps')){const device=fixture.data.overview.devices.find(d=>d.device_id===body.device_id);if(window.__observationAppFailure){const failure=window.__observationAppFailure;window.__observationAppFailure='';throw new Error(failure)}if(body.binding_id!==device.binding_id)throw new Error('binding mismatch');device.observation_apps=Array.isArray(device.observation_apps)?device.observation_apps:[];if(body.enabled)device.observation_apps.push({grant_id:'grant-'+window.__calls.length,package:body.package,binding_id:device.binding_id,fish_id:fixture.data.overview.fish_id});else device.observation_apps=device.observation_apps.filter(grant=>!(grant.package===body.package&&grant.binding_id===body.binding_id));return {overview:structuredClone(fixture.data.overview)}}
    if(url.endsWith('/actions/devices.list'))return [{id:'lab',state:'ready'}];
    throw new Error('unexpected synthetic API '+url);
   }};
  });
  for(const script of ['refresh.js','dialogs.js','console.js','plugins.js','plugin-packages.js','save-actions.js'])await page.addScriptTag({path:path.join(root,script)});
  const open=async(section)=>{await page.evaluate(section=>window.WechatPluginPackages.open('mobile-control',section),section);await page.getByRole('heading',{name:'设备控制',exact:true}).waitFor()};
  const settle=()=>page.evaluate(()=>new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve))));
  const save=()=>page.locator('#top-save-actions').getByRole('button',{name:'保存设备设置',exact:true});
  const calls=async suffix=>page.evaluate(s=>window.__calls.filter(call=>call.url.endsWith(s)),suffix);
  const waitPending=()=>page.waitForFunction(()=>window.__pending!==null);
  const resolvePending=()=>page.evaluate(()=>{window.__pending.resolve();window.__pending=null});
  await open();await settle();
  assert.equal(await page.locator('#plugins-refresh').evaluate(n=>n.hasAttribute('data-refresh-source')),true);
  assert.equal(await page.locator('#plugin-guide').isVisible(),false);
  assert.equal((await calls('/actions/overview.observe')).length,0,'opening or refreshing the page must not probe a device');
  assert(await page.getByText('上次观察应用：com.example.reader',{exact:true}).isVisible());
  assert(await page.getByText(/^观察于 /).isVisible());
  await page.evaluate(()=>{const device=window.__accounts.one.data.overview.devices[0];device.checked_at='0001-01-01T00:00:00Z';device.observed_at='0001-01-01T00:00:00Z'});await open();
  assert(await page.getByText('检查于 尚未检查',{exact:true}).isVisible());assert(await page.getByText('观察于 时间待核对',{exact:true}).isVisible());
  await page.evaluate(()=>{const device=window.__accounts.one.data.overview.devices[0];device.checked_at='2026-11-05T06:00:00Z';device.observed_at='2026-11-05T05:58:00Z'});await open();
   assert(await page.getByText('已登记应用：com.example.reader · 已登记动作：observe、tap',{exact:true}).isVisible());
   assert(await page.getByText('仅设置授权，暂不自动查看手机；后续探索仅可读取当前应用画面，不包含点击、评论或发布。',{exact:true}).isVisible());
   assert(await page.getByText('绑定大肥鱼设备后才能设置自主观察。',{exact:true}).isVisible());
  await page.getByRole('button',{name:'设为大肥鱼的设备',exact:true}).click();await page.waitForFunction(()=>window.__accounts.one.data.overview.devices[0].binding_id==='binding-1');
   const readerPermission=page.getByRole('checkbox',{name:/^com\.example\.reader · 允许自主观察/});
   assert(await readerPermission.isEnabled());await readerPermission.check();await page.waitForFunction(()=>window.__accounts.one.data.overview.devices[0].observation_apps.length===1);
   assert.equal((await calls('/actions/overview.observation-apps')).at(-1).body.enabled,true);
   await page.evaluate(()=>{const device=window.__accounts.one.data.overview.devices[0];device.authorization_state='unavailable';device.authorized_apps=[];device.authorized_actions=[]});await open();
   assert(await page.getByText('尚未读取授权范围，暂不能新增授权。',{exact:true}).isVisible());assert(await readerPermission.isChecked());assert(await readerPermission.isEnabled(),'existing grants stay revocable if the profile later becomes unavailable');await page.evaluate(()=>{window.__accounts.one.data.overview.devices[0].observation_apps_state='unavailable'});await open();assert(await page.getByText('授权记录暂无法读取，暂不能更改。',{exact:true}).isVisible());assert(await readerPermission.isDisabled());await page.evaluate(()=>{window.__accounts.one.data.overview.devices[0].observation_apps_state='ready'});await open();await readerPermission.click();await page.waitForFunction(()=>window.__accounts.one.data.overview.devices[0].observation_apps.length===0);
   await page.evaluate(()=>{const device=window.__accounts.one.data.overview.devices[0];device.authorization_state='ready';device.authorized_apps=['com.example.reader'];device.authorized_actions=['observe','tap']});await open();
   assert(await page.getByText('已绑定大肥鱼',{exact:true}).isVisible());await page.evaluate(()=>{window.__unbindWarning='设备绑定已解除；停止任务记录待更新'});await page.getByRole('button',{name:'解除绑定',exact:true}).click();
  await page.getByRole('dialog').getByRole('button',{name:'确认解除',exact:true}).click();await page.waitForFunction(()=>window.__accounts.one.data.overview.devices[0].binding_id==='');await page.getByText('设备绑定已解除；停止任务记录待更新',{exact:true}).waitFor();
  assert.equal(await page.getByRole('button',{name:'观察一次',exact:true}).count(),0,'unbinding pauses the device before later work can observe it');
  await page.evaluate(()=>{const device=window.__accounts.one.data.overview.devices[0];device.paused=false;device.connection_state='disconnected'});await open();
  await page.evaluate(()=>{window.__accounts.one.data.overview.fish_id=''});await open();assert.equal(await page.getByRole('button',{name:'设为大肥鱼的设备',exact:true}).count(),0);assert(await page.getByText('请先在微信账号绑定身份，再为设备归属大肥鱼。',{exact:true}).isVisible());
  await page.evaluate(()=>{window.__accounts.one.data.overview.fish_id='fish-one';const device=window.__accounts.one.data.overview.devices[0];device.fish_id='fish-other';device.binding_id='binding-other'});await open();assert(await page.getByText('已绑定其他身份。',{exact:true}).isVisible());assert.equal(await page.getByRole('button',{name:'解除绑定',exact:true}).count(),0);
  await page.evaluate(()=>{const device=window.__accounts.one.data.overview.devices[0];device.fish_id='';device.binding_id='';device.authorization_state='unavailable';device.authorized_apps=[];device.authorized_actions=[]});await open();assert(await page.getByText('尚未读取授权范围。',{exact:true}).isVisible());
   await page.evaluate(()=>{const device=window.__accounts.one.data.overview.devices[0];device.authorization_state='ready';device.authorized_apps=['com.example.reader'];device.authorized_actions=['observe','tap']});await open();assert(await page.getByText('先绑定设备后才能观察或启动任务。',{exact:true}).isVisible());assert(await page.getByRole('button',{name:'观察一次',exact:true}).isDisabled());assert(await page.getByText('绑定大肥鱼设备后才能设置自主观察。',{exact:true}).isVisible());
  // A late binding response from a previous account must not repaint the account now on screen.
  await page.evaluate(()=>{window.__defer='/api/plugin-packages/mobile-control/actions/overview.bind'});await page.getByRole('button',{name:'设为大肥鱼的设备',exact:true}).click();await waitPending();
  await page.evaluate(()=>{window.dispatchEvent(new CustomEvent('wechat-admin:logout'));window.WechatAccountContext.id='two'});await open();await resolvePending();await settle();
  assert.equal(await page.evaluate(()=>window.__accounts.two.data.overview.devices[0].binding_id||''),'','late binding response crossed an account boundary');
   await page.evaluate(()=>{window.dispatchEvent(new CustomEvent('wechat-admin:logout'));window.WechatAccountContext.id='one';const device=window.__accounts.one.data.overview.devices[0];device.fish_id='fish-one';device.binding_id='binding-ready';device.authorization_state='ready';device.authorized_apps=['com.example.reader'];device.authorized_actions=['observe'];device.observation_apps=[]});await open();
   // A late permission update must not paint the next account; the server result belongs to the original account only.
   await page.evaluate(()=>{window.__defer='/api/plugin-packages/mobile-control/actions/overview.observation-apps'});await page.getByRole('checkbox',{name:/^com\.example\.reader · 允许自主观察/}).check();await waitPending();
   await page.evaluate(()=>{window.dispatchEvent(new CustomEvent('wechat-admin:logout'));window.WechatAccountContext.id='two'});await open();await resolvePending();await settle();
   assert.equal(await page.evaluate(()=>window.__accounts.two.data.overview.devices[0].observation_apps.length),0,'late observation authorization crossed an account boundary');
   await page.evaluate(()=>{window.dispatchEvent(new CustomEvent('wechat-admin:logout'));window.WechatAccountContext.id='one'});await open();assert(await page.getByRole('checkbox',{name:/^com\.example\.reader · 允许自主观察/}).isChecked());await page.getByRole('checkbox',{name:/^com\.example\.reader · 允许自主观察/}).click();await page.waitForFunction(()=>window.__accounts.one.data.overview.devices[0].observation_apps.length===0);
  await page.evaluate(()=>{window.__observeFailure='桥接暂时不可用'});await page.getByRole('button',{name:'观察一次',exact:true}).click();await page.getByText('桥接暂时不可用',{exact:true}).waitFor();
  assert.equal(await page.evaluate(()=>window.__accounts.one.data.overview.devices[0].connection_state),'disconnected','failed observation must refresh the visible connection state');
  await page.getByRole('button',{name:'观察一次',exact:true}).click();await page.waitForFunction(()=>window.__accounts.one.data.overview.devices[0].connection_state==='connected');
  assert.equal((await calls('/actions/overview.observe')).length,2,'only an explicit observation action may probe a device');
  await page.getByRole('button',{name:'暂停观察',exact:true}).click();await page.waitForFunction(()=>window.__accounts.one.data.overview.devices[0].connection_state==='paused');
  assert(await page.getByRole('button',{name:'恢复观察',exact:true}).isVisible());
  assert.equal(await page.getByRole('button',{name:'观察一次',exact:true}).count(),0,'paused device must use an explicit recovery action');
  await page.getByRole('button',{name:'恢复观察',exact:true}).click();await page.waitForFunction(()=>window.__accounts.one.data.overview.devices[0].connection_state==='unknown');
  await page.getByRole('button',{name:'接管设备',exact:true}).click();await page.waitForFunction(()=>window.__accounts.one.data.overview.devices[0].takeover===true);
  assert(await page.getByText('已接管',{exact:true}).isVisible());assert.equal(await page.getByRole('button',{name:'观察一次',exact:true}).count(),0,'takeover must suppress further observation');
  await page.getByRole('button',{name:'恢复观察',exact:true}).click();await page.waitForFunction(()=>window.__accounts.one.data.overview.devices[0].takeover===false);
  assert(await page.getByRole('button',{name:'开始任务',exact:true}).isDisabled(),'runtime disabled must block creation even when settings.enabled is true');await save().click();await page.getByText('设备设置已保存；工具运行状态未改变。',{exact:true}).waitFor();assert.equal(await page.evaluate(()=>window.__accounts.one.enabled),false);assert.equal((await calls('/runtime')).length,0,'saving execution settings started the package');
  await page.getByRole('button',{name:'启动工具',exact:true}).click();await page.getByRole('button',{name:'停止工具',exact:true}).waitFor();
  assert(await page.getByRole('button',{name:'开始任务',exact:true}).isDisabled(),'nested unavailable planner must block creation');
  await page.getByRole('button',{name:'检查已授权设备',exact:true}).click();await page.getByText('[{"id":"lab","state":"ready"}]',{exact:true}).waitFor();
  await page.screenshot({path:path.join(output,'blocked-light-1440.png'),fullPage:true});await page.setViewportSize({width:375,height:900});await page.evaluate(()=>document.documentElement.dataset.theme='dark');await page.locator('#plugin-section-diagnostics').scrollIntoViewIfNeeded();await page.screenshot({path:path.join(output,'blocked-dark-375.png'),fullPage:true});await page.setViewportSize({width:1440,height:900});await page.evaluate(()=>document.documentElement.dataset.theme='light');
  await page.getByRole('button',{name:'停止全部任务',exact:true}).click();await page.waitForFunction(()=>window.__accounts.one.data.tasks[0].state==='stopped');
  await page.evaluate(()=>{window.__accounts.one.data.tasks[0].state='queued'});await open();await page.getByRole('button',{name:'取消任务',exact:true}).click();await page.waitForFunction(()=>window.__accounts.one.data.tasks[0].state==='cancelled');
  await page.locator('#package-mobile-devices').fill('lab-two');await save().click();await page.waitForFunction(()=>window.__accounts.one.data.settings.devices[0]==='lab-two');await settle();
  assert.equal((await calls('/actions/settings.save')).at(-1).body.max_steps,8);assert.equal((await calls('/actions/settings.save')).at(-1).body.bridge,undefined);
  assert(await page.getByRole('button',{name:'停止工具',exact:true}).isVisible(),'saving host execution settings must retain package runtime');
  await page.evaluate(()=>{window.__accounts.one.data.diagnostics={planner_available:true,planner_reason:''}});await open();assert(await page.getByRole('button',{name:'开始任务',exact:true}).isEnabled());
  // Schema declarations own ordering, headings and presence. Mismatched bindings/actions are ignored.
  await page.evaluate(()=>{window.__originalSchema=structuredClone(window.__schema);window.__schema=[{...window.__schema[2],title:'自定义素材标题'},window.__schema[0],{...window.__schema[3],binding:'host_data.settings'}, {...window.__schema[1],action:'settings.save'}]});await open();
  assert.deepEqual(await page.locator('.package-editor>[data-host-component]').evaluateAll(nodes=>nodes.map(n=>n.dataset.hostComponent)),['resources','configuration']);assert(await page.getByRole('heading',{name:'自定义素材标题'}).isVisible());assert.equal(await page.locator('.package-host-task-form').count(),0);
  await page.evaluate(()=>{window.__schema=window.__originalSchema.map(({binding,action,...section})=>section)});await open();assert.equal(await page.locator('.package-editor>[data-host-component]').count(),4,'legacy component declarations must map to safe defaults');assert(await save().isVisible());assert(await page.getByRole('button',{name:'开始任务',exact:true}).isEnabled());await page.evaluate(()=>{window.__schema=structuredClone(window.__originalSchema)});await open();
  // A later edit must remain dirty when a successful earlier save completes.
  await page.locator('#package-mobile-devices').fill('submitted');await page.evaluate(()=>{window.__defer='/api/plugin-packages/mobile-control/actions/settings.save'});await save().click();await waitPending();await page.locator('#package-mobile-devices').fill('newer-draft');await resolvePending();
  await page.getByText('已保存提交的设备设置，后续编辑尚未保存。',{exact:true}).waitFor();assert.equal(await page.locator('#package-mobile-devices').inputValue(),'newer-draft');assert.equal(await page.evaluate(()=>window.WechatAdmin.pluginPackageDirty),true);
  await save().click();await page.waitForFunction(()=>window.__accounts.one.data.settings.devices[0]==='newer-draft');await settle();
  // A detail response that arrives after another edit must preserve the draft.
  await page.evaluate(()=>{window.__defer='/api/plugin-packages/mobile-control';window.__openRequest=window.WechatPluginPackages.open('mobile-control')});await waitPending();await page.locator('#package-mobile-devices').fill('detail-response-draft');await resolvePending();await page.evaluate(()=>window.__openRequest);assert.equal(await page.locator('#package-mobile-devices').inputValue(),'detail-response-draft');await save().click();await page.waitForFunction(()=>window.__accounts.one.data.settings.devices[0]==='detail-response-draft');await settle();
  // Unknown results and network failures keep one idempotency key until payload changes or verified success.
  await page.locator('#package-mobile-goal').fill('观察合成页面');await page.evaluate(()=>{window.__createFailure='network'});await page.getByRole('button',{name:'开始任务',exact:true}).click();await page.getByText(/合成网络中断/).waitFor();const first=(await calls('/actions/tasks.create')).at(-1).body.request_key;await open();assert.equal(await page.locator('#package-mobile-goal').inputValue(),'观察合成页面','refresh discarded an unknown submission draft');assert.equal((await calls('/actions/tasks.create')).length,1,'failed submission auto-retried');
  await page.evaluate(()=>{window.__createFailure='unknown'});await page.getByRole('button',{name:'开始任务',exact:true}).click();await page.getByText(/任务提交结果待核对/).waitFor();assert.equal((await calls('/actions/tasks.create')).at(-1).body.request_key,first);
  await page.getByRole('button',{name:'开始任务',exact:true}).click();await page.waitForFunction(()=>window.__accounts.one.data.tasks.some(t=>t.goal==='观察合成页面'));await settle();assert.equal((await calls('/actions/tasks.create')).at(-1).body.request_key,first);
  await page.locator('#package-mobile-goal').fill('另一合成目标');await page.evaluate(()=>{window.__createFailure='network'});await page.getByRole('button',{name:'开始任务',exact:true}).click();await page.getByText(/合成网络中断/).waitFor();const changed=(await calls('/actions/tasks.create')).at(-1).body.request_key;assert.notEqual(changed,first);
  await page.locator('#package-mobile-goal').fill('修改后的目标');await page.getByRole('button',{name:'开始任务',exact:true}).click();await page.waitForFunction(()=>window.__accounts.one.data.tasks.some(t=>t.goal==='修改后的目标'));await settle();assert.notEqual((await calls('/actions/tasks.create')).at(-1).body.request_key,changed);
  // Selecting another plugin while a task action is in flight invalidates its response.
  await page.evaluate(()=>{window.__defer='/api/plugin-packages/mobile-control/actions/tasks.stop'});await page.getByRole('button',{name:'停止全部任务',exact:true}).click();await waitPending();await page.evaluate(()=>window.WechatPluginPackages.open('synthetic-placeholder'));const placeholder=await page.locator('.package-editor').innerText();await resolvePending();await settle();assert.equal(await page.locator('.package-editor').innerText(),placeholder);await open();
  // Switching account while an old save resolves must not mutate the newly edited account form.
  await page.locator('#package-mobile-devices').fill('old-account-submit');await page.evaluate(()=>{window.__defer='/api/plugin-packages/mobile-control/actions/settings.save'});await save().click();await waitPending();
  await page.evaluate(()=>{window.dispatchEvent(new CustomEvent('wechat-admin:logout'));window.WechatAccountContext.id='two'});await open();await page.locator('#package-mobile-devices').fill('account-two-draft');await resolvePending();await settle();
  assert.equal(await page.locator('#package-mobile-devices').inputValue(),'account-two-draft');assert.equal(await page.evaluate(()=>window.WechatAdmin.pluginPackageDirty),true);await save().click();await page.waitForFunction(()=>window.__accounts.two.data.settings.devices[0]==='account-two-draft');await settle();
  // Both successful and rejected stale host actions must leave a new account's view intact.
  await page.evaluate(()=>{window.__accounts.two.enabled=true;window.__accounts.two.data.diagnostics.planner_available=true;window.__accounts.two.data.tasks=[{id:'two-task',goal:'二号任务',state:'queued',device_id:'account-two-draft'}]});await open();
  await page.evaluate(()=>{window.__defer='/api/plugin-packages/mobile-control/actions/tasks.stop'});await page.getByRole('button',{name:'停止全部任务',exact:true}).click();await waitPending();await page.evaluate(()=>{window.dispatchEvent(new CustomEvent('wechat-admin:logout'));window.WechatAccountContext.id='one'});await open();await page.locator('#package-mobile-devices').fill('retained-draft');await resolvePending();await settle();assert.equal(await page.locator('#package-mobile-devices').inputValue(),'retained-draft');
  await save().click();await page.waitForFunction(()=>window.__accounts.one.data.settings.devices[0]==='retained-draft');await settle();
  await page.evaluate(()=>{window.__defer='/api/plugin-packages/mobile-control/actions/devices.list'});await page.getByRole('button',{name:'检查已授权设备',exact:true}).click();await waitPending();await page.evaluate(()=>{window.dispatchEvent(new CustomEvent('wechat-admin:logout'));window.WechatAccountContext.id='two'});await open();await page.locator('#package-mobile-devices').fill('last-draft');await page.evaluate(()=>{window.__pending.reject(new Error('stale device error'));window.__pending=null});await settle();assert.equal(await page.getByText('stale device error',{exact:true}).count(),0);assert.equal(await page.locator('#package-mobile-devices').inputValue(),'last-draft');
  await save().click();await page.waitForFunction(()=>window.__accounts.two.data.settings.devices[0]==='last-draft');await settle();
  // Landing on the plugin resets its scroll containers, while explicit section opening can scroll to tasks.
  await page.evaluate(()=>{document.querySelector('.package-editor').scrollTop=700;document.querySelector('.package-workspace').scrollTop=700});await open();await settle();assert.equal(await page.locator('.package-editor').evaluate(n=>n.scrollTop),0);
  for(const theme of ['light','dark'])for(const width of [375,1440]){
   await page.setViewportSize({width,height:900});await page.evaluate(value=>{document.documentElement.dataset.theme=value;scrollTo(0,0);document.querySelector('.content').scrollTop=0;document.querySelector('.package-editor').scrollTop=0;document.querySelector('.package-workspace').scrollTop=0},theme);await settle();
   const layout=await page.evaluate(()=>({scrollWidth:document.documentElement.scrollWidth,top:document.querySelector('.topbar').getBoundingClientRect().top,save:document.querySelector('#top-save-actions').getBoundingClientRect().toJSON(),inline:document.querySelector('.package-host-form>.primary')!==null,overflow:[...document.querySelectorAll('.package-workspace,.package-editor,[data-host-component],.mobile-observation-workbench,.mobile-observation-card,.mobile-observation-content')].map(n=>({name:n.className,width:n.clientWidth,scroll:n.scrollWidth})),buttons:[...document.querySelectorAll('#plugins button')].filter(n=>n.getClientRects().length).map(n=>({text:n.textContent,...n.getBoundingClientRect().toJSON()}))}));
   assert(layout.scrollWidth<=width+1,`horizontal document overflow ${theme}/${width}`);assert(layout.top>=0,'clipped page header');assert(layout.save.top>=0&&layout.save.right<=width+1,'top save offscreen');assert.equal(layout.inline,false);assert(layout.overflow.every(n=>n.scroll<=n.width+1),`nested horizontal overflow ${JSON.stringify(layout.overflow)}`);assert(layout.buttons.every(b=>b.left>=-1&&b.right<=width+1),`button overflow ${theme}/${width}: ${JSON.stringify(layout.buttons)}`);
   await page.screenshot({path:path.join(output,`active-${theme}-${width}.png`),fullPage:false});
  }
   // Inspect a persisted but no-longer-profile-available grant: it remains visible
   // for revocation, but never presents as active autonomous observation.
   await page.evaluate(()=>{window.dispatchEvent(new CustomEvent('wechat-admin:logout'));window.WechatAccountContext.id='one';const device=window.__accounts.one.data.overview.devices[0];device.fish_id='fish-one';device.binding_id='binding-visual';device.authorization_state='ready';device.authorized_apps=[];device.authorized_actions=[];device.observation_apps_state='ready';device.observation_apps=[{grant_id:'grant-visual',fish_id:'fish-one',device_id:'lab',binding_id:'binding-visual',package:'com.example.reader',available:false}]});await open();
   assert(await page.getByText('com.example.reader · 允许自主观察',{exact:true}).isVisible());assert(await page.getByText('暂不可用',{exact:true}).isVisible());assert(await page.getByRole('checkbox',{name:/^com\.example\.reader · 允许自主观察/}).isChecked());
   // Inspect the actual observation card itself at desktop and narrow widths;
   // neither rendering nor the top-level refresh may initiate a bridge probe.
  for(const [theme,width] of [['light',1440],['dark',375]]){
   await page.setViewportSize({width,height:1400});await page.evaluate(value=>{document.documentElement.dataset.theme=value;for(const selector of ['.package-editor','.package-workspace']){const node=document.querySelector(selector);node.style.maxHeight='none';node.style.overflow='visible'}},theme);await page.locator('.mobile-observation-workbench').scrollIntoViewIfNeeded();await settle();
   await page.screenshot({path:path.join(output,`observation-${theme}-${width}.png`),fullPage:false});
  }
  await page.evaluate(()=>{window.dispatchEvent(new CustomEvent('wechat-admin:logout'));window.WechatAccountContext.id='two'});await open();
  await page.evaluate(()=>{window.__calls=[];window.__accounts.two.data={state:'archive',read_only:true,tasks:[{id:'old',goal:'已保留任务',state:'queued',device_id:'lab',artifacts:[]}],resources:{},diagnostics:{planner_available:false,planner_reason:'手机控制已停用；只能查看已保留的任务、截图和录屏。'}};window.__schema=[{id:'tasks',title:'保留的设备任务',component:'tasks',binding:'host_data.tasks',action:''},window.__originalSchema[2],{...window.__originalSchema[3],action:''},window.__originalSchema[0]]});await open();await settle();
  assert.equal(await page.getByRole('button',{name:'保存设备设置',exact:true}).count(),0);assert.equal(await page.getByRole('button',{name:'开始任务',exact:true}).count(),0);assert.equal(await page.getByRole('button',{name:'启动工具',exact:true}).count(),0);assert.equal(await page.getByRole('button',{name:'停止全部任务',exact:true}).count(),0);assert.equal(await page.getByRole('button',{name:'取消任务',exact:true}).count(),0);assert.equal(await page.getByRole('button',{name:'观察一次',exact:true}).count(),0);assert(await page.getByText('手机控制已停用；只能查看已保留的任务、截图和录屏。',{exact:true}).isVisible());
  assert.equal(await page.evaluate(()=>window.__calls.filter(call=>call.url.includes('/actions/')).length),0,'opening an archive must not invoke a host action');
  await page.evaluate(()=>{document.documentElement.dataset.theme='light'});await page.screenshot({path:path.join(output,'archive-light-1440.png'),fullPage:true});
  await page.getByRole('button',{name:'管理连接工具',exact:true}).click();assert(await page.locator('.plugin-workspace').isVisible());await page.getByRole('button',{name:'返回工具目录',exact:true}).click();assert(await page.locator('.package-browser').isVisible());assert.deepEqual(errors,[]);assert.deepEqual(consoleErrors,[]);console.log(JSON.stringify({schemaOrderOmitTitle:true,exactBindings:true,nestedPlannerFlags:true,disabledRuntimeStart:true,saveInFlightDraft:true,accountIsolation:true,idempotencyRetry:true,archiveReadOnly:true,visualWidths:[375,1440],visualThemes:['light','dark'],screenshots:output,paidCalls:0,wechatSends:0}));
 }catch(error){if(page)await page.screenshot({path:path.join(output,'failure.png'),fullPage:true}).catch(()=>{});console.error('Screenshots: '+output);throw error}finally{await browser.close()}
})().catch(error=>{console.error(error);process.exitCode=1});
