"""
Git workspace resilience and isolated worktree management.
"""

import os
import shutil
import subprocess
import json
import shlex
from pathlib import Path
from typing import Optional, Dict, Any, List, Tuple, Union
from makewand.config import c, COLOR_YELLOW, COLOR_RED

SAFE_GIT_SECURITY_FLAGS = [
    "-c", "diff.tool=",
    "-c", "core.fsmonitor=",
    "-c", "core.hooksPath=/dev/null",
    "-c", "core.attributesFile=/dev/null",
    "-c", "core.pager=cat",
    "-c", "commit.gpgsign=false",
]

def _get_git_info_attributes_path(cwd: Optional[Union[str, Path]]) -> Optional[Path]:
    try:
        p = Path(cwd).resolve() if cwd else Path.cwd().resolve()
        for cur in [p] + list(p.parents):
            gp = cur / ".git"
            if gp.is_dir():
                ia = gp / "info" / "attributes"
                if ia.exists():
                    return ia
                break
            elif gp.is_file():
                try:
                    txt = gp.read_text(encoding="utf-8").strip()
                    if txt.startswith("gitdir:"):
                        gd = Path(txt[7:].strip())
                        if not gd.is_absolute():
                            gd = (gp.parent / gd).resolve()
                        ia = gd / "info" / "attributes"
                        if ia.exists():
                            return ia
                except Exception:
                    pass
                break
    except Exception:
        pass
    return None

def run_git_cmd(cmd, cwd=None, input_data=None, binary=False, safe=True):
    shielded_info = None
    try:
        is_bytes = isinstance(input_data, bytes) or binary
        if safe and isinstance(cmd, str) and "&&" in cmd:
            subcmds = [s.strip() for s in cmd.split("&&") if s.strip()]
            last_rc, last_out, last_err = 0, "" if not is_bytes else b"", "" if not is_bytes else b""
            for sc in subcmds:
                last_rc, last_out, last_err = run_git_cmd(sc, cwd=cwd, input_data=input_data, binary=binary, safe=safe)
                if last_rc != 0:
                    return last_rc, last_out, last_err
            return last_rc, last_out, last_err

        use_shell = False
        if isinstance(cmd, str):
            if safe:
                if any(op in cmd for op in ["||", ";", "|", "`", "$("]):
                    raise ValueError(f"Unsafe shell metacharacter detected in git command: {cmd}")
                cmd = shlex.split(cmd)
            else:
                if any(op in cmd for op in ["&&", "||", ";", "|", "`", "$("]):
                    exec_cmd = cmd
                    use_shell = True
                else:
                    cmd = shlex.split(cmd)

        if isinstance(cmd, list) and len(cmd) > 0 and cmd[0] == "git" and safe:
            subcmd = cmd[1] if len(cmd) > 1 else ""
            extra_global = ["--no-pager"]
            if subcmd not in ["apply", "clone"]:
                extra_global.append("--attr-source=4b825dc642cb6eb9a060e54bf8d69288fbee4904")
            exec_cmd = [cmd[0]] + extra_global + SAFE_GIT_SECURITY_FLAGS + cmd[1:]
            if subcmd == "diff":
                diff_idx = exec_cmd.index("diff")
                if "--no-ext-diff" not in exec_cmd:
                    exec_cmd.insert(diff_idx + 1, "--no-ext-diff")
                if "--no-textconv" not in exec_cmd:
                    exec_cmd.insert(diff_idx + 2, "--no-textconv")
            use_shell = False
        elif not use_shell:
            exec_cmd = cmd
            use_shell = False

        git_env = os.environ.copy()
        if safe:
            for k in list(git_env.keys()):
                if k in ("GIT_EXTERNAL_DIFF", "GIT_DIFF_OPTS", "GIT_PAGER", "GIT_SSH", "GIT_SSH_COMMAND", "GIT_ASKPASS") or k.startswith("GIT_CONFIG_"):
                    git_env.pop(k, None)

            # S01: Temporarily shield .git/info/attributes to neutralize host clean/smudge execution
            ia_target = _get_git_info_attributes_path(cwd)
            if ia_target and ia_target.exists():
                try:
                    shield_file = ia_target.parent / (ia_target.name + ".makewand_shield")
                    ia_target.rename(shield_file)
                    shielded_info = (ia_target, shield_file)
                except Exception:
                    pass

        res = subprocess.run(
            exec_cmd,
            input=input_data,
            shell=use_shell,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=not is_bytes,
            cwd=cwd,
            timeout=30,
            env=git_env
        )
        return res.returncode, res.stdout, res.stderr
    except Exception as e:
        empty = b"" if (isinstance(input_data, bytes) or binary) else ""
        return -1, empty, str(e)
    finally:
        if shielded_info:
            try:
                orig_ia, shield_ia = shielded_info
                if shield_ia.exists():
                    shield_ia.rename(orig_ia)
            except Exception:
                pass

