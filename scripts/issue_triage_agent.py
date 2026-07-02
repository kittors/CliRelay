#!/usr/bin/env python3
import argparse
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request


PRIMARY_LABELS = {"bug", "enhancement", "documentation", "question"}
NEEDS_LABELS = {"needs-info", "needs-repro", "needs-logs"}
AREA_LABELS = {
    "area:api",
    "area:management",
    "area:auth",
    "area:billing",
    "area:deployment",
    "area:model-routing",
    "area:docs",
}
SPECIAL_LABELS = {"security", "ui", "performance", "triaged"}
MANAGED_LABELS = PRIMARY_LABELS | NEEDS_LABELS | AREA_LABELS | SPECIAL_LABELS
SYNCED_LABELS = PRIMARY_LABELS | NEEDS_LABELS | AREA_LABELS
QUIET_LABEL = "bot:quiet"

LABEL_DEFS = {
    "bug": ("Something isn't working", "d73a4a"),
    "enhancement": ("New feature or request", "0075ca"),
    "documentation": ("Improvements or additions to documentation", "0075ca"),
    "question": ("Further information is requested", "d876e3"),
    "security": ("Security-sensitive report or hardening", "b60205"),
    "ui": ("UI and UX improvements", "fbca04"),
    "performance": ("Performance or latency issues", "5319e7"),
    "needs-info": ("More information is needed from the reporter", "ededed"),
    "needs-repro": ("Reproduction steps are needed", "ededed"),
    "needs-logs": ("Sanitized logs or request details are needed", "ededed"),
    "triaged": ("Reviewed by the issue triage agent", "0e8a16"),
    QUIET_LABEL: ("Do not auto-comment on this issue or pull request", "bfdadc"),
    "area:api": ("Request forwarding and API compatibility", "c5def5"),
    "area:management": ("Management panel and management API", "c5def5"),
    "area:auth": ("Auth files, OAuth accounts, keys, and permissions", "c5def5"),
    "area:billing": ("Usage logs, billing, quotas, and statistics", "c5def5"),
    "area:deployment": ("Deployment, Docker, reverse proxy, and operations", "c5def5"),
    "area:model-routing": ("Models, routing, channel groups, and fallback", "c5def5"),
    "area:docs": ("Documentation", "c5def5"),
}

COMMENT_MARKER = "<!-- issue-triage-agent -->"
MAINTAINER_ASSOCIATIONS = {"OWNER", "MEMBER", "COLLABORATOR"}
BOT_COMMAND_RE = re.compile(r"(?i)^/(?:bot|triagebot)(?:\s+([a-z][a-z0-9_-]*))?(?:\s+(.*))?$")
SUPPORTED_COMMANDS = {"help", "triage", "summarize", "needs-info", "quiet", "unquiet"}
PUBLIC_COMMANDS = {"help"}
COMMAND_ALIASES = {
    "": "help",
    "usage": "help",
    "info": "help",
    "classify": "triage",
    "summary": "summarize",
    "needs": "needs-info",
    "need-info": "needs-info",
    "needsinfo": "needs-info",
    "mute": "quiet",
    "silence": "quiet",
    "unmute": "unquiet",
    "unsilence": "unquiet",
}
SECURITY_BOILERPLATE_PATTERNS = [
    r"(?im)^#+\s*Safety check\s*$",
    r"(?im)^[-*]\s*\[[ xX]\]\s*I have not included API keys, auth files, cookies, tokens, or other secrets\.\s*$",
    r"(?im)^[-*]\s*\[[ xX]\]\s*I have removed API keys, auth files, cookies, tokens, and other secrets from this issue\.\s*$",
]


