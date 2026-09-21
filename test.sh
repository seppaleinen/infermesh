#!/usr/bin/env bash
set -euo pipefail

SESSION_NAME="infermesh"

# 1. Start a new detached tmux session running router in Pane 0 (Top)
tmux new-session -d -s "$SESSION_NAME" -n "dev-cluster" "./bin/infermesh-router --dev-mode"

# 2. Split horizontally to create Pane 1 (Bottom) for the worker
tmux split-window -v -t "$SESSION_NAME:0" "./bin/infermesh-worker --dev-mode \
  --router http://127.0.0.1:8080 \
  --backend lmstudio --model-path /Users/daveri/.lmstudio/hub/models/openai/gpt-oss-20b"

sleep 3

# 3. Split vertically to create Pane 2 (Right) for a chat completion request
tmux split-window -h -t "$SESSION_NAME:0.1" "curl -X POST http://localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' -d '{\"model\": \"gpt-oss-20b\", \"messages\": [{\"role\": \"user\", \"content\": \"Hello\"}], \"stream\": false}'; exec $SHELL"

# 4. Use tiled layout for clean dynamic resizing across panes
tmux select-layout -t "$SESSION_NAME:0" tiled

# 5. Attach to the session
tmux attach-session -t "$SESSION_NAME"