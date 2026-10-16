# ask-jev

`ask-jev` is a small Go CLI for TypeSafe's System One decision API. It sends
one `choice`, `noul`, or `score` question per invocation, or several through
`batch`, and prints JSON. The API client and types live in `internal/jev`;
`cmd/ask-jev` handles the CLI,
JSONC config, and local outcome log. An MCP transport can use the same client
later. The project uses only the Go standard library.

## Setup

Copy `config.example.jsonc` to `config.jsonc` and fill in your endpoint, API
key (or `api_key_path`), model, and timeout. `config.jsonc` is ignored by Git.
Keep a key file outside the repository and restrict its permissions. The
wrapper reads this config from the repository root even when called from
another directory; direct binary execution can use `--config` or
`ASK_JEV_CONFIG`.

```sh
cp config.example.jsonc config.jsonc
chmod 600 config.jsonc
bin/ask-jev version
```

The default endpoint is the [documented System One evaluation endpoint](https://docs.typesafe.ai/api.md),
`https://api.typesafe.ai/v1/systemone`. The example model is `jev-latest`.
The CLI requires HTTPS except for a loopback endpoint used by local tests.
Only the literal `ASK_JEV_FORCE_BUILD=1` rebuilds into a temporary binary;
other values reuse the cached binary. `make build` builds a repo-local binary,
and `make test` uses mock HTTP servers without calling TypeSafe.

## Questions

`--state` accepts a JSON string, object, or array directly, or `@file` with
JSON in that file. `--question` becomes the API's `instructions`. Choice
options use `key=description`, or just `key` to send a null description.
Score levels are ordered from low to high. You may override config with
`--model` and `--timeout 15s`.

```sh
bin/ask-jev choice --state '{"ticket":"My payment failed"}' \
  --question 'Which team should handle this?' \
  --option billing='Payments and refunds' --option technical='Bugs and outages'

bin/ask-jev noul --state @state.json \
  --question 'Does this need a human?' \
  --true 'Human help requested' --false 'No human help requested'

bin/ask-jev score --state '"The export is broken"' \
  --question 'How severe is this issue?' \
  --level 'Cosmetic' --level 'Degraded with workaround' --level 'Blocking'
```

The API also accepts a map of questions in one request. `batch` reads a JSON
file with the documented `{state, model, questions}` shape; `model` may be
omitted to use the configured default. Question ids are returned as keys in
`answers`. Instructions can be a string, object, or array in this mode.

```json
{
  "state": {"ticket": "My payment failed"},
  "questions": {
    "urgent": {"type": "noul", "instructions": "Is this urgent?"},
    "team": {
      "type": "choice",
      "instructions": "Which team should handle this?",
      "criteria": {"billing": "Payments", "technical": "Bugs"}
    }
  }
}
```

```sh
bin/ask-jev batch --file batch.json
```

The CLI sends the documented `{state, model, questions}` request to
`POST /v1/systemone`. [Choice](https://docs.typesafe.ai/primitives/choice.md)
returns a selected option, probabilities, and confidence;
[Noul](https://docs.typesafe.ai/primitives/noul.md) returns a yes probability
without a separate confidence field; and
[Score](https://docs.typesafe.ai/primitives/score.md) returns a weighted score,
level probabilities, a legend, and confidence. See TypeSafe's
[confidence guide](https://docs.typesafe.ai/confidence.md) when deciding
whether to act on a result.

Single-question output contains `answer`; batch output contains `answers`.
Both include `model`, `usage`, measured `latency_ms`, and a
client-generated `request_id` with `request_id_source:"local"`. The API
reference does not document a server request ID or server latency field, so
these values describe this CLI invocation. HTTP errors include up to 300
characters of the API's error message or response body; credentials and
request state are suppressed if echoed verbatim by the server. JSON error
responses without an `error` or `message` envelope show only the HTTP status.
The key is not written to JSONL logs. The API reference documents input
and output token usage but no cost field. If a response includes a `cost`
field, this CLI passes it through without calculating or interpreting it.

Add `-H` or `--human` to an evaluation for a single readable line instead
of JSON on stdout. `-h` or `--help` prints command help.
For `noul`, the line says YES when its probability is at least 0.5 and NO
otherwise, with confidence shown as the probability of that result. Choice
uses the selected option's probability. Score shows the API's numeric
expected value, the label of its nearest level, and the API confidence.
Descriptions are omitted when none were supplied. Batch prints one line per
question, prefixed with its ID, in ID order. If `--log` is set, the JSONL
entry still contains the full structured result.

```sh
bin/ask-jev choice -H --state @state.json --question 'Route?' \
  --option billing='Payments' --option technical='Bugs'
# billing - Payments - 67% confidence
```

## Record an outcome

`--log` appends a private JSONL evaluation entry, with the answer, local
request ID, and a SHA-256 hash plus byte length of the state. It omits the
full state and key. Add `--log-state` only when the full state belongs in the
log. Later, `outcome` appends the actual
value for that ID. A JSON value such as `true` or `42` stays typed; other text
is stored as a string. `outcome` checks that the evaluation exists and refuses
a second outcome for the same ID. Set `log_path` in config to avoid repeating
`--log`.

```sh
bin/ask-jev choice --state @state.json --question 'Route?' \
  --option a --option b --log decisions.jsonl
bin/ask-jev outcome <request_id> true --log decisions.jsonl
```

This JSONL log is a local record for later comparison; `outcome` does not
send anything to TypeSafe.

## License

MIT
