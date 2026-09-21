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
RUN useradd --create-home --uid 65532 --shell /usr/sbin/nologin genesis \
	&& mkdir -p /var/lib/genesis/config /var/lib/genesis/data /usr/share/genesis/defaults
WORKDIR /app
COPY --from=build /out/genesis /usr/local/bin/genesis
COPY --from=python-deps /app/.venv /app/.venv
COPY agents.d /usr/share/genesis/defaults/agents.d
COPY rules.d /usr/share/genesis/defaults/rules.d
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod 0755 /usr/local/bin/genesis /usr/local/bin/docker-entrypoint.sh \
	&& chmod -R a+rX /app/.venv /usr/share/genesis/defaults \
	&& chmod -R go-w /app/.venv /usr/share/genesis/defaults \
	&& chown -R genesis:genesis /var/lib/genesis
ENV PATH="/app/.venv/bin:/usr/local/bin:/usr/bin:/bin"
ENV GENESIS_LISTENER_USER=genesis
ENV PYTHONDONTWRITEBYTECODE=1
EXPOSE 8787
# Orchestrator stays root so it can drop the listener to `genesis`.
USER root
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["launch", "-listen", "0.0.0.0:8787", "-agents", "/var/lib/genesis/config/agents.d", "-rules", "/var/lib/genesis/config/rules.d", "-data", "/var/lib/genesis/data", "-listener-user", "genesis"]
