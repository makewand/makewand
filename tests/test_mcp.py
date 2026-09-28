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
        sys.stdout.write(json.dumps(resp) + "\\n")
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
