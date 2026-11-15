# Council of minds

*`cmd/council`, next to the Metabarons · working name **Les Technopères** (Castaka already has the Metabarons; the Incal's techno-priests suit technical pickles; rename freely)*

The chair brings a technical pickle, and the greatest and quirkiest minds of four labs argue it out in rounds. Unlike the Council of the Castaka, this one reconvenes: every message, the chair's or a mind's, is queued into the next turn of every other mind.

## Goal

- A terminal tool. You chair; five minds debate: Opus 5.5, Fable 5.2, Gemini 4 Hydrogen, Gemini 4 Carbon and GPT-6 Astra, with Grok later.
- Each mind is the model itself, called with your API keys. There is no subscription CLI, so usage terms are not an issue.
- Started inside a GitHub repository, the minds read what git tracks there, and all of them search the web, so they argue from the material and from fresh sources.
- You can interject at any time and nothing is lost. The transcript can be read while the debate runs, and `/fold` turns the debate into a report.
- The minds' tools only read: nothing they write can act.

Not yet: private messages, personas, a web UI.

## The floor: how a message reaches every mind's next turn

One append-only log per session (`log.jsonl`) is the whole state. There is no queue object. A mind's queue is everything in the log that someone else wrote and that the mind has not yet seen.

Each entry has `seq`, `from` (`chair` or a seat), `kind` (`msg`, `pass` or `error`) and `text`. A mind's turn also carries `seen` (the last `seq` it had read when it spoke), `model` (the seat's configured model), `served` (the model that answered), `usage`, the tools it called, and the provider's native exchange for replay: tool calls, results and reasoning. Errors are shown to you and never to the minds; a failed turn still records what its completed steps cost.

Each session folder also holds `session.json`: the roster, the repository and every seat's system prompt, fixed when the session starts and reused verbatim on resume. Changing any of them under replayed turns would break those turns; Fable's history check compares the system prompt and the tool list too.

A mind's request is rebuilt from the log on every turn (`floor.View`):

```
view(seat, head):
  prev ← 0
  for each turn t of seat (msg or pass), oldest first:
      user      ← others' msgs in (prev, t.seen], oldest first, each wrapped <msg from="…">
      assistant ← t exactly as the provider returned it (tool exchange and reasoning included); "PASS" for a pass
      prev ← t.seen
  user ← others' msgs in (prev, head]          ← the queue; an empty queue means no turn
```

Example with three seats:

```
#1 chair     Should we shard the media table or partition it?
   ── round 1: every seat reads up to #1, all in parallel ──
#2 astra     (seen #1) Neither yet: archive the cold rows first…
#3 chair     Assume 2B rows, 80% cold.            ← typed while round 1 runs
#4 opus      (seen #1) Partition by type…
#5 hydrogen  (seen #1) Shard by owner…
   ── round 2: every seat reads up to #5 ──
opus, turn 2:  user [chair #1] · assistant [opus #4] · user [astra #2, chair #3, hydrogen #5]
```

Astra's #2 comes before Opus's #4 in the log, yet it is in Opus's queue, because Opus had only seen #1. `seen` is what makes "queued into the next turn" exact; `floor/view_test.go` pins it.

What follows from this design:
- Every request alternates user and assistant, which all three APIs require.
- Each request extends the previous one byte for byte: the session freezes the system prompt and the tools, history is append-only, and rendering is a pure function of the log (no clock, no randomness). So prompt caches hit, and Claude's history-editing check never trips on replayed reasoning.
- `seen` records what each mind knew when it spoke.
- Quitting or crashing loses nothing: resuming rereads the file (a last line cut short by a crash is dropped). A failed turn leaves its seat's queue intact for the next round.

## Rounds

