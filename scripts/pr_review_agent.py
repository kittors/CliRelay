#!/usr/bin/env python3
"""GitHub PR review agent: reads the PR diff and posts a structured review via OpenAI-compatible API."""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request

COMMENT_MARKER = "<!-- pr-review-agent -->"
QUIET_LABEL = "bot:quiet"
MAINTAINER_ASSOCIATIONS = {"OWNER", "MEMBER", "COLLABORATOR"}
BOT_COMMAND_RE = re.compile(
    r"(?i)^/(?:bot|triagebot|reviewbot)(?:\s+([a-z][a-z0-9_-]*))?(?:\s+(.*))?$"
)
SUPPORTED_COMMANDS = {"help", "review", "quiet", "unquiet"}
PUBLIC_COMMANDS = {"help", "review"}
COMMAND_ALIASES = {
    "": "help",
    "usage": "help",
    "info": "help",
    "re-review": "review",
    "rereview": "review",
    "check": "review",
    "mute": "quiet",
    "silence": "quiet",
    "unmute": "unquiet",
    "unsilence": "unquiet",
}

# Skip noisy / generated paths in the review payload.
SKIP_PATH_PATTERNS = [
    re.compile(p, re.I)
    for p in [
        r"(^|/)\.git/",
        r"(^|/)node_modules/",
        r"(^|/)vendor/",
        r"(^|/)dist/",
        r"(^|/)build/",
        r"(^|/)coverage/",
        r"\.(lock|sum)$",
        r"(^|/)go\.sum$",
        r"(^|/)bun\.lockb?$",
        r"(^|/)package-lock\.json$",
        r"(^|/)yarn\.lock$",
        r"(^|/)pnpm-lock\.yaml$",
        r"\.(min|map)\.(js|css)$",
        r"\.(png|jpe?g|gif|webp|ico|svg|woff2?|ttf|eot|mp4|pdf)$",
    ]
]

MAX_FILES = 40
MAX_PATCH_CHARS_PER_FILE = 12000
MAX_TOTAL_DIFF_CHARS = 90000
MAX_FINDINGS = 12
SEVERITIES = {"critical", "high", "medium", "low", "nit"}


def redact_text(text: str) -> str:
    if not text:
        return ""
    replacements = [
        (r"(?i)(authorization\s*:\s*bearer\s+)[^\s`]+", r"\1[REDACTED]"),
        (
            r"(?i)((?:api[-_ ]?key|x-api-key|token|secret|password|cookie)\s*[:=]\s*)[^\s`,;]+",
            r"\1[REDACTED]",
        ),
        (
            r"cli-proxy-api-default-auto-created-sk-[A-Za-z0-9_\-]+",
            "cli-proxy-api-default-auto-created-sk-[REDACTED]",
        ),
        (r"sk-[A-Za-z0-9_\-]{8,}", "sk-[REDACTED]"),
        (r"github_pat_[A-Za-z0-9_]+", "github_pat_[REDACTED]"),
        (r"gh[pousr]_[A-Za-z0-9_]{20,}", "ghx_[REDACTED]"),
        (r"\b[A-Za-z0-9_\-]{48,}\b", "[REDACTED_LONG_TOKEN]"),
    ]
    redacted = text
    for pattern, repl in replacements:
        redacted = re.sub(pattern, repl, redacted)
    return redacted


def compact(text: str | None, limit: int) -> str:
    text = redact_text(text or "").strip()
    if len(text) <= limit:
        return text
    return text[:limit].rsplit("\n", 1)[0] + "\n...[truncated]"


def github_request(method: str, path: str, token: str, data=None, accept: str | None = None):
    url = f"https://api.github.com{path}"
    body = None if data is None else json.dumps(data).encode()
    request = urllib.request.Request(url, data=body, method=method)
    request.add_header("Accept", accept or "application/vnd.github+json")
    request.add_header("X-GitHub-Api-Version", "2022-11-28")
    request.add_header("Authorization", f"Bearer {token}")
    if data is not None:
        request.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(request, timeout=60) as response:
            raw = response.read().decode()
            return json.loads(raw) if raw else None
    except urllib.error.HTTPError as exc:
        if exc.code in {404, 422}:
            detail = exc.read().decode(errors="replace")
            print(f"GitHub API soft-fail {method} {path}: {exc.code} {detail[:400]}", file=sys.stderr)
            return None
        detail = exc.read().decode(errors="replace")
        raise RuntimeError(f"GitHub API {method} {path} failed: {exc.code} {detail}") from exc


