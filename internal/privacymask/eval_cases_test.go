// Ported from magpie's internal/redact/eval_cases_test.go (MIT, see doc.go).
// The texts are made up; nothing in them is anyone's data.

package privacymask

// evalCase is one text of the evaluation (eval_test.go): where it comes
// from (an issue or a commit, or "true" for one of the values masking is
// for), the values in it to mask and those to keep as written.
type evalCase struct {
	ID, From, Text string
	Mask, Keep     []evalValue
}

// evalValue is a value in a case's text. Kind is what it is, or for one
// to keep what it was or could be taken for. Known, where set, is the
// issue of a value that comes out wrong with the rules and Privacy
// Filter; GuardKnown, with the rules and AstrLink Guard.
type evalValue struct{ V, Kind, Known, GuardKnown string }

// The keys are made up, in the vendors' shapes; the people, places and
// numbers are made up or the ones vendors and standards give as examples.
var (
	evalOpenAIKey = "sk-proj-" + "Qm7vT2xLk9RbW4nZpE6sJc8YhD3fGa5uKiN1oVr0"
	evalGitHub    = "ghp_" + "0123456789abcdefghijABCDEFGHIJ0123"
	evalAWSSecret = "wJalrXUtnFEMI/K7MDENG/" + "bPxRfiCYEXAMPLEKEY"
	evalGoogleKey = "AIzaSy" + "B3xQ9mLk2Vr7TpW4nZs8YhC1dFgJ6uKe0oA"
	evalJWT       = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9." + "eyJzdWIiOiI0MjEiLCJuYW1lIjoiTWFyaWEifQ." + "x7Hq2LmZ9pVb4WnT8sRc1YeKd6gJ3uFa0oNi5vQwE"
)

