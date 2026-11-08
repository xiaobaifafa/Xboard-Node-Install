# xboard-node

Node backend for [Xboard](https://github.com/cedar2025/Xboard). Supports \`sing-box\` / \`xray-core\` dual kernels.

> **Disclaimer**: This project is for educational and learning purposes only.

## Features

- Protocols: V2Ray family, Trojan, Shadowsocks, Hysteria2, TUIC, AnyTLS
- Sync: WebSocket push + REST polling dual channel
- User controls: speed limit, device limit, alive-IP tracking, hot update
- Deploy modes: node mode, machine mode, standalone mode
- Multi-instance: single process binding multiple panels / nodes

## Install

### Docker

\`\`\`bash
docker run -d --restart=always --network=host \\
  -e apiHost=https://panel.com -e apiKey=TOKEN -e nodeID=1 \\
  ghcr.io/cedar2025/xboard-node:latest
\`\`\`

### Docker Compose

\`\`\`bash
git clone -b compose --depth 1 https://github.com/cedar2025/xboard-node.git
cd xboard-node
vim config/config.yml   # set panel.url / token / node_id
docker compose up -d
\`\`\`

### Installer (upstream release, Linux systemd)

The following commands use the upstream project's installer and are separate from the custom test installer documented below.

\`\`\`bash
# Node mode
curl -fsSL https://raw.githubusercontent.com/cedar2025/xboard-node/dev/install.sh | \\
  sudo bash -s -- --mode node --panel https://panel.example.com --token TOKEN --node-id 1

# Machine mode
curl -fsSL https://raw.githubusercontent.com/cedar2025/xboard-node/dev/install.sh | \\
  sudo bash -s -- --mode machine --panel https://panel.example.com --token TOKEN --machine-id 1
\`\`\`

### One-command installer for this fork (Release v1)

This fork provides a one-command installer for the prebuilt Linux binaries in the pinned [GitHub Release v1](https://github.com/xiaobaifafa/Xboard-Node/releases/tag/v1). It automatically detects the CPU architecture, downloads the matching \`xboard-node\` and \`xbctl\` binaries, verifies both files with SHA256, installs them, creates the node configuration, and registers the \`xboard-node\` systemd service. The installer URL is stable: do not add a script-version query such as ?v=....

**Supported systems:** Linux with systemd on \`amd64\` / \`x86_64\` or \`arm64\` / \`aarch64\`.

#### 1. Prepare the node information

Before running the command, get these three values from your Xboard panel:

- **Panel URL:** the panel root address, for example \`https://panel.example.com/\`. Do not append \`/api/v1\`.
- **Node Token:** the communication token for this node.
- **Node ID:** the numeric ID of this node in the panel.

#### 2. Run the installation command

Replace the example panel URL, token, and node ID with your own values. Run the command as shown; it will request root privileges through \`sudo\`.

\`\`\`bash
curl -fsSL https://raw.githubusercontent.com/xiaobaifafa/Xboard-Node/fix/close-user-connections-on-quota/scripts/install-test-node.sh | sudo bash -s -- --mode node --panel https://panel.example.com/ --token 'YOUR_NODE_TOKEN' --node-id 1 --kernel xray
\`\`\`

The installer downloads the correct binaries for the server automatically. You do not need to upload binaries manually or install Go.

#### 3. Select the kernel

- \`--kernel xray\`: use Xray (default).
- \`--kernel singbox\`: use sing-box.

If you use the default Xray kernel, you can omit \`--kernel xray\`.

#### 4. Check whether the node started successfully

The installer prints the service status and recent logs at the end. You can also run:

\`\`\`bash
# Check systemd service status
systemctl status xboard-node --no-pager

# Follow live logs
journalctl -u xboard-node -f

# Check node management CLI
xbctl status
\`\`\`

A running systemd process alone does not prove that the node is connected to the panel. Check the logs for a successful WebSocket connection (\`ws connected\`), kernel startup, and node configuration update.

#### 5. Upgrade or reinstall

Run the same installation command again to download and install the binaries pinned to Release \`v1\`, regenerate the node configuration, and restart the service. Back up \`/etc/xboard-node/config.yml\` and \`/etc/xboard-node/node.env\` first if you need to preserve an existing configuration. When publishing a newer node release, maintainers only need to update \`RELEASE_BASE\` and the four architecture-specific SHA256 values in \`scripts/install-test-node.sh\`; the user-facing installation command stays unchanged.

#### Installer options

| Option | Required | Description |
| --- | --- | --- |
| \`--mode node\` | Yes | Install in node mode |
| \`--panel URL\` | Yes | Xboard panel root URL |
| \`--token TOKEN\` | Yes | Node communication token |
| \`--node-id ID\` | Yes | Numeric node ID |
| \`--kernel xray\` | No | \`xray\` or \`singbox\`; defaults to \`xray\` |

## xbctl

Run \`xbctl\` after installation for help. Common commands:

\`\`\`bash
xbctl list                          # list all instances
xbctl status                        # running status
xbctl bind add-node --panel URL --token TOKEN --node-id 1
xbctl bind add-machine --panel URL --token TOKEN --machine-id 1
xbctl bind remove-node --panel URL --node-id 1
xbctl service restart
\`\`\`

## Configuration

Legacy single-panel config is fully compatible. Appending bindings auto-migrates to \`instances\` format. See \`config.yml.example\`.

## Extensions

- Custom routes: [docs-custom-routes.md](docs-custom-routes.md)
- Custom outbounds: [docs-custom-outbounds.md](docs-custom-outbounds.md)
- DNS providers (ACME DNS-01): [docs-dns-providers.md](docs-dns-providers.md)

## License

MPL-2.0.