def redact_text(text):
    if not text:
        return ""
    replacements = [
        (r"(?i)(authorization\s*:\s*bearer\s+)[^\s`]+", r"\1[REDACTED]"),
        (r"(?i)((?:api[-_ ]?key|x-api-key|token|secret|password|cookie)\s*[:=]\s*)[^\s`,;]+", r"\1[REDACTED]"),
        (r"cli-proxy-api-default-auto-created-sk-[A-Za-z0-9_\-]+", "cli-proxy-api-default-auto-created-sk-[REDACTED]"),
        (r"sk-[A-Za-z0-9_\-]{8,}", "sk-[REDACTED]"),
        (r"github_pat_[A-Za-z0-9_]+", "github_pat_[REDACTED]"),
        (r"gh[pousr]_[A-Za-z0-9_]{20,}", "ghx_[REDACTED]"),
        (r"\b[A-Za-z0-9_\-]{48,}\b", "[REDACTED_LONG_TOKEN]"),
    ]
    redacted = text
    for pattern, repl in replacements:
        redacted = re.sub(pattern, repl, redacted)
    return redacted


def compact(text, limit):
    text = redact_text(text or "").strip()
    if len(text) <= limit:
        return text
    return text[:limit].rsplit("\n", 1)[0] + "\n...[truncated]"


def strip_security_boilerplate(text):
    stripped = text or ""
    for pattern in SECURITY_BOILERPLATE_PATTERNS:
        stripped = re.sub(pattern, "", stripped)
    return stripped


def parse_bot_command(body):
    in_fence = False
    for line in (body or "").splitlines():
        stripped = line.strip()
        if stripped.startswith("```") or stripped.startswith("~~~"):
            in_fence = not in_fence
            continue
        if in_fence or not stripped or stripped.startswith(">"):
            continue

        match = BOT_COMMAND_RE.match(stripped)
        if not match:
            continue

        raw_name = (match.group(1) or "").lower()
        args = (match.group(2) or "").strip()
        name = COMMAND_ALIASES.get(raw_name, raw_name or "help")
        lower_args = args.lower()
        if raw_name == "needs" and lower_args.startswith("info"):
            name = "needs-info"
        if name == "quiet" and lower_args in {"off", "false", "0", "解除", "unquiet"}:
            name = "unquiet"
        return {
            "name": name,
            "args": args,
            "raw": stripped,
            "known": name in SUPPORTED_COMMANDS,
        }
    return None


def github_request(method, path, token, data=None):
    url = f"https://api.github.com{path}"
    body = None if data is None else json.dumps(data).encode()
    request = urllib.request.Request(url, data=body, method=method)
    request.add_header("Accept", "application/vnd.github+json")
    request.add_header("X-GitHub-Api-Version", "2022-11-28")
    request.add_header("Authorization", f"Bearer {token}")
    if data is not None:
        request.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            raw = response.read().decode()
            return json.loads(raw) if raw else None
    except urllib.error.HTTPError as exc:
        if exc.code in {404, 422}:
            return None
        detail = exc.read().decode(errors="replace")
        raise RuntimeError(f"GitHub API {method} {path} failed: {exc.code} {detail}") from exc


def load_event():
    event_path = os.environ.get("GITHUB_EVENT_PATH")
    if not event_path:
        raise RuntimeError("GITHUB_EVENT_PATH is not set")
    with open(event_path, encoding="utf-8") as handle:
        return json.load(handle)


def get_issue(event, event_name, repo, token):
    if event_name == "workflow_dispatch":
        number = os.environ.get("TRIAGE_ISSUE_NUMBER") or event.get("inputs", {}).get("issue_number")
        if not number:
            raise RuntimeError("workflow_dispatch requires issue_number")
        issue = github_request("GET", f"/repos/{repo}/issues/{number}", token)
        return issue, None

    issue = event.get("issue")
    if not issue:
        raise RuntimeError(f"No issue payload for {event_name}")
    if issue.get("pull_request") and event_name != "issue_comment":
        print("Skipping pull request issue payload.")
        return None, None
    return issue, event.get("comment")


def fetch_comments(repo, issue_number, token):
    comments = github_request(
        "GET",
        f"/repos/{repo}/issues/{issue_number}/comments?per_page=100",
        token,
    )
    return comments or []


def ensure_labels(repo, token):
    for name, (description, color) in LABEL_DEFS.items():
        github_request(
            "POST",
            f"/repos/{repo}/labels",
            token,
            {"name": name, "description": description, "color": color},
        )