def find_git_root(path: Union[str, Path]) -> Optional[str]:
    """
    Traverses upward from path to find the enclosing git repository root.
    """
    if not path:
        return None
    try:
        curr = Path(path).resolve()
        while curr != curr.parent:
            if (curr / ".git").exists():
                return str(curr)
            curr = curr.parent
    except Exception:
        pass
    return None

def ensure_git_worktree(cwd: str) -> bool:
    """
    Ensures cwd is inside a git repository.
    If not, automatically initializes a lightweight shadow git tracking tree
    so diff extraction and red-team audits work seamlessly without manual git init.
    Protects root system directories (/, /tmp, ~) from accidental init.
    """
    if not cwd:
        cwd = os.getcwd()
    resolved = Path(cwd).resolve()
    if resolved in [Path("/"), Path("/tmp"), Path.home()]:
        return False

    code, _, _ = run_git_cmd("git rev-parse --is-inside-work-tree", cwd=cwd)
    if code != 0:
        import sys
        print(c("[Makewand Git] 检测到当前目录尚未初始化 Git，自动建立影子 Git 跟踪树...", COLOR_YELLOW), file=sys.stderr)
        run_git_cmd(["git", "init"], cwd=cwd)
        run_git_cmd(["git", "config", "user.name", "Makewand"], cwd=cwd)
        run_git_cmd(["git", "config", "user.email", "makewand@local"], cwd=cwd)
        run_git_cmd(["git", "add", "-A"], cwd=cwd)
        run_git_cmd(["git", "commit", "-m", "Makewand baseline snapshot", "--allow-empty"], cwd=cwd)
        return True
    return False

def get_submodule_paths(repo_dir: str) -> List[str]:
    """
    Returns an ordered list of submodule relative paths, sorted descending by path depth
    (deepest submodules first). Correctly handles submodule paths containing spaces.
    """
    r_path = Path(repo_dir)
    gitmodules = r_path / ".gitmodules"
    if not gitmodules.exists():
        return []

    paths = []
    # 1. Read .gitmodules paths via git config (preserves spaces in path values)
    code, out, _ = run_git_cmd(["git", "config", "--file", ".gitmodules", "--get-regexp", r"^submodule\..*\.path$"], cwd=str(r_path))
    if code == 0 and out:
        for line in out.splitlines():
            line = line.strip()
            if not line:
                continue
            parts = line.split(None, 1)
            if len(parts) == 2:
                paths.append(parts[1].strip())

    # 2. Also parse git submodule status --recursive with regex to capture nested submodules
    st_code, st_out, _ = run_git_cmd(["git", "submodule", "status", "--recursive"], cwd=str(r_path))
    if st_code == 0 and st_out:
        import re
        for line in st_out.splitlines():
            m = re.match(r"^[-+ U]?[0-9a-fA-F]+\s+(.*?)(?:\s+\([^\)]*\))?$", line)
            if m:
                paths.append(m.group(1).strip())

    # Sort descending by directory depth so nested submodules appear first
    return sorted(list(dict.fromkeys(paths)), key=lambda s: len(Path(s).parts), reverse=True)

