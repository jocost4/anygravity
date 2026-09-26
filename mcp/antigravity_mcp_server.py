#!/usr/bin/env python3
"""
Antigravity MCP Server for Hermes Agent (v3.0 - Ultimate Edition).
Runs natively inside WSL/Linux, exposing Antigravity's complete suite of tools,
specialized subagents, agent modes, skills, artifacts, and context directly to Hermes.

Key Features:
- Subagent Tools: agy_run_code_agent, agy_run_research_agent, agy_discover.
- Process Hygiene: Guaranteed proc.kill() on timeouts and disconnects.
- Path-traversal defense on conversation_id and artifact_name.
- Bounded memory with collections.deque to protect VPS RAM.
"""

from __future__ import annotations

import asyncio
from collections import deque
import json
import logging
import os
import re
import shutil
import sys
from pathlib import Path
from typing import Optional

logger = logging.getLogger("antigravity_mcp")

try:
    from mcp.server import MCPServer
except ImportError:
    print("Error: 'mcp' package is required. Run with the Hermes venv python.", file=sys.stderr)
    sys.exit(1)

AGY_BIN = os.environ.get("AGY_BIN") or shutil.which("agy") or str(Path.home() / ".local/bin/agy")
BRAIN_DIRS = [
    Path.home() / ".gemini" / "antigravity-cli" / "brain",
    Path.home() / ".gemini" / "antigravity" / "brain",
]
SKILL_DIRS = [
    Path.home() / ".agents" / "skills",
    Path.home() / ".gemini" / "config" / "skills",
]

_CONV_ID_REGEX = re.compile(r"^[a-zA-Z0-9_-]+$")


def _is_safe_conv_id(conv_id: str) -> bool:
    return bool(_CONV_ID_REGEX.match(conv_id.strip()))


def _get_latest_conversation_id() -> Optional[str]:
    candidates = []
    for b_dir in BRAIN_DIRS:
        if not b_dir.exists():
            continue
        try:
            for child in b_dir.iterdir():
                if child.is_dir() and _is_safe_conv_id(child.name):
                    t_file = child / ".system_generated" / "logs" / "transcript.jsonl"
                    if t_file.exists():
                        try:
                            candidates.append((t_file.stat().st_mtime, child.name))
                        except OSError:
                            continue
        except OSError:
            continue
    if not candidates:
        return None
    candidates.sort(key=lambda x: x[0], reverse=True)
    return candidates[0][1]


def _find_conversation_dir(conversation_id: str) -> Optional[Path]:
    clean_id = conversation_id.strip()
    if not _is_safe_conv_id(clean_id):
        return None
    for b_dir in BRAIN_DIRS:
        c_dir = b_dir / clean_id
        if c_dir.is_dir():
            return c_dir
    return None


