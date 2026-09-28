"""
Unit tests for Makewand Repo-Map Engine & CLI.
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import io
import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from makewand.repomap import (
    _extract_python_symbols,
    _extract_go_symbols,
    _extract_ts_js_symbols,
    _extract_rust_symbols,
    extract_file_symbols,
    generate_repo_map,
    format_repo_map_for_prompt,
)
from makewand.cli import cmd_repomap


class TestRepoMap(unittest.TestCase):
    def test_extract_python_symbols(self):
        py_code = """
class MyService:
    def __init__(self, host: str, port: int):
        self.host = host
    async def fetch_data(self, query: str):
        pass

def top_level_func(a, b, c):
    return a + b + c

async def background_worker():
    pass
"""
        symbols = _extract_python_symbols(py_code)
        self.assertIn("  class MyService:", symbols)
        self.assertIn("    def __init__(host, port)", symbols)
        self.assertIn("    async def fetch_data(query)", symbols)
        self.assertIn("  def top_level_func(a, b, c)", symbols)
        self.assertIn("  async def background_worker()", symbols)

    def test_extract_go_symbols(self):
        go_code = """
package main

type Config struct {
    Port int
}

type Runner interface {
    Run() error
}

func (c *Config) Validate() bool {
    return true
}

func StartServer(addr string) error {
    return nil
}

func TestServer(t *testing.T) {}
func BenchmarkServer(b *testing.B) {}
"""
        symbols = _extract_go_symbols(go_code)
        self.assertIn("  type Config struct", symbols)
        self.assertIn("  type Runner interface", symbols)
        self.assertIn("  func (c *Config) Validate()", symbols)
        self.assertIn("  func StartServer(addr string)", symbols)
        # Tests and benchmarks must be excluded
        self.assertFalse(any("TestServer" in s for s in symbols))
        self.assertFalse(any("BenchmarkServer" in s for s in symbols))

    def test_extract_ts_js_symbols(self):
        ts_code = """
export class UserService {
    getUser() {}
}

export interface UserProfile {
    id: string;
}

export type UserRole = "admin" | "user";

export function handleRequest(req, res) {}

export const calculateHash = (data, algo) => {
    return "";
};
"""
        symbols = _extract_ts_js_symbols(ts_code)
        self.assertIn("  class UserService", symbols)
        self.assertIn("  interface UserProfile", symbols)
        self.assertIn("  type UserRole", symbols)
        self.assertIn("  function handleRequest(req, res)", symbols)
        self.assertIn("  const calculateHash(data, algo)", symbols)

    def test_extract_rust_symbols(self):
        rs_code = """
pub struct TaskQueue {
    capacity: usize,
}

pub enum Priority {
    High,
    Low,
}

pub trait TaskProcessor {
    fn process(&self);
}

pub fn create_queue(cap: usize) -> TaskQueue {
    TaskQueue { capacity: cap }
}

impl TaskQueue {
    pub fn push(&mut self) {}
}

impl TaskProcessor for TaskQueue {
    fn process(&self) {}
}