- A round snapshots the head of the log. Every active seat with a non-empty queue takes one turn, all in parallel, on that snapshot. Replies print and are appended as they land. The round ends when every turn has finished.
- So round 1 is blind: first opinions are independent, and nobody anchors on whoever answers fastest. Castaka's first round is blind the same way.
- A chair message sets the round budget to `rounds`. The default is 2: blind opinions, then one round of rebuttal. Then the floor waits for you. `/more` runs one more round. The message and its budget are applied under one lock, so a round sees both or neither.
- `@Hûrl @Noren …` in a chair message runs one round in which only those seats speak. A mention takes a seat's name or ID, case and accents aside (`@hurl`). The others still read all of it at their next turn.
- A reply of exactly `PASS` means nothing new. It is logged, shown to you dimmed and sent to no one. When no seat has anything queued, the floor waits.
- Dropped for v1: free-running minds, each looping on its own queue. The fast models would talk several times more than the deliberate ones, and the replies would ping-pong and run up costs. If it comes back, only the scheduler changes; the log and the view stay as they are.

## The minds

| Seat | Name | Model ID | Effort | Search | Key (env name) | $ per 1M tokens, in / out | Notes |
|---|---|---|---|---|---|---|---|
| opus | Morkaï | `claude-opus-5-5` | high | its own | `ANTHROPIC_ORG_API_KEY` | 4 / 20, cache read 0.20 | default effort is medium, so set it |
| fable | Noren | `claude-fable-5-2` | high | MiniMax | `ANTHROPIC_ORG_API_KEY` | 8 / 40, cache read 0.20 | needs 30-day retention (400 under zero data retention) |
| hydrogen | Ewandor | `gemini-4-hydrogen` | high (its default) | its own | `GEMINI_API_KEY` | 1.50 / 7.50, cache 0.075 | Google's "fast" Gemini 4; levels low to xhigh |
| carbon | Hûrl | `gemini-4-carbon` | high | its own | `GEMINI_API_KEY` | 2 / 10 until 2026-11-30, then 4 / 20 | the "deliberate" flagship; default medium; `maxTokens` 32768, since at high effort it thought past 16K |
| astra | Firfïn | `gpt-6-astra` | high | MiniMax | `OPENAI_CODE_API_KEY` | 10 / 50, cached 1 | |
| grok | | later | | | `XAI_API_KEY` | | xAI's API is OpenAI-compatible, so it should be config only |

Every ID is listed by its key's models endpoint (2026-11-14); limits and prices come from the providers' model and pricing pages. Every model has a 1M context window. Output caps are 128K for Claude and Astra, 64K for Hydrogen, and 32K for Carbon (from its API listing). Claude 5.x rejects sampling parameters, and reasoning models gain little from them, so the quirk comes from the minds themselves, plus an optional `card`. A seat is config (`seat`, `name`, `provider`, `model`, `effort`, `search`, `card`, `maxTokens` in `council.json`), so two Opus seats with different cards would work.

The minds and the console call each seat by its name; the log keeps the seat ID. A session freezes its roster and charters, so a session started before the names keeps the IDs.

## Charter (system prompt, frozen per session)

`floor/charter.go` (a seat without a name goes by its ID):

```
You are {name} ({model}) on a council: {roster: name (model, lab), …}.
The chair brings a technical problem and decides; the council argues it out.
Messages since your last turn arrive as <msg from="name">…</msg>, oldest first.
Today is {date}. {What the seat can read (the repository, through read_file and
git) and that it can search the web; check facts before you assert them and cite
files, commits or URLs; what you read stays private, so quote what matters.}

- Speak only for yourself: no lines for another member, no tags around your reply.
- The council does not need to agree. If someone is wrong, say so and why.
  If you were wrong, say so and what changed your mind.
- Add, don't echo: a fact, a counter-example, a risk, a test, a number, a better
  question. No compliments, no recap of what others said.
- Concrete beats principled: code, commands, numbers, failure scenarios.
- Wild ideas are welcome: flag them, and defend them when challenged.
- Stay under {words} words unless the chair asks for depth.
- Nothing new to add? Reply exactly: PASS
{card}
```

## Chair's console

