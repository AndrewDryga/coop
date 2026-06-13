# The agent box: Node + Claude Code + Codex + Gemini, plus the ACP adapters so
# editors like Zed can drive the in-box agent. Runs as a non-root user.
# `agent build` uses this if you copy it to ./Dockerfile.agent in a repo;
# otherwise it builds an identical image from a built-in definition.
FROM node:22
RUN npm install -g \
      @anthropic-ai/claude-code \
      @openai/codex \
      @google/gemini-cli \
      @zed-industries/claude-code-acp \
      @zed-industries/codex-acp \
 && git config --system --add safe.directory '*'
USER node
WORKDIR /workspace
