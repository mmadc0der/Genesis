# syntax=docker/dockerfile:1.7

FROM node:22-bookworm AS web
WORKDIR /src
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM rust:1.85-bookworm AS eventer-build
WORKDIR /src/eventer
COPY submodules/eventer ./
RUN cargo build --release -p eventer --lib

FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY pkg ./pkg
COPY submodules/eventer/include ./submodules/eventer/include
COPY --from=eventer-build /src/eventer/target/release/libeventer.a ./submodules/eventer/target/release/libeventer.a
RUN CGO_ENABLED=1 go build -o /out/genesis ./cmd/genesis

FROM python:3.12-bookworm AS python-deps
COPY --from=ghcr.io/astral-sh/uv:0.6.14 /uv /usr/local/bin/uv
WORKDIR /app
COPY pyproject.toml uv.lock ./
RUN uv sync --locked --no-dev --compile-bytecode

FROM python:3.12-bookworm
RUN apt-get update \
	&& apt-get install -y --no-install-recommends openssh-client git \
	&& rm -rf /var/lib/apt/lists/*
RUN groupadd --gid 65532 genesis \
	&& useradd --create-home --uid 65532 --gid 65532 --shell /usr/sbin/nologin genesis \
	&& mkdir -p /var/lib/genesis/config /var/lib/genesis/data /var/lib/genesis/credentials /var/lib/genesis/secrets /usr/share/genesis/defaults /usr/share/genesis/web /usr/share/genesis/schema /etc/genesis/providers.d
WORKDIR /app
COPY --from=build /out/genesis /usr/local/bin/genesis
COPY --from=python-deps /app/.venv /app/.venv
COPY --from=web /src/dist /usr/share/genesis/web
COPY agents.d /usr/share/genesis/defaults/agents.d
COPY rules.d /usr/share/genesis/defaults/rules.d
COPY schema /usr/share/genesis/schema
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod 0755 /usr/local/bin/genesis /usr/local/bin/docker-entrypoint.sh \
	&& chmod -R a+rX /app/.venv /usr/share/genesis/defaults /usr/share/genesis/web /usr/share/genesis/schema \
	&& chmod -R go-w /app/.venv /usr/share/genesis/defaults /usr/share/genesis/web \
	&& chown root:root /etc/genesis \
	&& chown root:genesis /etc/genesis/providers.d \
	&& chmod 0755 /etc/genesis \
	&& chmod 0750 /etc/genesis/providers.d \
	&& chown -R genesis:genesis /var/lib/genesis/config /var/lib/genesis/data \
	&& chown root:root /var/lib/genesis /var/lib/genesis/credentials /var/lib/genesis/secrets \
	&& chmod 0755 /var/lib/genesis \
	&& chmod 0700 /var/lib/genesis/credentials /var/lib/genesis/secrets
ENV PATH="/app/.venv/bin:/usr/local/bin:/usr/bin:/bin"
ENV GENESIS_LISTENER_USER=genesis
ENV PYTHONDONTWRITEBYTECODE=1
EXPOSE 8790
# Orchestrator stays root so it can drop the listener to `genesis`,
# reconcile dedicated agent OS users, and exec those agents as their uid.
# The listener port is internal to the Compose network. The published UI is
# the control process; see compose.yaml.
USER root
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["launch", "-listen", "0.0.0.0:8787", "-agents", "/var/lib/genesis/config/agents.d", "-rules", "/var/lib/genesis/config/rules.d", "-repos", "/var/lib/genesis/config/repos.d", "-providers", "/etc/genesis/providers.d", "-data", "/var/lib/genesis/data", "-credentials", "/var/lib/genesis/credentials", "-secrets", "/var/lib/genesis/secrets", "-listener-user", "genesis"]
