# kokoro-agent

Go agent for the Kokoro PHP master daemon.

## Features

- Compatible with `masterd/server.php` hello/config/metrics/tcpping_batch protocol
- Linux metrics collection from `/proc`
- Auto reconnect
- Systemd install script
- Optional TCP ping worker driven by master config

## Build

```bash
go build -o kokoro-agent ./cmd/kokoro-agent
```

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/Vincentkeio/kokoroagent/main/scripts/install.sh | bash -s -- \
  --master wss://hub.spacelite.top/agent/ws \
  --token YOUR_AGENT_TOKEN \
  --agent-id YOUR_AGENT_ID \
  --alias my-node \
  --repo-url https://github.com/Vincentkeio/kokoroagent.git
```
