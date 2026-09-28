"""
Makewand Model Context Protocol (MCP) Client Engine.
Implements the standard JSON-RPC 2.0 MCP specification over stdio transport.
Allows Makewand to connect to any external MCP server (filesystem, sqlite, github, playwright, etc.).
"""

import os
import sys
import time
import json
import subprocess
import threading
import select
from typing import Dict, Any, List, Optional, Tuple

class MCPClient:
    """
    Standard Model Context Protocol client communicating with MCP server via stdio.
    Supports JSON-RPC 2.0 with both newline-delimited JSON and Content-Length headers.
    Features robust unbuffered I/O with timeout protection and deadlock-free stderr disposal.
    """

    def __init__(self, command: List[str], cwd: Optional[str] = None, env: Optional[Dict[str, str]] = None, timeout: float = 30.0):
        self.command = command
        self.cwd = cwd or os.getcwd()
        self.env = env or dict(os.environ)
        self.timeout = timeout
        self.proc: Optional[subprocess.Popen] = None
        self._req_id = 0
        self._lock = threading.Lock()
        self.server_info: Dict[str, Any] = {}
        self.capabilities: Dict[str, Any] = {}
        self._buf = bytearray()

    def start(self) -> bool:
        """Starts the external MCP server process."""
        if self.proc is not None:
            return True
        try:
            self.proc = subprocess.Popen(
                self.command,
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=subprocess.DEVNULL,  # Prevent 64KB pipe buffer deadlock from unread stderr
                cwd=self.cwd,
                env=self.env
            )
            self._buf = bytearray()
            return True
        except Exception:
            self.proc = None
            return False

    def _read_bytes(self, n: int, timeout: float) -> Optional[bytes]:
        deadline = time.time() + timeout
        while len(self._buf) < n:
            rem = deadline - time.time()
            if rem <= 0 or not self.proc or not self.proc.stdout:
                return None
            try:
                rlist, _, _ = select.select([self.proc.stdout.fileno()], [], [], max(0.01, rem))
                if not rlist:
                    continue
                chunk = os.read(self.proc.stdout.fileno(), 4096)
                if not chunk:
                    return None
                self._buf.extend(chunk)
            except Exception:
                return None
        res = bytes(self._buf[:n])
        del self._buf[:n]
        return res

    def _read_line(self, timeout: float) -> Optional[str]:
        deadline = time.time() + timeout
        while b"\n" not in self._buf:
            rem = deadline - time.time()
            if rem <= 0 or not self.proc or not self.proc.stdout:
                return None
            try:
                rlist, _, _ = select.select([self.proc.stdout.fileno()], [], [], max(0.01, rem))
                if not rlist:
                    continue
                chunk = os.read(self.proc.stdout.fileno(), 4096)
                if not chunk:
                    if self._buf:
                        break
                    return None
                self._buf.extend(chunk)
            except Exception:
                return None
        if b"\n" in self._buf:
            idx = self._buf.index(b"\n")
            line = bytes(self._buf[:idx]).decode("utf-8", errors="replace").rstrip("\r")
            del self._buf[:idx + 1]
            return line
        elif self._buf:
            line = bytes(self._buf).decode("utf-8", errors="replace").rstrip("\r")
            self._buf.clear()
            return line
        return None

    def _send_rpc(self, method: str, params: Optional[Dict[str, Any]] = None, is_notification: bool = False) -> Optional[Dict[str, Any]]:
        if not self.proc or self.proc.poll() is not None:
            return None

        with self._lock:
            self._req_id += 1
            req_id = self._req_id
            payload: Dict[str, Any] = {
                "jsonrpc": "2.0",
                "method": method
            }
            if not is_notification:
                payload["id"] = req_id
            if params is not None:
                payload["params"] = params

            msg_bytes = (json.dumps(payload, ensure_ascii=False) + "\n").encode("utf-8")
            try:
                assert self.proc.stdin is not None
                self.proc.stdin.write(msg_bytes)
                self.proc.stdin.flush()
            except Exception:
                return None

            if is_notification:
                return None

            while True:
                line = self._read_line(self.timeout)
                if line is None:
                    return None
                line = line.strip()
                if not line:
                    continue

                # Handle Content-Length header if present
                if line.lower().startswith("content-length:"):
                    try:
                        content_len = int(line.split(":", 1)[1].strip())
                        # Drain any remaining headers until empty line
                        while True:
                            header_line = self._read_line(self.timeout)
                            if header_line is None or header_line == "":
                                break
                        body_bytes = self._read_bytes(content_len, self.timeout)
                        if not body_bytes:
                            return None
                        data = json.loads(body_bytes.decode("utf-8", errors="replace"))
                        if data.get("id") == req_id:
                            return data
                    except Exception:
                        continue
                else:
                    try:
                        data = json.loads(line)
                        if data.get("id") == req_id:
                            return data
                    except Exception:
                        continue

    def initialize(self) -> Tuple[bool, str]:
        """
        Executes standard MCP protocol initialization handshake.
        """
        if not self.start():
            return False, "Failed to start MCP server subprocess"

        init_params = {
            "protocolVersion": "2024-11-05",
            "capabilities": {
                "tools": {}
            },
            "clientInfo": {
                "name": "makewand",
                "version": "3.1.0"
            }
        }

        resp = self._send_rpc("initialize", init_params)
        if not resp or "result" not in resp:
            err = resp.get("error", {}).get("message", "Unknown initialize error") if resp else "No response"
            return False, f"MCP handshake failed: {err}"

        result = resp["result"]
        self.server_info = result.get("serverInfo", {})
        self.capabilities = result.get("capabilities", {})

        # Send initialized notification
        self._send_rpc("notifications/initialized", is_notification=True)
        return True, "Initialized"

    def list_tools(self) -> List[Dict[str, Any]]:
        """
        Retrieves list of tools exposed by the MCP server.
        """
        resp = self._send_rpc("tools/list", {})
        if resp and "result" in resp:
            return resp["result"].get("tools", [])
        return []

    def call_tool(self, name: str, arguments: Optional[Dict[str, Any]] = None) -> Dict[str, Any]:
        """
        Calls a specific tool on the MCP server and returns the result.
        """
        params = {
            "name": name,
            "arguments": arguments or {}
        }
        resp = self._send_rpc("tools/call", params)
        if not resp:
            return {"isError": True, "content": [{"type": "text", "text": "MCP Server did not respond"}]}
        if "error" in resp:
            return {"isError": True, "content": [{"type": "text", "text": str(resp["error"])}]}
        return resp.get("result", {})

    def close(self) -> None:
        """Closes connection and terminates server subprocess."""
        if self.proc:
            try:
                if self.proc.stdin:
                    self.proc.stdin.close()
                self.proc.terminate()
                self.proc.wait(timeout=2)
            except Exception:
                try:
                    self.proc.kill()
                except Exception:
                    pass
            finally:
                self.proc = None
                self._buf.clear()