def sync_issue_labels(repo, issue, token, desired_labels):
    current = {label["name"] for label in issue.get("labels", [])}
    desired = set(desired_labels) & MANAGED_LABELS
    desired.add("triaged")
    to_remove = sorted((current & SYNCED_LABELS) - (desired & SYNCED_LABELS))
    to_add = sorted(desired - current)

    if to_add:
        github_request(
            "POST",
            f"/repos/{repo}/issues/{issue['number']}/labels",
            token,
            {"labels": to_add},
        )

    for label in to_remove:
        github_request(
            "DELETE",
            f"/repos/{repo}/issues/{issue['number']}/labels/{urllib.parse.quote(label, safe='')}",
            token,
        )

    return to_add, to_remove


def has_label(issue, label):
    return label in {item["name"] for item in issue.get("labels", [])}


def add_issue_labels(repo, issue, token, labels):
    desired = sorted(set(labels))
    current = {label["name"] for label in issue.get("labels", [])}
    to_add = [label for label in desired if label not in current]
    if to_add:
        github_request(
            "POST",
            f"/repos/{repo}/issues/{issue['number']}/labels",
            token,
            {"labels": to_add},
        )
    return to_add


def remove_issue_label(repo, issue, token, label):
    if not has_label(issue, label):
        return False
    github_request(
        "DELETE",
        f"/repos/{repo}/issues/{issue['number']}/labels/{urllib.parse.quote(label, safe='')}",
        token,
    )
    return True


def security_hint(text):
    lower = strip_security_boilerplate(text).lower()
    strong_terms = [
        "api key",
        "apikey",
        "x-api-key",
        "authorization:",
        "bearer ",
        "secret",
        "cookie",
        "credential",
        "password",
        "private key",
        "auth bypass",
        "authentication bypass",
        "unauthorized",
        "vulnerability",
        "exploit",
        "ssrf",
        "rce",
        "xss",
        "csrf",
        "泄露",
        "漏洞",
        "未授权",
        "暴露",
        "密钥",
        "凭证",
    ]
    if any(term in lower for term in strong_terms):
        return True
    return bool(re.search(r"\btoken\b.{0,40}\b(leak|exposed|stolen|secret|compromised)\b", lower))


def fallback_triage(issue, comments, comment_required=True):
    text = f"{issue.get('title', '')}\n{issue.get('body', '')}"
    lower = text.lower()
    labels = []
    if security_hint(text):
        labels.append("security")
    if any(word in lower for word in ["docs", "documentation", "文档"]):
        primary = "documentation"
    elif any(word in lower for word in ["feature", "希望", "建议", "support", "add "]):
        primary = "enhancement"
    elif "?" in text or any(word in lower for word in ["how", "why", "请教", "怎么"]):
        primary = "question"
    else:
        primary = "bug"
    labels.append(primary)
    if primary == "bug" and len(issue.get("body") or "") < 300:
        labels.extend(["needs-info", "needs-repro"])
    if any(word in lower for word in ["ui", "界面", "页面", "布局"]):
        labels.append("ui")
        labels.append("area:management")
    if any(word in lower for word in ["slow", "卡", "latency", "performance"]):
        labels.append("performance")
    comment = (
        "已做初步分类。请补充最小复现步骤、版本信息和脱敏日志，这样维护者可以更快定位。"
        if re.search(r"[\u4e00-\u9fff]", text)
        else "Initial triage is done. Please add minimal reproduction steps, version details, and sanitized logs so maintainers can investigate faster."
    )
    return {"primary_label": primary, "labels": labels, "comment_required": comment_required, "comment": comment}


