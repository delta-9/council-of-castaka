# The council in the rawpixel GUI

*Planned 2026-11-16; phase 1, the council's side, and phase 2, the GUI's, built the same day. A Council screen in the rawpixel GUI (`packages/rawpixel-cli/gui` in rawpixel-services), for any colleague with their own API keys: debates on a matter, and pull request reviews posted on GitHub.*

## Goal

- A colleague opens the Council screen, enters four API keys once, and convenes the council: on a matter, or on a pull request (`--pr`), whose review is posted under their own `gh` account.
- Nothing personal ships: no keys, no paths, no fleet. The chair uses the same screen and the same key store as everyone else.

## Getting the council

- The council stays in Castaka. rawpixel-services pins a tagged release and builds it with its own Go toolchain (`docs/development/go-toolchain.md` there), the way it pins golangci-lint: `go install github.com/delta-9/council-of-castaka/cmd/council@<tag>`. `bun run setup:council` builds it into the Go store every checkout shares, and the Council screen offers that build while it is missing, so nobody clones Castaka. `setup:go` leaves it out: only those who open the screen need it.
- The binary carries a default config: the seats, models, prices and search server. So an install needs no `council.json`.
- `RAWPIXEL_COUNCIL_COMMAND` points the GUI at another build, the chair's own for example.

## Everything in `~/.council`

- `keys.env`: the API keys, one `NAME=value` line each. Only its owner can read it (mode 0600), and it sits outside every repository. An environment variable of the same name still wins, and an older env file the config names (`envFile`) is read after it.
- `council.json`: the user's settings over the built-in defaults, the roster first. The chair's holds his roster, his own key names and his hermes `.env` as that older file.
- `sessions/`: the sessions, as now.

## Keys and the gate

- **The four keys:** Anthropic, Google (Gemini), OpenAI, and MiniMax, which the seats that search through the MCP server use.
- **Commands:**
  - `council keys` prints which keys are set, as JSON, never a value.
  - `council keys set anthropic` takes the value on standard input and rewrites `keys.env` atomically.
  - `council keys check` makes one call per provider that costs nothing, its models listing, and says which keys work. MiniMax has no such listing, so its key is tried with one search through the MCP server, in the plan.
  - `council keys import <file>` copies the four keys from an env file, by the names the config gives them, without printing them. The chair runs it once on the hermes `.env`, and the config then stops pointing there.
- **The GUI never reads `keys.env`** and never gets a value back. Its key form is write-only, and it shows each key as set, missing or refused.
- **A council the GUI starts gets no key variable.** The GUI's own environment holds a team Anthropic key, and a variable wins over `keys.env`, so the GUI removes the key variables, the built-in names and the config's, before it starts a council: what the form saved is what the council spends.
- **The gate:** the screen shows its content only once all four keys are set and accepted. Until then it shows the key form.

## The roster

- **Fixed seats:** Morkaï and Noren (Anthropic), Ewandor and Hûrl (Google), Firfïn (OpenAI). Each has an on switch, a model among its lab's, and an effort among the levels that model takes. At least two seats must be on.
- **Models** are the priced ones in the config, so `/cost` stays honest: adding a model means adding its price.
- **Efforts** are those the APIs take: Anthropic low to max (xhigh included), Gemini low to xhigh, OpenAI low to max. Gemini's xhigh is not in its Go SDK's list, but the API took it in a one-word check (2026-11-16).
- **Output budgets follow the effort,** as tuned on the chair's runs: thinking and the answer share the budget. Anthropic seats get 16000 up to high and 64000 at xhigh and max, since their requests stream. Gemini and OpenAI seats get 16000 up to medium and 32768 from high up, since their requests do not stream. A seat's `maxTokens` in `council.json` still overrides.
- **The built-in default** is the five seats at high effort, the setting of the first real runs.
- **The GUI draws its form from the council** and holds no list of its own:
  - `council options` lists the seats, models, prices and efforts as JSON.
  - `council roster` prints the roster.
  - `council roster set` takes one on standard input, checks it (two seats at least, known models and efforts) and saves it in `council.json`.
- **The GUI edits the default roster.** A new session starts from it and can change it for that session only, since a session freezes its roster.

## Running a session from the GUI

- **Events:** `council --events` writes one JSON line per event on standard output: the session, each round and its speakers, each tool call, each message as the log stores it (without its raw exchange), each retry, a posted review, and the end. It reads the chair's lines and commands the way the console does.
- **The council runs on its own,** not as a child of the GUI's sidecar, which restarts on a worktree swap. The GUI attaches through the session folder:
  - `chair` is a pipe the council reads the chair's lines from;
  - `events.jsonl` holds the events;
  - `running.json` names the council's process while the session runs.
  
  So a GUI restart costs no round: the screen reattaches and reads the events from where it left off.
- **Starting one:** the GUI picks the session's folder and starts `council --detached -resume <folder>` there. A session whose roster differs from the default gets `-config <folder>/roster.json -roster gui`. A debate's matter goes in through the pipe once the session event is out.
- **Sessions** are listed from `~/.council/sessions`: the matter or the pull request, running or ended, its rounds and its cost.

## The screen

- The key gate, then:
  - the sessions;
  - a new debate: the matter, the roster for this session, the rounds;
  - a new review: the repository (by default the GUI's active one), the pull request, and a dry-run switch, with the `gh` account it will post under;
  - the live session: each round with each seat's messages, tool calls and retries; the chair's box; fold, with the folding seat; stop and more; the running cost; and the report or the review's link.

## Phases

1. **The council (Castaka):** the built-in defaults and `~/.council`; `keys`, `options` and `roster`; `--events` with the attach (`--detached`). Then a tagged release. Built 2026-11-16 and checked: the tests, among them one that starts the real binary detached, posts through its pipe and reads its events; and on the chair's own setup, `council keys` and `keys check` (all four keys accepted), `options`, `roster`, and Gemini at xhigh.
2. **rawpixel-services:** the pinned install, the sidecar module (`server/council.ts`), and the screen. Built 2026-11-16 and checked: the sidecar's tests, among them one that runs the pinned council with fake minds through a session folder until `/quit`; and in the chair's GUI, a debate between two seats at low effort with a fold and its report, which kept running through a GUI restart and answered a message posted after it, and the review form naming the `gh` account. A review run through the screen is left to phase 3.
3. **A test as a colleague:** a fresh home folder, the keys entered through the form, a debate, and a dry-run review.
