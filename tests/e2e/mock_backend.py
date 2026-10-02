#!/usr/bin/env python3
"""Mock OpenAI-compatible backend for e2e firewall failover tests.

Usage:
  python3 mock_backend.py --port <port> --mode healthy
  python3 mock_backend.py --port <port> --mode firewall

Modes:
  healthy:   Responds to GET /v1/models and POST /v1/chat/completions
             with valid OpenAI-compatible JSON.
  firewall:  Binds the port but never accepts connections, simulating a
             host/port that refuses all connections (connection refused).
             The worker's HTTP client dials, gets ECONNREFUSED, and the
             router treats that as a retryable transport error.
"""

import argparse
import json
import socket
import time
from http.server import BaseHTTPRequestHandler, HTTPServer


def run_healthy(port):
    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            if self.path == "/v1/models":
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps({
                    "data": [
                        {
                            "id": "other-model",
                            "object": "model",
                            "loaded": True,
                        }
                    ]
                }).encode())
            else:
                self.send_response(404)
                self.end_headers()

        def do_POST(self):
            if self.path == "/v1/chat/completions":
                length = int(self.headers.get("Content-Length", 0))
                _ = self.rfile.read(length)
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps({
                    "id": "mock-1",
                    "object": "chat.completion",
                    "created": 1,
                    "model": "other-model",
                    "choices": [
                        {
                            "index": 0,
                            "message": {
                                "role": "assistant",
                                "content": "failover-verified",
                            },
                            "finish_reason": "stop",
                        }
                    ],
                    "usage": {
                        "prompt_tokens": 1,
                        "completion_tokens": 1,
                        "total_tokens": 2,
                    },
                }).encode())
            else:
                self.send_response(404)
                self.end_headers()

        def log_message(self, format, *args):
            pass  # silence access logs

    server = HTTPServer(("127.0.0.1", port), Handler)
    server.serve_forever()


def run_firewall(port):
    # Bind the port but never listen or accept — every dial gets
    # ECONNREFUSED, which the worker's HTTP client surfaces as a transport
    # error and the router classifies as retryable.
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.bind(("127.0.0.1", port))
    try:
        time.sleep(999999)
    finally:
        s.close()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--port", type=int, required=True)
    parser.add_argument("--mode", choices=["healthy", "firewall"], required=True)
    args = parser.parse_args()

    if args.mode == "healthy":
        run_healthy(args.port)
    else:
        run_firewall(args.port)


if __name__ == "__main__":
    main()