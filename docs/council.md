# Council of minds

*`cmd/council`, next to the Metabarons · working name **Les Technopères** (Castaka already has the Metabarons; the Incal's techno-priests suit technical pickles; rename freely)*

The chair brings a technical pickle, and the greatest and quirkiest minds of four labs argue it out in rounds. Unlike the Council of the Castaka, this one reconvenes: every message, the chair's or a mind's, is queued into the next turn of every other mind.

## Goal

- A terminal tool. You chair; five minds debate: Opus 5.5, Fable 5.2, Gemini 4 Hydrogen, Gemini 4 Carbon and GPT-6 Astra, with Grok later.
- Each mind is the model itself, called with your API keys. There is no subscription CLI, so usage terms are not an issue.
- You can interject at any time and nothing is lost. The transcript can be read while the debate runs and is saved afterwards.
- The minds get no tools, so nothing they write can act.

Not in v1: web search, private messages, personas, a web UI.

## The floor: how a message reaches every mind's next turn

One append-only log per session (`log.jsonl`) is the whole state. There is no queue object. A mind's queue is everything in the log that someone else wrote and that the mind has not yet seen.

Each entry has `seq`, `from` (`chair` or a seat), `kind` (`msg`, `pass` or `error`) and `text`. A mind's turn also carries `seen` (the last `seq` it had read when it spoke), `model` (the seat's configured model), `served` (the model that answered), `usage`, and the provider's native reply for replay. Errors are shown to you and never to the minds. Each session folder also holds `systems.json`, every seat's system prompt, written when the session starts and reused verbatim on resume, so a later change to the charter never rewrites a running session.

A mind's request is rebuilt from the log on every turn (`floor.View`):

```
view(seat, head):
  prev ← 0
  for each turn t of seat (msg or pass), oldest first:
      user      ← others' msgs in (prev, t.seen], oldest first, each wrapped <msg from="…">
      assistant ← t exactly as the provider returned it (reasoning included); "PASS" for a pass
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
- Each request extends the previous one byte for byte: the system prompt is frozen per session, history is append-only, and rendering is a pure function of the log (no clock, no randomness). So prompt caches hit, and Claude's history-editing check never trips on replayed reasoning.
- `seen` records what each mind knew when it spoke.
- Quitting or crashing loses nothing: resuming rereads the file (a last line cut short by a crash is dropped). A failed turn leaves its seat's queue intact for the next round.

## Rounds

- A round snapshots the head of the log. Every active seat with a non-empty queue takes one turn, all in parallel, on that snapshot. Replies print and are appended as they land. The round ends when every turn has finished.
- So round 1 is blind: first opinions are independent, and nobody anchors on whoever answers fastest. Castaka's first round is blind the same way.
- A chair message sets the round budget to `rounds`. The default is 2: blind opinions, then one round of rebuttal. Then the floor waits for you. `/more` runs one more round. The message and its budget are applied under one lock, so a round sees both or neither.
- `@opus @carbon …` in a chair message runs one round in which only those seats speak. The others still read all of it at their next turn.
- A reply of exactly `PASS` means nothing new. It is logged, shown to you dimmed and sent to no one. When no seat has anything queued, the floor waits.
- Dropped for v1: free-running minds, each looping on its own queue. The fast models would talk several times more than the deliberate ones, and the replies would ping-pong and run up costs. If it comes back, only the scheduler changes; the log and the view stay as they are.

## The minds

| Seat | Model ID | Effort | Key (env name) | $ per 1M tokens, in / out | Notes |
|---|---|---|---|---|---|
| opus | `claude-opus-5-5` | high | `ANTHROPIC_ORG_API_KEY` | 4 / 20, cache read 0.20 | default effort is medium, so set it |
| fable | `claude-fable-5-2` | high | `ANTHROPIC_ORG_API_KEY` | 8 / 40, cache read 0.20 | needs 30-day retention (400 under zero data retention) |
| hydrogen | `gemini-4-hydrogen` | high (its default) | `GEMINI_API_KEY` | 1.50 / 7.50, cache 0.075 | Google's "fast" Gemini 4; levels low to xhigh |
| carbon | `gemini-4-carbon` | high | `GEMINI_API_KEY` | 2 / 10 until 2026-11-30, then 4 / 20 | the "deliberate" flagship; default medium |
| astra | `gpt-6-astra` | high | `OPENAI_CODE_API_KEY` | 10 / 50, cached 1 | |
| grok | later | | `XAI_API_KEY` | | xAI's API is OpenAI-compatible, so it should be config only |

Every ID is listed by its key's models endpoint (2026-11-14); limits and prices come from the providers' model and pricing pages. Every model has a 1M context window. Output caps are 128K for Claude and Astra, 64K for Hydrogen, and 32K for Carbon (from its API listing). Claude 5.x rejects sampling parameters, and reasoning models gain little from them, so the quirk comes from the minds themselves, plus an optional `card`. A seat is config (`seat`, `provider`, `model`, `effort`, `card` in `council.json`), so two Opus seats with different cards would work.

## Charter (system prompt, frozen per session)

`floor/charter.go`:

```
You are {seat} ({model}) on a council: {roster: seat (model, lab), …}.
The chair brings a technical problem and decides; the council argues it out.
Messages since your last turn arrive as <msg from="name">…</msg>, oldest first.

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

