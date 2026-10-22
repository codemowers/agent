FROM node:24.22.0-trixie-slim AS node-runtime

FROM node-runtime AS dependencies
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci

FROM dependencies AS production-dependencies
RUN npm prune --omit=dev

FROM ubuntu:26.04 AS toolchain
COPY --from=node-runtime /usr/local /usr/local
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git openssh-client bash coreutils findutils tar unzip ripgrep jq curl python3 python3-venv python3-pip make g++ \
    && rm -rf /var/lib/apt/lists/*
# Pinned static CLIs for the build host's architecture. runner/checksums.sha256
# holds each file's reviewed checksum; a missing line fails the build.
COPY runner/checksums.sha256 /tmp/checksums.sha256
RUN set -e; arch="$(dpkg --print-architecture)"; cd /tmp; \
    fetch() { curl -fsSL "$1" -o "$2"; grep "  $2\$" checksums.sha256 | sha256sum -c -; }; \
    fetch "https://github.com/cli/cli/releases/download/v2.102.0/gh_2.102.0_linux_${arch}.tar.gz" "gh_2.102.0_linux_${arch}.tar.gz"; \
    tar -xzf "gh_2.102.0_linux_${arch}.tar.gz"; install -m 0755 "gh_2.102.0_linux_${arch}/bin/gh" /usr/local/bin/gh; \
    fetch "https://dl.gitea.com/tea/0.16.0/tea-0.16.0-linux-${arch}" "tea-0.16.0-linux-${arch}"; \
    install -m 0755 "tea-0.16.0-linux-${arch}" /usr/local/bin/tea; \
    fetch "https://get.helm.sh/helm-v3.22.1-linux-${arch}.tar.gz" "helm-v3.22.1-linux-${arch}.tar.gz"; \
    tar -xzf "helm-v3.22.1-linux-${arch}.tar.gz"; install -m 0755 "linux-${arch}/helm" /usr/local/bin/helm; \
    fetch "https://storage.googleapis.com/skaffold/releases/v2.25.0/skaffold-linux-${arch}" "skaffold-v2.25.0-linux-${arch}"; \
    install -m 0755 "skaffold-v2.25.0-linux-${arch}" /usr/local/bin/skaffold; \
    rm -rf /tmp/*
# Claude Code and Codex are pinned in runner/package.json so dependency
# tooling can update them.
COPY runner/package.json runner/package-lock.json /opt/agent-clis/
RUN cd /opt/agent-clis && npm ci --omit=dev && npm cache clean --force \
    && ln -s /opt/agent-clis/node_modules/.bin/claude /opt/agent-clis/node_modules/.bin/codex /usr/local/bin/

FROM toolchain AS runtime
WORKDIR /app
COPY --from=production-dependencies /app/node_modules ./node_modules
COPY package.json ./
COPY src/task-worker-main.js src/task-worker.js src/identity.js src/child-signals.js src/mcp-auth.js src/http.js src/kube-env.js src/metrics.js ./src/
RUN chmod -R a+rX /app
EXPOSE 8080
CMD ["node", "/app/src/task-worker-main.js"]

FROM node-runtime AS mailbox
WORKDIR /app
COPY --from=production-dependencies /app/node_modules ./node_modules
COPY package.json ./
COPY mailbox ./mailbox
RUN chmod -R a+rX /app
USER node
CMD ["node", "/app/mailbox/imap.js"]

FROM toolchain AS runner
WORKDIR /app
COPY --from=runtime /app /app
EXPOSE 8080
CMD ["node", "/app/src/task-worker-main.js"]

