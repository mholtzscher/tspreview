# Usage guide

## Authentication

`tspreview login` creates its own Tailscale device; it does not reuse the host's
login. Device approval, key expiry, and tailnet access rules apply. The CLI
provisions an HTTPS certificate before reporting readiness. The device hostname
can appear in public certificate transparency logs; content remains tailnet-only.

Choose a hostname or allow more time for authentication:

```sh
tspreview login --hostname tspreview-workbox --timeout 5m
```

For unattended setup, supply `TS_AUTHKEY` through your secret manager or
environment, then run `tspreview login`. Subsequent commands reuse saved state and
do not need the key. Never put keys in generated HTML, flags, or source control.
To reauthenticate, run `tspreview stop` before `tspreview login`.

## Publish snapshots

A single-file publication includes only that file. Publish its containing
directory to include CSS, JavaScript, images, and other assets. Root-relative
paths such as `/assets/app.js` are not rewritten; use `./assets/app.js` instead.
For a directory without `index.html`, append the desired filename to its URL.
Republishing creates a new snapshot and URL rather than updating an existing one.

With `--json`, publication returns `id`, `name`, `entry`, `created_at`,
`expires_at`, and `url`. `list` and `rm` also support `--json`.

Cancellation during copying aborts before commit. A lost response at the commit
boundary can still mean publication succeeded. If the CLI reports an unknown
outcome, run `tspreview list --json` with the same `--state-dir`. Check recent
entries by name, time, and URL before retrying, and remove unwanted copies.
Retries create new publications; they are not exactly-once operations.

## Server, state, and logs

One detached process owns the tsnet HTTPS listener and preview registry. CLI
commands control it through a private Unix socket, not a network-accessible
management API. A supervisor, container, or agent sandbox that kills descendant
processes can still stop the server.

Default state is `$XDG_STATE_HOME/tspreview` or `~/.local/state/tspreview`:

- `node/` holds node credentials; `previews/` holds snapshots.
- `daemon.log` records server output. These paths are private to the OS user.
- `--verbose` enables tsnet diagnostics when starting the server. Stop an existing
  server first if you need to change its logging level.

For the default path when `XDG_STATE_HOME` is unset:

```sh
tail -f ~/.local/state/tspreview/daemon.log
```

tsnet uses Tailscale's default diagnostic logging, including uploads to Tailscale.
Publication does not upload preview files to a hosting service.

The socket's private directory is under `$XDG_RUNTIME_DIR`, falling back to the
system temporary directory. Use the same runtime-directory environment and
`--state-dir` when controlling an existing server.

## Expiration and removal

Expired URLs immediately return 404. The running server checks for expired
copies roughly once a minute. When stopped, cleanup waits until the next start.
Downloaded or browser-cached copies cannot be revoked.

`tspreview stop` disconnects the node but preserves login and unexpired snapshots.
To uninstall completely, stop the server with its original `--state-dir`, remove
that state directory and the executable, and remove its registered device from
the Tailscale admin console.

## Verification

The automated tests cover snapshots, expiry, path containment, concurrent callers,
detached processes, cancellation, TLS wrapping, and restart. Their network
adapters bind loopback only in test code. Real Tailscale authentication,
certificate issuance, and access from another tailnet peer require a live check.
