/* Account-scoped package administration. Does not invoke provider or WeChat tools. */
(() => {
 'use strict';
 const $=s=>document.querySelector(s),api=(...a)=>window.WechatAdmin.api(...a);
 const state={catalog:[],installed:[],connections:[],builtins:[],runtimes:{},readiness:{},retained:new Set(),selected:'',detail:null,epoch:0,busy:false,loadPromise:null,loaded:false,dirty:false,editVersion:0,hostRequest:null};
 const page=$('#plugins'),legacy=page.querySelector('.plugin-workspace'),guide=$('#plugin-guide');
 const el=(tag,text,klass)=>{const n=document.createElement(tag);if(text!==undefined)n.textContent=text;if(klass)n.className=klass;return n};
 const button=(text,action)=>{const b=el('button',text,'quiet');b.type='button';if(text==='刷新任务')window.WechatRefresh.bind(b,action);else b.addEventListener('click',action);return b};
 const area=el('div',undefined,'package-workspace'),browser=el('aside',undefined,'panel package-browser'),editor=el('article',undefined,'panel package-editor');
 const list=el('div',undefined,'package-list'),status=el('p','选择服务查看配置、权限和任务。','hint');status.setAttribute('role','status');
 const head=el('div',undefined,'section-heading');head.append(el('h3','浏览工具'),button('新建工具',createPlugin),button('管理连接工具',()=>manageConnections()),button('导入本机工具',importPackage));browser.append(head,list);editor.append(status);area.append(browser,editor);legacy.before(area);
 // The legacy declaration editor remains mounted for compatibility, but all
 // provider-facing configuration now has one visible home: the package detail
 // or the dedicated Connection & MCP page.
 function showPackages(){area.hidden=false;legacy.hidden=true;if(guide)guide.hidden=true;window.WechatSaveActions?.sync()}
 const account=()=>window.WechatAccountContext?.id||'';
 const context=()=>({id:state.selected,account:account(),epoch:state.epoch});
 const current=scope=>scope.epoch===state.epoch&&scope.id===state.selected&&scope.account===account()&&window.WechatAdmin?.isAuthenticated();
 const requestKey=()=>crypto.randomUUID?.()||Array.from(crypto.getRandomValues(new Uint8Array(16)),byte=>byte.toString(16).padStart(2,'0')).join('');
 const runtimeNames={running:'运行中',starting:'正在启动',stopping:'正在停止',failed:'异常',stopped:'已停止'};
 const names={'network':'访问网络','gpu':'使用 GPU','device':'控制设备','workspace.read':'读取授权工作区','workspace.write':'修改授权工作区','process.execute':'执行受控进程','artifact.read':'读取授权素材','artifact.write':'生成素材','model.invoke':'调用模型'};
 function packageSummary(p,installed,check,runtime){
  if(installed?.removed||!installed&&state.retained.has(p.id))return ['已卸载','查看保留的任务'];
  if(!installed)return ['未安装',p.status==='ready'?'导入工具后可配置':'开发模板，尚不能运行。'];
  const missing=(check?.missing_required_fields||[]).map(field=>field.label||field.id);
  const reason=(check?.reasons||[]).map(item=>item.message).filter(Boolean).join(' · ');
  if(runtime?.state==='starting'||runtime?.state==='stopping')return [runtimeNames[runtime.state],'查看运行状态'];
  if(runtime?.state==='running')return ['运行中',runtime.auto_managed?'按需运行，空闲后释放':'本次服务期间常驻'];
  if(check?.state==='needs_configuration')return ['待配置',missing.length?'需要填写：'+missing.join('、'):reason||'检查已保存的配置'];
  if(check?.state==='runtime_unavailable')return ['环境未就绪',reason||'检查本机执行环境'];
  if(check?.state==='not_enableable')return ['无法启用',reason||'查看工具权限与声明'];
  if(runtime?.state==='failed')return ['启动异常','查看运行状态或调整配置'];
  if(!check||check.state!=='ready')return ['状态待核对',reason||'打开查看配置与启用状态'];
  return installed.enabled?['按需待命','已启用，需要时启动']:['已停用','配置已齐，可按需启用'];
 }
 function allPackages(){
  const all=new Map(state.catalog.map(p=>[p.id,p]));
  for(const item of state.installed)all.set(item.package.id,item.package);
  for(const id of state.retained)if(!all.has(id))all.set(id,{id,name:id==='mobile-control'?'设备控制':'已保留工具',status:'retained'});
  return all;
 }
 function capabilityCard(p,installed,klass='package-choice'){
  const card=button('',()=>choose(p.id));card.className=klass;card.setAttribute('aria-pressed',String(state.selected===p.id));
  card.dataset.packageId=p.id;
  const [label,hint]=packageSummary(p,installed,state.readiness[p.id],state.runtimes[p.id]);
  const title=el('span',undefined,'package-card-title');
  title.append(el('strong',p.id==='music'?p.name+'（RVC / YuE）':p.name),el('small',label,'package-state'));
  card.append(title,el('small',hint,'hint'));
  return card;
 }
 function builtinCard(item,klass='package-choice'){
  const card=button('',()=>window.WechatAdmin.nav('permissions'));card.className=klass+' builtin-capability';
  const title=el('span',undefined,'package-card-title');title.append(el('strong',item.name),el('small',item.state,'package-state'));card.append(title);
  card.append(el('small',item.description,'hint'));card.append(el('small','授权范围：'+(item.permissions||[]).join('、'),'hint'));
  return card;
 }
 function renderGroups(container,all,overview){
  const cardClass=overview?'capability-overview-card':'package-choice';
  for(const builtin of [true,false]){
   const group=el('section',undefined,overview?'capability-overview-group':'package-group');
   const title=el('div',undefined,'package-group-heading'),cards=el('div',undefined,'package-group-cards');
   title.append(el('h4',builtin?'内置工具':'通用工具'));
   if(builtin)for(const item of state.builtins)cards.append(builtinCard(item,cardClass));
   for(const p of all.values()){
    if((p.id==='web-search')!==builtin)continue;
    cards.append(capabilityCard(p,state.installed.find(item=>item.package.id===p.id),cardClass));
   }
   if(!builtin)for(const item of state.connections){
    const card=button('',()=>manageConnections(item.id));card.className=cardClass;
    card.append(el('strong',item.manifest.name||item.id),el('small',(item.manifest.mcp?'MCP':'HTTP')+' · '+(item.enabled?'已启用':'未启用')));
    cards.append(card);
   }
   if(!builtin&&overview){const connections=button('管理 HTTP 与 MCP',()=>window.WechatAdmin.nav('mcp'));connections.className='secondary capability-connections';title.append(connections)}
   group.append(title,cards);
   if(cards.childElementCount||title.childElementCount>1)container.append(group);
  }
 }
 function renderOverview(all){
  if(state.selected)return;
  const intro=el('div',undefined,'package-overview-intro'),copy=el('div');copy.append(el('h3','工具目录'),el('p','点开工具查看配置与任务；微信收发权限可从内置工具卡片调整。','hint'));const actions=el('div',undefined,'package-overview-actions');actions.append(button('新建工具',createPlugin),button('管理连接工具',()=>manageConnections()),button('导入本机工具',importPackage));intro.append(copy,actions);
  const grid=el('div',undefined,'capability-overview-grid');
  renderGroups(grid,all,true);
  editor.replaceChildren(intro,grid);window.WechatSaveActions?.sync();
 }
 function dirty(value){state.dirty=value;if(value)state.editVersion++;window.WechatAdmin.pluginPackageDirty=value}
 function discard(){return !state.dirty||confirm('有未保存的工具配置，确定放弃？')}
 function renderList(){
  area.classList.toggle('package-overview',!state.selected);list.replaceChildren();const all=allPackages();
  renderGroups(list,all,false);
  renderOverview(all);
 }
 async function load(){
  if(!window.WechatAdmin?.isAuthenticated())return;if(state.loadPromise)return state.loadPromise;state.busy=true;const epoch=state.epoch,owner=account();
  const pending=(async()=>{try{const [data,connections]=await Promise.all([api('/api/plugin-packages'),api('/api/plugins').catch(()=>null)]);if(epoch!==state.epoch||owner!==account())return;state.connections=connections?.installed||[];state.catalog=data.catalog||[];state.installed=data.installed||[];state.builtins=data.builtins||[];state.runtimes=data.runtimes||{};state.readiness=data.readiness||{};state.retained=new Set(data.retained_ids||[]);state.loaded=true;renderList()}
  catch(e){if(epoch===state.epoch&&owner===account()){status.textContent='工具目录读取失败：'+e.message;if(!state.selected)editor.prepend(status)}}})();state.loadPromise=pending;try{return await pending}finally{if(state.loadPromise===pending){state.busy=false;state.loadPromise=null}}
 }
 function backToOverview(){
  if(!discard())return;
  state.epoch++;state.loadPromise=null;state.selected='';state.detail=null;dirty(false);
  renderList();editor.scrollTop=0;area.scrollTop=0;load();
 }
 function detailHeading(name){
  const heading=el('div',undefined,'section-heading');
  heading.append(el('h3',name),button('返回工具概览',backToOverview));
  return heading;
 }
 async function choose(id){
  if(id===state.selected&&state.detail)return;if(!discard())return;
  const epoch=++state.epoch,owner=account();state.selected=id;state.detail=null;dirty(false);renderList();editor.replaceChildren(detailHeading(id),status);status.textContent='正在读取工具…';
  const installed=state.installed.find(x=>x.package.id===id),p=installed?.package||state.catalog.find(x=>x.id===id);
  if(!installed&&!state.retained.has(id)){status.textContent=p?.status==='ready'?'此工具尚未安装。导入并确认权限后可配置。':'此开发模板尚不能运行。';editor.replaceChildren(detailHeading(p?.name||id),status);return}
  try{const d=await api('/api/plugin-packages/'+encodeURIComponent(id));if(epoch!==state.epoch||owner!==account())return;state.detail=d;renderDetail(d)}catch(e){if(epoch===state.epoch&&owner===account())status.textContent=e.message}
 }
 function renderDetail(d){
  if(state.selected==='mobile-control'&&d.host_data){renderHostDetail(d);return}
  const p=d.installed.package,heading=detailHeading(p.name),runtime=d.runtime||{state:'stopped'};
  if(!d.installed.removed)runtimeControls(heading,d);
  editor.replaceChildren(heading,status);status.textContent=d.runtime_connected?(runtimeNames[runtime.state]||'状态待核对')+(d.installed.enabled?'；停止后可修改配置。':'；保存配置后可启动。'):'执行环境未配置；可先保存配置，安装不会自动启动。';
  if(d.readiness){const [label,hint]=packageSummary(p,d.installed,d.readiness,runtime);status.textContent=label+' · '+hint+(d.installed.enabled?'。停用后可修改配置。':'')}
  const permissions=el('p',(d.installed.permissions||[]).map(x=>names[x]||x).join('、')||'未授予额外权限','hint');editor.append(permissions);
  const form=el('form');form.id='package-config-form';const fields=el('div',undefined,'package-fields');
  const configured=new Set(d.configuration.configured_secrets||[]);
  for(const [name,spec] of Object.entries(d.schema.properties||{})){
   const row=el('div',undefined,'package-field'),label=el('label',(spec.title||name)+(d.schema.required?.includes(name)?'（必填）':''));row.append(label);let input;
   if(spec.enum?.length){input=el('select');for(const v of spec.enum){const o=new Option(String(v),JSON.stringify(v));input.add(o)}}else{input=el('input');input.type=spec['x-secret']?'password':spec.type==='boolean'?'checkbox':['integer','number'].includes(spec.type)?'number':'text';if(spec.type==='integer')input.step='1';if(spec.minimum!=null)input.min=spec.minimum;if(spec.maximum!=null)input.max=spec.maximum;if(spec.maxLength!=null)input.maxLength=spec.maxLength}
   input.disabled=d.installed.enabled||d.installed.removed;input.id='package-field-'+name;label.htmlFor=input.id;input.dataset.configField=name;const value=d.configuration.values[name]??spec.default;
   if(spec['x-secret']){input.autocomplete='new-password';input.placeholder=configured.has(name)?'已配置，留空保留':'未配置'}else if(spec.enum?.length){input.value=value===undefined?'':JSON.stringify(value)}else if(spec.type==='boolean')input.checked=!!value;else input.value=value??'';
   if(d.schema.required?.includes(name)&&!spec['x-secret'])input.required=true;
   row.append(input);if(spec.description)row.append(el('small',spec.description,'hint'));fields.append(row);
   if(spec['x-secret']&&configured.has(name)){const clear=el('label',undefined,'package-secret-clear'),check=el('input');check.type='checkbox';check.disabled=d.installed.enabled||d.installed.removed;check.dataset.clearSecret=name;clear.append(check,el('span','清除此密钥'));row.append(clear)}
  }
  const save=button('保存工具配置',()=>{});save.id='package-config-save';save.type='submit';save.className='primary';save.disabled=d.installed.enabled||d.installed.removed;form.append(fields,save);editor.append(form);
  form.addEventListener('input',()=>dirty(true));form.addEventListener('submit',saveConfig);
  const taskHead=el('div',undefined,'section-heading');taskHead.append(el('h3','任务'),button('刷新任务',()=>loadJobs(true)));editor.append(taskHead);const tasks=el('div');tasks.id='package-jobs';editor.append(tasks);loadJobs(true);window.WechatSaveActions?.sync();
 }
 function runtimeControls(heading,d){
  const runtime=d.runtime||{},enabled=d.installed.enabled,scope=context();
  function add(label,action,tip){
   const control=button(label,()=>{if(current(scope))runtimeAction(action)});
   control.title=tip;
   control.dataset[action==='enable_on_demand'?'pluginPolicyAction':'pluginRuntimeAction']=action;
   control.disabled=action!=='stop'&&(!d.runtime_connected||Boolean(d.readiness&&d.readiness.state!=='ready'));
   heading.append(control);
  }
  if(!enabled||runtime.state==='running'&&!runtime.auto_managed)add(enabled?'恢复按需':'按需启用','enable_on_demand','任务需要时启动，空闲后释放');
  if(!enabled||runtime.state==='stopped'||runtime.auto_managed)add('启动工具','start','立即启动并保留至本次服务退出');
  if(enabled)add('停止工具','stop','停止并停用，后续请求不会启动此工具');
 }
 async function runtimeAction(action){
  if(!state.detail)return;if(state.dirty){status.textContent='请先保存当前配置，再启动工具。';return}
  const scope=context(),id=scope.id,editVersion=state.editVersion,controls=Array.from(editor.querySelectorAll('[data-plugin-runtime-action],[data-plugin-policy-action]')).filter(control=>!control.disabled);controls.forEach(control=>control.disabled=true);
  let failure='';
  try{await api('/api/plugin-packages/'+encodeURIComponent(id)+'/runtime',{method:'POST',json:{action,revision:state.detail.installed.revision}})}catch(e){failure=e.message}
  if(!current(scope))return;
  try{const d=await api('/api/plugin-packages/'+encodeURIComponent(id));if(!current(scope))return;state.detail=d;const i=state.installed.findIndex(x=>x.package.id===id);if(i>=0)state.installed[i]=d.installed;state.runtimes[id]=d.runtime||{};state.readiness[id]=d.readiness;renderList();if(editVersion!==state.editVersion||state.dirty){status.textContent=failure||'工具状态已更新；当前未保存的编辑已保留。';return}renderDetail(d);if(failure)status.textContent=failure}
  catch(e){if(current(scope)){status.textContent=failure||e.message;controls.forEach(control=>{if(control.isConnected)control.disabled=false})}}
 }
 async function saveConfig(event){
  event.preventDefault();if(!state.detail)return;const scope=context(),id=scope.id,editVersion=state.editVersion,d=state.detail,form=$('#package-config-form');if(!form.reportValidity())return;
  const body={revision:d.configuration.revision,values:{},secrets:{},clear_secrets:[]};
  for(const input of form.querySelectorAll('[data-config-field]')){const name=input.dataset.configField,spec=d.schema.properties[name];if(spec['x-secret'])body.secrets[name]=input.value;else if(spec.enum?.length){if(input.value!=='')body.values[name]=JSON.parse(input.value)}else if(spec.type==='boolean')body.values[name]=input.checked;else if(['integer','number'].includes(spec.type)){if(input.value!=='')body.values[name]=Number(input.value)}else if(input.value!==''||!(d.installed.package.http_routes||[]).some(r=>r.optional&&r.endpoint_config===name))body.values[name]=input.value}
  for(const check of form.querySelectorAll('[data-clear-secret]:checked'))body.clear_secrets.push(check.dataset.clearSecret);
  const save=$('#package-config-save');save.disabled=true;
  try{
   const result=await api('/api/plugin-packages/'+encodeURIComponent(id)+'/configuration',{method:'POST',json:body});
   if(!current(scope))return;
   state.detail.configuration=result;
   // The submitted revision is saved, but a valid edit can still lack other required fields.
   state.detail.readiness={state:'unknown'};state.readiness[id]=state.detail.readiness;renderList();
   if(editVersion!==state.editVersion){status.textContent='已保存提交的配置，后续编辑尚未保存。';return}
   dirty(false);
   let refreshed;
   try{refreshed=await api('/api/plugin-packages/'+encodeURIComponent(id))}catch{
    if(current(scope)&&editVersion===state.editVersion){renderDetail(state.detail);status.textContent='配置已保存，暂时无法核对启用条件。请返回工具概览刷新。'}
    return;
   }
   if(!current(scope)||editVersion!==state.editVersion)return;
   state.detail=refreshed;state.readiness[id]=refreshed.readiness;renderList();renderDetail(refreshed);
   if(!refreshed.readiness||refreshed.readiness.state==='ready')status.textContent='工具配置已保存，未启动服务。';
   else status.textContent='配置已保存。'+status.textContent;
  }catch(e){if(current(scope))status.textContent=e.message+'；当前编辑已保留。'}finally{if(save.isConnected)save.disabled=false}

 }
 let next=0;
 function jobDiagnostic(job){
  const stage=job?.diagnostic?.stage;
  if(stage==='dispatch')return '调用工具未完成。';
  if(stage==='authority')return '核对权限时未能继续执行。';
  if(stage==='validation')return '检查结果时未能通过核对。';
  if(stage==='result_mode')return '结果格式不符合此操作。';
  return job?.state==='needs_review'?'结果需要核对；未自动重试。':'';
 }
 function time(value,fallback='时间待核对'){
  const instant=new Date(value||'');
  return !Number.isFinite(instant.getTime())||instant.getTime()<0?fallback:new Intl.DateTimeFormat('zh-CN',{dateStyle:'medium',timeStyle:'medium'}).format(instant);
 }
 function phoneConnectionLabel(value,takeover){
  if(takeover)return '已接管';
  return ({connected:'已连接',disconnected:'未连接',paused:'已暂停',takeover:'已接管',unknown:'状态待核对'})[value]||'状态待核对';
 }
 function registeredRange(device){
  if(device.authorization_state!=='ready')return '尚未读取授权范围。';
  const apps=Array.isArray(device.authorized_apps)?device.authorized_apps.filter(Boolean):[];
  const actions=Array.isArray(device.authorized_actions)?device.authorized_actions.filter(Boolean):[];
  return '已登记应用：'+(apps.length?apps.join('、'):'无')+' · 已登记动作：'+(actions.length?actions.join('、'):'无');
 }
 function observationGrants(device,fish,bindingID){
  const grants=Array.isArray(device.observation_apps)?device.observation_apps:[];
  return grants.filter(grant=>grant&&typeof grant.package==='string'&&grant.package.trim()&&String(grant.binding_id||'').trim()===bindingID&&(!grant.fish_id||String(grant.fish_id).trim()===fish));
 }
 function observationAppControls(device,fish,deviceFish,bindingID,action,reload,current,message){
  const box=el('fieldset',undefined,'mobile-observation-apps'),legend=el('legend','应用观察授权'),apps=(Array.isArray(device.authorized_apps)?device.authorized_apps:[]).map(value=>String(value||'').trim()).filter(Boolean),actions=(Array.isArray(device.authorized_actions)?device.authorized_actions:[]).map(value=>String(value||'').trim()),grants=observationGrants(device,fish,bindingID),granted=new Set(grants.map(grant=>grant.package)),packages=[...new Set([...apps,...granted])],bound=Boolean(fish&&deviceFish===fish&&bindingID),recordsReadable=device.observation_apps_state!=='unavailable',canEnable=bound&&recordsReadable&&device.authorization_state==='ready'&&actions.includes('observe');
  box.append(legend,el('small','仅设置授权，暂不自动查看手机；后续探索仅可读取当前应用画面，不包含点击、评论或发布。','hint'));
  if(!bound)box.append(el('small','绑定大肥鱼设备后才能设置自主观察。','hint'));
  else if(!recordsReadable)box.append(el('small','授权记录暂无法读取，暂不能更改。','hint'));
  else if(!granted.size&&!packages.length)box.append(el('small','当前没有已登记应用可设置。','hint'));
  else if(!canEnable)box.append(el('small',device.authorization_state==='ready'?'当前技术授权未允许观察，暂不能新增授权。':'尚未读取授权范围，暂不能新增授权。','hint'));
  if(!packages.length)return box;
  const list=el('div',undefined,'mobile-observation-app-list');
  for(const pkg of packages){
   const grant=grants.find(item=>item.package===pkg),enabled=Boolean(grant),available=canEnable&&apps.includes(pkg)&&grant?.available!==false,label=el('label',undefined,'mobile-observation-app'),toggle=document.createElement('input'),copy=el('span',pkg+' · 允许自主观察');
   toggle.type='checkbox';toggle.checked=enabled;toggle.disabled=!bound||!recordsReadable||(!enabled&&!canEnable);
   toggle.addEventListener('change',async()=>{
    const next=toggle.checked;toggle.disabled=true;
    try{const result=await action('overview.observation-apps',{device_id:device.device_id,binding_id:bindingID,package:pkg,enabled:next});await reload();if(current()&&result?.warning)message(result.warning)}catch(error){toggle.checked=enabled;const detail=error?.message||'应用观察授权未更新。';try{await reload()}catch{}message(detail,true)}finally{if(current()&&toggle.isConnected)toggle.disabled=!bound||!recordsReadable||(!toggle.checked&&!canEnable)}
   });
   label.append(toggle,copy);if(!available)label.append(el('small','暂不可用','mobile-observation-app-state'));list.append(label);
  }
  box.append(list);return box;
 }
 function renderMobileObservation(panel,data,action,reload,current,archive,message){
  if(!Object.prototype.hasOwnProperty.call(data,'overview'))return;
  const overview=data.overview||{},devices=Array.isArray(overview.devices)?overview.devices:[];
  const workbench=el('section',undefined,'mobile-observation-workbench');
  workbench.append(el('h4','手机观察'));
  const summary=el('p',devices.length?'连接状态来自最近一次检查，画面显示最近一次实际截图。':'尚未登记可观察的设备。请先在设备设置中授权设备。','hint');
  workbench.append(summary);
  if(!devices.length){panel.append(workbench);return;}
  const list=el('div',undefined,'mobile-observation-list');
  for(const device of devices){
   const card=el('article',undefined,'mobile-observation-card'),head=el('div',undefined,'mobile-observation-head');
   const fish=typeof overview.fish_id==='string'?overview.fish_id.trim():'',deviceFish=typeof device.fish_id==='string'?device.fish_id.trim():'',bindingID=typeof device.binding_id==='string'?device.binding_id.trim():'';
   const title=el('div');title.append(el('strong',device.device_id||'未命名设备'),el('small',phoneConnectionLabel(device.connection_state,device.takeover),'mobile-observation-state'));
   const checked=el('small','检查于 '+time(device.checked_at,'尚未检查'),'hint');head.append(title,checked);card.append(head);
   const content=el('div',undefined,'mobile-observation-content'),visual=el('div',undefined,'mobile-observation-visual'),details=el('div',undefined,'mobile-observation-details'),meta=el('div',undefined,'mobile-observation-meta');
   meta.append(el('p',device.current_app?'上次观察应用：'+device.current_app:'上次观察未取得应用信息','hint'),el('p',registeredRange(device),'hint'));details.append(meta);
   if(device.screenshot_uri){
    const image=el('img');image.className='mobile-observation-image';image.alt='此设备最近一次实际截图';image.loading='lazy';image.src=window.WechatAccountContext?.url?.(device.screenshot_uri)||device.screenshot_uri;visual.append(image);
    visual.append(el('small','观察于 '+time(device.observed_at),'hint'));
   }else visual.append(el('small','还没有可显示的实际截图。','hint'));
   if(!archive){
    const controls=el('div',undefined,'inline-actions mobile-observation-actions');
    const invoke=(name,label,disabled=false)=>{const control=button(label,async()=>{control.disabled=true;try{await action(name,{device_id:device.device_id});await reload()}catch(error){const detail=error?.message||'观察操作未完成。';try{await reload()}catch{}message(detail,true)}finally{if(current()&&control.isConnected)control.disabled=false}});control.disabled=disabled;controls.append(control)};
    if(device.connection_state==='paused'||device.takeover)invoke('overview.resume','恢复观察');
    else invoke('overview.observe','观察一次',Boolean(fish&&!deviceFish));
    if(device.connection_state!=='paused'&&!device.takeover)invoke('overview.pause','暂停观察');
    if(!device.takeover)invoke('overview.takeover','接管设备');
    details.append(controls);
    const identity=el('div',undefined,'mobile-observation-identity');
    if(!fish)identity.append(el('small','请先在微信账号绑定身份，再为设备归属大肥鱼。','hint'));
    else if(!deviceFish){
     identity.append(el('small','先绑定设备后才能观察或启动任务。','hint'));
     const bind=button('设为大肥鱼的设备',async()=>{bind.disabled=true;try{await action('overview.bind',{device_id:device.device_id});await reload()}catch(error){const detail=error?.message||'设备绑定未完成。';try{await reload()}catch{}message(detail,true)}finally{if(current()&&bind.isConnected)bind.disabled=false}});
     identity.append(bind);
    }else if(deviceFish===fish&&bindingID){
     identity.append(el('small','已绑定大肥鱼','mobile-observation-bound'));
     const unbind=button('解除绑定',()=>{
      const popup=window.WechatDialogs.open({title:'解除设备绑定',subtitle:device.device_id||'未命名设备'}),copy=el('p','解除后会暂停此设备的后续任务，已保存的任务和观察历史仍会保留。','hint'),actions=el('div',undefined,'button-row'),cancel=button('取消',()=>popup.close()),confirm=button('确认解除',async()=>{confirm.disabled=true;cancel.disabled=true;try{const result=await action('overview.unbind',{device_id:device.device_id,binding_id:bindingID});if(current())popup.close();await reload();if(current()&&result?.warning)message(result.warning)}catch(error){message((error?.message||'解除绑定未完成。'),true);confirm.disabled=false;cancel.disabled=false}});confirm.className='danger-text';actions.append(cancel,confirm);popup.body.append(copy,actions);
     });identity.append(unbind);
    }else if(deviceFish===fish)identity.append(el('small','绑定信息待核对。','hint'));
    else identity.append(el('small','已绑定其他身份。','hint'));
    details.append(identity);
    details.append(observationAppControls(device,fish,deviceFish,bindingID,action,reload,current,message));
   }
   content.append(details,visual);card.append(content);
   list.append(card);
  }
  workbench.append(list);panel.append(workbench);
 }
 async function loadJobs(reset){
  const id=state.selected,epoch=state.epoch,box=$('#package-jobs');if(!box)return;if(reset){next=0;box.replaceChildren()}
  try{const page=await api('/api/plugin-packages/'+encodeURIComponent(id)+'/jobs?limit=30&after='+next);if(epoch!==state.epoch||!box.isConnected)return;box.querySelector('[data-more-jobs]')?.remove();next=page.next;
   const labels={pending:'待执行',submitting:'正在提交',running:'运行中',completed:'已完成',failed:'失败',cancelled:'已取消',needs_review:'需要核对'};
   for(const job of page.items||[]){const row=el('div',undefined,'package-job'),summary=el('div');summary.append(el('span',(labels[job.state]||job.state)+(job.cancel_requested&&job.state!=='cancelled'?' · 正在请求取消':'')));const diagnosis=jobDiagnostic(job);if(diagnosis)summary.append(el('small',diagnosis,'hint'));row.append(summary);if(!['completed','failed','cancelled'].includes(job.state)){const cancel=button('取消任务',async()=>{cancel.disabled=true;try{await api('/api/plugin-packages/'+encodeURIComponent(id)+'/jobs/'+encodeURIComponent(job.id)+'/cancel',{method:'POST',json:{revision:job.revision}});if(epoch===state.epoch)loadJobs(true)}catch(e){status.textContent=e.message;cancel.disabled=false}});cancel.disabled=job.cancel_requested;row.append(cancel)}box.append(row)}
   if(!box.children.length)box.append(el('p','暂无工具任务。','hint'));if(page.has_more){const more=button('继续读取任务',()=>loadJobs(false));more.dataset.moreJobs='';box.append(more)}
  }catch(e){if(epoch===state.epoch)status.textContent=e.message}
 }
 function renderHostDetail(d){
  const scope=context(),data=d.host_data||{},diagnostics=data.diagnostics||{},archive=data.state==='archive',readOnly=archive&&data.read_only===true,runtime=d.runtime||{state:'stopped'};
  const heading=detailHeading(d.installed?.package?.name||'设备控制');
  if(!archive&&d.installed&&!d.installed.removed){
   runtimeControls(heading,d);
  }
  editor.replaceChildren(heading,status);status.textContent=readOnly?'手机控制已停用。原有设备任务、截图和录屏仅供查看，不会创建、停止或取消任务。':archive?'此工具已卸载。原有设备任务和受保护素材仍可查看或停止，不会创建新任务。':!d.installed?.enabled?'工具未启动；保存设备设置后，可按需启用或手动启动工具。':runtimeNames[runtime.state]||'状态待核对';
  const message=(text,error=false)=>{if(current(scope)){status.textContent=text;status.classList.toggle('form-error',error)}};
  status.classList.remove('form-error');
  const action=async(name,body={})=>{
   if(!current(scope))throw new Error('账号或工具已变化，请重新打开工具。');
   return api('/api/plugin-packages/'+encodeURIComponent(scope.id)+'/actions/'+encodeURIComponent(name),{method:'POST',json:body});
  };
  const reload=async()=>{
   if(!current(scope))return;const next=await api('/api/plugin-packages/'+encodeURIComponent(scope.id));if(!current(scope))return;
   state.detail=next;
   if(state.dirty){message('状态已读取；当前未保存的设备设置已保留。');return}
   renderDetail(next);
  };
  const bind=(container,label,input,id)=>{input.id='package-mobile-'+id;const caption=el('label',label);caption.htmlFor=input.id;container.append(caption,input)};
  const allowed={configuration:['host_data.settings',['settings.save']],tasks:['host_data.tasks',['tasks.create','tasks.cancel','tasks.stop','']],resources:['host_data.resources',['']],diagnostics:['host_data.diagnostics',['devices.list','']]},seen=new Set();
  for(const declaration of Array.isArray(d.ui_schema?.sections)?d.ui_schema.sections:[]){
   const spec=allowed[declaration.component],section={...declaration,binding:declaration.binding||spec?.[0],action:declaration.action||({configuration:'settings.save',tasks:readOnly?'':archive?'tasks.cancel':'tasks.create',diagnostics:archive?'':'devices.list'})[declaration.component]||''};
   if(!spec||section.binding!==spec[0]||!spec[1].includes(section.action||'')||seen.has(section.component)||archive&&section.component==='configuration')continue;
   seen.add(section.component);
   const panel=el('section',undefined,'panel package-host-'+section.component);panel.id='plugin-section-'+section.component;panel.dataset.hostComponent=section.component;panel.append(el('h3',section.title||section.component));editor.append(panel);
   if(section.component==='diagnostics'){
    panel.append(el('p',archive?diagnostics.planner_reason||(readOnly?'仅可查看历史任务和素材。':'仅可查看历史或停止已有任务。'):diagnostics.planner_available===true?'设备规划工具已就绪。':diagnostics.planner_reason||'设备规划工具未就绪，暂不能新建任务。','hint'));
    if(!archive&&section.action==='devices.list'){
     const result=el('p','','hint'),discover=button('检查已授权设备',async()=>{discover.disabled=true;try{const devices=await action('devices.list');if(current(scope)&&result.isConnected)result.textContent=JSON.stringify(devices)}catch(error){message(error.message,true)}finally{if(current(scope)&&discover.isConnected)discover.disabled=false}});panel.append(discover,result);
    }
   }
   if(section.component==='configuration'){
    const settings=data.settings||{},form=el('form'),grid=el('div',undefined,'field-grid'),enabled=el('input'),devices=el('textarea');form.className='package-host-form';enabled.type='checkbox';enabled.checked=!!settings.enabled;enabled.id='package-mobile-enabled';devices.rows=2;devices.value=(settings.devices||[]).join('\n');
    const switchRow=el('label',undefined,'switch-row');switchRow.htmlFor=enabled.id;switchRow.append(el('span','启用设备执行'),enabled);const deviceField=el('div');bind(deviceField,'允许设备 ID（每行一个）',devices,'devices');grid.append(switchRow,deviceField);
    const limits=el('div',undefined,'field-grid'),maxSteps=el('input'),maxSeconds=el('input');for(const input of [maxSteps,maxSeconds]){input.type='number';input.min='1';input.step='1';input.required=true}
    maxSteps.value=settings.max_steps??8;maxSeconds.value=settings.max_seconds??120;const stepField=el('div'),timeField=el('div');bind(stepField,'单次任务最多步骤',maxSteps,'max-steps');bind(timeField,'单次任务最长秒数',maxSeconds,'max-seconds');limits.append(stepField,timeField);
    const actions=el('fieldset',undefined,'mobile-action-options');actions.append(el('legend','允许动作（不选时只观察）'));
    for(const [key,label] of Object.entries({launch:'打开允许应用',tap:'点击',swipe:'滑动',back:'返回',home:'主屏幕',text:'输入英文/数字',capture:'短录屏观察（无音轨）',open_url:'允许浏览器打开公开链接'})){
     const row=el('label'),input=el('input');input.type='checkbox';input.checked=(settings.actions||[]).includes(key);input.dataset.mobileAction=key;row.append(input,document.createTextNode(label));actions.append(row);
    }
    const save=button('保存设备设置',()=>{});save.type='submit';save.className='primary';save.id='package-mobile-save';
    form.append(grid,limits,actions,el('p',settings.connection_profile_status==='已登记'?'设备连接已登记。设备范围和动作权限由当前账号保存。':'设备连接待登记；本页只保存执行范围和权限。','hint'),save);
    form.addEventListener('input',()=>{if(current(scope))dirty(true)});
    form.addEventListener('submit',async event=>{
     event.preventDefault();if(!current(scope)||save.disabled||!form.reportValidity())return;
     const editVersion=state.editVersion,body={revision:state.detail.host_data.settings.revision,enabled:enabled.checked,devices:devices.value.split(/\r?\n/).map(x=>x.trim()).filter(Boolean),actions:[...form.querySelectorAll('[data-mobile-action]:checked')].map(x=>x.dataset.mobileAction),max_steps:Number(maxSteps.value),max_seconds:Number(maxSeconds.value)};save.disabled=true;
     try{const result=await action('settings.save',body);if(!current(scope))return;state.detail.host_data.settings=result;
      if(editVersion!==state.editVersion){message('已保存提交的设备设置，后续编辑尚未保存。');return}
      dirty(false);await reload();if(!state.dirty)message('设备设置已保存；工具运行状态未改变。');
     }catch(error){message(error.message+'；当前编辑已保留。',true)}finally{if(current(scope)&&save.isConnected)save.disabled=false}
    });panel.append(form);
   }
   if(section.component==='tasks'){
    const controls=el('div',undefined,'inline-actions'),refresh=button('刷新任务',async()=>{try{await reload()}catch(error){message(error.message,true)}});controls.append(refresh);
    if(!readOnly){const stop=button('停止全部任务',async()=>{stop.disabled=true;try{await action('tasks.stop');await reload()}catch(error){message(error.message,true)}finally{if(current(scope)&&stop.isConnected)stop.disabled=false}});stop.disabled=!(data.tasks||[]).some(task=>['queued','running'].includes(task.state));controls.append(stop)}
    panel.append(controls);
    renderMobileObservation(panel,data,action,reload,()=>current(scope),archive,message);
    if(!archive&&section.action==='tasks.create'){
     const form=el('form',undefined,'package-host-task-form'),grid=el('div',undefined,'field-grid'),select=el('select'),goal=el('textarea');select.required=goal.required=true;goal.rows=2;goal.maxLength=1000;goal.placeholder='查看当前页面，描述可见内容，不点击或修改设置';for(const id of data.settings?.devices||[])select.append(new Option(id,id));
     const previous=state.hostRequest;if(previous?.account===scope.account&&previous.id===scope.id){select.value=previous.payload.device_id;goal.value=previous.payload.goal}
     const a=el('div'),b=el('div');bind(a,'设备',select,'device');bind(b,'任务目标',goal,'goal');grid.append(a,b);
     const start=button('开始任务',()=>{});start.type='submit';start.className='primary';start.disabled=!(d.installed?.enabled&&runtime.state==='running'&&data.settings?.enabled&&data.settings?.devices?.length&&diagnostics.planner_available===true);
     form.append(grid,start,el('p','任务只在已授权设备和动作范围内执行；登录、验证码及不确定结果会停止并记录。','hint'));
     const invalidate=()=>{if(current(scope)&&state.hostRequest?.signature!==JSON.stringify({device_id:select.value,goal:goal.value.trim()}))state.hostRequest=null};select.addEventListener('change',invalidate);goal.addEventListener('input',invalidate);
     form.addEventListener('submit',async event=>{
      event.preventDefault();if(!current(scope)||!form.reportValidity()||start.disabled)return;
      const payload={device_id:select.value,goal:goal.value.trim()},signature=JSON.stringify(payload),prior=state.hostRequest;
      if(!prior||prior.account!==scope.account||prior.id!==scope.id||prior.signature!==signature)state.hostRequest={account:scope.account,id:scope.id,signature,payload,key:requestKey()};
      const request=state.hostRequest;start.disabled=true;
      try{const result=await action('tasks.create',{...payload,request_key:request.key});if(!current(scope))return;if(!result?.id)throw new Error('任务提交结果待核对，可使用相同请求编号重试');if(state.hostRequest===request)state.hostRequest=null;await reload()}
      catch(error){message(error.message+'；未自动重试。再次提交相同目标会复用请求编号。',true)}finally{if(current(scope)&&start.isConnected)start.disabled=false}
     });panel.append(form);
    }
    const tasks=el('div',undefined,'mobile-tasks');for(const task of data.tasks||[]){
     const row=el('article',undefined,'mobile-task'),head=el('div',undefined,'section-heading');head.append(el('strong',task.goal||'未命名设备任务'),el('small',({queued:'等待执行',running:'执行中',completed:'已完成',cancelled:'已取消',needs_review:'需要检查',failed:'失败',stopped:'已停止'})[task.state]||task.state,'hint'));row.append(head,el('p',(task.result||task.reason||'正在检查设备状态')+' · '+(task.device_id||'设备待核对'),'hint'));
     if(task.observation_id){const image=el('img');image.alt='此任务最近一次实际设备截图';image.loading='lazy';image.src=window.WechatAccountContext?.url?.('/api/mobile/tasks/'+encodeURIComponent(task.id)+'/screenshot/'+encodeURIComponent(task.observation_id))||'';row.append(image)}
     for(const artifact of task.artifacts||[]){const video=el('video');video.controls=true;video.preload='metadata';video.src=window.WechatAccountContext?.url?.('/api/mobile/tasks/'+encodeURIComponent(task.id)+'/recording/'+encodeURIComponent(artifact.id))||'';row.append(el('p',artifact.summary||'实际短录屏，无音轨','hint'),video)}
     if(!readOnly&&['queued','running'].includes(task.state)){const cancel=button('取消任务',async()=>{cancel.disabled=true;try{await action('tasks.cancel',{id:task.id});await reload()}catch(error){message(error.message,true)}finally{if(current(scope)&&cancel.isConnected)cancel.disabled=false}});row.append(cancel)}tasks.append(row);
    }
    if(!tasks.children.length)tasks.append(el('p',archive?'没有保留的设备任务。':'暂无设备任务。','hint'));panel.append(tasks);
   }
   if(section.component==='resources')panel.append(el('p','截图和录屏会随对应任务显示，仍受当前账号权限保护。','hint'));
  }
  editor.scrollTop=0;area.scrollTop=0;window.WechatSaveActions?.sync();
 }
 function manageConnections(id){
  if(!discard())return;
  if(state.dirty){dirty(false);if(state.detail)renderDetail(state.detail)}
  area.hidden=true;legacy.hidden=false;
  window.WechatPluginDeclarations.open(id);
  window.WechatSaveActions?.sync();
 }
 function createPlugin(){
  if(!window.WechatAdmin.isAuthenticated())return;
  const owner=account(),epoch=state.epoch;
  const modal=window.WechatDialogs.open({title:'新建工具',subtitle:'连接已有的 HTTP 接口或 MCP 服务，无需准备压缩包。'});
  const form=el('form',undefined,'package-fields'),heading=el('h3'),fields=el('div',undefined,'package-fields'),actions=el('div',undefined,'inline-actions'),error=el('p','','error');
  error.setAttribute('role','alert');
  const type=el('select'),id=el('input'),name=el('input'),endpoint=el('input'),detail=el('input'),param=el('input');
  type.add(new Option('HTTP 工具（POST JSON）','http'));type.add(new Option('远程 MCP 服务','mcp'));
  id.pattern='[A-Za-z][A-Za-z0-9_\\-]{0,63}';id.placeholder='my-tool';name.placeholder='例如：术语查询';
  endpoint.type='url';endpoint.placeholder='https://…';detail.placeholder='说明 Agent 何时使用这个工具';param.value='text';
  const field=(label,input)=>{const row=el('label',label);input.required=true;row.append(input);return row};
  const back=button('上一步',()=>{step--;draw()}),next=el('button','下一步','primary');next.type='submit';
  let step=0,busy=false;
  const valid=()=>modal.alive()&&owner===account()&&epoch===state.epoch&&window.WechatAdmin.isAuthenticated();
  function draw(){
   const http=type.value==='http';fields.replaceChildren();error.textContent='';
   heading.textContent=['1 / 3　工具信息','2 / 3　连接设置','3 / 3　核对并创建'][step];
   back.hidden=step===0;next.textContent=step===2?'确认创建':'下一步';
   if(step===0)fields.append(field('工具类型',type),field('显示名称',name),field('唯一标识',id));
   if(step===1){
    fields.append(field('服务地址',endpoint),field('用途说明',detail));
    if(http){param.pattern='[A-Za-z][A-Za-z0-9_]{0,63}';fields.append(field('接口接收的文本参数名',param))}
   }
   if(step===2)fields.append(el('strong',name.value+' · '+id.value),el('p',(http?'HTTP POST':'MCP')+'　'+endpoint.value),el('p',detail.value),el('p',http?'请求字段：'+param.value:'由 MCP 服务声明可用工具'),el('p','创建后保持未启用，可在连接工具中编辑参数、鉴权和启用状态。','hint'));
  }
  form.addEventListener('submit',async event=>{
   event.preventDefault();if(busy||!valid())return;
   if(step===0&&(!id.value.trim()||id.value.includes('__')||!name.value.trim())){error.textContent='请填写名称，并使用不含双下划线的有效标识。';return}
   if(step===1){
    let url;try{url=new URL(endpoint.value)}catch{error.textContent='请输入完整的服务地址。';return}
    if(!['http:','https:'].includes(url.protocol)||url.username||url.password||url.hash||!detail.value.trim()){error.textContent='请输入 HTTP 或 HTTPS 服务地址和用途，凭据请在创建后单独配置。';return}
   }
   if(step<2){step++;draw();return}
   const manifest={id:id.value.trim(),name:name.value.trim(),version:'1',description:detail.value.trim()};
   if(type.value==='http'){
    const key=param.value.trim();
    manifest.tools=[{name:'call',description:manifest.description,method:'POST',url:endpoint.value.trim(),input_schema:{type:'object',properties:{[key]:{type:'string'}},required:[key]},body_template:{[key]:'{{arg.'+key+'}}'}}];
   }else manifest.mcp={id:manifest.id,name:manifest.name,transport:'http',url:endpoint.value.trim()};
   busy=true;back.disabled=next.disabled=true;
   try{
    await api('/api/plugins/install',{method:'POST',json:{manifest,enabled:false}});
    if(valid()){modal.close();manageConnections(manifest.id)}
   }catch(e){if(valid())error.textContent=e.message}
   finally{busy=false;back.disabled=next.disabled=false}
  });
  actions.append(back,next);form.append(heading,fields,error,actions);modal.body.append(form);draw();
 }
 function importPackage(){
  const account=window.WechatAccountContext?.id||'',epoch=state.epoch;
  const modal=window.WechatDialogs.open({title:'导入工具',subtitle:'选择 ZIP 文件或填写本机目录，检查后确认安装。'}),label=el('label','本机工具目录'),input=el('input'),fileLabel=el('label','工具 ZIP 文件'),file=el('input'),result=el('div');
  file.type='file';file.accept='.zip,application/zip';fileLabel.append(file);input.placeholder='未选择 ZIP 时，使用此目录';label.append(input);
  const current=()=>modal.alive()&&account===(window.WechatAccountContext?.id||'')&&epoch===state.epoch;
  const invalidate=()=>result.replaceChildren();file.addEventListener('change',invalidate);input.addEventListener('input',invalidate);
  const prepare=button('检查工具',async()=>{
   prepare.disabled=true;file.disabled=true;input.disabled=true;result.replaceChildren();try{
    if(!current())return;const selected=file.files[0];
    if(selected&&selected.size>64*1024*1024)throw new Error('ZIP 超过 64 MiB，请将模型与运行环境作为独立资源安装。');
    const p=selected?await api('/api/plugin-packages/import',{method:'POST',headers:{'Content-Type':'application/zip'},body:selected}):await api('/api/plugin-packages/prepare',{method:'POST',json:{directory:input.value.trim()}});
    if(!current())return;result.append(el('h3',p.package.name),el('p','版本 '+p.package.version,'hint'));
    if(p.package.status!=='ready'){result.append(el('p','此包尚未完成迁移，不能安装。'));return}
    const destinations=(p.package.http_routes||[]).filter(r=>r.credential).map(r=>r.endpoint||('管理员配置的 HTTPS 接口：'+r.endpoint_config));
    if(destinations.length){result.append(el('p','凭据仅发送到以下声明接口：','hint'));const targets=el('ul');for(const endpoint of [...new Set(destinations)]){const item=el('li',endpoint);item.style.overflowWrap='anywhere';targets.append(item)}result.append(targets)}
    const permissions=[];for(const permission of p.package.permissions||[]){const row=el('label',undefined,'switch-row'),check=el('input');check.type='checkbox';check.checked=true;permissions.push([permission,check]);row.append(el('span',names[permission]||permission),check);result.append(row)}
    const confirm=button('确认安装',async()=>{confirm.disabled=true;try{if(!current())return;await api('/api/plugin-packages/install',{method:'POST',json:{token:p.token,digest:p.digest,permissions:permissions.filter(([,c])=>c.checked).map(([name])=>name),confirm:true}});if(!current())return;modal.close();await load();await choose(p.package.id)}catch(e){if(current()){result.append(el('p',e.message,'error'));confirm.disabled=false}}});confirm.className='primary';result.append(el('p','安装后保持未启用；升级将保留兼容的配置和密钥，配置不兼容时保留原安装并要求迁移。权限以本次确认为准。','hint'),confirm);
   }catch(e){if(current())result.append(el('p',e.message,'error'))}finally{prepare.disabled=false;file.disabled=false;input.disabled=false}
  });modal.body.append(fileLabel,label,prepare,result);
 }
 const returnButton=button('返回工具目录',()=>{if(window.WechatAdmin.pluginDirty&&!confirm('放弃未保存的连接设置？'))return;window.WechatPluginDeclarations.reset();showPackages();load()});
 legacy.querySelector('.plugin-template-head').append(returnButton);
 window.WechatRefresh.bind($('#plugins-refresh'),load);
 window.addEventListener('wechat-admin:navigate',e=>{if(e.detail?.section==='plugins')load()});
 window.WechatPluginPackages={
  async open(id,section){
   if(!window.WechatAdmin?.isAuthenticated())return;showPackages();const owner=account(),epoch=state.epoch;
   await load();if(owner!==account()||epoch!==state.epoch)return;
   if(id===state.selected&&state.detail){const scope=context(),next=await api('/api/plugin-packages/'+encodeURIComponent(id));if(current(scope)){state.detail=next;if(!state.dirty)renderDetail(next)}}else await choose(id);
   if(section&&owner===account()){const scope=context();requestAnimationFrame(()=>{if(current(scope))document.getElementById('plugin-section-'+section)?.scrollIntoView({block:'start',behavior:'auto'})})}
  },
 };
 window.addEventListener('wechat-admin:logout',()=>{state.epoch++;state.loaded=false;state.loadPromise=null;state.busy=false;state.selected='';state.detail=null;state.catalog=[];state.installed=[];state.builtins=[];state.connections=[];state.runtimes={};state.readiness={};state.retained.clear();state.hostRequest=null;dirty(false);list.replaceChildren();editor.replaceChildren(status);status.textContent='选择服务查看配置、权限和任务。'});
 showPackages();
})();