def load_event():
    event_path = os.environ.get("GITHUB_EVENT_PATH")
    if not event_path:
        raise RuntimeError("GITHUB_EVENT_PATH is not set")
    with open(event_path, encoding="utf-8") as handle:
        return json.load(handle)


def parse_bot_command(body: str | None):
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
        if name == "quiet" and lower_args in {"off", "false", "0", "解除", "unquiet"}:
            name = "unquiet"
        return {
            "name": name,
            "args": args,
            "raw": stripped,
            "known": name in SUPPORTED_COMMANDS,
        }
    return None


def is_bot_user(user) -> bool:
    return bool(user and user.get("type") == "Bot")


def is_maintainer(association: str | None) -> bool:
    return association in MAINTAINER_ASSOCIATIONS


def has_label(labels, name: str) -> bool:
    return name in {item.get("name") for item in (labels or [])}


def should_skip_path(path: str) -> bool:
    return any(pattern.search(path) for pattern in SKIP_PATH_PATTERNS)


def extract_json(text: str):
    try:
        return json.loads(text)
    except json.JSONDecodeError:
        start = text.find("{")
        end = text.rfind("}")
        if start == -1 or end == -1 or end <= start:
            raise
        return json.loads(text[start : end + 1])


def call_model(system: str, user_payload: str, max_tokens: int = 3500) -> str:
    api_key = os.environ.get("OPENAI_API_KEY")
    if not api_key:
        raise RuntimeError("OPENAI_API_KEY is not set")
    base_url = os.environ.get("OPENAI_BASE_URL", "https://relay.07230805.xyz/v1").rstrip("/")
    model = os.environ.get("OPENAI_MODEL", "grok-4.5")
    data = {
        "model": model,
        "messages": [
            {"role": "system", "content": system},
            {"role": "user", "content": user_payload},
        ],
        "temperature": 0.1,
        "max_tokens": max_tokens,
    }
    request = urllib.request.Request(
        f"{base_url}/chat/completions",
        data=json.dumps(data).encode(),
        method="POST",
    )
    request.add_header("Content-Type", "application/json")
    request.add_header("Authorization", f"Bearer {api_key}")
    with urllib.request.urlopen(request, timeout=180) as response:
        result = json.loads(response.read().decode())
    return result["choices"][0]["message"]["content"]


def get_pr_number(event, event_name: str) -> int:
    if event_name == "workflow_dispatch":
        number = os.environ.get("REVIEW_PR_NUMBER") or event.get("inputs", {}).get("pr_number")
        if not number:
            raise RuntimeError("workflow_dispatch requires pr_number")
        return int(number)
    if event_name in {"pull_request", "pull_request_target"}:
        return int(event["pull_request"]["number"])
    if event_name == "issue_comment":
        issue = event.get("issue") or {}
        if not issue.get("pull_request"):
            raise RuntimeError("issue_comment is not on a pull request")
        return int(issue["number"])
    if event_name == "pull_request_review_comment":
        return int(event["pull_request"]["number"])
    raise RuntimeError(f"Unsupported event: {event_name}")


def fetch_pr(repo: str, number: int, token: str):
    pr = github_request("GET", f"/repos/{repo}/pulls/{number}", token)
    if not pr:
        raise RuntimeError(f"PR #{number} not found")
    return pr


def fetch_pr_files(repo: str, number: int, token: str):
    files = []
    page = 1
    while page <= 5:
        batch = github_request(
            "GET",
            f"/repos/{repo}/pulls/{number}/files?per_page=100&page={page}",
            token,
        )
        if not batch:
            break
        files.extend(batch)
        if len(batch) < 100:
            break
        page += 1
    return files


