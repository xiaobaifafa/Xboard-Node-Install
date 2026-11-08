# Xboard-Node Installer

This repository contains the public one-command installer for the Xboard-Node test build.

## Supported systems

Linux with systemd on amd64/x86_64 or arm64/aarch64.

## Install

Get the panel URL, node communication token, and numeric node ID from your Xboard panel. Run as root:

```bash
curl -fsSL https://raw.githubusercontent.com/xiaobaifafa/Xboard-Node-Install/main/scripts/install-test-node.sh | bash -s -- --mode node --panel https://panel.example.com/ --token 'YOUR_NODE_TOKEN' --node-id 1 --kernel xray
```

Replace the example panel URL, token, and node ID with your own values.

- `--kernel xray`: use Xray (default).
- `--kernel singbox`: use sing-box.

The installer detects the CPU architecture, downloads the matching binaries from this repository's GitHub Release `v1`, verifies their SHA256 checksums, installs them, generates the node configuration, and registers the `xboard-node` systemd service.

## Check status

```bash
systemctl status xboard-node --no-pager
journalctl -u xboard-node -f
xbctl status
```

A running service alone does not prove that the node is connected. Check for `ws connected`, successful kernel startup, and node configuration update in the logs.

## Upgrade or reinstall

Run the same installation command again. Back up `/etc/xboard-node/config.yml` and `/etc/xboard-node/node.env` first if you need to preserve the current configuration.

When publishing a newer node build, maintainers update the pinned Release URL and four SHA256 values in `scripts/install-test-node.sh`. The user-facing installation command remains unchanged.
