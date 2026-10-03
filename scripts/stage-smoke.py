#!/usr/bin/env python3
"""Verify the isolated stage through HTTPS and MCP. Credentials never leave this host's test account."""
import argparse,json,pathlib,time,uuid
import requests
p=argparse.ArgumentParser();p.add_argument('--credentials',required=True);p.add_argument('--output',required=True);args=p.parse_args()
base='https://toolyard.stage.dev.beknown.live'
cred=json.loads(pathlib.Path(args.credentials).read_text())
s=requests.Session();s.headers.update({'Origin':base,'X-Requested-With':'XMLHttpRequest'})
r=s.post(base+'/v1/auth/login',json={'username':cred['username'],'password':cred['password']});r.raise_for_status()
assert r.cookies and 'Secure' in r.headers['Set-Cookie'] and 'HttpOnly' in r.headers['Set-Cookie']
a=requests.Session();a.headers.update({'Authorization':'Bearer '+cred['agent_token'],'Accept':'application/json, text/event-stream','MCP-Protocol-Version':'2025-03-26'})
def call(name,arguments):
 r=a.post(base+'/mcp',json={'jsonrpc':'2.0','id':str(uuid.uuid4()),'method':'tools/call','params':{'name':name,'arguments':{'_reason':'Verify the isolated Toolyard stage with synthetic data',**arguments}}},timeout=15);r.raise_for_status()
 data=r.json() if r.headers.get('content-type','').startswith('application/json') else json.loads(next(x[6:] for x in r.text.splitlines() if x.startswith('data: ')))
 assert 'result' in data,data
 return data['result']
def content(res):
 if 'structuredContent' in res:return res['structuredContent']
 return json.loads(next(x['text'] for x in res['content'] if x['type']=='text'))
results=[]
health=s.get(base+'/v1/health').json();assert health['environment']=='stage';results.append('HTTPS, stage metadata, and secure login cookie')
assert requests.get(base+'/v1/inbox').status_code==401;results.append('Unauthenticated inbox refused')
invalid=s.post(base+'/v1/inbox/missing/decide',json={'action':'deny'},headers={'Origin':'https://wrong.example'});assert invalid.status_code==403;results.append('Cross-origin mutation refused')
servers=s.get(base+'/v1/servers').json();assert len(servers)==2 and all(x.get('last_status')=='ok' for x in servers);results.append('Two test connectors healthy')
key='verification-'+str(uuid.uuid4())
q={'schema_version':2,'client_request_id':key,'prompt':'Browser verification: choose checks','context':'A synthetic check of choices, drafts, and text. This item is separate from your sample questions.','question':{'type':'multiple_choice','min_selections':1,'max_selections':2,'options':[{'id':'mobile','label':'Mobile layout'},{'id':'keyboard','label':'Keyboard access'},{'id':'none','label':'Neither','exclusive':True}]}}
res=content(call('inbox.ask',q));assert res['ok'],res
qid=res['request_id'];assert content(call('inbox.ask',q))['request_id']==qid;results.append('MCP question and idempotent submission')
# Keep qid pending for the browser to answer.
req={'title':'Verification: update the sample note','summary':'A synthetic permission test.','message':'Update only the local welcome record to the exact approved text.','urgency':'soon','facts':{'why_now':'Verify scoped permissions.','if_it_goes_wrong':'Only synthetic data changes.','undo':'Restore the original sample text.'},'tools':[{'tool':'sandbox-files.update_record','required':True,'summary':'Change the welcome sample record.','params':{'id':'welcome','text':'Stage verification passed'}}]}
res=content(call('inbox.request',req));assert res['ok'],res;rid=res['request_id']
blocked=call('sandbox-files.update_record',{'id':'welcome','text':'Stage verification passed'});assert content(blocked).get('status')=='permission_required',blocked;results.append('Write refused without permission')
d=s.post(base+f'/v1/inbox/{rid}/decide',json={'action':'approve','allow':[True]});d.raise_for_status()
st=content(call('inbox.status',{'ids':[rid]}))
views=st if isinstance(st,list) else st.get('requests',st.get('results',[]))
assert views,st
grant=views[0]['tools'][0]['grant']
wrong=call('sandbox-files.update_record',{'id':'welcome','text':'Unapproved text','_grant':grant});assert wrong.get('isError') or content(wrong).get('status')=='permission_required';results.append('Permission refuses changed parameters')
right=call('sandbox-files.update_record',{'id':'welcome','text':'Stage verification passed','_grant':grant});assert not right.get('isError'),right
again=call('sandbox-files.update_record',{'id':'welcome','text':'Stage verification passed','_grant':grant});assert again.get('isError') or content(again).get('status')=='permission_required';results.append('Approved synthetic write succeeds once')
failed=call('sandbox-errors.get_failure',{});assert failed.get('isError');results.append('Logical tool failure remains an error')
slow=call('sandbox-errors.get_slow',{});assert not slow.get('isError');results.append('Slow connector completes')
auth=call('sandbox-errors.get_auth_required',{});assert auth.get('isError');results.append('Simulated sign-in failure remains an error')
pathlib.Path(args.output).write_text(json.dumps({'question_id':qid,'permission_id':rid,'checks':results,'version':health['version']},indent=2)+'\n')
print(json.dumps({'question_id':qid,'checks':results},indent=2))
