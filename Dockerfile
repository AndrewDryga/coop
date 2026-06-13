# The agent box: Node + Claude Code + Codex + Gemini, as a non-root user.
# `agent build` uses this if you copy it to ./Dockerfile.agent in a repo;
# otherwise it builds an identical image from a built-in definition.
FROM node:22
RUN npm install -g \
      @anthropic-ai/claude-code \
      @openai/codex \
      @google/gemini-cli \
 && git config --system --add safe.directory /workspace
USER node
WORKDIR /workspace
