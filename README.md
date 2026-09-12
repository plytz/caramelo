<p align="center"><img src="assets/logo/caramelo-icon.png" width="160" alt="caramelo"></p>

# caramelo

caramelo builds, tests, runs, deploys and operates applications so that developers can focus on
their business problem. One binary, one `caramelo.yaml`, one mental model from `git clone` to
production. Every environment caramelo produces is built from the same definition file.

**Status: early prototype.** Nothing about the commands, the config file or the on-disk layout is
stable yet. Binaries for Linux and macOS are on the
[releases page](https://github.com/plytz/caramelo/releases), each with a `MANUAL.md` beside it (the
same text `caramelo manual` prints).

## Goal

caramelo aims to simplify the whole development cycle, aiding the developer from the
coding/exploring phase, through testing, deploying, load testing, performance testing, security,
operation and feature development. It is designed with well-thought-out defaults in place so that it
just works, but it also allows for customization of the architecture to meet a particular need.

## Agents first

caramelo is built for agents first. Most of its users will be AI agents driving it from a shell,
working alongside the developer or autonomously. People can still type the commands and get the same
results but the human experience will be tuned to work through a coding agent.

- **One CLI, machine-readable by default.** Every command has `--json` output, stable exit codes,
  and a progress stream on standard error that can also be JSON, one event per line. An agent can
  drive a deploy and see exactly which step failed.
- **Nothing waits on a terminal.** Anything a person is asked can be answered with a flag, and a
  command with a non-terminal standard input never blocks on a prompt.
- **The manual is part of the tool.** `caramelo manual` is generated from the same definitions the
  commands run on, so it cannot drift, and every example in it is checked against the real flags.
- **Humans get the same commands.** No separate mode, no behaviour that changes because a terminal
  was attached.

## Philosophy

- **One config.** A single `caramelo.yaml` describes the app. Services, dependencies, how to build,
  run, test and check health, what to expose. Local, preview and production are the same
  description with different targets.
- **Convention over configuration.** The stack is detected. The config file is optional for common
  cases and short for the rest.
- **Isolation is the default.** Environments never share mutable state unless asked.
- **Disposable everything.** Any environment or deploy can be recreated from git plus config, so
  creating and destroying must be cheap.
- **Boring, observable, reversible.** Every action says what it did, can be inspected afterwards,
  and can be undone.
- **Progressive depth.** `caramelo up` just works. Every knob underneath is reachable.
- **Local first, then fleet.** Everything works on one machine. The same primitives extend to many.

## Development

Docker is required. The integration tests run caramelo inside containers that stand in for real
machines, so they need a Linux container host with cgroup v2, privileged containers, and
`br_netfilter` available in the host kernel, either built in (as in Docker Desktop's VM) or loaded
as a module (`sudo modprobe br_netfilter` on a Linux host that has not loaded it). A container
shares the host kernel and cannot load a module for it, so a machine's `server setup` needs the host
to have it; the suites check it once the first container is up and stop there if it is missing.
`make check` is everything that must pass before a commit and touches no container. `make
integration` runs the whole integration tier, `SUITE=` narrows it to one suite, and `make
integration-clean` removes every container, network and volume a run left behind. The machine state
each suite asserts is described by the goss specs in `test/integration/goss/`, which goss renders as
Go templates, so a double opening brace may appear in them only as `.Vars.user`, `.Vars.home`,
`.Vars.uid` or `.Vars.arch`.

```sh
make check
make integration
make integration SUITE=api
make integration-clean
```

## End-to-end tests on machines of your own

The same suites run against real machines over ssh instead of containers. You need one to three
boxes, plus Docker on whatever machine runs `go test`.

A box must be a fresh Debian 12/13 or Ubuntu 22.04+ install with nothing on it. No Docker, no
caramelo, no leftovers from an earlier run. It needs an ssh key you hold, passwordless sudo for the
user you log in as, and it must be reachable at the address you write in the inventory both from the
machine running the tests and from the other boxes. The fleet suites join machines to each other
over exactly that address.

The boxes form one caramelo cluster. The first machine in the inventory is the **hub**. Every other
machine is a **node** that joins the hub's fleet. Three machines run the whole test suite. With
fewer, the cases that need more machines skip with a message naming what they wanted.

End-to-end tests require an inventory file in this format:

```json
{
  "version": 1,
  "machines": [
    {"name": "m1", "host": "10.0.0.10", "port": 22, "user": "debian",
     "key": "/home/me/.ssh/caramelo-e2e", "arch": "amd64",
     "host_key": "ssh-ed25519 AAAA..."}
  ]
}
```

`port` defaults to 22 and `host_key` is optional. Give it and the run pins it, leave it out and the
run accepts the key on first contact into a known_hosts file of its own. `arch` (`amd64` or `arm64`)
says which binary to ship.

Suites assume a clean box. Putting a machine back to that state between suites is yours to do. You
can also set `CARAMELO_E2E_RESET` to a command of your own that reinstalls one machine. The tests
append the machine's inventory `name` to it and will re-read the inventory afterwards, in case
something in the reset changes the host key or the architecture.

```sh
make e2e INVENTORY=inventory.json
make e2e INVENTORY=inventory.json SUITE=api
```

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
