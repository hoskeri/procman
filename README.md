# procman - procfile runner.

Runs Heroku style [Procfile][] process definitions.

[Procfile]: https://devcenter.heroku.com/articles/procfile

Exists because I need something that's embeddable in other golang applications.
Use https://github.com/ddollar/foreman unless you need this as well.

WIP. Currently missing:

- [ ] any kind of tests or documentation.
- [ ] formation support
- [ ] port allocation
- [ ] dotenv support
- [ ] throttle terminal output/discard logs if terminal is too slow.

## License

procman is licensed under the MIT license.
See LICENSE for the full license text.
