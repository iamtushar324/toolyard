// Rich Inbox items in the dashboard: the safe Markdown renderer, the SVG
// chart shapes, and the one-decision answer form for every question type.
const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const vm=require('node:vm');
const path=require('node:path');
const source=fs.readFileSync(path.join(__dirname,'../web/dashboard/app.js'),'utf8');
const settings=fs.readFileSync(path.join(__dirname,'../web/dashboard/settings.js'),'utf8');

function harness(storage={}) {
  const context=vm.createContext({URLSearchParams,console,crypto:require('node:crypto').webcrypto,window:{location:{origin:'https://stage.example'},addEventListener(){}},document:{addEventListener(){},getElementById:()=>null,querySelector:()=>null},navigator:{},sessionStorage:storage,localStorage:{getItem:()=>null},setTimeout:()=>0,clearTimeout(){},setInterval:()=>0});
  Object.defineProperties(storage,{getItem:{value:key=>storage[key]||null,configurable:true},setItem:{value:(key,value)=>{storage[key]=value;},configurable:true},removeItem:{value:key=>{delete storage[key];},configurable:true}});
  vm.runInContext(settings,context);
  vm.runInContext(source.slice(0,source.lastIndexOf('(async () => {')),context);
  vm.runInContext(`el=(tag,attrs={},...children)=>({tag,attrs,children:children.flat(Infinity).filter(x=>x!=null&&x!==false)});render=()=>{};ibRegion=()=>{};toasts=[];toast=(m)=>{toasts.push(m);};staleNote=()=>{};loadInboxDetail=async()=>{};loadInbox=()=>{};state.user={id:'owner',role:'admin'};state.inbox.detail={grants:[]};`,context);
  return {context,storage,run:code=>vm.runInContext(code,context)};
}
function text(n){return typeof n==='object'&&n?(n.children||[]).map(text).join(' '):String(n==null?'':n);}
function nodes(n,tag){if(Array.isArray(n))return n.flatMap(c=>nodes(c,tag));return typeof n==='object'&&n?[...(n.tag===tag?[n]:[]),...(n.children||[]).flatMap(c=>nodes(c,tag))]:[];}
function plain(x){return JSON.parse(JSON.stringify(x));}
function md(h,src,req){h.context.src=src;h.context.req=req||{};return h.run('ibMarkdown(src, ibMdContext(req))');}
const SHA='a'.repeat(64), BLOB='b'.repeat(64);

test('Markdown never turns text into HTML',()=>{
  const h=harness();
  const tree=md(h,'<script>alert(1)</script>\n\n<img src=x onerror="alert(2)"> and <b>bold</b>\n\n&lt;i&gt;entity&lt;/i&gt; &amp; &#60;tag&#62;');
  for(const tag of ['script','img','b','i'])assert.equal(nodes(tree,tag).length,0,tag+' must not be created');
  const all=text({children:tree});
  assert.match(all,/<script>alert\(1\)<\/script>/);
  assert.match(all,/onerror="alert\(2\)"/);
  assert.match(all,/<i>entity<\/i> & <tag>/);
  for(const n of nodes(tree,'p'))assert.deepEqual(Object.keys(n.attrs),[]);
});

test('Markdown keeps only http, https and mailto links',()=>{
  const h=harness();
  const tree=md(h,['[a](javascript:alert(1))','[b](data:text/html;base64,PHNjcmlwdD4=)','[c](&#106;avascript:alert(1))','[d](//evil.example/x)','[e](vbscript:msgbox)','<javascript:alert(1)>','[f](java\tscript:alert(1))','[g](https://example.com/a_(b))','<mailto:owner@example.com>','see https://ci.example.com/run/42.','[h](http://example.com "Title")'].join('\n\n'));
  const links=nodes(tree,'a');
  assert.deepEqual(plain(links.map(a=>a.attrs.href)),['https://example.com/a_(b)','mailto:owner@example.com','https://ci.example.com/run/42','http://example.com']);
  for(const a of links){assert.match(a.attrs.rel,/noopener/);assert.match(a.attrs.rel,/noreferrer/);}
  assert.equal(links[0].attrs.target,'_blank');
  assert.equal(links[3].attrs.title,'Title');
  assert.match(text({children:tree}),/\ba\b.*\bb\b.*\bc\b/s);
});