def build_prompt(repo, event_name, issue, comments, source_comment):
    recent_comments = []
    for comment in comments[-8:]:
        user = comment.get("user", {}).get("login", "unknown")
        body = compact(comment.get("body"), 1200)
        if COMMENT_MARKER in body:
            continue
        recent_comments.append(f"{user}:\n{body}")

    payload = {
        "repo": repo,
        "event": event_name,
        "issue": {
            "number": issue.get("number"),
            "kind": "pull_request" if issue.get("pull_request") else "issue",
            "title": compact(issue.get("title"), 500),
            "body": compact(issue.get("body"), 9000),
            "labels": [label["name"] for label in issue.get("labels", [])],
            "author": issue.get("user", {}).get("login"),
        },
        "latest_comment": None
        if not source_comment
        else {
            "author": source_comment.get("user", {}).get("login"),
            "author_association": source_comment.get("author_association"),
            "body": compact(source_comment.get("body"), 2500),
        },
        "recent_comments": recent_comments,
    }

    system = """
You are a senior maintainer triaging GitHub issues and pull requests for CliRelay and codeProxy.
Return only JSON. No markdown fence.

Allowed JSON shape:
{
  "primary_label": "bug|enhancement|documentation|question",
  "labels": ["..."],
  "comment_required": true,
  "comment": "..."
}

Managed labels:
- Primary: bug, enhancement, documentation, question. Choose exactly one primary_label.
- State: needs-info, needs-repro, needs-logs, triaged.
- Special: security, ui, performance.
- Area: area:api, area:management, area:auth, area:billing, area:deployment, area:model-routing, area:docs.

Rules:
- Use the same language as the reporter when practical.
- No emoji. Do not say you are an AI.
- Do not close issues, promise fixes, assign people, or ask for private secrets.
- Never repeat API keys, tokens, cookies, auth files, private domains, or long log bodies.
- Add security for key leaks, auth bypass, exposed management panels, credential handling, SSRF, RCE, supply chain, or other security-sensitive reports.
- Do not add security for normal token usage metrics, "token count", ordinary auth configuration, smoke tests, or missing logs.
- For public security reports, remind the reporter not to share secrets; if live credentials or exploit details are involved, suggest GitHub Security Advisory or a private maintainer channel.
- Add needs-repro when a bug lacks clear minimal reproduction steps.
- Add needs-logs when a runtime/API/deployment/billing bug lacks sanitized logs, request id, response error, or relevant screenshots.
- Add needs-info when versions, deployment mode, affected page/API/model, or expected/actual behavior are missing.
- For bug reports, include a concise investigation or likely fix direction if there is enough evidence.
- For feature requests, include a concise implementation direction and any obvious scope split.
- For questions, answer if possible; otherwise ask only for the missing facts.
- Ordinary comments are not a reason to speak. The bot should comment only after a workflow_dispatch run or an explicit /bot command.
- For issue_comment events, set comment_required=false unless a new reply would materially help.
- For issue_comment events, do not repeat the initial triage request or ask generic reproduction/version/log questions that were already asked.
- For issue_comment events from maintainers, owners, members, or collaborators, set comment_required=false unless the comment explicitly asks the triage agent to answer.
- If the latest comment says the maintainer will fix, optimize, rename, adjust wording, or otherwise handle the issue, set comment_required=false.
""".strip()

    return system, json.dumps(payload, ensure_ascii=False, indent=2)


def build_command_prompt(repo, command, issue, comments, source_comment):
    recent_comments = []
    for comment in comments[-10:]:
        body = compact(comment.get("body"), 1200)
        if COMMENT_MARKER in body:
            continue
        recent_comments.append(
            {
                "author": comment.get("user", {}).get("login", "unknown"),
                "author_association": comment.get("author_association"),
                "body": body,
            }
        )

    if command == "summarize":
        task = (
            "Summarize the current state, what is agreed or still uncertain, and the next useful action. "
            "For pull requests, do not claim to have reviewed code diff because only title, body, labels, and issue comments are provided."
        )
    else:
        task = (
            "Ask only for missing information that would materially unblock maintainers. "
            "Use a short checklist and avoid generic requests that are already answered in the issue."
        )

    payload = {
        "repo": repo,
        "command": command,
        "task": task,
        "issue": {
            "number": issue.get("number"),
            "kind": "pull_request" if issue.get("pull_request") else "issue",
            "title": compact(issue.get("title"), 500),
            "body": compact(issue.get("body"), 9000),
            "labels": [label["name"] for label in issue.get("labels", [])],
            "author": issue.get("user", {}).get("login"),
        },
        "latest_comment": None
        if not source_comment
        else {
            "author": source_comment.get("user", {}).get("login"),
            "author_association": source_comment.get("author_association"),
            "body": compact(source_comment.get("body"), 2500),
        },
        "recent_comments": recent_comments,
    }

    system = """
You are a careful GitHub maintainer assistant for CliRelay and codeProxy.
Return only JSON. No markdown fence.

Allowed JSON shape:
{
  "comment": "..."
}

Rules:
- Use the same language as the issue or pull request author when practical.
- No emoji. Do not say you are an AI.
- Do not invent facts, promise fixes, close issues, assign people, or ask for private secrets.
- Never repeat API keys, tokens, cookies, auth files, private domains, or long log bodies.
- Keep the answer concise and action-oriented.
""".strip()
    return system, json.dumps(payload, ensure_ascii=False, indent=2)


