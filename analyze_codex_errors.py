import json
import sys

p = sys.argv[1] if len(sys.argv) > 1 else '/dev/stdin'
data = json.load(open(p) if p != '/dev/stdin' else sys.stdin)
files = data if isinstance(data, list) else data.get('data', data.get('files', []))

print('=== CODEX ERROR STATUS (unavailable=False) ===')
for f in files:
    if f.get('provider') == 'codex' and not f.get('unavailable') and f.get('status') == 'error':
        print(f"\nLabel: {f.get('label')}")
        print(f"  unavailable: {f.get('unavailable')}")
        print(f"  status: {f.get('status')}")
        msg = f.get('status_message', {})
        if isinstance(msg, dict):
            err = msg.get('error', msg)
            if isinstance(err, dict):
                print(f"  error.type: {err.get('type')}")
                print(f"  error.message: {err.get('message', '')[:100]}")
                print(f"  error.code: {err.get('code')}")
                resets = err.get('resets_at') or err.get('resets_in_seconds')
                if resets:
                    print(f"  resets_at/in_seconds: {resets}")
            else:
                print(f"  error: {err}")
        elif isinstance(msg, str):
            print(f"  msg: {msg[:100]}")

print('\n=== CODEX UNAVAILABLE ===')
for f in files:
    if f.get('provider') == 'codex' and f.get('unavailable'):
        print(f"\nLabel: {f.get('label')}")
        print(f"  unavailable: {f.get('unavailable')}")
        print(f"  status: {f.get('status')}")
        msg = f.get('status_message', {})
        if isinstance(msg, dict):
            err = msg.get('error', msg)
            if isinstance(err, dict):
                print(f"  error.type: {err.get('type')}")
                print(f"  error.message: {err.get('message', '')[:100]}")
                print(f"  error.code: {err.get('code')}")
                resets = err.get('resets_at') or err.get('resets_in_seconds')
                if resets:
                    print(f"  resets_at/in_seconds: {resets}")
            else:
                print(f"  error: {err}")