test('Markdown images show only the item\'s own uploads',()=>{
  const h=harness();
  const req={inline_images:[{ref:'upload:'+SHA,blob:BLOB}],attachments:[{type:'image',url:'https://cdn.example/shot.png',blob:'c'.repeat(64)}]};
  const tree=md(h,`![own](upload:${SHA}) ![copied](https://cdn.example/shot.png) ![remote](https://evil.example/x.png) ![bad](javascript:alert(1)) ![other](upload:${'d'.repeat(64)})`,req);
  const imgs=nodes(tree,'img');
  assert.deepEqual(plain(imgs.map(i=>i.attrs.src)),['/v1/inbox/blobs/'+BLOB,'/v1/inbox/blobs/'+'c'.repeat(64)]);
  assert.equal(imgs[0].attrs.alt,'own');
  assert.deepEqual(plain(nodes(tree,'a').map(a=>a.attrs.href)),['https://evil.example/x.png']);
  assert.match(text({children:tree}),/bad/);
});

test('Markdown renders GitHub tables, code, lists, quotes and emphasis',()=>{
  const h=harness();
  const tree=md(h,['## Plan','','| check | result |','|:--|--:|','| unit | **212** passed |','| e2e | `18` \\| ok |','','```js',"const x = '<b>';",'```','','- [x] migrate','- [ ] deploy','  - nested','','1. first','2. second','','> quoted *note*','','**bold**, *em*, ~~gone~~, snake_case_name and a\\*literal\\*.','','---'].join('\n'));
  assert.equal(nodes(tree,'h4').length,1);
  const table=nodes(tree,'table')[0];
  assert.ok(table);
  assert.deepEqual(nodes(table,'th').map(text),['check','result']);
  assert.equal(nodes(table,'th')[0].attrs.align,'left');
  assert.equal(nodes(table,'td')[1].attrs.align,'right');
  assert.equal(nodes(nodes(table,'td')[1],'strong').length,1);
  assert.match(text(nodes(table,'td')[3]),/18 .*\| ok/);
  const code=nodes(tree,'pre')[0];
  assert.equal(nodes(code,'code')[0].attrs.class,'language-js');
  assert.equal(text(nodes(code,'code')[0]).trim(),"const x = '<b>';");
  const tasks=nodes(tree,'li').filter(li=>li.attrs.class==='ib-task');
  assert.equal(tasks.length,2);
  assert.match(text(tasks[0]),/☑/);
  assert.equal(nodes(tree,'ol').length,1);
  assert.equal(nodes(nodes(tree,'ol')[0],'li').length,2);
  assert.equal(nodes(nodes(tree,'blockquote')[0],'em').length,1);
  const last=nodes(tree,'p').at(-1);
  assert.deepEqual(['strong','em','del'].map(t=>nodes(last,t).length),[1,1,1]);
  assert.match(text(last),/snake_case_name/);
  assert.match(text(last),/\*\s*literal\s*\*/);
  assert.equal(nodes(tree,'hr').length,1);
});

test('charts: every kind draws its own shapes',()=>{
  const h=harness();
  const spec=a=>{h.context.a=a;return h.run('ibChartSpec(a)');};
  const pie=spec({chart:'pie',title:'Errors',x:['api','web','jobs'],series:[{name:'errors',values:[6,3,1]}]});
  assert.equal(nodes(pie,'path').filter(p=>/slice/.test(p.attrs.class)).length,3);
  assert.match(pie.attrs['aria-label'],/Errors \(pie chart\)/);
  h.context.a={chart:'pie',x:['api','web','jobs'],series:[{values:[6,3,1]}]};
  assert.deepEqual(plain(h.run('ibChartLegend(a)')).map(l=>l.label),['api · 6 (60%)','web · 3 (30%)','jobs · 1 (10%)']);
  const whole=spec({chart:'pie',x:['all'],series:[{values:[5]}]});
  assert.equal(nodes(whole,'circle').length,1);
  const scatter=spec({chart:'scatter',x:[1,2.5,4],series:[{name:'p95',values:[120,180,260]},{name:'p50',values:[40,60,90]}]});
  assert.equal(nodes(scatter,'circle').length,6);
  const stacked=spec({chart:'stacked_bar',x:['Mon','Tue'],series:[{name:'ok',values:[4,5]},{name:'failed',values:[1,0]}]});
  const rects=nodes(stacked,'rect');
  assert.equal(rects.length,3);
  assert.ok(rects.every(r=>/stack/.test(r.attrs.class)));
  const area=spec({chart:'area',series:[{name:'a',values:[1,-2,3]},{name:'b',values:[2,2,2]}]});
  assert.equal(nodes(area,'path').filter(p=>/areafill/.test(p.attrs.class)).length,2);
  assert.equal(nodes(area,'path').filter(p=>/\bline\b/.test(p.attrs.class)).length,2);
  const bar=spec({chart:'bar',x:['a','b'],series:[{name:'x',values:[1,2]},{name:'y',values:[3,4]}]});
  assert.equal(nodes(bar,'rect').length,4);
  const line=spec({chart:'line',series:[{name:'rps',values:[1,2,3]}]});
  assert.equal(nodes(line,'path').length,1);
  const unknown=spec({chart:'radar',series:[{values:[1,2]}]});
  assert.equal(nodes(unknown,'path').length,1);
  const empty=spec({chart:'pie',x:['a'],series:[{values:[0]}]});
  assert.match(text(empty),/No data|^$/);
  for(const s of [pie,scatter,stacked,area,bar,line])for(const n of [...nodes(s,'path'),...nodes(s,'rect'),...nodes(s,'circle')])assert.ok(!('style' in n.attrs),'no inline styles: the CSP forbids them');
});

