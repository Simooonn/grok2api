#!/usr/bin/env python3
"""Host-side upgrade agent for grok2api.

The grok2api container never touches Docker or host TCP sockets. Communication
goes through a split control directory mounted into the container:
  inbox/   writable by container (uid 10001); only request.json is accepted
  outbox/  mounted read-only in container; holds agent.json and status.json

Everything else (claimed requests, backups, lockfile, logs) lives in STATE_DIR
on the host (0700) and is never mounted into the container.

Subcommands:
  run             Claim inbox/request.json once (triggered by systemd .path)
  heartbeat       Refresh outbox/agent.json (triggered by systemd .timer)
  upgrade <tag>   Run the same backup -> pull -> recreate -> healthcheck ->
                  auto-rollback flow manually from the host shell
"""

from __future__ import annotations

import datetime
import fcntl
import json
import os
import re
import secrets
import shutil
import sqlite3
import stat
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from pathlib import Path

AGENT_VERSION = 1
PROJECT_DIR = Path(os.environ.get("PROJECT_DIR", "/home/ubuntu/grok2api"))
CONTROL_DIR = Path(os.environ.get("CONTROL_DIR", str(PROJECT_DIR / "upgrade-control")))
STATE_DIR = Path(os.environ.get("STATE_DIR", str(PROJECT_DIR / ".upgrade")))
IMAGE_REPO = os.environ.get("IMAGE_REPO", "ghcr.io/simooonn/grok2api").strip()
COMPOSE_FILE = Path(os.environ.get("COMPOSE_FILE", str(PROJECT_DIR / "docker-compose.yml")))
CONTAINER_NAME = os.environ.get("CONTAINER_NAME", "grok2api").strip()
HEALTH_URL = os.environ.get("HEALTH_URL", "http://127.0.0.1:8000/healthz").strip()
HEALTH_TIMEOUT = int(os.environ.get("HEALTH_TIMEOUT", "90"))
ROLLBACK_TIMEOUT = int(os.environ.get("ROLLBACK_TIMEOUT", "60"))
BACKUP_KEEP = int(os.environ.get("BACKUP_KEEP", "10"))

INBOX_DIR = CONTROL_DIR / "inbox"
OUTBOX_DIR = CONTROL_DIR / "outbox"
AGENT_DIR = STATE_DIR / "agent"
BACKUP_DIR = STATE_DIR / "backups"
REQUEST_FILE = INBOX_DIR / "request.json"
STATUS_FILE = OUTBOX_DIR / "status.json"
AGENT_FILE = OUTBOX_DIR / "agent.json"
LOCK_FILE = AGENT_DIR / "agent.lock"
LOG_FILE = AGENT_DIR / "last-run.log"

# Must stay identical to ForkTagPattern in backend/internal/application/upgrade/service.go.
TAG_RE = re.compile(r"^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?-roamai\.[1-9][0-9]*$")
ID_RE = re.compile(r"^[0-9a-f]{16}$")
IMAGE_LINE_RE = re.compile(r'^(\s*image:\s*)(["\']?)(\S+?)\2(\s*(?:#.*)?)$')


def now_iso() -> str:
    return datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def ensure_dirs() -> None:
    INBOX_DIR.mkdir(parents=True, exist_ok=True)
    OUTBOX_DIR.mkdir(parents=True, exist_ok=True)
    AGENT_DIR.mkdir(parents=True, exist_ok=True)
    BACKUP_DIR.mkdir(parents=True, exist_ok=True)
    os.chmod(STATE_DIR, 0o700)
    os.chmod(AGENT_DIR, 0o700)
    os.chmod(BACKUP_DIR, 0o700)


def write_json_atomic(path: Path, payload: dict) -> None:
    """Write JSON via a temp file in host-only AGENT_DIR then atomically replace target."""
    AGENT_DIR.mkdir(parents=True, exist_ok=True)
    fd, tmp_name = tempfile.mkstemp(dir=str(AGENT_DIR), prefix=".tmp-")
    try:
        os.fchmod(fd, 0o644)
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            json.dump(payload, fh, ensure_ascii=False)
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp_name, str(path))
    except BaseException:
        try:
            os.unlink(tmp_name)
        except FileNotFoundError:
            pass
        raise


def log_line(msg: str) -> None:
    line = f"[{now_iso()}] {msg}"
    print(line, flush=True)
    try:
        with open(LOG_FILE, "a", encoding="utf-8") as fh:
            fh.write(line + "\n")
    except OSError:
        pass