- `council` opens a session and `council -resume <session>` reopens one. `-roster rehearsal` swaps in the cheap models, and `-fake` makes every seat answer with what it was shown, for free. `council -q -m "…"` runs the rounds without a console, saves the transcript and prints its path. That is Castaka's `-q`, and it can be called from Claude Code or by an agent.
- A typed line is posted at once and queued into every mind's next turn, even mid-round. `"""` on a line of its own starts or ends a multi-line message.
- Commands:
  - `/more`: run one more round.
  - `/stop`: cancel the round in flight (Ctrl-C does the same). Cancelled turns leave no trace.
  - `/rounds N`: set the round budget.
  - `/seats`: list the seats. `/seat -carbon` and `/seat +carbon` take a seat out or put it back.
  - `/attach <path>`: post a file (up to 256 KB) as a chair message.
  - `/view <seat>`: print exactly what that seat will receive next (the queue made visible).
  - `/synth [seat]`: run a mention round that asks for the consensus, the open disagreements and who holds them, and what to test first.
  - `/wait`: hold the next line until the floor is quiet, so piped chair scripts run in order.
  - `/cost` and `/quit`.
- Each round prints its speakers; each message prints when it lands, with its round, time and tokens.
- On disk: `~/.council/sessions/<date-time>/` holds `log.jsonl`, `systems.json` and `transcript.md`, all private to your user. The transcript is rewritten after each event, so it can stay open in a Markdown viewer.

## Provider notes (knowledge, not gates)

- **Anthropic** (Opus 5.5, Fable 5.2), `minds/anthropic.go`:
  - Thinking is always on. `Thinking` stays unset and `OutputConfig.Effort` sets the depth. Sampling parameters and prefill return 400.
  - Requests are not streamed: `maxTokens` is 16000 and the request timeout 15 minutes. If long Fable turns time out, streaming is the next step.
  - Top-level `CacheControl` caches the prefix.
  - Each seat's own turns are replayed with `BetaMessage.ToParam()` on the stored response, thinking blocks and signatures included. The append-only floor keeps them valid under the history-editing check, which Fable 5.2 enforces on every account. After a mid-output fallback, the reasoning blocks before the last fallback block are left out, as the fallback rules require.
  - `fallbacks: "default"` (beta `server-side-fallback-2026-07-01`). A turn that another model rescued is labelled with that model. A refusal that survives the fallback is an error you see, and the seat sits out the round.
