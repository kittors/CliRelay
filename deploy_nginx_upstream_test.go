package main

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// Behavioural tests for the multi-node nginx layout and the slot decisions,
// run against the functions exactly as scripts/deploy-blue-green.sh ships them,
// with fake nginx, systemctl and curl binaries in front of PATH.

// upstreamVhost is the multi-node layout: user traffic goes through an upstream
// whose primary server is the local slot, overflow goes to the peer node over
// mTLS, and the peer's overflow arrives on 8445 and goes to the local slot
// only. The slot port appears twice and both must move together.
const upstreamVhost = `upstream clirelay_app {
    zone clirelay_app 64k;
    server 127.0.0.1:8319 max_conns=400;   # local active slot, rewritten by the deploy
    server 127.0.0.1:8446 backup;          # overflow to the peer node
    keepalive 64;
}
server {
    listen 127.0.0.1:8444 ssl http2;
    server_name relay.example.test;
    location / {
        proxy_pass http://clirelay_app;
        proxy_next_upstream error timeout;
        proxy_next_upstream_tries 2;
    }
}
server {
    listen 127.0.0.1:8446;
    location / {
        proxy_pass https://198.51.100.20:8445;
        proxy_ssl_verify on;
    }
}
server {
    listen 203.0.113.10:8445 ssl;
    ssl_verify_client on;
    location / {
        proxy_pass http://127.0.0.1:8319;
    }
}
`

var nginxCutoverFuncs = []string{
	"fail",
	"nginx_conf_text",
	"slot_refs",
	"nginx_slot_ports",
	"nginx_slot_port",
	"nginx_routes_to",
	"slot_ref_lines",
	"require_one_slot_in_conf",
	"verify_slot_locations",
	"rewrite_slot_port",
	"ensure_host_body_size_conf",
	"reload_nginx",
	"switch_nginx_port",
}

// nginxSandbox is a temp dir with a vhost file and a fake nginx that logs its
// arguments.
type nginxSandbox struct {
	dir, conf, nginxLog, binDir string
}