const everyType={id:'rq_rich',agent_name:'Agent',kind:'question',status:'pending',revision:1,expires_at:Date.now()+86400000,questions:[
  {id:'notes',type:'free_text',prompt:'Anything else?',required:false,multiline:true,max_length:50},
  {id:'env',type:'single_choice',prompt:'Where first?',required:true,options:[{id:'stage',label:'Stage'},{id:'prod',label:'Production'}],recommended:'stage',allow_other:true},
  {id:'checks',type:'multiple_choice',prompt:'Which checks?',required:true,min_selections:1,max_selections:3,options:[{id:'unit',label:'Unit'},{id:'e2e',label:'E2E'},{id:'none',label:'None',exclusive:true}]},
  {id:'notify',type:'yes_no',prompt:'Tell the team?',required:true,default:true},
  {id:'budget',type:'number',prompt:'Budget?',required:true,min:0,max:500,step:25,unit:'USD'},
  {id:'start',type:'date',prompt:'From when?',required:true},
  {id:'window',type:'datetime',prompt:'Window opens?',required:true},
  {id:'confidence',type:'scale',prompt:'Confidence?',required:true,scale:5,min_label:'Low',max_label:'High'},
  {id:'order',type:'ranking',prompt:'Order these',required:true,options:[{id:'speed',label:'Speed'},{id:'cost',label:'Cost'},{id:'safety',label:'Safety'}]},
]};

test('the answer form renders every question type with native, labelled controls',()=>{
  const h=harness();h.context.req=structuredClone(everyType);h.run('state.inbox.detail.request=req');
  const tree=h.run('ibDecideEl(req,[])');
  assert.equal(tree.tag,'form');
  const qs=nodes(tree,'div').filter(n=>n.attrs['data-question']);
  assert.deepEqual(qs.map(n=>n.attrs['data-question']),everyType.questions.map(q=>q.id));
  const inputs=nodes(tree,'input');
  const byType=t=>inputs.filter(i=>i.attrs.type===t);
  assert.equal(byType('radio').length,2+2+5);
  assert.equal(byType('checkbox').length,3);
  assert.equal(byType('number').length,1);
  assert.equal(byType('number')[0].attrs.step,'25');
  assert.equal(byType('date').length,1);
  assert.equal(byType('datetime-local').length,1);
  assert.equal(nodes(tree,'textarea').length,2);
  const yes=byType('radio').find(i=>i.attrs.id==='ibq-3-0');
  assert.equal(yes.attrs.checked,true,'the default fills the draft');
  assert.ok(nodes(tree,'button').some(b=>b.attrs['aria-label']==='Move Speed down'));
  assert.ok(nodes(tree,'legend').length>=5);
  for(const i of byType('number').concat(byType('date'),byType('datetime-local')))assert.ok(nodes(tree,'label').some(l=>l.attrs.for===i.attrs.id),'labelled '+i.attrs.id);
  assert.match(text(tree),/Suggested/);
  assert.equal(nodes(tree,'button').filter(b=>b.attrs.type==='submit').length,1);
});

