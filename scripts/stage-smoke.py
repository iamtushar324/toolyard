#!/usr/bin/env python3
"""Verify Toolyard through HTTPS/MCP using pre-existing synthetic fixtures.

This script does not seed a database or create credentials. --base-url must be
an explicitly authorized deployment with the sandbox-files/sandbox-errors tools.
Secrets and grant tokens stay in memory and never enter the output artifact.
"""
import argparse
import json
import pathlib
import urllib.parse
import uuid

import requests

DEFAULT_BASE = 'https://toolyard.stage.dev.beknown.live'


def access_request(calls, title='Verify synthetic Inbox decisions'):
    """Create useful permission context for each exact synthetic fixture call."""
    return {
        'title': title, 'summary': 'Verify a batch against synthetic fixture data.',
        'message': 'I request these exact synthetic updates to verify Inbox decisions and scoped grant execution.',
        'task': {'objective': 'Validate complete decisions and single-use permissions without external side effects.'},
        'urgency': 'soon', 'pending_ttl_seconds': 86400, 'ttl_seconds': 1800,
        'facts': {'why_now': 'Validate the deployed Inbox integration before human feedback.',
                  'if_it_goes_wrong': 'Only the synthetic welcome text changes.',
                  'undo': 'Restore the captured original welcome text with a separately approved fixture call.'},
        'tools': [{
            'call_id': call_id, 'tool': 'sandbox-files.update_record', 'required': True,
            'summary': 'Set the synthetic welcome text to verify this specific batch outcome.',
            'target': 'The synthetic welcome record', 'operation': 'write',
            'expected_effects': 'The local welcome text becomes the exact requested value.',
            'affected_scope': 'One synthetic record in the sandbox-files connector.',
            'material_risks': 'The previous test text is replaced until the restoration call succeeds.',
            'undo': 'Use a new approved update_record call with the captured original text.',
            'params': {'id': {'eq': 'welcome'}, 'text': {'eq': text}},
        } for call_id, text in calls],
    }


def decision_body(revision, verdicts, note):
    return {'action': 'submit', 'request_revision': revision,
            'submission_id': str(uuid.uuid4()), 'verdicts': verdicts, 'note': note}


