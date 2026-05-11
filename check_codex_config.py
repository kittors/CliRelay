import json
import sys

p = sys.argv[1] if len(sys.argv) > 1 else '/tmp/clirelay_model_configs.json'
data = json.load(open(p))
configs = data.get('data', data) if isinstance(data, dict) else data

found = False
for cfg in configs:
    model = cfg.get('model', cfg.get('name', ''))
    if 'gpt-5.3-codex' in model or 'codex' in model.lower():
        found = True
        print('---')
        print('model:', model)
        print('enabled:', cfg.get('enabled'))
        print('provider:', cfg.get('provider'))
        print('channel:', cfg.get('channel'))
        print('credentials_count:', len(cfg.get('credentials', [])))
        for i, cred in enumerate(cfg.get('credentials', [])[:3]):
            print(f'  cred[{i}] enabled:', cred.get('enabled'))
            key = cred.get('api_key', '')
            print(f'  cred[{i}] key prefix:', key[:8] + '...' if key else 'N/A')

if not found:
    print('No codex-related model configs found')
    print('Total configs:', len(configs) if isinstance(configs, list) else 'N/A')
