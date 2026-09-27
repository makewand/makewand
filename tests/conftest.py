import os
import tempfile
import pytest

@pytest.fixture(autouse=True, scope="session")
def isolate_test_usage():
    pid = os.getpid()
    test_file = os.path.join(tempfile.gettempdir(), f"makewand_test_usage_{pid}.json")
    os.environ["MAKEWAND_USAGE_FILE"] = test_file
    yield test_file
    try:
        if os.path.exists(test_file):
            os.remove(test_file)
        lock_file = os.path.join(tempfile.gettempdir(), f".makewand_test_usage_{pid}.lock")
        if os.path.exists(lock_file):
            os.remove(lock_file)
    except Exception:
        pass
