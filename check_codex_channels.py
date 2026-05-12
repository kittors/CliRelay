import json
import sys

p = sys.argv[1] if len(sys.argv) > 1 else '/tmp/clirelay_channels.json'
data = json.load(open(p))
channels = data.get('data', data) if isinstance(data, dict) else data

print('Total channels:', len(channels) if isinstance(channels, list) else 'N/A')
found = False
for ch in (channels if isinstance(channels, list) else []):
    name = ch.get('name', '')
    models = ch.get('models', [])
    if 'codex' in name.lower() or any('codex' in m.lower() or 'gpt-5.3' in m.lower() for m in models):
        found = True
        print('---')
        print('name:', name)
        print('type:', ch.get('type'))
        print('enabled:', ch.get('enabled'))
        print('models:', models[:10])

if not found:
    print('No codex-related channels found')