- **Gemini** (Hydrogen, Carbon), `minds/gemini.go`:
  - The key's model list shows `generateContent` for both, and v1 uses it.
  - Thinking level is set per seat (the SDK's enum is upper case, so effort is upper-cased). The output cap covers thinking and answer together.
  - Google's current docs describe the Interactions API, whose stateless mode wants thought blocks resent exactly as received. The floor's replay rule covers that on either surface. If `generateContent` falls short on Gemini 4, the adapter moves to Interactions.
- **OpenAI** (Astra), `minds/openai.go`:
  - Responses API, with `reasoning.effort` from low to max (`none` returns 400).
  - `store: false`, because the log is the state. Its output items are replayed with `param.Override` and `include: ["reasoning.encrypted_content"]`, when they came from the same model.
  - `prompt_cache_key` per session and seat; caching is automatic, and usage reports cache writes separately. Prompts over 272K input tokens cost 2× on input and 1.5× on output.
- **Data terms:** Anthropic and OpenAI do not train on API traffic by default. Gemini counts as paid only for a key whose Cloud project has active billing; the council's key is on paid Tier 1 (checked 2026-11-14), so prompts are not used for training and are logged only to detect abuse.
- **SDKs:** `anthropic-sdk-go/v2` v2.9.0, `google.golang.org/genai` v1.77.0, `openai-go/v3` v3.99.0. Grok comes later through `openai-go` with xAI's base URL. Adding them moved `go.mod` to Go 1.26.

## Rehearsal

The same seats, adapters and requests on a cheaper roster (`-roster rehearsal`): the real models at lower effort where their contract matters, and the cheapest model with the same contract where it doesn't.

| Seats | Rehearsal model | Effort | Why |
|---|---|---|---|
| opus, fable | the same: `claude-opus-5-5`, `claude-fable-5-2` | medium | The whole Anthropic contract, fallbacks included. Fable 5.2 enforces the history check on every account, so replayed turns are tested as strictly as they will run. Medium rather than low, because at low effort a model can skip thinking and leave no thinking blocks to replay. |
| hydrogen, carbon | `gemini-4-hydrogen` | low | The Gemini 4 contract itself, at its cheapest ($1.50 / $7.50). |
| astra | `gpt-6-luna` | medium (its default) | The same Responses contract as Astra (effort levels, encrypted reasoning, caching, the 272K price step) at 1% of the price ($0.10 / $0.50). |

The run is a piped chair script:

```
(sleep 1; echo "Should we shard the media table or partition it?"; sleep 2
 echo "Assume 2B rows, 80% cold."                    # lands during round 1
 echo "/wait"; echo "@astra defend your position against opus"
 echo "/wait"; echo "/view carbon"; echo "/cost"; echo "/quit") | ./council -roster rehearsal -resume <session>
(echo "/more"; echo "/wait"; echo "/quit") | ./council -roster rehearsal -resume <session>
```

The first `sleep` lets the binary start: macOS scans a freshly built binary on its first launch, and lines that arrive before the console reads them all land in round 1.

It passes when:
- round 1 is blind (no turn had read another mind's message), and the interjection is in every round-2 queue;
- the mention round runs Astra alone, and `/view carbon` shows the mention and Astra's reply in Carbon's queue;
- Fable never answers 400 on its replayed turns, before or after the restart;
- cache reads show up once the transcript passes each provider's minimum.

What it can't show: Carbon and Astra themselves, the minds at full effort (Fable's long turns), and fallbacks, which only run on a refusal. Phase 3's first round covers the first two.

### Results, 2026-11-14

Session `~/.council/sessions/2026-11-14-rehearsal`: 20 turns over five rounds, with a restart before the last one. Every pass condition held, and no turn failed.
- **Queues:** round 1 read only #1; the interjection (#2) landed during round 1 and every round-2 turn read it; the mention round (round 4) was Astra alone; `/view carbon` showed `#15 astra, #16 fable, #17 opus, #18 chair, #19 astra`; after the restart, `/more` ran the four seats with something new and skipped Astra.
- **Replayed reasoning:** every Opus turn, and every Fable turn from round 2 on, carried a signed thinking block (Fable skipped thinking in round 1, even at medium). Every Gemini turn carried a thought signature, and every Luna turn an encrypted reasoning item. Fable accepted all its replayed turns, before and after the restart.
- **Caching:** no cache in round 1, whose prefixes were under the providers' minimums; writes from round 2; reads from round 3 on all three providers.
- **Time per turn:** Opus 14–17 s and Fable 10–17 s at medium, Hydrogen 2.5–7.6 s at low, Luna 3.5–5.8 s.
- **Cost:** $0.55 in all: Fable $0.33, Opus $0.17, the two Hydrogen seats $0.05, Luna $0.002.
- **The debate itself:** the minds read each other. In round 3, Opus answered Fable's migration plan with two bugs it would hit at cutover, and Fable conceded a point to Opus on the partition key.

## Cost shape

Each turn rereads the transcript. Caching makes that cheap, so thinking and output tokens are most of the bill. A rough estimate at high effort, assuming about 3.5K output tokens per turn: about $0.55 per round of five seats, with Astra and Fable at roughly 70% of it. Thinking lengths vary a lot, so trust `/cost`, which is computed from each turn's usage and the prices in `council.json`.

## Code shape

Go, in the Castaka module: one more static binary next to the Metabarons, whose `internal/council` stays untouched. About 1,350 lines with comments, half of them the console:
- `internal/floor`: the log (`log.go`), `View` (`view.go`, with `view_test.go`), rounds (`floor.go`), the charter and the transcript.
- `internal/minds`: one file per provider, plus a `fake` mind that echoes what it received.
- `cmd/council`: flags, the console, `/cost`.
- `council.json`: the rosters (`minds`, `rehearsal`), the key file and its variable names, the defaults and the prices. No secrets.

The test runs `View` over a fixture log with a parallel round whose replies land out of order, a chair message mid-round, a mention round, a pass and a failed turn. It catches the regression that matters: a mind missing a message, or reading one twice. Tracking a seat's reads by its own last message instead of `seen` makes it fail.

## Phases

1. Core, standard library only: the floor, the console, the transcript, the `fake` mind and the view test. Done.
2. Providers and rehearsal: the three adapters, `council.json`, and the rehearsal run above. Done; results above.
3. The real minds: `-roster minds` on a real pickle, with `/cost` read against the estimate.

**v1 is done when** all of these hold:
- the rehearsal passes;
- a real pickle runs two rounds with all five minds;
- `/view` matches the log;
- usage shows cache reads on each provider once the transcript passes that provider's minimum;
- `-q` writes the same transcript as the console.

**Later, only if v1 earns it:**
- Grok.
- Web search per seat, using each provider's own.
- `/thoughts <seat>`, so you can read a mind's reasoning summary; never shown to the other minds.
- Anonymous seats (A, B, C), to test whether the minds defer to brands.
- Castaka-style private messages.
- The Metabarons on the same engine and keys, so the Castaka reconvenes too.