def get_git_diff_status(cwd: str, base_rev: Optional[str] = None, sub_baselines: Optional[Dict[str, str]] = None) -> Tuple[str, Optional[str]]:
    """
    Extracts git diff for the workspace, including newly added, modified, and deleted files.
    Returns (diff: str, error: Optional[str]).
    Enforces fail-closed security: if git command errors (e.g. invalid repo, exit 128),
    returns the explicit error instead of disguising as an empty diff.
    """
    if not cwd:
        cwd = os.getcwd()

    chk_code, _, chk_err = run_git_cmd(["git", "rev-parse", "--is-inside-work-tree"], cwd=cwd)
    if chk_code != 0:
        return "", f"Not inside a valid git working tree ({chk_err.strip()})"

    run_git_cmd(["git", "add", "-A", "--intent-to-add"], cwd=cwd)
    ref = base_rev if base_rev else "HEAD"
    code, diff_out, err = run_git_cmd(["git", "diff", ref], cwd=cwd)
    if code != 0 and not base_rev:
        code, diff_out, err = run_git_cmd(["git", "diff"], cwd=cwd)

    if code != 0:
        return "", f"git diff failed with exit code {code}: {err.strip()}"

    main_diff = diff_out.strip() if diff_out else ""

    # Recursively extract actual submodule diffs against sub_baselines
    sub_diffs = []
    sub_paths = get_submodule_paths(cwd)
    for sub_rel in sub_paths:
        sub_p = Path(cwd) / sub_rel
        if sub_p.exists() and (sub_p / ".git").exists():
            run_git_cmd(["git", "add", "-A", "--intent-to-add"], cwd=str(sub_p))
            sub_base = sub_baselines.get(sub_rel) if sub_baselines else None
            if not sub_base:
                rev_code, gitlink_out, _ = run_git_cmd(["git", "rev-parse", f"HEAD:{sub_rel}"], cwd=cwd)
                if rev_code == 0 and gitlink_out and gitlink_out.strip():
                    sub_base = gitlink_out.strip()
            sub_ref = sub_base if sub_base else "HEAD"
            s_code, s_diff, s_err = run_git_cmd(["git", "diff", "--binary", sub_ref], cwd=str(sub_p))
            if s_code == 0 and s_diff and s_diff.strip():
                sub_diffs.append(f"\n--- [Submodule: {sub_rel}] (diff against {sub_ref}) ---\n{s_diff.strip()}")
            elif s_code != 0:
                return "", f"Submodule {sub_rel} diff extraction failed with code {s_code}: {s_err.strip()}"

    full_diff = main_diff
    if sub_diffs:
        full_diff = (full_diff + "\n" if full_diff else "") + "\n".join(sub_diffs)

    return full_diff.strip(), None

def get_git_diff(cwd: str, base_rev: Optional[str] = None, sub_baselines: Optional[Dict[str, str]] = None) -> str:
    diff_text, _ = get_git_diff_status(cwd, base_rev=base_rev, sub_baselines=sub_baselines)
    return diff_text

def clone_isolated_worktree(src_dir: str, target_dir: Path):
    """
    Safely copies/clones workspace into an isolated directory for race or testing,
    skipping system sockets, fifos, .git, and cache directories.
    Preserves internal symlinks safely remapped to target_dir.
    Enforces fail-closed protection against write-through external symlinks when bwrap is unavailable.
    """
    target_dir.mkdir(parents=True, exist_ok=True)
    resolved = Path(src_dir).resolve()
    from makewand.sandbox import is_bwrap_available
    has_bwrap = is_bwrap_available()

    if resolved not in [Path("/"), Path("/tmp"), Path.home()]:
        for item in resolved.glob("*"):
            if item.name not in [".git", "__pycache__", ".pytest_cache"]:
                try:
                    dst_item = target_dir / item.name
                    if item.is_symlink():
                        raw_target = os.readlink(item)
                        raw_path = Path(raw_target)
                        if raw_path.is_absolute():
                            target_res = raw_path.resolve()
                            if target_res.is_relative_to(resolved):
                                if has_bwrap:
                                    os.symlink(raw_target, dst_item)
                                else:
                                    new_target = target_dir / target_res.relative_to(resolved)
                                    rel_target = os.path.relpath(new_target, dst_item.parent)
                                    os.symlink(rel_target, dst_item)
                            elif has_bwrap:
                                os.symlink(raw_target, dst_item)
                        else:
                            target_res = item.resolve()
                            if target_res.is_relative_to(resolved):
                                os.symlink(raw_target, dst_item)
                            elif has_bwrap:
                                os.symlink(raw_target, dst_item)
                    elif item.is_dir():
                        shutil.copytree(item, dst_item, dirs_exist_ok=True, symlinks=True, ignore_dangling_symlinks=True)
                    elif item.is_file() and not item.is_socket():
                        try:
                            os.link(item, dst_item)
                        except OSError:
                            shutil.copy2(item, dst_item, follow_symlinks=False)
                except Exception:
                    pass

    # Sanitize symlinks across target_dir
    sanitize_shadow_symlinks(target_dir, resolved)

    # Fail-closed check: remove any lingering external symlinks if bwrap is not available
    if not has_bwrap:
        ext_links = find_external_symlinks(target_dir, resolved, allow_repo_root=False)
        for link_path, _ in ext_links:
            try:
                link_path.unlink()
            except Exception:
                pass

    # Initialize isolated git baseline in target_dir so all existing files are committed
    run_git_cmd(["git", "init"], cwd=str(target_dir))
    run_git_cmd(["git", "config", "user.name", "Makewand"], cwd=str(target_dir))
    run_git_cmd(["git", "config", "user.email", "makewand@local"], cwd=str(target_dir))
    run_git_cmd(["git", "add", "-A"], cwd=str(target_dir))
    run_git_cmd(["git", "commit", "-m", "Makewand isolated baseline", "--allow-empty"], cwd=str(target_dir))