test('the answer form builds one decision with every answer, typed per question',async()=>{
  const h=harness();h.context.req=structuredClone(everyType);h.run('state.inbox.detail.request=req;let sent=[];api=async(url,opts)=>{sent.push({url,body:opts.body});return {};};');
  h.run(`const d=ibAnswersDraft(req);
    d.values.env='stage'; d.values.checks=['unit','e2e']; d.values.notify=false; d.values.budget='125';
    d.values.start='2026-11-02'; d.values.window='2026-11-02T09:30'; d.values.confidence=4; d.values.order=['safety','speed','cost'];
    d.texts.env='Then production on Monday.'; ibSaveAnswersDraft(req,d);`);
  assert.equal(h.run('Object.keys(sessionStorage).filter(k=>k.startsWith("toolyard.answer.v3.")).length'),1,'the draft is kept on this device');
  const body=plain(h.run('ibAnswersBody(req)'));
  assert.deepEqual(Object.keys(body).sort(),['budget','checks','confidence','env','notify','order','start','window']);
  assert.deepEqual(body.env,{value:'stage',text:'Then production on Monday.'});
  assert.deepEqual(body.checks,{value:['unit','e2e']});
  assert.deepEqual(body.notify,{value:false});
  assert.deepEqual(body.budget,{value:125});
  assert.deepEqual(body.confidence,{value:4});
  assert.deepEqual(body.order,{value:['safety','speed','cost']});
  assert.match(body.window.value,/^2026-11-02T09:30:00(?:Z|[+-]\d\d:\d\d)$/);
  assert.deepEqual(plain(h.run('ibAnswerProblems(req)')),{});
  const form=h.run('ibDecideEl(req,[])');
  await form.attrs.on.submit({preventDefault(){}});
  const sent=plain(h.run('sent'));
  assert.equal(sent.length,1);
  assert.equal(sent[0].url,'/v1/inbox/rq_rich/decide');
  assert.equal(sent[0].body.action,'answer');
  assert.equal(sent[0].body.request_revision,1);
  assert.ok(sent[0].body.submission_id);
  assert.deepEqual(sent[0].body.answers,body);
  assert.equal(h.run('Object.keys(sessionStorage).filter(k=>k.startsWith("toolyard.answer.v3.")).length'),0,'a sent answer set drops its draft');
});

test('the answer form refuses missing and invalid answers before sending',()=>{
  const h=harness();h.context.req=structuredClone(everyType);h.run('state.inbox.detail.request=req;sent=0;api=async()=>{sent++;return {};};');
  h.run(`const d=ibAnswersDraft(req); d.values.budget='130'; d.values.checks=['none','unit']; d.values.confidence=6; d.values.order=['speed']; d.values.notes='x'.repeat(51);`);
  const problems=plain(h.run('ibAnswerProblems(req)'));
  assert.deepEqual(Object.keys(problems).sort(),['budget','checks','confidence','env','notes','order','start','window']);
  assert.match(problems.budget,/steps of 25/);
  assert.match(problems.checks,/alone/);
  assert.match(problems.order,/every option/);
  assert.equal(problems.notify,undefined,'the yes/no default counts as an answer');
  h.run('ibDecideEl(req,[]).attrs.on.submit({preventDefault(){}})');
  assert.equal(h.run('sent'),0);
  assert.ok(h.run('state.inbox.qproblems.rq_rich.env'));
  const tree=h.run('ibDecideEl(req,[])');
  assert.ok(nodes(tree,'p').some(p=>p.attrs.role==='alert'&&/Answer this question/.test(text(p))));
  h.run(`ibAnswersDraft(req).texts.env='Somewhere else';`);
  assert.equal(plain(h.run('ibAnswerProblems(req)')).env,undefined,'allow_other accepts a custom answer');
});

test('an access item sends its verdicts and answers in one decision',()=>{
  const h=harness();
  h.context.req={id:'rq_mix',agent_name:'Agent',kind:'access',status:'pending',revision:1,expires_at:Date.now()+86400000,ttl_seconds:1800,
    tools:[{call_id:'deploy',tool:'deploy.run',required:true,summary:'Deploy.',target:'api',expected_effects:'New build.',params:{env:{eq:'prod'}},flags:[]}],
    questions:[{id:'rollout',type:'single_choice',prompt:'Rollout?',required:true,options:[{id:'staged',label:'Staged'},{id:'all',label:'All at once'}]}]};
  h.run('state.inbox.detail.request=req;sent=0;api=async()=>{sent++;return {};};');
  const tree=h.run('ibDecideEl(req,[])');
  assert.ok(nodes(tree,'button').some(b=>text(b)==='Accept 1, reject 0 and send answers'));
  assert.equal(nodes(tree,'div').filter(n=>n.attrs['data-question']==='rollout').length,1);
  h.run('ibApprove(req,state.inbox.allow[req.id])');
  assert.equal(h.run('sent'),0,'a missing answer keeps the decision local');
  h.run(`ibAnswersDraft(req).values.rollout='staged'`);
  const body=plain(h.run('ibApproveBody(req,state.inbox.allow[req.id])'));
  assert.deepEqual(body.verdicts,{deploy:{verdict:'accepted'}});
  assert.deepEqual(body.answers,{rollout:{value:'staged'}});
  const first=body.submission_id;
  h.run(`ibAnswersDraft(req).values.rollout='all'`);
  assert.notEqual(plain(h.run('ibApproveBody(req,state.inbox.allow[req.id])')).submission_id,first,'changed answers get a new submission ID');
});

