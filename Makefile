.PHONY: all install test test-py test-go test-dispatch test-install test-benchmark check-secrets prelaunch probe status clean

all: test

install:
	./scripts/install.sh

test-py:
	python3 -I scripts/test_python.py

test-go:
	go test -count=1 ./...

test-dispatch:
	bash dispatch/test.sh

test-install:
	python3 -I scripts/test_installation.py

test-benchmark:
	python3 -I benchmarks/test_runner.py

test: test-py test-go test-dispatch test-install test-benchmark

check-secrets:
	./scripts/check_secrets.sh

prelaunch: test check-secrets
	@echo "Checking shell script syntax..."
	@for f in scripts/*.sh site/install.sh dispatch/*.sh dispatch/ai-dispatch; do bash -n "$$f" || exit 1; done
	@echo "Prelaunch checks passed successfully!"

probe:
	./bin/makewand probe

status:
	./bin/makewand status

clean:
	find . -type d -name "__pycache__" -exec rm -rf {} + 2>/dev/null || true
	find . -type f -name "*.pyc" -delete 2>/dev/null || true
