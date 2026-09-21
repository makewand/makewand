.PHONY: all install test test-py test-go prelaunch probe status clean

all: test

install:
	./scripts/install.sh

test-py:
	python3 -m unittest discover tests -v

test-go:
	go test -count=1 ./internal/tui ./serverui ./router ./serverteam ./serverauth ./serveradmin ./internal/remotesession ./internal/engine ./cmd/makewand

test: test-py test-go

prelaunch: test
	@echo "Checking shell script syntax..."
	@for f in scripts/*.sh; do bash -n "$$f" || exit 1; done
	@echo "Checking for local absolute paths in docs..."
	@! grep --line-number --fixed-strings '/path/to/workspace/makewand' README.md docs/*.md 2>/dev/null || (echo "Found local absolute paths!" && exit 1)
	@echo "Prelaunch checks passed successfully!"

probe:
	./bin/makewand probe

status:
	./bin/makewand status

clean:
	find . -type d -name "__pycache__" -exec rm -rf {} + 2>/dev/null || true
	find . -type f -name "*.pyc" -delete 2>/dev/null || true
