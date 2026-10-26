# Remote feed

A remote feed is a JSON document the deck polls and turns into tiles. Grok bots have no public status API, so something else writes this document (a file on the Mac, or an https URL) and the deck only reads it. One feed, one source name on the tiles. The default name is Grok.

The deck reads it about every 20 seconds. Setup is in [features](features.md#remote-feed-grok-bots). `docs/fixtures/remote-feed.json` is a complete example.

## Document

```json
{
  "source": "Grok",
  "fetched_at": "2026-10-25T12:00:00Z",
  "agents": [
    {
      "id": "inbox",
      "name": "Inbox triage",
      "group": "ops",
      "status": "working",
      "summary": "Reading the latest thread",
      "last_activity": "2026-10-25T12:00:00Z",
      "open_url": "https://example.com/bots/inbox"
    }
  ]
}
```

| field | required | what it is |
|---|---|---|
| `source` | no | label, used when `remoteFeed.name` is empty. The deck's app name is this label in lowercase letters and digits, at most 11 characters |
| `fetched_at` | no | when the publisher built the document (ISO 8601). `fetchedAt` is accepted too |
| `agents` | yes | the list. An empty list is a feed with no agents |
| `id` | yes | stable id. The tile goes away when the id leaves the list. A row with no id is skipped |
| `name` | no | the tile's title. Without one, the source name is used |
| `group` | no | team or group, shown as the working folder. `team` is accepted too |
| `status` | no | see below. An unknown word is shown as idle, not as working |
| `summary` | no | the last thing the agent said, shown on the card. `last_message` is accepted too |
| `last_activity` | no | when that row last changed (ISO 8601). `lastActivity` is accepted too |
| `open_url` | no | page opened by **open in …**. http or https, with no username or password in it. `openUrl` is accepted too |

`agents` may be empty. A document that is not an object with an `agents` array is an error and the previous tiles stay up until the feed is stale.

## Status

| `status` | tile |
|---|---|
| `working`, `running` | working |
| `idle`, `done`, `finished` | done |
| `needs_input`, `needs_attention`, `waiting` | needs you. The card says to open the agent; there is nothing to answer on the deck |
| `error`, `failed` | error. The summary is the message, or "error" |
| `offline` | offline |
| `stale` | stale |
| anything else, or missing | done |

## Stale

Two clocks, either one:

- **The feed.** `fetched_at` is older than `staleSeconds` (default 180), or the deck itself has not had a successful read in that long (the URL was down, the file was missing). Every agent in that document shows as **stale**.
- **A live row.** A working or needs-you agent whose own `last_activity` is older than `staleSeconds` shows as **stale**, even when the document was just written. A stuck "working" is not working.

Idle and error rows in a fresh document keep their status, so a quiet bot does not flap to stale just because it has nothing new to say. A row with no `last_activity` is judged only by the feed clock.

`staleSeconds` below zero turns the check off.

## URL

`remoteFeed.url` has to be https. The deck does not follow a redirect, so a bearer token stays on the host you named. The token is `REMOTE_FEED_TOKEN`, or whatever `tokenEnv` names, and if that variable is empty, `token` in config. It is sent as `Authorization: Bearer …` and never logged.

`remoteFeed.path` is a file, used only when `url` is empty. The example file works as a path. Its timestamps are fixed, so with the default stale window the working row whose `last_activity` is hours behind `fetched_at` shows as stale, and the others show their own status when the file is read near that `fetched_at`.