def get_active_interactive_working_trees():
    """
    Returns a mapping of canonical working directory paths to session details
    for all active interactive AI sessions (tmux panes + external terminals).
    """
    active_trees = {}
    try:
        from makewand.observer import get_external_ai_sessions, get_active_tmux_sessions, get_session_cwd
        # 1. Tmux sessions
        for s in get_active_tmux_sessions():
            cwd = get_session_cwd(s)
            if cwd and os.path.exists(cwd):
                canon = str(Path(cwd).resolve())
                active_trees[canon] = {
                    "source": "tmux",
                    "session_name": s,
                    "cwd": canon
                }

        # 2. External AI sessions
        for ext in get_external_ai_sessions():
            cwd = ext.get("cwd")
            if cwd and os.path.exists(cwd):
                canon = str(Path(cwd).resolve())
                if canon not in active_trees:
                    active_trees[canon] = {
                        "source": "external_terminal",
                        "tty": ext.get("tty"),
                        "pid": ext.get("pid"),
                        "ai_type": ext.get("ai_type"),
                        "cwd": canon
                    }
    except Exception:
        pass
    return active_trees

P920_PROTECTED_SUBSTRINGS = [
    "/release-candidates/",
    "/static-releases/",
    "/runtime-venvs/",
]

def get_protected_paths() -> List[Path]:
    """
    Returns user-configured forbidden production paths.
    Loaded dynamically from MAKEWAND_PROTECTED_PATHS environment variable
    or ~/.config/makewand/protected_paths.json.
    """
    paths = []
    env_val = os.environ.get("MAKEWAND_PROTECTED_PATHS", "")
    if env_val:
        for p in env_val.split(os.pathsep):
            if p.strip():
                try:
                    paths.append(Path(p.strip()).expanduser().resolve())
                except Exception:
                    pass

    try:
        from makewand.config import CONFIG_DIR
        prot_file = CONFIG_DIR / "protected_paths.json"
        if prot_file.exists():
            with open(prot_file, "r", encoding="utf-8") as f:
                data = json.load(f)
                if isinstance(data, list):
                    for p in data:
                        paths.append(Path(str(p)).expanduser().resolve())
    except Exception:
        pass

    return paths

def is_protected_production_path(path: Union[str, Path]) -> Tuple[bool, Optional[str]]:
    """
    Checks whether the path is a forbidden production / release-candidate tree.
    """
    if not path:
        return False, None
    try:
        resolved = Path(path).expanduser().resolve()
        resolved_str = str(resolved)
        for p in get_protected_paths():
            if resolved == p or p in resolved.parents:
                return True, f"目标路径 {resolved} 位于受保护生产封印目录 ({p}) 下，严禁直接热写"
        for sub in P920_PROTECTED_SUBSTRINGS:
            if sub in resolved_str:
                return True, f"目标路径 {resolved} 包含受保护生产封印模式 ({sub})，严禁直接热写"
    except Exception:
        pass
    return False, None

# Backward compatibility alias
is_p920_protected_path = is_protected_production_path

def check_working_tree_isolation(target_dir: str):
    """
    Checks if target_dir overlaps with any active interactive AI session
    or is a protected production directory.
    Returns (is_safe, conflict_warning_message).
    """
    if not target_dir:
        return True, None
    try:
        target_path = Path(target_dir).resolve()

        # 1. Production Tree Guard
        is_prod, prod_msg = is_protected_production_path(target_path)
        if is_prod:
            return False, f"生产保护警报: {prod_msg}"

        # 2. Multi-Session Overlap Detection (bidirectional)
        active = get_active_interactive_working_trees()
        for active_cwd, info in active.items():
            active_path = Path(active_cwd).resolve()
            if target_path == active_path or active_path in target_path.parents or target_path in active_path.parents:
                src_desc = f"tmux 会话 [{info['session_name']}]" if info.get("source") == "tmux" else f"外部独立终端 [{info.get('tty')} · PID {info.get('pid')} · {info.get('ai_type')}]"
                return False, f"工作区 {target_dir} 与活跃交互会话 ({src_desc}) 存在工作树重叠冲突"
    except Exception:
        pass
    return True, None

