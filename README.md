# ownstack

`ownstackctl` builds and maintains a Kubernetes platform on your own Ubuntu server from one file, `ownstack.yaml`: k3s, ingress with TLS, an image registry, CI/CD for your app repositories, and environment namespaces.

## Install

On the server, as root:

```bash
curl -fsSL https://raw.githubusercontent.com/getownstack/ownstack/main/install.sh | bash
```

`install.sh` installs the latest release from this repository and verifies its checksum. To install another release, set `OWNSTACKCTL_VERSION` to its tag, for example `| OWNSTACKCTL_VERSION=ownstackctl-vX.Y.Z bash`.

Then write `ownstack.yaml` (start from [examples/](examples/)) and run `ownstackctl apply -f ownstack.yaml`.

## Documentation

- [docs/ownstackctl.md](docs/ownstackctl.md): commands, the manifest, providers, deploying apps, upgrades.
- [examples/](examples/): one manifest per preset.
- [AGENTS.md](AGENTS.md): architecture and development notes.
