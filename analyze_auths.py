import json
import sys

p = sys.argv[1] if len(sys.argv) > 1 else '/dev/stdin'
data = json.load(open(p) if p != '/dev/stdin' else sys.stdin)
files = data if isinstance(data, list) else data.get('data', data.get('files', []))

print(f'Total: {len(files)}')
active_count = sum(1 for f in files if f.get('status') == 'active')
unavail_count = sum(1 for f in files if f.get('unavailable'))
print(f'active: {active_count}, unavailable: {unavail_count}')

print('\n=== UNAVAILABLE ===')
for f in files:
    if f.get('unavailable'):
        print(f"  {f.get('provider')} | {f.get('label')} | unavailable={f.get('unavailable')} | status={f.get('status')}")

print('\n=== AVAILABLE ===')
for f in files:
    if not f.get('unavailable'):
        print(f"  {f.get('provider')} | {f.get('label')} | unavailable={f.get('unavailable')} | status={f.get('status')}")

print('\n=== PROVIDER SUMMARY ===')
from collections import Counter
providers = Counter(f.get('provider', 'unknown') for f in files)
for prov, cnt in providers.most_common():
    active = sum(1 for f in files if f.get('provider') == prov and not f.get('unavailable'))
    print(f"  {prov}: {active}/{cnt} active")