class ShadowWorktreeResult(tuple):
    """
    Backward-compatible 3-tuple (effective_dir, branch_name, cleanup)
    augmented with delivery isolation metadata.
    """
    def __new__(cls, effective_dir, branch_name, cleanup, baseline_commit=None, repo_head=None, repo_root=None, worktree_root=None, sub_baselines=None):
        return super().__new__(cls, (effective_dir, branch_name, cleanup))

    def __init__(self, effective_dir, branch_name, cleanup, baseline_commit=None, repo_head=None, repo_root=None, worktree_root=None, sub_baselines=None):
        self.effective_dir = effective_dir
        self.branch_name = branch_name
        self.cleanup = cleanup
        self.baseline_commit = baseline_commit
        self.repo_head = repo_head
        self.repo_root = repo_root
        self.worktree_root = worktree_root
        self.sub_baselines = sub_baselines or {}

def find_external_symlinks(worktree_dir: Path, repo_root: Optional[Path] = None, allow_repo_root: bool = False) -> List[Tuple[Path, str]]:
    """
    Finds all symlinks within worktree_dir that point outside worktree_dir (or repo_root if allow_repo_root is True).
    Returns list of (symlink_path, target_string).
    """
    external = []
    try:
        wt_resolved = worktree_dir.resolve()
        repo_resolved = repo_root.resolve() if repo_root else None
    except Exception:
        return []

    for root, dirs, files in os.walk(worktree_dir, followlinks=False):
        items = [(f, False) for f in files] + [(d, True) for d in dirs]
        for name, is_dir in items:
            p = Path(root) / name
            if p.is_symlink():
                try:
                    raw_target = os.readlink(p)
                    resolved = p.resolve()
                    is_in_wt = resolved.is_relative_to(wt_resolved)
                    is_in_repo = (repo_resolved is not None and resolved.is_relative_to(repo_resolved))
                    if is_in_wt or (allow_repo_root and is_in_repo):
                        continue
                    external.append((p, raw_target))
                except Exception:
                    pass
    return external

def sanitize_shadow_symlinks(worktree_dir: Path, repo_root: Path):
    """
    Audit and sanitize all symlinks within worktree_dir.
    - When bwrap sandbox is available:
      Internal symlinks resolve safely because sandbox mounts host root read-only.
    - When bwrap sandbox is not available:
      Rewrites ALL symlinks (both absolute and relative) that resolve to repo_root so they resolve
      strictly within worktree_dir via relative paths.
    """
    try:
        wt_resolved = worktree_dir.resolve()
        repo_resolved = repo_root.resolve()
    except Exception:
        return

    from makewand.sandbox import is_bwrap_available
    has_bwrap = is_bwrap_available()

    if has_bwrap:
        return

    for root, dirs, files in os.walk(worktree_dir, followlinks=False):
        items = [(f, False) for f in files] + [(d, True) for d in dirs]
        for name, is_dir in items:
            p = Path(root) / name
            if p.is_symlink():
                try:
                    resolved = p.resolve()
                    # If it resolves inside repo_root and NOT inside worktree_dir, remap to worktree_dir
                    if resolved.is_relative_to(repo_resolved) and not resolved.is_relative_to(wt_resolved):
                        rel = resolved.relative_to(repo_resolved)
                        new_target = wt_resolved / rel
                        rel_target = os.path.relpath(new_target, p.parent)
                        p.unlink()
                        os.symlink(rel_target, p)
                    elif resolved.is_relative_to(wt_resolved):
                        raw_target = os.readlink(p)
                        if Path(raw_target).is_absolute():
                            rel_target = os.path.relpath(resolved, p.parent)
                            p.unlink()
                            os.symlink(rel_target, p)
                except Exception:
                    pass

