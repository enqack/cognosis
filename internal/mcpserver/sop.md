# Cognosis

This project has a Cognosis vault -- persistent memory across sessions, reachable through the
`cognosis` MCP tools. It is not a file store to browse; it is where your own past findings live.
This SOP is how to operate it.

## Retrieval

- Before deciding anything non-obvious, `query_knowledge` first -- a past session may have already
  settled it, or already been wrong about it. Retrieval returns matching chunks; `get_note` reads
  one note in full once you know its path.
- Falsified notes are retained but excluded from results unless you ask for them. A missing answer
  may be a falsified one.

## Writing

- Capture in-session, the moment something durable surfaces (a decision, a gotcha, a dead end
  worth not re-walking). End-of-session capture loses whatever the session forgot it learned.
- `entries/` holds raw dated capture and requires `category: entry`. `notes/` holds distilled
  knowledge -- legal categories there are `concept`, `cursed-knowledge`, and `lesson-learned` --
  and every note requires non-empty `sources` pointing at entries or reflections. Provenance
  first: the entry must exist before the note that cites it.
- Omit `id` in frontmatter; the server assigns one (and preserves it on overwrite).
- For small changes use `edit_note`, never a full rewrite via `write_note` -- resending a whole
  file risks clobbering what another session wrote.
- `updated` is hand-maintained. An edit that forgets to stamp it silently misinforms the archival
  clock, which keys on exactly that field.
- Tag notes that only make sense in this project with `project:` in frontmatter; leave knowledge
  that applies anywhere untagged (untagged notes are global).

## Lifecycle

- `compile_lifecycle` is the deliberate pass that reinforces, falsifies, and graduates. Nothing is
  inferred from mention alone -- a note you relied on but never reinforced still decays.
- Only `notes/` participate in decay; entries and reflections are permanent record.
- Reinforcing a disputed note silently clears the dispute. Review the dispute first and decide on
  the merits; do not clear it as a side effect of a bulk pass.

## Trust and failure shapes

- Vault writes self-commit. Never git-commit the vault yourself, and never re-read a note to
  verify a write landed -- the write either errored or it is durable.
- A bare MCP connection error usually means the daemon is stopped, not that the vault or a token
  is broken. Check `cognosis status` before concluding anything worse.
- A green `cognosis status` covers the subsystems it names and nothing else. Do not read it as
  proof that the thing you are debugging is healthy.
- A newly added or renamed MCP tool is invisible to an already-connected client until it
  reconnects. Verify against a fresh connection before concluding the tool is broken.

The index below is what is already in the vault -- paths only. Use `query_knowledge` to search it,
or `get_note` to read one in full.