def fetch_issue_comments(repo: str, number: int, token: str):
    return github_request("GET", f"/repos/{repo}/issues/{number}/comments?per_page=50", token) or []


def fetch_review_comments(repo: str, number: int, token: str):
    return github_request("GET", f"/repos/{repo}/pulls/{number}/comments?per_page=50", token) or []


def ensure_quiet_label(repo: str, token: str):
    github_request(
        "POST",
        f"/repos/{repo}/labels",
        token,
        {
            "name": QUIET_LABEL,
            "description": "Do not auto-comment on this issue or pull request",
            "color": "bfdadc",
        },
    )


def add_label(repo: str, number: int, token: str, label: str):
    github_request(
        "POST",
        f"/repos/{repo}/issues/{number}/labels",
        token,
        {"labels": [label]},
    )


def remove_label(repo: str, number: int, token: str, label: str):
    github_request(
        "DELETE",
        f"/repos/{repo}/issues/{number}/labels/{urllib.parse.quote(label, safe='')}",
        token,
    )


def build_diff_payload(files: list) -> tuple[list, dict]:
    selected = []
    skipped = []
    total = 0
    for item in files:
        path = item.get("filename") or ""
        if should_skip_path(path):
            skipped.append(path)
            continue
        patch = item.get("patch") or ""
        if not patch and item.get("status") in {"removed", "renamed"}:
            patch = f"(no patch body; status={item.get('status')})"
        if not patch:
            skipped.append(path)
            continue
        if len(patch) > MAX_PATCH_CHARS_PER_FILE:
            patch = patch[:MAX_PATCH_CHARS_PER_FILE] + "\n...[patch truncated]"
        entry = {
            "path": path,
            "status": item.get("status"),
            "additions": item.get("additions"),
            "deletions": item.get("deletions"),
            "patch": redact_text(patch),
        }
        size = len(entry["patch"])
        if len(selected) >= MAX_FILES or total + size > MAX_TOTAL_DIFF_CHARS:
            skipped.append(path)
            continue
        selected.append(entry)
        total += size
    meta = {
        "selected_files": len(selected),
        "skipped_files": len(skipped),
        "skipped_sample": skipped[:20],
        "total_patch_chars": total,
    }
    return selected, meta


def line_map_from_patch(patch: str) -> dict[int, int]:
    """Map diff position (1-based in patch lines with content) to new-file line number."""
    mapping: dict[int, int] = {}
    if not patch:
        return mapping
    new_line = 0
    position = 0
    for raw in patch.splitlines():
        if raw.startswith("@@"):
            match = re.search(r"\+(\d+)", raw)
            new_line = int(match.group(1)) - 1 if match else 0
            continue
        # GitHub review comments use position in the diff hunks (not including @@? actually including all lines after file header)
        # For pull request review comments API with "line" + "side", prefer absolute line on RIGHT.
        if raw.startswith("+") and not raw.startswith("+++"):
            new_line += 1
            position += 1
            mapping[position] = new_line
        elif raw.startswith("-") and not raw.startswith("---"):
            position += 1
        elif raw.startswith("\\"):
            position += 1
        else:
            # context line
            new_line += 1
            position += 1
            mapping[position] = new_line
    return mapping


def build_file_line_index(files: list) -> dict[str, set[int]]:
    index: dict[str, set[int]] = {}
    for item in files:
        path = item.get("filename") or ""
        patch = item.get("patch") or ""
        lines = set()
        new_line = 0
        for raw in patch.splitlines():
            if raw.startswith("@@"):
                match = re.search(r"\+(\d+)", raw)
                new_line = int(match.group(1)) - 1 if match else 0
                continue
            if raw.startswith("+") and not raw.startswith("+++"):
                new_line += 1
                lines.add(new_line)
            elif raw.startswith("-") and not raw.startswith("---"):
                continue
            elif raw.startswith("\\"):
                continue
            else:
                new_line += 1
                lines.add(new_line)
        if lines:
            index[path] = lines
    return index


