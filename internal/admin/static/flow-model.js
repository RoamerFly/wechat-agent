/* Diagram-only model. Node metadata never grants capabilities or executes code. */
(function(root,factory){const api=factory();if(typeof module==='object'&&module.exports)module.exports=api;else root.WechatFlowModel=api})(typeof window==='object'?window:globalThis,function(){
 'use strict';
 const W=208,H=84;
 const definitions=[
  ['receive','微信接入','接收','blue','account',['wechat'],'微信账号与消息入口'],
  ['inbox','接收与去重','接收','blue','logs',['logs','tools'],'接收落盘、游标与重启恢复'],
  ['rules','会话与权限','接收','blue','chat',['rules','catalog'],'群聊、好友与触发方式'],
  ['quote','引用与正文','理解','blue','chat',['rules'],'关联原消息与真实素材'],
  ['aggregate','聚合与补充','理解','teal','chat',['agent'],'紧邻说明与执行中补充'],
  ['observe','媒体理解','理解','teal','tools',['permissions','provider'],'图片、表情、语音、视频与分享'],
  ['memory','上下文与记忆','理解','teal','memory',['memory','agent'],'摘要、关系、素材与文档'],
  ['persona','角色与关系','决策','violet','whale',['companion'],'人设、表达与相处方式'],
  ['model','模型与语音','决策','violet','tools',['provider'],'服务商、语言和语音'],
  ['agent','Agent决策','决策','violet','whale',['agent','companion'],'回复、工具、等待或安静'],
  ['tools','工具与工具','行动','amber','tools',['plugins','mcp','mobile','permissions'],'联网、MCP、媒体与本机能力'],
  ['feedback','工具结果反馈','行动','amber','tools',['logs','agent'],'成功、失败和下一步'],
  ['tasks','异步任务续办','行动','amber','logs',['agent','provider'],'后台进程、音乐生成与提醒'],
  ['plan','回复计划','交付','violet','chat',['agent','logs'],'组织内容并复核未发部分'],
  ['send','动态排队发送','交付','blue','chat',['tools','permissions'],'文字、图片、语音、视频、文件'],
  ['receipt','回执核对','交付','teal','logs',['logs'],'已接受、未确认与观察记录'],
  ['retry','恢复与补发','交付','rose','logs',['logs','tools'],'核对证据后继续交付'],
  ['archive','投递记忆','交付','teal','memory',['memory'],'记录实际送达与素材来源'],
  ['interrupt','中断与变更','行动','rose','chat',['agent','logs'],'叫停、替换和未发内容复核'],
  ['silent','选择安静','自主','slate','whale',['companion'],'此时不发言，也能安排后续'],
  ['wake','自主复评','自主','violet','whale',['permissions','agent'],'持久计划与后续重新判断'],
  ['surf','网上冲浪','自主','amber','tools',['surf','mcp'],'多源探索、核实和分享'],
  ['permissions','能力权限','系统','slate','account',['permissions','agent'],'媒体、主动行为与主人权限'],
  ['status','运行与费用','系统','slate','dashboard',['overview'],'微信连接、运行与消耗'],
  ['note','说明便签','自定义','slate','memory',[],'记录想法、注意事项或待办'],
  ['condition','条件规划','自定义','violet','tools',[],'标注一个计划中的判断分支'],
  ['action','行动规划','自定义','amber','tools',[],'记录想扩展的功能与行动']
 ];
 const catalog=Object.fromEntries(definitions.map(([kind,title,group,tone,icon,pages,summary])=>[kind,{kind,title,group,tone,icon,pages:[...pages],summary}]));
 // Do not briefly expose retired device controls before the authenticated
 // runtime status has loaded. A non-retired server restores this entry.
 let deviceControlRetired=true;
 function setDeviceControlRetired(value){
  deviceControlRetired=value===true;
  const pages=['plugins','mcp','permissions'];
  if(!deviceControlRetired)pages.splice(2,0,'mobile');
  catalog.tools.pages=pages;
  catalog.tools.summary=deviceControlRetired?'联网、MCP、媒体与本机工具；设备记录已归档':'联网、MCP、媒体与本机能力';
  return deviceControlRetired;
 }
 setDeviceControlRetired(true);
 const tones=['blue','teal','violet','amber','rose','slate'];
 const clone=x=>JSON.parse(JSON.stringify(x));
 const bytes=s=>new TextEncoder().encode(s).length;
 const idOK=s=>typeof s==='string'&&/^[a-z][a-z0-9_-]{0,63}$/.test(s);
 const textOK=(s,max)=>typeof s==='string'&&!s.includes('\0')&&bytes(s)<=max;
 function exactKeys(obj,keys){if(!obj||typeof obj!=='object'||Array.isArray(obj)||Object.keys(obj).some(k=>!keys.includes(k)))throw Error('画布包含不支持的字段');}
 function normalize(input){
  exactKeys(input,['version','name','nodes','edges']);
  if(input.version!==1||!textOK(input.name,120)||!input.name.trim()||!Array.isArray(input.nodes)||!Array.isArray(input.edges))throw Error('画布版本、名称或节点列表无效');
  if(input.nodes.length>200||input.edges.length>600)throw Error('单画布最多容纳200个节点和600条连线，请拆分设计');
  const seen=new Set(),edges=new Set();
  const nodes=input.nodes.map(n=>{exactKeys(n,['id','kind','title','note','tone','x','y']);
   if(!idOK(n.id)||seen.has(n.id)||!Object.hasOwn(catalog,n.kind)||!textOK(n.title,120)||!n.title.trim()||!textOK(n.note??'',8192)||!tones.includes(n.tone)||![n.x,n.y].every(v=>typeof v==='number'&&Number.isFinite(v)&&Math.abs(v)<=100000))throw Error('节点名称、说明、类型或位置无效，或编号重复');
   seen.add(n.id);return {id:n.id,kind:n.kind,title:n.title,note:n.note??'',tone:n.tone,x:n.x,y:n.y};
  });
  const links=input.edges.map(e=>{exactKeys(e,['id','from','to','label']);if(!idOK(e.id)||edges.has(e.id)||!seen.has(e.from)||!seen.has(e.to)||!textOK(e.label??'',240))throw Error('连线端点、编号或标签无效');edges.add(e.id);return {id:e.id,from:e.from,to:e.to,label:e.label??''};});
  const result={version:1,name:input.name,nodes,edges:links};if(bytes(JSON.stringify(result))>120*1024)throw Error('画布超过120KiB，请精简说明或拆分设计');return result;
 }
 function parse(text){return normalize(JSON.parse(text.replace(/^\uFEFF/,'')));}
 function uid(prefix='node'){return prefix+'-'+(globalThis.crypto?.randomUUID?.()||Date.now().toString(36)+'-'+Math.random().toString(36).slice(2));}
 function removeNode(doc,id){const d=clone(doc);d.nodes=d.nodes.filter(n=>n.id!==id);d.edges=d.edges.filter(e=>e.from!==id&&e.to!==id);return d;}
 function coverage(doc){const present=new Set(doc.nodes.map(n=>n.kind)),all=definitions.filter(d=>d[1]&&d[2]!=='自定义');return {present:all.filter(d=>present.has(d[0])).length,total:all.length,missing:all.filter(d=>!present.has(d[0])).map(d=>d[1])};}
 function bounds(nodes){if(!nodes.length)return {left:0,top:0,width:800,height:500};const x=nodes.map(n=>n.x),y=nodes.map(n=>n.y),left=Math.min(...x)-100,top=Math.min(...y)-100;return {left,top,width:Math.max(...x)+W-left+100,height:Math.max(...y)+H-top+100};}
 function arrange(doc,template,mode='chart'){
  const d=clone(doc),positions=new Map(template.nodes.map(n=>[n.kind,n])),used=new Set();let extra=0;
  const spine=['receive','inbox','rules','quote','aggregate','observe','memory','agent','plan','send','receipt','archive'];
  const rows=[['status','receive'],['permissions','inbox'],['interrupt','rules'],['quote'],['aggregate'],['observe'],['persona','memory','model'],['surf','agent'],['tools','plan','silent'],['feedback','send','wake','tasks'],['receipt'],['retry','archive']];
  const tree=new Map();for(let row=0;row<rows.length;row++){let side=0;for(const kind of rows[row]){const x=spine.includes(kind)?600:[300,900,0,1200][side++];tree.set(kind,{x,y:row*144})}}
  for(const n of d.nodes){const p=mode==='tree'?tree.get(n.kind):positions.get(n.kind);if(p&&!used.has(n.kind)){n.x=p.x;n.y=p.y;used.add(n.kind)}else{n.x=mode==='tree'?300+(extra%4)*248:(extra%7)*248;n.y=(mode==='tree'?1800:1060)+Math.floor(extra/(mode==='tree'?4:7))*144;extra++}}
  return d;
 }
 function edgePath(a,b){const x1=a.x+W,y1=a.y+H/2,x2=b.x,y2=b.y+H/2;
  if(x2>=x1+12){const bend=Math.max(35,(x2-x1)*.46);return `M${x1},${y1} C${x1+bend},${y1} ${x2-bend},${y2} ${x2},${y2}`;}
  const rail=Math.max(a.y,b.y)+H+34;
  return `M${x1},${y1} C${x1+40},${y1} ${x1+40},${rail} ${x1},${rail} L${x2-32},${rail} Q${x2-54},${rail} ${x2-54},${rail-22} L${x2-54},${y2+22} Q${x2-54},${y2} ${x2},${y2}`;
 }

 function edgeRole(e){if(['permissions','persona','model'].includes(e.from))return 'support';if(['feedback','retry','wake','tasks'].includes(e.from)||e.from==='interrupt')return 'return';return 'main';}
 function routeEdges(doc){
  const padding=22,points=new Map(),paths=new Map(),byID=new Map(doc.nodes.map(n=>[n.id,n]));
  const xs=new Set(),ys=new Set();for(const n of doc.nodes){for(const x of [n.x-padding,n.x+W/2,n.x+W+padding])xs.add(x);for(const y of [n.y-padding,n.y+H/2,n.y+H+padding])ys.add(y)}
  if(!doc.nodes.length)return {paths,points,degraded:false};
  xs.add(Math.min(...xs)-64);xs.add(Math.max(...xs)+64);ys.add(Math.min(...ys)-64);ys.add(Math.max(...ys)+64);
  const xx=[...xs].sort((a,b)=>a-b),yy=[...ys].sort((a,b)=>a-b),width=xx.length,size=width*yy.length;
  if(size>25000){for(const e of doc.edges)paths.set(e.id,edgePath(byID.get(e.from),byID.get(e.to)));return {paths,points,degraded:true};}
  const blocked=new Uint8Array(size),xIndex=new Map(xx.map((x,i)=>[x,i])),yIndex=new Map(yy.map((y,i)=>[y,i]));
  for(let j=0;j<yy.length;j++)for(let i=0;i<xx.length;i++){const x=xx[i],y=yy[j];if(doc.nodes.some(n=>x>n.x-8&&x<n.x+W+8&&y>n.y-8&&y<n.y+H+8))blocked[j*width+i]=1;}
  const ports=(n,face)=>face==='east'?{anchor:{x:n.x+W,y:n.y+H/2},stub:{x:n.x+W+padding,y:n.y+H/2}}:face==='west'?{anchor:{x:n.x,y:n.y+H/2},stub:{x:n.x-padding,y:n.y+H/2}}:face==='south'?{anchor:{x:n.x+W/2,y:n.y+H},stub:{x:n.x+W/2,y:n.y+H+padding}}:{anchor:{x:n.x+W/2,y:n.y},stub:{x:n.x+W/2,y:n.y-padding}};
  function search(start,end){
   const startPoint=yIndex.get(start.y)*width+xIndex.get(start.x),endPoint=yIndex.get(end.y)*width+xIndex.get(end.x);if(blocked[startPoint]||blocked[endPoint])return null;
   const heap=[],best=new Map(),parent=new Map();
   const push=item=>{heap.push(item);let i=heap.length-1;while(i>0){const p=(i-1)>>1;if(heap[p].score<=item.score)break;heap[i]=heap[p];i=p}heap[i]=item};
   const pop=()=>{const first=heap[0],tail=heap.pop();if(heap.length){let i=0;while(i*2+1<heap.length){let child=i*2+1;if(child+1<heap.length&&heap[child+1].score<heap[child].score)child++;if(heap[child].score>=tail.score)break;heap[i]=heap[child];i=child}heap[i]=tail}return first};
   const initial=startPoint*3;best.set(initial,0);push({state:initial,cost:0,score:Math.abs(start.x-end.x)+Math.abs(start.y-end.y)});let last;
   while(heap.length){const item=pop();if(best.get(item.state)!==item.cost)continue;const cell=Math.floor(item.state/3),dir=item.state%3;if(cell===endPoint){last=item.state;break}const i=cell%width,j=Math.floor(cell/width);
    for(const [di,dj,nextDir]of [[-1,0,1],[1,0,1],[0,-1,2],[0,1,2]]){const ni=i+di,nj=j+dj;if(ni<0||ni>=width||nj<0||nj>=yy.length)continue;const nextCell=nj*width+ni;if(blocked[nextCell])continue;const cost=item.cost+Math.abs(xx[ni]-xx[i])+Math.abs(yy[nj]-yy[j])+(dir&&dir!==nextDir?28:0),state=nextCell*3+nextDir;if(cost>=(best.get(state)??Infinity))continue;best.set(state,cost);parent.set(state,item.state);push({state,cost,score:cost+Math.abs(xx[ni]-end.x)+Math.abs(yy[nj]-end.y)})}
   }
   if(last===undefined)return null;const result=[];while(last!==undefined){const cell=Math.floor(last/3);result.push({x:xx[cell%width],y:yy[Math.floor(cell/width)]});last=parent.get(last)}return result.reverse();
  }
  function simplify(values){const result=[];for(const p of values){if(result.length&&p.x===result.at(-1).x&&p.y===result.at(-1).y)continue;while(result.length>=2){const a=result.at(-2),b=result.at(-1);if((a.x===b.x&&b.x===p.x)||(a.y===b.y&&b.y===p.y))result.pop();else break}result.push(p)}return result;}
  function rounded(values){let d=`M${values[0].x},${values[0].y}`;for(let i=1;i<values.length-1;i++){const a=values[i-1],b=values[i],c=values[i+1],before=Math.hypot(b.x-a.x,b.y-a.y),after=Math.hypot(c.x-b.x,c.y-b.y),radius=Math.min(9,before/2,after/2);d+=` L${b.x+(a.x-b.x)*radius/before},${b.y+(a.y-b.y)*radius/before} Q${b.x},${b.y} ${b.x+(c.x-b.x)*radius/after},${b.y+(c.y-b.y)*radius/after}`}const end=values.at(-1);return d+` L${end.x},${end.y}`;}
  for(const e of doc.edges){const a=byID.get(e.from),b=byID.get(e.to);if(!a||!b)continue;const dx=b.x-a.x,dy=b.y-a.y;let from,to;if(a===b){from='east';to='north'}else if(Math.abs(dx)>=Math.abs(dy)){from=dx>=0?'east':'west';to=dx>=0?'west':'east'}else{from=dy>=0?'south':'north';to=dy>=0?'north':'south'}const s=ports(a,from),t=ports(b,to),route=search(s.stub,t.stub);if(route){const all=simplify([s.anchor,...route,t.anchor]);paths.set(e.id,rounded(all));points.set(e.id,all)}else paths.set(e.id,edgePath(a,b));}
  return {paths,points,degraded:false};
 }
 return {W,H,catalog,tones,clone,normalize,parse,uid,removeNode,coverage,bounds,arrange,edgePath,routeEdges,edgeRole,setDeviceControlRetired,isDeviceControlRetired:()=>deviceControlRetired};
});
