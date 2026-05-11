import json
import sys

p = sys.argv[1] if len(sys.argv) > 1 else '/tmp/clirelay_auths.json'
data = json.load(open(p))
auths = data.get('data', data) if isinstance(data, dict) else data

print('Total auths:', len(auths) if isinstance(auths, list) else 'N/A')
found = False
for auth in (auths if isinstance(auths, list) else []):
    provider = auth.get('provider', '')
    model = auth.get('model', '')
    if 'codex' in provider.lower() or 'codex' in model.lower() or 'gpt-5' in model.lower():
        found = True
        print('---')
        print('provider:', provider)
        print('model:', model)
        print('enabled:', auth.get('enabled'))
        key = auth.get('api_key', '')
        print('api_key:', key[:8] + '...' if key else 'N/A')
        print('quota:', auth.get('quota'))
        print('used:', auth.get('used'))
        print('available:', auth.get('available'))

if not found:
    print('No codex-related auths found')