def review_system_prompt(repo: str) -> str:
    return f"""
You are a senior code reviewer for the GitHub repository {repo} (CliRelay backend / codeProxy frontend ecosystem).
Return ONLY JSON. No markdown fence.

JSON shape:
{{
  "summary": "2-5 sentence overall assessment in the PR author's language when practical",
  "verdict": "approve|comment|request_changes",
  "findings": [
    {{
      "severity": "critical|high|medium|low|nit",
      "path": "relative/file/path",
      "line": 123,
      "title": "short title",
      "body": "what is wrong, why it matters, and how to fix"
    }}
  ],
  "testing": ["suggested verification steps"],
  "positives": ["optional good points"]
}}

Rules:
- Focus on real defects: correctness bugs, security, authz, data loss, race conditions, API contract breaks, missing tests for risky changes, resource leaks.
- Prefer fewer high-signal findings over long style lectures. Max {MAX_FINDINGS} findings.
- Every finding must cite a path that appears in the provided diff. line must be a changed/context line on the new file when possible; use 0 if unknown.
- Do not invent files, symbols, or behavior not supported by the diff.
- Never request or repeat secrets, tokens, cookies, private keys, or long credentials.
- No emoji. Do not say you are an AI.
- Use Chinese if the PR title/body is mainly Chinese; otherwise English.
- verdict=request_changes only for critical/high correctness or security issues; otherwise comment or approve.
- If the diff is docs-only or trivially safe, say so and approve with zero or nit findings.
- Ignore lockfile noise and pure formatting unless it hides a functional change.
""".strip()


def build_review_user_payload(repo: str, pr: dict, files_payload: list, meta: dict, trigger: str) -> str:
    payload = {
        "repo": repo,
        "trigger": trigger,
        "pull_request": {
            "number": pr.get("number"),
            "title": compact(pr.get("title"), 500),
            "body": compact(pr.get("body"), 6000),
            "author": (pr.get("user") or {}).get("login"),
            "base": (pr.get("base") or {}).get("ref"),
            "head": (pr.get("head") or {}).get("ref"),
            "draft": pr.get("draft"),
            "additions": pr.get("additions"),
            "deletions": pr.get("deletions"),
            "changed_files": pr.get("changed_files"),
        },
        "diff_meta": meta,
        "files": files_payload,
    }
    return json.dumps(payload, ensure_ascii=False, indent=2)


def normalize_review(raw, line_index: dict[str, set[int]]) -> dict:
    if not isinstance(raw, dict):
        raw = {}
    summary = redact_text(str(raw.get("summary") or "").strip())
    verdict = str(raw.get("verdict") or "comment").strip().lower()
    if verdict not in {"approve", "comment", "request_changes"}:
        verdict = "comment"

    findings = []
    raw_findings = raw.get("findings") if isinstance(raw.get("findings"), list) else []
    for item in raw_findings:
        if not isinstance(item, dict):
            continue
        path = str(item.get("path") or "").strip()
        if not path:
            continue
        severity = str(item.get("severity") or "medium").strip().lower()
        if severity not in SEVERITIES:
            severity = "medium"
        try:
            line = int(item.get("line") or 0)
        except (TypeError, ValueError):
            line = 0
        if path in line_index and line not in line_index[path]:
            # Keep comment but drop invalid line so it stays in the summary body.
            line = 0
        title = redact_text(str(item.get("title") or severity).strip())[:120]
        body = redact_text(str(item.get("body") or "").strip())
        if not body:
            continue
        findings.append(
            {
                "severity": severity,
                "path": path,
                "line": line,
                "title": title,
                "body": body[:2000],
            }
        )
        if len(findings) >= MAX_FINDINGS:
            break

    # Escalate verdict if needed.
    if any(f["severity"] in {"critical", "high"} for f in findings) and verdict == "approve":
        verdict = "request_changes"

    testing = []
    if isinstance(raw.get("testing"), list):
        testing = [redact_text(str(x).strip()) for x in raw["testing"] if str(x).strip()][:8]
    positives = []
    if isinstance(raw.get("positives"), list):
        positives = [redact_text(str(x).strip()) for x in raw["positives"] if str(x).strip()][:6]

    if not summary:
        if findings:
            summary = f"Found {len(findings)} review item(s) that deserve attention before merge."
        else:
            summary = "No high-signal issues found in the provided diff."

    return {
        "summary": summary,
        "verdict": verdict,
        "findings": findings,
        "testing": testing,
        "positives": positives,
    }


