# The vmtool command line

vmtool is used by people and by agents. This tool follows the convention below; vmtool's own notes on it follow
the convention.

## Command-line tools

A command-line tool is run by people and by agents: programs that run its commands and read what they print. This
section is the convention such a tool follows, so that both can use it without guessing.

### Output with `--json`

- `--json` on every non-interactive command prints exactly one JSON object on stdout, never a bare array. Lists
  are under a named key, for example `{"items": [...]}`. Human text and progress go to stderr.
- With `--json`, an error is one object on stderr, `{"error":{"code":"...","message":"..."}}`. The exit code is
  non-zero.
- Fields are only added within a major version. Renaming or removing one is a breaking change, recorded in the
  tool's log.

### Exit codes

| Code | Meaning |
|---|---|
| `0` | Success. |
| `1` | Error. |
| `2` | Usage: a bad command, flag or argument. |
| `75` | Try again later, for example while a key request is pending. |

A command that runs another program exits with that program's code, says so in its help, and with `--json` also
reports `exit_code`.

### Help

- `-h` and `--help` print to stdout and exit `0`.
- Help has usage, a description, flags and at least one example.
- The top-level help has an "Agent tips" section:
  - use `--json`;
  - which commands block, and for how long;
  - how keys are obtained;
  - which commands are interactive.
- A command adds its own tip where it has one.

### Interactive commands

Interactive commands (a TUI, a shell, a tunnel, a server) are exempt. With `--json` they exit `2` with
"interactive command; no --json".

### Prompts and fixes

- Nothing prompts when stdin is not a terminal.
- An error that has a fix names the command to run.

### Conformance

A test walks the tool's command tree and checks every command. For each non-exempt command, `--json` is accepted
and the help carries the required parts.

A tool restates this section verbatim in its own `docs/CLI.md`.

## vmtool's notes

### `--json`

`--json` is a flag of the root command, so every command takes it, before or after the command's name
(`vmtool --json list`, `vmtool list --json`). After the target of `exec` and `metal exec`, arguments belong to the
remote command: `vmtool exec hv1/web ls --json` runs `ls --json` on the VM.

A command's result is the API's shape where the API has one (a VM is the REST API's `VMInfo`). A list is under a
named key. The inventory (`vmtool inventory --json`) is the REST API's `Inventory`.

### Error codes

The `code` of the error object is one of the REST API's `Error.code` values (`spec/openapi/vmtool.yaml`):

| Code | When |
|---|---|
| `not_found` | the VM, host, image, template, network or machine does not exist |
| `conflict` | the request conflicts with what is there (a name in use, a full pool) |
| `bad_request` | a bad command line, flag or argument (exit `2`), or a value the command refuses (exit `1`) |
| `unauthorized` | the token gate refused: no `VMTOOL_TOKEN`, no `config.yml`, or a token that does not match |
| `workstation_only` | not used by the command line |
| `store_unavailable` | the command needs the store and none is configured, or it does not answer |
| `host_unreachable` | a configured host's daemon did not answer |
| `host_timeout` | a configured host's daemon did not answer in time |
| `bad_gateway` | a host or guest answered with a failure (for example SSH to the guest failed) |
| `internal` | anything else |

Clients branch on `code`, never on `message`.

### Exit codes

- `0` success; `1` error; `2` usage (an unknown command or flag, the wrong number of arguments, `--json` on an
  interactive command).
- `exec` and `metal exec` run another program and exit with its code. With `--json` they also report it as
  `exit_code`.
- vmtool never exits `75`: no command waits on a key request.

Without `--json`, an error is printed as before: one line on stderr (cobra's `Error:` line and usage for a usage
error).

### Interactive commands

`i` (the TUI), `ssh`, `tunnel`, `server` and `metal ssh` are interactive. They take no `--json`; with it they
exit `2` with "interactive command; no --json".

### Conformance

`internal/cli/clicontract_test.go` walks the real command tree. Every runnable command, except cobra's own `help`
and `completion`, is checked: its `--help` exits `0` on stdout with usage and an example, and either it is
interactive and refuses `--json`, or a run with `--json` against fakes prints exactly one JSON object on stdout and
nothing else.
