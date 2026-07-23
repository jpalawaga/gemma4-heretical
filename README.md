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

The compiled executable is written to `.build/chat2` and then launched with
the same arguments.

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
- a full-resolution browser viewfinder with terminal/file-picker fallbacks and a review-before-share step

### Startup protocol

After you select a model, `chat2` loads the system/startup file, the five most
recent user/assistant messages, all saved memories, and the harness/tool
instructions as system messages. Those same five messages are restored into
the visible transcript after startup so the user and model open with matching
conversation history. Only then does it begin a private structured startup
exchange with the model. The model is told that harness requests are
control-plane messages, not user conversation, and must answer them through the
response tool supplied for that request.

The initial request asks the model to choose short labels for itself and the
user through `configure_session_labels`. The harness validates the response,
acknowledges it into model context, and retries malformed or prose responses.
Once accepted, those labels replace `ASSISTANT` and `USER` in the transcript
for that session. During this sequence the TUI displays only
`communicating with remote...`; startup exchanges are not shown in the user
transcript.

Key bindings:

```text
Enter       send
Ctrl+J      insert a newline
Ctrl+G      choose another model
Ctrl+D      show/hide debug messages
PgUp/PgDn  scroll
Ctrl+C      quit
```

Useful commands are `/whoop`, `/camera`, `/image PATH [PATH ...]`, `/images`, `/clear-images`,
`/debug [on|off]`, `/system TEXT`, `/model`, `/clear`, and `/help`.

The normal transcript contains user and assistant turns. Harness/system entries
such as connection details, memory counts, and local image-flow events are
retained but hidden. Toggle them with `Ctrl+D` or `/debug`; start with them
visible using `--debug`. The debug view also includes the complete structured
startup protocol exchange—request, model tool response, validation results,
acknowledgement, and accepted configuration—without dumping unrelated startup
instructions or memory contents. Errors and explicitly requested help remain
visible. Its status area shows the next heartbeat in local 12-hour time and
adds `[scheduled]` when the agent explicitly chose that time.

Assistant answers render as they arrive. On llama.cpp, `chat2` requests the
`deepseek` reasoning format and keeps `reasoning_content` separate from the
answer. Live and completed reasoning appears as an `ASSISTANT · THINKING` block
only while debug messages are visible; it is never mixed into the assistant's
answer. Tool-call names and JSON arguments may arrive over many stream events;
the harness assembles the complete calls before executing them and preserves
reasoning across tool-result follow-up requests.

### Native scheduling

`chat2` exposes `schedule_item`, `list_scheduled_items`, and
`complete_scheduled_item` directly to the model. Ask the assistant to schedule
or list something conversationally; no separate process or legacy `chat`
integration is required. Data is stored locally in
`~/.gemma4-heretical/schedule.jsonl`.

`schedule_item` accepts optional `recurrence_unit` and `recurrence_period`
fields. Supported units are `hour`, `day`, `week`, and `month`; the period is a
positive number of those units. Both recurrence fields must be supplied
together. The harness persists the series and expands it into ordinary schedule
items through the next 24 hours, plus one occurrence beyond that horizon.
Completing an occurrence retains it in recent history and does not stop the
series.

Before every user turn and heartbeat, the harness rereads the schedule and
adds bounded private JSON metadata: at most five overdue items, five items due
within the next 24 hours, five unseen recent completions, and the single next
item beyond 24 hours as `next_later`. Each recent completion is injected only
once per model session, so frequent heartbeats do not repeatedly consume
context. Metadata timestamps use readable local forms such as
`Monday July 7, 2:00 PM MDT`. Completing an item adds `completed_at` instead of
deleting it, which lets a newly started session recover recent history. The
last real user-activity timestamp is also persisted across sessions and used
in message and heartbeat metadata.
Response-envelope metadata is retained as a debug-only `HARNESS` entry for
user turns, so `Ctrl+D` can confirm exactly what context was injected without
showing it in the normal transcript.

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
Silent `<response-opt-out>` checks repeat at the configured interval, but once
the assistant sends a visible response during a heartbeat, checks stop until
the user responds again. Silent tool calls do not stop the checks. During an
ordinary reply, the assistant can call `schedule_next_heartbeat` with a
relative duration or future RFC3339 timestamp to override the next check. The
agent is explicitly instructed to invoke it through the structured tool-calling
channel, never response prose, and to send its visible response after the tool
result. `chat2` retains one-pass and narrowly parsed textual forms only as
compatibility safeguards.

The primary camera flow uses a temporary private page on `127.0.0.1` and opens
it in a new browser window. The browser provides a full-resolution live
viewfinder, camera selection, Capture, Retake, and Use Photo controls. Camera
data stays local, and the page closes its camera stream when the TUI session
ends.

On macOS, the optional terminal-preview fallback uses ImageSnap:

```bash
brew install imagesnap
```

`/camera` opens the capture flow without needing a photo path. Press `c` for
the browser viewfinder. A
model can also call `request_image` to ask for a specific view and explain why
it needs it. Neither route opens the camera until you choose a preview. After
selecting Use Photo, you still approve it in the TUI before the image is queued
or sent. Browser captures are encrypted before their first filesystem write
using AES-256-GCM and a random, memory-only key unique to that capture. Capture
files are also unlinked after encoding, and each key is cleared as its capture
is consumed or discarded, so filesystem remnants contain only ciphertext.
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