def create_ephemeral_shadow_worktree(base_dir: str, prefix: str = "shadow"):
    """
    Creates an isolated ephemeral git worktree or shadow copy for base_dir.
    Carries forward uncommitted tracked and untracked changes into a clean baseline commit
    without touching the shared .git/config or altering original files.
    Preserves relative subdirectories when called from inside a repository.
    Returns: (effective_worktree_dir, branch_name, cleanup_callback) as ShadowWorktreeResult.
    """
    import uuid
    from datetime import datetime

    base_path = Path(base_dir).resolve()
    timestamp = datetime.now().strftime("%Y%m%d_%H%M%S")
    rand_id = uuid.uuid4().hex[:6]
    shadow_base = Path("/tmp/makewand-shadow-worktrees")
    shadow_base.mkdir(parents=True, exist_ok=True)
    worktree_dir = shadow_base / f"{base_path.name}_{timestamp}_{rand_id}"
    branch_name = f"makewand/{prefix}_{timestamp}_{rand_id}"

    # Check if base_dir is inside a git repository
    code, is_inside, _ = run_git_cmd(["git", "rev-parse", "--is-inside-work-tree"], cwd=str(base_path))
    if code == 0 and "true" in is_inside.strip().lower():
        code, repo_root_str, _ = run_git_cmd(["git", "rev-parse", "--show-toplevel"], cwd=str(base_path))
        repo_root = Path(repo_root_str.strip()).resolve() if code == 0 and repo_root_str.strip() else base_path

        # Record original repository HEAD commit hash before dirty changes
        _, head_out, _ = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(repo_root))
        repo_head_hash = head_out.strip() if head_out else None

        try:
            rel_sub = base_path.relative_to(repo_root)
        except ValueError:
            rel_sub = Path(".")

        # Create lightweight independent clone with shared objects (decoupled git metadata for sandbox writeability)
        cmd = ["git", "-c", "protocol.file.allow=always", "clone", "--shared", str(repo_root), str(worktree_dir)]
        wt_code, wt_out, wt_err = run_git_cmd(cmd)
        if wt_code == 0:
            def cleanup():
                shutil.rmtree(worktree_dir, ignore_errors=True)

            co_target = repo_head_hash if repo_head_hash else "HEAD"
            run_git_cmd(["git", "checkout", "-b", branch_name, co_target], cwd=str(worktree_dir))

            # 0. Submodule recursion & dirty state forwarding
            sub_baselines = {}
            if (repo_root / ".gitmodules").exists():
                sub_code, _, sub_err = run_git_cmd(["git", "-c", "protocol.file.allow=always", "submodule", "update", "--init", "--recursive"], cwd=str(worktree_dir))
                if sub_code != 0:
                    import sys
                    print(c(f"❌ [Makewand Guard] 影子工作树子模块初始化失败 ({sub_err})，拒绝以残缺快照作为基线。", COLOR_RED), file=sys.stderr)
                    cleanup()
                    return None, None, None

                # Check and synchronize actual checkout commits and dirty state for all submodules
                sorted_subs = get_submodule_paths(str(repo_root))
                for sub_rel in sorted_subs:
                    src_sub = repo_root / sub_rel
                    dst_sub = worktree_dir / sub_rel
                    if src_sub.exists() and dst_sub.exists() and (src_sub / ".git").exists():
                        # 1. Sync exact checked-out commit from src_sub to dst_sub
                        sc_code, sc_out, _ = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(src_sub))
                        if sc_code == 0 and sc_out.strip():
                            target_sub_commit = sc_out.strip()
                            run_git_cmd(["git", "fetch", str(src_sub.resolve()), target_sub_commit], cwd=str(dst_sub))
                            co_code, _, co_err = run_git_cmd(["git", "checkout", target_sub_commit], cwd=str(dst_sub))
                            if co_code != 0:
                                import sys
                                print(c(f"❌ [Makewand Guard] 子模块 {sub_rel} 检出失败 ({co_err})，拒绝残缺快照。", COLOR_RED), file=sys.stderr)
                                cleanup()
                                return None, None, None
                            _, dst_head, _ = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(dst_sub))
                            if dst_head.strip() != target_sub_commit:
                                import sys
                                print(c(f"❌ [Makewand Guard] 子模块 {sub_rel} 版本校验不一致，拒绝残缺快照。", COLOR_RED), file=sys.stderr)
                                cleanup()
                                return None, None, None

                        # 2. Forward submodule dirty diffs with full index
                        sd_code, sd_diff, _ = run_git_cmd(["git", "diff", "--binary", "--full-index", "HEAD"], cwd=str(src_sub), binary=True)
                        if sd_code == 0 and sd_diff and len(sd_diff.strip()) > 0:
                            sa_code, _, sa_err = run_git_cmd(["git", "apply", "--binary", "-"], cwd=str(dst_sub), input_data=sd_diff)
                            if sa_code != 0:
                                import sys
                                print(c(f"❌ [Makewand Guard] 子模块 {sub_rel} 改动应用失败 ({sa_err})，拒绝残缺快照。", COLOR_RED), file=sys.stderr)
                                cleanup()
                                return None, None, None

                        # 3. Forward submodule untracked files and symlinks with traversal defense
                        su_code, su_out_b, su_err = run_git_cmd(["git", "ls-files", "-z", "--others", "--exclude-standard"], cwd=str(src_sub), binary=True)
                        if su_code != 0:
                            import sys
                            print(c(f"❌ [Makewand Guard] 列举子模块 {sub_rel} 未跟踪文件失败 ({su_err})，拒绝残缺快照。", COLOR_RED), file=sys.stderr)
                            cleanup()
                            return None, None, None

                        if su_out_b:
                            dst_sub_resolved = dst_sub.resolve()
                            src_sub_resolved = src_sub.resolve()
                            for su_b in su_out_b.split(b"\0"):
                                if not su_b:
                                    continue
                                try:
                                    su_path = Path(os.fsdecode(su_b))
                                    s_src = src_sub / su_path
                                    s_dst = dst_sub / su_path

                                    # Prevent symlink directory traversal escape
                                    dst_parent_resolved = s_dst.parent.resolve()
                                    if not dst_parent_resolved.is_relative_to(dst_sub_resolved):
                                        cleanup()
                                        return None, None, None

                                    if s_src.is_symlink():
                                        s_dst.parent.mkdir(parents=True, exist_ok=True)
                                        if s_dst.exists() or s_dst.is_symlink():
                                            s_dst.unlink()
                                        raw_target = os.readlink(s_src)
                                        raw_path = Path(raw_target)
                                        if raw_path.is_absolute():
                                            resolved_target = raw_path.resolve()
                                            if resolved_target.is_relative_to(src_sub_resolved):
                                                from makewand.sandbox import is_bwrap_available
                                                if is_bwrap_available():
                                                    os.symlink(raw_target, s_dst)
                                                else:
                                                    new_target = dst_sub_resolved / resolved_target.relative_to(src_sub_resolved)
                                                    rel_target = os.path.relpath(new_target, s_dst.parent)
                                                    os.symlink(rel_target, s_dst)
                                            else:
                                                # External symlink: preserve link without copying external content
                                                os.symlink(raw_target, s_dst)
                                        else:
                                            os.symlink(raw_target, s_dst)
                                    elif s_src.is_file():
                                        s_dst.parent.mkdir(parents=True, exist_ok=True)
                                        if s_dst.is_symlink():
                                            s_dst.unlink()
                                        try:
                                            os.link(s_src, s_dst)
                                        except OSError:
                                            shutil.copy2(s_src, s_dst, follow_symlinks=False)
                                except Exception as e:
                                    import sys
                                    print(c(f"❌ [Makewand Guard] 复制子模块 {sub_rel} 未跟踪文件失败 ({e})，中止基线建立。", COLOR_RED), file=sys.stderr)
                                    cleanup()
                                    return None, None, None

                        # Sanitize symlinks within submodule
                        sanitize_shadow_symlinks(dst_sub, src_sub)

                        # 4. Commit submodule dirty baseline inside dst_sub for all submodules
                        a_sub_code, _, a_sub_err = run_git_cmd(["git", "add", "-A"], cwd=str(dst_sub))
                        if a_sub_code != 0:
                            import sys
                            print(c(f"❌ [Makewand Guard] 子模块 {sub_rel} 暂存失败 ({a_sub_err})，拒绝残缺快照。", COLOR_RED), file=sys.stderr)
                            cleanup()
                            return None, None, None

                        c_sub_code, _, c_sub_err = run_git_cmd([
                            "git",
                            "-c", "user.name=Makewand Guard",
                            "-c", "user.email=guard@makewand.local",
                            "commit", "-m", "makewand: submodule dirty baseline",
                            "--allow-empty"
                        ], cwd=str(dst_sub))
                        if c_sub_code != 0:
                            import sys
                            print(c(f"❌ [Makewand Guard] 子模块 {sub_rel} 提交失败 ({c_sub_err})，拒绝残缺快照。", COLOR_RED), file=sys.stderr)
                            cleanup()
                            return None, None, None

                        _, s_base_out, _ = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(dst_sub))
                        sub_baselines[sub_rel] = s_base_out.strip() if s_base_out else None

            # 1. Forward modified binary and text diff FIRST using raw bytes
            diff_code, diff_out_b, diff_err = run_git_cmd(["git", "diff", "--binary", "HEAD"], cwd=str(repo_root), binary=True)
            if diff_code != 0:
                cleanup()
                return None, None, None

            if diff_out_b and len(diff_out_b.strip()) > 0:
                apply_code, _, apply_err = run_git_cmd(
                    ["git", "apply", "--binary", "-"],
                    cwd=str(worktree_dir),
                    input_data=diff_out_b
                )
                if apply_code != 0:
                    import sys
                    print(c(f"❌ [Makewand Guard] 影子工作树应用改动失败 ({apply_err})，拒绝以不完整副本作为基线。", COLOR_RED), file=sys.stderr)
                    cleanup()
                    return None, None, None

            # 2. Forward untracked files with NUL delimiter and symlink traversal defense
            u_code, u_out_b, _ = run_git_cmd(["git", "ls-files", "-z", "--others", "--exclude-standard"], cwd=str(repo_root), binary=True)
            if u_code != 0:
                cleanup()
                return None, None, None

            if u_out_b:
                wt_resolved = worktree_dir.resolve()
                repo_resolved = repo_root.resolve()
                for rel_b in u_out_b.split(b"\0"):
                    if not rel_b:
                        continue
                    try:
                        rel_path = Path(os.fsdecode(rel_b))
                        src_f = repo_root / rel_path
                        dst_f = worktree_dir / rel_path

                        # Prevent symlink directory traversal escape
                        dst_parent_resolved = dst_f.parent.resolve()
                        if not dst_parent_resolved.is_relative_to(wt_resolved):
                            cleanup()
                            return None, None, None

                        if src_f.is_symlink():
                            dst_f.parent.mkdir(parents=True, exist_ok=True)
                            if dst_f.exists() or dst_f.is_symlink():
                                dst_f.unlink()
                            raw_target = os.readlink(src_f)
                            raw_path = Path(raw_target)
                            if raw_path.is_absolute():
                                resolved_target = raw_path.resolve()
                                if resolved_target.is_relative_to(repo_resolved):
                                    from makewand.sandbox import is_bwrap_available
                                    if is_bwrap_available():
                                        os.symlink(raw_target, dst_f)
                                    else:
                                        new_target = wt_resolved / resolved_target.relative_to(repo_resolved)
                                        rel_target = os.path.relpath(new_target, dst_f.parent)
                                        os.symlink(rel_target, dst_f)
                                else:
                                    # External symlink: preserve link without copying external content
                                    os.symlink(raw_target, dst_f)
                            else:
                                os.symlink(raw_target, dst_f)
                        elif src_f.is_file():
                            dst_f.parent.mkdir(parents=True, exist_ok=True)
                            if dst_f.is_symlink():
                                dst_f.unlink()
                            try:
                                os.link(src_f, dst_f)
                            except OSError:
                                shutil.copy2(src_f, dst_f, follow_symlinks=False)
                    except Exception as e:
                        import sys
                        print(c(f"❌ [Makewand Guard] 复制未跟踪文件失败 ({e})，中止基线建立。", COLOR_RED), file=sys.stderr)
                        cleanup()
                        return None, None, None

            # Sanitize all symlinks across shadow worktree
            sanitize_shadow_symlinks(worktree_dir, repo_root)

            # Fail-closed defense: if external symlinks exist and physical sandbox is unavailable, abort
            from makewand.sandbox import is_bwrap_available
            ext_symlinks = find_external_symlinks(worktree_dir, repo_root, allow_repo_root=is_bwrap_available())
            if ext_symlinks and not is_bwrap_available():
                import sys
                ext_desc = f"{ext_symlinks[0][0].name} -> {ext_symlinks[0][1]}"
                print(c(f"❌ [Makewand Guard] 物理沙箱不可用且检测到越界外部符号链接 ({ext_desc})，为防止写穿宿主机阻断任务。", COLOR_RED), file=sys.stderr)
                cleanup()
                return None, None, None

            # 3. Stage and record baseline snapshot without modifying shared .git/config
            add_code, _, _ = run_git_cmd(["git", "add", "-A"], cwd=str(worktree_dir))
            if add_code != 0:
                cleanup()
                return None, None, None

            c_code, _, _ = run_git_cmd([
                "git",
                "-c", "user.name=Makewand Guard",
                "-c", "user.email=guard@makewand.local",
                "commit", "-m", "makewand: forward active session dirty baseline",
                "--allow-empty"
            ], cwd=str(worktree_dir))
            if c_code != 0:
                cleanup()
                return None, None, None

            _, base_commit_out, _ = run_git_cmd(["git", "rev-parse", "HEAD"], cwd=str(worktree_dir))
            baseline_commit_hash = base_commit_out.strip() if base_commit_out else None

            effective_dir = worktree_dir / rel_sub
            effective_dir.mkdir(parents=True, exist_ok=True)

            return ShadowWorktreeResult(
                str(effective_dir),
                branch_name,
                cleanup,
                baseline_commit=baseline_commit_hash,
                repo_head=repo_head_hash,
                repo_root=str(repo_root),
                worktree_root=str(worktree_dir),
                sub_baselines=sub_baselines
            )

    # Fallback to standalone isolated copy (no git branch)
    try:
        clone_isolated_worktree(str(base_path), worktree_dir)
        def clone_cleanup():
            shutil.rmtree(worktree_dir, ignore_errors=True)

        return ShadowWorktreeResult(
            str(worktree_dir),
            None,
            clone_cleanup,
            baseline_commit=None,
            repo_head=None,
            repo_root=str(base_path),
            worktree_root=str(worktree_dir)
        )
    except Exception:
        return None, None, None
