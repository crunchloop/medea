# Deploying Medea

Medea must run somewhere that is **not** the cluster it manages (PRD §13 #10) —
a small always-on host, a VM, or a container off-cluster. Two artifacts:

- [`medea.service`](medea.service) — a hardened systemd unit.
- [`Dockerfile`](Dockerfile) — a static binary on distroless.

Both run `medea serve --rollouts`. `--rollouts` enables the executor globally;
each cluster still needs `medea cluster enable-rollouts <name>` before anything
can act (default off). Drop `--rollouts` to run read-only.

See the repo [`README.md`](../README.md) for the full server flags, credential
layout (`<creds-dir>/<cluster>/{talosconfig,kubeconfig}`), and seeding.

## systemd

```sh
install -m0755 medea /usr/local/bin/medea
useradd --system --no-create-home --shell /usr/sbin/nologin medea
install -o medea -g medea -d -m0700 /etc/medea /etc/medea/tls
printf '%s' "<long-random-token>" | install -o medea -g medea -m0600 /dev/stdin /etc/medea/token

# Seed BEFORE starting (server stopped) so state + creds exist:
sudo -u medea medea seed --cluster home \
  --talosconfig ./talosconfig --kubeconfig ./kubeconfig \
  --store /var/lib/medea/medea.db --creds-dir /var/lib/medea/creds

cp medea.service /etc/systemd/system/medea.service
systemctl daemon-reload && systemctl enable --now medea
journalctl -u medea -f
```

`StateDirectory=medea` creates and owns `/var/lib/medea` (0700). TLS is
self-signed on first run if `/etc/medea/tls/{cert,key}.pem` are absent.

## container

```sh
docker build -f deploy/Dockerfile -t medea:dev .

docker run -d --name medea -p 7600:7600 \
  -v medea-state:/var/lib/medea \
  -v "$PWD/etc-medea:/etc/medea:ro" \
  medea:dev
```

Provide `/etc/medea/token` (and optionally pre-seeded TLS) via the mounted
config volume, and seed the state volume first (run `medea seed` in a one-shot
container against the same `/var/lib/medea` volume). Override `CMD` to change
flags.

## clients

Point the CLI at the server:

```sh
export MEDEA_ADDR=host:7600 MEDEA_TOKEN="$(cat /etc/medea/token)" MEDEA_CA=./cert.pem
medea get clusters
```