def format_review_body(review: dict, pr: dict, meta: dict) -> str:
    zh = bool(re.search(r"[\u4e00-\u9fff]", f"{pr.get('title','')}{pr.get('body','')}{review['summary']}"))
    verdict_map = {
        "approve": "可以合并 / approve" if zh else "approve",
        "comment": "建议讨论 / comment" if zh else "comment",
        "request_changes": "建议修改后再合 / request changes" if zh else "request changes",
    }
    lines = [
        COMMENT_MARKER,
        f"## PR Review Agent",
        "",
        f"**Verdict:** {verdict_map.get(review['verdict'], review['verdict'])}",
        "",
        review["summary"],
        "",
    ]
    if review["findings"]:
        lines.append("### Findings" if not zh else "### 问题列表")
        lines.append("")
        for idx, finding in enumerate(review["findings"], 1):
            loc = finding["path"]
            if finding["line"]:
                loc = f"{finding['path']}:{finding['line']}"
            lines.append(f"{idx}. **[{finding['severity']}] {finding['title']}** (`{loc}`)")
            lines.append(f"   {finding['body']}")
            lines.append("")
    else:
        lines.append("No blocking findings." if not zh else "未发现阻塞性问题。")
        lines.append("")

    if review["positives"]:
        lines.append("### Positives" if not zh else "### 优点")
        lines.append("")
        for item in review["positives"]:
            lines.append(f"- {item}")
        lines.append("")

    if review["testing"]:
        lines.append("### Suggested verification" if not zh else "### 建议验证")
        lines.append("")
        for item in review["testing"]:
            lines.append(f"- {item}")
        lines.append("")

    lines.append("---")
    lines.append(
        f"_Diff coverage: {meta['selected_files']} file(s) reviewed"
        f"{', ' + str(meta['skipped_files']) + ' skipped' if meta.get('skipped_files') else ''}."
        f" Re-run with `/bot review`._"
    )
    return "\n".join(lines).strip() + "\n"


def event_review_event(verdict: str) -> str:
    # Prefer COMMENT for automation safety; REQUEST_CHANGES can block if required reviewers are strict.
    # Still surface request_changes as text in the body.
    if verdict == "approve":
        return "COMMENT"
    return "COMMENT"


def post_pull_review(repo: str, pr: dict, token: str, body: str, review: dict, line_index: dict[str, set[int]]):
    comments = []
    for finding in review["findings"]:
        path = finding["path"]
        line = finding["line"]
        if not path or not line or path not in line_index or line not in line_index[path]:
            continue
        comments.append(
            {
                "path": path,
                "line": line,
                "side": "RIGHT",
                "body": f"**[{finding['severity']}] {finding['title']}**\n\n{finding['body']}",
            }
        )
        if len(comments) >= 8:
            break

    payload = {
        "commit_id": (pr.get("head") or {}).get("sha"),
        "body": body,
        "event": event_review_event(review["verdict"]),
    }
    if comments:
        payload["comments"] = comments

    result = github_request("POST", f"/repos/{repo}/pulls/{pr['number']}/reviews", token, payload)
    if result is None and comments:
        # Retry without inline comments if line mapping failed server-side.
        payload.pop("comments", None)
        result = github_request("POST", f"/repos/{repo}/pulls/{pr['number']}/reviews", token, payload)
    if result is None:
        # Last resort: plain issue comment.
        github_request(
            "POST",
            f"/repos/{repo}/issues/{pr['number']}/comments",
            token,
            {"body": body},
        )
        return "comment-fallback"
    return "review-created"