func newNginxSandbox(t *testing.T, vhost string) nginxSandbox {
	t.Helper()
	dir := t.TempDir()
	sb := nginxSandbox{
		dir:      dir,
		conf:     filepath.Join(dir, "nginx", "conf.d", "relay.conf"),
		nginxLog: filepath.Join(dir, "nginx.log"),
		binDir:   filepath.Join(dir, "bin"),
	}
	for _, d := range []string{filepath.Dir(sb.conf), sb.binDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if err := os.WriteFile(sb.conf, []byte(vhost), 0o644); err != nil {
		t.Fatalf("write vhost: %v", err)
	}
	writeExecutable(t, filepath.Join(sb.binDir, "nginx"), `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$NGINX_LOG"
`)
	return sb
}

func (sb nginxSandbox) run(t *testing.T, body string) (string, error) {
	t.Helper()
	prelude := fmt.Sprintf(`nginx_mode=host
nginx_conf=%q
PORT_A=8318
PORT_B=8319
NGINX_CONTAINER=nginx
NGINX_BODY_SIZE_CONF=%q
DEPLOY_ENV_FILE=/etc/clirelay2/deploy.env
DOMAIN=relay.example.test
`, sb.conf, filepath.Join(sb.dir, "nginx", "conf.d", "90-clirelay-body-size.conf"))
	return runShellFunctions(t, "scripts/deploy-blue-green.sh", nginxCutoverFuncs, prelude+body,
		"PATH="+sb.binDir+":"+os.Getenv("PATH"), "NGINX_LOG="+sb.nginxLog)
}

func (sb nginxSandbox) read(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(sb.conf)
	if err != nil {
		t.Fatalf("read vhost: %v", err)
	}
	return string(data)
}

func TestNginxCutoverMovesEveryUpstreamLocation(t *testing.T) {
	sb := newNginxSandbox(t, upstreamVhost)

	out, err := sb.run(t, `switch_nginx_port 8319 8318 && echo "routed=$(nginx_slot_port)"`)
	if err != nil {
		t.Fatalf("cutover 8319 -> 8318: %v\n%s", err, out)
	}
	conf := sb.read(t)
	for _, want := range []string{
		"server 127.0.0.1:8318 max_conns=400;",
		"proxy_pass http://127.0.0.1:8318;",
		// Nothing but slot references moves.
		"server 127.0.0.1:8446 backup;",
		"listen 127.0.0.1:8444 ssl http2;",
		"proxy_pass https://198.51.100.20:8445;",
		"listen 203.0.113.10:8445 ssl;",
	} {
		if !strings.Contains(conf, want) {
			t.Fatalf("after cutover the vhost is missing %q:\n%s", want, conf)
		}
	}
	if strings.Contains(conf, ":8319") {
		t.Fatalf("a location still routes to the old slot:\n%s", conf)
	}
	// Each location is checked and reported on its own.
	for _, want := range []string{"relay.conf:3 now routes to 8318", "relay.conf:27 now routes to 8318", "routed=8318"} {
		if !strings.Contains(out, want) {
			t.Fatalf("cutover output missing %q:\n%s", want, out)
		}
	}
	logData, err := os.ReadFile(sb.nginxLog)
	if err != nil {
		t.Fatalf("read nginx log: %v", err)
	}
	if got := strings.TrimSpace(string(logData)); got != "-t\n-s reload" {
		t.Fatalf("cutover must test then reload nginx, got %q", got)
	}

	// And back again on the next deploy.
	if out, err := sb.run(t, `switch_nginx_port 8318 8319`); err != nil {
		t.Fatalf("cutover 8318 -> 8319: %v\n%s", err, out)
	}
	if conf := sb.read(t); strings.Count(conf, "127.0.0.1:8319") != 2 || strings.Contains(conf, ":8318") {
		t.Fatalf("second cutover left the locations split:\n%s", conf)
	}
}

func TestNginxCutoverStillHandlesTheSingleProxyPassLayout(t *testing.T) {
	sb := newNginxSandbox(t, "server {\n    server_name relay.example.test;\n    location / {\n        proxy_pass http://127.0.0.1:8318;\n    }\n}\n")
	out, err := sb.run(t, `switch_nginx_port 8318 8319 && echo "routed=$(nginx_slot_port)"`)
	if err != nil || !strings.Contains(out, "routed=8319") {
		t.Fatalf("legacy cutover: %v\n%s", err, out)
	}
	if conf := sb.read(t); !strings.Contains(conf, "proxy_pass http://127.0.0.1:8319;") {
		t.Fatalf("legacy vhost not rewritten:\n%s", conf)
	}

	// Nothing to rewrite is a failure, not a success.
	if out, err := sb.run(t, `switch_nginx_port 8318 8319`); err == nil {
		t.Fatalf("a cutover away from a port the file no longer names must fail:\n%s", out)
	}
}

// A rewrite that moved one location but not the other would split user traffic
// from the peer node's overflow; the check runs per location, not on the file.
func TestNginxLocationCheckCatchesAHalfDoneRewrite(t *testing.T) {
	half := strings.Replace(upstreamVhost, "server 127.0.0.1:8319", "server 127.0.0.1:8318", 1)
	sb := newNginxSandbox(t, half)
	out, err := sb.run(t, `verify_slot_locations "$nginx_conf" 8319 8318 "3 27"`)
	if err == nil || !strings.Contains(out, "relay.conf:27 did not move from 8319 to 8318") {
		t.Fatalf("a location left on the old slot must fail verification, err=%v\n%s", err, out)
	}

	// Deploys refuse to start from such a config rather than guess which half is live.
	out, err = sb.run(t, `require_one_slot_in_conf`)
	if err == nil || !strings.Contains(out, "routes to both slots (8318 8319)") {
		t.Fatalf("a split config must be refused before any cutover, err=%v\n%s", err, out)
	}
	if out, err := sb.run(t, `nginx_slot_port; echo "<-routed"`); err != nil || !strings.HasPrefix(out, "<-routed") {
		t.Fatalf("a split config routes to neither slot as a whole, got %q (%v)", out, err)
	}
	if out, err := sb.run(t, `nginx_routes_to 8319 && nginx_routes_to 8318 && echo both`); err != nil || !strings.Contains(out, "both") {
		t.Fatalf("the failure path must see traffic on both slots: %v\n%s", err, out)
	}
}

// The drain guard refuses to stop a slot any live nginx file names, so an
// upstream layout split across files -- the peer listener in a file of its
// own -- would pin the old slot forever. The deploy refuses it up front.
func TestNginxSlotReferencesOutsideTheCutoverFileAreRefused(t *testing.T) {
	root := t.TempDir()
	confD := filepath.Join(root, "conf.d")
	enabled := filepath.Join(root, "sites-enabled")
	for _, d := range []string{confD, enabled} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	cutover := filepath.Join(confD, "relay.conf")
	files := map[string]string{
		cutover:                            "upstream a { server 127.0.0.1:8319; }\n",
		filepath.Join(confD, "peer.conf"):  "server { listen 203.0.113.10:8445 ssl; location / { proxy_pass http://127.0.0.1:8319; } }\n",
		filepath.Join(confD, "sonar.conf"): "server { location / { proxy_pass http://127.0.0.1:8787; } }\n",
		filepath.Join(confD, "relay.conf.bak-before-upstream"): "proxy_pass http://127.0.0.1:8318;\n",
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	// The same file reached through a sites-enabled link is not another file.
	if err := os.Symlink(cutover, filepath.Join(enabled, "relay.conf")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	pattern := extractShellAssignment(t, "scripts/deploy-blue-green.sh", "NGINX_BACKUP_PATTERN")
	body := fmt.Sprintf("nginx_conf=%q\nPORT_A=8318\nPORT_B=8319\nNGINX_DRAIN_GUARD_DIRS=%q\nNGINX_BACKUP_PATTERN=%q\nother_live_slot_files\n",
		cutover, confD+" "+enabled, pattern)
	out, err := runShellFunctions(t, "scripts/deploy-blue-green.sh", []string{"canonical_path", "other_live_slot_files"}, body)
	if err != nil {
		t.Fatalf("other_live_slot_files: %v\n%s", err, out)
	}
	if got := strings.Fields(out); len(got) != 1 || got[0] != filepath.Join(confD, "peer.conf") {
		t.Fatalf("stray slot files = %v, want only the split-off peer listener", got)
	}
}

// fakeSystemctl answers `is-active --quiet <unit>` from SYSTEMCTL_ACTIVE and
// `show -p <prop> --value <unit>` from FAKE_<PROP> variables.
const fakeSystemctl = `#!/usr/bin/env bash
if [ "$1" = "is-active" ] && [ "$2" = "--quiet" ]; then
  case " ${SYSTEMCTL_ACTIVE:-} " in *" $3 "*) exit 0 ;; esac
  exit 3
fi
if [ "$1" = "show" ] && [ "$2" = "-p" ]; then
  var="FAKE_$(printf '%s' "$3" | tr '[:lower:]' '[:upper:]')"
  printf '%s\n' "${!var:-}"
  exit 0
fi
exit 1
`

func TestDeployFreshNodeStartsAtTheFirstSlot(t *testing.T) {
	reconcile, err := filepath.Abs("scripts/reconcile-active-slot.sh")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	run := func(t *testing.T, recorded, active, reconcileScript string) (string, string, error) {
		t.Helper()
		dir := t.TempDir()
		binDir := filepath.Join(dir, "bin")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		writeExecutable(t, filepath.Join(binDir, "systemctl"), fakeSystemctl)
		portFile := filepath.Join(dir, ".active-port")
		if recorded != "" {
			if err := os.WriteFile(portFile, []byte(recorded+"\n"), 0o644); err != nil {
				t.Fatalf("write active port: %v", err)
			}
		}
		if reconcileScript == "" {
			reconcileScript = filepath.Join(dir, "reconcile-must-not-run.sh")
			writeExecutable(t, reconcileScript, "#!/usr/bin/env bash\ntouch \"$BASE_DIR/reconcile-ran\"\nexit 1\n")
		}
		body := fmt.Sprintf(`SERVICE_NAME=clirelay2
BASE_DIR=%q
PORT_A=8318
PORT_B=8319
ACTIVE_PORT_FILE=%q
RECONCILE_SCRIPT=%q
nginx_slot_port() { printf '%%s' "$ROUTED"; }
switch_nginx_port() { echo "unexpected nginx switch" >&2; return 1; }
resolve_slots
echo "first_deploy=${first_deploy} active=${active_port} next=${next_port} from=${cutover_from}"
`, dir, portFile, reconcileScript)
		out, err := runShellFunctions(t, "scripts/deploy-blue-green.sh", []string{"fail", "slot_is_running", "resolve_slots"}, body,
			"PATH="+binDir+":"+os.Getenv("PATH"), "SYSTEMCTL_ACTIVE="+active, "ROUTED=8319")
		return out, dir, err
	}

	// A new node: no record, nothing running. It used to fail in reconcile.
	out, dir, err := run(t, "", "", "")
	if err != nil {
		t.Fatalf("a fresh node must deploy: %v\n%s", err, out)
	}
	if !strings.Contains(out, "first_deploy=1 active= next=8318 from=8319") {
		t.Fatalf("a fresh node must start at 8318 and move nginx off its template port:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "reconcile-ran")); err == nil {
		t.Fatalf("a fresh node has nothing to reconcile")
	}

	// A live node alternates as before.
	out, _, err = run(t, "8319", "clirelay2-8319", reconcile)
	if err != nil || !strings.Contains(out, "first_deploy=0 active=8319 next=8318 from=8319") {
		t.Fatalf("a live node must alternate slots: %v\n%s", err, out)
	}

	// A record with nothing running is an outage, not a fresh node.
	out, _, err = run(t, "8319", "", reconcile)
	if err == nil || !strings.Contains(out, "no active deploy slot is running") {
		t.Fatalf("a node that lost its slots must not be treated as new, err=%v\n%s", err, out)
	}
}

func TestDeploySlotUserIsRequiredWithoutABaseUnit(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Fatalf("current user: %v", err)
	}
	binDir := t.TempDir()
	writeExecutable(t, filepath.Join(binDir, "systemctl"), fakeSystemctl)
	run := func(t *testing.T, slotUser string, env ...string) (string, error) {
		t.Helper()
		body := fmt.Sprintf(`SERVICE_NAME=clirelay2
DEPLOY_ENV_FILE=/etc/clirelay2/deploy.env
SLOT_USER=%q
SLOT_GROUP=
resolve_slot_identity
echo "slot_user=${slot_user}"
`, slotUser)
		return runShellFunctions(t, "scripts/deploy-blue-green.sh", []string{"fail", "read_service_property", "resolve_slot_identity"}, body,
			append([]string{"PATH=" + binDir + ":" + os.Getenv("PATH")}, env...)...)
	}

	out, err := run(t, "", "FAKE_LOADSTATE=not-found")
	if err == nil || !strings.Contains(out, "slot would run as root") || !strings.Contains(out, "set SLOT_USER in /etc/clirelay2/deploy.env") {
		t.Fatalf("a node without a base unit must name the slot user, err=%v\n%s", err, out)
	}
	if out, err := run(t, me.Username, "FAKE_LOADSTATE=not-found"); err != nil || !strings.Contains(out, "slot_user="+me.Username) {
		t.Fatalf("SLOT_USER must stand in for the missing base unit: %v\n%s", err, out)
	}
	if out, err := run(t, "", "FAKE_LOADSTATE=loaded", "FAKE_USER="+me.Username); err != nil || !strings.Contains(out, "slot_user="+me.Username) {
		t.Fatalf("the base unit's User= must still be copied: %v\n%s", err, out)
	}
	// An existing base unit without User= keeps running as before, loudly.
	if out, err := run(t, "", "FAKE_LOADSTATE=loaded"); err != nil || !strings.Contains(out, "slot_user=\n") || !strings.Contains(out, "runs as root as before") {
		t.Fatalf("a legacy root base unit must keep deploying with a warning: %v\n%s", err, out)
	}
	if out, err := run(t, "no_such_user_x9", "FAKE_LOADSTATE=not-found"); err == nil || !strings.Contains(out, "does not exist") {
		t.Fatalf("a slot user missing on the host must be refused, err=%v\n%s", err, out)
	}
}

// Behind DNS with several nodes, a smoke of the public name lands on whichever
// node DNS picks. The post-cutover smoke is pinned to this node instead, and
// keeps the real host name so SNI and the certificate are still checked.
func TestDeploySmokeIsPinnedToThisNode(t *testing.T) {
	binDir := t.TempDir()
	curlLog := filepath.Join(binDir, "curl.log")
	writeExecutable(t, filepath.Join(binDir, "curl"), `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$CURL_LOG"
printf 'HTTP/2 204\r\nx-cpa-version: dev-abc1234\r\n\r\nsmoke_status=204\n'
`)
	for ip, wantArgs := range map[string]string{
		"203.0.113.10": "--resolve relay.example.test:443:203.0.113.10 https://relay.example.test/readyz",
		"2001:db8::10": "--resolve relay.example.test:443:[2001:db8::10] https://relay.example.test/readyz",
		"":             "--connect-to relay.example.test:443:127.0.0.1:8444 https://relay.example.test/readyz",
	} {
		if err := os.Remove(curlLog); err != nil && !os.IsNotExist(err) {
			t.Fatalf("reset curl log: %v", err)
		}
		body := fmt.Sprintf(`DOMAIN=relay.example.test
NODE_PUBLIC_IP=%q
SMOKE_LOCAL_TLS_ADDR=127.0.0.1:8444
set_smoke_route
node_smoke /readyz
echo "status=${smoke_status} version=${smoke_version}"
`, ip)
		out, err := runShellFunctions(t, "scripts/deploy-blue-green.sh", []string{"header_version", "set_smoke_route", "node_smoke"}, body,
			"PATH="+binDir+":"+os.Getenv("PATH"), "CURL_LOG="+curlLog)
		if err != nil || !strings.Contains(out, "status=204 version=dev-abc1234") {
			t.Fatalf("NODE_PUBLIC_IP=%q: smoke failed: %v\n%s", ip, err, out)
		}
		logData, err := os.ReadFile(curlLog)
		if err != nil {
			t.Fatalf("read curl log: %v", err)
		}
		if !strings.Contains(string(logData), wantArgs) {
			t.Fatalf("NODE_PUBLIC_IP=%q: curl called with\n%s\nwant %q", ip, logData, wantArgs)
		}
	}
}