def parse_compose_image(compose_path: Path) -> str:
    lines = compose_path.read_text(encoding="utf-8").splitlines()
    in_service = False
    service_indent = 0
    for line in lines:
        stripped = line.lstrip(" ")
        indent = len(line) - len(stripped)
        if not in_service:
            if stripped == f"{CONTAINER_NAME}:" or stripped.startswith(f"{CONTAINER_NAME}: "):
                in_service = True
                service_indent = indent
            continue
        if stripped and not stripped.startswith("#") and indent <= service_indent:
            break
        match = IMAGE_LINE_RE.match(line)
        if match:
            return match.group(3)
    raise RuntimeError(f"image line for service {CONTAINER_NAME!r} not found in {compose_path}")


def rewrite_compose_image(compose_path: Path, new_image: str) -> str:
    """Atomically rewrite the grok2api service image line; return previous image ref."""
    raw_lines = compose_path.read_text(encoding="utf-8").splitlines(keepends=True)
    in_service = False
    service_indent = 0
    previous = ""
    replaced = False

    for idx, raw in enumerate(raw_lines):
        line = raw.rstrip("\r\n")
        eol = raw[len(line):] or "\n"
        stripped = line.lstrip(" ")
        indent = len(line) - len(stripped)
        if not in_service:
            if stripped == f"{CONTAINER_NAME}:" or stripped.startswith(f"{CONTAINER_NAME}: "):
                in_service = True
                service_indent = indent
            continue
        if stripped and not stripped.startswith("#") and indent <= service_indent:
            break
        match = IMAGE_LINE_RE.match(line)
        if match:
            previous = match.group(3)
            raw_lines[idx] = f"{match.group(1)}{match.group(2)}{new_image}{match.group(2)}{match.group(4)}{eol}"
            replaced = True
            break

    if not replaced:
        raise RuntimeError(f"image line for service {CONTAINER_NAME!r} not found in {compose_path}")

    st = os.stat(compose_path)
    fd, tmp_name = tempfile.mkstemp(dir=str(compose_path.parent), prefix=".compose-tmp-")
    try:
        os.fchmod(fd, stat.S_IMODE(st.st_mode))
        with os.fdopen(fd, "w", encoding="utf-8") as fh:
            fh.writelines(raw_lines)
            fh.flush()
            os.fsync(fh.fileno())
        os.replace(tmp_name, str(compose_path))
    except BaseException:
        try:
            os.unlink(tmp_name)
        except FileNotFoundError:
            pass
        raise
    return previous


def extract_tag_from_image(image_ref: str) -> str:
    if "@" in image_ref:
        image_ref = image_ref.split("@", 1)[0]
    last_slash = image_ref.rfind("/")
    colon = image_ref.rfind(":")
    if colon > last_slash:
        return image_ref[colon + 1 :]
    return ""


def run_cmd(args: list[str], timeout: int) -> str:
    proc = subprocess.run(
        args,
        cwd=str(PROJECT_DIR),
        capture_output=True,
        text=True,
        timeout=timeout,
    )
    if proc.returncode != 0:
        detail = (proc.stderr or proc.stdout).strip()[-1500:]
        raise RuntimeError(f"{' '.join(args)} failed (rc={proc.returncode}): {detail}")
    return proc.stdout


def discover_sqlite_path() -> Path | None:
    """Locate backend.db inside the grok2api /app/data mount."""
    try:
        out = run_cmd(
            [
                "docker",
                "inspect",
                "-f",
                '{{range .Mounts}}{{if eq .Destination "/app/data"}}{{.Source}}{{end}}{{end}}',
                CONTAINER_NAME,
            ],
            timeout=15,
        ).strip()
        if out:
            candidate = Path(out) / "backend.db"
            if candidate.is_file():
                return candidate
    except Exception:
        pass
    try:
        out = run_cmd(
            ["docker", "volume", "inspect", "-f", "{{.Mountpoint}}", f"{CONTAINER_NAME}_{CONTAINER_NAME}-data"],
            timeout=15,
        ).strip()
        if out:
            candidate = Path(out) / "backend.db"
            if candidate.is_file():
                return candidate
    except Exception:
        pass
    return None


def backup_sqlite(from_tag: str, db_path: Path | None = None) -> Path | None:
    src = db_path if db_path is not None else discover_sqlite_path()
    if src is None or not src.is_file():
        log_line("sqlite backup skipped: backend.db not found")
        return None
    safe_tag = re.sub(r"[^0-9A-Za-z._-]", "_", from_tag or "unknown")[:48]
    ts = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    dst = BACKUP_DIR / f"backend-{ts}-{safe_tag}.db"
    src_conn = sqlite3.connect(f"file:{src}?mode=ro", uri=True, timeout=30.0)
    try:
        dst_conn = sqlite3.connect(str(dst))
        try:
            src_conn.backup(dst_conn)
        finally:
            dst_conn.close()
    finally:
        src_conn.close()
    os.chmod(dst, 0o600)

    # Prune old backups, keeping newest BACKUP_KEEP files.
    backups = sorted(BACKUP_DIR.glob("backend-*.db"), key=lambda p: p.stat().st_mtime, reverse=True)
    for stale in backups[BACKUP_KEEP:]:
        try:
            stale.unlink()
        except OSError:
            pass
    log_line(f"sqlite backed up to {dst} ({dst.stat().st_size} bytes)")
    return dst


