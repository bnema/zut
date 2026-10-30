# Local daemon protocol (version 1)

The protocol uses UTF-8 NDJSON over a local AF_UNIX socket: one JSON object
per line, no pretty-printing. Each request has an opaque string `id`; exactly
one `result` echoes it. Notifications have no `id`. Unknown optional fields
are ignored. Frame size is limited to 32 MiB by the transport implementation.

## Connection and versions

Socket: Linux `$XDG_RUNTIME_DIR/zut/daemon.sock`, falling back to
`$ZUT_HOME/run/daemon.sock`; Windows `%LOCALAPPDATA%\zut\run\daemon.sock`.
`ZUT_DAEMON_SOCKET` overrides it. Directory permissions are 0700, socket 0600
where supported. There is no remote transport or multi-user access.

Send `hello` first. A protocol-version mismatch is a hard error. A binary
version mismatch prints `daemon is running zut <v>; run "zut daemon restart"`
and continues. `client_kind` is `tui`, `headless`, `cli`, or `bot`.

```json
{"id":"1","type":"hello","protocol_version":1,"client_version":"0.0.0-dev","client_kind":"tui","pid":123}
{"id":"1","type":"result","ok":true,"data":{"protocol_version":1,"daemon_version":"0.0.0-dev"}}
```

## Requests

The examples show the exact frame layout. Session IDs and titles below are
synthetic. `create.spec` is `session.CreateSpec`; `env` and `args` are captured
by the creating client, never inherited from daemon process cwd/environment.
Credentials are shared through the auth store, not sent through this protocol;
clients must not place credential values in creation args or env.

```json
{"id":"2","type":"create","spec":{"cwd":"/workspace","env":[],"args":[],"resume_path":"","fork":null,"headless":false}}
{"id":"3","type":"attach","session":"s1","after_seq":0,"controlling":true}
{"id":"4","type":"detach","session":"s1"}
{"id":"5","type":"close","session":"s1","reason":"user_exit"}
{"id":"6","type":"clear","session":"s1"}
{"id":"7","type":"list"}
{"id":"8","type":"tree"}
{"id":"9","type":"snapshot","session":"s1"}
{"id":"10","type":"port","session":"s1","method":"submit","params":{"in":{"text":"hello","images":[],"mode":"auto","origin":"user","from":null}}}
{"id":"11","type":"daemon_stop","graceful_ms":2000}
{"id":"12","type":"daemon_status"}
```

A fork uses `"fork":{"parent_path":"/sessions/parent.jsonl","up_to_message":3,"hidden":false}`.
`up_to_message` counts message rows copied, matching `core.BranchSession` and
`BranchSessionHidden`, not a zero-based message index. Root/cwd/version come
from creation and assembly dependencies.

`attach` replays events strictly after `after_seq` before live delivery.
A sequence outside retention fails with `event sequence too old`; an
`after_seq` ahead of the daemon's current sequence (for example after a daemon
restart) counts as too old; fetch a
snapshot and attach after its `seq`. Retention is 8192 events or 16 MiB,
whichever limit is reached first. A new controlling attach detaches the old
controller. Non-controlling subscribers do not take control.

## Results

```json
{"id":"4","type":"result","ok":true}
{"id":"3","type":"result","ok":false,"error":"event sequence too old"}
{"id":"12","type":"result","ok":true,"data":{"pid":123,"version":"0.0.0-dev","uptime_ms":1000,"sessions":1}}
```

`error` and `data` are optional. Errors are diagnostic strings, never serialized
Go error objects. Result data contracts:

| Request | Successful `data` |
| --- | --- |
| hello | `HelloData`: `protocol_version`, `daemon_version` |
| create | `session.Info` for the new session |
| attach | `session.Info` for the attached session |
| detach, close, daemon_stop | omitted |
| clear | `session.Info` for the successor |
| list | array of `session.Info` |
| tree | `session.Tree` (`workspaces` → `repo_root`, `sessions` → `info`, `residents`) |
| snapshot | `session.Snapshot` |
| port | method return value, or omitted for error-only methods |
| daemon_status | `DaemonStatusData`: `pid`, `version`, `uptime_ms`, `sessions` |

## Port dispatch

