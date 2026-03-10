#!/usr/bin/env bash
set -euo pipefail

MASTER=""
TOKEN=""
AGENT_ID=""
ALIAS=""
TAGS=""
REPO_URL="https://github.com/Vincentkeio/kokoroagent.git"
REF="main"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --master) MASTER="$2"; shift 2 ;;
    --token) TOKEN="$2"; shift 2 ;;
    --agent-id) AGENT_ID="$2"; shift 2 ;;
    --alias) ALIAS="$2"; shift 2 ;;
    --tags) TAGS="$2"; shift 2 ;;
    --repo-url) REPO_URL="$2"; shift 2 ;;
    --ref) REF="$2"; shift 2 ;;
    *) echo "unknown arg: $1" >&2; exit 1 ;;
  esac
done

if [[ -z "$MASTER" || -z "$TOKEN" || -z "$AGENT_ID" ]]; then
  echo "--master, --token and --agent-id are required" >&2
  exit 1
fi

case "$MASTER" in
  ws://*|wss://*) ;;
  *) echo "invalid --master, expected ws:// or wss:// URL" >&2; exit 1 ;;
esac

need_cmd() { command -v "$1" >/dev/null 2>&1; }

if ! need_cmd git; then
  if need_cmd apt-get; then
    apt-get update && apt-get install -y git ca-certificates curl
  elif need_cmd yum; then
    yum install -y git ca-certificates curl
  elif need_cmd dnf; then
    dnf install -y git ca-certificates curl
  elif need_cmd apk; then
    apk add --no-cache git ca-certificates curl
  else
    echo "please install git manually" >&2
    exit 1
  fi
fi

if ! need_cmd go; then
  if need_cmd apt-get; then
    apt-get update && apt-get install -y golang-go
  elif need_cmd yum; then
    yum install -y golang
  elif need_cmd dnf; then
    dnf install -y golang
  elif need_cmd apk; then
    apk add --no-cache go
  else
    echo "please install Go 1.21+ manually" >&2
    exit 1
  fi
fi

mkdir -p /opt /etc/kokoro-agent
rm -rf /opt/kokoro-agent
git clone --depth 1 --branch "$REF" "$REPO_URL" /opt/kokoro-agent

if [[ ! -d /opt/kokoro-agent/cmd/kokoro-agent ]]; then
  echo "repo is missing cmd/kokoro-agent; check --repo-url or repository contents" >&2
  exit 1
fi

cd /opt/kokoro-agent
go mod tidy
go build -o /usr/local/bin/kokoro-agent ./cmd/kokoro-agent
install -Dm644 systemd/kokoro-agent.service /etc/systemd/system/kokoro-agent.service

MASTER="$MASTER" TOKEN="$TOKEN" AGENT_ID="$AGENT_ID" ALIAS="$ALIAS" TAGS="$TAGS" python3 - <<'PYCONF'
import json, os, pathlib
cfg = {
    "master": os.environ["MASTER"],
    "token": os.environ["TOKEN"],
    "agent_id": os.environ["AGENT_ID"],
    "alias": os.environ.get("ALIAS", ""),
    "tags": os.environ.get("TAGS", "")
}
pathlib.Path("/etc/kokoro-agent").mkdir(parents=True, exist_ok=True)
pathlib.Path("/etc/kokoro-agent/config.json").write_text(
    json.dumps(cfg, ensure_ascii=False, indent=2) + "\n",
    encoding="utf-8"
)
PYCONF

systemctl daemon-reload
systemctl enable --now kokoro-agent
echo "kokoro-agent installed"