def check_health_once() -> tuple[str, bool]:
    """Return (state_description, is_healthy)."""
    try:
        out = run_cmd(
            [
                "docker",
                "inspect",
                "-f",
                "{{.State.Status}}|{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}|{{.RestartCount}}",
                CONTAINER_NAME,
            ],
            timeout=10,
        ).strip()
    except Exception as exc:
        return (f"inspect error: {exc}", False)

    parts = out.split("|")
    if len(parts) != 3:
        return (f"unexpected inspect output: {out}", False)
    status, health, restarts = parts
    if status in ("exited", "dead"):
        return (f"container {status} (restarts={restarts})", False)
    if status != "running":
        return (f"container {status}", False)
    if health not in ("healthy", "none"):
        return (f"docker health={health}", False)

    try:
        req = urllib.request.Request(HEALTH_URL, method="GET")
        with urllib.request.urlopen(req, timeout=5) as resp:
            if resp.status == 200:
                return ("healthy", True)
            return (f"healthz HTTP {resp.status}", False)
    except (urllib.error.URLError, OSError) as exc:
        return (f"healthz error: {exc}", False)


def wait_healthy(timeout_sec: int, poll_interval: float = 2.0) -> tuple[bool, str]:
    deadline = time.monotonic() + timeout_sec
    last_state = "starting"
    while time.monotonic() < deadline:
        state, ok = check_health_once()
        last_state = state
        if ok:
            return True, "healthy"
        if state.startswith("container exited") or state.startswith("container dead"):
            return False, state
        time.sleep(poll_interval)
    return False, f"timed out after {timeout_sec}s (last: {last_state})"


def claim_request() -> dict | None:
    """Atomically move inbox/request.json to host-only AGENT_DIR without following symlinks."""
    inbox_fd = os.open(str(INBOX_DIR), os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    claimed = AGENT_DIR / f"request-{os.getpid()}.json"
    try:
        try:
            os.rename("request.json", str(claimed), src_dir_fd=inbox_fd)
        except FileNotFoundError:
            return None
    finally:
        os.close(inbox_fd)

    def discard(path: Path) -> None:
        try:
            st = os.lstat(path)
            if stat.S_ISDIR(st.st_mode) and not stat.S_ISLNK(st.st_mode):
                shutil.rmtree(path, ignore_errors=True)
            else:
                os.unlink(path)
        except FileNotFoundError:
            pass

    raw = b""
    try:
        info = os.lstat(claimed)
        if stat.S_ISREG(info.st_mode):
            fd = os.open(str(claimed), os.O_RDONLY | os.O_NOFOLLOW)
            try:
                if stat.S_ISREG(os.fstat(fd).st_mode):
                    raw = os.read(fd, 4097)
            finally:
                os.close(fd)
    finally:
        discard(claimed)

    if not raw or len(raw) > 4096:
        return {}
    try:
        data = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError):
        return {}
    return data if isinstance(data, dict) else {}


def clean_field(data: dict, name: str, max_len: int = 80) -> str:
    val = data.get(name, "")
    if isinstance(val, bool) or not isinstance(val, (str, int)):
        return ""
    return str(val).replace("\t", " ").replace("\n", " ").replace("\r", " ").strip()[:max_len]


def cmd_heartbeat() -> None:
    ensure_dirs()
    current_tag = ""
    try:
        current_tag = extract_tag_from_image(parse_compose_image(COMPOSE_FILE))
    except Exception:
        pass
    write_json_atomic(
        AGENT_FILE,
        {
            "version": AGENT_VERSION,
            "image_repo": IMAGE_REPO,
            "current_tag": current_tag,
            "updated_at": now_iso(),
        },
    )