fn test_internal_helper() {}
"""
        symbols = _extract_rust_symbols(rs_code)
        self.assertIn("  struct TaskQueue", symbols)
        self.assertIn("  enum Priority", symbols)
        self.assertIn("  trait TaskProcessor", symbols)
        self.assertIn("  fn create_queue(cap: usize)", symbols)
        self.assertIn("  impl TaskQueue", symbols)
        self.assertIn("  impl TaskProcessor for TaskQueue", symbols)
        # Skip test functions
        self.assertFalse(any("test_internal_helper" in s for s in symbols))

    def test_generate_repo_map_prioritizes_core_code_over_tests(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            root = Path(tmpdir)
            # Core source directory
            src_dir = root / "src"
            src_dir.mkdir()
            (src_dir / "engine.py").write_text("class CoreEngine:\n    def run(self): pass\n")
            (src_dir / "engine_test.py").write_text("class TestEngine:\n    def test_it(self): pass\n")

            # Test directory
            tests_dir = root / "tests"
            tests_dir.mkdir()
            (tests_dir / "test_main.py").write_text("def test_all(): pass\n")

            # Benchmarks directory
            bench_dir = root / "benchmarks"
            bench_dir.mkdir()
            (bench_dir / "bench.py").write_text("def bench_speed(): pass\n")

            repo_map = generate_repo_map(str(root), max_lines=40)
            lines = repo_map.splitlines()

            # src/engine.py should appear before tests/ and benchmarks/
            src_idx = next(i for i, l in enumerate(lines) if "src/engine.py" in l)
            test_idx = next(i for i, l in enumerate(lines) if "tests/test_main.py" in l)
            self.assertLess(src_idx, test_idx)

    def test_generate_repo_map_budget_truncation(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            root = Path(tmpdir)
            app_dir = root / "app"
            app_dir.mkdir()
            for i in range(10):
                content = "\n".join([f"def func_{i}_{j}(): pass" for j in range(5)])
                (app_dir / f"mod_{i}.py").write_text(content)

            # Limit to 15 lines
            repo_map = generate_repo_map(str(root), max_lines=15)
            lines = repo_map.splitlines()
            self.assertLessEqual(len(lines), 16)
            self.assertTrue(lines[-1].endswith("(more symbols truncated)"))

    def test_format_repo_map_for_prompt(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            root = Path(tmpdir)
            # Empty workspace returns empty string
            self.assertEqual(format_repo_map_for_prompt(str(root)), "")

            # Workspace with code returns formatted prompt block
            (root / "main.py").write_text("def greet(): pass\n")
            formatted = format_repo_map_for_prompt(str(root))
            self.assertIn("【代码库全局架构拓扑感知 (Repo-Map)】", formatted)
            self.assertIn("main.py:", formatted)
            self.assertIn("def greet()", formatted)

    def test_cmd_repomap_cli(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            root = Path(tmpdir)
            (root / "handler.go").write_text("package main\ntype Handler struct {}\n")

            class MockArgs:
                cwd = str(root)
                max_lines = 50
                max_files = 20
                json = False

            with patch("sys.stdout", new_callable=io.StringIO) as mock_out:
                cmd_repomap(MockArgs())
                output = mock_out.getvalue()
                self.assertIn("Repo-Map", output)
                self.assertIn("handler.go:", output)
                self.assertIn("type Handler struct", output)

            # Test JSON output mode
            MockArgs.json = True
            with patch("sys.stdout", new_callable=io.StringIO) as mock_out:
                cmd_repomap(MockArgs())
                data = json.loads(mock_out.getvalue())
                self.assertEqual(data["cwd"], str(root))
                self.assertIn("handler.go:", data["repomap"])
                self.assertGreater(data["lines"], 0)


    def test_extract_go_symbols_advanced(self):
        go_code = """
package router

type (
    Provider = string
    Message = struct{ Text string }
)

type QuotaBand string

func MapSlice[T any, R any](items []T, f func(T) R) []R {
    return nil
}
"""
        symbols = _extract_go_symbols(go_code)
        self.assertIn("  type Provider = string", symbols)
        self.assertIn("  type Message = struct", symbols)
        self.assertIn("  type QuotaBand string", symbols)
        self.assertIn("  func MapSlice(items []T, f func(T) R)", symbols)

    def test_extract_ts_symbols_advanced(self):
        ts_code = """
export enum ServiceStatus {
    HEALTHY = 1,
    DEGRADED = 2,
}

export const executePipeline = async <T>(req: T, timeout: number) => {
    return true;
};
"""
        symbols = _extract_ts_js_symbols(ts_code)
        self.assertIn("  enum ServiceStatus", symbols)
        self.assertIn("  const executePipeline(req: T, timeout: number)", symbols)

    def test_extract_rust_symbols_advanced(self):
        rs_code = """
impl<T: Send + Sync> TaskProcessor for Box<dyn TaskProcessor + T> where T: 'static {
    fn process(&self) {}
}

impl std::fmt::Display for TaskQueue {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result { Ok(()) }
}
"""
        symbols = _extract_rust_symbols(rs_code)
        self.assertIn("  impl TaskProcessor for Box<dyn TaskProcessor + T>", symbols)
        self.assertIn("  impl std::fmt::Display for TaskQueue", symbols)

    def test_extract_c_cpp_symbols(self):
        cpp_code = """
class DatabaseConnection {
public:
    void connect();
};

struct PacketHeader {
    int id;
};

enum class State {
    INIT,
    RUNNING
};

int calculateChecksum(const char* buffer, size_t length_of_data) {
    return 0;
}
"""
        with tempfile.NamedTemporaryFile("w", suffix=".cpp", delete=False) as f:
            f.write(cpp_code)
            f_path = Path(f.name)
        try:
            symbols = extract_file_symbols(f_path)
            self.assertIn("  class DatabaseConnection", symbols)
            self.assertIn("  struct PacketHeader", symbols)
            self.assertIn("  enum class State", symbols)
            self.assertIn("  func calculateChecksum(const char* buffer, size_t ...)", symbols)
        finally:
            f_path.unlink()

    def test_repomap_on_makewand_codebase(self):
        repo_map = generate_repo_map(str(Path(__file__).resolve().parent.parent), max_lines=60)
        self.assertTrue(
            "makewand/orchestrator.py:" in repo_map
            or "makewand/candidate.py:" in repo_map
            or "makewand/cli.py:" in repo_map
        )


if __name__ == "__main__":
    unittest.main()

