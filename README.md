# procman - procfile runner.

Runs Heroku style [Procfile][procfile] process definitions.

You are probably looking for something like github.com/ddollar/foreman

Exists because I need something that's embeddable in other golang applications.
You probably want https://github.com/ddollar/foreman instead for now.

## Work in Progress

I wouldn't recommend using this until most of these are done.

- [ ] Tests, documentation.
- [ ] Formation support - set number of processes per type.
- [ ] Port allocation
- [ ] Support [dotenv][]
- [ ] Throttle terminal output/discard logs if terminal is too slow.

## License

procman is licensed under the MIT license.
See LICENSE for the full license text.

[procfile]: https://devcenter.heroku.com/articles/procfile