def call_model(system, user_payload):
    api_key = os.environ.get("OPENAI_API_KEY")
    if not api_key:
        raise RuntimeError("OPENAI_API_KEY is not set")
    base_url = os.environ.get("OPENAI_BASE_URL", "https://relay.07230805.xyz/v1").rstrip("/")
    model = os.environ.get("OPENAI_MODEL", "deepseek-v4-flash")
    data = {
        "model": model,
        "messages": [
            {"role": "system", "content": system},
            {"role": "user", "content": user_payload},
        ],
        "temperature": 0.1,
        "max_tokens": 1200,
    }
    request = urllib.request.Request(
        f"{base_url}/chat/completions",
        data=json.dumps(data).encode(),
        method="POST",
    )
    request.add_header("Content-Type", "application/json")
    request.add_header("Authorization", f"Bearer {api_key}")
    with urllib.request.urlopen(request, timeout=90) as response:
        result = json.loads(response.read().decode())
    return result["choices"][0]["message"]["content"]


def extract_json(text):
    try:
        return json.loads(text)
    except json.JSONDecodeError:
        start = text.find("{")
        end = text.rfind("}")
        if start == -1 or end == -1 or end <= start:
            raise
        return json.loads(text[start : end + 1])


def bool_or_default(value, default):
    if isinstance(value, bool):
        return value
    return default


def normalize_triage(raw, issue, comments, comment_default=True):
    if not isinstance(raw, dict):
        return fallback_triage(issue, comments, comment_required=comment_default)

    primary = raw.get("primary_label")
    if primary not in PRIMARY_LABELS:
        primary = fallback_triage(issue, comments, comment_required=comment_default)["primary_label"]

    security = security_hint(f"{issue.get('title', '')}\n{issue.get('body', '')}")
    raw_labels = raw.get("labels", [])
    if not isinstance(raw_labels, list):
        raw_labels = []
    labels = [
        label
        for label in raw_labels
        if label in MANAGED_LABELS and (label != "security" or security)
    ]
    labels.append(primary)
    labels.append("triaged")
    if security:
        labels.append("security")

    comment = str(raw.get("comment") or "").strip()
    comment = redact_text(comment)
    return {
        "primary_label": primary,
        "labels": sorted(set(labels)),
        "comment_required": bool_or_default(raw.get("comment_required"), comment_default),
        "comment": comment,
    }


def normalize_command_reply(raw, fallback):
    if isinstance(raw, dict):
        comment = raw.get("comment")
    else:
        comment = None
    comment = str(comment or fallback).strip()
    return redact_text(comment)


def format_label_list(labels):
    if not labels:
        return "无"
    return ", ".join(f"`{label}`" for label in sorted(set(labels)))


def format_help_message(issue):
    target = "PR" if issue.get("pull_request") else "issue"
    return f"""我是 CliProxy 的 issue/PR 助手。默认不会回复普通评论；只有看到明确的 `/bot ...` 指令才会行动。

当前对象：{target} `#{issue.get("number")}`

可用指令：
- `/bot help`：显示这份说明。
- `/bot triage`：重新分析当前 issue/PR 并同步标签。维护者可用。
- `/bot summarize`：总结当前状态、未决问题和下一步。维护者可用。
- `/bot needs-info`：根据上下文生成需要补充的信息清单，并标记 `needs-info`。维护者可用。
- `/bot quiet`：让本条 issue/PR 进入静默状态。维护者可用。
- `/bot unquiet` 或 `/bot quiet off`：解除静默状态。维护者可用。

说明：PR 里的 `/bot summarize` 只基于标题、正文、标签和评论，不读取代码 diff。"""


def format_unknown_command(command, issue):
    raw = redact_text(command.get("raw", "/bot"))
    return f"未识别指令 `{raw}`。\n\n{format_help_message(issue)}"