Method names are snake_case. `params` is an object keyed by the Go argument
names below (snake_case); context is transport-owned and never serialized.
Payload fields use the snake_case JSON tags in `session`.
`Input.host_event` is an optional boolean (default false); the turn cluster queues
it as `core.QueuedMessage.HostEvent`. Other input fields are `text`, `images`,
`mode`, `origin`, `from`, and optional `hidden` (provider messages carrying Meta,
appended before the prompt by the turn cluster). `ExtInsertEvent.submit` is optional (default false);
when true, the attached client executes the inserted input rather than only
inserting it into the editor.

| Method | Params | Successful data |
| --- | --- | --- |
| info | `{}` | Info |
| snapshot | `{}` | Snapshot |
| subscribe | `{"after_seq":0}` | omitted; subsequent event notifications |
| submit | `{"in":Input}` | omitted |
| abort, compact, reload_extensions, reload_credentials | `{}` | omitted |
| cancel_queued | `{"id":"q1"}` | omitted |
| slide_back_queued | `{}` | Input |
| answer_confirmation | `{"req_id":"r1","decision":ConfirmDecision}` | omitted |
| answer_question | `{"req_id":"r1","answers":[QuestionAnswer]}` | omitted |
| resolve_deferred | `{"id":"d1","action":"dismiss"}` | omitted |
| set_model | `{"provider_name":"provider","model":"model"}` | omitted |
| set_reasoning | `{"level":"high"}` | omitted |
| set_fast_mode, set_jail | `{"enabled":true}` | omitted |
| apply_profile | `{"n":1}` | omitted |
| apply_settings | `{"patch":SettingsPatch}` | omitted |
| set_goal | `{"command":{"op":"set","objective":"task"}}` | omitted |
| change_cwd | `{"path":"/workspace"}` | omitted |
| set_title | `{"title":"task"}` | omitted |
| toggle_skill_pin | `{"name":"skill","global":false}` | omitted |
| invoke_extension_command | `{"name":"command","args":"text"}` | omitted |
| extension_panel_input | `{"ext":"extension","panel_id":"panel","key":{"type":"panel_key","panel_id":"panel","key":"enter","text":""}}` | omitted |
| resident_action | `{"command":{"op":"stop","child_id":"child","prompt":""}}` | omitted |
| sessions_query | `{}` | Tree |

Types from `core`, `tools`, and `provider` retain their existing JSON field
spelling; the lossless codecs preserve interface-valued content. The panel key
is the existing `extproto.PanelKeyFromHost` (no additional key type).
`SettingsPatch` omits orchestration: `Args.Orchestrate` is launch-only through
`CreateSpec.Args`. Nil setting pointers mean unchanged. `GoalCommand.op` is
`set|pause|resume|clear|complete`; deferred action is `dismiss|send_to_agent`;
resident operation is `stop|interrupt|resume`.

## Bot services

The daemon hosts protocol bots; `telegram` is the only kind. Requests never
carry credentials: the daemon reads the bot token from `$ZUT_HOME/bot.json`
(mode 0600) and never logs it. Bot diagnostics go to the daemon log.

```json
{"id":"20","type":"bot_start","spec":{"kind":"telegram","cwd":"/workspace","env":[],"args":["--model","m"]}}
{"id":"21","type":"bot_stop","kind":"telegram"}
{"id":"22","type":"bot_status","kind":"telegram"}
{"id":"23","type":"telegram_bridge","session":"s1","on":true}
```

`bot_start` creates one headless session from `cwd`, `env` and `args` (as
`create` would) and runs the bot against it inside the daemon. Each DM is
submitted with origin `telegram`; the final assistant text of each run started
from Telegram is sent back. The session appears in `list` and can be attached.
A second `bot_start` for a running kind fails with `bot is already running`.
An unconfigured bot fails before any session is created. If the bot session
ends while the service runs (idle GC, `zut kill`), a new session replaces it.

`bot_stop` stops the service and closes its session (reason `bot_stopped`);
the session file stays resumable. Stopping a kind that is not running succeeds.

Bot services are not restarted after a daemon restart. The next `bot_start`
creates a new session; previous transcripts remain resumable.

