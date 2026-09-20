# procman - procfile runner.

Runs Heroku style [Procfile][procfile] process definitions.

You are probably looking for something like github.com/ddollar/foreman

Exists because I need something that's embeddable in other golang applications.
You probably want https://github.com/ddollar/foreman instead for now.

[procfile]: https://devcenter.heroku.com/articles/procfile

## Usage

```
procman [-f/--procfile PATH] [-w/--workdir PATH] [-e/--env PATH]
        [--formation web=2] [--output auto|term] [--columns N] [--debug]
```

- `-f/--procfile` — path to the Procfile (default: `./Procfile`).
- `-w/--workdir` — working directory for child processes; defaults to the
  Procfile's directory.
- `-e/--env` — dotenv-style env file (planned).
- `--formation` — per-type replica counts, e.g. `web=2` (planned).
- `--output auto|term` — `auto` (default) uses the colored, per-process
  prefixed term handler when stdout is a terminal and a plain text handler
  when piped; `term` forces the term handler (color even when piped).
  Log line colors are drawn from a 16-color palette, or a wider 256-color /
  24-bit palette when `TERM`/`COLORTERM` advertise the capability (whites
  and near-whites are excluded). When stdout is a terminal, terminal echo is
  disabled while procman runs so keystrokes don't smear into the log stream,
  and restored on exit.
- `--columns N` — truncates each term-handler output line to `N` bytes
  (0, the default, uses the terminal width when stdout is a tty; negative
  values disable truncation)
  (default 0 = off).
- `--debug` — debug logging on stderr.

## Building and testing

`make build` compiles the `procman` CLI and the `tools/trebuchet`
benchmark utility once into `_output/$(GOOS)_$(GOARCH)/bin/`. `make test`
then runs the unit tests plus end-to-end integration tests that execute the
built `procman` against the `tests/Procfile.*` fixtures using `trebuchet` as
a stand-in user application (clean exit, failed exit propagation, graceful
interrupt, launch failure, high-volume output).

## Exit status

The first process to exit on its own — cleanly or crashing — brings the
formation down: the remaining processes are terminated (SIGTERM to the whole
process group, then SIGKILL after 1s for anything that ignores it) and
`procman` exits with that process's own exit code (`128+signal` when it died
of a signal). If every process was instead shut down by an interrupt
(`SIGINT`/`SIGTERM`/`SIGHUP`/`SIGQUIT`), `procman` exits 0. A failure to
launch a process exits 1.

## Terminal throttling

Process output is never written to the terminal synchronously on the child's
write path. Each `stdout`/`stderr` stream feeds a bounded in-memory queue
(`writelog.DefaultMaxQueue` = 256 lines) drained by a background worker; when
the queue is full the oldest line is discarded. A slow terminal therefore
back-pressures neither the child process nor its reaping: logs are elided,
process cancellation semantics are unchanged. See `docs/THROTTLING.md`.

`procman`'s own `--max-log-queue` flag was intentionally not exposed: the
exact bound has no user-tunable sweet spot (any reasonable value keeps the
child unblocked), so the CLI keeps the default. Embedders who need to bound
it can pass `writelog.StreamConfig.MaxQueue` per stream (`<= 0` falls back to
`DefaultMaxQueue`). See `docs/THROTTLING.md` §9.

## Work in Progress

I wouldn't recommend using this until most of these are done.

- [ ] Tests, documentation.
- [ ] Formation support - set number of processes per type.
- [ ] Port allocation
- [ ] Support [dotenv][]
- [x] Throttle terminal output/discard logs if terminal is too slow.

## License

procman is licensed under the MIT license.
See LICENSE for the full license text.

