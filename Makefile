.PHONY: all install test test-py test-go check-secrets prelaunch probe status clean

all: test

install:
	./scripts/install.sh

test-py:
	python3 -m unittest discover tests -v

test-go:
	go test -count=1 ./internal/tui ./serverui ./router ./serverteam ./serverauth ./serveradmin ./internal/remotesession ./internal/engine ./cmd/makewand

test: test-py test-go

check-secrets:
	./scripts/check_secrets.sh

prelaunch: test check-secrets
	@echo "Checking shell script syntax..."
	@for f in scripts/*.sh; do bash -n "$$f" || exit 1; done
	@echo "Prelaunch checks passed successfully!"

probe:
	./bin/makewand probe

status:
	./bin/makewand status

clean:
	find . -type d -name "__pycache__" -exec rm -rf {} + 2>/dev/null || true
	find . -type f -name "*.pyc" -delete 2>/dev/null || true