var evalCases = []evalCase{
	// ---- reported: masked and should not have been ----------------------
	{
		ID: "snake-arg", From: "#3144",
		Text: "parser.add_argument(\"--sequential_cutoff\", type=int, default=3,\n    help=\"stop after this many misses\")",
		Keep: []evalValue{{V: "sequential_cutoff", Kind: "PERSON"}},
	},
	{
		ID: "snake-words", From: "#3144",
		Text: "def build_index(max_retries, warmup_steps):\n    return Index(stream_options=None, include_usage=True)",
		Keep: []evalValue{{V: "max_retries", Kind: "PERSON"}, {V: "warmup_steps", Kind: "PERSON"}, {V: "stream_options", Kind: "PERSON"}, {V: "include_usage", Kind: "PERSON"}},
	},
	{
		ID: "tool-servers", From: "#3143",
		Text: "Use mcp__claude-in-chrome__navigate to open the page, then mcp__linear__save_issue to file it.",
		Keep: []evalValue{{V: "mcp__claude-in-chrome__navigate", Kind: "PERSON"}, {V: "mcp__linear__save_issue", Kind: "PERSON"}},
	},
	{
		ID: "retina-file", From: "#3029",
		Text: "Here is the screenshot: ![shot](/Users/dev/Desktop/261020_14-16-11@2x.jpg) — the button is cut off.",
		Keep: []evalValue{{V: "261020_14-16-11@2x.jpg", Kind: "EMAIL"}},
	},
	{
		ID: "asset-files", From: "#3029",
		Text: "import logo from './assets/logo@3x.png';\nimport intro from './media/intro@2x.mp4';",
		Keep: []evalValue{{V: "logo@3x.png", Kind: "EMAIL"}, {V: "intro@2x.mp4", Kind: "EMAIL"}},
	},
	{
		ID: "url-userinfo", From: "#3538",
		Text: "DATABASE_URL=postgres://app:hunter2pass@db.acme-corp.cn:5432/orders",
		Mask: []evalValue{{V: "hunter2pass", Kind: "SECRET"}},
		Keep: []evalValue{{V: "db.acme-corp.cn", Kind: "EMAIL"}},
	},
	{
		ID: "git-user-url", From: "#3538",
		Text: "git clone https://git@gitlab.acme-corp.cn/team/repo.git && cd repo",
		Keep: []evalValue{{V: "git@gitlab.acme-corp.cn", Kind: "EMAIL"}},
	},
	{
		ID: "unix-time", From: "#3577",
		Text: `{"type":"tool_call","id":"call_7","ts":1761379200,"name":"read_file","args":{"path":"main.go"}}`,
		Keep: []evalValue{{V: "1761379200", Kind: "PHONE"}},
	},
	{
		ID: "unix-time-ms", From: "#3577",
		Text: "tool_call finished at 1761379200123 (took 812 ms)",
		Keep: []evalValue{{V: "1761379200123", Kind: "PHONE"}},
	},
	{
		ID: "session-uuid", From: "d3db461bb",
		Text: "Read ~/.claude/projects/-Users-dev-app/3f2b6c1e-8a4d-4f7e-9b1a-2c3d4e5f6a7b.jsonl for the transcript.",
		Keep: []evalValue{{V: "3f2b6c1e-8a4d-4f7e-9b1a-2c3d4e5f6a7b", Kind: "SECRET"}},
	},
	{
		ID: "digests", From: "d3db461bb",
		Text: "sha256: 80efc1803eda7c095a79741d2008c07e2e0a57b01bac8825fbeb448fd097998c\ncommit b855cfee9a1c3d4e5f60718293a4b5c6d7e8f901",
		Keep: []evalValue{{V: "80efc1803eda7c095a79741d2008c07e2e0a57b01bac8825fbeb448fd097998c", Kind: "SECRET"}, {V: "b855cfee9a1c3d4e5f60718293a4b5c6d7e8f901", Kind: "ACCOUNT"}},
	},
	{
		ID: "tool-ids", From: "611b9e173",
		Text: `{"type":"tool_result","tool_use_id":"toolu_01XkP9qLm2VbT7wZ4nR8sYc3","content":"ok"} request req_011CUq7vHn3kLp9WmT2xZb4R`,
		Keep: []evalValue{{V: "toolu_01XkP9qLm2VbT7wZ4nR8sYc3", Kind: "SECRET"}, {V: "req_011CUq7vHn3kLp9WmT2xZb4R", Kind: "SECRET"}},
	},
	{
		ID: "token-count", From: "611b9e173",
		Text: "<system-reminder><total_tokens>14902266 tokens left</total_tokens></system-reminder>",
		Keep: []evalValue{{V: "14902266", Kind: "ACCOUNT"}},
	},
	{
		ID: "assistant-names", From: "611b9e173",
		Text: "You are Claude, powered by Opus 5.5. Codex and Haiku 4.5 may also answer in this session.",
		Keep: []evalValue{{V: "Claude", Kind: "PERSON"}, {V: "Opus 5.5", Kind: "PERSON"}, {V: "Codex", Kind: "PERSON"}, {V: "Haiku 4.5", Kind: "PERSON"}},
	},
	{
		ID: "home-path", From: "611b9e173",
		Text: "cd /Users/jdoe/workspace/projects/magpie && go test ./internal/redact",
		Keep: []evalValue{{V: "jdoe", Kind: "PERSON"}, {V: "magpie", Kind: "PERSON"}},
	},
	{
		ID: "grep-pattern", From: "611b9e173",
		Text: "rg -n 'password|passwd|secret_key|api_token' --glob '*.go'",
		Keep: []evalValue{{V: "password|passwd|secret_key|api_token", Kind: "SECRET"}},
	},
	{
		ID: "sizes-versions", From: "611b9e173",
		Text: "Screenshot 1624x1487 at 2x; magpie v0.1.1513 built with go1.26.8 on darwin/arm64.",
		Keep: []evalValue{{V: "1624x1487", Kind: "PHONE"}, {V: "v0.1.1513", Kind: "ACCOUNT"}, {V: "1.26.8", Kind: "PHONE"}},
	},
	{
		ID: "noreply", From: "true",
		Text: "Author: dependabot[bot] <49699333+dependabot[bot]@users.noreply.github.com>",
		Keep: []evalValue{{V: "49699333+dependabot[bot]@users.noreply.github.com", Kind: "EMAIL"}},
	},
	{
		ID: "example-mail", From: "true",
		Text: "// e.g. send(\"user@example.com\", subject)",
		Keep: []evalValue{{V: "user@example.com", Kind: "EMAIL"}},
	},
	{
		ID: "code-locations", From: "true",
		Text: "internal/redact/model.go:212:9: undefined: epochNumber (pid 48213, exit status 2)",
		Keep: []evalValue{{V: "48213", Kind: "PHONE"}, {V: "model.go:212", Kind: "ACCOUNT"}},
	},
	{
		ID: "colors-ports", From: "true",
		Text: ".btn { color: #1a2b3c; } server listening on 127.0.0.1:3425, retry in 30000ms",
		Keep: []evalValue{{V: "#1a2b3c", Kind: "SECRET"}, {V: "30000", Kind: "PHONE"}},
	},
	{
		ID: "zh-words", From: "#1977",
		Text: "请帮我把这个函数改成异步的，并且加上错误处理和单元测试。",
		Keep: []evalValue{{V: "函数", Kind: "PERSON"}, {V: "错误处理", Kind: "PERSON"}},
	},

	{
		ID: "ci-table", From: "611b9e173",
		Text: "| run | file | result |\n| 6123456789 | ci.yml | success |",
		Keep: []evalValue{{V: "6123456789", Kind: "ACCOUNT"}, {V: "ci.yml", Kind: "PERSON"}},
	},
	{
		ID: "git-log", From: "d3db461bb",
		Text: "ba319a758 gateway: the lone tool_choice none goes to the Grok plugin only (#3110)\nUpdate to v0.1.1442 now",
		Keep: []evalValue{{V: "ba319a758", Kind: "ACCOUNT"}, {V: "v0.1.1442", Kind: "ACCOUNT"}},
	},
	{
		ID: "pixels", From: "d3db461bb",
		Text: "Attached a screenshot of 1624x1487 pixels, 2048000 bytes.",
		Keep: []evalValue{{V: "1624x1487", Kind: "ACCOUNT"}, {V: "2048000", Kind: "PHONE"}},
	},
	{
		ID: "assistant-prose", From: "d3db461bb",
		Text: "Thanks Claude, that fixed it. Also ask Gemini later. — Sam Lee\nHi Codex, please review. Opus said it looks good, Sonnet disagreed.",
		Mask: []evalValue{{V: "Sam Lee", Kind: "PERSON"}},
		Keep: []evalValue{{V: "Claude", Kind: "PERSON"}, {V: "Codex", Kind: "PERSON"}, {V: "Opus", Kind: "PERSON"}, {V: "Sonnet", Kind: "PERSON"}},
	},
	{
		ID: "commit-login", From: "d3db461bb",
		Text: "commit 919585d1700e515ac38da49e31bc60a527fc15c2\nAuthor: yetone <yetone@users.noreply.github.com>",
		Keep: []evalValue{{V: "yetone", Kind: "PERSON"}, {V: "919585d1700e515ac38da49e31bc60a527fc15c2", Kind: "SECRET"}},
	},
	{
		ID: "session-tokens", From: "d3db461bb",
		Text: "session 019a2b3c-4d5e-7f60-8a9b-0c1d2e3f4a5b token 019a2b3c4d5e7f608a9b0c1d2e3f4a5b",
		Keep: []evalValue{{V: "019a2b3c-4d5e-7f60-8a9b-0c1d2e3f4a5b", Kind: "SECRET"}},
	},
	{
		ID: "user-path", From: "d3db461bb",
		Text: "read /Users/yetone/workspace/projects/magpie/AGENTS.md and C:\\Users\\vdxz\\notes.txt, then /home/rina/.config/app.toml",
		Keep: []evalValue{{V: "yetone", Kind: "PERSON"}, {V: "AGENTS.md", Kind: "PERSON"}, {V: "vdxz", Kind: "PERSON"}, {V: "rina", Kind: "PERSON"}},
	},
	{
		ID: "skill-list", From: "d3db461bb",
		Text: "- artifact-design: Design guidance\n- workflow-authoring: Reference for writing scripts\nUser yetone logged in; local LLM ready\nReviewer: Approved",
		Keep: []evalValue{{V: "artifact-design", Kind: "PERSON"}, {V: "workflow-authoring", Kind: "PERSON"}, {V: "yetone", Kind: "PERSON"}, {V: "Reviewer: Approved", Kind: "PERSON"}},
	},
	{
		ID: "one-letter", From: "true",
		Text: "J said the build is fine; K and Q disagreed.",
		Keep: []evalValue{{V: "J", Kind: "PERSON"}, {V: "K", Kind: "PERSON"}},
	},
	{
		ID: "tool-use-ids", From: "d3db461bb",
		Text: `{"type":"tool_result","tool_use_id":"toolu_01XyZabcDEF123456","id":"srvtoolu_01AbCdEfGhIjKlMnOp","call":"call_Qm7vT2xLk9RbW4nZ"}`,
		Keep: []evalValue{{V: "toolu_01XyZabcDEF123456", Kind: "SECRET"}, {V: "srvtoolu_01AbCdEfGhIjKlMnOp", Kind: "SECRET"}, {V: "call_Qm7vT2xLk9RbW4nZ", Kind: "ACCOUNT"}},
	},
	{
		ID: "task-ids", From: "d3db461bb",
		Text: "<task-id>a97c9bc67cc1f1e27</task-id> wrote 31a4f944-e7cf-4f6f-acc7-eef50d9911dd.jsonl",
		Keep: []evalValue{{V: "a97c9bc67cc1f1e27", Kind: "SECRET"}, {V: "31a4f944-e7cf-4f6f-acc7-eef50d9911dd", Kind: "SECRET"}},
	},
	{
		ID: "hex-lines", From: "d3db461bb",
		Text: "hex40 ok? 1 2f1d0c9b8a7e6f5d4c3b2a1908f7e6d5c4b3a291\n919585d1700e515ac38da49e31bc60a527fc15c2 ok? 1",
		Keep: []evalValue{{V: "2f1d0c9b8a7e6f5d4c3b2a1908f7e6d5c4b3a291", Kind: "SECRET", Known: "#3597"}, {V: "919585d1700e515ac38da49e31bc60a527fc15c2", Kind: "SECRET"}},
	},
	{
		ID: "go-caches", From: "d3db461bb",
		Text: "open /Users/x/Library/Caches/go-build/63700b7ab65fd1d3@v63: no such file\nP=/Users/yetone/.npm/_npx/1e7f6d9597241db0 d6426ecaaf0a28de@v31",
		Keep: []evalValue{{V: "63700b7ab65fd1d3@v63", Kind: "EMAIL"}, {V: "d6426ecaaf0a28de@v31", Kind: "EMAIL"}, {V: "1e7f6d9597241db0", Kind: "SECRET"}},
	},
	{
		ID: "base64-image", From: "d3db461bb",
		Text: "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAA+k5zM7Md7tM9ehxd51xF/isvM9",
		Keep: []evalValue{{V: "k5zM7Md7tM9ehxd51xF", Kind: "SECRET"}, {V: "isvM9", Kind: "SECRET"}},
	},
	{
		ID: "grep-alts", From: "d3db461bb",
		Text: "rg 'modelTakes|modelFits|personFits' internal; join at discord.gg/Ab3dEf9h today",
		Keep: []evalValue{{V: "modelTakes|modelFits|personFits", Kind: "SECRET"}, {V: "discord.gg/Ab3dEf9h", Kind: "SECRET"}},
	},
	{
		ID: "json-settings", From: "d3db461bb",
		Text: `settings: {"mode": "fast", "retries": "3", "theme": "dark"}`,
		Keep: []evalValue{{V: `"mode": "fast"`, Kind: "SECRET"}, {V: `"retries": "3"`, Kind: "SECRET"}},
	},
	{
		ID: "event-times", From: "#3577",
		Text: `{"event": "tool_call", "timestamp": 1792903687, "ms": 1792903687123, "at": "2026-10-25T04:48:03.755Z", "desk": "+1 415 555 0132", "ext": "4821"}`,
		Mask: []evalValue{{V: "415 555 0132", Kind: "PHONE"}},
		Keep: []evalValue{{V: "1792903687", Kind: "PHONE"}, {V: "4821", Kind: "PHONE"}},
	},
	{
		ID: "codex-meta", From: "#3144",
		Text: `{"stream_options":{"reasoning_summary_delivery":"sequential_cutoff"},"session_id":"019a2b3c-4d5e-7f60-8a9b-0c1d2e3f4a5b","text":"set sequential_cutoff for Rina Kowalczyk, skill at /Users/vdxz/.codex/skills/wrangler/SKILL.md, my password is Tr0ub4dor&3xq"}`,
		Mask: []evalValue{{V: "Rina Kowalczyk", Kind: "PERSON"}, {V: "Tr0ub4dor&3xq", Kind: "SECRET"}},
		Keep: []evalValue{{V: "sequential_cutoff", Kind: "PERSON"}, {V: "reasoning_summary_delivery", Kind: "PERSON"}, {V: "019a2b3c-4d5e-7f60-8a9b-0c1d2e3f4a5b", Kind: "SECRET"}, {V: "vdxz", Kind: "PERSON"}},
	},
	{
		ID: "short-values", From: "true",
		Text: "office: Unit 5, postcode 94107",
		Keep: []evalValue{{V: "94107", Kind: "ADDRESS"}},
	},
	{
		ID: "url-login", From: "#3538",
		Text: "git clone https://deploy-bot@git.acme-corp.cn/team/app.git and ssh ops@10.0.0.7",
		Keep: []evalValue{{V: "deploy-bot@git.acme-corp.cn", Kind: "EMAIL"}},
	},
	{
		ID: "zh-names-edge", From: "true",
		Text: "这个问题请找王小明先生处理，抄送给李华老师。",
		Mask: []evalValue{{V: "王小明", Kind: "PERSON"}, {V: "李华", Kind: "PERSON"}},
		Keep: []evalValue{{V: "先生", Kind: "PERSON"}, {V: "老师", Kind: "PERSON"}},
	},
	// from Claude Code sessions, after a reporter found most of the
	// model's finds wrong (10-26); the shapes are theirs, the values made up
	{
		ID: "metric-decimals", From: "x-tearicerice",
		Text: "2026-10-21T08:10:00Z 2984.517203648091\n2026-10-21T08:20:00Z 1.0833333333333333\n2026-10-21T08:30:00Z 101.47368421052631",
		Keep: []evalValue{{V: "2984.517203648091", Kind: "PHONE"}, {V: "1.0833333333333333", Kind: "PHONE"}, {V: "101.47368421052631", Kind: "PHONE"}},
	},
	{
		ID: "run-ids", From: "x-tearicerice",
		Text: "Someone ran Deploy by hand ([run 48213097715](https://github.com/acme/web/actions/runs/48213097715)).\n$ env -u GH_TOKEN -u GITHUB_TOKEN gh run view 48213097716 --log | tail\n15:17:29Z fleet run 48213097716",
		Keep: []evalValue{{V: "run 48213097715", Kind: "PHONE"}, {V: "48213097716", Kind: "SECRET"}},
	},
	{
		ID: "background-tasks", From: "x-tearicerice",
		Text: "Command running in background with ID: kq3m8zx2t. Output is being written to: /private/tmp/claude-501/tasks/kq3m8zx2t.output\nThe command ran past its timeout and was moved to the background (ID: bv7n2wq5p).\n<task-notification>\n<task-id>kq3m8zx2t</task-id>",
		Keep: []evalValue{{V: "kq3m8zx2t", Kind: "SECRET"}, {V: "bv7n2wq5p", Kind: "SECRET"}},
	},
	{
		ID: "model-names", From: "x-tearicerice",
		Text: "{'api_provider': 'openai', 'status': '404', 'litellm_model_name': 'qwen/qwen3-30b-a3b-instruct-2507'} 2.1\n{'api_provider': 'None', 'litellm_model_name': 'qwen/qwen3-vl-32b-instruct'} 1.0\n$ grep -n 'qwen3-max\\b\\|qwen/qwen3-14b' usage.log",
		Keep: []evalValue{{V: "qwen/qwen3-30b-a3b-instruct-2507", Kind: "SECRET"}, {V: "qwen/qwen3-vl-32b-instruct", Kind: "SECRET"}, {V: `qwen3-max\b\|qwen/qwen3-14b`, Kind: "SECRET"}},
	},
	{
		ID: "ls-files", From: "x-tearicerice",
		Text: "-rwxr-xr-x@  1 sam  staff   690 Aug 20 19:58 make-icons.sh\n-rw-r--r--@  1 sam  staff  1204 Aug 21 09:02 notes-old.md",
		Keep: []evalValue{{V: "make-icons.sh", Kind: "SECRET"}, {V: "notes-old.md", Kind: "SECRET"}},
	},
	{
		ID: "gateway-ids", From: "x-tearicerice",
		Text: `{"stream":true,"gatewayRequestId":"req_gateway_7c41e09a5bd2f3687e1a90c4","ledgerId":"gl_Wq7rTb2xNe5YpLaZ"}` + "\n- tb_Ra4Kp9Lm-ZqT2wXy_8Hs, $1.61: release about 17:05Z",
		Keep: []evalValue{{V: "req_gateway_7c41e09a5bd2f3687e1a90c4", Kind: "SECRET"}, {V: "gl_Wq7rTb2xNe5YpLaZ", Kind: "SECRET"}, {V: "tb_Ra4Kp9Lm-ZqT2wXy_8Hs", Kind: "SECRET"}, {V: "release about 17:05Z", Kind: "SECRET"}},
	},
	{
		ID: "transcript-path", From: "x-tearicerice",
		Text: "read the full transcript at: /Users/sam/.claude/projects/-Users-sam-work-acme-web/5c0d2e1f-7a4b-4c3d-9e8f-0a1b2c3d4e5f.jsonl\nContinue the conversation",
		Keep: []evalValue{{V: "Users-sam-work-acme-web/5c0d2e1f-7a4b-4c3d-9e8f-0a1b2c3d4e5f.jsonl", Kind: "ADDRESS"}},
	},
	{
		ID: "pr-checks", From: "x-tearicerice",
		Text: "CodeRabbit\tpending\t0\t\tReview in progress\nGreptile Review\tpending\t0\thttps://app.greptile.com/review/3860\nCI\tpass\t4m12s\n" + `{"region":"us-east-1","edge":"eu.stg.acme.io","monitored":false}`,
		Keep: []evalValue{{V: "CodeRabbit", Kind: "PERSON"}, {V: "Greptile", Kind: "PERSON"}, {V: "CI", Kind: "PERSON"}, {V: "eu.stg.acme.io", Kind: "EMAIL"}},
	},
	{
		ID: "team-notes", From: "x-tearicerice",
		Text: "The 14:55Z errors came from our own org; Dana Whitfield is the lead if this turns out to be a settlement failure, and Priya wants it fixed first.",
		Mask: []evalValue{{V: "Dana Whitfield", Kind: "PERSON"}, {V: "Priya", Kind: "PERSON"}},
	},
	// the owner's 脱敏 page, 10-26: a model's id beside a setting's key,
	// and a toolchain's
	{
		ID: "settings-model", From: "owner",
		Text: `[providers]` + "\n" + `groqAPIKey = "gsk_x…"` + "\n" + `groqAPIModel = "llama3-70b-8192"` + "\n" + `claudeAPIURL = "https://api.anthropic.com"` + "\n" + `claudeAPIModel = "claude-3-5-sonnet-20241022"`,
		Keep: []evalValue{{V: "llama3-70b-8192", Kind: "SECRET"}, {V: "claude-3-5-sonnet-20241022", Kind: "SECRET"}},
	},
	{
		ID: "rust-toolchain", From: "owner",
		Text: "- MSRV check. The repo's minimum rust-version is 1.78. Pinning to `nightly-2026-09-01` (about 7 weeks old) is plausible.\n- Risk: the pinned toolchain lacks fixes.",
		Keep: []evalValue{{V: "nightly-2026-09-01", Kind: "PERSON"}},
	},

	// ---- what masking is for --------------------------------------------
	{
		ID: "prose-person", From: "true",
		Text: "Please email Maria Gonzalez about the overdue invoice before Friday.",
		Mask: []evalValue{{V: "Maria Gonzalez", Kind: "PERSON"}},
	},
	{
		ID: "git-author", From: "true",
		Text: "commit 4f1c2d9\nAuthor: Jane Smith <jane.smith@gmail.com>\nDate:   Tue Oct 21 09:14:03 2026 +0800",
		Mask: []evalValue{{V: "Jane Smith", Kind: "PERSON"}, {V: "jane.smith@gmail.com", Kind: "EMAIL"}},
	},
	{
		ID: "json-contact", From: "true",
		Text: `{"customer":{"name":"Wei Zhang","email":"wei.zhang@outlook.com","phone":"+86 139 1234 5678"}}`,
		Mask: []evalValue{{V: "Wei Zhang", Kind: "PERSON"}, {V: "wei.zhang@outlook.com", Kind: "EMAIL"}, {V: "139 1234 5678", Kind: "PHONE"}},
	},
	{
		ID: "us-address", From: "true",
		Text: "Ship the replacement to 742 Evergreen Terrace, Springfield, OR 97477 and call (212) 555-0199 on arrival.",
		Mask: []evalValue{{V: "742 Evergreen Terrace", Kind: "ADDRESS"}, {V: "555-0199", Kind: "PHONE"}},
	},
	{
		ID: "zh-contact", From: "#1977",
		Text: "联系人：张伟，电话 13800138000，地址：北京市朝阳区建国路88号院3号楼",
		Mask: []evalValue{{V: "张伟", Kind: "PERSON"}, {V: "13800138000", Kind: "PHONE"}, {V: "建国路88号", Kind: "ADDRESS"}},
	},
	{
		ID: "zh-prose", From: "#1977",
		Text: "王芳下周一去上海出差，酒店那边请联系她的助理刘洋。",
		Mask: []evalValue{{V: "王芳", Kind: "PERSON"}, {V: "刘洋", Kind: "PERSON", Known: "#3592"}},
	},
	{
		ID: "log-user", From: "true",
		Text: "2026-10-21T09:14:03Z INFO login ok user=li.na@qq.com tenant=acme ip=10.2.3.4",
		Mask: []evalValue{{V: "li.na@qq.com", Kind: "EMAIL"}},
	},
	{
		ID: "phone-intl", From: "true",
		Text: "If the build breaks overnight, page Tom on +1 415-555-0132.",
		Mask: []evalValue{{V: "415-555-0132", Kind: "PHONE"}, {V: "Tom", Kind: "PERSON"}},
	},
	{
		ID: "card", From: "true",
		Text: "The customer's card number is 4111 1111 1111 1111, expiry 12/29.",
		Mask: []evalValue{{V: "4111 1111 1111 1111", Kind: "ACCOUNT"}},
	},
	{
		ID: "iban", From: "true",
		Text: "Wire the refund to IBAN DE89 3704 0044 0532 0130 00 (Commerzbank).",
		Mask: []evalValue{{V: "DE89 3704 0044 0532 0130 00", Kind: "ACCOUNT"}},
	},
	{
		ID: "zh-id", From: "true",
		Text: "身份证号 11010519491231002X，开户行 招商银行",
		Mask: []evalValue{{V: "11010519491231002X", Kind: "ACCOUNT"}},
	},
	{
		ID: "env-file", From: "true",
		Text: "OPENAI_API_KEY=" + evalOpenAIKey + "\nDB_PASSWORD=Tr0ub4dor&3xyz\nDEBUG=true",
		Mask: []evalValue{{V: evalOpenAIKey, Kind: "SECRET"}, {V: "Tr0ub4dor&3xyz", Kind: "SECRET"}},
		Keep: []evalValue{{V: "DEBUG=true", Kind: "SECRET"}},
	},
	{
		ID: "prose-password", From: "true",
		Text: "the staging db password is hunter2Xq9z, don't commit it",
		Mask: []evalValue{{V: "hunter2Xq9z", Kind: "SECRET"}},
	},
	{
		ID: "zh-password", From: "true",
		Text: "服务器密码是 Qw8!zLp3mN，用完记得改掉",
		Mask: []evalValue{{V: "Qw8!zLp3mN", Kind: "SECRET"}},
	},
	{
		ID: "bearer", From: "true",
		Text: "curl -H 'Authorization: Bearer " + evalJWT + "' https://api.acme-corp.cn/v1/me",
		Mask: []evalValue{{V: evalJWT, Kind: "SECRET"}},
	},
	{
		ID: "code-keys", From: "true",
		Text: "const gh = \"" + evalGitHub + "\";\nconst maps = '" + evalGoogleKey + "';\naws_secret_access_key = " + evalAWSSecret,
		Mask: []evalValue{{V: evalGitHub, Kind: "SECRET"}, {V: evalGoogleKey, Kind: "SECRET"}, {V: evalAWSSecret, Kind: "SECRET"}},
	},
	{
		ID: "tool-result-mixed", From: "true",
		Text: "$ cat contacts.csv\nname,email,phone\nRina Kobayashi,rina.k@fastmail.com,+81 90-1234-5678\nOmar Haddad,omar.haddad@proton.me,+44 7700 900123",
		Mask: []evalValue{{V: "Rina Kobayashi", Kind: "PERSON"}, {V: "rina.k@fastmail.com", Kind: "EMAIL"}, {V: "90-1234-5678", Kind: "PHONE", GuardKnown: "#3776"}, {V: "Omar Haddad", Kind: "PERSON"}, {V: "omar.haddad@proton.me", Kind: "EMAIL"}, {V: "7700 900123", Kind: "PHONE"}},
		Keep: []evalValue{{V: "contacts.csv", Kind: "EMAIL"}},
	},
}