def format_permission_denied(command):
    return f"`/bot {command['name']}` 需要仓库维护者执行。普通评论不会触发 bot；可用 `/bot help` 查看指令。"


def format_triage_result(triage, added, removed):
    parts = [
        f"已按 `/bot triage` 重新分类：{format_label_list(triage['labels'])}。",
        f"标签变更：新增 {format_label_list(added)}；移除 {format_label_list(removed)}。",
    ]
    if triage["comment"]:
        parts.append(triage["comment"])
    return "\n\n".join(parts)


def fallback_command_comment(command, issue):
    text = f"{issue.get('title', '')}\n{issue.get('body', '')}"
    if command == "summarize":
        title = compact(issue.get("title"), 180) or f"#{issue.get('number')}"
        return f"当前主题：{title}\n\n下一步：请维护者根据最新上下文确认处理方向；如需重新分类，可执行 `/bot triage`。"
    if re.search(r"[\u4e00-\u9fff]", text):
        return "请补充：最小复现步骤、版本或部署方式、期望结果与实际结果、脱敏日志或截图。"
    return "Please add minimal reproduction steps, version or deployment details, expected vs actual behavior, and sanitized logs or screenshots."


def is_maintainer_comment(comment):
    if not comment:
        return False
    return comment.get("author_association") in MAINTAINER_ASSOCIATIONS


def is_bot_comment(comment):
    return bool(comment and comment.get("user", {}).get("type") == "Bot")


def command_allowed(command, comment):
    return command["name"] in PUBLIC_COMMANDS or is_maintainer_comment(comment)


def comment_already_posted(comments, marker):
    return any(marker in (comment.get("body") or "") for comment in comments)


def upsert_comment(repo, issue, token, comments, body, event_name, source_comment):
    if not body.strip():
        return "skipped-empty"

    event_marker = ""
    if event_name == "issue_comment" and source_comment:
        event_marker = f"<!-- issue-triage-agent:event:{source_comment['id']} -->"
        if comment_already_posted(comments, event_marker):
            return "skipped-duplicate"

    full_body = f"{COMMENT_MARKER}\n{event_marker}\n{body.strip()}".strip()

    if event_name != "issue_comment":
        for comment in comments:
            if COMMENT_MARKER in (comment.get("body") or ""):
                github_request(
                    "PATCH",
                    f"/repos/{repo}/issues/comments/{comment['id']}",
                    token,
                    {"body": full_body},
                )
                return "updated"

    github_request(
        "POST",
        f"/repos/{repo}/issues/{issue['number']}/comments",
        token,
        {"body": full_body},
    )
    return "created"


def run_model_triage(repo, issue, token, comments, event_name, source_comment, comment_default):
    system, user_payload = build_prompt(repo, event_name, issue, comments, source_comment)

    content = call_model(system, user_payload)
    try:
        raw = extract_json(content)
    except Exception as exc:
        print(f"Model returned invalid JSON, using fallback: {exc}", file=sys.stderr)
        raw = fallback_triage(issue, comments, comment_required=comment_default)

    triage = normalize_triage(raw, issue, comments, comment_default=comment_default)
    ensure_labels(repo, token)
    added, removed = sync_issue_labels(repo, issue, token, triage["labels"])
    print(f"labels_added={added}")
    print(f"labels_removed={removed}")
    return triage, added, removed


def run_model_command(repo, command, issue, comments, source_comment):
    system, user_payload = build_command_prompt(repo, command, issue, comments, source_comment)
    fallback = fallback_command_comment(command, issue)
    try:
        raw = extract_json(call_model(system, user_payload))
    except Exception as exc:
        print(f"Model returned invalid command JSON, using fallback: {exc}", file=sys.stderr)
        raw = {"comment": fallback}
    return normalize_command_reply(raw, fallback)