`telegram_bridge` with `on:true` mirrors the paired Telegram chat into an
existing session: DMs are submitted with origin `telegram`, prompts typed by
the user are sent as `you: <text>`, and each finished run's final assistant
text is sent to the chat. It runs in the daemon and keeps working while the
session is detached. Turning it on for another session moves it there.
`on:false` stops it when it is bound to `session` (any session when `session`
is empty). The bridge ends with its session or the daemon. A bridge and a bot
service cannot poll the same bot: whichever starts second fails with
`bot is already running`. Bridged and bot sessions get web search disabled
(no confirmation surface in Telegram).

| Request | Successful `data` |
| --- | --- |
| bot_start, bot_stop, bot_status | `BotStatusData`: `kind`, `running`, `session`, `started_at`, `username`, `paired_user_id`, `last_error` (why the service ended on its own), `bridge` (`TelegramBridgeData` when a bridge is active) |
| telegram_bridge | `TelegramBridgeData`: `active`, `session`, `username`, `paired_user_id` |

A daemon whose session factory does not host bots answers every bot request
with `this daemon does not host bots`.

## Notifications and event payloads

```json
{"type":"event","session":"s1","seq":42,"kind":"title","data":{"title":"task"}}
{"type":"detached_by_other","session":"s1"}
{"type":"closed","session":"s1","successor":"s2"}
```

`successor` is omitted when there is none. `/clear` closes the old session and
creates a successor with the same launch configuration; its old transcript
remains resumable. A slow consumer receives a final `attach` event with
`{"state":"detached","reason":"slow_consumer"}` and its subscription closes.
That final event is subscriber-local, using the sequence of the event that could
not be delivered; authoritative replay still contains the original event.

Go event constants and payload types have distinct names to avoid namespace
collisions. `Event.Data` carries the following concrete payloads:

| Kind / Go constant | Payload type | `data` fields |
| --- | --- | --- |
| agent / EvAgent | core.AgentEvent in Event.Agent | existing `modes.EventToJSON` shapes, extended losslessly by the agent-event codec |
| info / EvInfo | InfoEvent | info |
| queue / EvQueue | QueueEvent | queue |
| pending / EvPending | PendingEvent | pending |
| pending_resolved / EvPendingResolved | PendingResolvedEvent | id, outcome |
| deferred / EvDeferred | DeferredEvent | deferred |
| note / EvNote | NoteEvent | note (level, text) |
| ext_chrome / EvExtChrome | ExtChromeEvent | ext_chrome |
| ext_panel / EvExtPanel | ExtPanelEvent | op (`open|update|close`), panel |
| ext_alert / EvExtAlert | ExtAlertEvent | alert |
| ext_display / EvExtDisplay | ExtDisplayEvent | ext, text |
| ext_insert / EvExtInsert | ExtInsertEvent | text, optional submit |
| plan / EvPlan | PlanEvent | plan (map of core.PlanUpdate) |
| goal / EvGoal | GoalEvent | goal, status |
| title / EvTitle | TitleEvent | title |
| compaction / EvCompaction | CompactionEvent | phase (`start|done|failed`), summary |
| messages_replaced / EvMessagesReplaced | MessagesReplacedEvent | none; refetch Snapshot |
| inter_session / EvInterSession | InterSessionEvent | from, text |
| attach / EvAttach | AttachEvent | state, reason |
| closed / EvClosed | ClosedEvent | reason, successor |

`ExtStatus` and `ExtWidget` mirror the existing interactive extension chrome;
`ExtPanel` associates `ext` with `spec` (`extproto.PanelSpec`). Notes contain
`level` and `text`. Pending rescue options contain `error` and `candidates`
(`provider/model` strings). Confirmation/question details retain existing core
and tools types. Session attach state is always `attached` or `detached`.

Detached confirmation/question requests are denied and recorded for review;
client disconnect or detach resolves already pending requests the same way.
Inter-session sending requires an attached sender whose current input origin is
not `inter_session`. Listing and tree queries are always allowed. Every daemon start begins with an empty session registry. Previous session
transcripts remain available through the normal resumable sessions list. The
daemon does not persist open sessions or client environments in `state.json`;
startup deletes any old `state.json` and stale daemon session review journals
on a best-effort basis. `schedules.json` and core JSONL transcripts are untouched.