def execute_upgrade(req_id: str, tag: str, from_version: str, requested_by: str) -> int:
    def set_status(phase: str, message: str) -> None:
        log_line(f"phase={phase} tag={tag} msg={message}")
        try:
            write_json_atomic(
                STATUS_FILE,
                {
                    "id": req_id,
                    "tag": tag,
                    "phase": phase,
                    "message": message,
                    "from_version": from_version,
                    "requested_by": requested_by,
                    "updated_at": now_iso(),
                },
            )
        except Exception as exc:
            print(f"warning: could not write status.json: {exc}", file=sys.stderr)

    # Reset run log.
    try:
        LOG_FILE.write_text("", encoding="utf-8")
    except OSError:
        pass

    if not ID_RE.match(req_id) or not TAG_RE.match(tag):
        set_status("failed", f"拒绝执行：非法版本号 {tag!r}（仅允许 vX.Y.Z-roamai.N）")
        return 1

    try:
        previous_image = parse_compose_image(COMPOSE_FILE)
    except Exception as exc:
        set_status("failed", f"读取 docker-compose.yml 失败：{exc}")
        return 1

    previous_tag = extract_tag_from_image(previous_image)
    if not from_version:
        from_version = previous_tag

    target_image = f"{IMAGE_REPO}:{tag}"
    if previous_image == target_image:
        ok, _ = check_health_once()
        if ok:
            set_status("succeeded", f"当前已运行 {tag}，无需重复切换")
            cmd_heartbeat()
            return 0

    set_status("backing_up", f"正在备份 SQLite 数据库（变更前版本 {from_version}）")
    try:
        backup_sqlite(from_version)
    except Exception as exc:
        set_status("failed", f"数据库备份失败，已中止升级：{exc}")
        return 1

    set_status("pulling", f"正在拉取镜像 {target_image}")
    try:
        run_cmd(["docker", "pull", target_image], timeout=600)
    except Exception as exc:
        set_status("failed", f"拉取镜像失败（当前服务未受影响）：{exc}")
        return 1

    set_status("recreating", f"正在切换 compose 到 {tag} 并重建容器")
    try:
        rewrite_compose_image(COMPOSE_FILE, target_image)
        run_cmd(["docker", "compose", "up", "-d"], timeout=300)
    except Exception as exc:
        set_status("rolling_back", f"容器重建失败，正在自动回滚到 {previous_tag or previous_image}")
        rb_ok, rb_detail = perform_rollback(previous_image)
        if rb_ok:
            set_status("failed", f"重建失败（{exc}），已自动回滚到 {previous_tag or previous_image} 且健康")
        else:
            set_status("failed", f"重建失败（{exc}），自动回滚亦异常（{rb_detail}），需人工介入")
        cmd_heartbeat()
        return 1

    set_status("health_check", f"等待新容器 {tag} 通过健康检查（最多 {HEALTH_TIMEOUT}s）")
    healthy, detail = wait_healthy(HEALTH_TIMEOUT)
    if healthy:
        set_status("succeeded", f"已成功切换到 {tag} 并通过健康检查")
        cmd_heartbeat()
        return 0

    set_status("rolling_back", f"新版本健康检查失败（{detail}），正在自动回滚到 {previous_tag or previous_image}")
    rb_ok, rb_detail = perform_rollback(previous_image)
    if rb_ok:
        set_status("failed", f"新版本 {tag} 健康检查失败（{detail}），已自动回滚到 {previous_tag or previous_image} 且健康")
    else:
        set_status("failed", f"新版本 {tag} 健康检查失败（{detail}），自动回滚亦异常（{rb_detail}），需人工介入")
    cmd_heartbeat()
    return 1


def perform_rollback(previous_image: str) -> tuple[bool, str]:
    try:
        rewrite_compose_image(COMPOSE_FILE, previous_image)
        run_cmd(["docker", "compose", "up", "-d"], timeout=300)
        return wait_healthy(ROLLBACK_TIMEOUT)
    except Exception as exc:
        return False, str(exc)


def with_lock(fn):
    ensure_dirs()
    lock_fd = os.open(str(LOCK_FILE), os.O_CREAT | os.O_RDWR, 0o600)
    try:
        fcntl.flock(lock_fd, fcntl.LOCK_EX)
        return fn()
    finally:
        fcntl.flock(lock_fd, fcntl.LOCK_UN)
        os.close(lock_fd)


def cmd_run() -> int:
    def _inner() -> int:
        if not REQUEST_FILE.exists() and not REQUEST_FILE.is_symlink():
            return 0
        cmd_heartbeat()
        data = claim_request()
        if data is None:
            return 0
        req_id = clean_field(data, "id", 32)
        tag = clean_field(data, "tag", 80)
        from_version = clean_field(data, "from_version", 64)
        requested_by = clean_field(data, "requested_by", 64) or "admin"
        if not ID_RE.match(req_id):
            req_id = secrets.token_hex(8)
        return execute_upgrade(req_id, tag, from_version, requested_by)

    return with_lock(_inner)


def cmd_cli_upgrade(tag: str) -> int:
    def _inner() -> int:
        cmd_heartbeat()
        return execute_upgrade(secrets.token_hex(8), tag.strip(), "", os.environ.get("USER", "cli"))

    return with_lock(_inner)


def main(argv: list[str]) -> int:
    if len(argv) == 2 and argv[1] == "heartbeat":
        cmd_heartbeat()
        return 0
    if len(argv) == 2 and argv[1] == "run":
        return cmd_run()
    if len(argv) == 3 and argv[1] == "upgrade":
        return cmd_cli_upgrade(argv[2])
    print("Usage: grok2api-upgrade-agent.py {run|heartbeat|upgrade <tag>}", file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv))
