#!/usr/bin/env bash
# PostToolUse: format the Go file Claude just edited.
FILE=$(python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get(\"tool_input\",{}).get(\"file_path\",\"\"))" 2>/dev/null || true)
case "$FILE" in
  *.go) gofmt -w "$FILE" ;;
esac
exit 0
