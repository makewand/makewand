"""
Unit tests for Makewand Repo-Map Engine & CLI.
"""

try:  # 测试隔离必须先于 makewand 导入：临时 HOME/配置、AI CLI 桩、屏蔽本地模型端点
    import _isolation  # noqa: F401
except ImportError:  # python3 -m unittest tests.<module>
    from tests import _isolation  # noqa: F401

import io
import json
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
            src_idx = next(i for i, line in enumerate(lines) if "src/engine.py" in line)
            test_idx = next(i for i, line in enumerate(lines) if "tests/test_main.py" in line)
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
        root = str(Path(__file__).resolve().parent.parent)
        line_budget, max_files = 60, 40
        repo_map = generate_repo_map(root, max_lines=line_budget, max_files=max_files)
        lines = repo_map.splitlines()
        self.assertTrue(lines)
        self.assertLessEqual(len(lines), line_budget + 1)

        # New tracked modules change PageRank, so a small prompt budget need not
        # include these specific files. Preserve their coverage assertion in the
        # full selected map (a header plus at most eight symbols per file).
        full_map = generate_repo_map(root, max_lines=max_files * 9 + 1, max_files=max_files)
        self.assertTrue(
            "makewand/orchestrator.py:" in full_map
            or "makewand/candidate.py:" in full_map
            or "makewand/cli.py:" in full_map
        )
        self.assertEqual(lines[-1], "  ... (more symbols truncated)")
        self.assertEqual(lines[:-1], full_map.splitlines()[:line_budget])


    def test_core_package_remains_visible_with_custom_checkout_name(self):
        # A renamed checkout must keep its own Python package in the scan pool.
        # More than 150 core Go files would otherwise consume that whole pool.
        with tempfile.TemporaryDirectory() as tmpdir:
            for checkout_name in ("makewand", "custom-checkout-name"):
                with self.subTest(checkout_name=checkout_name):
                    root = Path(tmpdir) / checkout_name
                    package = root / "makewand"
                    auxiliary = root / "internal" / "auxiliary"
                    package.mkdir(parents=True)
                    auxiliary.mkdir(parents=True)
                    (package / "engine.py").write_text(
                        "class ProjectEngine:\n    def run(self):\n        return True\n",
                        encoding="utf-8",
                    )
                    for index in range(160):
                        (auxiliary / f"helper_{index:03}.go").write_text(
                            f"package auxiliary\n// ProjectEngine coordinates this helper.\n"
                            f"func Helper{index}() {{}}\n",
                            encoding="utf-8",
                        )
                    full_map = generate_repo_map(str(root), max_lines=361, max_files=40)
                    short_map = generate_repo_map(str(root), max_lines=60, max_files=40)
                    self.assertIn("makewand/engine.py:", full_map)
                    self.assertIn("  class ProjectEngine:", full_map)
                    self.assertIn("    def run()", full_map)
                    self.assertLessEqual(len(short_map.splitlines()), 61)
                    self.assertEqual(short_map.splitlines()[-1], "  ... (more symbols truncated)")
                    self.assertEqual(short_map.splitlines()[:-1], full_map.splitlines()[:60])

    def test_generate_repo_map_header_budget_boundaries(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            root = Path(tmpdir)
            self.assertEqual(generate_repo_map(str(root), max_lines=1), "")
            (root / "empty.py").write_text("# No public symbols.\n", encoding="utf-8")
            self.assertEqual(generate_repo_map(str(root), max_lines=1), "")
            (root / "a.py").write_text(
                "def header_one():\n    pass\ndef header_two():\n    pass\n",
                encoding="utf-8",
            )
            for index in range(30):
                (root / f"z_{index:02}.py").write_text(
                    f"def symbol_{index}():\n    pass\n", encoding="utf-8"
                )
            full_map = generate_repo_map(str(root), max_lines=361, max_files=40)
            full_lines = full_map.splitlines()
            self.assertGreater(len(full_lines), 60)
            self.assertEqual(full_lines[0], "a.py:")
            self.assertTrue(full_lines[59].endswith(":"))
            for budget in (1, 2, 60):
                with self.subTest(budget=budget):
                    short_lines = generate_repo_map(
                        str(root), max_lines=budget, max_files=40
                    ).splitlines()
                    self.assertEqual(len(short_lines), budget + 1)
                    self.assertEqual(short_lines[-1], "  ... (more symbols truncated)")
                    self.assertEqual(short_lines[:-1], full_lines[:budget])

    def test_compute_symbol_pagerank_promotes_central_files(self):
        from makewand.repomap import compute_symbol_pagerank, extract_file_symbols
        with tempfile.TemporaryDirectory() as tmpdir:
            tmpp = Path(tmpdir)
            (tmpp / "core.py").write_text("class CoreEngine:\n    def run(self):\n        pass\n")
            (tmpp / "service_a.py").write_text("from core import CoreEngine\ne = CoreEngine()\n")
            (tmpp / "service_b.py").write_text("from core import CoreEngine\ne2 = CoreEngine()\n")
            (tmpp / "isolated.py").write_text("def lone_function():\n    return 42\n")

            candidates = ["core.py", "service_a.py", "service_b.py", "isolated.py"]
            file_symbols = {p: extract_file_symbols(tmpp / p) for p in candidates}
            ranks = compute_symbol_pagerank(tmpp, candidates, file_symbols)

            self.assertIn("core.py", ranks)
            self.assertIn("isolated.py", ranks)
            # core.py is referenced by 2 services, so its PageRank must exceed isolated.py
            self.assertGreater(ranks["core.py"], ranks["isolated.py"])

    def test_compute_symbol_pagerank_word_boundary_precision(self):
        from makewand.repomap import compute_symbol_pagerank, extract_file_symbols
        with tempfile.TemporaryDirectory() as tmpdir:
            tmpp = Path(tmpdir)
            (tmpp / "items.py").write_text("class Item:\n    def get_id(self): pass\n")
            (tmpp / "user.py").write_text("def process(x: Item): pass\n")
            (tmpp / "unrelated.py").write_text("def itemize_all(): pass\n")

            candidates = ["items.py", "user.py", "unrelated.py"]
            file_symbols = {p: extract_file_symbols(tmpp / p) for p in candidates}
            ranks = compute_symbol_pagerank(tmpp, candidates, file_symbols)

            self.assertGreater(ranks["items.py"], ranks["unrelated.py"])

    def test_extract_with_treesitter_fallback(self):
        from makewand.repomap import _extract_with_treesitter
        res = _extract_with_treesitter(Path("foo.py"), "def bar(): pass")
        # When tree-sitter bindings are not installed in the environment, returns None gracefully
        self.assertIsNone(res)

    def test_extract_with_treesitter_mocked_success(self):
        from makewand.repomap import _extract_with_treesitter
        from unittest.mock import MagicMock
        import sys

        # Mock tree_sitter and tree_sitter_languages
        mock_ts = MagicMock()
        mock_ts_lang = MagicMock()
        mock_parser = MagicMock()

        # Construct a fake tree with a function_definition node
        class FakeNode:
            def __init__(self, node_type, start_byte, end_byte, children=None):
                self.type = node_type
                self.start_byte = start_byte
                self.end_byte = end_byte
                self.children = children or []

        code = "def calculate_total(items):\n    return sum(items)\n"
        # "calculate_total" is at offset 4 to 19
        id_node = FakeNode("identifier", 4, 19)
        fn_node = FakeNode("function_definition", 0, len(code), children=[id_node])
        root_node = FakeNode("module", 0, len(code), children=[fn_node])

        mock_tree = MagicMock()
        mock_tree.root_node = root_node
        mock_parser.parse.return_value = mock_tree
        mock_ts_lang.get_parser.return_value = mock_parser

        with patch.dict(sys.modules, {"tree_sitter": mock_ts, "tree_sitter_languages": mock_ts_lang}):
            res = _extract_with_treesitter(Path("calc.py"), code)
            self.assertIsNotNone(res)
            self.assertIn("  function_definition calculate_total", res)

    def test_extract_defined_names_treesitter_and_go_receivers(self):
        from makewand.repomap import _extract_defined_names
        syms = [
            "  function_definition calculate_total",
            "  class_definition MyService",
            "    method_definition fetch_data",
            "  struct_item Record",
            "  type_declaration Config",
            "  func (c *Config) Validate()",
            "  type Handler struct",
            "  def standalone_func()",
            "  function_item process_batch",
            "  trait_item Processor",
            "  interface_declaration UserProfile",
            "  class_specifier DatabaseEngine",
            "  enum_declaration Status",
        ]
        names = _extract_defined_names(syms)
        self.assertIn("calculate_total", names)
        self.assertIn("MyService", names)
        self.assertIn("fetch_data", names)
        self.assertIn("Record", names)
        self.assertIn("Config", names)
        self.assertIn("Validate", names)
        self.assertIn("Handler", names)
        self.assertIn("standalone_func", names)
        self.assertIn("process_batch", names)
        self.assertIn("Processor", names)
        self.assertIn("UserProfile", names)
        self.assertIn("DatabaseEngine", names)
        self.assertIn("Status", names)

    def test_treesitter_captures_class_methods_at_depth(self):
        from makewand.repomap import _extract_with_treesitter
        from unittest.mock import MagicMock
        import sys

        mock_ts = MagicMock()
        mock_ts_lang = MagicMock()
        mock_parser = MagicMock()

        class FakeNode:
            def __init__(self, node_type, start_byte, end_byte, children=None):
                self.type = node_type
                self.start_byte = start_byte
                self.end_byte = end_byte
                self.children = children or []

        code = "class Service:\n    def run(self):\n        pass\n"
        # module (0) -> class_definition (1) -> block (2) -> function_definition (3) -> identifier (4)
        m_id = FakeNode("identifier", 23, 26)
        fn_node = FakeNode("function_definition", 19, 41, children=[m_id])
        block_node = FakeNode("block", 14, 41, children=[fn_node])
        c_id = FakeNode("identifier", 6, 13)
        class_node = FakeNode("class_definition", 0, 41, children=[c_id, block_node])
        root_node = FakeNode("module", 0, 41, children=[class_node])

        mock_tree = MagicMock()
        mock_tree.root_node = root_node
        mock_parser.parse.return_value = mock_tree
        mock_ts_lang.get_parser.return_value = mock_parser

        with patch.dict(sys.modules, {"tree_sitter": mock_ts, "tree_sitter_languages": mock_ts_lang}):
            res = _extract_with_treesitter(Path("service.py"), code)
            self.assertIsNotNone(res)
            self.assertIn("  class_definition Service", res)
            self.assertIn("    function_definition run", res)

    def test_treesitter_availability_caching(self):
        from makewand.repomap import is_treesitter_available
        import makewand.repomap as rm
        # When tree_sitter is not installed and not in sys.modules
        rm._TREESITTER_AVAILABLE = False
        self.assertFalse(is_treesitter_available())
        # Caching works: repeated call doesn't raise or re-import
        self.assertFalse(is_treesitter_available())


if __name__ == "__main__":
    unittest.main()



