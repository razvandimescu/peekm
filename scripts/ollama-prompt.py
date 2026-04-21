#!/usr/bin/env python3
"""Send a prompt to the Ollama model used by peekm summarization."""

import sys
import json
import urllib.request

OLLAMA_URL = "http://localhost:11434/api/chat"
MODEL = "qwen3.5:27b-q8_0"


def main():
    prompt = " ".join(sys.argv[1:]) if len(sys.argv) > 1 else input("prompt: ")
    if not prompt.strip():
        sys.exit("empty prompt")

    body = json.dumps({
        "model": MODEL,
        "stream": False,
        "think": False,
        "messages": [{"role": "user", "content": prompt}],
    }).encode()

    req = urllib.request.Request(OLLAMA_URL, data=body, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=300) as resp:
        data = json.load(resp)

    print(data["message"]["content"])


if __name__ == "__main__":
    main()