def already_reviewed_commit(comments: list, reviews: list, head_sha: str) -> bool:
    marker = f"<!-- pr-review-agent:sha:{head_sha} -->"
    for item in comments:
        if marker in (item.get("body") or ""):
            return True
    for item in reviews or []:
        body = item.get("body") or ""
        if marker in body or (COMMENT_MARKER in body and item.get("commit_id") == head_sha):
            # If same commit already has our review body, skip.
            if COMMENT_MARKER in body:
                return True
    return False


def list_reviews(repo: str, number: int, token: str):
    return github_request("GET", f"/repos/{repo}/pulls/{number}/reviews?per_page=50", token) or []


def format_help() -> str:
    return f"""{COMMENT_MARKER}
我是 CliProxy 的 PR 审查助手。

可用指令：
- `/bot help`：显示说明
- `/bot review`：基于最新 diff 重新审查并发布详细报告（PR 作者或维护者）
- `/bot quiet`：本 PR 静默，不再自动审查（维护者）
- `/bot unquiet`：解除静默（维护者）

打开/更新 PR 时会自动审查一次（同一 head commit 不重复）。
"""


def command_allowed(command: dict, association: str | None, pr: dict, actor_login: str | None) -> bool:
    if command["name"] in PUBLIC_COMMANDS:
        if command["name"] == "review":
            author = ((pr.get("user") or {}).get("login") or "").lower()
            actor = (actor_login or "").lower()
            return is_maintainer(association) or (author and actor and author == actor)
        return True
    return is_maintainer(association)


def run_review(repo: str, pr: dict, token: str, trigger: str, force: bool = False) -> str:
    labels = pr.get("labels") or []
    # pulls API may not include labels; fetch issue for labels if missing
    if not labels:
        issue = github_request("GET", f"/repos/{repo}/issues/{pr['number']}", token) or {}
        labels = issue.get("labels") or []
    if has_label(labels, QUIET_LABEL) and not force:
        print("review=skipped-quiet")
        return "skipped-quiet"

    head_sha = (pr.get("head") or {}).get("sha") or ""
    comments = fetch_issue_comments(repo, pr["number"], token)
    reviews = list_reviews(repo, pr["number"], token)
    if not force and head_sha and already_reviewed_commit(comments, reviews, head_sha):
        print("review=skipped-same-sha")
        return "skipped-same-sha"

    files = fetch_pr_files(repo, pr["number"], token)
    files_payload, meta = build_diff_payload(files)
    line_index = build_file_line_index(files)

    if not files_payload:
        body = (
            f"{COMMENT_MARKER}\n<!-- pr-review-agent:sha:{head_sha} -->\n"
            "没有可审查的文本 diff（可能全是二进制/lockfile/生成物）。请人工确认。"
        )
        github_request(
            "POST",
            f"/repos/{repo}/issues/{pr['number']}/comments",
            token,
            {"body": body},
        )
        return "no-diff"

    system = review_system_prompt(repo)
    user_payload = build_review_user_payload(repo, pr, files_payload, meta, trigger)
    try:
        content = call_model(system, user_payload)
        raw = extract_json(content)
    except Exception as exc:
        print(f"model failure, fallback summary: {exc}", file=sys.stderr)
        raw = {
            "summary": f"自动审查调用模型失败，请维护者人工查看 diff（{meta['selected_files']} files）。",
            "verdict": "comment",
            "findings": [],
            "testing": [],
            "positives": [],
        }

    review = normalize_review(raw, line_index)
    body = format_review_body(review, pr, meta)
    # Embed sha marker for dedupe.
    body = body.replace(COMMENT_MARKER, f"{COMMENT_MARKER}\n<!-- pr-review-agent:sha:{head_sha} -->", 1)
    result = post_pull_review(repo, pr, token, body, review, line_index)
    print(f"review={result} findings={len(review['findings'])} verdict={review['verdict']}")
    return result