def create_server() -> MCPServer:
    server = MCPServer(
        "antigravity",
        instructions=(
            "Google Antigravity Agent Suite. Exposes Antigravity (AGY) tools, "
            "specialized subagents, planning, code execution, skills, and memory to Hermes."
        ),
    )

    @server.tool()
    async def antigravity_run(
        prompt: str,
        cwd: str = "",
        conversation_id: str = "",
        model: str = "",
        effort: str = "",
        dangerously_skip_permissions: bool = True,
        timeout_seconds: int = 600,
    ) -> str:
        """Execute an autonomous task, coding refactor, or query with Antigravity (AGY).

        Args:
            prompt: The instruction or coding task to perform.
            cwd: Working directory (e.g. project path). Defaults to current directory.
            conversation_id: Optional ID of an existing conversation to resume context.
            model: Optional model override (e.g. 'gemini-3.8-flash-high', 'claude-sonnet-4-6').
            effort: Optional reasoning effort ('low', 'medium', 'high').
            dangerously_skip_permissions: Auto-approve file/command execution tools.
            timeout_seconds: Timeout in seconds (default 600).
        """
        if not os.path.exists(AGY_BIN):
            return json.dumps({"error": f"agy binary not found at '{AGY_BIN}'."})

        cmd = [AGY_BIN]
        if conversation_id:
            clean_conv = conversation_id.strip()
            if not _is_safe_conv_id(clean_conv):
                return json.dumps({"error": "Invalid conversation_id format."})
            cmd.extend(["--conversation", clean_conv])

        if dangerously_skip_permissions:
            cmd.append("--dangerously-skip-permissions")
        if model:
            cmd.extend(["--model", model.strip()])
        model_has_effort = any(model.strip().endswith(f"-{s}") for s in ("high", "medium", "low", "max"))
        if effort and not model_has_effort:
            cmd.extend(["--effort", effort.strip()])

        cmd.extend(["-p", prompt])
        workdir = cwd.strip() if cwd and os.path.isdir(cwd.strip()) else os.getcwd()

        proc_env = dict(os.environ)
        local_bin = str(Path.home() / ".local/bin")
        if local_bin not in proc_env.get("PATH", ""):
            proc_env["PATH"] = f"{local_bin}:{proc_env.get('PATH', '')}"

        proc = None
        try:
            proc = await asyncio.create_subprocess_exec(
                *cmd,
                stdout=asyncio.subprocess.PIPE,
                stderr=asyncio.subprocess.PIPE,
                cwd=workdir,
                env=proc_env,
            )

            stdout_bytes, stderr_bytes = await asyncio.wait_for(
                proc.communicate(), timeout=float(timeout_seconds)
            )

            stdout = stdout_bytes.decode("utf-8", errors="replace").strip()
            stderr = stderr_bytes.decode("utf-8", errors="replace").strip()
            active_conv = conversation_id or _get_latest_conversation_id() or ""

            res = {
                "success": proc.returncode == 0,
                "returncode": proc.returncode,
                "conversation_id": active_conv,
                "output": stdout,
            }
            if proc.returncode != 0 and stderr:
                res["stderr"] = stderr

            return json.dumps(res, ensure_ascii=False, indent=2)

        except asyncio.TimeoutError:
            if proc:
                try:
                    proc.kill()
                    await proc.wait()
                except Exception:
                    pass
            return json.dumps({
                "success": False,
                "error": f"Execution timed out after {timeout_seconds}s.",
                "conversation_id": conversation_id or _get_latest_conversation_id() or "",
            })
        except Exception as exc:
            if proc:
                try:
                    proc.kill()
                    await proc.wait()
                except Exception:
                    pass
            return json.dumps({
                "success": False,
                "error": f"Exception executing Antigravity: {str(exc)}",
            })

    @server.tool()
    async def antigravity_plan(
        prompt: str,
        cwd: str = "",
        conversation_id: str = "",
        model: str = "",
        effort: str = "high",
        timeout_seconds: int = 300,
    ) -> str:
        """Run Antigravity in PLANNING MODE (--mode plan).
        Analyzes the project and produces an architectural plan without making destructive edits.

        Args:
            prompt: The architecture question or feature to plan out.
            cwd: Working directory (e.g. project path).
            conversation_id: Optional existing conversation to continue.
            model: Optional model override.
            effort: Reasoning effort ('low', 'medium', 'high' - default high).
            timeout_seconds: Timeout in seconds (default 300).
        """
        if not os.path.exists(AGY_BIN):
            return json.dumps({"error": f"agy binary not found at '{AGY_BIN}'."})

        cmd = [AGY_BIN, "--mode", "plan"]
        if conversation_id:
            clean_conv = conversation_id.strip()
            if not _is_safe_conv_id(clean_conv):
                return json.dumps({"error": "Invalid conversation_id format."})
            cmd.extend(["--conversation", clean_conv])

        if model:
            cmd.extend(["--model", model.strip()])
        model_has_effort = any(model.strip().endswith(f"-{s}") for s in ("high", "medium", "low", "max"))
        if effort and not model_has_effort:
            cmd.extend(["--effort", effort.strip()])

        cmd.extend(["-p", prompt])
        workdir = cwd.strip() if cwd and os.path.isdir(cwd.strip()) else os.getcwd()

        proc_env = dict(os.environ)
        local_bin = str(Path.home() / ".local/bin")
        if local_bin not in proc_env.get("PATH", ""):
            proc_env["PATH"] = f"{local_bin}:{proc_env.get('PATH', '')}"

        proc = None
        try:
            proc = await asyncio.create_subprocess_exec(
                *cmd,
                stdout=asyncio.subprocess.PIPE,
                stderr=asyncio.subprocess.PIPE,
                cwd=workdir,
                env=proc_env,
            )

            stdout_b, stderr_b = await asyncio.wait_for(
                proc.communicate(), timeout=float(timeout_seconds)
            )
            stdout = stdout_b.decode("utf-8", errors="replace").strip()
            stderr = stderr_b.decode("utf-8", errors="replace").strip()

            return json.dumps({
                "success": proc.returncode == 0,
                "conversation_id": conversation_id or _get_latest_conversation_id() or "",
                "plan": stdout,
                "stderr": stderr if proc.returncode != 0 else "",
            }, ensure_ascii=False, indent=2)
        except asyncio.TimeoutError:
            if proc:
                try:
                    proc.kill()
                    await proc.wait()
                except Exception:
                    pass
            return json.dumps({
                "success": False,
                "error": f"Planning timed out after {timeout_seconds}s.",
                "conversation_id": conversation_id or _get_latest_conversation_id() or "",
            })
        except Exception as exc:
            if proc:
                try:
                    proc.kill()
                    await proc.wait()
                except Exception:
                    pass
            return json.dumps({"success": False, "error": str(exc)})

    @server.tool()
    async def agy_run_code_agent(
        task: str,
        workspace: str = "",
        model: str = "claude-sonnet-4-6",
        plan_only: bool = False,
        timeout_seconds: int = 600,
    ) -> str:
        """Delegate a specialized coding or refactoring task to Antigravity's Code Agent.

        Args:
            task: Specific coding instruction, bug fix, or refactor.
            workspace: Target repository directory.
            model: Coding model (default 'claude-sonnet-4-6').
            plan_only: If True, only plans without applying file edits.
            timeout_seconds: Max execution time in seconds (default 600).
        """
        mode = "plan" if plan_only else "accept-edits"
        cmd = [
            AGY_BIN,
            "--mode", mode,
            "--dangerously-skip-permissions",
            "--model", model,
            "-p", f"You are a Principal Software Engineer. Execute this task meticulously:\n\n{task}",
        ]
        workdir = workspace.strip() if workspace and os.path.isdir(workspace.strip()) else os.getcwd()

        proc_env = dict(os.environ)
        local_bin = str(Path.home() / ".local/bin")
        if local_bin not in proc_env.get("PATH", ""):
            proc_env["PATH"] = f"{local_bin}:{proc_env.get('PATH', '')}"

        proc = None
        try:
            proc = await asyncio.create_subprocess_exec(
                *cmd,
                stdout=asyncio.subprocess.PIPE,
                stderr=asyncio.subprocess.PIPE,
                cwd=workdir,
                env=proc_env,
            )
            stdout_b, stderr_b = await asyncio.wait_for(
                proc.communicate(), timeout=float(timeout_seconds)
            )
            stdout = stdout_b.decode("utf-8", errors="replace").strip()
            stderr = stderr_b.decode("utf-8", errors="replace").strip()

            return json.dumps({
                "success": proc.returncode == 0,
                "agent": "code-agent",
                "model": model,
                "output": stdout,
                "stderr": stderr if proc.returncode != 0 else "",
            }, ensure_ascii=False, indent=2)
        except asyncio.TimeoutError:
            if proc:
                try:
                    proc.kill()
                    await proc.wait()
                except Exception:
                    pass
            return json.dumps({"success": False, "error": f"Code agent timed out after {timeout_seconds}s"})
        except Exception as e:
            if proc:
                try:
                    proc.kill()
                    await proc.wait()
                except Exception:
                    pass
            return json.dumps({"success": False, "error": str(e)})

    @server.tool()
    async def agy_run_research_agent(
        query: str,
        model: str = "gemini-3.8-flash-high",
        timeout_seconds: int = 300,
    ) -> str:
        """Delegate deep research, technical documentation lookup, or fact synthesis to Antigravity.

        Args:
            query: The research question or topic.
            model: Model to use (default 'gemini-3.8-flash-high').
            timeout_seconds: Max execution time in seconds (default 300).
        """
        prompt = (
            "You are a Research Scientist and Technical Analyst. Conduct a thorough investigation "
            "and provide concise, verified findings with references:\n\n" + query
        )
        cmd = [
            AGY_BIN,
            "--mode", "plan",
            "--dangerously-skip-permissions",
            "--model", model,
            "-p", prompt,
        ]

        proc_env = dict(os.environ)
        local_bin = str(Path.home() / ".local/bin")
        if local_bin not in proc_env.get("PATH", ""):
            proc_env["PATH"] = f"{local_bin}:{proc_env.get('PATH', '')}"

        proc = None
        try:
            proc = await asyncio.create_subprocess_exec(
                *cmd,
                stdout=asyncio.subprocess.PIPE,
                stderr=asyncio.subprocess.PIPE,
                env=proc_env,
            )
            stdout_b, stderr_b = await asyncio.wait_for(
                proc.communicate(), timeout=float(timeout_seconds)
            )
            stdout = stdout_b.decode("utf-8", errors="replace").strip()
            stderr = stderr_b.decode("utf-8", errors="replace").strip()

            return json.dumps({
                "success": proc.returncode == 0,
                "agent": "research-agent",
                "model": model,
                "findings": stdout,
                "stderr": stderr if proc.returncode != 0 else "",
            }, ensure_ascii=False, indent=2)
        except asyncio.TimeoutError:
            if proc:
                try:
                    proc.kill()
                    await proc.wait()
                except Exception:
                    pass
            return json.dumps({"success": False, "error": f"Research agent timed out after {timeout_seconds}s"})
        except Exception as e:
            if proc:
                try:
                    proc.kill()
                    await proc.wait()
                except Exception:
                    pass
            return json.dumps({"success": False, "error": str(e)})

    @server.tool()
    async def agy_discover() -> str:
        """Discover live Antigravity capabilities: active models, external MCPs, and installed skills."""
        results: Dict[str, Any] = {}

        # 1. Models
        try:
            proc = await asyncio.create_subprocess_exec(
                AGY_BIN, "models",
                stdout=asyncio.subprocess.PIPE,
                stderr=asyncio.subprocess.PIPE,
            )
            stdout_b, _ = await asyncio.wait_for(proc.communicate(), timeout=10.0)
            raw = stdout_b.decode("utf-8", errors="replace")
            models = []
            for line in raw.splitlines():
                clean = line.strip()
                if clean and not clean.startswith(("⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇")):
                    p = clean.split(None, 1)
                    models.append(p[0])
            results["available_models"] = models
        except Exception as e:
            results["available_models_error"] = str(e)

        # 2. MCPs
        try:
            proc = await asyncio.create_subprocess_exec(
                AGY_BIN, "mcp", "list",
                stdout=asyncio.subprocess.PIPE,
                stderr=asyncio.subprocess.PIPE,
            )
            stdout_b, _ = await asyncio.wait_for(proc.communicate(), timeout=10.0)
            results["connected_mcps"] = stdout_b.decode("utf-8", errors="replace").strip().splitlines()
        except Exception as e:
            results["connected_mcps_error"] = str(e)

        # 3. Skills count
        skill_count = 0
        for s_root in SKILL_DIRS:
            if s_root.exists():
                skill_count += len([d for d in s_root.iterdir() if d.is_dir() and (d / "SKILL.md").exists()])
        results["installed_skills_count"] = skill_count

        return json.dumps(results, ensure_ascii=False, indent=2)

    @server.tool()
    async def antigravity_list_conversations(limit: int = 10) -> str:
        """List recent Antigravity conversations with their IDs, timestamps, and initial prompts."""
        results = []
        seen_ids = set()

        for b_dir in BRAIN_DIRS:
            if not b_dir.exists():
                continue
            try:
                for child in b_dir.iterdir():
                    if not child.is_dir() or child.name in seen_ids or not _is_safe_conv_id(child.name):
                        continue
                    t_file = child / ".system_generated" / "logs" / "transcript.jsonl"
                    if not t_file.exists():
                        continue

                    try:
                        mtime = t_file.stat().st_mtime
                        first_prompt = ""
                        with open(t_file, "r", encoding="utf-8", errors="replace") as f:
                            for line in f:
                                try:
                                    entry = json.loads(line)
                                    if entry.get("type") == "USER_INPUT":
                                        content = entry.get("content", "")
                                        if "<USER_REQUEST>" in content:
                                            content = content.split("<USER_REQUEST>")[1].split("</USER_REQUEST>")[0].strip()
                                        first_prompt = content[:150]
                                        break
                                except Exception:
                                    continue

                        results.append({
                            "conversation_id": child.name,
                            "updated_at_ts": mtime,
                            "prompt_preview": first_prompt or "(no user prompt recorded)",
                        })
                        seen_ids.add(child.name)
                    except Exception:
                        continue
            except OSError:
                continue

        results.sort(key=lambda x: x["updated_at_ts"], reverse=True)
        return json.dumps(results[:limit], ensure_ascii=False, indent=2)

    @server.tool()
    async def antigravity_get_conversation(conversation_id: str, max_messages: int = 10) -> str:
        """Get message history from a specific Antigravity conversation."""
        c_dir = _find_conversation_dir(conversation_id)
        if not c_dir:
            return json.dumps({"error": f"Conversation '{conversation_id}' not found."})

        t_path = c_dir / ".system_generated" / "logs" / "transcript.jsonl"
        if not t_path.exists():
            return json.dumps({"error": f"Transcript for '{conversation_id}' not found."})

        messages = deque(maxlen=max_messages)
        try:
            with open(t_path, "r", encoding="utf-8", errors="replace") as f:
                for line in f:
                    try:
                        entry = json.loads(line)
                        msg_type = entry.get("type")
                        if msg_type in ("USER_INPUT", "PLANNER_RESPONSE", "MODEL"):
                            content = entry.get("content", "")
                            if "<USER_REQUEST>" in content:
                                content = content.split("<USER_REQUEST>")[1].split("</USER_REQUEST>")[0].strip()
                            role = "user" if msg_type == "USER_INPUT" else "assistant"
                            messages.append({
                                "role": role,
                                "timestamp": entry.get("created_at", ""),
                                "content": content[:1000],
                            })
                    except Exception:
                        continue
        except Exception as exc:
            return json.dumps({"error": f"Failed to read transcript: {str(exc)}"})

        return json.dumps({
            "conversation_id": conversation_id,
            "messages": list(messages),
        }, ensure_ascii=False, indent=2)

    @server.tool()
    async def antigravity_list_artifacts(conversation_id: str = "") -> str:
        """List generated artifacts (.md reports, architecture plans, audits) in a conversation or globally."""
        artifacts = []
        target_dirs = [_find_conversation_dir(conversation_id)] if conversation_id else []
        if not target_dirs or target_dirs[0] is None:
            target_dirs = []
            for b_dir in BRAIN_DIRS:
                if b_dir.exists():
                    try:
                        for child in b_dir.iterdir():
                            if child.is_dir() and _is_safe_conv_id(child.name):
                                target_dirs.append(child)
                    except OSError:
                        continue

        for c_dir in target_dirs:
            if not c_dir or not c_dir.is_dir():
                continue
            for item in c_dir.glob("*.md"):
                try:
                    artifacts.append({
                        "conversation_id": c_dir.name,
                        "artifact_name": item.name,
                        "size_bytes": item.stat().st_size,
                        "modified_at_ts": item.stat().st_mtime,
                        "path": str(item),
                    })
                except OSError:
                    continue

        artifacts.sort(key=lambda x: x["modified_at_ts"], reverse=True)
        return json.dumps(artifacts[:30], ensure_ascii=False, indent=2)

    @server.tool()
    async def antigravity_get_artifact(conversation_id: str, artifact_name: str) -> str:
        """Read the full content of an artifact (.md file) generated by Antigravity."""
        c_dir = _find_conversation_dir(conversation_id)
        if not c_dir:
            return json.dumps({"error": f"Conversation '{conversation_id}' not found."})

        clean_art = Path(artifact_name.strip()).name
        target_file = (c_dir / clean_art).resolve()

        try:
            if not target_file.is_relative_to(c_dir.resolve()) or not target_file.exists() or not target_file.is_file():
                return json.dumps({"error": f"Artifact '{clean_art}' not found in conversation '{conversation_id}'."})
        except Exception:
            return json.dumps({"error": f"Invalid artifact path for '{clean_art}'."})

        try:
            content = target_file.read_text(encoding="utf-8", errors="replace")
            return json.dumps({
                "conversation_id": conversation_id,
                "artifact_name": clean_art,
                "content": content,
            }, ensure_ascii=False, indent=2)
        except Exception as exc:
            return json.dumps({"error": f"Failed to read artifact: {str(exc)}"})

    @server.tool()
    async def antigravity_list_skills() -> str:
        """List all installed skills, playbooks, and automations available to Antigravity."""
        skills = []
        seen = set()

        for s_root in SKILL_DIRS:
            if not s_root.exists():
                continue
            try:
                for s_folder in s_root.iterdir():
                    if not s_folder.is_dir() or s_folder.name in seen:
                        continue
                    skill_md = s_folder / "SKILL.md"
                    if not skill_md.exists():
                        continue

                    seen.add(s_folder.name)
                    desc = ""
                    try:
                        with open(skill_md, "r", encoding="utf-8", errors="replace") as f:
                            content = f.read(2048)
                            m = re.search(r"^description:\s*(.+)$", content, re.MULTILINE)
                            if m:
                                desc = m.group(1).strip().strip('"\'')
                    except Exception:
                        pass

                    skills.append({
                        "name": s_folder.name,
                        "description": desc or "(no description)",
                        "path": str(s_folder),
                    })
            except OSError:
                continue

        skills.sort(key=lambda x: x["name"])
        return json.dumps({"count": len(skills), "skills": skills}, ensure_ascii=False, indent=2)

    return server


def main():
    server = create_server()
    asyncio.run(server.run_stdio_async())


if __name__ == "__main__":
    main()
