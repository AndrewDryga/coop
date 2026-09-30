# Service configuration writer

`sh deploy.sh app.conf service.env` writes a service environment file. `app.conf` has one `name=`, `port=`, and `log=` line. An optional `APP_PORT` environment variable overrides `port`. The existing output format is part of the contract.

Run `sh test.sh` after changes. The queue adds safe path handling, override validation, and atomic replacement without changing the output format.