- `council` opens a session and `council -resume <session>` reopens one with the roster it started with. `-roster rehearsal` picks the cheap models for a new session, and `-fake` makes every seat answer with what it was shown, for free.
- `council -q -m "…" [-fold opus]` runs the rounds without a console and prints the transcript's path, or folds and prints the report's path. It can be called from Claude Code or by an agent.
- `council -search "…"` runs one search through the MiniMax MCP server and prints what came back, with its size.
- A typed line is posted at once and queued into every mind's next turn, even mid-round. `"""` on a line of its own starts or ends a multi-line message.
- Commands (a seat goes by its name or ID, case and accents aside):
  - `/more`: run one more round.
  - `/stop`: cancel the round in flight (Ctrl-C does the same). Cancelled turns leave no trace.
  - `/rounds N`: set the round budget.
  - `/seats`: list the seats, with names and IDs. `/seat -hurl` and `/seat +hurl` take a seat out or put it back.
  - `/attach <path>`: post a file (up to 256 KB) as a chair message.
  - `/view <seat>`: print exactly what that seat will receive next (the queue made visible).
  - `/fold [seat]`: that seat folds the debate into `report.md` (below).
  - `/wait`: hold the next line until the floor is quiet, so piped chair scripts run in order.
  - `/cost` and `/quit`.
- Each round prints its speakers; each tool call prints as a dim line while the mind works (`Morkaï · git log -n 5`, `Noren · web search: …`); each message prints when it lands, with its round, time and tokens.
- On disk: `~/.council/sessions/<date-time>/` holds `log.jsonl`, `session.json`, `transcript.md` and `report.md`, all private to your user. The transcript is rewritten after each event and lists each turn's tool calls, so it can stay open in a Markdown viewer.

## Tools and web search

### Reading the repository

- **Root:** when the council starts inside a git repository with a GitHub remote, the repository's top level becomes the minds' read root. Otherwise they get web search only, and the console says so. Launching from the repository root is the usual case; git history is per repository, so a subdirectory could not fence it anyway.
- **What they see is what git tracks.** Untracked and ignored files (`.env.local`, `trusad.md`, build output) are invisible, which keeps local secrets out of reach of a mind that a web page might try to steer.
- **Two local tools, read-only** (`internal/tools`):
  - `read_file(path, start, lines)`: a tracked file of the working tree with numbered lines, 400 lines by default and at most 2,000 or 100 KB per call. Symlinks that point outside the repository are refused.
  - `git(args)`: one of `log`, `show`, `diff`, `blame`, `grep`, `ls-files`, `ls-tree`, `status`, `describe`, `rev-parse`, `shortlog`. It runs as `git --no-pager -c core.fsmonitor=false` without optional locks, adds `--no-ext-diff` to `log`, `show` and `diff`, and has a 30 s timeout and a 64 KB output cap. Options that write or reach past what git tracks are refused, abbreviations included: `--output`, `--no-index`, `--contents`, `--untracked`, `--open-files-in-pager`, `--ext-diff`, `--ignore-revs-file`, grep's `-O` and `-f`, blame's `-S`.
- `tools_test.go` pins all of it on a scratch repository. Without the tracked-file check, it fails on `.env.local` and the ignored file.

### Web search

- Each seat searches either with its provider's own tool or through MiniMax's MCP server (`"search": "mcp"` in `council.json`): `uvx minimax-coding-plan-mcp`, with `MINIMAX_API_KEY` from the same env file. The server starts on the first search and stays up for the session.
- **What a search costs**, measured 2026-11-14:

  | Path | Results added to the mind's input | One search |
  |---|---|---|
  | Anthropic's own (`web_search_20260209`) | about 20K tokens | Opus about $0.09, Fable about $0.17 |
  | OpenAI's own (`web_search`) | about 8.7K tokens | Astra $0.10 to $0.20 |
  | Gemini's own (Google Search) | not billed as tokens | free up to 5,000 a month, then $0.014 |
  | MiniMax MCP | about 800 tokens: results with titles, links and snippets, in 3.6 s | about $0.006 on Fable, in your plan |