test('a decided item lists its answers',()=>{
  const h=harness();
  h.context.req=Object.assign(structuredClone(everyType),{status:'answered',answers:{env:{value:'prod',labels:['Production']},notify:{value:false,labels:['No']},budget:{value:125},order:{value:['cost','speed','safety']}}});
  const tree=h.run('ibDecideEl(req,[])');
  const all=text(tree);
  assert.match(all,/Answers saved/);
  assert.match(all,/Production/);
  assert.match(all,/\bNo\b/);
  assert.match(all,/125 USD/);
  assert.match(all,/Cost → Speed → Safety/);
  assert.match(all,/Not answered/);
});

test('links with a user name or password are not links',()=>{
  const h=harness();
  const tree=md(h,'[bank](https://bank.example@evil.example/login) and https://user:pw@evil.example/x and <https://a@b.example>');
  assert.equal(nodes(tree,'a').length,0);
  assert.match(text({children:tree}),/bank/);
});

test('pathological Markdown finishes quickly',()=>{
  const h=harness();
  const inputs=['['.repeat(20000),'*a '.repeat(7000),'`'.repeat(20000),'`a``'.repeat(5000),'![x]('.repeat(3500),'_a '.repeat(7000),
    '~~a '.repeat(5000),'[a]('.repeat(5000),'<'.repeat(20000),'http://x'.repeat(2500),'> '.repeat(10000),'- '.repeat(10000),
    '|a'.repeat(10000)+'\n|-|\n','**'.repeat(10000),'[`'.repeat(10000),'1. '.repeat(7000)];
  for(const src of inputs){
    const t0=Date.now();
    md(h,src);
    const ms=Date.now()-t0;
    assert.ok(ms<1500,`${JSON.stringify(src.slice(0,12))}… took ${ms} ms`);
  }
});

test('answer drafts are maps without a prototype',()=>{
  const h=harness();h.context.req=structuredClone(everyType);
  assert.equal(h.run('Object.getPrototypeOf(ibAnswersDraft(req).values)'),null);
  assert.equal(h.run('Object.getPrototypeOf(ibAnswersDraft(req).texts)'),null);
  assert.equal(h.run('Object.getPrototypeOf(ibAnswersBody(req))'),null);
  // A draft read back from storage is converted too.
  h.run('ibSaveAnswersDraft(req,ibAnswersDraft(req));ibAnswerDrafts.clear()');
  assert.equal(h.run('Object.getPrototypeOf(ibAnswersDraft(req).values)'),null);
  assert.equal(h.run('ibAnswersDraft(req).values.notify'),true);
});

test('rejecting every call does not require the answers',()=>{
  const h=harness();
  h.context.req={id:'rq_rej',agent_name:'Agent',kind:'access',status:'pending',revision:1,expires_at:Date.now()+86400000,ttl_seconds:1800,
    tools:[{call_id:'deploy',tool:'deploy.run',required:true,summary:'Deploy.',target:'api',expected_effects:'New build.',params:{env:{eq:'prod'}},flags:[]}],
    questions:[{id:'rollout',type:'single_choice',prompt:'Rollout?',required:true,options:[{id:'staged',label:'Staged'},{id:'all',label:'All at once'}]}]};
  h.run('state.inbox.detail.request=req;sent=[];api=async(url,opts)=>{sent.push(opts.body);return {};};');
  h.run('ibDecideEl(req,[]);ibSelectCalls(req,[false]);ibApprove(req,[false])');
  const sent=plain(h.run('sent'));
  assert.equal(sent.length,1);
  assert.deepEqual(sent[0].verdicts,{deploy:{verdict:'rejected'}});
  assert.deepEqual(sent[0].answers,{});
});