def run_issue_comment_command(repo, issue, token, source_comment):
    if is_bot_comment(source_comment):
        print("comment=skipped-bot")
        return

    command = parse_bot_command(source_comment.get("body") if source_comment else "")
    if not command:
        print("comment=skipped-no-command")
        return

    comments = fetch_comments(repo, issue["number"], token)

    if has_label(issue, QUIET_LABEL) and command["name"] != "unquiet":
        print("comment=skipped-quiet")
        return

    if not command["known"]:
        result = upsert_comment(
            repo,
            issue,
            token,
            comments,
            format_unknown_command(command, issue),
            "issue_comment",
            source_comment,
        )
        print(f"comment={result}")
        return

    if not command_allowed(command, source_comment):
        result = upsert_comment(
            repo,
            issue,
            token,
            comments,
            format_permission_denied(command),
            "issue_comment",
            source_comment,
        )
        print(f"comment={result}")
        return

    name = command["name"]
    if name == "help":
        body = format_help_message(issue)
    elif name == "quiet":
        ensure_labels(repo, token)
        added = add_issue_labels(repo, issue, token, [QUIET_LABEL])
        print(f"labels_added={added}")
        body = "已开启静默。本条 issue/PR 后续不会收到 bot 自动回复；需要恢复时执行 `/bot unquiet`。"
    elif name == "unquiet":
        ensure_labels(repo, token)
        removed = remove_issue_label(repo, issue, token, QUIET_LABEL)
        print(f"label_removed={QUIET_LABEL if removed else ''}")
        body = "已解除静默。后续仍然只会响应明确的 `/bot ...` 指令。"
    elif name == "triage":
        triage, added, removed = run_model_triage(
            repo,
            issue,
            token,
            comments,
            "issue_comment",
            source_comment,
            comment_default=False,
        )
        body = format_triage_result(triage, added, removed)
    elif name == "needs-info":
        ensure_labels(repo, token)
        added = add_issue_labels(repo, issue, token, ["needs-info"])
        print(f"labels_added={added}")
        body = run_model_command(repo, "needs-info", issue, comments, source_comment)
    else:
        body = run_model_command(repo, "summarize", issue, comments, source_comment)

    result = upsert_comment(repo, issue, token, comments, body, "issue_comment", source_comment)
    print(f"comment={result}")


def run():
    token = os.environ.get("GITHUB_TOKEN")
    repo = os.environ.get("GITHUB_REPOSITORY")
    event_name = os.environ.get("GITHUB_EVENT_NAME", "")
    if not token or not repo:
        raise RuntimeError("GITHUB_TOKEN and GITHUB_REPOSITORY are required")

    event = load_event()
    issue, source_comment = get_issue(event, event_name, repo, token)
    if not issue:
        return

    if event_name == "issue_comment":
        run_issue_comment_command(repo, issue, token, source_comment)
        return

    comments = fetch_comments(repo, issue["number"], token)
    triage, _, _ = run_model_triage(
        repo,
        issue,
        token,
        comments,
        event_name,
        source_comment,
        comment_default=event_name == "workflow_dispatch",
    )

    should_comment = event_name == "workflow_dispatch" and triage["comment_required"] and not has_label(issue, QUIET_LABEL)
    if should_comment:
        result = upsert_comment(repo, issue, token, comments, triage["comment"], event_name, source_comment)
        print(f"comment={result}")
    else:
        print("comment=skipped")


