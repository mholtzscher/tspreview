# Proxy running apps

[CLI recording and verification scope](proxy-demo.md).

Run `tspreview login` once, then start a proxy beside your development app:

```sh
tspreview proxy http://127.0.0.1:3000
# https://tspreview-devbox.example.ts.net:18234
```

The URL above is illustrative. The command prints the actual URL after binding
its HTTPS listener, then stays foreground. Ctrl-C or process termination removes
that proxy. The shared server, snapshots, and other worktrees keep running.
The proxy can start before its upstream is ready. Requests return 502 while the
upstream is unavailable and recover when the app returns.

After upgrading tspreview, stop an old shared server with `tspreview stop` before
starting a proxy. This interrupts all serving but preserves login, snapshots,
and saved port assignments.

## Worktrees and ports

Mise or your app still owns **local ports**. If two apps both try to bind
`127.0.0.1:3000`, tspreview cannot resolve that collision. Pass each app's assigned
local port as the upstream.

tspreview assigns **tailnet HTTPS ports** on one shared tsnet device:

- Identity is the canonical absolute Git worktree root plus `--name` (`web` by
  default). Outside Git, the current directory is the default identity root.
- `--worktree PATH` selects a root explicitly. Symbolic links resolve to the same
  identity. Run from the same root outside Git, or select it explicitly.
- Each identity retains a port in **10000–59999**, including across upstream port
  changes and server restarts. Colliding candidates get another unused saved port.
  Port 10080 is excluded because browsers block it.
- `--name api` adds an independently managed app within the same worktree.
- A second live registration for the same identity fails. Stop the old foreground
  process before replacing its upstream or extra routes.
- Moving a worktree changes its identity. Changing state directories creates
  independent nodes and assignments. Saved assignments persist after worktree
  deletion; they do not keep listeners open.

Allow your clients to reach the tspreview device on TCP 10000–59999 in tailnet
access rules. Certificates use the same MagicDNS hostname as snapshots on 443.
There are no extra Tailscale devices, DNS aliases, host Serve changes, or public
Funnel endpoints. Production listeners bind through tsnet, not host TCP ports.

## Mise daemon example for Hearth

Hearth already assigns worktree-specific ports with `port.auto`. Add a daemon
that consumes those variables without requesting a local port of its own:

```toml
[daemons.sim-preview]
run = "exec tspreview proxy http://127.0.0.1:$SIM_WEB_PORT --name web --route /nats=http://127.0.0.1:$(($SIM_NATS_PORT + 1))"
proxy = false
depends = ["sim-web"]
ready_cmd = "tspreview proxies --json | jq -e --arg root \"$(realpath .)\" '.[] | select(.worktree == $root and .name == \"web\")' >/dev/null"
```

Add `sim-preview` to the existing `simulator-stack` daemon group and include it in
the `simulator-stop` task before the upstream daemons. Authenticate tspreview
before starting the stack. Run `tspreview proxies` to find each worktree's URL.

The shared server remains a detached process. The foreground Mise daemon owns
only its proxy lease. If the server stops, the foreground command exits with an
error instead of pretending to serve. Restart that daemon to register again.
`--timeout` limits startup, not the lifetime of an established proxy.

### Browser-facing WebSockets

Hearth's Vite server already forwards Core's `/v1`, readiness, and NATS monitoring
requests. The default upstream therefore only needs to point at `SIM_WEB_PORT`.
The extra `/nats` route forwards WebSocket upgrades to NATS, stripping `/nats`.

Hearth currently supplies a browser default of `ws://127.0.0.1:<NATS-port+1>`.
That points to the browser's own machine when opened remotely, and an HTTPS page
needs a secure `wss://` connection. In the dashboard's NATS page, save the override
`wss://<tspreview-hostname>:<assigned-port>/nats`. It takes precedence over the
loopback default and is also used by Device Facts. This is a manual setting for
each browser origin; tspreview does not rewrite JavaScript or Hearth config.

For automatic selection, Hearth would need to derive a same-origin WSS URL and
use `/nats` locally too. That is a separate Hearth change, not required to run
the proxy with the saved override.

Hearth's NATS WebSocket (+1) and monitoring (+4000) offsets are not separately
reserved by Mise. Their existing collision risk remains outside tspreview.

## Extra routes and forwarding

```sh
tspreview proxy http://127.0.0.1:5173 \
  --route /api=http://127.0.0.1:8080 \
  --route /events=http://127.0.0.1:4223 \
  --json
```

Routes match whole path segments, longest prefix first. `/api/items` forwards
as `/items`; `/api` forwards as `/`; `/api-other` stays on the default upstream.
Use prefixes containing letters, digits, underscores, and hyphens, with no trailing
slash. The default upstream receives every other path unchanged.

Upstreams must be HTTP/HTTPS origins on `localhost` or a loopback IP, without
credentials, paths, queries, or fragments. Use `http://` even for a plain WebSocket
server; upgrades become `wss://` on the public HTTPS endpoint. HTTPS upstreams
require a certificate trusted by the host. No insecure TLS bypass is provided.
`localhost` connects directly to IPv4 or IPv6 loopback, without DNS resolution.

The proxy preserves methods, bodies, escaped paths, query strings, streaming,
and WebSocket upgrades. It replaces incoming forwarding headers with
`X-Forwarded-For`, `X-Forwarded-Host`, and `X-Forwarded-Proto`. The upstream Host
header uses the configured loopback origin. The browser Origin header is not
rewritten; apps that validate origins must allow their public proxy origin.

Vite HMR normally uses the page's public port through the WebSocket proxy. Remove
hardcoded HMR host/port overrides or point them at the public URL. Prefer a
specific public hostname allowlist if the app requires one; do not disable host
or origin validation globally.

There is no HTML, absolute redirect URL, cookie, or response-body rewriting.
Apps that advertise `localhost` URLs must be configured for their public URL.
Extra routes strip their prefix, so upstream root-relative links still refer to
the default app unless that app's routing agrees.

## Inspection and safety

```sh
tspreview proxies          # Active app registrations only
tspreview proxies --json   # id, worktree, name, upstream, routes, port, url
tspreview list             # Static snapshots only
tspreview stop             # Stop all serving, including every proxy
```

`proxy --json` prints one registration object and then waits. Proxies have no
TTL. Stopping a foreground command also closes its active HTTP and WebSocket
connections; already-downloaded or cached data cannot be revoked.

Tailnet access rules remain the access boundary. Dev APIs may be unauthenticated,
and the NATS route exposes the broker's browser interface to permitted peers.
Only expose services you intend those peers to use. Different ports are separate
browser origins, but cookies ignore ports and sibling origins are not a security
sandbox. Use trusted apps; avoid sensitive cookies scoped to the shared hostname.

If `proxy-ports.json` is invalid, proxy commands fail with an explicit error while
snapshots remain available. Repair the file and restart the shared server to
restore proxies. Deleting it discards the stable URL assignments.
