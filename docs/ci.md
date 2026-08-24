# Continuous integration

The monorepo keeps component checks independent and adds one whole-appliance contract gate:

- `router.yaml` runs only when the Go router or its root command changes.
- `crw.yaml` runs only when the Rust component or its root command changes.
- `appliance.yaml` runs for every push and pull request. It validates packaging and Compose, builds the router, and exercises it against a deterministic local upstream without contacting Firecrawl Cloud.

The root `make check` command is the local equivalent of the component and packaging gates. Its Rust invocation disables incremental artifacts and debugger symbols to keep combined checks within bounded disk usage; component developers can still use `crw/Makefile` directly when debugger artifacts are useful.

## Firewire status

Forgejo Actions is not currently enabled on `git.firewire.cc`. These workflow files define and review the intended automation, but their presence must not be interpreted as an executed gate. Until the server enables Actions, release evidence comes from `make check`, isolated appliance validation, and the recorded Hermes regression artifacts.