def self_test():
    sample = "Authorization: Bearer sk-abc1234567890 token=github_pat_abc123456789012345678901234567890"
    redacted = redact_text(sample)
    assert "abc1234567890" not in redacted
    assert "github_pat_abc" not in redacted
    parsed = extract_json("noise {\"primary_label\":\"bug\",\"labels\":[\"bug\"]} tail")
    assert parsed["primary_label"] == "bug"
    triage = normalize_triage(
        {"primary_label": "bug", "labels": ["area:api", "unknown"], "comment": "ok"},
        {"title": "Key 泄露", "body": "", "labels": []},
        [],
    )
    assert "security" in triage["labels"]
    assert "unknown" not in triage["labels"]
    triage = normalize_triage(
        {"primary_label": "bug", "labels": ["security"], "comment": "ok"},
        {"title": "token count remains 0", "body": "API requests succeed but token count remains 0.", "labels": []},
        [],
    )
    assert "security" not in triage["labels"]
    assert not security_hint(
        "### Safety check\n\n"
        "- [X] I have not included API keys, auth files, cookies, tokens, or other secrets."
    )
    assert not security_hint(
        "### Safety check\n\n"
        "- [X] I have removed API keys, auth files, cookies, tokens, and other secrets from this issue."
    )
    assert security_hint("The response leaked an API key in logs.")
    triage = normalize_triage(
        {"primary_label": "enhancement", "labels": ["enhancement"], "comment": "ok"},
        {"title": "命名优化", "body": "", "labels": []},
        [],
        comment_default=False,
    )
    assert triage["comment_required"] is False
    triage = normalize_triage(
        {"primary_label": "bug", "labels": "bug", "comment_required": "false", "comment": "ok"},
        {"title": "normal bug", "body": "", "labels": []},
        [],
        comment_default=False,
    )
    assert triage["comment_required"] is False
    assert triage["labels"] == ["bug", "triaged"]
    assert fallback_triage({"title": "命名优化", "body": "", "labels": []}, [], comment_required=False)["comment_required"] is False
    assert is_maintainer_comment({"author_association": "OWNER", "user": {"type": "User"}})
    assert not is_maintainer_comment({"author_association": "CONTRIBUTOR", "user": {"type": "User"}})
    assert not is_maintainer_comment({"author_association": "NONE", "user": {"type": "Bot"}})
    assert is_bot_comment({"author_association": "NONE", "user": {"type": "Bot"}})
    assert parse_bot_command("普通评论") is None
    assert parse_bot_command("> /bot help") is None
    assert parse_bot_command("```text\n/bot help\n```") is None
    assert parse_bot_command("请处理\n/bot help")["name"] == "help"
    assert parse_bot_command("/bot triage")["name"] == "triage"
    assert parse_bot_command("/bot needs info")["name"] == "needs-info"
    assert parse_bot_command("/bot quiet off")["name"] == "unquiet"
    assert not parse_bot_command("/bot dance")["known"]
    assert command_allowed({"name": "help"}, {"author_association": "NONE", "user": {"type": "User"}})
    assert not command_allowed({"name": "triage"}, {"author_association": "CONTRIBUTOR", "user": {"type": "User"}})
    assert command_allowed({"name": "triage"}, {"author_association": "MEMBER", "user": {"type": "User"}})

    issue = {"number": 7, "title": "命名优化", "body": "", "labels": []}
    normal_comment = {
        "id": 101,
        "body": "确实，直接把接口返回拿过来显示，不太好。我优化下",
        "author_association": "OWNER",
        "user": {"type": "User", "login": "maintainer"},
    }
    help_comment = {
        "id": 102,
        "body": "/bot help",
        "author_association": "CONTRIBUTOR",
        "user": {"type": "User", "login": "reporter"},
    }
    triage_comment = {
        "id": 103,
        "body": "/bot triage",
        "author_association": "CONTRIBUTOR",
        "user": {"type": "User", "login": "reporter"},
    }

    original_fetch_comments = fetch_comments
    original_upsert_comment = upsert_comment
    original_call_model = call_model
    try:
        def fail_fetch_comments(*args):
            raise AssertionError("ordinary comments must not fetch comments")

        globals()["fetch_comments"] = fail_fetch_comments
        run_issue_comment_command("owner/repo", issue, "token", normal_comment)

        posted = []

        def fake_fetch_comments(*args):
            return []

        def fake_upsert_comment(repo, issue, token, comments, body, event_name, source_comment):
            posted.append(body)
            return "created"

        def fail_call_model(*args):
            raise AssertionError("help and permission-denied commands must not call the model")

        globals()["fetch_comments"] = fake_fetch_comments
        globals()["upsert_comment"] = fake_upsert_comment
        globals()["call_model"] = fail_call_model
        run_issue_comment_command("owner/repo", issue, "token", help_comment)
        assert "默认不会回复普通评论" in posted[-1]

        run_issue_comment_command("owner/repo", issue, "token", triage_comment)
        assert "需要仓库维护者执行" in posted[-1]
    finally:
        globals()["fetch_comments"] = original_fetch_comments
        globals()["upsert_comment"] = original_upsert_comment
        globals()["call_model"] = original_call_model


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    if args.self_test:
        self_test()
        print("self-test ok")
    else:
        run()
