"""
Unit tests for Makewand Model Context Protocol (MCP) Client (makewand/mcp.py).
"""

try:  # 测试隔离必须先于 makewand 导入
    import _isolation  # noqa: F401
except ImportError:
    from tests import _isolation  # noqa: F401

import sys
import os
import json
import tempfile
import unittest
from pathlib import Path
from makewand.mcp import MCPClient

MOCK_SERVER_CODE = """
import sys
import json

while True:
    line = sys.stdin.readline()
    if not line:
        break
    try:
        msg = json.loads(line.strip())
    except Exception:
        continue

    method = msg.get("method")
    req_id = msg.get("id")

    if method == "initialize":
        resp = {
            "jsonrpc": "2.0",
            "id": req_id,
            "result": {
                "protocolVersion": "2024-11-05",
                "serverInfo": {"name": "test-mock-server", "version": "1.0.0"},
                "capabilities": {"tools": {}}
            }
        }
        sys.stdout.write(json.dumps(resp) + "\\n")
        sys.stdout.flush()
    elif method == "notifications/initialized":
        pass
    elif method == "tools/list":
        resp = {
            "jsonrpc": "2.0",
            "id": req_id,
            "result": {
                "tools": [
                    {"name": "fetch_file", "description": "Fetches a file content"},
                    {"name": "run_query", "description": "Runs a SQL query"}
                ]
            }
        }
        body = json.dumps(resp)
        # Test multi-line HTTP headers before body
        sys.stdout.write(f"Content-Length: {len(body.encode('utf-8'))}\\r\\nContent-Type: application/json\\r\\n\\r\\n{body}")
        sys.stdout.flush()
    elif method == "tools/call":
        tool_name = msg.get("params", {}).get("name")
        resp = {
            "jsonrpc": "2.0",
            "id": req_id,
            "result": {
                "content": [
                    {"type": "text", "text": f"Successfully called {tool_name}"}
                ]
            }
        }
        sys.stdout.write(json.dumps(resp) + "\\n")
        sys.stdout.flush()
"""

class TestMCPClient(unittest.TestCase):

    def setUp(self):
        self.temp_dir = tempfile.TemporaryDirectory()
        self.server_py = Path(self.temp_dir.name) / "mock_mcp_server.py"
        self.server_py.write_text(MOCK_SERVER_CODE, encoding="utf-8")

    def tearDown(self):
        self.temp_dir.cleanup()

    def test_mcp_client_handshake_and_tool_execution(self):
        cmd = [sys.executable, str(self.server_py)]
        client = MCPClient(cmd, cwd=self.temp_dir.name)

        # 1. Initialize
        ok, msg = client.initialize()
        self.assertTrue(ok)
        self.assertEqual(client.server_info.get("name"), "test-mock-server")

        # 2. List tools
        tools = client.list_tools()
        self.assertEqual(len(tools), 2)
        tool_names = [t["name"] for t in tools]
        self.assertIn("fetch_file", tool_names)
        self.assertIn("run_query", tool_names)

        # 3. Call tool
        res = client.call_tool("fetch_file", {"path": "test.txt"})
        content = res.get("content", [])
        self.assertEqual(len(content), 1)
        self.assertIn("Successfully called fetch_file", content[0].get("text", ""))

        # 4. Clean close
        client.close()
        self.assertIsNone(client.proc)

    def test_mcp_client_stderr_flood_no_deadlock(self):
        # Creates server that writes 256KB to stderr before responding
        flood_server = Path(self.temp_dir.name) / "flood_server.py"
        flood_code = """
import sys
import json

# Write 256KB of garbage to stderr
sys.stderr.write("X" * (256 * 1024))
sys.stderr.flush()

line = sys.stdin.readline()
msg = json.loads(line)
resp = {
    "jsonrpc": "2.0",
    "id": msg.get("id"),
    "result": {
        "protocolVersion": "2024-11-05",
        "serverInfo": {"name": "flood-server", "version": "1.0"},
        "capabilities": {}
    }
}
sys.stdout.write(json.dumps(resp) + "\\n")
sys.stdout.flush()
"""
        flood_server.write_text(flood_code, encoding="utf-8")
        client = MCPClient([sys.executable, str(flood_server)], cwd=self.temp_dir.name, timeout=5.0)
        ok, msg = client.initialize()
        self.assertTrue(ok)
        self.assertEqual(client.server_info.get("name"), "flood-server")
        client.close()

    def test_mcp_client_notification_flood_respects_deadline(self):
        # Creates server that continuously emits notifications without responding
        flood_notify_server = Path(self.temp_dir.name) / "flood_notify_server.py"
        flood_code = """
import sys
import time
import json

line = sys.stdin.readline()
while True:
    sys.stdout.write(json.dumps({"jsonrpc": "2.0", "method": "log", "params": {"msg": "spam"}}) + "\\n")
    sys.stdout.flush()
    time.sleep(0.02)
"""
        flood_notify_server.write_text(flood_code, encoding="utf-8")
        import time
        start_t = time.monotonic()
        client = MCPClient([sys.executable, str(flood_notify_server)], cwd=self.temp_dir.name, timeout=0.15)
        ok, msg = client.initialize()
        elapsed = time.monotonic() - start_t
        self.assertFalse(ok)
        self.assertLess(elapsed, 1.0)
        client.close()
