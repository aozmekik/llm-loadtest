# ADR 0005: Send credentials only where the user pointed them

- Status: accepted
- Date: 2026-10-18

## Context

A load tester is pointed at many servers in a short time: the built-in
mock, a local engine, a colleague's GPU box on the LAN, a hosted API. Users
keep API keys in environment variables, most commonly `OPENAI_API_KEY`, and
pass other credentials as headers (`--header "api-key: ..."`) or in the
base URL (`?sig=...`, `https://user:pass@host`).

Three paths can move a credential somewhere the user did not intend:

1. **A default key variable.** The first version read `OPENAI_API_KEY` by
   default and sent it as a Bearer token to whatever `--base-url` was
   given. Anyone with the variable exported sent their OpenAI key to the
   mock, to a LAN server over plain HTTP, or to a third-party API.
2. **Redirects.** Go's HTTP client follows redirects. On a cross-host
   redirect it drops `Authorization` but forwards custom headers such as
   `api-key`, and a 301 to 303 turns the POST into a body-less GET that was
   still timed as a request.
3. **Echoed errors.** Servers often echo the request (headers, the URL) in
   error messages, which end up on stderr and in the JSON, CSV and Markdown
   results that people attach to issues.

## Options considered

For the key variable:

1. **Keep `OPENAI_API_KEY` as the default for every host.** Convenient for
   OpenAI users, and how many client libraries behave, but it leaks the key
   to every other server tested.
2. **No default.** Safe, but every OpenAI run needs `--api-key-env`.
3. **Default only for OpenAI's own host.** `OPENAI_API_KEY` is read only
   when the base URL's host is `api.openai.com`; any other server gets a key
   only when the user names its variable with `--api-key-env`.

For redirects: follow them (Go's default), follow same-host redirects only,
or never follow them and report the 3xx.

For echoed errors: trust servers not to echo, or scrub known secret values
from every message before it is stored or printed.

## Decision

- Option 3 for the key variable. An empty `--api-key-env ""` sends no key
  at all. The run says which variable's key goes to which host, warns when
  the named variable is empty, and warns when a key would travel over plain
  HTTP to a host that is not loopback.
- Never follow redirects with the default HTTP client: a 3xx fails the
  request as `http_3xx`, and the message names the redacted `Location`. A
  benchmark should measure the endpoint it was given, and a redirect is
  almost always a wrong base URL.
- Scrub every error message before it leaves the client: the API key and
  the URL password whatever their length, and header values, the URL's
  username and query values of 8 bytes or more (shorter values are usually
  ordinary words, and scrubbing them would garble messages). Each value is
  matched as configured and in its query- and path-escaped forms. Messages
  are scrubbed in full before they are shortened to 300 bytes, so a secret
  cannot survive by being cut in half. The base URL is stored with its
  credentials and query values replaced, and only header names are saved.

## Consequences

- OpenAI users need nothing extra; users of OpenRouter, vLLM with
  `--api-key`, or any other authenticated server pass `--api-key-env NAME`.
  A forgotten flag shows up as a warmup failure with `http_401` and a note
  that no key was sent, not as a leaked key.
- Servers behind a redirect (an `http` to `https` upgrade, a trailing-slash
  rewrite) fail fast with a message naming the target, and the user fixes
  `--base-url`.
- A server that echoes only part of a secret, or a transformed form of it
  (hashed, truncated by the server itself), is not caught; the README says
  so. Scrubbing is a safety net, not a guarantee about arbitrary servers.
- Tests pin the behaviour: `TestAPIKeySelection` and
  `TestDefaultKeyIsNotSentToOtherHosts` for the default,
  `TestRedirectsAreNotFollowed`, and `TestEchoedSecretsAreRedacted`,
  `TestSecretAcrossTruncationIsRedacted` and `TestSecretsAreNeverWritten`
  for scrubbing.
