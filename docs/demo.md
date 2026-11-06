# Demo recording

The [README GIF](demo.gif) captures the real CLI at
[e920165](https://github.com/mholtzscher/tspreview/commit/e9201653102d5ea7f4348efc6c1123a17f517388),
recorded in a 112-column, 26-row PTY with
[Terminal Control](https://github.com/anomalyco/terminal-control).

It shows file publication, an HTTPS 200 response, directory publication with a
one-hour TTL and JSON output, listing, removal, and an HTTPS 404 after removal.
The directory's HTML and stylesheet were also fetched and checked against the
source files. Authentication was completed before capture; credentials are not
recorded. The existing server was not restarted.

For privacy, the capture shell replaces real tailnet hostnames with
`tspreview-demo.example.ts.net` and filters unrelated previews from the list.
`jq` selects and formats real JSON fields. The shell sets `PREVIEW_URL` to the
latest actual URL for the `curl` checks; this variable is a recording convenience,
not something the CLI sets. Idle time is trimmed and readable holds are added.
No CLI responses are simulated.

Only the GIF is included here. Raw recordings and manifests stay private.
All disposable demo previews were removed afterward; existing previews and the
shared server were preserved.
