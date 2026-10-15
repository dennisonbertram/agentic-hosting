# README assets

| File | What |
|------|------|
| `demo.gif` | Animated terminal recording of a real local `ah --dev` loop (register → git deploy → logs → running). |
| `demo.cast` | Source [asciinema](https://asciinema.org/) v2 cast. Render with `agg demo.cast demo.gif`. |

The GIF is not a mock. Commands were run against `ah --dev` on `127.0.0.1:8080` with gVisor verified; the git deploy used Nixpacks on `https://github.com/hashicorp/http-echo`. Capture notes: [docs/discoverability/README.md](../discoverability/README.md).
