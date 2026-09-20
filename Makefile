.PHONY: all install test probe status clean

all: test

install:
	./scripts/install.sh

test:
	python3 -m unittest discover tests

probe:
	./bin/makewand probe

status:
	./bin/makewand status

clean:
	find . -type d -name "__pycache__" -exec rm -rf {} + 2>/dev/null || true
	find . -type f -name "*.pyc" -delete 2>/dev/null || true