def content(result):
    if 'structuredContent' in result:
        return result['structuredContent']
    return json.loads(next(item['text'] for item in result['content'] if item['type'] == 'text'))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--credentials', required=True)
    parser.add_argument('--output', required=True)
    parser.add_argument('--base-url', default=DEFAULT_BASE)
    args = parser.parse_args()
    base = args.base_url.rstrip('/')
    parsed = urllib.parse.urlsplit(base)
    if parsed.scheme != 'https' or not parsed.hostname or parsed.username or parsed.password or parsed.query or parsed.fragment or parsed.path:
        parser.error('--base-url must be an HTTPS instance origin without credentials or a path')
    credential = json.loads(pathlib.Path(args.credentials).read_text())
    owner = requests.Session()
    owner.headers.update({'Origin': base, 'X-Requested-With': 'XMLHttpRequest'})
    agent = requests.Session()
    agent.headers.update({'Authorization': 'Bearer ' + credential['agent_token'],
                          'Accept': 'application/json, text/event-stream', 'MCP-Protocol-Version': '2025-03-26'})

    def request(session, method, path, **kwargs):
        response = session.request(method, base + path, timeout=15, allow_redirects=False, **kwargs)
        assert not response.is_redirect, 'Instance redirects are not allowed with test credentials'
        return response

    def call(name, arguments):
        response = request(agent, 'POST', '/mcp', json={
            'jsonrpc': '2.0', 'id': str(uuid.uuid4()), 'method': 'tools/call',
            'params': {'name': name, 'arguments': {'_reason': 'Verify Toolyard using only synthetic fixture data', **arguments}}})
        response.raise_for_status()
        payload = response.json() if response.headers.get('content-type', '').startswith('application/json') else json.loads(next(line[6:] for line in response.text.splitlines() if line.startswith('data: ')))
        assert 'result' in payload, 'MCP returned a protocol error'
        return payload['result']

    def status(inbox_id):
        value = content(call('inbox.status', {'ids': [inbox_id]}))
        views = value if isinstance(value, list) else value.get('requests', value.get('results', []))
        assert len(views) == 1, 'Inbox status must return exactly one owned request'
        return views[0]

    def detail(inbox_id):
        response = request(owner, 'GET', '/v1/inbox/' + inbox_id)
        response.raise_for_status()
        return response.json()

    def submit(calls, title='Verify synthetic Inbox decisions'):
        result = content(call('inbox.request', access_request(calls, title)))
        assert result.get('ok'), 'Synthetic Inbox request failed validation'
        return result['request_id']

    def decide(inbox_id, verdicts, note):
        body = decision_body(detail(inbox_id)['request']['revision'], verdicts, note)
        response = request(owner, 'POST', '/v1/inbox/' + inbox_id + '/decide', json=body)
        response.raise_for_status()
        # A matching retry cannot create another grant or change the revision.
        response = request(owner, 'POST', '/v1/inbox/' + inbox_id + '/decide', json=body)
        response.raise_for_status()
        return body

    response = request(owner, 'POST', '/v1/auth/login', json={'username': credential['username'], 'password': credential['password']})
    response.raise_for_status()
    assert response.cookies and 'Secure' in response.headers['Set-Cookie'] and 'HttpOnly' in response.headers['Set-Cookie'], 'Secure dashboard session cookie required'
    health_response = request(owner, 'GET', '/v1/health')
    health_response.raise_for_status()
    health = health_response.json()
    assert health['environment'] == 'stage', 'Smoke test requires stage metadata'
    checks = ['HTTPS, stage metadata, and secure login cookie']
    assert requests.get(base + '/v1/inbox', timeout=15, allow_redirects=False).status_code == 401
    checks.append('Unauthenticated Inbox refused')
    invalid = request(owner, 'POST', '/v1/inbox/missing/decide', json={'action': 'deny'}, headers={'Origin': 'https://wrong.example'})
    assert invalid.status_code == 403
    checks.append('Cross-origin mutation refused')
    servers_response = request(owner, 'GET', '/v1/servers')
    servers_response.raise_for_status()
    fixtures = {item['name']: item for item in servers_response.json() if item['name'] in {'sandbox-files', 'sandbox-errors'}}
    assert len(fixtures) == 2 and all(item.get('last_status') == 'ok' for item in fixtures.values()), 'Synthetic connectors must be healthy'
    checks.append('Two synthetic connectors healthy')

    question = {'schema_version': 2, 'client_request_id': 'verification-' + str(uuid.uuid4()),
                'prompt': 'Browser verification: choose checks',
                'context': 'A synthetic check of choices, drafts, and written text.',
                'question': {'type': 'multiple_choice', 'min_selections': 1, 'max_selections': 2,
                             'options': [{'id': 'mobile', 'label': 'Mobile layout'}, {'id': 'keyboard', 'label': 'Keyboard access'}, {'id': 'none', 'label': 'Neither', 'exclusive': True}]}}
    question_result = content(call('inbox.ask', question))
    assert question_result.get('ok'), 'Question failed validation'
    question_id = question_result['request_id']
    assert content(call('inbox.ask', question))['request_id'] == question_id
    checks.append('MCP question and idempotent submission')
    # Keep the independent question pending for a browser answer.

    original_result = call('sandbox-files.get_record', {})
    assert not original_result.get('isError'), 'Synthetic record cannot be read'
    original_text = content(original_result)['text']
    accepted_text = 'Stage verification passed ' + str(uuid.uuid4())
    rejected_text = 'This required call must remain rejected ' + str(uuid.uuid4())
    mixed_id = submit([('accepted_update', accepted_text), ('rejected_required_update', rejected_text)])
    blocked = call('sandbox-files.update_record', {'id': 'welcome', 'text': accepted_text})
    assert content(blocked).get('status') == 'permission_required', 'Write ran without permission'
    checks.append('Restricted write refused without Inbox context')
    try:
        note = 'Execute only the accepted synthetic call.'
        body = decide(mixed_id, {'accepted_update': {'verdict': 'accepted', 'reason': 'Validate this exact synthetic update.'},
                                 'rejected_required_update': {'verdict': 'rejected', 'reason': 'Required is a hint; this update is unnecessary.'}}, note)
        view = status(mixed_id)
        tools = {item['call_id']: item for item in view['tools']}
        assert view['status'] == 'approved' and view['owner_note'] == note
        assert tools['accepted_update']['verdict'] == 'accepted' and tools['rejected_required_update']['verdict'] == 'rejected'
        assert tools['accepted_update']['reason'] == body['verdicts']['accepted_update']['reason']
        assert tools['rejected_required_update']['reason'] == body['verdicts']['rejected_required_update']['reason']
        assert not tools['rejected_required_update'].get('grant') and not tools['rejected_required_update'].get('grant_id')
        grant = tools['accepted_update']['grant']
        recovered = {item['call_id']: item for item in status(mixed_id)['tools']}['accepted_update']
        assert recovered['grant'] == grant and recovered['grant_id'] == tools['accepted_update']['grant_id'], 'Lost response recovery changed permission'
        assert recovered['execution']['state'] == 'not_started', 'Status consumed a grant'
        assert len(detail(mixed_id)['grants']) == 1, 'Submission retry created extra permission'
        checks.append('Repeated tool, mixed verdicts, required rejection, notes, and stable grant recovery')
        wrong = call('sandbox-files.update_record', {'id': 'welcome', 'text': rejected_text, '_grant': grant})
        assert wrong.get('isError'), 'Changed parameters escaped approved scope'
        right = call('sandbox-files.update_record', {'id': 'welcome', 'text': accepted_text, '_grant': grant})
        assert not right.get('isError'), 'Approved synthetic write failed'
        again = call('sandbox-files.update_record', {'id': 'welcome', 'text': accepted_text, '_grant': grant})
        assert again.get('isError'), 'Single-use grant executed twice'
        executed = {item['call_id']: item for item in status(mixed_id)['tools']}['accepted_update']
        assert not executed.get('grant') and executed['execution']['state'] == 'succeeded', 'Execution outcome missing'
        assert content(call('sandbox-files.get_record', {}))['text'] == accepted_text
        checks.append('Exact scope, single execution claim, and persisted success')
        rejected_id = submit([('reject_first', rejected_text), ('reject_second', rejected_text + ' second')])
        decide(rejected_id, {key: {'verdict': 'rejected', 'reason': 'No additional synthetic write is necessary.'} for key in ['reject_first', 'reject_second']}, 'Reject this entire batch.')
        denied = status(rejected_id)
        assert denied['status'] == 'denied'
        assert all(item['verdict'] == 'rejected' and not item.get('grant') and not item.get('grant_id') for item in denied['tools'])
        assert not detail(rejected_id)['grants']
        assert content(call('sandbox-files.get_record', {}))['text'] == accepted_text
        checks.append('All-rejected batch completes without grants or writes')
    finally:
        # Restore only if an authorized fixture call changed the synthetic record.
        if content(call('sandbox-files.get_record', {}))['text'] != original_text:
            restore_id = submit([('restore_original', original_text)], 'Restore the original synthetic note')
            decide(restore_id, {'restore_original': {'verdict': 'accepted', 'reason': 'Restore the synthetic value captured before the test.'}}, 'Restore only the original synthetic note.')
            restore_grant = status(restore_id)['tools'][0]['grant']
            restored = call('sandbox-files.update_record', {'id': 'welcome', 'text': original_text, '_grant': restore_grant})
            assert not restored.get('isError'), 'Synthetic record restoration failed'
            assert content(call('sandbox-files.get_record', {}))['text'] == original_text
            checks.append('Original synthetic record restored through a separate grant')

    assert call('sandbox-errors.get_failure', {}).get('isError')
    checks.append('Logical tool failure remains an error')
    assert not call('sandbox-errors.get_slow', {}).get('isError')
    checks.append('Slow connector completes')
    assert call('sandbox-errors.get_auth_required', {}).get('isError')
    checks.append('Simulated sign-in failure remains an error')
    report = {'base_url': base, 'question_id': question_id, 'permission_id': mixed_id,
              'rejected_permission_id': rejected_id, 'checks': checks, 'version': health['version']}
    pathlib.Path(args.output).write_text(json.dumps(report, indent=2) + '\n')
    print(json.dumps({'question_id': question_id, 'checks': checks}, indent=2))


if __name__ == '__main__':
    main()