- So Fable and Astra search through MiniMax. Opus keeps its own, which reads whole pages. The Gemini seats keep theirs, which is nearly free. `/cost` counts the provider searches.
- Provider details:
  - Anthropic: at most 5 searches per request.
  - Gemini: mixing Google Search with functions needs `toolConfig.includeServerSideToolInvocations`; a probe got a 400 without it.
  - OpenAI: `web_search` is wired but unused, since both rosters send Astra through MiniMax.

### A turn is a loop

- The mind calls tools; the council runs the local ones and sends the results back until the mind answers. Searches the provider runs happen inside the request; Anthropic's `pause_turn` is resumed by resending. 40 requests is the hard limit.
- **The tool budget** is 25 calls a turn (`maxCalls` in `council.json`):
  - The result of the call that uses it up says so ("tools are off now: answer with what you have"), and the tool choice becomes `none`. Told nothing, Fable once announced a check it could no longer run and ended its turn empty.
  - A clean stop with no text gets one nudge request.
  - Calls that arrive together in one response are all answered, so a mind can pass the budget within that response: Carbon made 7 with a budget of 2.
- **The whole exchange is stored with the turn and replayed as returned on later turns:** tool calls, results and reasoning. Fable's history check and Gemini's signatures require it, and it lets each mind remember what it read. The tool list is built once per seat and never changes within a session, because Fable's check compares it.
- **What a mind reads stays private to it**, so the charter tells it to quote what matters.

### /fold {seat}

- One mention round, in which that seat writes the final report. It leads with the conclusion, then gives the evidence and arguments that led there: who brought them, and which files, commits and sources they came from. It names what is still disputed, and ends with next steps when relevant. It may use the tools to check before writing, and the word limit does not apply.
- The report is saved as `report.md` in the session folder (the latest fold wins) and its path is printed. A fold that fails says so.

### Considered and dropped

Running each mind through its own agent CLI (Claude Code, Codex, a Gemini CLI) in read-only mode. Their tools would come free, but each brings its own sandbox rules, a large coding-agent prompt on every turn, and session state outside the floor. That loses the shared log, replay and caching the rehearsals proved.

## Provider notes (knowledge, not gates)

- **Anthropic** (Opus 5.5, Fable 5.2), `minds/anthropic.go`:
  - Thinking is always on. `Thinking` stays unset and `OutputConfig.Effort` sets the depth. Sampling parameters and prefill return 400.
  - Each request is streamed and built back into the `BetaMessage` the tool loop expects:
    - A stream that ends without `message_stop` fails the step, so a cut turn is never stored. Reading stops at `message_stop`, so what the connection does afterwards cannot fail a finished answer.
    - A watchdog at the transport fails a request after 120 s without a byte, headers and pings included. It cancels a context of its own, never the chair's. The longest silence measured on a high-effort turn is 30 s (rehearsal 4), and each turn logs its own as `quietMs`.
    - Bounds: 15 minutes per request, its retries included, and 30 per turn, tool calls included. The client has no request timeout, which would cut long healthy streams.
    - HTTP/2 health checks ping a connection silent for 45 s and close it after 15 s without an answer, so a dead connection leaves the pool within a minute.
    - The SDK retries a request that got no response or a retryable status, twice. A stream the API fails partway with `overloaded_error` or `api_error` runs once more in place, 2 s later, and the console says so: that costs less than the next round rerunning every step of the turn, searches included. The chair chose this after rehearsal 4, over no retry and over Fable's retry only for steps that ran no search. Any other failure, or a second one, fails the turn, and the next round reruns it.
    - Usage counts once per request, failed ones included: input from `message_start`, output and billed searches from the last `message_delta`.
  - `maxTokens` is 16000. Top-level `CacheControl` caches the prefix.
  - A turn's exchange is replayed message by message with `param.Override`; each response becomes its message through `BetaMessage.ToParam()`, thinking blocks and signatures included. After a mid-output fallback, the reasoning blocks before the last fallback block are left out, as the fallback rules require.
  - `fallbacks: "default"` (beta `server-side-fallback-2026-07-01`). A turn that another model rescued is labelled and priced as that model, which the SDK's accumulator takes from the last fallback block. A refusal that survives the fallback is an error you see, and the seat sits out the round.
