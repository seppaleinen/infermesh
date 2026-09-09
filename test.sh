#!/usr/bin/env bash
set -euo pipefail

SESSION_NAME="infermesh"

# 1. Start a new detached tmux session running the router in Pane 0 (Top)
tmux new-session -d -s "$SESSION_NAME" -n "dev-cluster" "./bin/infermesh-router --dev-mode"

# 2. Split vertically to create Pane 1 (Bottom)
tmux split-window -v -t "$SESSION_NAME:0" "./bin/infermesh-worker --dev-mode --router http://localhost:8080 \
  --model-path /Users/daveri/.lmstudio/hub/models/openai/gpt-oss-20b \
  --backend lmstudio"

sleep 3

# 3. Split the bottom pane horizontally to create Pane 2 (Bottom-Right)
tmux split-window -h -t "$SESSION_NAME:0.1" "curl -X POST http://localhost:8080/v1/chat/completions \
  -H 'Content-Type: application/json' -d '{\"model\": \"gpt-oss-20b\", \"messages\": [{\"role\": \"user\", \"content\": \"Hello\"}], \"stream\": false}'; exec $SHELL"

# 4. Use tiled layout for clean dynamic resizing across panes
tmux select-layout -t "$SESSION_NAME:0" tiled

# 5. Attach to the session
tmux attach-session -t "$SESSION_NAME"
