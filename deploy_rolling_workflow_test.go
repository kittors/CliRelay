package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Guards for the multi-node rollout: the workflow deploys one node at a time,
// checks every node before touching any, and keeps probing each node, pinned
// to it, until its old slot has really stopped.

const deployNodesMatrix = `${{ fromJSON(vars.CLIRELAY_DEPLOY_NODES || '["default"]') }}`

type ghWorkflow struct {
	Env  map[string]string `yaml:"env"`
	Jobs map[string]ghJob  `yaml:"jobs"`
}

type ghJob struct {
	Needs    ghNeeds           `yaml:"needs"`
	If       string            `yaml:"if"`
	Strategy *ghStrategy       `yaml:"strategy"`
	Env      map[string]string `yaml:"env"`
	Steps    []ghStep          `yaml:"steps"`
}

type ghStrategy struct {
	MaxParallel *int              `yaml:"max-parallel"`
	FailFast    *bool             `yaml:"fail-fast"`
	Matrix      map[string]string `yaml:"matrix"`
}

type ghStep struct {
	Name string            `yaml:"name"`
	ID   string            `yaml:"id"`
	If   string            `yaml:"if"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	Env  map[string]string `yaml:"env"`
}

// ghNeeds accepts both `needs: build` and `needs: [build, preflight]`.
type ghNeeds []string

func (n *ghNeeds) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		*n = ghNeeds{value.Value}
		return nil
	}
	var list []string
	if err := value.Decode(&list); err != nil {
		return err
	}
	*n = list
	return nil
}

func (n ghNeeds) has(job string) bool {
	for _, need := range n {
		if need == job {
			return true
		}
	}
	return false
}

func loadDeployWorkflow(t *testing.T) (ghWorkflow, yaml.Node) {
	t.Helper()
	data, err := os.ReadFile(".github/workflows/deploy.yml")
	if err != nil {
		t.Fatalf("read deploy workflow: %v", err)
	}
	var wf ghWorkflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatalf("parse deploy workflow: %v", err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse deploy workflow: %v", err)
	}
	return wf, root
}

func workflowJob(t *testing.T, wf ghWorkflow, name string) ghJob {
	t.Helper()
	job, ok := wf.Jobs[name]
	if !ok {
		t.Fatalf("deploy workflow has no %q job", name)
	}
	return job
}

func workflowStep(t *testing.T, job ghJob, name string) (int, ghStep) {
	t.Helper()
	for i, step := range job.Steps {
		if step.Name == name {
			return i, step
		}
	}
	t.Fatalf("job has no step named %q", name)
	return -1, ghStep{}
}

// mappingKeys yields every mapping key anywhere in a YAML document.
func mappingKeys(node *yaml.Node, visit func(key string)) {
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			visit(node.Content[i].Value)
		}
	}
	for _, child := range node.Content {
		mappingKeys(child, visit)
	}
}

func TestDeployWorkflowRollsOutOneNodeAtATime(t *testing.T) {
	wf, root := loadDeployWorkflow(t)

	deploy := workflowJob(t, wf, "deploy")
	if deploy.Strategy == nil {
		t.Fatalf("deploy job must be a matrix over the rollout nodes")
	}
	// max-parallel 1 is what makes the rollout rolling: every node stays
	// healthy through its drain before the next one is touched.
	if deploy.Strategy.MaxParallel == nil || *deploy.Strategy.MaxParallel != 1 {
		t.Fatalf("deploy matrix must set max-parallel: 1, got %v", deploy.Strategy.MaxParallel)
	}
	// fail-fast stops the rollout at the first bad node, leaving the rest on
	// the previous build instead of pushing a broken one everywhere.
	if deploy.Strategy.FailFast == nil || !*deploy.Strategy.FailFast {
		t.Fatalf("deploy matrix must set fail-fast: true")
	}
	if got := deploy.Strategy.Matrix["node"]; got != deployNodesMatrix {
		t.Fatalf("deploy matrix nodes = %q, want %q so a repository without CLIRELAY_DEPLOY_NODES deploys the single default node", got, deployNodesMatrix)
	}
	if !deploy.Needs.has("build") || !deploy.Needs.has("preflight") {
		t.Fatalf("deploy must wait for the build and for the preflight of every node, needs = %v", deploy.Needs)
	}

	preflight := workflowJob(t, wf, "preflight")
	if preflight.Strategy == nil || preflight.Strategy.Matrix["node"] != deployNodesMatrix {
		t.Fatalf("preflight must cover exactly the nodes the deploy matrix covers")
	}
	if preflight.Strategy.FailFast == nil || !*preflight.Strategy.FailFast {
		t.Fatalf("preflight matrix must set fail-fast: true")
	}
	if !preflight.Needs.has("build") {
		t.Fatalf("preflight must run after the build, needs = %v", preflight.Needs)
	}

	docker := workflowJob(t, wf, "docker")
	if !docker.Needs.has("deploy") {
		t.Fatalf("the image build must wait for every node to be deployed, needs = %v", docker.Needs)
	}
	for _, name := range []string{"preflight", "deploy", "docker"} {
		if job := workflowJob(t, wf, name); !strings.Contains(job.If, "needs.build.outputs.should_deploy == 'true'") {
			t.Fatalf("%s job must honour the SHOULD_DEPLOY opt-out, if = %q", name, job.If)
		}
	}

	// A step or job that may fail without failing the run would let the
	// rollout continue past a broken node.
	mappingKeys(&root, func(key string) {
		if key == "continue-on-error" {
			t.Fatalf("deploy workflow must not use continue-on-error")
		}
	})
}

func TestDeployWorkflowResolvesSecretsPerNodeWithDefaultFallback(t *testing.T) {
	wf, _ := loadDeployWorkflow(t)
	want := map[string]string{
		// A missing per-node host must never fall back to SERVER_HOST: that
		// would deploy some other node twice and report the missing one done.
		"NODE_HOST":        `${{ matrix.node == 'default' && secrets.SERVER_HOST || secrets[format('SERVER_HOST_{0}', matrix.node)] }}`,
		"NODE_PORT":        `${{ matrix.node == 'default' && secrets.SERVER_PORT || secrets[format('SERVER_PORT_{0}', matrix.node)] }}`,
		"NODE_SSH_KEY":     `${{ secrets[format('SSH_PRIVATE_KEY_{0}', matrix.node)] || secrets.SSH_PRIVATE_KEY }}`,
		"NODE_KNOWN_HOSTS": `${{ secrets[format('DEPLOY_SSH_KNOWN_HOSTS_{0}', matrix.node)] || secrets.DEPLOY_SSH_KNOWN_HOSTS }}`,
		"NODE_SMOKE_IP":    `${{ matrix.node == 'default' && (secrets.SMOKE_IP || vars.CLIRELAY_SMOKE_IP) || secrets[format('SMOKE_IP_{0}', matrix.node)] || vars[format('CLIRELAY_SMOKE_IP_{0}', matrix.node)] }}`,
	}
	for _, jobName := range []string{"preflight", "deploy"} {
		_, step := workflowStep(t, workflowJob(t, wf, jobName), "Configure SSH for node")
		if step.Run != "./scripts/gha-node-ssh.sh" {
			t.Fatalf("%s must configure ssh with scripts/gha-node-ssh.sh, runs %q", jobName, step.Run)
		}
		for key, expr := range want {
			if got := step.Env[key]; got != expr {
				t.Fatalf("%s: %s = %q, want %q", jobName, key, got, expr)
			}
		}
	}

	// The workflow runs the helpers directly, so they must be committed executable.
	for _, path := range []string{"scripts/gha-node-ssh.sh", "scripts/gha-node-verify.sh"} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("%s must exist and be executable (err=%v)", path, err)
		}
	}

	_, plan := workflowStep(t, workflowJob(t, wf, "build"), "Record deploy mode")
	for _, marker := range []string{`^[A-Za-z0-9_]+$`, `ascii_downcase`, `CLIRELAY_DEPLOY_NODES`} {
		if !strings.Contains(plan.Run+plan.Env["DEPLOY_NODES"], marker) {
			t.Fatalf("the build must validate node names before any node is touched; missing %q", marker)
		}
	}
}

func TestDeployWorkflowPreflightGatesEveryNode(t *testing.T) {
	wf, _ := loadDeployWorkflow(t)
	_, step := workflowStep(t, workflowJob(t, wf, "preflight"), "Check node is ready for this release")
	for _, want := range []string{
		`ssh deploy-target true`,
		// The root scripts are synced by hand; a stale node fails here, before
		// any node has been touched.
		`grep -m1 '^SCRIPT_VERSION='`,
		`EXPECTED_SCRIPT_VERSION`,
		`/usr/local/sbin/clirelay-gha-deploy --preflight`,
		`^CLIRELAY_DEPLOY_CHECK ok`,
		// sudo must refuse variables outside the env_keep whitelist.
		`CLIRELAY_SETENV_PROBE=1`,
		`setenv-probe-accepted`,
	} {
		if !strings.Contains(step.Run, want) {
			t.Fatalf("preflight check missing %q", want)
		}
	}
}

func TestDeployWorkflowProbesEachNodeAcrossItsDrain(t *testing.T) {
	wf, _ := loadDeployWorkflow(t)

	drain, err := strconv.Atoi(extractShellAssignment(t, "scripts/deploy-blue-green.sh", "DRAIN_SECONDS"))
	if err != nil {
		t.Fatalf("parse DRAIN_SECONDS: %v", err)
	}
	minProbe, err := strconv.Atoi(wf.Env["PROBE_MIN_SECONDS"])
	if err != nil {
		t.Fatalf("parse PROBE_MIN_SECONDS: %v", err)
	}
	margin, err := strconv.Atoi(wf.Env["PROBE_MARGIN_SECONDS"])
	if err != nil {
		t.Fatalf("parse PROBE_MARGIN_SECONDS: %v", err)
	}
	// The old slot is stopped DRAIN_SECONDS after cutover; probing for less
	// declares success before the step that caused both past outages.
	if minProbe < drain+30 || margin < 30 {
		t.Fatalf("node probes must outlast the drain by 30s: PROBE_MIN_SECONDS=%d, PROBE_MARGIN_SECONDS=%d, DRAIN_SECONDS=%d", minProbe, margin, drain)
	}

	deploy := workflowJob(t, wf, "deploy")
	deployIndex, _ := workflowStep(t, deploy, "Blue-green deploy via fixed root entrypoint")
	verifyIndex, verify := workflowStep(t, deploy, "Verify service stays healthy through drain")
	if verifyIndex <= deployIndex {
		t.Fatalf("the per-node verification must run after that node's deploy")
	}
	if !strings.Contains(verify.Run, "./scripts/gha-node-verify.sh node") ||
		verify.Env["DRAIN_SECONDS"] != "${{ steps.deploy.outputs.drain_seconds }}" ||
		verify.Env["OLD_PORT"] != "${{ steps.deploy.outputs.old_port }}" {
		t.Fatalf("per-node verification must use the drain and slots the node reported, got run=%q env=%v", verify.Run, verify.Env)
	}

	docker := workflowJob(t, wf, "docker")
	publicIndex, public := workflowStep(t, docker, "Verify public endpoint after rollout")
	triggerIndex, _ := workflowStep(t, docker, "Trigger dev Docker image build")
	if publicIndex >= triggerIndex || !strings.Contains(public.Run, "./scripts/gha-node-verify.sh public") {
		t.Fatalf("an unpinned public probe must pass before the image build is triggered")
	}

	data, err := os.ReadFile("scripts/gha-node-verify.sh")
	if err != nil {
		t.Fatalf("read verify script: %v", err)
	}
	if !strings.Contains(string(data), `pin=(--resolve "${url_host}:${url_port}:${pin_addr}")`) {
		t.Fatalf("per-node probes must be pinned to the node with curl --resolve")
	}

	window := extractShellFunction(t, "scripts/gha-node-verify.sh", "probe_window_seconds")
	for drainSeconds, want := range map[string]string{"180": "210", "300": "330", "60": "210"} {
		cmd := exec.Command("bash", "-c", window+"\nprobe_window_seconds")
		cmd.Env = append(os.Environ(), "DRAIN_SECONDS="+drainSeconds,
			"PROBE_MIN_SECONDS="+wf.Env["PROBE_MIN_SECONDS"], "PROBE_MARGIN_SECONDS="+wf.Env["PROBE_MARGIN_SECONDS"])
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("probe_window_seconds: %v\n%s", err, out)
		}
		if got := strings.TrimSpace(string(out)); got != want {
			t.Fatalf("drain %ss: probe window = %ss, want %ss", drainSeconds, got, want)
		}
	}
}

// The next node may only be touched once the old slot on this one has really
// exited: until then the drain can still be refused, or still be cutting
// streams, and neither shows up in a health probe.
func TestWorkflowNodeVerifyWaitsForTheOldSlotToStop(t *testing.T) {
	binDir := t.TempDir()
	curlLog := filepath.Join(binDir, "curl.log")
	writeExecutable(t, filepath.Join(binDir, "curl"), `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$CURL_LOG"
printf 'HTTP/1.1 204 No Content\r\nx-cpa-version: %s\r\n\r\nprobe_status=204\n' "$FAKE_PUBLIC_VERSION"
`)
	writeExecutable(t, filepath.Join(binDir, "ssh"), `#!/usr/bin/env bash
cat > /dev/null
printf 'new_state=active old_state=%s served_version=%s\n' "$FAKE_OLD_STATE" "$FAKE_SERVED_VERSION"
`)

	run := func(t *testing.T, oldState, served string) (string, error) {
		t.Helper()
		cmd := exec.Command("bash", "scripts/gha-node-verify.sh", "node")
		cmd.Env = append(os.Environ(),
			"PATH="+binDir+":"+os.Getenv("PATH"),
			"CURL_LOG="+curlLog,
			"FAKE_PUBLIC_VERSION=dev-abc1234",
			"FAKE_OLD_STATE="+oldState,
			"FAKE_SERVED_VERSION="+served,
			"HEALTH_URL=https://relay.example.test/healthz",
			"APP_VERSION=dev-abc1234",
			"NODE=n43",
			"NODE_SMOKE_IP=203.0.113.10",
			"SLOT_SERVICE=clirelay2",
			"NEW_PORT=8318",
			"OLD_PORT=8319",
			"DRAIN_SECONDS=1",
			"SHUTDOWN_GRACE_SECONDS=0",
			"PROBE_MIN_SECONDS=1",
			"PROBE_MARGIN_SECONDS=0",
			"PROBE_INTERVAL_SECONDS=1",
			"STOP_WAIT_EXTRA_SECONDS=1",
			"STOP_POLL_SECONDS=1",
		)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	if out, err := run(t, "inactive", "dev-abc1234"); err != nil {
		t.Fatalf("a node whose old slot stopped must pass: %v\n%s", err, out)
	}
	logData, err := os.ReadFile(curlLog)
	if err != nil {
		t.Fatalf("read curl log: %v", err)
	}
	if !strings.Contains(string(logData), "--resolve relay.example.test:443:203.0.113.10 https://relay.example.test/healthz") {
		t.Fatalf("probes were not pinned to the node:\n%s", logData)
	}

	out, err := run(t, "active", "dev-abc1234")
	if err == nil || !strings.Contains(out, "old slot clirelay2-8319 is still active") {
		t.Fatalf("an old slot that never stopped must fail the node, err=%v\n%s", err, out)
	}

	out, err = run(t, "inactive", "dev-0000000")
	if err == nil || !strings.Contains(out, "reports build dev-0000000, expected dev-abc1234") {
		t.Fatalf("a new slot serving another build must fail the node, err=%v\n%s", err, out)
	}
}

// ssh looks a host on a non-standard port up as "[host]:port"; an entry for the
// bare host only covers port 22 and the connection then fails with a message
// that does not say why.
func TestWorkflowNodeSSHRequiresKnownHostsEntryForThePort(t *testing.T) {
	keyDir := t.TempDir()
	keygen := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", filepath.Join(keyDir, "host"))
	if out, err := keygen.CombinedOutput(); err != nil {
		t.Skipf("ssh-keygen unavailable: %v\n%s", err, out)
	}
	pub, err := os.ReadFile(filepath.Join(keyDir, "host.pub"))
	if err != nil {
		t.Fatalf("read host key: %v", err)
	}
	hostKey := strings.Join(strings.Fields(string(pub))[:2], " ")

	run := func(t *testing.T, env ...string) (string, string, error) {
		t.Helper()
		home := t.TempDir()
		githubEnv := filepath.Join(home, "github_env")
		cmd := exec.Command("bash", "scripts/gha-node-ssh.sh")
		cmd.Env = append(os.Environ(), append([]string{
			"HOME=" + home,
			"GITHUB_ENV=" + githubEnv,
			"NODE_SSH_KEY=placeholder-key-material",
		}, env...)...)
		out, err := cmd.CombinedOutput()
		config, _ := os.ReadFile(filepath.Join(home, ".ssh", "config"))
		exported, _ := os.ReadFile(githubEnv)
		return string(out), string(config) + string(exported), err
	}

	out, written, err := run(t, "NODE=n2", "NODE_HOST=203.0.113.10", "NODE_PORT=47222",
		"NODE_KNOWN_HOSTS=[203.0.113.10]:47222 "+hostKey)
	if err != nil {
		t.Fatalf("a bracketed entry must match port 47222: %v\n%s", err, out)
	}
	for _, want := range []string{"Host deploy-target", "Port 47222", "User deploy", "StrictHostKeyChecking yes", "NODE_SMOKE_IP=203.0.113.10"} {
		if !strings.Contains(written, want) {
			t.Fatalf("ssh setup missing %q:\n%s", want, written)
		}
	}

	out, _, err = run(t, "NODE=n2", "NODE_HOST=203.0.113.10", "NODE_PORT=47222",
		"NODE_KNOWN_HOSTS=203.0.113.10 "+hostKey)
	if err == nil || !strings.Contains(out, "no entry for [203.0.113.10]:47222") {
		t.Fatalf("a bare-host entry must be reported for port 47222, err=%v\n%s", err, out)
	}

	out, written, err = run(t, "NODE=default", "NODE_HOST=203.0.113.10",
		"NODE_KNOWN_HOSTS=203.0.113.10 "+hostKey, "NODE_SMOKE_IP=198.51.100.7")
	if err != nil || !strings.Contains(written, "Port 22") || !strings.Contains(written, "NODE_SMOKE_IP=198.51.100.7") {
		t.Fatalf("the default node on port 22 with an explicit smoke address must work, err=%v\n%s\n%s", err, out, written)
	}

	out, _, err = run(t, "NODE=n-156", "NODE_HOST=203.0.113.10", "NODE_KNOWN_HOSTS=203.0.113.10 "+hostKey)
	if err == nil || !strings.Contains(out, "letters, digits and underscores") {
		t.Fatalf("a node name that cannot be a secret suffix must be refused, err=%v\n%s", err, out)
	}

	out, _, err = run(t, "NODE=n2", "NODE_KNOWN_HOSTS=203.0.113.10 "+hostKey)
	if err == nil || !strings.Contains(out, "secret SERVER_HOST_n2 is not set") {
		t.Fatalf("a missing per-node host must name the per-node secret, err=%v\n%s", err, out)
	}

	// A node whose provider drops runner SSH is reached through a jump host,
	// and the jump host is held to the same strict known_hosts check.
	bothKeys := "[203.0.113.10]:47222 " + hostKey + "\n[198.51.100.53]:2233 " + hostKey
	out, written, err = run(t, "NODE=n2", "NODE_HOST=203.0.113.10", "NODE_PORT=47222",
		"NODE_KNOWN_HOSTS="+bothKeys, "NODE_JUMP=gha-jump@198.51.100.53:2233")
	if err != nil {
		t.Fatalf("a jump host with a known key must be accepted: %v\n%s", err, out)
	}
	for _, want := range []string{"Host deploy-jump", "HostName 198.51.100.53", "Port 2233", "User gha-jump", "ProxyJump deploy-jump"} {
		if !strings.Contains(written, want) {
			t.Fatalf("jump ssh setup missing %q:\n%s", want, written)
		}
	}
	if !strings.Contains(out, "via gha-jump@198.51.100.53:2233") {
		t.Fatalf("the setup summary must name the jump host:\n%s", out)
	}

	out, _, err = run(t, "NODE=n2", "NODE_HOST=203.0.113.10", "NODE_PORT=47222",
		"NODE_KNOWN_HOSTS=[203.0.113.10]:47222 "+hostKey, "NODE_JUMP=gha-jump@198.51.100.53:2233")
	if err == nil || !strings.Contains(out, "no entry for the jump host [198.51.100.53]:2233") {
		t.Fatalf("a jump host without a known key must be refused, err=%v\n%s", err, out)
	}

	out, _, err = run(t, "NODE=n2", "NODE_HOST=203.0.113.10", "NODE_PORT=47222",
		"NODE_KNOWN_HOSTS="+bothKeys, "NODE_JUMP=198.51.100.53")
	if err == nil || !strings.Contains(out, "SSH_JUMP_n2 must be user@host[:port]") {
		t.Fatalf("a jump host without a user must be refused, err=%v\n%s", err, out)
	}

	out, written, err = run(t, "NODE=n2", "NODE_HOST=203.0.113.10", "NODE_PORT=47222",
		"NODE_KNOWN_HOSTS=[203.0.113.10]:47222 "+hostKey)
	if err != nil || strings.Contains(written, "ProxyJump") {
		t.Fatalf("without NODE_JUMP the node must be reached directly, err=%v\n%s\n%s", err, out, written)
	}
}
