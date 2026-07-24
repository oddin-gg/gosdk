# Examples

Each example is a self-contained `main.go`. All of them read the access
token from the `TOKEN` environment variable and default to the
integration environment; every example also honours
`ENV=integration|test|production` to target another environment:

```shell
TOKEN=<your-token> go run ./examples/basic
TOKEN=<your-token> ENV=test go run ./examples/basic
```

| Example | What it shows |
|---|---|
| [basic/](basic/main.go) | Minimal feed consumer: connect, subscribe, print odds changes, shut down on signal. |
| [api_only/](api_only/main.go) | Catalog/entity reads over HTTP only — the AMQP feed is never opened. |
| [recovery/](recovery/main.go) | Explicit event recovery with `RecoveryHandle` (`Done()` / `Result()`), plus connection-event handling. |
| [multi_locale/](multi_locale/main.go) | Fetching the same entities in several locales; per-locale cache fill-in. |
| [replay/](replay/main.go) | Replay queue management and playback via `client.Replay()`. |
| [graceful/](graceful/main.go) | Clean shutdown: drain subscriptions with `sub.Close(ctx)` before `client.Close(ctx)`. |
