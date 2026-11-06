# tspreview

Publish disposable HTML files and static directories to your Tailscale network.
A Go CLI embeds its own [tsnet](https://tailscale.com/kb/1244/tsnet) node: no sudo,
separate Tailscale installation, host Serve configuration, or public hosting.

```sh
tspreview login
tspreview ./hello.html
# https://tspreview-devbox.your-tailnet.ts.net/p/<id>/hello.html

tspreview ./prototype/ --ttl 1h
tspreview list
tspreview rm <id>
tspreview stop
```

## Build

Linux and Go 1.27.3 or newer are required by the pinned Tailscale dependency.

Install into your Go binary directory:

```sh
go install github.com/mholtzscher/tspreview/cmd/tspreview@latest
```

Or build from a checkout:

```sh
go build -o bin/tspreview ./cmd/tspreview
./bin/tspreview --help
```

## Authenticate once per machine and user

```sh
tspreview login
# Follow the authentication URL printed on stderr.
```

This registers a **separate Tailscale device**, normally named
`tspreview-<machine-hostname>`. It does not borrow the host's Tailscale login.
The device may require approval under your tailnet's policies. Enable MagicDNS
and HTTPS certificates in the tailnet's DNS settings, and allow clients to reach
this node on TCP 443 in your access policy. The CLI provisions a certificate
before reporting readiness. The hostname can appear in public certificate
transparency logs; preview contents remain tailnet-only.

You can choose the node's name on login:

```sh
tspreview login --hostname tspreview-workbox --timeout 5m
```

For unattended provisioning, supply `TS_AUTHKEY` through your existing secret
manager/environment, then run `tspreview login`. Do not put keys into generated
HTML, command-line flags, or source control. Subsequent commands reuse saved node
state; they do not need the key. Device approval, key expiry, and access policy
still apply. To reauthenticate, run `tspreview stop` first, then `tspreview login`.

## Publish from an agent

```sh
tspreview ./prototype.html
tspreview ./site/ --ttl 30m --json
```

The command starts one detached server if needed, snapshots the selected content,
and exits. On success, stdout contains only the URL, or a JSON object with
`id`, `name`, `entry`, `created_at`, `expires_at`, and `url`. Diagnostics go to
stderr; daemon diagnostics are stored in `daemon.log`.

Multiple agents can publish concurrently without sharing ports, overwriting
files, or creating a new tailnet device per preview. CLI control uses a private
Unix socket, not a publicly reachable management API. The only production HTTP
listener is inside tsnet.

Cancellation during copying aborts the snapshot before commit. A timeout or lost
response at the commit boundary can still mean publication succeeded. In that
case the CLI reports an unknown outcome. Run `tspreview list --json` with the
same `--state-dir`, check the recent entry's name/time and URL, and remove any
unwanted snapshot before retrying. Retrying is a new publication, not an
exactly-once operation.

- A single file includes **only that file**. Publish a directory to include CSS,
  JavaScript, images, and other relative assets.
- A directory's root URL serves `index.html`. There are no directory listings;
  for a directory without an index, append the desired filename to the URL.
- Files are snapshots, not live views. Republishing produces a new URL.
- Root-relative asset URLs such as `/assets/app.js` are not rewritten. Use
  relative URLs such as `./assets/app.js` in prototypes served under `/p/<id>/`.
- Hidden files/directories are skipped during directory publication. Single
  hidden files, symlinks, and special files are rejected.
- Each snapshot is limited to 100 MiB and 10,000 regular files.
- Default lifetime is 24 hours. Expired previews immediately return 404.
  The running server checks for expired copies roughly once a minute. If stopped,
  cleanup occurs when the server next starts. Browser caches or downloaded
  copies cannot be revoked.

Example JSON output:

```json
{
  "id": "a77726d944d244ee934cb429b67df3102",
  "name": "hello.html",
  "entry": "hello.html",
  "created_at": "2026-11-05T12:00:00Z",
  "expires_at": "2026-11-06T12:00:00Z",
  "url": "https://tspreview-devbox.example.ts.net/p/a77726d944d244ee934cb429b67df3102/hello.html"
}
```

## Manage previews and the server

```sh
tspreview list --json
tspreview rm <id>
tspreview stop
```

`list` and `rm` start the server if it is stopped. `stop` disconnects the node and
leaves unexpired snapshots and login state on disk. Publishing or listing starts
it again. The detached server survives the invoking shell, but does not install
a boot service; after a reboot, run a publishing or listing command to restart it.
It is not a supervised service: a process supervisor, container, or remote-agent
sandbox that kills descendant processes may still stop it.

All commands accept `--state-dir`, `--timeout`, and `--verbose`. Default state is
`$XDG_STATE_HOME/tspreview` or `~/.local/state/tspreview`. Node credentials live
under `node/`; snapshots live separately under `previews/`. These directories are
private to the current OS user. Never publish or share the state directory.
tsnet uses Tailscale's default diagnostic logging behavior, including log uploads
to Tailscale; it does not upload preview files as part of publication.

The control socket uses a hashed per-state-directory name under
`$XDG_RUNTIME_DIR`, falling back to the system temporary directory. The containing
directory is private and ownership-checked. Use the same runtime-directory
environment when controlling an existing server.

To uninstall completely, stop the server using its original `--state-dir`, remove
that state directory and the executable, and remove the registered device from
the Tailscale admin console if you no longer need it.

## Trust model

Tailnet access rules protect the node. Preview IDs are not authentication, and
any allowed peer with a URL can fetch it. All previews on a node share a browser
origin. Use this for your own generated prototypes, not for safely executing
hostile HTML or isolating different users. There is no injected restrictive CSP
that would break prototype scripts. Publishing a directory includes its visible
regular files; inspect it for secrets first.

## Development checks

```sh
go test ./...
go test -race ./...
go vet ./...
go build -o bin/tspreview ./cmd/tspreview
```

Tests exercise snapshot persistence, expiry, traversal prevention, unsafe-source
rejection, concurrent publication, Unix-socket control, a detached subprocess,
and server restart. Their network adapter binds loopback **only in test code**;
they do not authenticate to Tailscale. Real tsnet authentication, certificate
issuance, and access from another tailnet peer require a separate live check.
