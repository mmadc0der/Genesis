# syntax=docker/dockerfile:1.7

FROM golang:1.22-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
RUN CGO_ENABLED=0 go build -o /out/genesis ./cmd/genesis

FROM python:3.12-bookworm AS python-deps
COPY --from=ghcr.io/astral-sh/uv:0.6.14 /uv /usr/local/bin/uv
WORKDIR /app
COPY pyproject.toml uv.lock ./
RUN uv sync --locked --no-dev --compile-bytecode

FROM python:3.12-bookworm
RUN useradd --create-home --uid 65532 --shell /usr/sbin/nologin genesis
WORKDIR /app
COPY --from=build /out/genesis /usr/local/bin/genesis
COPY --from=python-deps --chown=genesis:genesis /app/.venv /app/.venv
COPY agents.d /app/agents.d
COPY rules.d /app/rules.d
ENV PATH="/app/.venv/bin:/usr/local/bin:/usr/bin:/bin"
ENV GENESIS_LISTENER_USER=genesis
EXPOSE 8787
# The orchestrator stays root so it can drop the listener to `genesis`.
USER root
ENTRYPOINT ["/usr/local/bin/genesis"]
CMD ["launch", "-listen", "0.0.0.0:8787", "-agents", "/app/agents.d", "-rules", "/app/rules.d", "-listener-user", "genesis"]
