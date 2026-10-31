.PHONY: install run test lint

# 依存パッケージを uv.lock のとおりに .venv へ入れる
install:
	uv sync --frozen --no-dev

run:
	uv run --no-dev gunicorn

test:
	uv run pytest

lint:
	uv run ruff format --check .
	uv run ruff check .
	uv run mypy isureal tests