def run_comment_command(repo: str, event: dict, token: str):
    comment = event.get("comment") or {}
    if is_bot_user(comment.get("user")):
        print("comment=skipped-bot")
        return
    command = parse_bot_command(comment.get("body"))
    if not command:
        print("comment=skipped-no-command")
        return

    pr_number = get_pr_number(event, "issue_comment")
    pr = fetch_pr(repo, pr_number, token)
    association = comment.get("author_association")
    actor = (comment.get("user") or {}).get("login")

    if not command["known"]:
        github_request(
            "POST",
            f"/repos/{repo}/issues/{pr_number}/comments",
            token,
            {"body": f"{COMMENT_MARKER}\n未识别指令 `{redact_text(command.get('raw','/bot'))}`。\n\n" + format_help()},
        )
        return

    if not command_allowed(command, association, pr, actor):
        github_request(
            "POST",
            f"/repos/{repo}/issues/{pr_number}/comments",
            token,
            {
                "body": f"{COMMENT_MARKER}\n`/bot {command['name']}` 需要 PR 作者或仓库维护者执行。"
            },
        )
        return

    name = command["name"]
    if name == "help":
        github_request(
            "POST",
            f"/repos/{repo}/issues/{pr_number}/comments",
            token,
            {"body": format_help()},
        )
        print("comment=help")
        return
    if name == "quiet":
        ensure_quiet_label(repo, token)
        add_label(repo, pr_number, token, QUIET_LABEL)
        github_request(
            "POST",
            f"/repos/{repo}/issues/{pr_number}/comments",
            token,
            {"body": f"{COMMENT_MARKER}\n已开启静默。本 PR 不会再自动审查；需要时执行 `/bot unquiet` 或 `/bot review`。"},
        )
        print("comment=quiet")
        return
    if name == "unquiet":
        ensure_quiet_label(repo, token)
        remove_label(repo, pr_number, token, QUIET_LABEL)
        github_request(
            "POST",
            f"/repos/{repo}/issues/{pr_number}/comments",
            token,
            {"body": f"{COMMENT_MARKER}\n已解除静默。后续 PR 更新仍会自动审查。"},
        )
        print("comment=unquiet")
        return

    # review
    run_review(repo, pr, token, trigger="issue_comment:/bot review", force=True)


def run():
    token = os.environ.get("GITHUB_TOKEN")
    repo = os.environ.get("GITHUB_REPOSITORY")
    event_name = os.environ.get("GITHUB_EVENT_NAME", "")
    if not token or not repo:
        raise RuntimeError("GITHUB_TOKEN and GITHUB_REPOSITORY are required")

    event = load_event()

    if event_name == "issue_comment":
        issue = event.get("issue") or {}
        if not issue.get("pull_request"):
            print("skip non-PR issue_comment")
            return
        run_comment_command(repo, event, token)
        return

    pr_number = get_pr_number(event, event_name)
    pr = fetch_pr(repo, pr_number, token)
    if pr.get("draft") and event_name != "workflow_dispatch":
        print("review=skipped-draft")
        return

    force = event_name == "workflow_dispatch"
    run_review(repo, pr, token, trigger=event_name, force=force)


def self_test():
    assert "sk-[REDACTED]" in redact_text("key sk-abcdefghijklmnopqrstuv")
    assert parse_bot_command("/bot review")["name"] == "review"
    assert parse_bot_command("/bot quiet off")["name"] == "unquiet"
    assert parse_bot_command("普通评论") is None
    assert should_skip_path("package-lock.json")
    assert not should_skip_path("internal/api/router.go")

    patch = """@@ -1,3 +1,4 @@
 context
-old
+new
 more
"""
    files = [{"filename": "a.go", "status": "modified", "additions": 1, "deletions": 1, "patch": patch}]
    payload, meta = build_diff_payload(files)
    assert meta["selected_files"] == 1
    assert payload[0]["path"] == "a.go"
    index = build_file_line_index(files)
    assert "a.go" in index
    review = normalize_review(
        {
            "summary": "ok",
            "verdict": "approve",
            "findings": [
                {
                    "severity": "high",
                    "path": "a.go",
                    "line": 2,
                    "title": "bug",
                    "body": "fix me",
                }
            ],
        },
        index,
    )
    assert review["verdict"] == "request_changes"
    body = format_review_body(review, {"title": "test", "body": ""}, meta)
    assert COMMENT_MARKER in body
    assert "bug" in body
    print("self-test ok")


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    if args.self_test:
        self_test()
    else:
        run()
