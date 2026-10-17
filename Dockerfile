FROM node:24.18.0-bookworm-slim AS node-runtime

FROM node-runtime AS dependencies
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci

FROM dependencies AS production-dependencies
RUN npm prune --omit=dev

FROM dependencies AS build
COPY web ./web
COPY src/refinement-defaults.js src/profiles.js ./src/
RUN npm run build

FROM ubuntu:24.04 AS toolchain
COPY --from=node-runtime /usr/local /usr/local
ARG CLAUDE_VERSION=2.1.296
ARG SKAFFOLD_VERSION=v2.18.0
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git openssh-client bash coreutils findutils tar unzip ripgrep jq curl gh python3 python3-venv python3-pip make g++ \
    && rm -rf /var/lib/apt/lists/* \
    && curl -fsSL https://storage.googleapis.com/skaffold/releases/${SKAFFOLD_VERSION}/skaffold-linux-amd64 -o /usr/local/bin/skaffold \
    && chmod 0755 /usr/local/bin/skaffold \
    && npm install -g @anthropic-ai/claude-code@${CLAUDE_VERSION}

FROM toolchain AS schemas
RUN mkdir -p /opt/agent-schemas \
    && skaffold version > /opt/agent-schemas/skaffold-version.txt \
    && skaffold schema get v4beta11 > /opt/agent-schemas/skaffold-v4beta11.json

FROM toolchain AS runtime
WORKDIR /app
COPY --from=production-dependencies /app/node_modules ./node_modules
COPY --from=build /app/public ./public
COPY package.json ./
COPY src ./src
COPY --from=schemas /opt/agent-schemas ./schemas
RUN chmod -R a+rX /app
EXPOSE 8080
CMD ["node", "/app/src/main.js"]

# kubectl is provided by an OCI image volume in controller and worker Pods.
FROM node-runtime AS controller
WORKDIR /app
COPY --from=runtime /app /app
EXPOSE 8080
CMD ["node", "/app/src/main.js"]

FROM toolchain AS runner
ARG CODEX_VERSION=0.162.1
RUN npm install -g @openai/codex@${CODEX_VERSION} && npm cache clean --force
WORKDIR /app
COPY --from=runtime /app /app
EXPOSE 8080
CMD ["node", "/app/src/main.js"]

