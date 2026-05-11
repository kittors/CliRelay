import json
import sys

p = sys.argv[1] if len(sys.argv) > 1 else '/dev/stdin'
data = json.load(open(p) if p != '/dev/stdin' else sys.stdin)
files = data if isinstance(data, list) else data.get('data', data.get('files', []))

# Get full error message for a few key accounts
print('=== FULL ERROR FOR izm.cool accounts ===')
for f in files:
    label = f.get('label', '')
    if 'izm.cool' in label:
        print(f"\nLabel: {label}")
        print(f"  unavailable: {f.get('unavailable')}")
        print(f"  status: {f.get('status')}")
        msg = f.get('status_message', '')
        print(f"  full status_message: {json.dumps(msg, indent=4)}")
        print(f"  cooling_down: {f.get('cooling_down')}")
        break

print('\n=== FULL ERROR FOR oldelm account ===')
for f in files:
    label = f.get('label', '')
    if 'oldelm' in label:
        print(f"\nLabel: {label}")
        msg = f.get('status_message', '')
        print(f"  full status_message: {json.dumps(msg, indent=4)}")
        print(f"  cooling_down: {f.get('cooling_down')}")
        break

print('\n=== cooling_down field across all ===')
cd_keys = [k for k in files[0].keys() if 'cool' in k.lower()] if files else []
print(f"cooling_down related keys: {cd_keys}")
for f in files:
    if f.get('provider') == 'codex':
        cd = f.get('cooling_down')
        if cd:
            print(f"  {f.get('label')}: cooling_down={cd}")