- **Gemini** (Hydrogen, Carbon), `minds/gemini.go`:
  - `generateContent`, with the thinking level set per seat (the SDK's enum is upper case, so effort is upper-cased). The output cap covers thinking and answer together.
  - Function results go back as `functionResponse` parts. Server-side search steps come back as signed `toolCall`/`toolResponse` parts and are replayed with the rest; the Go SDK's `Part` keeps them through a save and reload.
  - Google's current docs describe the Interactions API, whose stateless mode also wants thought blocks resent exactly as received. `generateContent` does everything the council needs, so v1 stays on it.
- **OpenAI** (Astra), `minds/openai.go`:
  - Responses API, with `reasoning.effort` from low to max (`none` returns 400).
  - `store: false`, because the log is the state. Output items, encrypted reasoning included, are replayed with `param.Override` when they came from the same model. Function calls are answered with `function_call_output` items.
  - `prompt_cache_key` per session and seat; caching is automatic, and usage reports cache writes separately. Prompts over 272K input tokens cost 2× on input and 1.5× on output.
- **Data terms:** Anthropic and OpenAI do not train on API traffic by default. Gemini counts as paid only for a key whose Cloud project has active billing; the council's key is on paid Tier 1 (checked 2026-11-14), so prompts are not used for training and are logged only to detect abuse.
- **SDKs:** `anthropic-sdk-go/v2` v2.9.0, `google.golang.org/genai` v1.77.0, `openai-go/v3` v3.99.0. Grok comes later through `openai-go` with xAI's base URL. Adding them moved `go.mod` to Go 1.26.

## Rehearsals

The same seats, adapters and requests on a cheaper roster (`-roster rehearsal`): the real models at lower effort where their contract matters, and the cheapest model with the same contract where it doesn't. Search modes are the same as in the `minds` roster.

| Seats | Rehearsal model | Effort | Why |
|---|---|---|---|
| opus, fable | the same: `claude-opus-5-5`, `claude-fable-5-2` | medium | The whole Anthropic contract, fallbacks included. Fable 5.2 enforces the history check on every account, so replayed turns are tested as strictly as they will run. Medium rather than low, because at low effort a model can skip thinking and leave no thinking blocks to replay. |
| hydrogen, carbon | `gemini-4-hydrogen` | low | The Gemini 4 contract itself, at its cheapest ($1.50 / $7.50). |
| astra | `gpt-6-luna` | medium (its default) | The same Responses contract as Astra (effort levels, encrypted reasoning, caching, the 272K price step) at 1% of the price ($0.10 / $0.50). |

The first `sleep` in a piped chair script lets the binary start: macOS scans a freshly built binary on its first launch, and lines that arrive before the console reads them all land in round 1. A script must not be piped into `head`, which kills the council on its next write.

### 1. The floor, 2026-11-14

Session `~/.council/sessions/2026-11-14-rehearsal`: 20 turns over five rounds, with a restart before the last one. Every pass condition held, and no turn failed.
- **Queues:** round 1 read only #1; the interjection (#2) landed during round 1 and every round-2 turn read it; the mention round (round 4) was Astra alone; `/view carbon` showed `#15 astra, #16 fable, #17 opus, #18 chair, #19 astra`; after the restart, `/more` ran the four seats with something new and skipped Astra.
- **Replayed reasoning:** every Opus turn, and every Fable turn from round 2 on, carried a signed thinking block (Fable skipped thinking in round 1, even at medium). Every Gemini turn carried a thought signature, and every Luna turn an encrypted reasoning item. Fable accepted all its replayed turns, before and after the restart.
- **Caching:** no cache in round 1, whose prefixes were under the providers' minimums; writes from round 2; reads from round 3 on all three providers.
- **Time per turn:** Opus 14–17 s and Fable 10–17 s at medium, Hydrogen 2.5–7.6 s at low, Luna 3.5–5.8 s.
- **Cost:** $0.55 in all: Fable $0.33, Opus $0.17, the two Hydrogen seats $0.05, Luna $0.002.
- **The debate itself:** the minds read each other. In round 3, Opus answered Fable's migration plan with two bugs it would hit at cutover, and Fable conceded a point to Opus on the partition key.

### 2. Tools, search and /fold, 2026-11-14

Session `~/.council/sessions/2026-11-14-tools`, run inside this repository. The question needed the code and the web: "Should cmd/council stream its Anthropic requests instead of waiting up to 15 minutes on one non-streaming call?" Two rounds, then `/fold opus`. No turn failed.
- **Tools:** every seat read the code before arguing, through `git ls-files`, `git grep` and `read_file` on `anthropic.go`, `minds.go` and `floor.go`. Astra (Luna) made 16 calls in its first turn.
- **Search:** Opus used 7 Anthropic searches, the two Hydrogen seats 3 Google searches, and Fable and Astra searched through MiniMax. Gemini's server-side search steps and functions mixed in one turn and replayed cleanly.
- **Replay:** Fable accepted its replayed tool exchange, with signed thinking, in round 2 (round 3 was the fold, Opus alone). Opus replayed its exchanges in rounds 2 and 3 without error.
- **Fold:** Opus made five tool calls to check facts before writing a 9 KB `report.md`. The report has a conclusion, file:line evidence, cited sources, a table of what is still disputed, and next steps.
- **Time and cost:** 12 to 50 s per turn that spoke, about 3 s per pass. $0.92 in all: Opus $0.47 over three turns with its context at 109K cached tokens by the fold, Fable $0.30, the two Hydrogen seats $0.14, Luna $0.006.

### 3. The real minds, 2026-11-14

Session `~/.council/sessions/2026-11-14-minds`, the `minds` roster at high effort, watched live in Ghostty. The chair pasted rehearsal 2's report and asked the council to check it again against the code and the current docs. Two rounds, then `/fold fable`, in 14 minutes.
- **Two failed turns in round 1, both back in round 2:**
  - Fable used its 25 tool calls and ended its turn with no text.
  - Carbon thought past its 16K output budget after 6 minutes.
  - Their tokens still counted in `/cost`. Both are fixed since: the tool-budget notice with its nudge, and Carbon's 32K budget.
- **Tools:** up to 26 calls a turn. Opus ran 5 Anthropic searches and the Gemini seats 26 Google searches; Fable and Astra searched through MiniMax.
- **Fold:** Fable wrote a 1,949-word report. It checks rehearsal 2's report against the code, saying what holds, what is stale or wrong, and what was missed. It includes a table of what it could and could not verify, the exact streaming change, and a tests-first plan.
- **Time and cost:** 34 s to 6 min per turn. $5.36 in all: Fable $2.52 (112K tokens of written context, 33K output), Astra $0.79, Carbon $0.77, Hydrogen $0.72, Opus $0.57.
- **Errors never reach the minds,** so the fold said nothing had failed yet while two of its own turns had.

A cap check afterwards (`maxCalls` 2, one round, $0.25) had Opus, Fable, Luna and the real Carbon each reach the budget and then answer; Carbon took its 32,768-token budget. The nudge did not fire, so that path has not yet run against a provider.

### 4. Streaming, reviewed by the council, 2026-11-14

Session `~/.council/sessions/2026-11-14-streaming`: Opus and Fable at high effort, on the newly streamed adapter, reviewing that very change. Two rounds, then `/fold opus`. Round 1 took 3.4 minutes, round 2 37 s and the fold 83 s. No turn failed.
- **Replay:** Fable accepted its streamed turns replayed in round 2, and Opus its own in rounds 2 and 3.
- **Silence on the wire:** 30.0 s at most, in round 1 for both seats (29,999 and 30,008 ms), and 14 to 15 s after that. The 120 s idle limit has a 4× margin, and the API seems to send something every 30 s.
- **Tools:** in round 1, Opus made 9 calls and 5 Anthropic searches, and Fable 19 calls, searching through MiniMax.
- **The review:** Opus's 1,680-word report raised three bugs. Two were real, and their fixes are pinned by tests that fail on the old code:
  - A finished answer could fail if the connection lingered after `message_stop`.
  - Slow headers ate the body's idle window, and the silence figure missed both the header wait and the gap a stall cut.
  - Not a bug: the served model after a mid-output fallback. SDK v2.9.0 already relabels it, and a test now pins that.
- **Out of date:** neither seat could read v2.9.0, so the report took a stream's `error` event for a plain string. It comes back as a typed `*anthropic.Error`, which is what lets the retry pick its error types.
- **Test gaps from the report, now closed:** replayed tool inputs, the search query in the call reports, billed searches (the SDK fix it worried about is in v2.9.0), the retry count before headers, and a turn-limit case that could flake. `go test -race -count=20` passes.
- **Cost:** $2.22: Fable $1.43, Opus $0.79.

## Cost shape

Each turn rereads the transcript, and caching makes that cheap. So thinking, output and freshly written context (tool results, search content) make up most of the bill.
- **Without tools,** at high effort and about 3.5K output tokens per turn, the estimate is about $0.55 per round of five seats, with Astra and Fable at roughly 70% of it.
- **With tools,** a mind's context grows with everything it reads and stays cached for the session. Opus's native searches are the fastest-growing part.

Trust `/cost`, which is computed from each turn's usage and the prices in `council.json`.

## Code shape

Go, in the Castaka module: one more static binary next to the Metabarons, whose `internal/council` stays untouched. About 2,700 lines with comments, plus 810 lines of tests:
- `internal/floor`: the log, `View`, rounds, the charter, the transcript.
- `internal/minds`: one file per provider, each with its tool loop, plus a `fake` mind that echoes what it received and reads the last commit.
- `internal/tools`: the repository tools behind the git guard, and the MCP client.
- `cmd/council`: flags, sessions, the console, `/fold`, `/cost`.
- `council.json`: the rosters, the key file and its variable names, the search server, the defaults and the prices. No secrets.

Tests, each catching one regression:
- `view_test.go`: a mind missing a message or reading one twice. Tracking a seat's reads by its own last message instead of `seen` makes it fail.
- `tools_test.go`: a mind reading something git does not track, or a tool writing a file.
- `floor_test.go`: a failed turn dropping its cost from `/cost` or losing its queue, a cancelled turn leaving an entry, a mention by name reaching the wrong seat.
- `anthropic_test.go`, on SSE scripts served from an `httptest` server: a cut or stalled stream stored as a turn, a finished answer failed after `message_stop`, slow headers stalling the body, a streamed turn replayed differently from a buffered one, a fallback turn priced as the model that declined, a transient mid-stream error not retried once in place or any other failure retried, lost usage or searches, a tool call reported before its input arrived.

## Phases

1. Core: the floor, the console, the transcript, the `fake` mind and the view test. Done.
2. Providers and the first rehearsal. Done.
3. The real minds: `-roster minds` on a real pickle. Done in rehearsal 3; its two round-1 failures are fixed.
4. Tools, web search and `/fold`. Done; rehearsal 2.
5. Streaming the Anthropic requests, tests first. Done, as rehearsals 2 and 3 designed it; rehearsal 4 reviewed it, and its findings are fixed.

## Open

- Should `/stop` keep the cost of the turns it cancels? Today they leave no trace.
- Not run yet: the HTTP/2 health check (the fake API speaks HTTP/1.1), and the empty-reply nudge against a provider.

## Later, only if v1 earns it

- Grok.
- `/thoughts <seat>`, so you can read a mind's reasoning summary; never shown to the other minds.
- Anonymous seats (A, B, C), to test whether the minds defer to brands.
- Castaka-style private messages.
- The Metabarons on the same engine and keys, so the Castaka reconvenes too.
