# gemma4-heretical

Local chat, memory tools, vision, and optional MTP speculative decoding for
[HauhauCS/Gemma4-26B-A4B-QAT-Uncensored-HauhauCS-Balanced-MTP](https://huggingface.co/HauhauCS/Gemma4-26B-A4B-QAT-Uncensored-HauhauCS-Balanced-MTP).

The model is a 26B-parameter mixture of experts with 4B active parameters per
token. Its QAT Q4_K_M GGUF is 16.8 GB and supports up to 256K context, although
this project defaults to 16K to leave comfortable memory headroom on a 48 GB
Apple Silicon machine.

There are two supported local backends:

| Backend | Text/tools | Images | MTP | API |
|---|---:|---:|---:|---|
| Ollama | Yes | Yes | No | `127.0.0.1:11434/v1` |
| llama.cpp | Yes | Yes | Yes | `127.0.0.1:8082/v1` |

The Hugging Face Ollama manifest includes the repository's separate `mmproj`
vision projector, so both backends accept images. llama.cpp is still required
for the separate MTP draft head.

Both model copies can be installed at once, but a 48 GB machine should run only
one inference backend at a time. Stop the llama.cpp server before loading the
Ollama model, or stop the Ollama model with `ollama stop gemma4-heretical`
before starting `serve-llama`.

## Option 1: Ollama

Requirements:

- [Ollama](https://ollama.com) 0.30.6 or newer (Gemma 4 QAT support)
- About 18 GB disk space
- About 24 GB or more available memory

Install and register the model:

```bash
./get-gemma4-heretical
```

Run the project chat client:

```bash
./chat
./chat memory.md --heartbeat 120
```

The installer registers the model and projector as `gemma4-heretical`, applies
Ollama's Gemma 4 renderer/parser, and uses the model author's recommended
sampling: temperature 0.6, top-k 64, top-p 0.9, min-p 0.05, and repeat penalty
1.1.

## `chat2`: Charm TUI

`chat2` is the Go/Charm interface. It discovers models from both local APIs at
startup and opens with a model picker showing which routes are online. The
original Python `chat` client remains available.

Run it through the launcher, which rebuilds the Go binary before every start:

```bash
./chat2
# Or load a startup/system prompt before the private harness exchange:
./chat2 instructions.md
```

Persistent options such as the system prompt file, context size, and
preferred model live in `settings.json` and are edited in the app with `F2`;
see [Settings](#settings).

The compiled executable is written to `.build/chat2` and then launched with
the same arguments.

## `chat3`: basic chat

`chat3` is a stripped-down sibling of `chat2` for plain conversation. It keeps
the live model picker, token-by-token streaming with separately rendered
llama.cpp reasoning, the Markdown transcript, and image attachments, and
drops everything agentic: there are no tools, no saved memories, no recent
message recording, no scheduling, no heartbeats, and no startup protocol.
Nothing is written to disk, and each session starts from a blank history.

```bash
./chat3
# Or send a file as the system prompt:
./chat3 instructions.md
./chat3 --image photo.jpg
```

Use `/image PATH` to attach images to the next message, `/system TEXT` to add
system context mid-session, `/clear` to reset the conversation, `Ctrl+D` to
show or hide model reasoning, and `Ctrl+G` or `/model` to switch models. The
compiled executable is written to `.build/chat3`.

## `tts`: local expressive speech

`tts` is a small Charm TUI for local text-to-speech on Apple Silicon. It uses
[Qwen3-TTS](https://github.com/QwenLM/Qwen3-TTS) through
[MLX-Audio](https://github.com/Blaizzy/mlx-audio), with natural-language
voice design and performance direction, optional preset voice selection, speed
control, automatic playback, and reusable WAV output.

Install [uv](https://docs.astral.sh/uv/) or enter the Nix development shell,
then run:

```bash
./tts
```

The first synthesis downloads MLX-Audio and the roughly 2.7 GB 8-bit
`mlx-community/Qwen3-TTS-12Hz-1.7B-VoiceDesign-8bit` model into the normal
uv/Hugging Face caches. Generated WAV files are saved under
`~/.gemma4-heretical/tts`.

Design mode is the default. Write the spoken text in Script, describe the
speaker’s age, vocal character, accent, and other durable traits in Voice
Design, then put line-specific acting notes—such as “restrained at first, then
increasingly urgent”—in Performance Direction. `Ctrl+T` switches to Preset mode,
which uses Qwen’s nine CustomVoice speakers and downloads its separate model on
first use.

Use `Tab`/`Shift+Tab` to move between visible fields, `Enter` to speak,
`Ctrl+J` for a newline, `Ctrl+N`/`Ctrl+P` to change the Preset-mode voice,
`Ctrl+Left`/`Ctrl+Right` to adjust speed, and `Ctrl+R` to replay the latest
result. Run `./tts --no-play` to generate without automatic playback; use
`./tts -h` to see mode, models, voice, language, initial voice design,
performance direction, output, and timeout overrides.

The TUI uses Bubble Tea, Bubbles, Lip Gloss, and Glamour from
[Charm](https://charm.land/). It includes:

- a live Ollama/llama.cpp model picker at startup
- an in-session model picker with `Ctrl+G` or `/model`
- a scrollable Markdown transcript and multiline composer
- token-by-token responses with separately parsed llama.cpp reasoning
- model/backend, MTP, image, memory, and context status in the header
- streamed tool calls, multimodal messages, memory tools, optional heartbeats, and system prompts
- native persistent one-off and recurring scheduling with bounded due/upcoming context
- a model-callable image request that pauses for explicit user consent
- a full-resolution browser viewfinder, terminal/file-picker fallbacks, and a dedicated image queue manager

### Settings

`chat2` keeps its configuration in `~/.gemma4-heretical/settings.json`
(override the location with `GEMMA4_SETTINGS`). Press `F2` (or `Ctrl+,`, or
type `/settings`) in the chat or the model picker to open the settings
screen: `↑/↓` move between rows, `Enter` toggles a switch, cycles a choice,
or opens an inline editor for text, numbers, and durations, `←/→` change a
switch or choice directly, `d` restores a row's default, `s` saves, and
`esc` cancels. Saving writes the JSON file and applies the values to the
running session; rows that only take effect at the next connection or model
discovery say so in their help line.

Settings: preferred model (connect automatically when it is online instead
of showing the picker), system prompt file, the memory category editor and
the categories file location, context window, heartbeat interval, request
timeout, debug and compact transcript defaults, lean tool exposure, terminal
camera preview, compaction history, instructions, and thinking, Ollama and
llama.cpp URLs, and the Whoop callback details.

Precedence is defaults, then `settings.json`, then the legacy environment
variables (`GEMMA4_CONTEXT`, `GEMMA4_OLLAMA_URL`, `GEMMA4_LLAMA_URL`,
`WHOOP_CALLBACK_LISTEN`, `WHOOP_REDIRECT_URL`), then command-line flags, so
a flag still overrides the file for one run. `./chat2 -h` lists the flags.

### Startup protocol

After you select a model, `chat2` loads the system/startup file, the five most
recent user/assistant messages, all saved memories, and the harness/tool
instructions as system messages. Those same five messages are restored into
the visible transcript after startup so the user and model open with matching
conversation history. Memory creation timestamps remain in local storage for
bookkeeping but are omitted from startup context and memory-search results so
they cannot be mistaken for conversation or event dates. Only then does it
begin a private structured startup exchange with the model. The model is told
that harness requests are
control-plane messages, not user conversation, and must answer them through the
response tool supplied for that request.

The initial request asks the model to choose short labels for itself and the
user through `configure_session_labels`. The harness validates the response,
acknowledges it into model context, and retries malformed or prose responses.
Once accepted, those labels replace `ASSISTANT` and `USER` in the transcript
for that session. During this sequence the TUI displays only
`communicating with remote...`; startup exchanges are not shown in the user
transcript.

### Exit and compaction

`Ctrl+C` (or `/quit`) in the chat starts an exit sequence. If the model
answered at least one message this session, `chat2` sends a private
`compact_conversation` harness request and shows a progress panel; pressing
`Ctrl+C` again cancels it and exits immediately, leaving the stored history
untouched. Sessions with no replies exit at once.

The compaction agent has the full conversation, the startup context (saved
memories, earlier session summaries), and the same memory and schedule tools
a heartbeat gets. It is asked to:

1. Review the session for anything the in-turn replies failed to persist and
   fix it with `save_memory`, `update_memory`, or `schedule_item`.
2. If the history is full, read the session summaries that are about to be
   deleted and save any still-relevant details to memory.
3. Finish with a `complete_compaction` call carrying a handoff summary of at
   most 250 words for this session.

Each summary is stored with a datestamp in
`~/.gemma4-heretical/last-conversation-compacted.txt` under a
`## Session compacted <RFC3339>` heading. The file keeps the most recent
sessions, five by default (**Compaction history** in settings), and older
entries are deleted after the archiving step. At startup every retained
summary is loaded as one system message, oldest first, labelled as
background context from earlier sessions, and placed between the startup
file and the restored recent messages.

If the server rejects the compaction request as larger than its context
window, `chat2` retries with roughly the older half of the session's turns
dropped and a system note saying how many were omitted, up to four times,
so compaction completes even for a session that filled the window.

`/compact` runs the same review-and-summarise exchange mid-session. On
completion the conversation history is replaced by the startup context, a
system note carrying the summary, and the last two user turns with their
replies; the transcript shows a `Conversation Compacted` chip and, under
debug, the summary itself. The history file is not written until exit.
After any reply that leaves the context more than 85% full the status line
suggests `/compact`. `esc` or `Ctrl+C` during a mid-session compaction
cancels it and keeps the current history.

Compaction runs with model thinking disabled on llama.cpp, which is several
times faster than the chat's own setting; switch **Compaction thinking** on
to keep reasoning. **Compaction instructions** is multi-line text written in
the settings screen (`Enter` for a newline, `Ctrl+S` to apply) and appended
to the request, for guidance such as what to emphasise or leave out of
summaries; **Compaction instructions file** adds the contents of a file after
it, for longer guidance kept elsewhere. The exit notice reports what was
kept, removed, and saved.

### Memory categories

Every saved memory belongs to exactly one category. The agent's tools are
`save_memory` (content and category), `list_memories` (one category, or a
summary of every category with entry counts), `search_memories` (keyword,
optionally within a category), `update_memory` (rewrite the one memory a
keyword identifies, optionally re-filing it; ambiguous keywords are rejected
with the candidates), `move_memory` (re-file matches into another category),
and `forget_memory` (keyword, optionally within a category).
Startup context groups memories under `## category` headings so the model sees
the organisation rather than a flat list.

Categories are configured in `~/.gemma4-heretical/memory-categories.txt`, one
per line as `name: description`, with `#` comments allowed; see
[memory-categories.example.txt](memory-categories.example.txt). The easiest
way to maintain the list is the editor inside the settings screen: press
`F2`, select **Memory categories**, then `a` adds a row, `Enter` edits the
selected `name: description`, `d` deletes, `Shift+↑/↓` reorders, and `s`
writes the file. Each row shows how many memories it holds, categories that
are in use but no longer configured are flagged, and saving an empty list
removes the file so names become free-form again. To keep the
list somewhere else, such as a `memory-categories.txt` next to `memory.md`
in this repository (both are gitignored), set **Memory categories file** in
the settings screen or `GEMMA4_MEMORY_CATEGORIES`; `./memory` follows the
same setting. When the file
exists, the configured list is enforced: `save_memory` and `move_memory`
reject other names and their schemas carry the list as an enum, and the
descriptions are sent to the model as filing guidance. Without the file,
category names are free-form and anything saved without one lands in
`general`.

The storage format is unchanged (`memories.jsonl`), with a `category` field
added. Entries from the earlier tag-based format are mapped on load: the first
tag that matches a configured category wins, otherwise the first tag, otherwise
`general`. They are rewritten with an explicit category the next time the file
is modified.

In the chat, `/memory` shows the category summary, `/memory list CATEGORY`
lists one category, `/memory categories` shows the configured list and the
file path, and `/memory search QUERY` searches everything.

The `./memory` command-line tool works on the same files with the same
category rules: `./memory list` groups everything by category,
`./memory list lasts` shows one category, `./memory categories` shows the
configured list with counts, and `add`, `edit`, `move`, `search`, and
`forget` all take `--category`. Set `GEMMA4_MEMORY_DIR` to point it at a
different data directory.

### Tool exposure and `request_toolcall`

By default every tool is offered on every turn. Starting `chat2` with
`--lean-tools` switches to a gated mode: regular turns then offer only a
core set (`save_memory`, `search_memories`, `get_current_time`,
`request_toolcall`, `request_tool`, and `schedule_next_heartbeat`), and the
remaining tools are grouped into capabilities that stay hidden until the model
asks for one with `request_toolcall(capability, reason)`:

| Capability | Tools |
|---|---|
| `scheduling` | `schedule_item`, `list_scheduled_items`, `complete_scheduled_item` |
| `memory-admin` | `list_memories`, `update_memory`, `move_memory`, `forget_memory` |
| `images` | `request_image` |
| `whoop` | `get_whoop_recovery_sleep` (only once Whoop is connected) |

An accepted request exposes the group for the rest of the session, starting
with the model's next tool-calling round, and renders a `Tools: NAME` chip on
that turn. Heartbeats always see every available tool, since they are private
maintenance turns. `request_tool` remains the way to propose a tool that does
not exist at all.

`/tools` shows each capability and whether it is enabled; `/tools enable NAME`
and `/tools disable NAME` override the model's choices. In lean mode the
startup context also lists the hidden groups so the model knows what it can
ask for.

### Native scheduling

`chat2` exposes `schedule_item`, `list_scheduled_items`, and
`complete_scheduled_item` directly to the model. Ask the assistant to schedule
or list something conversationally; no separate process or legacy `chat`
integration is required. Data is stored locally in
`~/.gemma4-heretical/schedule.jsonl`.

Press `Ctrl+S` (or run `/schedule`) to open the schedule manager. It lists the
persisted schedule in reverse chronological order within the terminal height.
Select open items with Enter, Space, or `c`, then exit with `Ctrl+S` or Escape.
A confirmation window requires a comment and can either mark those items
complete immediately or ask the agent to complete them. Neither choice starts
an agent turn; the update is attached to the next normal message or heartbeat.
Direct completions include the user's label and comment in private harness
metadata; agent requests identify every selected schedule ID and carry the
comment into `complete_scheduled_item`. Repeated requests remain separate rather
than collapsing into an earlier request.

`schedule_item` accepts optional `recurrence_unit` and `recurrence_period`
fields. Supported units are `hour`, `day`, `week`, and `month`; the period is a
positive number of those units. Both recurrence fields must be supplied
together. The harness persists the series and expands it into ordinary schedule
items through the next 24 hours, plus one occurrence beyond that horizon.
Completing an occurrence retains it in recent history and does not stop the
series.

The model must supply the complete year, date, and time. The harness rejects a
time earlier than the current minute unless the tool call explicitly includes
`allow_past: true`; that flag is reserved for intentionally overdue or
backdated items. This prevents a guessed or stale year from silently becoming a
due schedule entry.

Before every user turn and heartbeat, the harness rereads the schedule and
adds bounded private JSON metadata: at most five overdue items, five items due
within the next 24 hours, five unseen recent completions, and the single next
item beyond 24 hours as `next_later`. Each recent completion is injected only
once per model session, so frequent heartbeats do not repeatedly consume
context. Metadata timestamps use readable local forms such as
`Monday July 7, 2026 at 2:00 PM MDT`. Completing an item adds `completed_at` instead of
deleting it, which lets a newly started session recover recent history. The
last real user-activity timestamp is also persisted across sessions and used
in message and heartbeat metadata.
Response metadata is sent to the model as separate private system context
immediately before the corresponding user turn. It is not mixed into
user-authored text or added to either the normal or debug transcript.

### Whoop recovery and sleep

`chat2` can retrieve recent Whoop recovery and sleep records through the
official v2 API. The model receives `get_whoop_recovery_sleep` only after a
usable OAuth token exists; disconnected sessions do not advertise the tool or
mention it in private model instructions. Returned health-adjacent data is not
written to chat2 memory automatically.

This machine's reusable ngrok endpoint is:

```text
https://deloras-grapiest-transnormally.ngrok-free.dev
```

Create a Whoop developer app and register this exact redirect URL:

```text
https://deloras-grapiest-transnormally.ngrok-free.dev/oauth/whoop/callback
```

Then launch `chat2` with the app credentials and redirect URL in the
environment. Keep the client secret out of shell history where practical (for
example, export it from a private shell startup file or password manager):

```bash
export WHOOP_CLIENT_ID='...'
export WHOOP_CLIENT_SECRET='...'
export WHOOP_REDIRECT_URL='https://deloras-grapiest-transnormally.ngrok-free.dev/oauth/whoop/callback'
./chat2
```

After choosing a model, run `/whoop`. `chat2` starts its loopback callback
server on `127.0.0.1:8787`, starts the ngrok CLI against the registered static
endpoint, opens Whoop's consent page with `offline`, `read:recovery`, and
`read:sleep` scopes, validates the eight-character OAuth state, exchanges the
code server-side, and shuts ngrok down. Access and rotating refresh tokens are
stored with mode `0600` in `~/.gemma4-heretical/whoop-token.json`; the client
secret is never persisted by chat2. Keep the credential environment variables
available on later runs so an expired access token can be refreshed.

The same flow can run before opening the TUI:

```bash
./chat2 --whoop-auth
```

Heartbeats default to 15 minutes and can be changed at startup with a Go-style
duration, for example `./chat2 --heartbeat 30m instructions.md`; use
`--heartbeat 0` to disable them. A real user message arms the heartbeat timer.
Private heartbeat runs repeat at the configured interval when the model returns
the structured `none` decision, but once it returns `notify` with a visible
message, checks stop until the user responds again. Heartbeat events and their
tool protocol are ephemeral and never enter the main conversation history;
silent tool calls do not stop the checks. During an
ordinary reply, the assistant can call `schedule_next_heartbeat` with a
relative duration or future RFC3339 timestamp to override the next check. The
agent is explicitly instructed to invoke it through the structured tool-calling
channel, never response prose, and to send its visible response after the tool
result. `chat2` retains one-pass and narrowly parsed textual forms only as
compatibility safeguards.

The primary camera flow uses a temporary private page on `127.0.0.1` and opens
it in a new browser window. The browser provides a full-resolution live
viewfinder, camera selection, immediate Capture, a three-second countdown
timer, Retake, and Use Photo controls. Camera data stays local, and the page
closes its camera stream when the TUI session ends.

On macOS, the optional terminal-preview fallback uses ImageSnap:

```bash
brew install imagesnap
```

`/camera` opens the capture flow without needing a photo path. Press `c` for
the browser viewfinder. A
model can also call `request_image` to ask for a specific view and explain why
it needs it. Neither route opens the camera until you choose a preview. After
selecting Use Photo is the approval: manual captures are queued immediately,
and model-requested captures immediately resume the interrupted turn. Press
`Ctrl+I` to open the image manager, where queued images can be added, removed,
or cleared before sending. Browser captures are encrypted before their first
filesystem write using AES-256-GCM and a random, memory-only key unique to that
capture. Capture files are also unlinked after encoding, and each key is
cleared as its capture is consumed or discarded, so filesystem remnants
contain only ciphertext.
Embedded image data is removed from in-memory conversation history after the
model finishes processing it. Press `f` at the consent screen to choose an
existing image instead, or `d` to decline. Files chosen through the picker are
never deleted by `chat2`.

The lower-resolution ImageSnap terminal preview is disabled by default because
ImageSnap must briefly write a plaintext frame before `chat2` can encrypt it.
Use `--allow-plaintext-terminal-preview` to opt into that weaker fallback, then
press `t` from the capture screen.

Startup options:

```text
--context TOKENS
--heartbeat DURATION
--debug
--image PATH
--system-file PATH
--ollama-url URL
--llama-url URL
--timeout DURATION
--allow-plaintext-terminal-preview
--whoop-auth
--whoop-callback-listen HOST:PORT
--whoop-redirect-url HTTPS_URL
--whoop-auth-timeout DURATION
```

Like `chat`, `chat2` shares memories under `~/.gemma4-heretical/`. Its rolling
five-message conversation history is stored separately in
`~/.gemma4-heretical/recent-messages.jsonl`.

## Option 2: llama.cpp with vision and MTP

Install a current llama.cpp build. On macOS:

```bash
brew install llama.cpp
```

Download the main model, vision projector, and MTP head (about 18.2 GB total):

```bash
./get-gemma4-llama
```

Start the local server:

```bash
./serve-llama
```

In another terminal, connect `chat`:

```bash
./chat --backend llama
```

`serve-llama` enables Metal offload, flash attention, the Jinja tool-call
parser, the vision projector, and `draft-mtp` speculative decoding. It uses one
server slot to keep the memory footprint predictable.

To compare MTP against ordinary decoding:

```bash
./serve-llama --no-mtp
```

MTP performance varies by llama.cpp build and hardware, so benchmark both modes
on representative conversations. To run text-only while retaining MTP:

```bash
./serve-llama --no-vision
```

Configuration can be overridden without editing scripts:

```bash
GEMMA4_CONTEXT=32768 GEMMA4_PORT=8083 ./serve-llama
GEMMA4_BASE_URL=http://127.0.0.1:8083/v1 ./chat --backend llama
```

### Alternative model: Qwen3.8-27B heretic

`serve-llama` can serve any directory under `models/` that contains a
`serve.env` profile. The bundled alternative is the dense Qwen3.8-27B
abliterated build from
[0bserverx](https://huggingface.co/0bserverx/Qwen3.8-27B-Heretic-Abliterated-Uncensored-GGUF)
(three Arbitrary-Rank Ablation passes, KL divergence 0.0085 against the base),
at Q6_K with the MTP draft head embedded in the GGUF:

```bash
./get-qwen38-llama              # about 23 GB into ./models/qwen3.8-27b
./serve-llama --model qwen3.8-27b
```

The download script also writes `chat_template.chat2.jinja`. The stock Qwen
3.8 template raises `System message must be at the beginning` for any later
system message, and `chat2` injects private system messages every turn, so
the patched template renders them as ordinary system blocks instead. It also
drops the `No user query found in messages` check, because `chat2`'s private
startup exchange consists only of system messages plus a response tool. Both
changes alter only what the template accepts, not the token format. The
profile enables thinking at `medium` reasoning effort and uses Qwen's
recommended thinking-mode sampling; `xhigh` is the model's own default and
costs roughly three times the tokens of a plain reply. Override per run with
`./serve-llama --model qwen3.8-27b --thinking low` (or `xhigh`) or
`--no-thinking`. The server registers the model as `qwen3.8-27b-heretical`.

The profile starts with MTP speculative decoding off. On a 48 GB M4 Pro the
embedded draft head accepted only about 37% of drafts on a 350-word prose
request and generation measured 5.5 tokens/s, against 10 tokens/s with plain
decoding; short factual replies accepted 78 to 100% of drafts. Pass
`./serve-llama --model qwen3.8-27b --mtp` to try it on your own workload.
Prompt processing measured 48 to 85 tokens/s, and the Q6_K weights leave
enough headroom to avoid swapping, which the 34 GB Q8 Gemma 4 31B did not.
A profile can also carry extra server flags in `PROFILE_ARGS`; the Qwen one
sets `--image-min-tokens 1024`, which llama.cpp recommends for Qwen-VL
grounding accuracy.

## Images in `chat`

Queue one or more images, then send the question as the next message:

```text
>>> /image ./photo.jpg
(queued image: /absolute/path/photo.jpg)

>>> What is unusual about this image?
```

Paths containing spaces should be quoted. Useful commands:

```text
/image PATH [PATH ...]  attach images to the next message
/images                 list queued images
/clear-images           discard queued images
/help                    show chat commands
```

Images can also be queued at startup:

```bash
./chat --backend llama --image screenshot.png
```

Supported formats are BMP, GIF, JPEG, PNG, and TGA. Images are encoded locally
as data URLs and sent only to the selected local inference server.

## Chat configuration

The main CLI options are:

```text
--backend ollama|llama
--base-url URL
--api-key KEY
--model MODEL_ID
--context TOKENS
--image PATH
--heartbeat SECONDS
```

Equivalent environment variables are `GEMMA4_BACKEND`, `GEMMA4_BASE_URL`,
`GEMMA4_API_KEY`, `GEMMA4_MODEL`, and `GEMMA4_CONTEXT`.

Memories and the last exchange remain under `~/.gemma4-heretical/`, independent
of the backend.

## Older experimental paths

The repository still contains the previous `abliterate`, `convert-to-ollama`,
and `serve-mlx` experiments for the dense Gemma 4 31B model. They are not part
of the new 26B-A4B quick start.

## License

Project scripts are MIT licensed. Model weights are subject to
[Google's Gemma license](https://ai.google.dev/gemma/terms).
